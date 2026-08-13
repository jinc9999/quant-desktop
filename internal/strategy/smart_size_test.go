package strategy

import (
	"math"
	"testing"
)

func TestSmartSizeMultiplier(t *testing.T) {
	cases := []struct {
		name     string
		gain5m   float64
		high     float64
		low      float64
		boundary float64
		want     float64
	}{
		{"爆拉≥边界 高倍", 3.5, 1.5, 0.7, 2.5, 1.5},
		{"边界命中 高倍", 2.5, 1.5, 0.7, 2.5, 1.5},
		{"较强 2~边界 均仓", 2.2, 1.5, 0.7, 2.5, 1.0},
		{"较强 刚好2% 均仓", 2.0, 1.5, 0.7, 2.5, 1.0},
		{"温和<2% 低倍", 1.0, 1.5, 0.7, 2.5, 0.7},
		{"无数据负值 均仓", -1, 1.5, 0.7, 2.5, 1.0},
		{"自定义边界3%", 2.8, 1.8, 0.6, 3.0, 1.0},
		{"自定义高倍2.0", 3.2, 2.0, 0.5, 3.0, 2.0},
		{"非法默认兜底", 1.0, 0, 0, 0, 0.7},
		{"爆拉且非法默认 高倍1.5", 3.0, 0, 0, 0, 1.5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := SmartSizeMultiplier(tc.gain5m, tc.high, tc.low, tc.boundary)
			if math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("SmartSizeMultiplier(%v,%v,%v,%v)=%v, want %v",
					tc.gain5m, tc.high, tc.low, tc.boundary, got, tc.want)
			}
		})
	}
}
