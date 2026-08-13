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
	cls, why := classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "failed", Reason: "failed", ErrorCode: -1001}}, nil)
	if cls != clsOrderFail {
		t.Fatalf("执行失败归因错误: %s %s", cls, why)
	}
	// 2) 余额不足
	cls, _ = classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "balance"}}, nil)
	if cls != clsBalance {
		t.Fatalf("余额不足归因错误: %s", cls)
	}
	// 3) 数据缺口
	cls, _ = classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "kline_missing"}}, nil)
	if cls != clsDataGap {
		t.Fatalf("数据缺口归因错误: %s", cls)
	}
	// 4) 规则拦截（该挡）
	cls, _ = classifySim(so, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "cooldown"}}, nil)
	if cls != clsRule {
		t.Fatalf("拦截归因错误: %s", cls)
	}
	// 5) 激活错配：追单机会 + no_active
	addon := simOpenRec{symbol: "BTCUSDT", ts: so.ts, bucket: "爆拉桶", addOn: true}
	cls, _ = classifySim(addon, []attemptRow{{Ts: so.ts, Symbol: "BTCUSDT", Stage: "candidate", Reason: "no_active"}}, nil)
	if cls != clsActivation {
		t.Fatalf("激活错配归因错误: %s", cls)
	}
	// 6) 在线但无尝试 → 信号未触发
	beats := []int64{so.ts}
	cls, _ = classifySim(so, nil, beats)
	if cls != clsSignalRace {
		t.Fatalf("信号未触发归因错误: %s", cls)
	}
	// 7) 离线 → 客户端未运行
	beats = []int64{so.ts - 10 * 60 * 1000}
	cls, _ = classifySim(so, nil, beats)
	if cls != clsOffline {
		t.Fatalf("离线归因错误: %s", cls)
	}
	// 8) 无心跳数据 → 未归因
	cls, _ = classifySim(so, nil, nil)
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
	cls, why := classifySim(so, attempts, nil)
	if cls != clsOrderFail {
		t.Fatalf("应优先取最具体的失败记录, 实际 %s (%s)", cls, why)
	}
}
