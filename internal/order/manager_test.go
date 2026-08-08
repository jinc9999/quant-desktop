// Package order 委托管理器单元测试
package order

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/storage"
)

// setupTestEnv 创建测试环境（临时 DB + DRY_RUN Client + Manager）
// 参数:
//   - t: 测试实例，用于创建临时目录和注册清理函数
//
// 返回:
//   - *Manager: 初始化好的委托管理器
//   - *storage.DB: 临时数据库实例
func setupTestEnv(t *testing.T) (*Manager, *storage.DB) {
	t.Helper()

	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.NewDB(dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	client := binance.NewClient("", "", "DRY_RUN", "", 0)
	mgr := NewManager(client, db)
	return mgr, db
}

// insertTestPosition 向数据库插入一条测试持仓记录
// 参数:
//   - t: 测试实例
//   - db: 数据库实例
//
// 返回:
//   - *storage.Position: 已入库的持仓记录（含自增 ID）
func insertTestPosition(t *testing.T, db *storage.DB) *storage.Position {
	t.Helper()

	pos := &storage.Position{
		Symbol:     "BTCUSDT",
		Side:       "LONG",
		EntryPrice: 50000.0,
		Amount:     0.001,
		Leverage:   10,
		Status:     "OPEN",
		OpenedAt:   time.Now().UnixMilli(),
	}
	id, err := db.InsertPosition(pos)
	if err != nil {
		t.Fatalf("插入测试持仓失败: %v", err)
	}
	pos.ID = id
	return pos
}

// testStrategyConfig 返回测试用策略配置
// 返回:
//   - binance.StrategyConfig: 包含止损和跟踪止损参数的配置
func testStrategyConfig() binance.StrategyConfig {
	return binance.StrategyConfig{
		StopLossPct:        0.10,
		TrailingActivation: 0.04,
		TrailingCallback:   0.04,
	}
}

// TestNewManager 验证 NewManager 创建成功，maxRetries=3, retryDelay=1s
func TestNewManager(t *testing.T) {
	client := binance.NewClient("", "", "DRY_RUN", "", 0)
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.NewDB(dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	defer db.Close()

	mgr := NewManager(client, db)
	if mgr == nil {
		t.Fatal("NewManager 返回 nil")
	}
	if mgr.maxRetries != 3 {
		t.Errorf("maxRetries = %d, 期望 3", mgr.maxRetries)
	}
	if mgr.retryDelay != 1*time.Second {
		t.Errorf("retryDelay = %v, 期望 1s", mgr.retryDelay)
	}
}

// TestPlaceStopOrders_DryRun 验证 DRY_RUN 模式下挂出止损委托
// 验证点：无错误返回、DB 中有 2 条活跃委托（STOP_MARKET + TRAILING_STOP_MARKET）、2 条 CREATED 事件
func TestPlaceStopOrders_DryRun(t *testing.T) {
	mgr, db := setupTestEnv(t)
	pos := insertTestPosition(t, db)
	cfg := testStrategyConfig()

	err := mgr.PlaceStopOrders(context.Background(), pos, cfg)
	if err != nil {
		t.Fatalf("PlaceStopOrders 返回错误: %v", err)
	}

	// 验证活跃委托数为 2
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("活跃委托数 = %d, 期望 2", len(orders))
	}

	// 验证委托类型：一条 STOP_MARKET，一条 TRAILING_STOP_MARKET
	typeSet := map[string]bool{}
	for _, o := range orders {
		typeSet[o.OrderType] = true
	}
	if !typeSet[binance.OrderTypeStopMarket] {
		t.Error("缺少 STOP_MARKET 委托")
	}
	if !typeSet[binance.OrderTypeTrailingStop] {
		t.Error("缺少 TRAILING_STOP_MARKET 委托")
	}

	// 验证 CREATED 事件数为 2
	events, err := db.GetRecentOrderEvents(100)
	if err != nil {
		t.Fatalf("GetRecentOrderEvents 失败: %v", err)
	}
	createdCount := 0
	for _, e := range events {
		if e.EventType == storage.EventCreated {
			createdCount++
		}
	}
	if createdCount != 2 {
		t.Errorf("CREATED 事件数 = %d, 期望 2", createdCount)
	}
}

// TestPlaceStopOrders_Idempotent 验证幂等性：重复调用 PlaceStopOrders 不会重复挂单
func TestPlaceStopOrders_Idempotent(t *testing.T) {
	mgr, db := setupTestEnv(t)
	pos := insertTestPosition(t, db)
	cfg := testStrategyConfig()
	ctx := context.Background()

	// 第一次挂单
	if err := mgr.PlaceStopOrders(ctx, pos, cfg); err != nil {
		t.Fatalf("第一次 PlaceStopOrders 失败: %v", err)
	}

	// 第二次挂单（应被幂等检查跳过）
	if err := mgr.PlaceStopOrders(ctx, pos, cfg); err != nil {
		t.Fatalf("第二次 PlaceStopOrders 失败: %v", err)
	}

	// 验证活跃委托数仍为 2
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(orders) != 2 {
		t.Errorf("活跃委托数 = %d, 期望 2（幂等检查应阻止重复挂单）", len(orders))
	}
}

// TestCancelRelatedOrders 验证取消关联委托后活跃委托数为 0
func TestCancelRelatedOrders(t *testing.T) {
	mgr, db := setupTestEnv(t)
	pos := insertTestPosition(t, db)
	cfg := testStrategyConfig()
	ctx := context.Background()

	if err := mgr.PlaceStopOrders(ctx, pos, cfg); err != nil {
		t.Fatalf("PlaceStopOrders 失败: %v", err)
	}

	// 取消关联委托
	if err := mgr.CancelRelatedOrders(ctx, pos.ID); err != nil {
		t.Fatalf("CancelRelatedOrders 失败: %v", err)
	}

	// 验证活跃委托数为 0
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("活跃委托数 = %d, 期望 0", len(orders))
	}
}

