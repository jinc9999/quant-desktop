// v6 异动币策略（回测实现）
// 定义见 docs/superpowers/plans/2026-08-08-v6-refactor-plan.md。
// 信号链: L1 候选筛选(5m) → L2 波幅扩张检测(5m 收盘) → L3 多因子加权 → L4 动态仓位/滑点 → L5 退出。
// 回测口径: 因子4(盘口深度)归零，权重归一化到前 3 因子；ΔOI 用成交量 Z-Score 代理。
package main

import (
	"fmt"
	"math"
)

// V6Defaults 将回测配置切换为 v6 口径（数值与总任务文档 §四 一致）
func V6Defaults(cfg *StrategyConfig) {
	cfg.Mode = "v6"
	cfg.MinGainPct = 0 // v6 不使用 15m 实体涨幅门槛
	cfg.MinQuoteVolume = 10000000
	cfg.Min24hGainPct = 8.0
	cfg.TopN = 20
	cfg.MaxOpenPositions = 20
	cfg.Leverage = 10
	cfg.CooldownMs = 60 * 60 * 1000
	cfg.StopLossPct = 0.06
	cfg.TrailingActivation = 0.03
	cfg.TrailingCallback = 0.02
	cfg.MaxHoldBars = 24 // 120 分钟
	cfg.EnableShort = false
	cfg.OnlyShort = false
	cfg.ClosedBarConfirm = true
	cfg.ExitClose = false
	cfg.FeeRate = 0.0004
	cfg.InitialEquity = 1000
	cfg.PositionMarginUSDT = 0 // v6 动态仓位，固定保证金字段不参与

	// v6 专属参数
	cfg.MinPrice = 5.0
	cfg.MaxATRPct = 8.0
	cfg.BBPeriod = 20
	cfg.BBMult = 1.5
	cfg.BBWidthWindow = 288
	cfg.BBWidthMinPct = 30.0
	cfg.RSIPeriod = 14
	cfg.RSIMin = 40.0
	cfg.RSIMax = 70.0
	cfg.L2VolMult = 2.0
	cfg.L2VolLookback = 5
	cfg.L2MinScore = 60.0
	cfg.L3MinScore = 70.0
	cfg.OIZWindow = 24
	cfg.TierBigVolume = 500000000
	cfg.TierMidVolume = 50000000
	cfg.OIZBig = 2.5
	cfg.OIZMid = 2.0
	cfg.OIZSmall = 1.5
	cfg.FundVetoBig = 0.005
	cfg.FundVetoMid = 0.01
	cfg.FundVetoSmall = 0.02
	cfg.FactorW1 = 0.35
	cfg.FactorW2 = 0.30
	cfg.FactorW3 = 0.20
	cfg.FactorW4 = 0.15
	cfg.RiskPct = 0.01
	cfg.SingleCoinMarginPct = 0.005
	cfg.MaxLeverageExposure = 3.0
	cfg.DailyLossPct = 0.02
	cfg.MaxConsecutiveLosses = 5
	cfg.SlippageBig = 0.0005
	cfg.SlippageMid = 0.01
	cfg.SlippageSmall = 0.02
	cfg.ATRDecayPct = 0.5
	cfg.ATRDecayMinHoldBars = 6
	cfg.FundReversalMult = 1.5
	cfg.NewListingMinDays = 60
}

// ============ 环形缓冲工具（WindowBars=288 与 symbolState 的 ring 布局一致）============

// ringAt 返回环形缓冲中相对最新值的第 back 个值（back=0 为最新一根）。
// idx 为「下一个写入槽」位置（写入后自增，最新值位于 idx-1）。
func ringAt(arr *[WindowBars]float64, idx, back int) float64 {
	return arr[(idx-1-back%WindowBars+WindowBars*2)%WindowBars]
}

// ringMean 返回最近 n 个值（含最新）的均值。
func ringMean(arr *[WindowBars]float64, idx, n int) float64 {
	if n <= 0 {
		return 0
	}
	sum := 0.0
	for i := 0; i < n; i++ {
		sum += ringAt(arr, idx, i)
	}
	return sum / float64(n)
}

