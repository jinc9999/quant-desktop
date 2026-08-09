// Package risk 风控状态机单元测试
package risk

import (
	"math"
	"testing"
)

// almostEqual 判断两个浮点数是否在容差范围内相等
// a: 待比较的第一个浮点数
// b: 待比较的第二个浮点数
// 返回: 两者差的绝对值小于 1e-9 时返回 true，否则返回 false
func almostEqual(a, b float64) bool {
	return math.Abs(a-b) < 1e-9
}

// testParams 返回测试用的统一风控参数（做多方向）
// 返回: 初始止损 10%、跟踪激活 5%、跟踪回撤 3% 的风控参数
func testParams() Params {
	return Params{
		Side:               "LONG",
		StopLossPct:        0.10,
		TrailingActivation: 0.05,
		TrailingCallback:   0.03,
	}
}

// TestInitState_StopLossPrice 验证初始风控状态的各项字段
// 覆盖: 初始止损价 = entryPrice * (1 - StopLossPct)、TrailingActive=false、HighestPrice=entryPrice
func TestInitState_StopLossPrice(t *testing.T) {
	params := testParams()
	entryPrice := 100.0
	state := params.InitState(entryPrice)

	// 初始止损价 = 100 * (1 - 0.10) = 90
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

// TestUpdate 表驱动验证风控状态更新与退出判定
// 覆盖: 初始止损触发、跟踪止损激活、跟踪止损触发、正常持有（创新高/涨幅不足）
func TestUpdate(t *testing.T) {
	params := testParams()

	tests := []struct {
		name              string
		state             State
		lastPrice         float64
		wantStopTriggered bool
		wantTrailing      bool
		wantHighest       float64
		wantStopPrice     float64
	}{
		{
			name:              "初始止损触发_价格等于止损价",
			state:             params.InitState(100),
			lastPrice:         90,
			wantStopTriggered: true,
			wantTrailing:      false,
			wantHighest:       100,
			wantStopPrice:     90,
		},
		{
			name:              "初始止损触发_价格跌破止损价",
			state:             params.InitState(100),
			lastPrice:         85,
			wantStopTriggered: true,
			wantTrailing:      false,
			wantHighest:       100,
			wantStopPrice:     90,
		},
		{
			name:              "激活跟踪止损_涨幅达到阈值",
			state:             params.InitState(100),
			lastPrice:         105,
			wantStopTriggered: false,
			wantTrailing:      true,
			wantHighest:       105,
			wantStopPrice:     105 * (1 - 0.03),
		},
		{
			name: "跟踪止损触发_价格回落至跟踪止损价",
			state: State{
				EntryPrice:       100,
				HighestPrice:     105,
				TrailingActive:   true,
				CurrentStopPrice: 105 * (1 - 0.03),
			},
			lastPrice:         101,
			wantStopTriggered: true,
			wantTrailing:      true,
			wantHighest:       105,
			wantStopPrice:     105 * (1 - 0.03),
		},
		{
			name: "正常持有_跟踪激活后创新高",
			state: State{
				EntryPrice:       100,
				HighestPrice:     105,
				TrailingActive:   true,
				CurrentStopPrice: 105 * (1 - 0.03),
			},
			lastPrice:         110,
			wantStopTriggered: false,
			wantTrailing:      true,
			wantHighest:       110,
			wantStopPrice:     110 * (1 - 0.03),
		},
		{
			name:              "正常持有_未激活且涨幅不足",
			state:             params.InitState(100),
			lastPrice:         102,
			wantStopTriggered: false,
			wantTrailing:      false,
			wantHighest:       102, // HighestPrice 始终追踪最高价，不限于跟踪激活后
			wantStopPrice:     90,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotState, gotStop, gotTrailing := params.Update(tt.state, tt.lastPrice)
			if gotStop != tt.wantStopTriggered {
				t.Errorf("stopTriggered = %v, 期望 %v", gotStop, tt.wantStopTriggered)
			}
			if gotTrailing != tt.wantTrailing {
				t.Errorf("trailingActivated = %v, 期望 %v", gotTrailing, tt.wantTrailing)
			}
			if !almostEqual(gotState.HighestPrice, tt.wantHighest) {
				t.Errorf("HighestPrice = %v, 期望 %v", gotState.HighestPrice, tt.wantHighest)
			}
			if !almostEqual(gotState.CurrentStopPrice, tt.wantStopPrice) {
				t.Errorf("CurrentStopPrice = %v, 期望 %v", gotState.CurrentStopPrice, tt.wantStopPrice)
			}
		})
	}
}

