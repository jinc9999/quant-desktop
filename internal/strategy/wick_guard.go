package strategy

// PriceSample 一次价格快照（最新价 + 标记价 + 时间戳）。
// 多源验证：最新价是"某个摊位的喊价"，标记价是"菜市场官方公告牌"，
// 两者都确认才认为是真实价格，防止单个极端成交价（插针）触发误动作。
type PriceSample struct {
	Last float64
	Mark float64
	Ts   int64 // Unix 毫秒
}

// WickGuard 插针守卫：多源 + 持续时间确认（Python 伪代码 verify_breakdown 的 Go 实现）。
//
// 判定规则：时间窗口内所有样本的最新价与标记价都低于止损线（做多方向），
// 且样本数足够，才判定为"真实击穿"；瞬间插针（先击穿又弹回）会被直接忽略。
// 注意引擎 tick 为 15s：窗口应按 tick 数设置（如 3 个 tick = 45s，minSamples=2），
// 不要照搬 Python 版按秒推送的 3s/3 个样本（15s 一个 tick 下永远凑不够）。
type WickGuard struct {
	windowMs   int64
	minSamples int
	samples    []PriceSample
}

// NewWickGuard 创建插针守卫
// windowMs: 确认时间窗口（毫秒），窗口内所有样本必须一致
// minSamples: 最少样本数（数据太少不确认，宁可等待）
func NewWickGuard(windowMs int64, minSamples int) *WickGuard {
	if windowMs <= 0 {
		windowMs = 3000
	}
	if minSamples <= 0 {
		minSamples = 3
	}
	return &WickGuard{windowMs: windowMs, minSamples: minSamples}
}

// Push 记录一个价格快照，并裁剪窗口外的旧样本。
// mark<=0 时（标记价拉取失败）用 last 兜底填充，保证单源也能确认（不丢保护）。
func (g *WickGuard) Push(last, mark float64, ts int64) {
	if last <= 0 {
		return
	}
	if mark <= 0 {
		mark = last
	}
	cutoff := ts - g.windowMs
	kept := g.samples[:0]
	for _, s := range g.samples {
		if s.Ts >= cutoff {
			kept = append(kept, s)
		}
	}
	g.samples = append(kept, PriceSample{Last: last, Mark: mark, Ts: ts})
}

// ConfirmBreak 做多方向：窗口内所有样本 last<=stop 且 mark<=stop 才算真实击穿。
func (g *WickGuard) ConfirmBreak(stopPrice float64, ts int64) bool {
	return g.confirm(stopPrice, ts, true)
}

// ConfirmRise 做空方向：窗口内所有样本 last>=stop 且 mark>=stop 才算真实击穿。
func (g *WickGuard) ConfirmRise(stopPrice float64, ts int64) bool {
	return g.confirm(stopPrice, ts, false)
}

func (g *WickGuard) confirm(stopPrice float64, ts int64, long bool) bool {
	if stopPrice <= 0 || len(g.samples) < g.minSamples {
		return false
	}
	cutoff := ts - g.windowMs
	n := 0
	for _, s := range g.samples {
		if s.Ts < cutoff {
			continue
		}
		n++
		if long {
			if s.Last > stopPrice || s.Mark > stopPrice {
				return false
			}
		} else {
			if s.Last < stopPrice || s.Mark < stopPrice {
				return false
			}
		}
	}
	return n >= g.minSamples
}

// Len 当前缓冲样本数（测试/观测用）
func (g *WickGuard) Len() int { return len(g.samples) }
