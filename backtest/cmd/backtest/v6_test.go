package main

import (
	"math"
	"testing"
)

// testV6Engine 构造 v6 模式的回测引擎（测试用）。
func testV6Engine() *Engine {
	cfg := &StrategyConfig{}
	V6Defaults(cfg)
	return &Engine{
		cfg:            cfg,
		states:         make(map[string]*symbolState),
		fundRate:       make(map[string]float64),
		fundPrev:       make(map[string]float64),
		equity:         cfg.InitialEquity,
		dayStartEquity: cfg.InitialEquity,
		lastDay:        -1,
	}
}

// testFeedBars 向引擎连续喂入 K 线（时间递增 5 分钟），返回最终状态。
func testFeedBars(e *Engine, symbol string, closes, vols []float64, baseTS int64) *symbolState {
	var st *symbolState
	for i, c := range closes {
		o := c
		if i > 0 {
			o = closes[i-1]
		}
		hi, lo := math.Max(o, c)+0.001, math.Min(o, c)-0.001
		b := &bar{ts: baseTS + int64(i)*300000, open: o, high: hi, low: lo, close: c, quoteVol: vols[i]}
		st, _ = e.updateState(symbol, b)
	}
	return st
}

// refRSI 独立参考实现（逐根重算，验证增量维护无环形索引错误）。
func refRSI(closes []float64, period int) []float64 {
	out := make([]float64, len(closes))
	var g, l float64
	for i := 1; i < len(closes); i++ {
		delta := closes[i] - closes[i-1]
		dg, dl := math.Max(delta, 0), math.Max(-delta, 0)
		if i < period {
			g += dg
			l += dl
			continue
		}
		if i == period {
			g += dg
			l += dl
			out[i] = rsiValue(g/float64(period), l/float64(period))
			g, l = g/float64(period), l/float64(period)
			continue
		}
		k := float64(period)
		g = (g*(k-1) + dg) / k
		l = (l*(k-1) + dl) / k
		out[i] = rsiValue(g, l)
	}
	return out
}

// refATR 独立参考实现（TR 简单平均 14）。
func refATR(bars []*bar) []float64 {
	out := make([]float64, len(bars))
	for i := 1; i < len(bars); i++ {
		if i < 14 {
			continue
		}
		sum := 0.0
		for j := i - 13; j <= i; j++ {
			trj := bars[j].high - bars[j].low
			if h := math.Abs(bars[j].high - bars[j-1].close); h > trj {
				trj = h
			}
			if l := math.Abs(bars[j].low - bars[j-1].close); l > trj {
				trj = l
			}
			sum += trj
		}
		out[i] = sum / 14
	}
	return out
}

// TestV6RSI 增量 RSI 与独立参考实现一致；全涨 → 100，全跌 → 0。
func TestV6RSI(t *testing.T) {
	// 混合序列
	closes := make([]float64, 120)
	vols := make([]float64, 120)
	closes[0] = 100
	for i := 1; i < 120; i++ {
		closes[i] = closes[i-1] + 0.3*math.Sin(float64(i)/2.0) + 0.05
		vols[i] = 40000
	}
	e := testV6Engine()
	_ = testFeedBars(e, "X", closes, vols, 1700000000000)
	ref := refRSI(closes, 14)
	// 逐根比对增量实现与独立参考
	e2 := testV6Engine()
	for i := 0; i < 120; i++ {
		o := closes[i]
		if i > 0 {
			o = closes[i-1]
		}
		b := &bar{ts: 1700000000000 + int64(i)*300000, open: o, high: math.Max(o, closes[i]) + 0.001,
			low: math.Min(o, closes[i]) - 0.001, close: closes[i], quoteVol: 40000}
		e2.updateState("X", b)
		if i >= 14 {
			if got := e2.states["X"].rsi; math.Abs(got-ref[i]) > 1e-9 {
				t.Fatalf("逐根 RSI 不一致 bar=%d: 增量=%.10f 参考=%.10f", i, got, ref[i])
			}
		}
	}

	// 全涨 → RSI=100；全跌 → RSI=0
	up := make([]float64, 40)
	upV := make([]float64, 40)
	for i := range up {
		up[i] = 100 + float64(i)
		upV[i] = 40000
	}
	e3 := testV6Engine()
	_ = testFeedBars(e3, "U", up, upV, 1700000000000)
	if e3.states["U"].rsi != 100 {
		t.Errorf("全涨 RSI = %.2f, 期望 100", e3.states["U"].rsi)
	}
	down := make([]float64, 40)
	for i := range down {
		down[i] = 100 - float64(i)
	}
	e4 := testV6Engine()
	_ = testFeedBars(e4, "D", down, upV, 1700000000000)
	if e4.states["D"].rsi != 0 {
		t.Errorf("全跌 RSI = %.2f, 期望 0", e4.states["D"].rsi)
	}
}