// TestRealizedPnlLong 表驱动验证多头盈亏计算
// 覆盖: 盈利、亏损、持平三种情况，公式 (exit-entry)*amount
func TestRealizedPnlLong(t *testing.T) {
	tests := []struct {
		name    string
		entry   float64
		exit    float64
		amount  float64
		wantPnl float64
	}{
		{name: "盈利", entry: 100, exit: 110, amount: 2, wantPnl: 20},
		{name: "亏损", entry: 100, exit: 90, amount: 2, wantPnl: -20},
		{name: "持平", entry: 100, exit: 100, amount: 5, wantPnl: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RealizedPnlLong(tt.entry, tt.exit, tt.amount)
			if !almostEqual(got, tt.wantPnl) {
				t.Errorf("RealizedPnlLong = %v, 期望 %v", got, tt.wantPnl)
			}
		})
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

// TestUpdate_Short 表驱动验证做空风控状态更新
func TestUpdate_Short(t *testing.T) {
	params := testParamsShort()

	tests := []struct {
		name              string
		state             State
		lastPrice         float64
		wantStopTriggered bool
		wantTrailing      bool
		wantHighest       float64 // 做空时复用作最低价
		wantStopPrice     float64
	}{
		{
			name:              "做空止损触发_价格涨到止损价",
			state:             params.InitState(100),
			lastPrice:         110,
			wantStopTriggered: true,
			wantTrailing:      false,
			wantHighest:       100,
			wantStopPrice:     110,
		},
		{
			name:              "做空止损触发_价格涨超止损价",
			state:             params.InitState(100),
			lastPrice:         115,
			wantStopTriggered: true,
			wantTrailing:      false,
			wantHighest:       100,
			wantStopPrice:     110,
		},
		{
			name:              "做空跟踪激活_跌幅达阈值",
			state:             params.InitState(100),
			lastPrice:         95,
			wantStopTriggered: false,
			wantTrailing:      true,
			wantHighest:       95, // 最低价
			wantStopPrice:     95 * (1 + 0.03),
		},
		{
			name: "做空跟踪触发_价格从最低点反弹",
			state: State{
				EntryPrice:       100,
				HighestPrice:     90, // 最低价
				TrailingActive:   true,
				CurrentStopPrice: 90 * (1 + 0.03),
			},
			lastPrice:         93,
			wantStopTriggered: true,
			wantTrailing:      true,
			wantHighest:       90,
			wantStopPrice:     90 * (1 + 0.03),
		},
		{
			name: "做空正常持有_继续创新低",
			state: State{
				EntryPrice:       100,
				HighestPrice:     95,
				TrailingActive:   true,
				CurrentStopPrice: 95 * (1 + 0.03),
			},
			lastPrice:         90,
			wantStopTriggered: false,
			wantTrailing:      true,
			wantHighest:       90, // 新最低价
			wantStopPrice:     90 * (1 + 0.03),
		},
		{
			name:              "做空正常持有_跌幅不足",
			state:             params.InitState(100),
			lastPrice:         98,
			wantStopTriggered: false,
			wantTrailing:      false,
			wantHighest:       98,
			wantStopPrice:     110,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotState, gotStop, gotTrailing := params.Update(tt.state, tt.lastPrice)
			if gotStop != tt.wantStopTriggered {
				t.Errorf("stopTriggered = %v, 期望 %v", gotStop, tt.wantStopTriggered)
			}
			if gotTrailing != tt.wantTrailing {
				t.Errorf("trailingActivated = %v, 期望 %v", gotTrailing, tt.wantTrailing)
			}
			if !almostEqual(gotState.HighestPrice, tt.wantHighest) {
				t.Errorf("HighestPrice = %v, 期望 %v", gotState.HighestPrice, tt.wantHighest)
			}
			if !almostEqual(gotState.CurrentStopPrice, tt.wantStopPrice) {
				t.Errorf("CurrentStopPrice = %v, 期望 %v", gotState.CurrentStopPrice, tt.wantStopPrice)
			}
		})
	}
}

// TestRealizedPnlShort 表驱动验证做空盈亏计算
func TestRealizedPnlShort(t *testing.T) {
	tests := []struct {
		name    string
		entry   float64
		exit    float64
		amount  float64
		wantPnl float64
	}{
		{name: "做空盈利", entry: 100, exit: 90, amount: 2, wantPnl: 20},
		{name: "做空亏损", entry: 100, exit: 110, amount: 2, wantPnl: -20},
		{name: "做空持平", entry: 100, exit: 100, amount: 5, wantPnl: 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := RealizedPnlShort(tt.entry, tt.exit, tt.amount)
			if !almostEqual(got, tt.wantPnl) {
				t.Errorf("RealizedPnlShort = %v, 期望 %v", got, tt.wantPnl)
			}
		})
	}
}
