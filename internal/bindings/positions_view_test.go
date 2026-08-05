package bindings

import (
	"testing"
	"time"

	"quant-desktop/internal/storage"
)

// insertViewTestPosition 插入测试持仓并返回带 ID 的记录
// 参数:
//   - t: 测试上下文
//   - svc: 测试服务实例
//   - symbol: 交易对
//   - entryPrice: 入场价
//   - trailingActive: 跟踪止损是否激活
//
// 返回: 已回填 ID 的持仓记录
func insertViewTestPosition(t *testing.T, svc *QuantService, symbol string, entryPrice float64, trailingActive bool) *storage.Position {
	t.Helper()
	pos := &storage.Position{
		Symbol:           symbol,
		Side:             "LONG",
		EntryPrice:       entryPrice,
		Amount:           0.1,
		Leverage:         10,
		TrailingActive:   trailingActive,
		CurrentStopPrice: entryPrice * 0.9,
		Status:           "OPEN",
		OpenedAt:         time.Now().UnixMilli(),
	}
	id, err := svc.db.InsertPosition(pos)
	if err != nil {
		t.Fatalf("插入测试持仓失败: %v", err)
	}
	pos.ID = id
	return pos
}

// insertViewTestOrder 插入测试委托
// 参数:
//   - t: 测试上下文
//   - svc: 测试服务实例
//   - positionID: 关联持仓 ID
//   - orderType: 委托类型
//   - status: 委托状态
func insertViewTestOrder(t *testing.T, svc *QuantService, positionID int64, orderType, status string) {
	t.Helper()
	now := time.Now().UnixMilli()
	_, err := svc.db.InsertOrder(&storage.Order{
		PositionID: positionID, ExchangeOrderID: now, Symbol: "BTCUSDT",
		OrderType: orderType, Side: "SELL", Status: status,
		Amount: 0.1, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入测试委托失败: %v", err)
	}
}

// TestGetPositionDetails 测试持仓明细聚合接口。
// 场景：持仓 A（挂 NEW 止损单）、持仓 B（跟踪激活 + NEW 跟踪单 + CANCELED 固定止损），验证:
//   - 保证金 = 入场价×数量/杠杆
//   - 无行情时标记价格/盈亏/回报率为 0
//   - 委托状态聚合：A → NEW，B → TRAILING
//   - 关联委托摘要数量正确
func TestGetPositionDetails(t *testing.T) {
	svc := newTestService(t)

	posA := insertViewTestPosition(t, svc, "BTCUSDT", 50000.0, false)
	insertViewTestOrder(t, svc, posA.ID, "STOP_MARKET", "NEW")
	insertViewTestOrder(t, svc, posA.ID, "TRAILING_STOP_MARKET", "NEW")

	posB := insertViewTestPosition(t, svc, "ETHUSDT", 3000.0, true)
	insertViewTestOrder(t, svc, posB.ID, "STOP_MARKET", "CANCELED")
	insertViewTestOrder(t, svc, posB.ID, "TRAILING_STOP_MARKET", "NEW")

	details, err := svc.GetPositionDetails()
	if err != nil {
		t.Fatalf("GetPositionDetails 失败: %v", err)
	}
	if len(details) != 2 {
		t.Fatalf("期望 2 条持仓明细, 实际 = %d", len(details))
	}

	// 按 symbol 建立索引便于断言
	bySymbol := map[string]PositionDetail{}
	for _, d := range details {
		bySymbol[d.Symbol] = d
	}

	a, ok := bySymbol["BTCUSDT"]
	if !ok {
		t.Fatal("缺少 BTCUSDT 持仓明细")
	}
	// 保证金 = 50000 × 0.1 / 10 = 500
	if a.Margin != 500.0 {
		t.Errorf("BTCUSDT 保证金: 期望 500, 实际 = %f", a.Margin)
	}
	// 无 WS 行情时实时字段为 0
	if a.MarkPrice != 0 || a.UnrealizedPnl != 0 || a.RoiPct != 0 {
		t.Errorf("无行情时实时字段应为 0: mark=%f pnl=%f roi=%f", a.MarkPrice, a.UnrealizedPnl, a.RoiPct)
	}
	if a.OrderStatus != "NEW" {
		t.Errorf("BTCUSDT 委托状态: 期望 NEW, 实际 = %s", a.OrderStatus)
	}
	if len(a.Orders) != 2 {
		t.Errorf("BTCUSDT 关联委托: 期望 2 条, 实际 = %d", len(a.Orders))
	}

	b, ok := bySymbol["ETHUSDT"]
	if !ok {
		t.Fatal("缺少 ETHUSDT 持仓明细")
	}
	// 保证金 = 3000 × 0.1 / 10 = 30
	if b.Margin != 30.0 {
		t.Errorf("ETHUSDT 保证金: 期望 30, 实际 = %f", b.Margin)
	}
	if b.OrderStatus != "TRAILING" {
		t.Errorf("ETHUSDT 委托状态: 期望 TRAILING, 实际 = %s", b.OrderStatus)
	}
}

// TestGetPositionDetails_Empty 测试无持仓时返回空数组而非 nil
func TestGetPositionDetails_Empty(t *testing.T) {
	svc := newTestService(t)

	details, err := svc.GetPositionDetails()
	if err != nil {
		t.Fatalf("GetPositionDetails 失败: %v", err)
	}
	if details == nil {
		t.Fatal("无持仓时应返回空数组而非 nil")
	}
	if len(details) != 0 {
		t.Errorf("期望 0 条, 实际 = %d", len(details))
	}
}

// TestDeriveOrderStatus 表驱动测试委托状态聚合规则的全部分支
func TestDeriveOrderStatus(t *testing.T) {
	pos := &storage.Position{TrailingActive: false}
	trailingPos := &storage.Position{TrailingActive: true}
	mk := func(statuses ...string) []storage.Order {
		orders := make([]storage.Order, len(statuses))
		for i, s := range statuses {
			orders[i] = storage.Order{Status: s}
		}
		return orders
	}

	cases := []struct {
		name   string
		p      *storage.Position
		orders []storage.Order
		want   string
	}{
		{"无委托", pos, nil, "NONE"},
		{"仅 NEW 未激活跟踪", pos, mk("NEW"), "NEW"},
		{"NEW 且跟踪激活", trailingPos, mk("NEW", "CANCELED"), "TRAILING"},
		{"部分成交优先", pos, mk("NEW", "PARTIALLY_FILLED"), "PARTIALLY_FILLED"},
		{"全部成交", pos, mk("FILLED"), "FILLED"},
		{"全部取消", pos, mk("CANCELED", "EXPIRED"), "CANCELED"},
		{"未知状态兜底", pos, mk("REJECTED"), "NONE"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := deriveOrderStatus(c.p, c.orders)
			if got != c.want {
				t.Errorf("期望 %s, 实际 = %s", c.want, got)
			}
		})
	}
}
