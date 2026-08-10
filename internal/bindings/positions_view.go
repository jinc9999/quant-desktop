// Package bindings 持仓明细视图：在 Position 基础上聚合实时行情与委托状态，
// 供前端持仓表格一次性获取全部展示字段，减少轮询次数
package bindings

import (
	"fmt"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/storage"
)

// OrderBrief 委托摘要（持仓展开行展示关联委托用）
type OrderBrief struct {
	ID              int64    `json:"id"`              // 本地委托 ID
	OrderType       string   `json:"orderType"`       // 委托类型（STOP_MARKET / TRAILING_STOP_MARKET）
	Side            string   `json:"side"`            // 委托方向
	Status          string   `json:"status"`          // 委托状态
	StopPrice       *float64 `json:"stopPrice"`       // 触发价
	ActivationPrice *float64 `json:"activationPrice"` // 激活价（仅跟踪止损）
	CallbackRate    *float64 `json:"callbackRate"`    // 回调率（仅跟踪止损）
	FilledPrice     *float64 `json:"filledPrice"`     // 成交价
	CreatedAt       int64    `json:"createdAt"`       // 创建时间（毫秒时间戳）
}

// PositionDetail 持仓明细视图（前端持仓表格行数据模型）
type PositionDetail struct {
	ID               int64        `json:"id"`               // 持仓 ID
	Symbol           string       `json:"symbol"`           // 交易对（如 BTCUSDT）
	Side             string       `json:"side"`             // 方向（LONG / SHORT）
	EntryPrice       float64      `json:"entryPrice"`       // 入场价
	Amount           float64      `json:"amount"`           // 数量（币）
	Leverage         int          `json:"leverage"`         // 杠杆倍数
	HighestPrice     *float64     `json:"highestPrice"`     // 持仓期间最高价
	TrailingActive   bool         `json:"trailingActive"`   // 跟踪止损是否已激活
	CurrentStopPrice float64      `json:"currentStopPrice"` // 当前止损价
	Status           string       `json:"status"`           // 持仓状态（OPEN / CLOSED）
	OpenedAt         int64        `json:"openedAt"`         // 开仓时间（毫秒时间戳）
	ClosedAt         *int64       `json:"closedAt"`         // 平仓时间
	CloseReason      *string      `json:"closeReason"`      // 平仓原因
	RealizedPnl      *float64     `json:"realizedPnl"`      // 已实现盈亏
	MarkPrice        float64      `json:"markPrice"`        // 标记价格（WS 实时行情，无行情为 0）
	Margin           float64      `json:"margin"`           // 保证金 = 入场价×数量/杠杆（USDT）
	UnrealizedPnl    float64      `json:"unrealizedPnl"`    // 未实现盈亏（USDT，无行情为 0）
	RoiPct           float64      `json:"roiPct"`           // 回报率 = 盈亏/保证金×100（%）
	OrderStatus      string       `json:"orderStatus"`      // 聚合委托状态（NEW/PARTIALLY_FILLED/FILLED/CANCELED/TRAILING/NONE）
	Orders           []OrderBrief `json:"orders"`           // 关联委托摘要列表
}