// TestV6ATR 增量 ATR 与独立参考实现一致。
func TestV6ATR(t *testing.T) {
	closes := make([]float64, 80)
	vols := make([]float64, 80)
	closes[0] = 100
	for i := 1; i < 80; i++ {
		closes[i] = closes[i-1] + 0.2*math.Sin(float64(i)/3.0)
		vols[i] = 40000
	}
	e := testV6Engine()
	bars := make([]*bar, 80)
	for i, c := range closes {
		o := c
		if i > 0 {
			o = closes[i-1]
		}
		b := &bar{ts: 1700000000000 + int64(i)*300000, open: o, high: math.Max(o, c) + 0.001,
			low: math.Min(o, c) - 0.001, close: c, quoteVol: vols[i]}
		bars[i] = b
		e.updateState("X", b)
	}
	ref := refATR(bars)
	// 逐根比对
	e2 := testV6Engine()
	for i := 0; i < 80; i++ {
		e2.updateState("X", bars[i])
		if i >= 14 {
			if got := e2.states["X"].atr; math.Abs(got-ref[i]) > 1e-9 {
				t.Fatalf("逐根 ATR 不一致 bar=%d: 增量=%.10f 参考=%.10f", i, got, ref[i])
			}
		}
	}
}

// TestV6Squeeze 稳定期挤压分位低；突破当根仍沿用上一根的分位（挤压先于突破）。
func TestV6Squeeze(t *testing.T) {
	e := testV6Engine()
	n := 300
	closes := make([]float64, n)
	vols := make([]float64, n)
	// 前 220 根高波动，后 80 根窄幅（挤压），最后一根突破
	for i := 0; i < n; i++ {
		amp := 1.2
		if i > 220 {
			amp = 0.3
		}
		closes[i] = 92 + 10*float64(i)/float64(n) + amp*math.Sin(float64(i)/5.0)
		vols[i] = 40000
	}
	st := testFeedBars(e, "X", closes, vols, 1700000000000)
	if !st.bbHasPrev {
		t.Fatal("bbHasPrev 应为 true")
	}
	if st.bbSqueezePct >= 0.3 {
		t.Fatalf("挤压分位 = %.3f, 期望 < 0.3（挤压应成立）", st.bbSqueezePct)
	}
	// 突破当根: 分位字段仍为上一根值（不会因突破扩张而失效）
	b := &bar{ts: 1700000000000 + int64(n)*300000, open: closes[n-1], high: closes[n-1] + 2, low: closes[n-1] - 0.001,
		close: closes[n-1] + 1.5, quoteVol: 400000}
	e.updateState("X", b)
	if got := e.states["X"].bbSqueezePct; got >= 0.3 {
		t.Fatalf("突破当根挤压分位 = %.3f, 期望仍 < 0.3", got)
	}
}

// TestV6Factors 因子计分与否决项。
func TestV6Factors(t *testing.T) {
	e := testV6Engine()
	cfg := e.cfg
	// ΔOI(Z): 达标/线性/零方差中性
	if got := v6FactorOI(3.0, "big", cfg); got != 100 {
		t.Errorf("big z=3.0 得分 %.2f, 期望 100", got)
	}
	if got := v6FactorOI(1.0, "small", cfg); math.Abs(got-100.0/1.5*1.0) > 1e-9 {
		t.Errorf("small z=1.0 得分 %.2f, 期望 %.2f", got, 100.0/1.5)
	}
	if got := v6FactorOI(0, "mid", cfg); got != 0 {
		t.Errorf("z=0 得分 %.2f, 期望 0", got)
	}
	// 资金费率: 过热否决 / 负转正 / 负费率中性 / 正费率衰减
	if _, veto := v6FactorFunding(0.005, 0.001, "big", cfg); !veto {
		t.Error("大币费率 0.005 应一票否决")
	}
	if s, veto := v6FactorFunding(0.0001, -0.0001, "small", cfg); veto || s != 100 {
		t.Errorf("负转正得分 %.2f veto=%v, 期望 100/false", s, veto)
	}
	if s, veto := v6FactorFunding(-0.0002, -0.0001, "small", cfg); veto || s != 50 {
		t.Errorf("负费率得分 %.2f veto=%v, 期望 50/false", s, veto)
	}
	if s, veto := v6FactorFunding(0.01, 0.001, "small", cfg); veto || math.Abs(s-50) > 1e-9 {
		t.Errorf("正费率(半程)得分 %.2f veto=%v, 期望 50/false", s, veto)
	}
	// RSI 因子
	if v6FactorRSI(60) != 100 || v6FactorRSI(50) != 70 || v6FactorRSI(42) != 50 || v6FactorRSI(30) != 0 {
		t.Error("RSI 因子分档错误")
	}
}

