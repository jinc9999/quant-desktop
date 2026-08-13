package main

import (
	"math"
	"testing"
)

// TestMax5mGainClose 验证报表分桶口径与客户端 maxGain5mFromKlines 完全一致：
// 周期内每根 5m 收盘 vs 前一根收盘的涨幅，取最大；首根用周期前一根做基准。
func TestMax5mGainClose(t *testing.T) {
	// 周期 09:15~09:30 内第 3 根 5m: 09:25~09:30（与客户端单测同款数据）
	periodStart := int64(9*3600+15*60) * 1000
	k5 := []kline{
		{openTime: periodStart - 300000, open: 99, high: 100.5, low: 98, close: 100},      // 周期前一根
		{openTime: periodStart, open: 100, high: 101.5, low: 99.5, close: 101},             // +1.0%
		{openTime: periodStart + 300000, open: 101, high: 102.5, low: 100.5, close: 102},   // +0.99%
		{openTime: periodStart + 600000, open: 102, high: 106, low: 101.5, close: 105.5},   // +3.43%
	}
	got := max5mGainClose(k5, 3)
	want := 3.431372549019608 // (105.5-102)/102*100
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("max5mGainClose=%v, want %v", got, want)
	}
	if b := bucketOf(got); b != "爆拉桶" {
		t.Fatalf("bucketOf(%v)=%s, want 爆拉桶", got, b)
	}
}

// TestMax5mGainClose_FirstBar 周期第一根 5m 用周期前一根收盘做基准。
func TestMax5mGainClose_FirstBar(t *testing.T) {
	periodStart := int64(9*3600+15*60) * 1000
	k5 := []kline{
		{openTime: periodStart - 300000, open: 100, high: 100, low: 100, close: 100},
		{openTime: periodStart, open: 101, high: 104, low: 100.5, close: 103.5}, // vs 前一根 +3.5%
	}
	if got := max5mGainClose(k5, 1); math.Abs(got-3.5) > 1e-9 {
		t.Fatalf("首根 5m 收盘涨幅=%v, want 3.5", got)
	}
}

// TestMax5mGainClose_NoData 越界/无有效数据返回 0。
func TestMax5mGainClose_NoData(t *testing.T) {
	if got := max5mGainClose(nil, 0); got != 0 {
		t.Fatalf("空数据=%v, want 0", got)
	}
	if got := max5mGainClose([]kline{{openTime: 0, close: 100}}, -1); got != 0 {
		t.Fatalf("越界=%v, want 0", got)
	}
}
