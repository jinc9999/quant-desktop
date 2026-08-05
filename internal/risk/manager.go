// Package risk 风控模块
package risk

// Params 风控参数
type Params struct {
	Side               string  // "LONG" 或 "SHORT"
	StopLossPct        float64 // 固定止损比例（如 0.10 = 10%）
	TrailingActivation float64 // 跟踪止损激活涨幅（如 0.10 = 10%）
	TrailingCallback   float64 // 跟踪止损回调比例（如 0.05 = 5%）
}

// State 持仓风控状态
type State struct {
	EntryPrice       float64
	HighestPrice     float64
	TrailingActive   bool
	CurrentStopPrice float64
}

// InitState 初始化风控状态
// 参数：entryPrice 入场价格
// 返回值：初始风控状态
func (p Params) InitState(entryPrice float64) State {
	var stopPrice float64
	if p.Side == "SHORT" {
		stopPrice = entryPrice * (1 + p.StopLossPct)
	} else {
		stopPrice = entryPrice * (1 - p.StopLossPct)
	}
	return State{
		EntryPrice:       entryPrice,
		HighestPrice:     entryPrice,
		CurrentStopPrice: stopPrice,
	}
}

// ExitReason 退出原因
type ExitReason string

const (
	ExitStopLoss ExitReason = "STOP_LOSS"
	ExitTrailing ExitReason = "TRAILING_STOP"
	ExitNone     ExitReason = ""
)

// Update 更新风控状态，检查是否触发退出
// 参数：state 当前风控状态，price 最新价格
// 返回值：新状态、是否触发止损、是否激活跟踪止损
func (p Params) Update(state State, price float64) (State, bool, bool) {
	newState := state
	stopTriggered := false
	trailingActivated := state.TrailingActive

	if p.Side == "SHORT" {
		// 做空：价格上涨触发止损（含浮点容差）
		if price >= state.CurrentStopPrice-1e-9 {
			stopTriggered = true
		}
		// 做空：价格跌幅达标激活跟踪
		loss := (state.EntryPrice - price) / state.EntryPrice
		if !trailingActivated && loss >= p.TrailingActivation {
			trailingActivated = true
		}
		// 做空：跟踪最低价（从最低点反弹触发）
		if price < newState.HighestPrice {
			newState.HighestPrice = price // HighestPrice 复用作最低价
		}
		if trailingActivated && newState.HighestPrice > 0 {
			newStop := newState.HighestPrice * (1 + p.TrailingCallback)
			if newStop < newState.CurrentStopPrice {
				newState.CurrentStopPrice = newStop
			}
			if price >= newState.CurrentStopPrice-1e-9 {
				stopTriggered = true
			}
		}
	} else {
		// 做多：价格下跌触发止损
		if price <= state.CurrentStopPrice {
			stopTriggered = true
		}
		// 做多：价格涨幅达标激活跟踪
		gain := (price - state.EntryPrice) / state.EntryPrice
		if !trailingActivated && gain >= p.TrailingActivation {
			trailingActivated = true
		}
		// 做多：跟踪最高价
		if price > newState.HighestPrice {
			newState.HighestPrice = price
		}
		if trailingActivated && newState.HighestPrice > 0 {
			newStop := newState.HighestPrice * (1 - p.TrailingCallback)
			if newStop > newState.CurrentStopPrice {
				newState.CurrentStopPrice = newStop
			}
			if price <= newState.CurrentStopPrice {
				stopTriggered = true
			}
		}
	}

	newState.TrailingActive = trailingActivated
	return newState, stopTriggered, trailingActivated
}

// RealizedPnlLong 计算多头盈亏
// entryPrice: 入场价
// exitPrice: 出场价
// amount: 数量
func RealizedPnlLong(entryPrice, exitPrice, amount float64) float64 {
	return (exitPrice - entryPrice) * amount
}

// RealizedPnlShort 计算做空已实现盈亏
// 参数：entryPrice 开仓价，exitPrice 平仓价，amount 数量
// 返回值：盈亏金额（正=盈利，负=亏损）
func RealizedPnlShort(entryPrice, exitPrice, amount float64) float64 {
	return (entryPrice - exitPrice) * amount
}