// TestSyncOrders_DryRun 验证 DRY_RUN 模式下同步不报错（GetOrderStatus 返回 NEW，状态不变）
func TestSyncOrders_DryRun(t *testing.T) {
	mgr, db := setupTestEnv(t)
	pos := insertTestPosition(t, db)
	cfg := testStrategyConfig()
	ctx := context.Background()

	if err := mgr.PlaceStopOrders(ctx, pos, cfg); err != nil {
		t.Fatalf("PlaceStopOrders 失败: %v", err)
	}

	// DRY_RUN 模式下 GetOrderStatus 返回 NEW，与本地状态一致，不触发变更
	if err := mgr.SyncOrders(ctx, nil); err != nil {
		t.Fatalf("SyncOrders 返回错误: %v", err)
	}

	// 验证活跃委托数仍为 2（状态未变化）
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(orders) != 2 {
		t.Errorf("活跃委托数 = %d, 期望 2", len(orders))
	}
}

// TestRetryWithBackoff 验证重试机制：前 2 次失败，第 3 次成功，最终成功且计数器 = 3
func TestRetryWithBackoff(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.NewDB(dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}
	defer db.Close()

	client := binance.NewClient("", "", "DRY_RUN", "", 0)
	mgr := NewManager(client, db)
	// 缩短重试间隔以加速测试
	mgr.retryDelay = 1 * time.Millisecond

	counter := 0
	fn := func() error {
		counter++
		if counter < 3 {
			return fmt.Errorf("模拟失败 第%d次", counter)
		}
		return nil
	}

	err = mgr.retryWithBackoff(context.Background(), fn)
	if err != nil {
		t.Fatalf("retryWithBackoff 最终应成功, 但返回错误: %v", err)
	}
	if counter != 3 {
		t.Errorf("计数器 = %d, 期望 3", counter)
	}
}

