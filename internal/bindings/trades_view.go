// Package bindings 已完成交易明细视图：在已平仓 Position 基础上计算出票价、
// 盈利百分比等展示字段，供前端「已完成交易」表格一次性获取全部列数据
package bindings

import "fmt"

// ClosedTradeDetail 已完成交易明细视图（前端已完成交易表格行数据模型）
type ClosedTradeDetail struct {
	ID          int64    `json:"id"`          // 持仓 ID
	Symbol      string   `json:"symbol"`      // 交易对（如 BTCUSDT）
	Side        string   `json:"side"`        // 方向（LONG / SHORT）
	EntryPrice  float64  `json:"entryPrice"`  // 入场价
	ExitPrice   *float64 `json:"exitPrice"`   // 出场价（平仓成交价，未知为 nil）
	Amount      float64  `json:"amount"`      // 数量（币）
	Leverage    int      `json:"leverage"`    // 杠杆倍数
	RealizedPnl float64  `json:"realizedPnl"` // 净盈亏（USDT）
	ProfitPct   float64  `json:"profitPct"`   // 盈利百分比 = 净盈亏/保证金×100（%）
	Fee         float64  `json:"fee"`         // 手续费（USDT）
	ClosedAt    *int64   `json:"closedAt"`    // 平仓时间（毫秒时间戳）
	CloseReason *string  `json:"closeReason"` // 平仓原因（STOP_LOSS / TRAILING_STOP / ROLLBACK）
}

// GetClosedTradeDetails 获取已完成交易明细列表（含出场价、净盈亏、盈利%、手续费）
// 数据流：positions 表（CLOSED）→ 逐行计算保证金与盈利百分比
// 参数:
//   - limit: 返回条数上限（<=0 时默认 50）
//
// 返回: 已完成交易明细列表（按平仓时间降序）；无记录时返回空数组
func (s *QuantService) GetClosedTradeDetails(limit int) ([]ClosedTradeDetail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	positions, err := s.db.GetClosedPositions(limit)
	if err != nil {
		return nil, err
	}

	details := make([]ClosedTradeDetail, 0, len(positions))
	for i := range positions {
		p := &positions[i]
		d := ClosedTradeDetail{
			ID:          p.ID,
			Symbol:      p.Symbol,
			Side:        p.Side,
			EntryPrice:  p.EntryPrice,
			ExitPrice:   p.ExitPrice,
			Amount:      p.Amount,
			Leverage:    p.Leverage,
			Fee:         p.Fee,
			ClosedAt:    p.ClosedAt,
			CloseReason: p.CloseReason,
		}

		// 净盈亏（nil 视为 0）
		if p.RealizedPnl != nil {
			d.RealizedPnl = *p.RealizedPnl
		}

		// 盈利百分比 = 净盈亏 / 保证金 × 100（杠杆异常时兜底 10，与持仓视图口径一致）
		lev := float64(p.Leverage)
		if lev <= 0 {
			lev = 10
		}
		margin := p.EntryPrice * p.Amount / lev
		if margin > 0 {
			d.ProfitPct = d.RealizedPnl / margin * 100
		}

		details = append(details, d)
	}
	return details, nil
}