// TestV6Sizing 动态仓位: 单币上限截断 / 总敞口上限 / ATR 因子。
func TestV6Sizing(t *testing.T) {
	e := testV6Engine()
	// 无 ATR 状态: 置信度 0.85（70 分）→ base=1000*1%*1*0.85/6% ≈ 141.7, 单币上限=1000*0.5%*10=50
	n, m, amt, ok := e.v6Sizing("X", 70, "small", 10)
	if !ok {
		t.Fatal("应可开仓")
	}
	if math.Abs(n-50) > 1e-9 {
		t.Errorf("名义 = %.4f, 期望单币上限 50", n)
	}
	if math.Abs(m-5) > 1e-9 || math.Abs(amt-5) > 1e-9 {
		t.Errorf("保证金=%.4f 数量=%.4f, 期望 5/5", m, amt)
	}
	// ATR 因子: ATR%=20% → factor=0.4 → base≈56.7，仍被单币上限截断
	e.states["X"] = &symbolState{atr: 2.0}
	n2, _, _, ok2 := e.v6Sizing("X", 70, "small", 10)
	if !ok2 || math.Abs(n2-50) > 1e-9 {
		t.Errorf("ATR 高波动后名义 = %.4f ok=%v, 期望仍 50（上限截断）", n2, ok2)
	}
	// 总敞口: 名义用满后拒绝
	e.notionalInUse = 2950
	_, _, _, ok3 := e.v6Sizing("X", 70, "small", 10)
	if !ok3 {
		t.Error("敞口 2950+50=3000 恰在 3x 上限，应允许")
	}
	e.notionalInUse = 2951
	if _, _, _, ok4 := e.v6Sizing("X", 70, "small", 10); ok4 {
		t.Error("敞口 2951+50>3000 应拒绝")
	}
}

// TestS01FundingVeto 费率过热否决（S01 实验）: 分级阈值 / fail-open。
func TestS01FundingVeto(t *testing.T) {
	e := testV6Engine()
	applyS01Experiments(e.cfg, true, 0, false)
	e.fundRate["BIGUSDT"] = 0.004     // 大币阈值 0.005，未过热
	e.fundRate["BIGHOTUSDT"] = 0.005  // 大币阈值 0.005，过热
	e.fundRate["MIDHOTUSDT"] = 0.02   // 中币阈值 0.01，过热
	e.fundRate["SMALLHOTUSDT"] = 0.02 // 小币阈值 0.02，过热（>=）
	e.fundRate["NEGUSDT"] = -0.01     // 负费率放行

	if e.fundingVetoed("BIGUSDT", 600000000) {
		t.Error("大币费率 0.004 < 0.005 不应否决")
	}
	if !e.fundingVetoed("BIGHOTUSDT", 600000000) {
		t.Error("大币费率 0.005 ≥ 0.005 应否决")
	}
	if !e.fundingVetoed("MIDHOTUSDT", 100000000) {
		t.Error("中币费率 0.02 ≥ 0.01 应否决")
	}
	if !e.fundingVetoed("SMALLHOTUSDT", 20000000) {
		t.Error("小币费率 0.02 ≥ 0.02 应否决")
	}
	if e.fundingVetoed("NEGUSDT", 20000000) {
		t.Error("负费率不应否决")
	}
	if e.fundingVetoed("UNKNOWN", 20000000) {
		t.Error("费率数据缺失应放行（fail-open）")
	}

	// 开关关闭时一律放行
	e2 := testV6Engine()
	e2.fundRate["BIGHOTUSDT"] = 0.05
	if e2.fundingVetoed("BIGHOTUSDT", 600000000) {
		t.Error("开关关闭时不应否决")
	}
}

