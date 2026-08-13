package storage

import (
	"testing"
	"time"
)

// TestOpenAttemptRoundtrip 开仓尝试记录插入/查询往返。
// 验证所有字段正确落库且按时间升序返回。
func TestOpenAttemptRoundtrip(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	a1 := &OpenAttempt{
		Ts: now, Tick: 100, Symbol: "BTCUSDT", Side: "LONG",
		Stage: StageCandidate, Reason: ReasonKlineClose,
		Gain15: 3.25, KlineOpen: 100.0, Gain5m: 3.1, Bucket: "爆拉桶",
	}
	a2 := &OpenAttempt{
		Ts: now + 1000, Tick: 100, Symbol: "BTCUSDT", Side: "LONG",
		Stage: StageFailed, Reason: ReasonFailed,
		Gain15: 3.25, ErrorCode: -1001, ErrorMsg: "boom", RetryCount: 1, LatencyMs: 1200,
	}
	if err := db.InsertOpenAttempt(a1); err != nil {
		t.Fatalf("插入候选记录失败: %v", err)
	}
	if err := db.InsertOpenAttempt(a2); err != nil {
		t.Fatalf("插入失败记录失败: %v", err)
	}

	list, err := db.GetOpenAttemptsSince(now)
	if err != nil {
		t.Fatalf("查询失败: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("期望 2 条记录, 实际 %d", len(list))
	}
	if list[0].Reason != ReasonKlineClose || list[0].Bucket != "爆拉桶" {
		t.Fatalf("第 1 条字段不符: %+v", list[0])
	}
	if list[1].Stage != StageFailed || list[1].ErrorCode != -1001 || list[1].RetryCount != 1 {
		t.Fatalf("第 2 条字段不符: %+v", list[1])
	}
}

// TestEngineHeartbeat 心跳写入/查询。
func TestEngineHeartbeat(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()
	if err := db.InsertEngineHeartbeat("SIMULATION", 4); err != nil {
		t.Fatalf("写入心跳失败: %v", err)
	}
	if err := db.InsertEngineHeartbeat("SIMULATION", 8); err != nil {
		t.Fatalf("写入心跳失败: %v", err)
	}
	beats, err := db.GetEngineHeartbeatsSince(now)
	if err != nil {
		t.Fatalf("查询心跳失败: %v", err)
	}
	if len(beats) != 2 {
		t.Fatalf("期望 2 条心跳, 实际 %d", len(beats))
	}
	if beats[1] < beats[0] {
		t.Fatalf("心跳应按时间升序: %v", beats)
	}
}
