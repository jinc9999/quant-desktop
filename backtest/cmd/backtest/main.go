// Command backtest 回测引擎主程序：归并流式读取全部币种 5m CSV，
// 按时间片驱动策略引擎，输出交易明细、权益曲线、绩效指标与可视化 SVG 图表。
//
// 用法:
//   go run ./cmd/backtest -data ../data -out ../out
//   go run ./cmd/backtest -data ../data -out ../out -start 2025-01-01 -end 2025-12-31
package main

import (
	"bufio"
	"encoding/csv"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// symbolStream 单币种 CSV 数据流（按 open_time 升序）
type symbolStream struct {
	symbol string
	cr     *csv.Reader
	cur    *bar
	eof    bool
}

// newSymbolStream 打开单币种 CSV 文件并跳过表头
// 参数:
//   - path: CSV 文件路径
//
// 返回:
//   - *symbolStream: 数据流实例
//   - error: 打开或读取失败时返回错误
func newSymbolStream(path string) (*symbolStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	// 跳过表头（若有）
	if rec, err := cr.Read(); err == nil && strings.HasPrefix(rec[0], "open_time") {
		// 表头已跳过
	} else if err == nil {
		// 不是表头，需要回退处理——直接以该行构造 cur
		s := &symbolStream{symbol: strings.TrimSuffix(filepath.Base(path), ".csv"), cr: cr}
		s.cur = parseBar(rec)
		return s, nil
	} else {
		return nil, err
	}
	s := &symbolStream{symbol: strings.TrimSuffix(filepath.Base(path), ".csv"), cr: cr}
	if err := s.next(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// parseBar 将一行 CSV 记录解析为 bar
// 字段: [0]open_time [1]open [2]high [3]low [4]close [5]volume [7]quote_volume
// 参数:
//   - rec: CSV 记录切片
//
// 返回:
//   - *bar: 解析结果，非法行返回 nil
func parseBar(rec []string) *bar {
	if len(rec) < 8 {
		return nil
	}
	ts, err := strconv.ParseInt(rec[0], 10, 64)
	if err != nil {
		return nil
	}
	vals := make([]float64, 5)
	for i := 1; i <= 4; i++ {
		vals[i-1], err = strconv.ParseFloat(rec[i], 64)
		if err != nil {
			return nil
		}
	}
	qv, err := strconv.ParseFloat(rec[7], 64)
	if err != nil {
		return nil
	}
	return &bar{ts: ts, open: vals[0], high: vals[1], low: vals[2], close: vals[3], quoteVol: qv}
}

// next 推进到下一行
// 返回:
//   - error: io.EOF 表示数据读完
func (s *symbolStream) next() error {
	rec, err := s.cr.Read()
	if err == io.EOF {
		s.eof = true
		s.cur = nil
		return err
	}
	if err != nil {
		s.eof = true
		s.cur = nil
		return err
	}
	if b := parseBar(rec); b != nil {
		s.cur = b
		return nil
	}
	return s.next() // 跳过非法行
}

// fundingStream 单币种资金费率 CSV 数据流（按 fundingTime 升序）
type fundingStream struct {
	symbol string
	cr     *csv.Reader
	cur    *fundingPoint
	eof    bool
}

// parseFundingPoint 将一行资金费率 CSV 记录解析为 fundingPoint
// 字段: [0]calc_time(或 fundingTime) [1]funding_interval_hours [2]last_funding_rate [3]markPrice(旧格式才有)
// calc_time 为结算时刻的实际记录时间（带毫秒偏移），归一到 8h 边界（28800000ms）后与 5m K 线切片对齐。
// 参数:
//   - rec: CSV 记录切片
//
// 返回:
//   - *fundingPoint: 解析结果，非法行返回 nil
func parseFundingPoint(rec []string) *fundingPoint {
	if len(rec) < 3 {
		return nil
	}
	calc, err := strconv.ParseInt(rec[0], 10, 64)
	if err != nil {
		return nil
	}
	ts := calc - calc%28800000 // 归一到 8h 结算边界（00:00/08:00/16:00 UTC）
	rate, err := strconv.ParseFloat(rec[2], 64)
	if err != nil {
		return nil
	}
	mp := 0.0
	if len(rec) >= 4 { // 旧 4 列格式含 markPrice
		if v, err := strconv.ParseFloat(rec[3], 64); err == nil {
			mp = v
		}
	}
	return &fundingPoint{ts: ts, rate: rate, markPrice: mp}
}

// newFundingStream 打开单币种资金费率 CSV 文件并跳过表头
// 参数:
//   - path: CSV 文件路径
//
// 返回:
//   - *fundingStream: 数据流实例
//   - error: 打开或读取失败时返回错误
func newFundingStream(path string) (*fundingStream, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	br := bufio.NewReaderSize(f, 1<<20)
	cr := csv.NewReader(br)
	cr.FieldsPerRecord = -1
	// 跳过表头（若有）
	if rec, err := cr.Read(); err == nil && (strings.HasPrefix(rec[0], "calc_time") || strings.HasPrefix(rec[0], "fundingTime")) {
		// 表头已跳过
	} else if err == nil {
		// 不是表头，直接以该行构造 cur
		s := &fundingStream{symbol: strings.TrimSuffix(filepath.Base(path), ".csv"), cr: cr}
		s.cur = parseFundingPoint(rec)
		if s.cur == nil {
			return nil, fmt.Errorf("首行解析失败: %s", path)
		}
		return s, nil
	} else {
		f.Close()
		return nil, err
	}
	s := &fundingStream{symbol: strings.TrimSuffix(filepath.Base(path), ".csv"), cr: cr}
	if err := s.next(); err != nil {
		f.Close()
		return nil, err
	}
	return s, nil
}

// next 推进资金费率流到下一行
// 返回:
//   - error: io.EOF 表示数据读完
func (s *fundingStream) next() error {
	rec, err := s.cr.Read()
	if err == io.EOF {
		s.eof = true
		s.cur = nil
		return err
	}
	if err != nil {
		s.eof = true
		s.cur = nil
		return err
	}
	if fp := parseFundingPoint(rec); fp != nil {
		s.cur = fp
		return nil
	}
	return s.next() // 跳过非法行
}

// openFundingStreams 打开资金费率数据目录下全部币种 CSV 数据流
// 参数:
//   - dir: 资金费率数据目录
//
// 返回:
//   - []*fundingStream: 全部数据流
func openFundingStreams(dir string) []*fundingStream {
	files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil || len(files) == 0 {
		fmt.Printf("[警告] 资金费率数据目录 %s 为空，funding 模式将无信号\n", dir)
		return nil
	}
	sort.Strings(files)
	streams := make([]*fundingStream, 0, len(files))
	for _, f := range files {
		s, err := newFundingStream(f)
		if err != nil {
			fmt.Printf("[警告] 打开 %s 失败: %v\n", f, err)
			continue
		}
		if s.eof {
			continue
		}
		streams = append(streams, s)
	}
	fmt.Printf("加载 %d 个币种资金费率数据流\n", len(streams))
	return streams
}

// parseDay 解析 "YYYY-MM-DD" 或 Unix 毫秒为时间戳
// 参数:
//   - s: 时间字符串
//
// 返回:
//   - int64: Unix 毫秒时间戳
//   - error: 解析失败时返回错误
func parseDay(s string) (int64, error) {
	if t, err := time.Parse("2006-01-02", s); err == nil {
		return t.UnixMilli(), nil
	}
	if v, err := strconv.ParseInt(s, 10, 64); err == nil {
		return v, nil
	}
	return 0, fmt.Errorf("时间格式无效 %q（支持 YYYY-MM-DD 或 Unix 毫秒）", s)
}

// openStreams 打开数据目录下全部币种 CSV 数据流
// 参数:
//   - dir: 数据目录
//
// 返回:
//   - []*symbolStream: 全部数据流
func openStreams(dir string) []*symbolStream {
	files, err := filepath.Glob(filepath.Join(dir, "*.csv"))
	if err != nil {
		fmt.Printf("[错误] 数据目录读取失败: %v\n", err)
		os.Exit(1)
	}
	sort.Strings(files)
	streams := make([]*symbolStream, 0, len(files))
	for _, f := range files {
		s, err := newSymbolStream(f)
		if err != nil {
			fmt.Printf("[警告] 打开 %s 失败: %v\n", f, err)
			continue
		}
		if s.eof {
			continue
		}
		streams = append(streams, s)
	}
	fmt.Printf("加载 %d 个币种数据流\n", len(streams))
	return streams
}

// writeTradesCSV 导出交易明细到 CSV
// 参数:
//   - path: 输出路径
//   - trades: 交易列表
//
// 返回:
//   - error: 写入失败时返回错误
func writeTradesCSV(path string, trades []*Trade) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"symbol", "side", "entry_time", "entry_price", "exit_time", "exit_price", "amount", "pnl", "pnl_pct", "reason", "held_bars"})
	for _, t := range trades {
		w.Write([]string{
			t.Symbol, t.Side,
			time.UnixMilli(t.EntryTS).Format("2006-01-02 15:04:05"),
			strconv.FormatFloat(t.EntryPx, 'f', -1, 64),
			time.UnixMilli(t.ExitTS).Format("2006-01-02 15:04:05"),
			strconv.FormatFloat(t.ExitPx, 'f', -1, 64),
			strconv.FormatFloat(t.Amount, 'f', -1, 64),
			strconv.FormatFloat(t.PnL, 'f', 2, 64),
			strconv.FormatFloat(t.PnLPct, 'f', 2, 64),
			t.Reason,
			strconv.Itoa(t.HeldBars),
		})
	}
	return w.Error()
}

