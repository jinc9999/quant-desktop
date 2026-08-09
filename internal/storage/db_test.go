// Package storage 数据库存储层单元测试，覆盖 4 个 repo 的全部 CRUD 操作和边界条件
package storage

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// newTestDB 创建用于测试的临时 SQLite 数据库实例。
// 使用 t.TempDir() 生成临时目录，测试结束后通过 t.Cleanup 自动关闭连接并清理文件。
// 参数:
//   - t: 测试上下文，用于创建临时目录和注册清理函数
//
// 返回:
//   - *DB: 已完成建表迁移的数据库实例
func newTestDB(t *testing.T) *DB {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := NewDB(dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	t.Cleanup(func() {
		db.Close()
	})
	return db
}

// ==================== 一、Position Repo 测试 ====================

// TestInsertPosition 测试插入持仓记录。
// 验证 InsertPosition 无错误且返回的自增 ID 大于 0。
func TestInsertPosition(t *testing.T) {
	db := newTestDB(t)

	id, err := db.InsertPosition(&Position{
		Symbol:     "BTCUSDT",
		Side:       "LONG",
		EntryPrice: 100.0,
		Amount:     0.01,
		Leverage:   10,
		Status:     "OPEN",
		OpenedAt:   time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("InsertPosition 失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("期望返回 ID > 0, 实际 = %d", id)
	}
}

// TestGetOpenPositions 测试获取 OPEN 状态持仓列表。
// 插入 2 条 OPEN + 1 条手动 CLOSED 持仓，验证只返回 2 条 OPEN 记录，
// 且按 opened_at 降序排列（较晚开仓的排在前面）。
func TestGetOpenPositions(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 第 1 条 OPEN（较早开仓）
	_, err := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 0.01,
		Leverage: 10, Status: "OPEN", OpenedAt: now - 2000,
	})
	if err != nil {
		t.Fatalf("插入持仓 1 失败: %v", err)
	}

	// 第 2 条 OPEN（较晚开仓）
	_, err = db.InsertPosition(&Position{
		Symbol: "ETHUSDT", Side: "SHORT", EntryPrice: 200, Amount: 0.02,
		Leverage: 5, Status: "OPEN", OpenedAt: now - 1000,
	})
	if err != nil {
		t.Fatalf("插入持仓 2 失败: %v", err)
	}

	// 第 3 条 CLOSED（手动关闭）
	_, err = db.InsertPosition(&Position{
		Symbol: "SOLUSDT", Side: "LONG", EntryPrice: 50, Amount: 0.05,
		Leverage: 10, Status: "CLOSED", OpenedAt: now,
	})
	if err != nil {
		t.Fatalf("插入持仓 3 失败: %v", err)
	}

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("GetOpenPositions 失败: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("期望 2 条 OPEN 持仓, 实际 = %d", len(positions))
	}
	// 验证降序：第 1 条应为 ETHUSDT（opened_at 更大）
	if positions[0].Symbol != "ETHUSDT" {
		t.Errorf("期望第 1 条为 ETHUSDT, 实际 = %s", positions[0].Symbol)
	}
	if positions[1].Symbol != "BTCUSDT" {
		t.Errorf("期望第 2 条为 BTCUSDT, 实际 = %s", positions[1].Symbol)
	}
}

// TestClosePosition 测试平仓操作。
// 插入持仓后调用 ClosePosition(id, "STOP_LOSS", 1.5)，
// 验证 status=CLOSED、close_reason=STOP_LOSS、realized_pnl=1.5、closed_at 非零。
func TestClosePosition(t *testing.T) {
	db := newTestDB(t)

	id, err := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 0.01,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("插入持仓失败: %v", err)
	}

	err = db.ClosePosition(id, "STOP_LOSS", 1.5, nil, 0)
	if err != nil {
		t.Fatalf("ClosePosition 失败: %v", err)
	}

	var status, closeReason string
	var realizedPnl float64
	var closedAt int64
	err = db.Conn.QueryRow(
		`SELECT status, close_reason, realized_pnl, closed_at FROM positions WHERE id=?`, id,
	).Scan(&status, &closeReason, &realizedPnl, &closedAt)
	if err != nil {
		t.Fatalf("查询平仓后的持仓失败: %v", err)
	}

	if status != "CLOSED" {
		t.Errorf("期望 status=CLOSED, 实际 = %s", status)
	}
	if closeReason != "STOP_LOSS" {
		t.Errorf("期望 close_reason=STOP_LOSS, 实际 = %s", closeReason)
	}
	if realizedPnl != 1.5 {
		t.Errorf("期望 realized_pnl=1.5, 实际 = %f", realizedPnl)
	}
	if closedAt == 0 {
		t.Error("期望 closed_at 非零, 实际 = 0")
	}
}

// TestUpdateRiskState 测试更新持仓风控状态。
// 插入持仓后调用 UpdateRiskState(id, highestPrice=110, trailingActive=true, stopPrice=95)，
// 重新查询验证 highest_price、trailing_active、current_stop_price 均已正确更新。
func TestUpdateRiskState(t *testing.T) {
	db := newTestDB(t)

	id, err := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 0.01,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("插入持仓失败: %v", err)
	}

	hp := 110.0
	err = db.UpdateRiskState(id, &hp, true, 95)
	if err != nil {
		t.Fatalf("UpdateRiskState 失败: %v", err)
	}

	var highestPrice float64
	var trailingActive bool
	var stopPrice float64
	err = db.Conn.QueryRow(
		`SELECT highest_price, trailing_active, current_stop_price FROM positions WHERE id=?`, id,
	).Scan(&highestPrice, &trailingActive, &stopPrice)
	if err != nil {
		t.Fatalf("查询风控状态失败: %v", err)
	}

	if highestPrice != 110.0 {
		t.Errorf("期望 highest_price=110, 实际 = %f", highestPrice)
	}
	if !trailingActive {
		t.Error("期望 trailing_active=true, 实际 = false")
	}
	if stopPrice != 95.0 {
		t.Errorf("期望 current_stop_price=95, 实际 = %f", stopPrice)
	}
}