// ringStd 返回最近 n 个值（含最新）的总体标准差。
func ringStd(arr *[WindowBars]float64, idx, n int) float64 {
	if n <= 1 {
		return 0
	}
	m := ringMean(arr, idx, n)
	s := 0.0
	for i := 0; i < n; i++ {
		d := ringAt(arr, idx, i) - m
		s += d * d
	}
	return math.Sqrt(s / float64(n))
}

// ringAvgVol 返回前 n 根（不含当前）成交量的均值；样本不足返回 0。
func ringAvgVol(st *symbolState, n int) float64 {
	if st.filled < n+1 {
		return 0
	}
	sum := 0.0
	for i := 1; i <= n; i++ {
		sum += ringAt(&st.quoteVols, st.idx, i)
	}
	return sum / float64(n)
}

// ============ v6 指标维护（updateState 内调用，每根 K 线一次）============

// updateV6Indicators 维护 RSI(14, Wilder)、ATR(14)、布林带宽度历史与上市日。
func (e *Engine) updateV6Indicators(st *symbolState, b *bar) {
	if st.firstTS == 0 {
		st.firstTS = b.ts
	}
	if st.filled < 2 {
		return // RSI/ATR 需要前一根收盘价
	}

	// ATR(14): TR = max(H-L, |H-prevC|, |L-prevC|)
	prevC := ringAt(&st.closes, st.idx, 1)
	tr := b.high - b.low
	if h := math.Abs(b.high - prevC); h > tr {
		tr = h
	}
	if l := math.Abs(b.low - prevC); l > tr {
		tr = l
	}
	if st.trFilled < len(st.trRing) {
		st.trRing[st.trFilled] = tr
		st.trFilled++
		if st.trFilled == len(st.trRing) {
			sum := 0.0
			for i := 0; i < len(st.trRing); i++ {
				sum += st.trRing[i]
			}
			st.atr = sum / float64(len(st.trRing))
		}
	} else {
		old := st.trRing[st.trIdx]
		st.trRing[st.trIdx] = tr
		st.trIdx = (st.trIdx + 1) % len(st.trRing)
		st.atr += (tr - old) / float64(len(st.trRing))
	}

	// RSI(14, Wilder): 前 14 个 delta 用简单平均播种，之后 Wilder 平滑
	delta := b.close - prevC
	if !st.rsiInit {
		st.rsiSeedSumG += math.Max(delta, 0)
		st.rsiSeedSumL += math.Max(-delta, 0)
		st.rsiSeedCnt++
		if st.rsiSeedCnt == e.cfg.RSIPeriod {
			st.rsiAvgGain = st.rsiSeedSumG / float64(e.cfg.RSIPeriod)
			st.rsiAvgLoss = st.rsiSeedSumL / float64(e.cfg.RSIPeriod)
			st.rsi = rsiValue(st.rsiAvgGain, st.rsiAvgLoss)
			st.rsiInit = true
		}
	} else {
		k := float64(e.cfg.RSIPeriod)
		st.rsiAvgGain = (st.rsiAvgGain*(k-1) + math.Max(delta, 0)) / k
		st.rsiAvgLoss = (st.rsiAvgLoss*(k-1) + math.Max(-delta, 0)) / k
		st.rsi = rsiValue(st.rsiAvgGain, st.rsiAvgLoss)
	}

	// 布林带宽度历史（分位计算用，每根都记录保证序列完整）。
	// 挤压判定必须用「上一根已完成 K 线」的宽度分位（突破当根的宽度会随价格扩张，
	// 若把突破当根计入，挤压前置条件在突破时永远不成立——v6 语义为挤压先于突破）。
	// 仅 v6 模式维护（momentum 实验不需要 O(288) 的宽度历史）。
	if e.cfg.Mode == "v6" && st.filled >= e.cfg.BBPeriod {
		w := v6BBWidth(st, e.cfg)
		if st.bbHasPrev {
			st.bbSqueezePct = v6BBPercentile(st, st.bbWidthPrev, e.cfg)
		}
		if st.bbFilled < e.cfg.BBWidthWindow {
			st.bbWidths[st.bbFilled] = w
			st.bbFilled++
		} else {
			st.bbWidths[st.bbIdx] = w
			st.bbIdx = (st.bbIdx + 1) % e.cfg.BBWidthWindow
		}
		st.bbWidthPrev = w
		st.bbHasPrev = true
	}
}