// writeEquityCSV 导出权益曲线到 CSV（按天采样，跨多日时每日一个点）
// 参数:
//   - path: 输出路径
//   - curve: 权益曲线（逐片）
//
// 返回:
//   - error: 写入失败时返回错误
func writeEquityCSV(path string, curve []EquityPoint) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	w := csv.NewWriter(f)
	defer w.Flush()
	w.Write([]string{"time", "equity"})
	lastDay := int64(-1)
	for _, p := range curve {
		day := p.TS / 86400000
		if day == lastDay {
			continue
		}
		lastDay = day
		w.Write([]string{
			time.UnixMilli(p.TS).Format("2006-01-02 15:04:05"),
			strconv.FormatFloat(p.Equity, 'f', 2, 64),
		})
	}
	return w.Error()
}

// Metrics 回测绩效指标
type Metrics struct {
	StartTime      string
	EndTime        string
	TotalBars      int
	TotalTrades    int
	LongTrades     int
	ShortTrades    int
	WinRate        float64
	AvgWin         float64
	AvgLoss        float64
	ProfitFactor   float64
	TotalPnL       float64
	TotalReturnPct float64
	MaxDrawdownPct float64
	SharpeRatio    float64
	AvgHoldBars    float64
	StopLossCount  int
	TrailingCount  int
	TakeProfitCount int // 固定止盈平仓数
	MaxHoldCount   int // 超时平仓数
	ATRDecayCount  int // 波动率衰减平仓数（v6）
	FundReversalCount int // 费率反转平仓数（v6）
	FundingIncome  float64 // funding 模式: 累计资金费收入(USDT)
	FirstCount     int     // 首笔入场数（追涨/回踩验证）
	FirstPnl       float64
	ChaseCount     int
	ChasePnl       float64
	PullbackCount  int
	PullbackPnl    float64
}