// TestGetOpenPositions_Empty 测试空库查询 OPEN 持仓。
// 验证返回空切片（非 nil）且长度为 0。
func TestGetOpenPositions_Empty(t *testing.T) {
	db := newTestDB(t)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("GetOpenPositions 失败: %v", err)
	}
	if positions == nil {
		t.Fatal("期望返回空切片（非 nil）, 实际 = nil")
	}
	if len(positions) != 0 {
		t.Fatalf("期望 0 条记录, 实际 = %d", len(positions))
	}
}

// ==================== 二、TradeLog Repo 测试 ====================

// TestInsertLog 测试插入交易日志记录。
// 验证 InsertLog 无错误返回。
func TestInsertLog(t *testing.T) {
	db := newTestDB(t)

	err := db.InsertLog(&TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "strategy",
		Message:   "开仓成功",
		Symbol:    "BTCUSDT",
		Price:     100.0,
		Amount:    0.01,
	})
	if err != nil {
		t.Fatalf("InsertLog 失败: %v", err)
	}
}

// TestGetRecentLogs 测试获取最近日志。
// 插入 5 条日志，调用 GetRecentLogs(3)，验证返回 3 条且按 timestamp 降序排列。
func TestGetRecentLogs(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	for i := 0; i < 5; i++ {
		err := db.InsertLog(&TradeLog{
			Timestamp: now + int64(i*1000),
			Level:     "info",
			Module:    "test",
			Message:   fmt.Sprintf("日志 %d", i),
		})
		if err != nil {
			t.Fatalf("插入日志 %d 失败: %v", i, err)
		}
	}

	logs, err := db.GetRecentLogs(3)
	if err != nil {
		t.Fatalf("GetRecentLogs 失败: %v", err)
	}
	if len(logs) != 3 {
		t.Fatalf("期望 3 条日志, 实际 = %d", len(logs))
	}
	// 验证降序：第 1 条应为 "日志 4"（timestamp 最大）
	if logs[0].Message != "日志 4" {
		t.Errorf("期望第 1 条为 '日志 4', 实际 = %s", logs[0].Message)
	}
	if logs[1].Message != "日志 3" {
		t.Errorf("期望第 2 条为 '日志 3', 实际 = %s", logs[1].Message)
	}
	if logs[2].Message != "日志 2" {
		t.Errorf("期望第 3 条为 '日志 2', 实际 = %s", logs[2].Message)
	}
}

// TestGetRecentLogs_DefaultLimit 测试 limit<=0 时的默认限制行为。
// 插入 5 条日志，分别用 limit=0 和 limit=-1 调用，验证均返回全部 5 条（默认上限 100）。
func TestGetRecentLogs_DefaultLimit(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	for i := 0; i < 5; i++ {
		err := db.InsertLog(&TradeLog{
			Timestamp: now + int64(i*1000),
			Level:     "info",
			Module:    "test",
			Message:   fmt.Sprintf("日志 %d", i),
		})
		if err != nil {
			t.Fatalf("插入日志 %d 失败: %v", i, err)
		}
	}

	// limit=0 应使用默认值 100
	logs, err := db.GetRecentLogs(0)
	if err != nil {
		t.Fatalf("GetRecentLogs(0) 失败: %v", err)
	}
	if len(logs) != 5 {
		t.Fatalf("limit=0: 期望 5 条日志, 实际 = %d", len(logs))
	}

	// limit=-1 应使用默认值 100
	logs, err = db.GetRecentLogs(-1)
	if err != nil {
		t.Fatalf("GetRecentLogs(-1) 失败: %v", err)
	}
	if len(logs) != 5 {
		t.Fatalf("limit=-1: 期望 5 条日志, 实际 = %d", len(logs))
	}
}

// ==================== 三、Order Repo 测试 ====================

