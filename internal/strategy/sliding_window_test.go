// Package strategy 滑动窗口单元测试
package strategy

import (
	"fmt"
	"testing"
)

// almostEqual 判断两个浮点数是否在容差内相等
// a, b: 待比较的浮点数
// 返回: 差值绝对值小于 1e-9 时返回 true
func almostEqual(a, b float64) bool {
	d := a - b
	if d < 0 {
		d = -d
	}
	return d < 1e-9
}

// TestParseTimeframeMs 验证周期字符串解析为毫秒
// 覆盖: m/h/s 后缀、非法输入回退默认值
func TestParseTimeframeMs(t *testing.T) {
	cases := []struct {
		in   string
		want int64
	}{
		{"5m", 300000},
		{"1m", 60000},
		{"15m", 900000},
		{"1h", 3600000},
		{"30s", 30000},
		{"", 300000},   // 空字符串回退默认
		{"abc", 300000}, // 非法输入回退默认
		{"x5m", 300000}, // 非法数字回退默认
	}
	for _, c := range cases {
		if got := ParseTimeframeMs(c.in); got != c.want {
			t.Errorf("ParseTimeframeMs(%q) = %d, 期望 %d", c.in, got, c.want)
		}
	}
}

// TestSlidingWindow_WarmupUsesOldestBaseline 验证预热期用最早采样点作基准
// 仅有 1 分钟前的采样点时，预热期以最早点（100）为基准，涨幅 = (105-100)/100*100 = 5%
func TestSlidingWindow_WarmupUsesOldestBaseline(t *testing.T) {
	w := NewSlidingWindow(300000, 10000)
	now := int64(1000000)

	// 仅喂入 1 分钟前与当前的采样点（预热期，窗口未满）
	w.Add("X", now-60000, 100)
	w.Add("X", now, 105)

	gain, ok := w.GainPct("X", 105, now)
	if !ok {
		t.Fatal("预热期有历史采样点时应返回 ok=true（用最早点作基准）")
	}
	if !almostEqual(gain, 5.0) {
		t.Errorf("gain = %v, 期望 5.0（基准为最早采样点 100）", gain)
	}

	// 验证窗口长度为 60 秒（预热期渐增）
	wLen := w.WindowLengthMs("X", now)
	if wLen != 60000 {
		t.Errorf("WindowLengthMs = %d, 期望 60000（预热期窗口=最早点到now）", wLen)
	}
}

// TestSlidingWindow_GainCalculation 验证滑动涨幅计算
// 基准价 100（5 分钟前），当前价 105，期望涨幅 5%
func TestSlidingWindow_GainCalculation(t *testing.T) {
	w := NewSlidingWindow(300000, 10000)
	now := int64(1000000)

	w.Add("X", now-300000, 100) // 恰好 5 分钟前
	w.Add("X", now, 105)

	gain, ok := w.GainPct("X", 105, now)
	if !ok {
		t.Fatal("窗口应已就绪，ok 期望 true")
	}
	if !almostEqual(gain, 5.0) {
		t.Errorf("gain = %v, 期望 5.0", gain)
	}
}

// TestSlidingWindow_BaselineSlides 验证基准点随时间滑动（核心需求）
// 模拟实时喂入：价格随时间递增，验证不同 now 时刻取到的基准点是"各自 5 分钟前"的价格，
// 而非固定某个整点 K 线 open。
func TestSlidingWindow_BaselineSlides(t *testing.T) {
	w := NewSlidingWindow(300000, 10000)

	// 模拟实时：每 10 秒喂入一个点（价格 = 100 + ts/10000），并在当前时刻查询基准
	var base1, base2 float64
	var ok1, ok2 bool
	for ts := int64(0); ts <= 360000; ts += 10000 {
		w.Add("X", ts, 100+float64(ts/10000))
		switch ts {
		case 300000:
			// now=300000 时，基准应为 ts=0 的价格 100
			base1, ok1 = w.baselinePrice("X", ts)
		case 360000:
			// now=360000 时，基准应滑动到 ts=60000 的价格 106
			base2, ok2 = w.baselinePrice("X", ts)
		}
	}

	if !ok1 || !almostEqual(base1, 100) {
		t.Errorf("now=300000 基准 = %v (ok=%v), 期望 100", base1, ok1)
	}
	if !ok2 || !almostEqual(base2, 106) {
		t.Errorf("now=360000 基准 = %v (ok=%v), 期望 106（基准已滑动）", base2, ok2)
	}
	if almostEqual(base1, base2) {
		t.Error("基准点应随 now 滑动，两次基准不应相同")
	}
}

