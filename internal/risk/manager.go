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
// 返回值：初始风控状态（初始止损价按方向计算；跟踪未激活；最高价=入场价）
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
