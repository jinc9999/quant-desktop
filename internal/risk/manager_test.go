// Package risk 风控状态机单元测试
package risk

import (
	"math"
	"testing"
)

// almostEqual 判断两个浮点数是否在容差范围内相等
func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// testParams 返回测试用的统一风控参数（做多方向）
func testParams() Params {
	return Params{
		Side:               "LONG",
		StopLossPct:        0.10,
		TrailingActivation: 0.05,
		TrailingCallback:   0.03,
	}
}

// testParamsShort 返回测试用的做空风控参数
func testParamsShort() Params {
	return Params{
		Side:               "SHORT",
		StopLossPct:        0.10,
		TrailingActivation: 0.05,
		TrailingCallback:   0.03,
	}
}

// TestInitState_StopLossPrice 验证做多初始风控状态
// 初始止损价 = entryPrice * (1 - StopLossPct) = 100 * 0.90 = 90
func TestInitState_StopLossPrice(t *testing.T) {
	params := testParams()
	entryPrice := 100.0
	state := params.InitState(entryPrice)

	if !almostEqual(state.CurrentStopPrice, 90.0) {
		t.Errorf("CurrentStopPrice = %v, 期望 %v", state.CurrentStopPrice, 90.0)
	}
	if state.TrailingActive {
		t.Errorf("TrailingActive = true, 期望 false")
	}
	if !almostEqual(state.HighestPrice, entryPrice) {
		t.Errorf("HighestPrice = %v, 期望 %v", state.HighestPrice, entryPrice)
	}
	if !almostEqual(state.EntryPrice, entryPrice) {
		t.Errorf("EntryPrice = %v, 期望 %v", state.EntryPrice, entryPrice)
	}
}

// TestInitState_Short 验证做空初始风控状态
// 做空止损价 = entryPrice * (1 + StopLossPct) = 100 * 1.10 = 110
func TestInitState_Short(t *testing.T) {
	params := testParamsShort()
	state := params.InitState(100)
	if !almostEqual(state.CurrentStopPrice, 110.0) {
		t.Errorf("CurrentStopPrice = %v, 期望 110.0", state.CurrentStopPrice)
	}
	if !almostEqual(state.EntryPrice, 100.0) {
		t.Errorf("EntryPrice = %v, 期望 100.0", state.EntryPrice)
	}
	if state.TrailingActive {
		t.Errorf("TrailingActive = true, 期望 false")
	}
}