// TestSlidingWindow_WindowLengthConstant 验证窗口长度恒定
// 模拟实时喂入（价格编码为时间戳），在多个当前时刻验证基准点距 now 恒为约一个完整窗口。
func TestSlidingWindow_WindowLengthConstant(t *testing.T) {
	w := NewSlidingWindow(300000, 10000)
	// 价格编码为时间戳，便于从基准价反推基准点时间
	for ts := int64(0); ts <= 600000; ts += 10000 {
		w.Add("X", ts, float64(ts))
	}

	// 在接近最新时刻查询（实时系统的合法查询时机）
	for _, now := range []int64{580000, 590000, 600000} {
		base, ok := w.baselinePrice("X", now)
		if !ok {
			t.Errorf("now=%d 窗口应就绪", now)
			continue
		}
		// 基准价（即基准点时间戳）应约为 now-300000
		target := float64(now - 300000)
		if d := base - target; d > 10000 || d < -10000 {
			t.Errorf("now=%d 基准 %v 偏离目标 %v 超过容差", now, base, target)
		}
	}
}

// TestSlidingWindow_AddIgnoresNonPositive 验证非法价格被忽略
// price<=0 的采样点不应进入序列
func TestSlidingWindow_AddIgnoresNonPositive(t *testing.T) {
	w := NewSlidingWindow(300000, 10000)
	w.Add("X", 1000, 0)
	w.Add("X", 2000, -5)

	w.mu.RLock()
	n := len(w.series["X"])
	w.mu.RUnlock()
	if n != 0 {
		t.Errorf("非法价格应被忽略，序列长度 = %d, 期望 0", n)
	}
}

// TestSlidingWindow_PruneOldPoints 验证旧采样点被裁剪，内存不无限增长
// 持续喂入跨越多个窗口的数据，序列长度应被限制在窗口+容差范围内
func TestSlidingWindow_PruneOldPoints(t *testing.T) {
	w := NewSlidingWindow(300000, 10000)
	// 喂入 20 分钟的数据（120 个点，每 10 秒一个）
	var lastTs int64
	for ts := int64(0); ts <= 1200000; ts += 10000 {
		w.Add("X", ts, 100)
		lastTs = ts
	}

	w.mu.RLock()
	n := len(w.series["X"])
	w.mu.RUnlock()

	// 窗口 300000 + 容差 10000 = 310000ms，按 10s 采样约 32 个点以内
	if n > 40 {
		t.Errorf("序列未正确裁剪，长度 = %d, 期望 <= 40", n)
	}
	_ = lastTs
}