// TestGetSyncStatus 验证 GetSyncStatus 返回的 map 包含 activeCount, lastSyncTime, lastSyncError 键
func TestGetSyncStatus(t *testing.T) {
	mgr, _ := setupTestEnv(t)

	status := mgr.GetSyncStatus()

	requiredKeys := []string{"activeCount", "lastSyncTime", "lastSyncError"}
	for _, key := range requiredKeys {
		if _, ok := status[key]; !ok {
			t.Errorf("GetSyncStatus 返回的 map 缺少键 %q", key)
		}
	}
}

// TestFilledCloseLoop_DryRun 验证 DRY_RUN 模式下止损委托成交后的完整平仓闭环
// 这是设计文档的核心可靠性路径：SyncOrders 检测到 FILLED → handleFilledOrder
// 验证点：
//  1. 持仓被标记为 CLOSED，close_reason = STOP_LOSS
//  2. 关联的另一条委托（跟踪止损单）被取消，活跃委托数归零
//  3. 写入 EventTriggered 触发事件
//
// 参数:
//   - t: 测试实例
func TestFilledCloseLoop_DryRun(t *testing.T) {
	mgr, db := setupTestEnv(t)
	pos := insertTestPosition(t, db)
	cfg := testStrategyConfig()
	ctx := context.Background()

	// 1. 开仓后挂出止损保护委托（STOP_MARKET + TRAILING_STOP_MARKET）
	if err := mgr.PlaceStopOrders(ctx, pos, cfg); err != nil {
		t.Fatalf("PlaceStopOrders 失败: %v", err)
	}

	// 2. 取出 STOP_MARKET 委托，模拟交易所回报其已成交
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	var stopOrder *storage.Order
	for i := range orders {
		if orders[i].OrderType == binance.OrderTypeStopMarket {
			stopOrder = &orders[i]
			break
		}
	}
	if stopOrder == nil {
		t.Fatal("未找到 STOP_MARKET 委托")
	}

	// 构造交易所成交回报（DRY_RUN 下 GetOrderStatus 恒返回 NEW，
	// 此处直接驱动 SyncOrders 在检测到 FILLED 时调用的 handleFilledOrder）
	filledInfo := &binance.OrderInfo{
		OrderID:      stopOrder.ExchangeOrderID,
		Symbol:       stopOrder.Symbol,
		Type:         binance.OrderTypeStopMarket,
		Side:         "SELL",
		Status:       binance.OrderStatusFilled,
		FilledPrice:  45000.0,
		FilledAmount: pos.Amount,
	}
	mgr.handleFilledOrder(ctx, stopOrder, filledInfo)

	// 3. 验证持仓已平仓：OPEN 持仓数为 0
	openPositions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("GetOpenPositions 失败: %v", err)
	}
	if len(openPositions) != 0 {
		t.Errorf("OPEN 持仓数 = %d, 期望 0（止损成交后应平仓）", len(openPositions))
	}

	// 4. 验证持仓 close_reason = STOP_LOSS
	var status string
	var closeReason string
	if err := db.Conn.QueryRow(
		`SELECT status, close_reason FROM positions WHERE id = ?`, pos.ID,
	).Scan(&status, &closeReason); err != nil {
		t.Fatalf("查询持仓状态失败: %v", err)
	}
	if status != "CLOSED" {
		t.Errorf("持仓状态 = %q, 期望 CLOSED", status)
	}
	if closeReason != "STOP_LOSS" {
		t.Errorf("close_reason = %q, 期望 STOP_LOSS", closeReason)
	}

	// 5. 验证关联委托已全部取消：活跃委托数为 0
	activeAfter, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	if len(activeAfter) != 0 {
		t.Errorf("活跃委托数 = %d, 期望 0（关联跟踪止损单应被取消）", len(activeAfter))
	}

	// 6. 验证写入了 EventTriggered 触发事件
	events, err := db.GetRecentOrderEvents(100)
	if err != nil {
		t.Fatalf("GetRecentOrderEvents 失败: %v", err)
	}
	triggeredCount := 0
	for _, e := range events {
		if e.EventType == storage.EventTriggered {
			triggeredCount++
		}
	}
	if triggeredCount != 1 {
		t.Errorf("EventTriggered 事件数 = %d, 期望 1", triggeredCount)
	}
}