// TestS01VolumeZ 成交量 Z 阈值（S01 实验）: 尖峰通过、平量不通过。
func TestS01VolumeZ(t *testing.T) {
	e := testV6Engine()
	applyS01Experiments(e.cfg, false, 2.0, false)
	n := 40
	closes := make([]float64, n)
	vols := make([]float64, n)
	for i := 0; i < n; i++ {
		closes[i] = 100 + float64(i)*0.1
		vols[i] = 40000 + 3000*math.Sin(float64(i)/3.0)
	}
	vols[n-1] = 120000 // 3x 尖峰
	st := testFeedBars(e, "X", closes, vols, 1700000000000)
	if z := v6VolumeZ(st, e.cfg); z < 2.0 {
		t.Errorf("3x 放量 Z = %.2f, 期望 ≥ 2.0", z)
	}
	// 平量（无尖峰）→ Z 低
	e2 := testV6Engine()
	applyS01Experiments(e2.cfg, false, 2.0, false)
	vols2 := make([]float64, n)
	for i := range vols2 {
		vols2[i] = 40000
	}
	st2 := testFeedBars(e2, "X", closes, vols2, 1700000000000)
	if z := v6VolumeZ(st2, e2.cfg); z >= 2.0 {
		t.Errorf("平量 Z = %.2f, 期望 < 2.0", z)
	}
}

// TestCanAddOn 追加仓位判定（与实盘 EnableAddOn 语义一致）。
func TestCanAddOn(t *testing.T) {
	e := testV6Engine()
	e.cfg.EnableAddOn = true
	e.cfg.MaxAddOnsPerSymbol = 1
	c := candidate{symbol: "X", side: "LONG"}

	// 无持仓 → 不允许追加
	if e.canAddOn(c) {
		t.Error("无持仓时不应允许追加")
	}
	// 持仓未激活移动止盈 → 不允许
	e.positions = append(e.positions, &Position{Symbol: "X", Side: "LONG", TrailingActive: false})
	if e.canAddOn(c) {
		t.Error("移动止盈未激活时不应允许追加")
	}
	// 移动止盈已激活 → 允许
	e.positions[0].TrailingActive = true
	if !e.canAddOn(c) {
		t.Error("移动止盈已激活时应允许追加")
	}
	// 已达 1+1 上限 → 不允许
	e.positions = append(e.positions, &Position{Symbol: "X", Side: "LONG", TrailingActive: true})
	if e.canAddOn(c) {
		t.Error("同币已达两仓上限时不应允许追加")
	}
	// 开关关闭 → 不允许
	e2 := testV6Engine()
	e2.positions = append(e2.positions, &Position{Symbol: "X", Side: "LONG", TrailingActive: true})
	if e2.canAddOn(c) {
		t.Error("EnableAddOn 关闭时不应允许追加")
	}
}

// TestCanAddOn_Threshold 独立追单门槛（-addonact）验证：
// 门槛 >0 时按同币持仓极值判断，过滤"小冲高即追顶"；=0 时沿用移动止盈激活状态。
func TestCanAddOn_Threshold(t *testing.T) {
	e := testV6Engine()
	e.cfg.EnableAddOn = true
	e.cfg.MaxAddOnsPerSymbol = 2
	e.cfg.AddOnActPct = 0.05 // 门槛 5%
	c := candidate{symbol: "X", side: "LONG"}

	// 首仓最高价 +3%（低于门槛）→ 不允许追
	e.positions = append(e.positions, &Position{Symbol: "X", Side: "LONG", EntryPrice: 100, ExtremePrice: 103})
	if e.canAddOn(c) {
		t.Error("极值 +3% 低于门槛 5% 时不应允许追加")
	}
	// 首仓最高价 +6%（超过门槛）→ 允许追
	e.positions[0].ExtremePrice = 106
	if !e.canAddOn(c) {
		t.Error("极值 +6% 达到门槛 5% 时应允许追加")
	}
	// 做空方向：极值下跌幅度判断
	e2 := testV6Engine()
	e2.cfg.EnableAddOn = true
	e2.cfg.MaxAddOnsPerSymbol = 1
	e2.cfg.AddOnActPct = 0.05
	c2 := candidate{symbol: "Y", side: "SHORT"}
	e2.positions = append(e2.positions, &Position{Symbol: "Y", Side: "SHORT", EntryPrice: 100, ExtremePrice: 96})
	if e2.canAddOn(c2) {
		t.Error("空头极值 -4% 未达门槛 -5%，不应允许追加")
	}
	e2.positions[0].ExtremePrice = 94
	if !e2.canAddOn(c2) {
		t.Error("空头极值 -6% 达到门槛 -5%，应允许追加")
	}
}

