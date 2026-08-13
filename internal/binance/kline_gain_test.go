package binance

import (
	"math"
	"testing"
)

// TestMaxGain5mFromKlines 验证 5m 爆拉计算（智慧版仓位因子）：
// 周期内每根 5m 收盘 vs 前一根收盘的涨幅，取最大；首根用周期前一根做基准。
func TestMaxGain5mFromKlines(t *testing.T) {
	// nowMs = 09:30:00：最新已收盘 5m 为 09:25~09:30，其所在 15m 周期为 09:15~09:30
	nowMs := int64(9*3600+30*60) * 1000
	klines := []klineLite{
		{openTime: int64(9*3600+10*60) * 1000, closePx: 100},   // 09:10 前一根基准
		{openTime: int64(9*3600+15*60) * 1000, closePx: 101},   // 09:15 +1.0%
		{openTime: int64(9*3600+20*60) * 1000, closePx: 102},   // 09:20 +0.99%
		{openTime: int64(9*3600+25*60) * 1000, closePx: 105.5}, // 09:25 +3.43%
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

// TestMaxGain5mFromKlines_FormingBarExcluded 未收盘的 5m（实时价）不参与爆拉桶判定
// （2026-08-14 与回测 max5mGain 已收盘口径对齐）。
func TestMaxGain5mFromKlines_FormingBarExcluded(t *testing.T) {
	nowMs := int64(9*3600+28*60) * 1000 // 09:28，当前 5m（09:25~09:30）尚未收盘
	periodStart := nowMs - nowMs%900000 // 09:15
	klines := []klineLite{
		{openTime: periodStart - 300000, closePx: 100},
		{openTime: periodStart, closePx: 101},          // +1.0%（已收盘）
		{openTime: periodStart + 300000, closePx: 102}, // +0.99%（已收盘）
		{openTime: periodStart + 600000, closePx: 106}, // 未收盘实时价 106，不应计入
	}
	got := maxGain5mFromKlines(klines, nowMs)
	want := 1.0 // 周期内已收盘最大 = 09:15 根 (101-100)/100 = +1.0%（09:25 未收盘 106 不计入）
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("含未收盘5m maxGain=%v, want %v（未收盘实时价不应计入爆拉桶）", got, want)
	}
	if got >= 3.9 {
		t.Fatalf("未收盘 5m 实时价 106 被计入爆拉桶: maxGain=%v", got)
	}
}