// computeMetrics 从交易与权益曲线计算全部绩效指标
// 参数:
//   - e: 引擎（含 trades / equityCurve）
//
// 返回:
//   - Metrics: 指标结果
func computeMetrics(e *Engine) Metrics {
	m := Metrics{}
	eq := e.equityCurve
	if len(eq) > 0 {
		m.StartTime = time.UnixMilli(eq[0].TS).Format("2006-01-02")
		m.EndTime = time.UnixMilli(eq[len(eq)-1].TS).Format("2006-01-02")
		m.TotalBars = len(eq)
	}

	// 交易统计
	winSum, lossSum := 0.0, 0.0
	wins, losses := 0, 0
	holdSum := 0
	for _, t := range e.trades {
		if t.PnL > 0 {
			wins++
			winSum += t.PnL
		} else {
			losses++
			lossSum += t.PnL
		}
		holdSum += t.HeldBars
		if t.Side == "LONG" {
			m.LongTrades++
		} else {
			m.ShortTrades++
		}
		switch t.Reason {
		case "STOP_LOSS":
			m.StopLossCount++
		case "TRAILING_STOP":
			m.TrailingCount++
		case "TAKE_PROFIT":
			m.TakeProfitCount++
		case "MAX_HOLD":
			m.MaxHoldCount++
		case "ATR_DECAY":
			m.ATRDecayCount++
		case "FUND_REVERSAL":
			m.FundReversalCount++
		}
	}
	m.TotalTrades = len(e.trades)
	if m.TotalTrades > 0 {
		m.WinRate = float64(wins) / float64(m.TotalTrades) * 100
		m.AvgWin = winSum / float64(wins)
		m.AvgLoss = lossSum / float64(losses)
		if losses > 0 && winSum > 0 {
			m.ProfitFactor = winSum / -lossSum
		}
		m.AvgHoldBars = float64(holdSum) / float64(m.TotalTrades)
	}
	m.FundingIncome = e.fundingIncome

	// 追涨/回踩分类统计
	for _, t := range e.trades {
		switch t.ChaseType {
		case "first":
			m.FirstCount++
			m.FirstPnl += t.PnL
		case "chase":
			m.ChaseCount++
			m.ChasePnl += t.PnL
		case "pullback":
			m.PullbackCount++
			m.PullbackPnl += t.PnL
		}
	}

	// 收益率与最大回撤
	initial := e.cfg.InitialEquity
	m.TotalPnL = e.equity - initial
	m.TotalReturnPct = m.TotalPnL / initial * 100
	peak := initial
	maxDD := 0.0
	for _, p := range eq {
		if p.Equity > peak {
			peak = p.Equity
		}
		if dd := (peak - p.Equity) / peak * 100; dd > maxDD {
			maxDD = dd
		}
	}
	m.MaxDrawdownPct = maxDD

	// 夏普比率（按日收益，年化 sqrt(365)）
	daily := map[int64]float64{}
	prev := initial
	for _, p := range eq {
		day := p.TS / 86400000
		daily[day] = p.Equity - prev // 当日已实现盈亏增量（近似日收益）
		prev = p.Equity
	}
	rets := make([]float64, 0, len(daily))
	for _, v := range daily {
		rets = append(rets, v)
	}
	m.SharpeRatio = sharpe(rets, e.cfg.InitialEquity)
	return m
}

// sharpe 计算日收益序列的年化夏普比率（无风险利率取 0）
// 参数:
//   - dailyPnl: 每日盈亏增量序列
//   - initial: 初始权益（收益率分母）
//
// 返回:
//   - float64: 年化夏普比率
func sharpe(dailyPnl []float64, initial float64) float64 {
	if len(dailyPnl) < 2 {
		return 0
	}
	mean := 0.0
	for _, v := range dailyPnl {
		mean += v
	}
	mean /= float64(len(dailyPnl))
	// 按初始权益换算日收益率
	meanRet := mean / initial
	var varSum float64
	for _, v := range dailyPnl {
		r := v/initial - meanRet
		varSum += r * r
	}
	std := math.Sqrt(varSum / float64(len(dailyPnl)-1))
	if std == 0 {
		return 0
	}
	return meanRet / std * math.Sqrt(365)
}