// rsiValue 由平均涨幅/平均跌幅计算 RSI（0~100）。
func rsiValue(avgGain, avgLoss float64) float64 {
	if avgLoss == 0 {
		if avgGain == 0 {
			return 50
		}
		return 100
	}
	rs := avgGain / avgLoss
	return 100 - 100/(1+rs)
}

// ============ 布林带 / 指标 ============

// v6BBUpper 布林带上轨 = MA20 + 1.5σ。
func v6BBUpper(st *symbolState, cfg *StrategyConfig) float64 {
	m := ringMean(&st.closes, st.idx, cfg.BBPeriod)
	return m + cfg.BBMult*ringStd(&st.closes, st.idx, cfg.BBPeriod)
}

// v6BBWidth 布林带相对宽度 = (上轨-下轨)/中轨。
func v6BBWidth(st *symbolState, cfg *StrategyConfig) float64 {
	m := ringMean(&st.closes, st.idx, cfg.BBPeriod)
	if m <= 0 {
		return 0
	}
	upper := v6BBUpper(st, cfg)
	lower := m*2 - upper
	if upper <= lower {
		return 0
	}
	return (upper - lower) / m
}

// v6BBPercentile 当前宽度在其历史宽度序列（不含当前）中的百分位（0~1）。
func v6BBPercentile(st *symbolState, w float64, cfg *StrategyConfig) float64 {
	n := st.bbFilled
	if n > cfg.BBWidthWindow {
		n = cfg.BBWidthWindow
	}
	if n == 0 {
		return 1
	}
	c := 0
	for i := 0; i < n; i++ {
		if st.bbWidths[i] < w {
			c++
		}
	}
	return float64(c) / float64(n)
}

// v6VolumeZ 成交量 Z-Score（ΔOI 因子在回测中的代理）：
// 当前根相对前 OIZWindow 根成交量的偏离程度。
func v6VolumeZ(st *symbolState, cfg *StrategyConfig) float64 {
	n := cfg.OIZWindow
	if st.filled < n+2 {
		return 0
	}
	sum, sumSq := 0.0, 0.0
	for i := 1; i <= n; i++ {
		v := ringAt(&st.quoteVols, st.idx, i)
		sum += v
		sumSq += v * v
	}
	mean := sum / float64(n)
	var2 := sumSq/float64(n) - mean*mean
	if var2 <= 0 {
		return 0
	}
	cur := ringAt(&st.quoteVols, st.idx, 0)
	return (cur - mean) / math.Sqrt(var2)
}

// ============ L1 / L2 / L3 信号链 ============

