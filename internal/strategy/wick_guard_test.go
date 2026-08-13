package strategy

import "testing"

// TestWickGuard_VerifyBreakdown 复刻 Python 伪代码 verify_breakdown 的核心逻辑：
// 窗口内所有最新价低于止损 且 标记价低于止损 才确认；插针（瞬间击穿又弹回）被忽略。
func TestWickGuard_VerifyBreakdown(t *testing.T) {
	g := NewWickGuard(3000, 3) // 3 秒窗口、至少 3 个样本（按秒推送场景）
	base := int64(1_000_000)
	// 插针场景：最新价瞬间击穿 95，但标记价始终未击穿 → 不确认
	g.Push(90, 96, base)      // 最新价 90 击穿，标记价 96 未击穿
	g.Push(94, 95.5, base+1000)
	g.Push(93, 95.2, base+2000)
	if g.ConfirmBreak(95, base+2000) {
		t.Fatalf("标记价未击穿不应确认（插针）")
	}
	// 价格弹回 100（插针结束）→ 不确认
	g.Push(100, 99, base+3000)
	if g.ConfirmBreak(95, base+3000) {
		t.Fatalf("插针弹回应被忽略")
	}
	// 真实击穿场景：连续 3 秒最新价与标记价都低于 95 → 确认
	g2 := NewWickGuard(3000, 3)
	g2.Push(94, 94, base)
	g2.Push(93, 93.5, base+1000)
	g2.Push(92, 92.8, base+2000)
	if !g2.ConfirmBreak(95, base+2000) {
		t.Fatalf("多源+持续确认应通过")
	}
}

// TestWickGuard_InsufficientData 数据太少不触发（宁可等待，不误杀）。
func TestWickGuard_InsufficientData(t *testing.T) {
	g := NewWickGuard(3000, 3)
	g.Push(94, 94, 1_000_000)
	if g.ConfirmBreak(95, 1_000_000) {
		t.Fatalf("样本不足不应确认")
	}
}

// TestWickGuard_15sTick 引擎 15s 一个 tick 的场景：窗口 45s、至少 2 个样本。
func TestWickGuard_15sTick(t *testing.T) {
	g := NewWickGuard(45*1000, 2)
	base := int64(2_000_000)
	g.Push(94, 94, base)       // tick1：击穿
	if g.ConfirmBreak(95, base) {
		t.Fatalf("仅 1 个样本不应确认")
	}
	g.Push(93, 93, base+15000) // tick2（15s 后）：仍击穿 → 确认
	if !g.ConfirmBreak(95, base+15000) {
		t.Fatalf("连续 2 个 tick 击穿且标记价确认应通过")
	}
	g.Push(96, 95.5, base+30000) // tick3：弹回 → 不再确认
	if g.ConfirmBreak(95, base+30000) {
		t.Fatalf("弹回后不应确认")
	}
}

// TestWickGuard_MarkFallback 标记价拉取失败时用最新价兜底（单源也能确认，不丢保护）。
func TestWickGuard_MarkFallback(t *testing.T) {
	g := NewWickGuard(45*1000, 2)
	base := int64(3_000_000)
	g.Push(94, 0, base) // mark<=0 → 兜底为 last
	g.Push(93, 0, base+15000)
	if !g.ConfirmBreak(95, base+15000) {
		t.Fatalf("标记价缺失时单源兜底应能确认（不丢保护）")
	}
}