// TestV6SignalChain 完整信号链: 构造挤压→突破序列，信号应触发；
// 破坏 RSI 区间 / 费率过热否决 / 新币过滤后应静默。
func TestV6SignalChain(t *testing.T) {
	e := testV6Engine()
	n := 289
	closes := make([]float64, n)
	vols := make([]float64, n)
	// 0..230: 高波动上行（历史高宽度，供挤压分位）
	for i := 0; i < 231; i++ {
		closes[i] = 92 + 12*float64(i)/288.0 + 1.2*math.Sin(float64(i)/5.0)
		vols[i] = 40000 + 3000*math.Sin(float64(i)/3.0)
	}
	// 231..274: 窄幅零漂移涨跌交替（挤压 + 压低 RSI 进入尾部）
	for i := 231; i < 275; i++ {
		d := 0.1
		if (i-231)%2 == 1 {
			d = -0.1
		}
		closes[i] = closes[i-1] + d
		vols[i] = 40000 + 3000*math.Sin(float64(i)/3.0)
	}
	// 尾部 14 根: 4 涨 0.15 / 5 跌 0.35 / 5 涨 0.40（RSI≈64，末根放量突破上轨）
	tail := []float64{0.15, -0.35, 0.15, -0.35, 0.15, -0.35, 0.15, -0.35, -0.35, 0.40, 0.40, 0.40, 0.40, 0.40}
	for j, d := range tail {
		i := 275 + j
		closes[i] = closes[i-1] + d
		vols[i] = 40000 + 3000*math.Sin(float64(i)/3.0)
	}
	vols[n-1] = 400000
	baseTS := int64(1700000000000)
	st := testFeedBars(e, "X", closes, vols, baseTS)
	// 上市日期人为设为 70 天前（60 天新币过滤已过）
	st.firstTS = baseTS - 70*86400000
	e.fundRate["X"] = 0.0001
	e.fundPrev["X"] = 0.00005
	b := &bar{ts: baseTS + int64(n-1)*300000, open: closes[n-2], high: closes[n-1] + 0.001,
		low: closes[n-1] - 0.001, close: closes[n-1], quoteVol: vols[n-1]}

	// 前置条件诊断（失败时给出可调参依据）
	if st.rsi < 40 || st.rsi > 70 {
		t.Fatalf("RSI = %.2f 不在 [40,70]，调整测试序列", st.rsi)
	}
	if !(b.close > v6BBUpper(st, e.cfg)) {
		t.Fatalf("收盘 %.4f 未突破上轨 %.4f，调整测试序列", b.close, v6BBUpper(st, e.cfg))
	}
	if st.bbSqueezePct >= 0.3 {
		t.Fatalf("挤压分位 = %.3f >= 0.3，调整测试序列", st.bbSqueezePct)
	}

	side, score, tier, ok := e.v6Signal("X", st, b, true)
	if !ok {
		t.Fatalf("信号未触发（side=%s score=%.2f tier=%s），前置条件已满足", side, score, tier)
	}
	if side != "LONG" {
		t.Errorf("方向 = %s, 期望 LONG", side)
	}
	if tier != "small" {
		t.Errorf("分级 = %s, 期望 small（24h 成交额约 1150 万）", tier)
	}
	if score < e.cfg.L3MinScore {
		t.Errorf("加权分 = %.2f < 门槛 %.0f", score, e.cfg.L3MinScore)
	}

	// RSI 破坏: 置为 80 → 不触发
	st.rsi = 80
	if _, _, _, ok2 := e.v6Signal("X", st, b, true); ok2 {
		t.Error("RSI=80 应被趋势确认过滤")
	}
	st.rsi = 60

	// 费率过热否决: 小币阈值 0.02
	e.fundRate["X"] = 0.02
	if _, _, _, ok3 := e.v6Signal("X", st, b, true); ok3 {
		t.Error("费率 0.02 ≥ 小币否决阈值 0.02 应一票否决")
	}
	e.fundRate["X"] = 0.0001

	// 新币过滤: 上市不足 60 天
	st.firstTS = baseTS + int64(n-1)*300000 - 30*86400000
	if _, _, _, ok4 := e.v6Signal("X", st, b, true); ok4 {
		t.Error("上市 30 天应被新币过滤拦截")
	}
}