// v6Signal 完整评估 v6 信号链。返回 (方向, L3加权总分, 币种分级, 是否开仓)。
func (e *Engine) v6Signal(symbol string, st *symbolState, b *bar, ready24 bool) (string, float64, string, bool) {
	cfg := e.cfg
	if !ready24 || st.filled < cfg.BBPeriod || st.firstTS == 0 {
		return "", 0, "", false
	}
	e.v6Gates[0]++
	// 冷却期: 平仓后 CooldownMin 分钟内不再开同一币
	if st.lastClose > 0 && b.ts-st.lastClose < cfg.CooldownMs {
		return "", 0, "", false
	}
	e.v6Gates[1]++
	// L1-新币过滤: 上市天数 > NewListingMinDays
	if b.ts-st.firstTS < int64(cfg.NewListingMinDays)*86400000 {
		return "", 0, "", false
	}
	e.v6Gates[2]++
	// L1-价格 > 5U
	if b.close <= cfg.MinPrice {
		return "", 0, "", false
	}
	// L1-ATR% ≤ 8%
	if st.atr <= 0 || st.atr/b.close*100 > cfg.MaxATRPct {
		return "", 0, "", false
	}
	// L1-24h 成交额 ≥ 1000 万
	if st.sumVol24 < cfg.MinQuoteVolume {
		return "", 0, "", false
	}
	// L1-24h 涨幅 ≥ 8%（back=287 即 288 根前；back=288 会绕回最新值）
	old := ringAt(&st.closes, st.idx, WindowBars-1)
	if old <= 0 {
		return "", 0, "", false
	}
	if (b.close-old)/old*100 < cfg.Min24hGainPct {
		return "", 0, "", false
	}
	e.v6Gates[3]++
	// L1-预筛量能: 当前 5m 成交量 ≥ 前 20 根均量 × 1.5
	avg20 := ringAvgVol(st, 20)
	curVol := ringAt(&st.quoteVols, st.idx, 0)
	if avg20 <= 0 || curVol < avg20*1.5 {
		return "", 0, "", false
	}
	e.v6Gates[4]++
	// L2-硬门槛: 上一根宽度分位 < 30%（至少 96 根历史 = 8h）
	if st.bbFilled < 96 || !st.bbHasPrev {
		return "", 0, "", false
	}
	if st.bbSqueezePct >= cfg.BBWidthMinPct/100 {
		return "", 0, "", false
	}
	e.v6Gates[5]++
	// L2-触发: 5m 收盘 > 上轨（MA20+1.5σ）
	upper := v6BBUpper(st, cfg)
	if b.close <= upper {
		return "", 0, "", false
	}
	// L2-方向确认: 阳线 + 实体/影线比 > 0.6
	body := math.Abs(b.close - b.open)
	rng := b.high - b.low
	if b.close <= b.open || rng <= 0 || body/rng <= 0.6 {
		return "", 0, "", false
	}
	// L2-量能确认: (当前+前1根) ≥ 前 5 根均值 × 2.0
	prev1 := ringAt(&st.quoteVols, st.idx, 1)
	avg5 := ringAvgVol(st, cfg.L2VolLookback)
	if avg5 <= 0 || (curVol+prev1) < avg5*cfg.L2VolMult {
		return "", 0, "", false
	}
	// L2-趋势确认: RSI(14) ∈ [40,70]
	if !st.rsiInit || st.rsi < cfg.RSIMin || st.rsi > cfg.RSIMax {
		return "", 0, "", false
	}
	// L2-原始分 ≥ 60
	if v6RawScore(b, st, cfg, upper, curVol, prev1, avg5) < cfg.L2MinScore {
		return "", 0, "", false
	}
	e.v6Gates[6]++
	// L3-多因子加权（因子4 盘口深度回测归零，权重归一化到前 3 因子）
	tier := e.v6Tier(st.sumVol24)
	f1 := v6FactorOI(v6VolumeZ(st, cfg), tier, cfg)
	curF, prevF := e.fundRate[symbol], e.fundPrev[symbol]
	f2, veto := v6FactorFunding(curF, prevF, tier, cfg)
	if veto {
		return "", 0, "", false // 费率过热一票否决
	}
	f3 := v6FactorRSI(st.rsi)
	wSum := cfg.FactorW1 + cfg.FactorW2 + cfg.FactorW3
	score := (cfg.FactorW1*f1 + cfg.FactorW2*f2 + cfg.FactorW3*f3) / wSum
	if score < cfg.L3MinScore {
		return "", 0, "", false
	}
	e.v6Gates[7]++
	return "LONG", score, tier, true
}