// TestSlidingWindow_WarmupSimulation 模拟预热期逐 tick 判断过程
//
// 场景：策略启动后每 10 秒喂入一次价格，前 5 分钟为预热期（窗口从 10s 渐增到 5min）。
// 价格设计：前 5 个 tick 平稳（100），第 6 个 tick（60s）跳涨到 105.5，
// 此时以第 0 秒的价格 100 为基准，涨幅 5.5% >= 5%，应立即触发入仓信号。
//
// 本测试逐 tick 打印判断过程，验证：
//  1. 预热期内只要有历史价格就能判断（不等满 5 分钟）
//  2. 窗口长度从 10s 逐渐增长
//  3. 第 60 秒涨幅达标时能正确识别
func TestSlidingWindow_WarmupSimulation(t *testing.T) {
	w := NewSlidingWindow(300000, 10000) // 5 分钟窗口，10 秒采样
	const minGain = 5.0                  // 入仓阈值 5%

	// 模拟价格序列：前 50 秒平稳 100，第 60 秒跳涨到 105.5，之后继续涨
	prices := []float64{
		100.0, // tick 0:  0s
		100.0, // tick 1:  10s
		100.0, // tick 2:  20s
		100.0, // tick 3:  30s
		100.0, // tick 4:  40s
		100.0, // tick 5:  50s
		105.5, // tick 6:  60s  ← 跳涨！以 tick0 为基准涨 5.5%
		106.0, // tick 7:  70s
		107.0, // tick 8:  80s
		108.0, // tick 9:  90s
		109.0, // tick 10: 100s
		110.0, // tick 11: 110s
	}

	fmt.Println("========== 预热期模拟：逐 Tick 判断过程 ==========")
	fmt.Printf("%-6s %-8s %-10s %-10s %-10s %-8s %-8s %-6s\n",
		"Tick", "时间", "窗口长度", "基准价", "现价", "涨幅%", "阈值%", "触发")
	fmt.Println("--------------------------------------------------------------")

	triggered := false
	triggerTick := -1

	for i, price := range prices {
		now := int64(i * 10000) // 每 tick 间隔 10 秒
		w.Add("SIM", now, price)

		wLen := w.WindowLengthMs("SIM", now)
		gain, ready := w.GainPct("SIM", price, now)

		wLenStr := fmt.Sprintf("%.0fs", float64(wLen)/1000)
		gainStr := "-"
		baseStr := "-"
		triggerStr := "-"

		if ready {
			baseline, _ := w.baselinePrice("SIM", now)
			baseStr = fmt.Sprintf("%.2f", baseline)
			gainStr = fmt.Sprintf("%.2f", gain)
			if gain >= minGain {
				triggerStr = ">>> 入仓"
				if !triggered {
					triggered = true
					triggerTick = i
				}
			}
		}

		fmt.Printf("%-6d %-8s %-10s %-10s %-10.2f %-8s %-8.1f %-6s\n",
			i, fmt.Sprintf("%ds", i*10), wLenStr, baseStr, price, gainStr, minGain, triggerStr)
	}

	fmt.Println("==============================================================")

	// 断言 1：第 6 个 tick（60s）应触发入仓
	if !triggered {
		t.Fatal("预热期内涨幅达标应触发入仓信号，但未触发")
	}
	if triggerTick != 6 {
		t.Errorf("首次触发 tick = %d, 期望 6（第 60 秒）", triggerTick)
	}

	// 断言 2：tick 0 无法判断（仅一个采样点）
	if wLen := w.WindowLengthMs("SIM", 0); wLen != 0 {
		t.Errorf("tick 0 窗口长度 = %d, 期望 0（仅一个点无法判断）", wLen)
	}

	// 断言 3：tick 1 窗口长度 = 10s（预热期渐增）
	if wLen := w.WindowLengthMs("SIM", 10000); wLen != 10000 {
		t.Errorf("tick 1 窗口长度 = %d, 期望 10000", wLen)
	}

	// 断言 4：tick 6 窗口长度 = 60s（预热期，用最早点作基准）
	if wLen := w.WindowLengthMs("SIM", 60000); wLen != 60000 {
		t.Errorf("tick 6 窗口长度 = %d, 期望 60000", wLen)
	}

	// 断言 5：tick 6 涨幅 = 5.5%（基准 100，现价 105.5）
	gain, ok := w.GainPct("SIM", 105.5, 60000)
	if !ok || !almostEqual(gain, 5.5) {
		t.Errorf("tick 6 涨幅 = %v (ok=%v), 期望 5.5", gain, ok)
	}
}

