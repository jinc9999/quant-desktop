package main

import "testing"

func TestOnlineAt(t *testing.T) {
	base := int64(1_000_000_000_000)
	beats := []int64{base, base + 60_000, base + 120_000}
	cases := []struct {
		name string
		ts   int64
		want bool
	}{
		{"心跳点", base, true},
		{"心跳后30秒", base + 30_000, true},
		{"心跳前30秒", base - 30_000, true},
		{"超过3分钟离线", base - 4*60_000, false},
		{"超过3分钟未来", base + 10*60_000, false},
	}
	for _, c := range cases {
		got, known := onlineAt(beats, c.ts)
		if !known {
			t.Fatalf("%s: 应有心跳数据", c.name)
		}
		if got != c.want {
			t.Fatalf("%s: online=%v, 期望 %v", c.name, got, c.want)
		}
	}
	if _, known := onlineAt(nil, base); known {
		t.Fatalf("无心跳数据应返回 known=false")
	}
}

func TestClassifySim(t *testing.T) {
	so := simOpenRec{symbol: "BTCUSDT", ts: 1_000_000_000_000, bucket: "爆拉桶"}
	// 1) 执行失败（错误码）
	cls, why, _ := classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "failed", Reason: "failed", ErrorCode: -1001}}, nil)
	if cls != clsOrderFail {
		t.Fatalf("执行失败归因错误: %s %s", cls, why)
	}
	// 2) 余额不足
	cls, _, _ = classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "balance"}}, nil)
	if cls != clsBalance {
		t.Fatalf("余额不足归因错误: %s", cls)
	}
	// 3) 数据缺口
	cls, _, _ = classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "kline_missing"}}, nil)
	if cls != clsDataGap {
		t.Fatalf("数据缺口归因错误: %s", cls)
	}
	// 4) 规则拦截（该挡）
	cls, _, reason := classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "cooldown"}}, nil)
	if cls != clsRule {
		t.Fatalf("拦截归因错误: %s", cls)
	}
	if reason != "cooldown" {
		t.Fatalf("拦截应带回原始原因, 实际 %s", reason)
	}
	// 5) 激活错配：追单机会 + no_active
	addon := simOpenRec{symbol: "BTCUSDT", ts: so.ts, bucket: "爆拉桶", addOn: true}
	cls, _, _ = classifySim(addon, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "no_active"}}, nil)
	if cls != clsActivation {
		t.Fatalf("激活错配归因错误: %s", cls)
	}
	// 6) 在线但无尝试 → 信号未触发
	beats := []int64{so.ts}
	cls, _, _ = classifySim(so, nil, beats)
	if cls != clsSignalRace {
		t.Fatalf("信号未触发归因错误: %s", cls)
	}
	// 7) 离线 → 客户端未运行
	beats = []int64{so.ts - 10 * 60 * 1000}
	cls, _, _ = classifySim(so, nil, beats)
	if cls != clsOffline {
		t.Fatalf("离线归因错误: %s", cls)
	}
	// 8) 无心跳数据 → 未归因
	cls, _, _ = classifySim(so, nil, nil)
	if cls != clsUnknown {
		t.Fatalf("未归因错误: %s", cls)
	}
}

func TestAttemptPriorityPickMostSpecific(t *testing.T) {
	so := simOpenRec{symbol: "BTCUSDT", ts: 1_000_000_000_000, bucket: "温和桶", addOn: true}
	attempts := []attemptRow{
		{Ts: so.ts - 30_000, Symbol: "BTCUSDT", Stage: "candidate", Reason: "kline_close"},
		{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "no_active"},
		{Ts: so.ts + 30_000, Symbol: "BTCUSDT", Stage: "failed", Reason: "failed", ErrorCode: -2019},
	}
	cls, why, _ := classifySim(so, attempts, nil)
	if cls != clsOrderFail {
		t.Fatalf("应优先取最具体的失败记录, 实际 %s (%s)", cls, why)
	}
}

// mkKline 构造测试 K 线
func mkKline(openTime int64, open, high, low, close float64) kline {
	return kline{openTime: openTime, open: open, high: high, low: low, close: close}
}

func TestReplayForcedOpen_StopLoss(t *testing.T) {
	k5 := []kline{
		mkKline(0, 100, 100, 100, 100), // 入仓根（收盘 100）
		mkKline(1, 100, 101, 96, 98),   // 跌破 97 → 止损
	}
	pnl, exit, closed := replayForcedOpen(k5, 0, "温和桶")
	if !closed || exit != "STOP_LOSS" {
		t.Fatalf("期望止损离场, 实际 exit=%s closed=%v", exit, closed)
	}
	// (96-100)/100*100*0.7 = -2.8
	if pnl > -2.79 || pnl < -2.81 {
		t.Fatalf("温和桶止损虚拟盈亏错误: %f", pnl)
	}
}

func TestReplayForcedOpen_Trailing(t *testing.T) {
	k5 := []kline{
		mkKline(0, 100, 100, 100, 100),
		mkKline(1, 100, 103, 100, 102), // 激活（high>=102）
		mkKline(2, 102, 102.5, 99.9, 100), // 回撤 3%（99.91）触发跟踪
	}
	pnl, exit, closed := replayForcedOpen(k5, 0, "爆拉桶")
	if !closed || exit != "TRAILING" {
		t.Fatalf("期望跟踪离场, 实际 exit=%s closed=%v", exit, closed)
	}
	want := (103*0.97 - 100) / 100 * 100 * 1.5
	if pnl > want+0.01 || pnl < want-0.01 {
		t.Fatalf("跟踪虚拟盈亏错误: got %f want %f", pnl, want)
	}
}

func TestReplayForcedOpen_MaxHoldAndHolding(t *testing.T) {
	// 36 根横盘 → 超时平仓
	k5 := []kline{mkKline(0, 100, 100, 100, 100)}
	for i := int64(1); i <= 36; i++ {
		k5 = append(k5, mkKline(i, 100, 100.5, 99.8, 100.1))
	}
	pnl, exit, closed := replayForcedOpen(k5, 0, "中间桶")
	if !closed || exit != "MAX_HOLD" {
		t.Fatalf("期望超时离场, 实际 exit=%s closed=%v", exit, closed)
	}
	if pnl <= 0 {
		t.Fatalf("超时平仓应为小幅盈利, 实际 %f", pnl)
	}
	// 数据不足（只回放 2 根）→ 持有中
	k5s := k5[:2]
	_, exit, closed = replayForcedOpen(k5s, 0, "爆拉桶")
	if closed || exit != "HOLDING" {
		t.Fatalf("期望持有中, 实际 exit=%s closed=%v", exit, closed)
	}
}