// printV6GateStats 输出 v6 信号漏斗统计（定位信号在哪一级被拦截）。
func (e *Engine) printV6GateStats() {
	fmt.Printf("\n--- v6 信号漏斗（各阶段累计通过数）---\n")
	names := []string{
		"就绪(24h/BB/上市日)",
		"冷却通过",
		"新币过滤通过(>60d)",
		"L1基础(价/ATR/额/24h涨)",
		"L1量能预筛(20均量x1.5)",
		"L2挤压分位<30%",
		"L2触发+方向+量能+RSI+原始分",
		"L3加权分>=70(开仓信号)",
	}
	base := e.v6Gates[0]
	for i, name := range names {
		pct := 0.0
		if base > 0 {
			pct = float64(e.v6Gates[i]) / float64(base) * 100
		}
		fmt.Printf("  %s: %d (%.2f%%)\n", name, e.v6Gates[i], pct)
	}
	fmt.Printf("--- v6 开仓拦截统计 ---\n")
	fmt.Printf("  熔断拦截(日亏/连亏): %d\n", e.v6Skip[0])
	fmt.Printf("  仓位/敞口/破产拦截: %d\n", e.v6Skip[1])
	fmt.Printf("  已持仓去重: %d\n", e.v6Skip[2])
	fmt.Printf("  实际成交: %d\n", e.v6Skip[3])
}

// applyS01Experiments 启用 S01 单因子实验并补齐其依赖的默认参数（实验开关默认全关）。
func applyS01Experiments(cfg *StrategyConfig, fveto bool, vz float64, rsiok bool) {
	if fveto {
		cfg.FundingVetoEnabled = true
		cfg.TierBigVolume = 500000000
		cfg.TierMidVolume = 50000000
		cfg.FundVetoBig = 0.005
		cfg.FundVetoMid = 0.01
		cfg.FundVetoSmall = 0.02
	}
	if vz > 0 {
		cfg.VolumeZThreshold = vz
		cfg.OIZWindow = 24
	}
	if rsiok {
		cfg.RSIFilterEnabled = true
		cfg.RSIPeriod = 14
		cfg.RSIMin = 40
		cfg.RSIMax = 70
	}
}

// fundingVetoed 费率过热否决（S01 实验）: 正费率 ≥ 分级阈值不追（做多场景）。
// 数据缺失或费率非正时放行（fail-open，与项目安全降级哲学一致）。
func (e *Engine) fundingVetoed(symbol string, vol24 float64) bool {
	if !e.cfg.FundingVetoEnabled {
		return false
	}
	cur, ok := e.fundRate[symbol]
	if !ok || cur <= 0 {
		return false
	}
	return cur >= e.v6Veto(e.v6Tier(vol24))
}

// v6RawScore L2 原始分（0~100，推荐初值，回测校准）。
func v6RawScore(b *bar, st *symbolState, cfg *StrategyConfig, upper, curVol, prev1, avg5 float64) float64 {
	s := 50.0
	// 突破强度: close/上轨超出每 0.5% +10，上限 +20
	if upper > 0 {
		excess := b.close/upper - 1
		s += math.Min(20, excess/0.005*10)
	}
	// 量能: 2.0x 起 +10，每多 1x +5，上限 +15
	if avg5 > 0 {
		surge := (curVol + prev1) / avg5
		if surge >= cfg.L2VolMult {
			s += math.Min(15, 10+(surge-cfg.L2VolMult)*5)
		}
	}
	// RSI 位置: 40-55 → 15; 55-65 → 10; 65-70 → 5
	switch {
	case st.rsi >= 40 && st.rsi <= 55:
		s += 15
	case st.rsi > 55 && st.rsi <= 65:
		s += 10
	case st.rsi > 65 && st.rsi <= 70:
		s += 5
	}
	// 实体占比: >0.8 → 10; 0.6~0.8 → 5
	rng := b.high - b.low
	if rng > 0 {
		ratio := math.Abs(b.close-b.open) / rng
		if ratio > 0.8 {
			s += 10
		} else if ratio >= 0.6 {
			s += 5
		}
	}
	return s
}

// v6FactorOI ΔOI(Z) 因子: Z ≥ 分级阈值 → 100；否则 Z/阈值×100 线性（下限 0）。
func v6FactorOI(z float64, tier string, cfg *StrategyConfig) float64 {
	th := cfg.OIZSmall
	switch tier {
	case "big":
		th = cfg.OIZBig
	case "mid":
		th = cfg.OIZMid
	}
	if th <= 0 {
		return 50
	}
	if z >= th {
		return 100
	}
	return math.Max(0, z/th*100)
}