// ==================== RecentGainPct / RecentVolumeSurge / BackfillCache 测试 ====================

// TestRecentGainPct 验证短窗口涨幅计算
// 前 12 分钟价格 100，后 3 分钟涨到 103，短窗口（3 分钟）涨幅约 3%
func TestRecentGainPct(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// 喂入 15 分钟数据：前 12 分钟价格 100，后 3 分钟涨到 103
	for i := 0; i < 24; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("TESTUSDT", 100.0, 500000, ts)
	}
	w.Sample("TESTUSDT", 101, 500000, now-120000)
	w.Sample("TESTUSDT", 102, 500000, now-60000)
	w.Sample("TESTUSDT", 103, 500000, now-30000)

	// 最近 3 分钟涨幅：基准为 cutoff(820000) 前最后一个点（ts=790000, price=100）
	// gain = (103 - 100) / 100 * 100 = 3%
	gain, ready := w.RecentGainPct("TESTUSDT", 103, now, 180000)
	if !ready {
		t.Fatal("期望 ready=true")
	}
	if gain < 2.5 || gain > 3.5 {
		t.Errorf("RecentGainPct = %f, 期望约 3.0", gain)
	}

	// 不存在的交易对
	_, ready2 := w.RecentGainPct("NOEXIST", 100, now, 180000)
	if ready2 {
		t.Error("不存在的交易对期望 ready=false")
	}
}

// TestRecentVolumeSurge 验证成交量放大倍数计算
// 前 12 分钟每 30 秒成交量 1000，最近 3 分钟每 30 秒成交量 3000（放量 3 倍）
// 边界取「最后一个 ts <= cutoff 的采样点」的累计值：recentVol=15000, priorVol=20000
// surge = (15000/180000) / (20000/720000) = 3.0
func TestRecentVolumeSurge(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// 前 12 分钟：每 30 秒成交量 1000（累计成交额每次增加 1000）
	cumVol := 0.0
	for i := 0; i < 24; i++ {
		ts := now - int64(30-i)*30000
		cumVol += 1000
		w.Sample("VOLUSDT", 100.0, cumVol, ts)
	}
	// 最近 3 分钟：每 30 秒成交量 3000（放量 3 倍）
	for i := 0; i < 6; i++ {
		ts := now - int64(6-i)*30000
		cumVol += 3000
		w.Sample("VOLUSDT", 100.0, cumVol, ts)
	}

	surge, ready := w.RecentVolumeSurge("VOLUSDT", now, 180000, 720000)
	if !ready {
		t.Fatal("期望 ready=true")
	}
	// surge = (15000/180000) / (20000/720000) = 3.0（放量 3 倍）
	if surge < 2.5 || surge > 3.5 {
		t.Errorf("VolumeSurge = %f, 期望约 3.0", surge)
	}
}

// TestSample_BackfillCache 验证 Sample 和 BackfillCache 的基本功能
// BackfillCache 仅在无数据时写入初始点，已有数据时不覆盖
func TestSample_BackfillCache(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// BackfillCache 首次写入（时间戳早于查询时刻，确保 MaxGainPct 可用）
	w.BackfillCache("BFUSDT", 50.0, 100000, now-300000)
	gain, ready := w.MaxGainPct("BFUSDT", 55, now)
	if !ready {
		t.Fatal("BackfillCache 后期望 ready=true")
	}
	if gain < 9.9 || gain > 10.1 {
		t.Errorf("MaxGainPct = %f, 期望约 10.0", gain)
	}

	// BackfillCache 不覆盖已有数据
	w.BackfillCache("BFUSDT", 999.0, 999999, now+1000)
	gain2, _ := w.MaxGainPct("BFUSDT", 55, now+1000)
	if gain2 > 11 {
		t.Errorf("BackfillCache 不应覆盖已有数据, MaxGainPct = %f", gain2)
	}
}