// TestInsertOrder 测试插入委托记录。
// 验证 InsertOrder 无错误且返回的自增 ID 大于 0。
func TestInsertOrder(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	id, err := db.InsertOrder(&Order{
		PositionID:      1,
		ExchangeOrderID: 10001,
		Symbol:          "BTCUSDT",
		OrderType:       "TRAILING_STOP_MARKET",
		Side:            "SELL",
		Status:          "NEW",
		Amount:          0.01,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		t.Fatalf("InsertOrder 失败: %v", err)
	}
	if id <= 0 {
		t.Fatalf("期望返回 ID > 0, 实际 = %d", id)
	}
}

// TestGetOrdersByPosition 测试按持仓 ID 查询委托。
// 插入 2 条同 position_id + 1 条不同 position_id 的委托，
// 验证 GetOrdersByPosition 只返回对应持仓的 2 条记录。
func TestGetOrdersByPosition(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 2 条属于 position_id=1
	_, err := db.InsertOrder(&Order{
		PositionID: 1, ExchangeOrderID: 10001, Symbol: "BTCUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.01, CreatedAt: now - 1000, UpdatedAt: now - 1000,
	})
	if err != nil {
		t.Fatalf("插入委托 1 失败: %v", err)
	}
	_, err = db.InsertOrder(&Order{
		PositionID: 1, ExchangeOrderID: 10002, Symbol: "BTCUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.02, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入委托 2 失败: %v", err)
	}

	// 1 条属于 position_id=2
	_, err = db.InsertOrder(&Order{
		PositionID: 2, ExchangeOrderID: 10003, Symbol: "ETHUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.03, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入委托 3 失败: %v", err)
	}

	orders, err := db.GetOrdersByPosition(1)
	if err != nil {
		t.Fatalf("GetOrdersByPosition 失败: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("期望 2 条委托, 实际 = %d", len(orders))
	}
	for _, o := range orders {
		if o.PositionID != 1 {
			t.Errorf("期望 position_id=1, 实际 = %d", o.PositionID)
		}
	}
}

// TestGetActiveOrders 测试获取活跃委托。
// 插入 NEW、FILLED、CANCELED 各 1 条，验证 GetActiveOrders 只返回状态为 NEW 的 1 条。
func TestGetActiveOrders(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	statuses := []string{"NEW", "FILLED", "CANCELED"}
	for i, s := range statuses {
		_, err := db.InsertOrder(&Order{
			PositionID: 1, ExchangeOrderID: int64(20001 + i), Symbol: "BTCUSDT",
			OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: s,
			Amount: 0.01, CreatedAt: now + int64(i*1000), UpdatedAt: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入 %s 委托失败: %v", s, err)
		}
	}

	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(orders) != 1 {
		t.Fatalf("期望 1 条活跃委托, 实际 = %d", len(orders))
	}
	if orders[0].Status != "NEW" {
		t.Errorf("期望 status=NEW, 实际 = %s", orders[0].Status)
	}
}

// TestGetAllOrders_Filter 测试 GetAllOrders 的三种过滤模式。
// 插入 NEW、FILLED、CANCELED 各 1 条委托，分别验证:
//   - 空字符串: 返回全部 3 条
//   - "ACTIVE": 返回活跃委托 1 条（NEW）
//   - "FILLED": 精确匹配返回 1 条
func TestGetAllOrders_Filter(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	statuses := []string{"NEW", "FILLED", "CANCELED"}
	for i, s := range statuses {
		_, err := db.InsertOrder(&Order{
			PositionID: 1, ExchangeOrderID: int64(30001 + i), Symbol: "BTCUSDT",
			OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: s,
			Amount: 0.01, CreatedAt: now + int64(i*1000), UpdatedAt: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入 %s 委托失败: %v", s, err)
		}
	}

	// 空字符串 → 全部
	all, err := db.GetAllOrders("")
	if err != nil {
		t.Fatalf("GetAllOrders(\"\") 失败: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("空过滤: 期望 3 条, 实际 = %d", len(all))
	}

	// "ACTIVE" → 活跃委托
	active, err := db.GetAllOrders("ACTIVE")
	if err != nil {
		t.Fatalf("GetAllOrders(\"ACTIVE\") 失败: %v", err)
	}
	if len(active) != 1 {
		t.Fatalf("ACTIVE 过滤: 期望 1 条, 实际 = %d", len(active))
	}
	if active[0].Status != "NEW" {
		t.Errorf("ACTIVE 过滤: 期望 status=NEW, 实际 = %s", active[0].Status)
	}

	// "FILLED" → 精确匹配
	filled, err := db.GetAllOrders("FILLED")
	if err != nil {
		t.Fatalf("GetAllOrders(\"FILLED\") 失败: %v", err)
	}
	if len(filled) != 1 {
		t.Fatalf("FILLED 过滤: 期望 1 条, 实际 = %d", len(filled))
	}
	if filled[0].Status != "FILLED" {
		t.Errorf("FILLED 过滤: 期望 status=FILLED, 实际 = %s", filled[0].Status)
	}
}

// TestGetOrdersByPositionIDs 测试按持仓 ID 列表批量查询委托。
// 为持仓 1 插入 2 条委托、持仓 2 插入 1 条、持仓 3 插入 1 条，验证:
//   - 查询 [1,2] 返回 3 条且均属于这两个持仓
//   - 空列表直接返回空结果，不报错
func TestGetOrdersByPositionIDs(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 持仓 1 两条委托，持仓 2、3 各一条
	seed := []struct {
		positionID int64
		exchangeID int64
	}{
		{1, 50001}, {1, 50002}, {2, 50003}, {3, 50004},
	}
	for i, s := range seed {
		_, err := db.InsertOrder(&Order{
			PositionID: s.positionID, ExchangeOrderID: s.exchangeID, Symbol: "BTCUSDT",
			OrderType: "STOP_MARKET", Side: "SELL", Status: "NEW",
			Amount: 0.01, CreatedAt: now + int64(i*1000), UpdatedAt: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入委托失败: %v", err)
		}
	}

	// 查询持仓 1、2 → 期望 3 条
	orders, err := db.GetOrdersByPositionIDs([]int64{1, 2})
	if err != nil {
		t.Fatalf("GetOrdersByPositionIDs 失败: %v", err)
	}
	if len(orders) != 3 {
		t.Fatalf("期望 3 条委托, 实际 = %d", len(orders))
	}
	for _, o := range orders {
		if o.PositionID != 1 && o.PositionID != 2 {
			t.Errorf("返回了非目标持仓的委托: positionID=%d", o.PositionID)
		}
	}

	// 空列表 → 直接返回空，不报错
	empty, err := db.GetOrdersByPositionIDs(nil)
	if err != nil {
		t.Fatalf("空列表查询不应报错: %v", err)
	}
	if len(empty) != 0 {
		t.Errorf("空列表查询期望 0 条, 实际 = %d", len(empty))
	}
}

// TestUpdateOrderStatus 测试更新委托状态和成交信息。
// 插入委托后调用 UpdateOrderStatus(id, "FILLED", 100.5, 0.01)，
// 验证 status、filled_price、filled_amount 已更新，且 updated_at 发生变化。
func TestUpdateOrderStatus(t *testing.T) {
	db := newTestDB(t)
	oldTime := int64(1000) // 使用一个很早的时间戳作为初始值

	id, err := db.InsertOrder(&Order{
		PositionID: 1, ExchangeOrderID: 40001, Symbol: "BTCUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.01, CreatedAt: oldTime, UpdatedAt: oldTime,
	})
	if err != nil {
		t.Fatalf("InsertOrder 失败: %v", err)
	}

	fp := 100.5
	fa := 0.01
	err = db.UpdateOrderStatus(id, "FILLED", &fp, &fa)
	if err != nil {
		t.Fatalf("UpdateOrderStatus 失败: %v", err)
	}

	var status string
	var filledPrice, filledAmount float64
	var updatedAt int64
	err = db.Conn.QueryRow(
		`SELECT status, filled_price, filled_amount, updated_at FROM orders WHERE id=?`, id,
	).Scan(&status, &filledPrice, &filledAmount, &updatedAt)
	if err != nil {
		t.Fatalf("查询委托失败: %v", err)
	}

	if status != "FILLED" {
		t.Errorf("期望 status=FILLED, 实际 = %s", status)
	}
	if filledPrice != 100.5 {
		t.Errorf("期望 filled_price=100.5, 实际 = %f", filledPrice)
	}
	if filledAmount != 0.01 {
		t.Errorf("期望 filled_amount=0.01, 实际 = %f", filledAmount)
	}
	if updatedAt == oldTime {
		t.Error("期望 updated_at 发生变化, 实际未变")
	}
}

// TestGetOrderByExchangeID 测试按交易所委托 ID 查询。
// 插入委托后通过 exchange_order_id 查询，验证返回正确的记录。
func TestGetOrderByExchangeID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	_, err := db.InsertOrder(&Order{
		PositionID: 1, ExchangeOrderID: 50001, Symbol: "BTCUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.01, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("InsertOrder 失败: %v", err)
	}

	order, err := db.GetOrderByExchangeID(50001)
	if err != nil {
		t.Fatalf("GetOrderByExchangeID 失败: %v", err)
	}
	if order == nil {
		t.Fatal("期望返回委托记录, 实际 = nil")
	}
	if order.ExchangeOrderID != 50001 {
		t.Errorf("期望 exchange_order_id=50001, 实际 = %d", order.ExchangeOrderID)
	}
	if order.Symbol != "BTCUSDT" {
		t.Errorf("期望 symbol=BTCUSDT, 实际 = %s", order.Symbol)
	}
}

// TestGetOrderByExchangeID_NotFound 测试查询不存在的交易所委托 ID。
// 验证返回 nil, nil（无记录且无错误）。
func TestGetOrderByExchangeID_NotFound(t *testing.T) {
	db := newTestDB(t)

	order, err := db.GetOrderByExchangeID(999999)
	if err != nil {
		t.Fatalf("期望无错误, 实际 = %v", err)
	}
	if order != nil {
		t.Fatalf("期望 nil, 实际 = %+v", order)
	}
}

// ==================== 四、OrderEvent Repo 测试 ====================

// TestInsertOrderEvent 测试插入委托事件记录。
// 验证 InsertOrderEvent 无错误返回。
func TestInsertOrderEvent(t *testing.T) {
	db := newTestDB(t)

	msg := "委托已创建"
	err := db.InsertOrderEvent(&OrderEvent{
		OrderID:         1,
		ExchangeOrderID: 60001,
		EventType:       EventCreated,
		NewStatus:       strPtr("NEW"),
		Message:         &msg,
		Timestamp:       time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("InsertOrderEvent 失败: %v", err)
	}
}

// TestGetOrderEvents 测试按委托 ID 查询事件流水。
// 插入 5 条同 order_id + 2 条不同 order_id 的事件，
// 调用 GetOrderEvents(orderID, 3) 验证返回 3 条且按 timestamp 降序排列。
func TestGetOrderEvents(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 5 条属于 order_id=1
	for i := 0; i < 5; i++ {
		err := db.InsertOrderEvent(&OrderEvent{
			OrderID: 1, ExchangeOrderID: 60001,
			EventType: EventStatusChange, Timestamp: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入事件 %d 失败: %v", i, err)
		}
	}

	// 2 条属于 order_id=2
	for i := 0; i < 2; i++ {
		err := db.InsertOrderEvent(&OrderEvent{
			OrderID: 2, ExchangeOrderID: 60002,
			EventType: EventCreated, Timestamp: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入 order_id=2 事件 %d 失败: %v", i, err)
		}
	}

	events, err := db.GetOrderEvents(1, 3)
	if err != nil {
		t.Fatalf("GetOrderEvents 失败: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("期望 3 条事件, 实际 = %d", len(events))
	}
	// 验证降序
	for i := 1; i < len(events); i++ {
		if events[i-1].Timestamp < events[i].Timestamp {
			t.Errorf("事件未按 timestamp 降序排列: events[%d].Timestamp=%d < events[%d].Timestamp=%d",
				i-1, events[i-1].Timestamp, i, events[i].Timestamp)
		}
	}
	// 验证全部属于 order_id=1
	for _, e := range events {
		if e.OrderID != 1 {
			t.Errorf("期望 order_id=1, 实际 = %d", e.OrderID)
		}
	}
}

// TestGetOrderEvents_AllOrders 测试 orderID=0 时查询全部委托的事件。
// 插入 7 条事件（分属不同 order_id），验证 GetOrderEvents(0, 100) 返回全部 7 条。
func TestGetOrderEvents_AllOrders(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	for i := 0; i < 5; i++ {
		err := db.InsertOrderEvent(&OrderEvent{
			OrderID: 1, ExchangeOrderID: 60001,
			EventType: EventStatusChange, Timestamp: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入 order_id=1 事件 %d 失败: %v", i, err)
		}
	}
	for i := 0; i < 2; i++ {
		err := db.InsertOrderEvent(&OrderEvent{
			OrderID: 2, ExchangeOrderID: 60002,
			EventType: EventCreated, Timestamp: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入 order_id=2 事件 %d 失败: %v", i, err)
		}
	}

	events, err := db.GetOrderEvents(0, 100)
	if err != nil {
		t.Fatalf("GetOrderEvents(0, 100) 失败: %v", err)
	}
	if len(events) != 7 {
		t.Fatalf("期望 7 条事件, 实际 = %d", len(events))
	}
}

// TestGetRecentOrderEvents 测试查询最近的事件流水。
// 插入 5 条事件，调用 GetRecentOrderEvents(2) 验证返回最新 2 条且按 timestamp 降序。
func TestGetRecentOrderEvents(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	for i := 0; i < 5; i++ {
		msg := fmt.Sprintf("事件 %d", i)
		err := db.InsertOrderEvent(&OrderEvent{
			OrderID: 1, ExchangeOrderID: 60001,
			EventType: EventStatusChange, Message: &msg,
			Timestamp: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入事件 %d 失败: %v", i, err)
		}
	}

	events, err := db.GetRecentOrderEvents(2)
	if err != nil {
		t.Fatalf("GetRecentOrderEvents 失败: %v", err)
	}
	if len(events) != 2 {
		t.Fatalf("期望 2 条事件, 实际 = %d", len(events))
	}
	// 最新的应为 "事件 4"
	if *events[0].Message != "事件 4" {
		t.Errorf("期望第 1 条为 '事件 4', 实际 = %s", *events[0].Message)
	}
	if *events[1].Message != "事件 3" {
		t.Errorf("期望第 2 条为 '事件 3', 实际 = %s", *events[1].Message)
	}
}

// ==================== 五、边界条件测试 ====================

// TestClosePosition_NonExistent 测试对不存在的持仓执行平仓。
// SQLite UPDATE 不影响任何行时不报错，验证 ClosePosition(999999, ...) 返回 nil。
func TestClosePosition_NonExistent(t *testing.T) {
	db := newTestDB(t)

	err := db.ClosePosition(999999, "STOP_LOSS", 0, nil, 0)
	if err != nil {
		t.Fatalf("期望不报错, 实际 = %v", err)
	}
}

// TestUpdateOrderStatus_NonExistent 测试对不存在的委托更新状态。
// SQLite UPDATE 不影响任何行时不报错，验证 UpdateOrderStatus(999999, ...) 返回 nil。
func TestUpdateOrderStatus_NonExistent(t *testing.T) {
	db := newTestDB(t)

	fp := 100.0
	fa := 0.01
	err := db.UpdateOrderStatus(999999, "FILLED", &fp, &fa)
	if err != nil {
		t.Fatalf("期望不报错, 实际 = %v", err)
	}
}

// TestConcurrentInsert 测试并发写入安全性。
// 启动 10 个 goroutine 并发调用 InsertLog，验证全部成功写入（WAL 模式并发安全性），
// 最终查询确认 10 条日志全部存在。
func TestConcurrentInsert(t *testing.T) {
	db := newTestDB(t)

	var wg sync.WaitGroup
	errs := make(chan error, 10)

	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			err := db.InsertLog(&TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "info",
				Module:    "concurrent",
				Message:   fmt.Sprintf("并发日志 %d", n),
			})
			if err != nil {
				errs <- err
			}
		}(i)
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		t.Errorf("并发插入失败: %v", err)
	}

	logs, err := db.GetRecentLogs(100)
	if err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	if len(logs) != 10 {
		t.Fatalf("期望 10 条日志, 实际 = %d", len(logs))
	}
}

// strPtr 返回字符串指针的辅助函数。
// 参数:
//   - s: 目标字符串
//
// 返回:
//   - *string: 指向 s 的指针
func strPtr(s string) *string {
	return &s
}

// ==================== 五、今日盈亏与平仓查询测试 ====================

// TestGetTodayPnl 测试今日盈亏聚合查询
// 验证点: 插入今日平仓记录后，GetTodayPnl 返回正确的总和与次数
func TestGetTodayPnl(t *testing.T) {
	db := newTestDB(t)

	// 插入持仓并平仓（今日）
	id1, _ := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	db.ClosePosition(id1, "STOP_LOSS", 5.5, nil, 0)

	id2, _ := db.InsertPosition(&Position{
		Symbol: "ETHUSDT", Side: "LONG", EntryPrice: 200, Amount: 2,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	db.ClosePosition(id2, "TRAILING_STOP", -3.2, nil, 0)

	totalPnl, closedCount, err := db.GetTodayPnl()
	if err != nil {
		t.Fatalf("GetTodayPnl 失败: %v", err)
	}
	if closedCount != 2 {
		t.Errorf("closedCount = %d, 期望 2", closedCount)
	}
	expected := 5.5 + (-3.2)
	if diff := totalPnl - expected; diff > 0.001 || diff < -0.001 {
		t.Errorf("totalPnl = %f, 期望 %f", totalPnl, expected)
	}
}

// TestGetTodayPnl_Empty 测试无平仓记录时返回零值
func TestGetTodayPnl_Empty(t *testing.T) {
	db := newTestDB(t)

	totalPnl, closedCount, err := db.GetTodayPnl()
	if err != nil {
		t.Fatalf("GetTodayPnl 失败: %v", err)
	}
	if totalPnl != 0 || closedCount != 0 {
		t.Errorf("空库应返回 0, 实际 pnl=%f count=%d", totalPnl, closedCount)
	}
}

// TestGetClosedPositions 测试已平仓持仓查询
// 验证点: 返回 CLOSED 状态的持仓，按平仓时间降序，OPEN 状态不返回
func TestGetClosedPositions(t *testing.T) {
	db := newTestDB(t)

	// 插入 3 条持仓，平仓 2 条
	id1, _ := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	db.ClosePosition(id1, "STOP_LOSS", 2.0, nil, 0)

	id2, _ := db.InsertPosition(&Position{
		Symbol: "ETHUSDT", Side: "LONG", EntryPrice: 200, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	db.ClosePosition(id2, "TRAILING_STOP", -1.0, nil, 0)

	// 第 3 条保持 OPEN
	db.InsertPosition(&Position{
		Symbol: "SOLUSDT", Side: "LONG", EntryPrice: 50, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})

	closed, err := db.GetClosedPositions(10)
	if err != nil {
		t.Fatalf("GetClosedPositions 失败: %v", err)
	}
	if len(closed) != 2 {
		t.Fatalf("期望 2 条平仓记录, 实际 = %d", len(closed))
	}
	// 验证都是 CLOSED 状态
	for _, p := range closed {
		if p.Status != "CLOSED" {
			t.Errorf("持仓 %s 状态 = %q, 期望 CLOSED", p.Symbol, p.Status)
		}
	}
}

// ==================== 六、幂等守卫与单条查询测试 ====================

// TestClosePosition_Idempotent 测试 ClosePosition 幂等性
// 验证点: 对已平仓持仓再次调用 ClosePosition 不会覆盖原有 PnL
func TestClosePosition_Idempotent(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})

	// 第一次平仓：PnL = 5.0
	db.ClosePosition(id, "STOP_LOSS", 5.0, nil, 0)

	// 第二次平仓（竞态模拟）：PnL = 0，不应覆盖
	db.ClosePosition(id, "TRAILING_STOP", 0, nil, 0)

	// 验证 PnL 仍为 5.0，reason 仍为 STOP_LOSS
	pos, err := db.GetPositionByID(id)
	if err != nil || pos == nil {
		t.Fatalf("GetPositionByID 失败: %v", err)
	}
	if pos.RealizedPnl == nil || *pos.RealizedPnl != 5.0 {
		t.Errorf("PnL = %v, 期望 5.0（不应被覆盖）", pos.RealizedPnl)
	}
	if pos.CloseReason == nil || *pos.CloseReason != "STOP_LOSS" {
		t.Errorf("CloseReason = %v, 期望 STOP_LOSS（不应被覆盖）", pos.CloseReason)
	}
}

// TestGetPositionByID 测试单条持仓查询
// 验证点: 存在时返回正确记录，不存在时返回 nil
func TestGetPositionByID(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.InsertPosition(&Position{
		Symbol: "ETHUSDT", Side: "LONG", EntryPrice: 200, Amount: 2,
		Leverage: 5, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})

	// 查询存在的持仓
	pos, err := db.GetPositionByID(id)
	if err != nil || pos == nil {
		t.Fatalf("GetPositionByID(%d) 失败: %v", id, err)
	}
	if pos.Symbol != "ETHUSDT" || pos.EntryPrice != 200 {
		t.Errorf("持仓数据不正确: %+v", pos)
	}

	// 查询不存在的持仓
	pos, err = db.GetPositionByID(99999)
	if err != nil {
		t.Fatalf("不存在的 ID 不应返回错误: %v", err)
	}
	if pos != nil {
		t.Error("不存在的 ID 应返回 nil")
	}
}

// TestGetPositionByID_DBError 验证 DB 故障时返回错误而不是被误判为"记录不存在"。
// 回归背景：旧实现把除 ErrNoRows 外的所有查询错误都吞掉并返回 nil, nil，
// 调用方（如委托同步）会把 DB 故障当成持仓不存在，静默跳过风控闭环。
func TestGetPositionByID_DBError(t *testing.T) {
	db := newTestDB(t)
	if err := db.Close(); err != nil {
		t.Fatalf("关闭测试数据库失败: %v", err)
	}
	pos, err := db.GetPositionByID(1)
	if err == nil {
		t.Fatal("数据库已关闭时 GetPositionByID 应返回错误")
	}
	if pos != nil {
		t.Error("查询失败时不应返回持仓记录")
	}
}

// TestUpdateRiskState_ClosedGuard 测试 UpdateRiskState 对已平仓持仓无效
// 验证点: 平仓后调用 UpdateRiskState 不修改任何字段
func TestUpdateRiskState_ClosedGuard(t *testing.T) {
	db := newTestDB(t)

	id, _ := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
		CurrentStopPrice: 90,
	})

	// 平仓
	db.ClosePosition(id, "STOP_LOSS", -10, nil, 0)

	// 尝试更新已平仓持仓的风控状态
	hp := 120.0
	db.UpdateRiskState(id, &hp, true, 115)

	// 验证风控状态未被修改
	pos, _ := db.GetPositionByID(id)
	if pos.TrailingActive {
		t.Error("已平仓持仓的 TrailingActive 不应被更新")
	}
	if pos.CurrentStopPrice != 90 {
		t.Errorf("已平仓持仓的 StopPrice = %f, 期望 90（不应被更新）", pos.CurrentStopPrice)
	}
}

// ==================== 七、补充覆盖率测试 ====================

// TestGetOpenOrders 测试获取活跃（未成交）委托列表。
// 插入 NEW、PARTIALLY_FILLED、FILLED、CANCELED 各 1 条委托，
// 验证 GetActiveOrders 只返回 NEW 和 PARTIALLY_FILLED 状态的 2 条记录。
func TestGetOpenOrders(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	statuses := []string{"NEW", "PARTIALLY_FILLED", "FILLED", "CANCELED"}
	for i, s := range statuses {
		_, err := db.InsertOrder(&Order{
			PositionID: 1, ExchangeOrderID: int64(70001 + i), Symbol: "BTCUSDT",
			OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: s,
			Amount: 0.01, CreatedAt: now + int64(i*1000), UpdatedAt: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入 %s 委托失败: %v", s, err)
		}
	}

	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("期望 2 条活跃委托, 实际 = %d", len(orders))
	}
	for _, o := range orders {
		if o.Status != "NEW" && o.Status != "PARTIALLY_FILLED" {
			t.Errorf("返回了非活跃委托: status=%s", o.Status)
		}
	}
}

// TestGetOrdersByPositionID 测试按持仓 ID 查询关联委托。
// 为持仓 10 插入 2 条委托、持仓 20 插入 1 条委托，
// 验证 GetOrdersByPosition(10) 只返回持仓 10 的 2 条记录且按 created_at 降序。
func TestGetOrdersByPositionID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 持仓 10 的两条委托
	_, err := db.InsertOrder(&Order{
		PositionID: 10, ExchangeOrderID: 80001, Symbol: "BTCUSDT",
		OrderType: "STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.01, CreatedAt: now - 2000, UpdatedAt: now - 2000,
	})
	if err != nil {
		t.Fatalf("插入委托 1 失败: %v", err)
	}
	_, err = db.InsertOrder(&Order{
		PositionID: 10, ExchangeOrderID: 80002, Symbol: "BTCUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "FILLED",
		Amount: 0.02, CreatedAt: now - 1000, UpdatedAt: now - 1000,
	})
	if err != nil {
		t.Fatalf("插入委托 2 失败: %v", err)
	}

	// 持仓 20 的一条委托
	_, err = db.InsertOrder(&Order{
		PositionID: 20, ExchangeOrderID: 80003, Symbol: "ETHUSDT",
		OrderType: "STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.03, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入委托 3 失败: %v", err)
	}

	orders, err := db.GetOrdersByPosition(10)
	if err != nil {
		t.Fatalf("GetOrdersByPosition 失败: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("期望 2 条委托, 实际 = %d", len(orders))
	}
	for _, o := range orders {
		if o.PositionID != 10 {
			t.Errorf("期望 position_id=10, 实际 = %d", o.PositionID)
		}
	}
	// 验证降序：第 1 条应为 exchange_order_id=80002（created_at 更大）
	if orders[0].ExchangeOrderID != 80002 {
		t.Errorf("期望第 1 条为 80002, 实际 = %d", orders[0].ExchangeOrderID)
	}
}

// TestGetOrderEventsByPositionID 测试按持仓 ID 查询关联的委托事件。
// 为持仓 1 创建 2 条委托并各插入事件，为持仓 2 创建 1 条委托并插入事件，
// 验证 GetOrderEventsByPositionID(1, 100) 只返回持仓 1 关联的事件。
func TestGetOrderEventsByPositionID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 持仓 1 的委托
	orderID1, err := db.InsertOrder(&Order{
		PositionID: 1, ExchangeOrderID: 90001, Symbol: "BTCUSDT",
		OrderType: "TRAILING_STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.01, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入委托 1 失败: %v", err)
	}
	orderID2, err := db.InsertOrder(&Order{
		PositionID: 1, ExchangeOrderID: 90002, Symbol: "BTCUSDT",
		OrderType: "STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.02, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入委托 2 失败: %v", err)
	}

	// 持仓 2 的委托
	orderID3, err := db.InsertOrder(&Order{
		PositionID: 2, ExchangeOrderID: 90003, Symbol: "ETHUSDT",
		OrderType: "STOP_MARKET", Side: "SELL", Status: "NEW",
		Amount: 0.03, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil {
		t.Fatalf("插入委托 3 失败: %v", err)
	}

	// 为委托 1 插入 2 条事件
	for i := 0; i < 2; i++ {
		err = db.InsertOrderEvent(&OrderEvent{
			OrderID: orderID1, ExchangeOrderID: 90001,
			EventType: EventStatusChange, Timestamp: now + int64(i*1000),
		})
		if err != nil {
			t.Fatalf("插入事件失败: %v", err)
		}
	}
	// 为委托 2 插入 1 条事件
	err = db.InsertOrderEvent(&OrderEvent{
		OrderID: orderID2, ExchangeOrderID: 90002,
		EventType: EventCreated, Timestamp: now + 3000,
	})
	if err != nil {
		t.Fatalf("插入事件失败: %v", err)
	}
	// 为委托 3（持仓 2）插入 1 条事件
	err = db.InsertOrderEvent(&OrderEvent{
		OrderID: orderID3, ExchangeOrderID: 90003,
		EventType: EventCreated, Timestamp: now + 4000,
	})
	if err != nil {
		t.Fatalf("插入事件失败: %v", err)
	}

	// 查询持仓 1 的事件
	events, err := db.GetOrderEventsByPositionID(1, 100)
	if err != nil {
		t.Fatalf("GetOrderEventsByPositionID 失败: %v", err)
	}
	if len(events) != 3 {
		t.Fatalf("期望 3 条事件, 实际 = %d", len(events))
	}
	// 验证全部属于持仓 1 的委托
	for _, e := range events {
		if e.OrderID != orderID1 && e.OrderID != orderID2 {
			t.Errorf("返回了非持仓 1 的事件: orderID=%d", e.OrderID)
		}
	}
}

// TestInsertTradeLog 测试插入交易日志并验证可检索。
// 插入一条带完整字段的日志，通过 GetRecentLogs 验证数据完整性。
func TestInsertTradeLog(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	err := db.InsertLog(&TradeLog{
		Timestamp:  now,
		Level:      "warn",
		Module:     "risk",
		Message:    "触发止损",
		Symbol:     "ETHUSDT",
		Price:      2500.5,
		Amount:     0.5,
		PositionID: 42,
	})
	if err != nil {
		t.Fatalf("InsertLog 失败: %v", err)
	}

	logs, err := db.GetRecentLogs(10)
	if err != nil {
		t.Fatalf("GetRecentLogs 失败: %v", err)
	}
	if len(logs) != 1 {
		t.Fatalf("期望 1 条日志, 实际 = %d", len(logs))
	}

	l := logs[0]
	if l.Level != "warn" {
		t.Errorf("期望 level=warn, 实际 = %s", l.Level)
	}
	if l.Module != "risk" {
		t.Errorf("期望 module=risk, 实际 = %s", l.Module)
	}
	if l.Message != "触发止损" {
		t.Errorf("期望 message=触发止损, 实际 = %s", l.Message)
	}
	if l.Symbol != "ETHUSDT" {
		t.Errorf("期望 symbol=ETHUSDT, 实际 = %s", l.Symbol)
	}
	if l.Price != 2500.5 {
		t.Errorf("期望 price=2500.5, 实际 = %f", l.Price)
	}
	if l.Amount != 0.5 {
		t.Errorf("期望 amount=0.5, 实际 = %f", l.Amount)
	}
	if l.PositionID != 42 {
		t.Errorf("期望 position_id=42, 实际 = %d", l.PositionID)
	}
}

// TestGetTradeLogsByPositionID 测试按持仓 ID 查询交易日志。
// 为持仓 5 插入 2 条日志、持仓 6 插入 1 条日志，
// 验证 GetTradeLogsByPositionID(5, 100) 只返回持仓 5 的 2 条记录。
func TestGetTradeLogsByPositionID(t *testing.T) {
	db := newTestDB(t)
	now := time.Now().UnixMilli()

	// 持仓 5 的日志
	for i := 0; i < 2; i++ {
		err := db.InsertLog(&TradeLog{
			Timestamp:  now + int64(i*1000),
			Level:      "info",
			Module:     "strategy",
			Message:    fmt.Sprintf("持仓5日志 %d", i),
			PositionID: 5,
		})
		if err != nil {
			t.Fatalf("插入日志失败: %v", err)
		}
	}
	// 持仓 6 的日志
	err := db.InsertLog(&TradeLog{
		Timestamp:  now + 3000,
		Level:      "info",
		Module:     "strategy",
		Message:    "持仓6日志",
		PositionID: 6,
	})
	if err != nil {
		t.Fatalf("插入日志失败: %v", err)
	}

	logs, err := db.GetTradeLogsByPositionID(5, 100)
	if err != nil {
		t.Fatalf("GetTradeLogsByPositionID 失败: %v", err)
	}
	if len(logs) != 2 {
		t.Fatalf("期望 2 条日志, 实际 = %d", len(logs))
	}
	for _, l := range logs {
		if l.PositionID != 5 {
			t.Errorf("期望 position_id=5, 实际 = %d", l.PositionID)
		}
	}
}

// TestUpdatePositionHighestPrice 测试单独更新持仓最高价。
// 插入持仓后调用 UpdatePositionHighestPrice，验证 highest_price 已更新。
func TestUpdatePositionHighestPrice(t *testing.T) {
	db := newTestDB(t)

	id, err := db.InsertPosition(&Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 0.01,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("插入持仓失败: %v", err)
	}

	err = db.UpdatePositionHighestPrice(id, 155.5)
	if err != nil {
		t.Fatalf("UpdatePositionHighestPrice 失败: %v", err)
	}

	pos, err := db.GetPositionByID(id)
	if err != nil || pos == nil {
		t.Fatalf("GetPositionByID 失败: %v", err)
	}
	if pos.HighestPrice == nil || *pos.HighestPrice != 155.5 {
		t.Errorf("期望 highest_price=155.5, 实际 = %v", pos.HighestPrice)
	}
}

// TestUpdatePositionTrailingActivated 测试单独更新持仓跟踪止盈激活状态。
// 插入持仓后调用 UpdatePositionTrailingActivated(id, true)，验证 trailing_active 已更新。
func TestUpdatePositionTrailingActivated(t *testing.T) {
	db := newTestDB(t)

	id, err := db.InsertPosition(&Position{
		Symbol: "ETHUSDT", Side: "LONG", EntryPrice: 200, Amount: 0.05,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("插入持仓失败: %v", err)
	}

	// 初始状态应为 false
	pos, _ := db.GetPositionByID(id)
	if pos.TrailingActive {
		t.Fatal("初始 trailing_active 应为 false")
	}

	err = db.UpdatePositionTrailingActivated(id, true)
	if err != nil {
		t.Fatalf("UpdatePositionTrailingActivated 失败: %v", err)
	}

	pos, err = db.GetPositionByID(id)
	if err != nil || pos == nil {
		t.Fatalf("GetPositionByID 失败: %v", err)
	}
	if !pos.TrailingActive {
		t.Error("期望 trailing_active=true, 实际 = false")
	}
}

// TestGetKeyValue_SetKeyValue 测试键值对的设置与获取。
// 设置一个键值对后通过 GetKeyValue 验证返回值正确。
func TestGetKeyValue_SetKeyValue(t *testing.T) {
	db := newTestDB(t)

	err := db.SetKeyValue("test_key", "test_value_123")
	if err != nil {
		t.Fatalf("SetKeyValue 失败: %v", err)
	}

	value, err := db.GetKeyValue("test_key")
	if err != nil {
		t.Fatalf("GetKeyValue 失败: %v", err)
	}
	if value != "test_value_123" {
		t.Errorf("期望 value=test_value_123, 实际 = %s", value)
	}

	// 测试覆盖更新
	err = db.SetKeyValue("test_key", "updated_value")
	if err != nil {
		t.Fatalf("SetKeyValue 更新失败: %v", err)
	}
	value, err = db.GetKeyValue("test_key")
	if err != nil {
		t.Fatalf("GetKeyValue 更新后查询失败: %v", err)
	}
	if value != "updated_value" {
		t.Errorf("期望 value=updated_value, 实际 = %s", value)
	}
}

// TestGetKeyValue_NotFound 测试获取不存在的键返回空字符串。
// 验证 GetKeyValue 对不存在的键返回空字符串且无错误。
func TestGetKeyValue_NotFound(t *testing.T) {
	db := newTestDB(t)

	value, err := db.GetKeyValue("non_existent_key")
	if err != nil {
		t.Fatalf("GetKeyValue 不应返回错误: %v", err)
	}
	if value != "" {
		t.Errorf("期望空字符串, 实际 = %q", value)
	}
}

// TestDeletePosition 测试删除持仓记录。
// 插入持仓后调用 DeletePosition，验证记录已被删除（GetPositionByID 返回 nil）。
func TestDeletePosition(t *testing.T) {
	db := newTestDB(t)

	id, err := db.InsertPosition(&Position{
		Symbol: "SOLUSDT", Side: "SHORT", EntryPrice: 50, Amount: 0.1,
		Leverage: 5, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("插入持仓失败: %v", err)
	}

	// 确认存在
	pos, _ := db.GetPositionByID(id)
	if pos == nil {
		t.Fatal("插入后应能查询到持仓")
	}

	// 删除
	err = db.DeletePosition(id)
	if err != nil {
		t.Fatalf("DeletePosition 失败: %v", err)
	}

	// 验证已删除
	pos, err = db.GetPositionByID(id)
	if err != nil {
		t.Fatalf("查询已删除持仓不应返回错误: %v", err)
	}
	if pos != nil {
		t.Error("删除后 GetPositionByID 应返回 nil")
	}
}