// v6FactorFunding 资金费率因子: 负转正 → 100；负费率 → 50；正费率按接近 0 递减。
// 返回 (得分, 是否过热一票否决)。
func v6FactorFunding(cur, prev float64, tier string, cfg *StrategyConfig) (float64, bool) {
	veto := cfg.FundVetoSmall
	switch tier {
	case "big":
		veto = cfg.FundVetoBig
	case "mid":
		veto = cfg.FundVetoMid
	}
	if cur >= veto {
		return 0, true
	}
	if prev <= 0 && cur > 0 {
		return 100, false
	}
	if cur <= 0 {
		return 50, false
	}
	if veto <= 0 {
		return 50, false
	}
	return 100 * math.Max(0, 1-cur/veto), false
}

// v6FactorRSI RSI 因子（趋势强度确认，非否决）。
func v6FactorRSI(rsi float64) float64 {
	switch {
	case rsi >= 55 && rsi <= 65:
		return 100
	case (rsi >= 45 && rsi < 55) || (rsi > 65 && rsi <= 70):
		return 70
	case rsi >= 40 && rsi < 45:
		return 50
	}
	return 0
}

// ============ 币种分级 / 滑点 / 否决阈值 / 仓位 ============

// v6Tier 按 24h 成交额分级: big ≥5亿 / mid ≥5000万 / small。
func (e *Engine) v6Tier(vol24 float64) string {
	if vol24 >= e.cfg.TierBigVolume {
		return "big"
	}
	if vol24 >= e.cfg.TierMidVolume {
		return "mid"
	}
	return "small"
}

// v6Slippage 分级滑点。
func (e *Engine) v6Slippage(tier string) float64 {
	switch tier {
	case "big":
		return e.cfg.SlippageBig
	case "mid":
		return e.cfg.SlippageMid
	}
	return e.cfg.SlippageSmall
}

// v6Veto 分级费率过热否决阈值。
func (e *Engine) v6Veto(tier string) float64 {
	switch tier {
	case "big":
		return e.cfg.FundVetoBig
	case "mid":
		return e.cfg.FundVetoMid
	}
	return e.cfg.FundVetoSmall
}

// v6Sizing L4 动态仓位:
// base = 账户×1%×ATR因子×置信度因子 / 6%止损距离；
// 名义价值 = min(base, 单币上限=账户×0.5%×杠杆)；
// 同时校验总敞口 ≤ 账户×3 与破产保护。
// 返回 (名义价值, 保证金, 数量, 是否可开)。
func (e *Engine) v6Sizing(symbol string, score float64, tier string, entryPrice float64) (float64, float64, float64, bool) {
	cfg := e.cfg
	if entryPrice <= 0 || score <= 0 {
		return 0, 0, 0, false
	}
	// ATR 因子: 波动越大仓位越小（基准 ATR% = L1 上限 8%）
	atrFactor := 1.0
	if st := e.states[symbol]; st != nil && st.atr > 0 {
		atrPct := st.atr / entryPrice * 100
		if atrPct > 0 {
			atrFactor = math.Min(1, cfg.MaxATRPct/atrPct)
		}
	}
	// 置信度因子: 0.5 + score/200（70 分 → 0.85，100 分 → 1.0）
	conf := 0.5 + score/200
	base := e.equity * cfg.RiskPct * atrFactor * conf / cfg.StopLossPct
	// 单币上限: 保证金 ≤ 账户×0.5% → 名义 ≤ 账户×0.5%×杠杆
	capN := e.equity * cfg.SingleCoinMarginPct * cfg.Leverage
	notional := math.Min(base, capN)
	if notional <= 0 {
		return 0, 0, 0, false
	}
	// 总敞口: 名义总和 ≤ 账户×3
	if e.notionalInUse+notional > e.equity*cfg.MaxLeverageExposure {
		return 0, 0, 0, false
	}
	margin := notional / cfg.Leverage
	// 破产保护
	if e.equity-e.marginInUse < margin {
		return 0, 0, 0, false
	}
	return notional, margin, notional / entryPrice, true
}