// printReport 打印绩效指标报告
// 参数:
//   - m: 指标结果
//   - cfg: 策略配置（用于打印参数）
func printReport(m Metrics, cfg *StrategyConfig) {
	fmt.Printf("\n===== 回测绩效报告 =====\n")
	fmt.Printf("回测区间: %s ~ %s（%d 个 5m 时间片）\n", m.StartTime, m.EndTime, m.TotalBars)
	fmt.Printf("范式: %s | ", cfg.Mode)
	switch cfg.Mode {
	case "funding":
		fmt.Printf("费率阈值>=%.3f%%开仓(负费率做多/正费率做空收取) 回归<=%.3f%%平仓 最长%d周期 价格止损%.1f%% | 10x杠杆 %.0fU/仓 x%d 仓 | 手续费%.2f%%/边\n",
			cfg.FundTh*100, cfg.FundExitTh*100, cfg.FundMaxHold, cfg.FundSLPct*100,
			cfg.PositionMarginUSDT, cfg.MaxOpenPositions, cfg.FeeRate*100)
	case "mr":
		fmt.Printf("触发跌幅>=%.1f%% 距24h高回撤[%.1f%%,%.1f%%] | 止盈+%.1f%% 止损-%.1f%% 最长%dh | 杠杆10x %.0fU/仓 x%d 仓 | 手续费%.2f%%/边\n",
			cfg.MRDropPct*100, cfg.MRMinDrawdownPct*100, cfg.MRMaxDrawdownPct*100,
			cfg.MRTpPct*100, cfg.MRSlPct*100, cfg.MRMaxHoldBars/12,
			cfg.PositionMarginUSDT, cfg.MaxOpenPositions, cfg.FeeRate*100)
	case "trend":
		fmt.Printf("EMA(%d,%d) 交叉 | 止损%.0f%% 固定止盈%.0f%% 跟踪+%.0f%%/-%.0f%% 持仓<=%d片 反向交叉平仓 | %.0fx杠杆 %.0fU/仓 x%d 仓 | 手续费%.2f%%/边\n",
			cfg.TrendFast, cfg.TrendSlow, cfg.StopLossPct*100, cfg.TakeProfitPct*100, cfg.TrailingActivation*100, cfg.TrailingCallback*100,
			cfg.MaxHoldBars, cfg.Leverage, cfg.PositionMarginUSDT, cfg.MaxOpenPositions, cfg.FeeRate*100)
	case "adaptive":
		fmt.Printf("BTC EMA%d 门控 | 回踩%v 追涨%v 做空%v | ATR%%<=%.1f%%→回踩 >%.1f%%→追涨 BTC<=EMA→做空\n",
			cfg.AdaptBTCEMA, !cfg.AdaptDisablePullback, !cfg.AdaptDisableChase, cfg.EnableShort, cfg.AdaptATRTh*100, cfg.AdaptATRTh*100)
		fmt.Printf("  追涨: 止损%.0f%% 固定止盈%.0f%% 跟踪+%.0f%%/-%.0f%% 持仓<=%d片 | 回踩: 24h涨幅>=%.1f%% 触EMA%d 企稳%d根 缩量<%.1fx 止损%.1f%% 止盈%.1f%% 跟踪+%.1f%%/-%.1f%% 持仓<=%d片 | 做空: 止损%.1f%% 止盈%.1f%% 跟踪+%.1f%%/-%.1f%% 持仓<=%d片 | 单日<=%d笔\n",
			cfg.StopLossPct*100, cfg.TakeProfitPct*100, cfg.TrailingActivation*100, cfg.TrailingCallback*100, cfg.MaxHoldBars,
			cfg.RBPullGain, cfg.RBEMA, cfg.RBStable, cfg.RBShrink, cfg.RBSL*100, cfg.RBTP*100, cfg.RBAct*100, cfg.RBCb*100, cfg.RBHold,
			cfg.SSL*100, cfg.STP*100, cfg.SAct*100, cfg.SCb*100, cfg.SHold, cfg.DailyMax)
		fmt.Printf("  %.0fx杠杆 %.0fU/仓 x%d 仓 | 手续费%.2f%%/边 | 收盘确认=%v\n",
			cfg.Leverage, cfg.PositionMarginUSDT, cfg.MaxOpenPositions, cfg.FeeRate*100, cfg.ClosedBarConfirm)
	case "v6":
		fmt.Printf("异动币波幅扩张 | L1: 上市>%dd 价>%.0fU ATR%%<=%.0f%% 24h涨>=%.0f%% 额>=%.0fU 量>=20均量x1.5 | L2: BB(20,%.1fσ)宽度分位<%.0f%% 收盘>上轨 阳线实体>0.6 量能x%.1f RSI[%.0f,%.0f] 分>=%.0f\n",
			cfg.NewListingMinDays, cfg.MinPrice, cfg.MaxATRPct, cfg.Min24hGainPct, cfg.MinQuoteVolume, cfg.BBMult, cfg.BBWidthMinPct, cfg.L2VolMult, cfg.RSIMin, cfg.RSIMax, cfg.L2MinScore)
		fmt.Printf("  L3: ΔOI(Z)%.0f%% 费率%.0f%%(否决大%.1f%%/中%.1f%%/小%.1f%%) RSI%.0f%% 深度%.0f%%(回测归零) 加权>=%.0f | 仓位=账户x%.1f%%xATR因子x置信度(上限单币%.1f%%) | 止损%.0f%% 跟踪+%.0f%%/-%.0f%% ATR衰减%.0f%% 超时%dh 费率反转x%.1f | 滑点 大%.2f%%/中%.1f%%/小%.1f%% | 日亏<=%.1f%% 连亏<=%d单 敞口<=%.0fx\n",
			cfg.FactorW1*100, cfg.FactorW2*100, cfg.FundVetoBig*100, cfg.FundVetoMid*100, cfg.FundVetoSmall*100, cfg.FactorW3*100, cfg.FactorW4*100, cfg.L3MinScore,
			cfg.RiskPct*100, cfg.SingleCoinMarginPct*100, cfg.StopLossPct*100, cfg.TrailingActivation*100, cfg.TrailingCallback*100, cfg.ATRDecayPct*100,
			cfg.MaxHoldBars*5/60, cfg.FundReversalMult, cfg.SlippageBig*100, cfg.SlippageMid*100, cfg.SlippageSmall*100,
			cfg.DailyLossPct*100, cfg.MaxConsecutiveLosses, cfg.MaxLeverageExposure)
	default:
		fmt.Printf("15m实体>=%.0f%% 24h>=%.0f%% 放量>=%.1fx(前%d根) 山顶<=%.0f%% 24h成交额>=%.0fU | %.0fx杠杆 %.0fU/仓 x%d 仓 | 止损%.0f%% 固定止盈%.0f%% 跟踪+%.0f%%/-%.0f%% 持仓<=%d片 手续费%.2f%%/边 | 收盘确认=%v\n",
			cfg.MinGainPct, cfg.Min24hGainPct, cfg.VolumeSurgeThreshold, cfg.SurgeLookback, cfg.MaxPullbackPct, cfg.MinQuoteVolume,
			cfg.Leverage, cfg.PositionMarginUSDT, cfg.MaxOpenPositions, cfg.StopLossPct*100, cfg.TakeProfitPct*100,
			cfg.TrailingActivation*100, cfg.TrailingCallback*100, cfg.MaxHoldBars,
			cfg.FeeRate*100, cfg.ClosedBarConfirm)
	}
	fmt.Printf("\n--- 交易统计 ---\n")
	fmt.Printf("总交易数: %d（多 %d / 空 %d）\n", m.TotalTrades, m.LongTrades, m.ShortTrades)
	fmt.Printf("胜率: %.2f%%\n", m.WinRate)
	fmt.Printf("平均盈利: %.2fU  平均亏损: %.2fU\n", m.AvgWin, m.AvgLoss)
	fmt.Printf("盈亏比(Profit Factor): %.2f\n", m.ProfitFactor)
	fmt.Printf("平均持仓: %.1f 根 5m K 线\n", m.AvgHoldBars)
	fmt.Printf("平仓原因: 止损 %d 笔 / 固定止盈 %d 笔 / 跟踪止盈 %d 笔 / 超时 %d 笔", m.StopLossCount, m.TakeProfitCount, m.TrailingCount, m.MaxHoldCount)
	if cfg.Mode == "v6" {
		fmt.Printf(" / 波动率衰减 %d 笔 / 费率反转 %d 笔", m.ATRDecayCount, m.FundReversalCount)
	}
	fmt.Printf("\n")
	if m.FirstCount+m.ChaseCount+m.PullbackCount > 0 {
		fmt.Printf("追涨/回踩: 首笔 %d 笔 %+.2fU (%.3f/笔) | 追涨 %d 笔 %+.2fU (%.3f/笔) | 回踩 %d 笔 %+.2fU (%.3f/笔)\n",
			m.FirstCount, m.FirstPnl, safeDiv(m.FirstPnl, float64(m.FirstCount)),
			m.ChaseCount, m.ChasePnl, safeDiv(m.ChasePnl, float64(m.ChaseCount)),
			m.PullbackCount, m.PullbackPnl, safeDiv(m.PullbackPnl, float64(m.PullbackCount)))
	}
	fmt.Printf("\n--- 收益与风险 ---\n")
	fmt.Printf("累计盈亏: %.2fU\n", m.TotalPnL)
	if cfg.Mode == "funding" {
		fmt.Printf("资金费收入: %.2fU（占累计盈亏 %.1f%%）\n", m.FundingIncome, safePct(m.FundingIncome, m.TotalPnL))
	}
	fmt.Printf("总收益率: %.2f%%（初始 %.0fU）\n", m.TotalReturnPct, cfg.InitialEquity)
	fmt.Printf("最大回撤: %.2f%%\n", m.MaxDrawdownPct)
	fmt.Printf("夏普比率: %.2f（日收益年化）\n", m.SharpeRatio)
}