// GetPositionDetails 获取持仓明细列表（含标记价格、保证金、盈亏、回报率、委托状态）
// 数据流：positions 表 → 批量查询关联委托（单次 SQL）→ WS 行情缓存补充标记价格
// 返回: 持仓明细列表；无持仓时返回空数组
func (s *QuantService) GetPositionDetails() ([]PositionDetail, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	positions, err := s.db.GetOpenPositions()
	if err != nil {
		return nil, err
	}
	if len(positions) == 0 {
		return []PositionDetail{}, nil
	}

	// 批量查询全部持仓的关联委托，按 position_id 分组（避免 N+1 查询）
	ids := make([]int64, 0, len(positions))
	for i := range positions {
		ids = append(ids, positions[i].ID)
	}
	orders, err := s.db.GetOrdersByPositionIDs(ids)
	if err != nil {
		return nil, err
	}
	ordersByPos := make(map[int64][]storage.Order, len(positions))
	for i := range orders {
		ordersByPos[orders[i].PositionID] = append(ordersByPos[orders[i].PositionID], orders[i])
	}

	details := make([]PositionDetail, 0, len(positions))
	for i := range positions {
		p := &positions[i]
		d := PositionDetail{
			ID:               p.ID,
			Symbol:           p.Symbol,
			Side:             p.Side,
			EntryPrice:       p.EntryPrice,
			Amount:           p.Amount,
			Leverage:         p.Leverage,
			HighestPrice:     p.HighestPrice,
			TrailingActive:   p.TrailingActive,
			CurrentStopPrice: p.CurrentStopPrice,
			Status:           p.Status,
			OpenedAt:         p.OpenedAt,
			ClosedAt:         p.ClosedAt,
			CloseReason:      p.CloseReason,
			RealizedPnl:      p.RealizedPnl,
		}

		// 保证金 = 入场价 × 数量 / 杠杆（杠杆异常时兜底 10，与 GetDashboardData 口径一致）
		lev := float64(p.Leverage)
		if lev <= 0 {
			lev = 10
		}
		d.Margin = p.EntryPrice * p.Amount / lev

		// 标记价格取 WS 实时行情缓存；有行情时计算未实现盈亏与回报率
		if price, ok := s.ws.GetPrice(p.Symbol); ok && price > 0 {
			d.MarkPrice = price
			pnl := (price - p.EntryPrice) * p.Amount
			if p.Side == "SHORT" {
				pnl = -pnl // 空单盈亏方向相反
			}
			d.UnrealizedPnl = pnl
		if d.Margin > 0 {
			d.RoiPct = pnl / d.Margin * 100
			// 防御：异常小保证金（幽灵仓等）会让回报率爆出离谱数字，置 0 显示 "--"。
			if d.Margin < 1 {
				d.RoiPct = 0
			}
		}
		}

		posOrders := ordersByPos[p.ID]
		d.OrderStatus = deriveOrderStatus(p, posOrders)
		d.Orders = toOrderBriefs(posOrders)
		details = append(details, d)
	}
	return details, nil
}

// deriveOrderStatus 聚合持仓关联委托状态，得出持仓级委托状态
// 优先级：部分成交 > 未成交（跟踪激活时显示 TRAILING）> 已成交 > 已取消 > 无委托
// 参数:
//   - p: 持仓记录（用 TrailingActive 判断跟踪止损状态）
//   - orders: 该持仓关联的全部委托
//
// 返回: 聚合状态码（NEW / PARTIALLY_FILLED / FILLED / CANCELED / TRAILING / NONE）
func deriveOrderStatus(p *storage.Position, orders []storage.Order) string {
	if len(orders) == 0 {
		return "NONE"
	}
	var hasNew, hasPartial, hasFilled, hasCanceled bool
	for i := range orders {
		switch orders[i].Status {
		case binance.OrderStatusNew:
			hasNew = true
		case binance.OrderStatusPartiallyFilled:
			hasPartial = true
		case binance.OrderStatusFilled:
			hasFilled = true
		case binance.OrderStatusCanceled, binance.OrderStatusExpired:
			hasCanceled = true
		}
	}
	switch {
	case hasPartial:
		return binance.OrderStatusPartiallyFilled
	case hasNew:
		if p.TrailingActive {
			return "TRAILING"
		}
		return binance.OrderStatusNew
	case hasFilled:
		return binance.OrderStatusFilled
	case hasCanceled:
		return binance.OrderStatusCanceled
	default:
		return "NONE"
	}
}

// toOrderBriefs 将 storage.Order 列表转换为委托摘要列表
// 参数:
//   - orders: 关联委托列表
//
// 返回: 委托摘要列表（保留展开行所需字段）
func toOrderBriefs(orders []storage.Order) []OrderBrief {
	briefs := make([]OrderBrief, 0, len(orders))
	for i := range orders {
		o := &orders[i]
		briefs = append(briefs, OrderBrief{
			ID:              o.ID,
			OrderType:       o.OrderType,
			Side:            o.Side,
			Status:          o.Status,
			StopPrice:       o.StopPrice,
			ActivationPrice: o.ActivationPrice,
			CallbackRate:    o.CallbackRate,
			FilledPrice:     o.FilledPrice,
			CreatedAt:       o.CreatedAt,
		})
	}
	return briefs
}