// TestOnCloseFiredOnFilledClose 验证条件单触发平仓后，OnClose 回调被通知（冷却期闭环修复）。
// 背景：交易所条件单平仓是主平仓路径，此前从不通知引擎写冷却期，
// 导致同币无限快速重复开仓（实盘单币日开 40+ 次）。
func TestOnCloseFiredOnFilledClose(t *testing.T) {
	mgr, db := setupTestEnv(t)
	pos := insertTestPosition(t, db)
	cfg := testStrategyConfig()
	ctx := context.Background()

	closed := make([][2]string, 0, 2)
	mgr.OnClose = func(symbol, reason string) {
		closed = append(closed, [2]string{symbol, reason})
	}

	if err := mgr.PlaceStopOrders(ctx, pos, cfg); err != nil {
		t.Fatalf("PlaceStopOrders 失败: %v", err)
	}
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("GetActiveOrders 失败: %v", err)
	}
	var stopOrder *storage.Order
	for i := range orders {
		if orders[i].OrderType == binance.OrderTypeStopMarket {
			stopOrder = &orders[i]
			break
		}
	}
	if stopOrder == nil {
		t.Fatal("未找到 STOP_MARKET 委托")
	}

	filledInfo := &binance.OrderInfo{
		OrderID:      stopOrder.ExchangeOrderID,
		Symbol:       stopOrder.Symbol,
		Type:         binance.OrderTypeStopMarket,
		Side:         "SELL",
		Status:       binance.OrderStatusFilled,
		FilledPrice:  45000.0,
		FilledAmount: pos.Amount,
	}
	mgr.handleFilledOrder(ctx, stopOrder, filledInfo)

	if len(closed) != 1 || closed[0][0] != "BTCUSDT" || closed[0][1] != "STOP_LOSS" {
		t.Fatalf("OnClose 回调 = %v, 期望恰好一次 [BTCUSDT STOP_LOSS]", closed)
	}

	// 重复触发（幂等：持仓已 CLOSED，ClosePosition 无效果）不应再次通知
	mgr.handleFilledOrder(ctx, stopOrder, filledInfo)
	if len(closed) != 1 {
		t.Errorf("重复触发后回调次数 = %d, 期望仍为 1", len(closed))
	}
}

// TestPlaceStopOrders_MultiplePositionsSameSymbol 回归 -4130：
// 同币多仓（追加仓）各挂一张按数量止损单，互不冲突。
// 修复前 STOP_MARKET 使用 ClosePosition(true)，币安同一方向只允许一张 closePosition 条件单，
// 第 2 仓挂单必然 -4130 并触发回滚；修复后按数量+reduceOnly 挂单（与跟踪止损一致）。
func TestPlaceStopOrders_MultiplePositionsSameSymbol(t *testing.T) {
	mgr, db := setupTestEnv(t)
	ctx := context.Background()
	cfg := testStrategyConfig()

	// 同币两个独立持仓（模拟 1 首仓 + 1 追加仓）
	posA := insertTestPosition(t, db)
	posA.Symbol = "BTCUSDT"
	posB := insertTestPosition(t, db)
	posB.Symbol = "BTCUSDT"

	if err := mgr.PlaceStopOrders(ctx, posA, cfg); err != nil {
		t.Fatalf("首仓挂止损失败: %v", err)
	}
	if err := mgr.PlaceStopOrders(ctx, posB, cfg); err != nil {
		t.Fatalf("追加仓挂止损失败（-4130 回归）: %v", err)
	}

	for _, pos := range []*storage.Position{posA, posB} {
		orders, err := db.GetOrdersByPosition(pos.ID)
		if err != nil {
			t.Fatalf("查询持仓 %d 委托失败: %v", pos.ID, err)
		}
		if len(orders) != 2 {
			t.Fatalf("持仓 %d 委托数 = %d, 期望 2（STOP_MARKET + TRAILING_STOP_MARKET）", pos.ID, len(orders))
		}
	}
}
