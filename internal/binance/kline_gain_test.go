package binance

import (
	"math"
	"testing"
)

// TestMaxGain5mFromKlines 验证 5m 爆拉计算（智慧版仓位因子）：
// 周期内每根 5m 收盘 vs 前一根收盘的涨幅，取最大；首根用周期前一根做基准。
func TestMaxGain5mFromKlines(t *testing.T) {
	// nowMs 落在 09:30（周期 09:15~09:30 内第 3 根 5m: 09:25~09:30）
	nowMs := int64(9*3600+30*60) * 1000
	periodStart := nowMs - nowMs%900000 // 09:15:00
	// 前一根（09:10~09:15）收盘 100 → 周期内三根收盘 101 / 102 / 105.5
	klines := []klineLite{
		{openTime: periodStart - 300000, closePx: 100},
		{openTime: periodStart, closePx: 101},       // +1.0%
		{openTime: periodStart + 300000, closePx: 102}, // +0.99%
		{openTime: periodStart + 600000, closePx: 105.5}, // +3.43%
	}
	got := maxGain5mFromKlines(klines, nowMs)
	want := 3.431372549019608 // (105.5-102)/102*100
	if math.Abs(got-want) > 1e-6 {
		t.Fatalf("maxGain5mFromKlines=%v, want %v", got, want)
	}
}

// TestMaxGain5mFromKlines_FirstBar 周期第一根 5m（09:15~09:20）涨幅用周期前一根做基准。
func TestMaxGain5mFromKlines_FirstBar(t *testing.T) {
	nowMs := int64(9*3600+20*60) * 1000 // 09:20，周期 09:15 刚开第一根
	periodStart := nowMs - nowMs%900000
	klines := []klineLite{
		{openTime: periodStart - 300000, closePx: 100},
		{openTime: periodStart, closePx: 103.5}, // vs 前一根 +3.5%
	}
	got := maxGain5mFromKlines(klines, nowMs)
	if math.Abs(got-3.5) > 1e-9 {
		t.Fatalf("首根 5m 爆拉=%v, want 3.5", got)
	}
}

// TestMaxGain5mFromKlines_NoData 无 K 线/无前一根基准时返回 0（按均仓处理）。
func TestMaxGain5mFromKlines_NoData(t *testing.T) {
	nowMs := int64(9*3600+30*60) * 1000
	if got := maxGain5mFromKlines(nil, nowMs); got != 0 {
		t.Fatalf("空数据=%v, want 0", got)
	}
	// 只有周期前一根（当前周期无 5m 数据），也应返回 0
	periodStart := nowMs - nowMs%900000
	klines := []klineLite{{openTime: periodStart - 300000, closePx: 100}}
	if got := maxGain5mFromKlines(klines, nowMs); got != 0 {
		t.Fatalf("无周期内 K 线=%v, want 0", got)
	}
}