// safePct 计算 a 占 b 的百分比，b 为 0 时返回 0（避免除零）
// 参数:
//   - a: 分子
//   - b: 分母
//
// 返回:
//   - float64: 百分比数值
func safePct(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b * 100
}

func safeDiv(a, b float64) float64 {
	if b == 0 {
		return 0
	}
	return a / b
}

// main 回测主程序入口
// 参数由命令行 flag 提供（-data / -out / -start / -end）
func main() {
	dataDir := flag.String("data", "data", "5m CSV 数据目录")
	outDir := flag.String("out", "out", "输出目录（trades.csv / equity.csv / report.svg）")
	fundingDir := flag.String("funding-dir", "data_funding", "资金费率 CSV 数据目录（funding 范式使用）")
	startFlag := flag.String("start", "", "回测起始日期 YYYY-MM-DD（默认不限制）")
	endFlag := flag.String("end", "", "回测结束日期 YYYY-MM-DD（默认不限制）")
	slFlag := flag.Float64("sl", 8.0, "止损比例 %%（默认 8）")
	actFlag := flag.Float64("act", 3.0, "跟踪止盈激活涨幅 %%（默认 3）")
	cbFlag := flag.Float64("cb", 2.0, "跟踪止盈回调 %%（默认 2）")
	surgeFlag := flag.Float64("surge", 1.8, "放量倍数阈值（默认 1.8）")
	gainFlag := flag.Float64("gain", 5.0, "15m 实体涨幅门槛 %%（默认 5）")
	minVolFlag := flag.Float64("minvol", 50000, "24h 成交额下限 USDT（默认 50000）")
	feeFlag := flag.Float64("fee", 0.04, "单边手续费率 %%taker（默认 0.04）")
	closedFlag := flag.Bool("closed", true, "已收盘 15m K 线实体确认（默认 true）")
	exitModeFlag := flag.String("exit-mode", "ohlc", "退出检测模式: ohlc=片内高低价(保守) / close=片收盘价(近似 tick 采样)")
	min24GainFlag := flag.Float64("min24gain", 5.0, "24h 涨幅门槛 %%（设 0 关闭，默认 5）")
	pullbackFlag := flag.Float64("pullback", 9.0, "山顶过滤: 距24h高点最大回撤 %%（设大如 1000 关闭，默认 9）")
	cooldownFlag := flag.Int("cooldown", 20, "平仓后冷却分钟数（默认 20）")
	marginFlag := flag.Float64("margin", 20.0, "每仓保证金 USDT（默认 20）")
	modeFlag := flag.String("mode", "momentum", "信号范式: momentum/mr/trend/funding/adaptive")
	adaptatrFlag := flag.Float64("adaptatr", 2.0, "ADAPT BTC ATR%% 阈值（回踩/追涨判定，默认 2）")
	btcemaFlag := flag.Int("btcema", 50, "ADAPT BTC EMA 周期（牛熊判定，默认 50）")
	rbgainFlag := flag.Float64("rbgain", 5.0, "ADAPT 回踩 24h 涨幅门槛 %%（默认 5）")
	rbemaFlag := flag.Int("rbema", 20, "ADAPT 回踩支撑 EMA 周期（默认 20）")
	rbshrinkFlag := flag.Float64("rbshrink", 0.7, "ADAPT 回踩缩量倍数（默认 0.7）")
	rbstableFlag := flag.Int("rbstable", 3, "ADAPT 回踩企稳根数（默认 3）")
	rbslFlag := flag.Float64("rbsl", 2.5, "ADAPT 回踩止损 %%（默认 2.5）")
	rbtpFlag := flag.Float64("rbtp", 5.0, "ADAPT 回踩固定止盈 %%（默认 5）")
	rbactFlag := flag.Float64("rbact", 2.0, "ADAPT 回踩移动激活 %%（默认 2）")
	rbcbFlag := flag.Float64("rbcb", 1.0, "ADAPT 回踩移动回调 %%（默认 1）")
	rbholdFlag := flag.Int("rbhold", 120, "ADAPT 回踩最长持仓分钟（默认 120）")
	sslFlag := flag.Float64("ssl", 5.0, "ADAPT 做空止损 %%（默认 5）")
	stpFlag := flag.Float64("stp", 8.0, "ADAPT 做空固定止盈 %%（默认 8）")
	sactFlag := flag.Float64("sact", 2.0, "ADAPT 做空移动激活 %%（默认 2）")
	scbFlag := flag.Float64("scb", 1.5, "ADAPT 做空移动回调 %%（默认 1.5）")
	sholdFlag := flag.Int("shold", 90, "ADAPT 做空最长持仓分钟（默认 90）")
	dailymaxFlag := flag.Int("dailymax", 0, "ADAPT 单日最大开仓数（0 不限，默认 0）")
	adaptnopull := flag.Bool("adaptnopull", false, "ADAPT 关闭回踩模式（趋势优先）")
	adaptnochase := flag.Bool("adaptnochase", false, "ADAPT 关闭追涨模式（震荡优先）")
	mrdropFlag := flag.Float64("mrdrop", 3.0, "MR 触发跌幅 %（默认 3）")
	mrtpFlag := flag.Float64("mrtp", 2.0, "MR 反弹止盈 %（默认 2）")
	mrslFlag := flag.Float64("mrsl", 1.5, "MR 反弹失败止损 %（默认 1.5）")
	mrholdFlag := flag.Int("mrhold", 24, "MR 最长持有 K 线数（默认 24=2h）")
	mrddminFlag := flag.Float64("mrddmin", 2.0, "MR 距24h高点最小回撤 %（默认 2）")
	mrddmaxFlag := flag.Float64("mrddmax", 15.0, "MR 距24h高点最大回撤 %（默认 15）")
	fastFlag := flag.Int("fast", 96, "TREND 快线 EMA 周期（默认 96≈8h）")
	slowFlag := flag.Int("slow", 288, "TREND 慢线 EMA 周期（默认 288≈24h）")
	fthFlag := flag.Float64("fth", 0.05, "FUND 开仓费率阈值 %（默认 0.05，|费率|>=该值开仓收取）")
	fexitFlag := flag.Float64("fexit", 0.01, "FUND 费率回归平仓阈值 %（默认 0.01）")
	fholdFlag := flag.Int("fhold", 3, "FUND 最长持有资金费结算周期数（默认 3=24h）")
	fslFlag := flag.Float64("fsl", 5.0, "FUND 价格止损 %（默认 5）")
	topnFlag := flag.Int("topn", 8, "每片候选排序取前 N（默认 8）")
	maxposFlag := flag.Int("maxpos", 5, "最大同时持仓数（默认 5）")
	tpFlag := flag.Float64("tp", 0.0, "固定止盈 %%（0 关闭，默认 0）")
	holdFlag := flag.Int("hold", 0, "最大持仓分钟数（0 关闭，默认 0）")
	trailcdFlag := flag.Int("trailcd", -1, "S01 实验: 移动止盈平仓后冷却分钟数（-1=统一 CooldownMs；0=立即再入）")
	addonFlag := flag.Bool("addon", false, "S01 实验: 启用追加仓位（同币移动止盈激活后再命中信号可加仓）")
	addonmaxFlag := flag.Int("addonmax", 1, "S01 实验: 单币最大追加次数（默认 1 = 同币最多两仓）")
	levFlag := flag.Float64("lev", 10.0, "杠杆倍数（默认 10）")
	onlyLong := flag.Bool("only-long", false, "仅做多（默认双向）")
	onlyShort := flag.Bool("only-short", false, "仅做空（默认双向）")
	v6rawFlag := flag.Bool("v6raw", false, "V6 诊断: 关闭日亏/连亏熔断（仅诊断，勿用于正式对比）")
	slipoffFlag := flag.Bool("slipoff", false, "V6 诊断: 滑点置零（仅诊断，勿用于正式对比）")
	fvetoFlag := flag.Bool("fveto", false, "S01 实验: 费率过热否决（正费率 ≥ 分级阈值不追）")
	vzFlag := flag.Float64("vz", 0, "S01 实验: 成交量 Z-Score 确认阈值（0=关闭）")
	rsiokFlag := flag.Bool("rsiok", false, "S01 实验: RSI[40,70] 趋势带确认")
	flag.Parse()

	var startTs, endTs int64
	if *startFlag != "" {
		v, err := parseDay(*startFlag)
		if err != nil {
			fmt.Printf("[错误] %v\n", err)
			os.Exit(1)
		}
		startTs = v
	}
	if *endFlag != "" {
		v, err := parseDay(*endFlag)
		if err != nil {
			fmt.Printf("[错误] %v\n", err)
			os.Exit(1)
		}
		endTs = v + 86400000 // 含结束日
	}

	if err := os.MkdirAll(*outDir, 0o755); err != nil {
		fmt.Printf("[错误] 创建输出目录失败: %v\n", err)
		os.Exit(1)
	}

	streams := openStreams(*dataDir)
	if len(streams) == 0 {
		fmt.Println("[错误] 无可用数据，请先运行 download 下载数据")
		os.Exit(1)
	}

	cfg := DefaultConfig()
	cfg.MinGainPct = *gainFlag
	cfg.MinQuoteVolume = *minVolFlag
	cfg.StopLossPct = *slFlag / 100
	cfg.TrailingActivation = *actFlag / 100
	cfg.TrailingCallback = *cbFlag / 100
	cfg.VolumeSurgeThreshold = *surgeFlag
	cfg.ClosedBarConfirm = *closedFlag
	cfg.ExitClose = *exitModeFlag == "close"
	cfg.Min24hGainPct = *min24GainFlag
	cfg.MaxPullbackPct = *pullbackFlag
	cfg.CooldownMs = int64(*cooldownFlag) * 60 * 1000
	cfg.PositionMarginUSDT = *marginFlag
	cfg.FeeRate = *feeFlag / 100
	cfg.Mode = *modeFlag
	cfg.MRDropPct = *mrdropFlag / 100
	cfg.MRTpPct = *mrtpFlag / 100
	cfg.MRSlPct = *mrslFlag / 100
	cfg.MRMaxHoldBars = *mrholdFlag
	cfg.MRMinDrawdownPct = *mrddminFlag / 100
	cfg.MRMaxDrawdownPct = *mrddmaxFlag / 100
	cfg.TrendFast = *fastFlag
	cfg.TrendSlow = *slowFlag
	cfg.FundTh = *fthFlag / 100
	cfg.FundExitTh = *fexitFlag / 100
	cfg.FundMaxHold = *fholdFlag
	cfg.FundSLPct = *fslFlag / 100
	cfg.TopN = *topnFlag
	cfg.MaxOpenPositions = *maxposFlag
	cfg.TakeProfitPct = *tpFlag / 100
	cfg.MaxHoldBars = *holdFlag / 5 // 持仓分钟 → 5m 片数
	cfg.CooldownAfterTrailingMin = *trailcdFlag
	cfg.EnableAddOn = *addonFlag
	cfg.MaxAddOnsPerSymbol = *addonmaxFlag
	cfg.Leverage = *levFlag
	cfg.AdaptATRTh = *adaptatrFlag / 100
	cfg.AdaptBTCEMA = *btcemaFlag
	cfg.RBPullGain = *rbgainFlag
	cfg.RBEMA = *rbemaFlag
	cfg.RBShrink = *rbshrinkFlag
	cfg.RBStable = *rbstableFlag
	cfg.RBSL = *rbslFlag / 100
	cfg.RBTP = *rbtpFlag / 100
	cfg.RBAct = *rbactFlag / 100
	cfg.RBCb = *rbcbFlag / 100
	cfg.RBHold = *rbholdFlag / 5 // 分钟 → 5m 片数
	cfg.SSL = *sslFlag / 100
	cfg.STP = *stpFlag / 100
	cfg.SAct = *sactFlag / 100
	cfg.SCb = *scbFlag / 100
	cfg.SHold = *sholdFlag / 5 // 分钟 → 5m 片数
	cfg.DailyMax = *dailymaxFlag
	cfg.AdaptDisablePullback = *adaptnopull
	cfg.AdaptDisableChase = *adaptnochase
	if *onlyLong {
		cfg.EnableShort = false
	}
	if *onlyShort {
		cfg.EnableShort = true
		cfg.OnlyShort = true
	}
	// S01 单因子实验（默认全关，不改动 S01 现有行为）
	applyS01Experiments(cfg, *fvetoFlag, *vzFlag, *rsiokFlag)
	// v6 口径覆盖共享参数（必须在 flag 赋值之后，否则默认 flag 会覆盖 v6 规范值）
	if *modeFlag == "v6" {
		V6Defaults(cfg)
		if *v6rawFlag {
			cfg.DailyLossPct = 100          // 日亏熔断关闭
			cfg.MaxConsecutiveLosses = 1000000 // 连亏熔断关闭
		}
		if *slipoffFlag {
			cfg.SlippageBig, cfg.SlippageMid, cfg.SlippageSmall = 0, 0, 0
		}
	}
	eng := NewEngine(cfg)
	begin := time.Now()

	// funding / v6 / S01 费率实验: 加载资金费率数据流
	var fundingStreams []*fundingStream
	if cfg.Mode == "funding" || cfg.Mode == "v6" || (cfg.Mode == "momentum" && cfg.FundingVetoEnabled) {
		fundingStreams = openFundingStreams(*fundingDir)
	}

	// 归并流主循环: 每次取全部数据流的最小时间片，组成该片横截面
	processed := 0
	var lastBars map[string]*bar
	for {
		minTS := int64(math.MaxInt64)
		for _, s := range streams {
			if !s.eof && s.cur != nil && s.cur.ts < minTS {
				minTS = s.cur.ts
			}
		}
		if minTS == math.MaxInt64 {
			break
		}
		if startTs > 0 && minTS < startTs {
			for _, s := range streams {
				if !s.eof && s.cur != nil && s.cur.ts == minTS {
					s.next()
				}
			}
			continue // 跳过起始日之前的数据（预热不完整，但保持代码简单）
		}
		if endTs > 0 && minTS >= endTs {
			break
		}

		bars := make(map[string]*bar)
		for _, s := range streams {
			if !s.eof && s.cur != nil && s.cur.ts == minTS {
				bars[s.symbol] = s.cur
				s.next()
			}
		}

		// 资金费率: 该片若有结算点则纳入（先追上慢于 K 线的资金费流）
		fundings := make(map[string]fundingPoint)
		for _, fs := range fundingStreams {
			if fs.eof || fs.cur == nil {
				continue
			}
			for fs.cur.ts < minTS {
				fs.next()
				if fs.eof || fs.cur == nil {
					break
				}
			}
			if !fs.eof && fs.cur != nil && fs.cur.ts == minTS {
				fundings[fs.symbol] = *fs.cur
				fs.next()
			}
		}

		eng.OnBar(bars, fundings, minTS)
		lastBars = bars
		processed++
	}

	eng.Finalize(lastBars)

	if cfg.Mode == "v6" {
		eng.printV6GateStats()
	}
	if cfg.Mode == "momentum" && cfg.FundingVetoEnabled {
		fmt.Printf("费率过热否决信号数: %d\n", eng.fundingVetoCount)
	}

	elapsed := time.Since(begin)
	fmt.Printf("回测完成: %d 个时间片，耗时 %.1f 秒\n", processed, elapsed.Seconds())

	// 输出交易与权益
	if err := writeTradesCSV(filepath.Join(*outDir, "trades.csv"), eng.trades); err != nil {
		fmt.Printf("[错误] 写交易明细失败: %v\n", err)
	}
	if err := writeEquityCSV(filepath.Join(*outDir, "equity.csv"), eng.equityCurve); err != nil {
		fmt.Printf("[错误] 写权益曲线失败: %v\n", err)
	}

	// 指标 + 图表
	m := computeMetrics(eng)
	printReport(m, cfg)
	if err := writeReportSVG(filepath.Join(*outDir, "report.svg"), eng.equityCurve, cfg.InitialEquity); err != nil {
		fmt.Printf("[错误] 写图表失败: %v\n", err)
	} else {
		fmt.Printf("\n输出文件: %s\n", filepath.Join(*outDir, "report.svg"))
	}
}
