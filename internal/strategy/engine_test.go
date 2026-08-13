// Package strategy 策略引擎核心交易流程单元测试
package strategy

import (
	"bytes"
	"context"
	"fmt"
	"log"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/order"
	"quant-desktop/internal/risk"
	"quant-desktop/internal/storage"
)

// TestFetchTickers_PartialCacheFallsBackToREST 验证行情缓存不完整时回退 REST 全量刷新
// 背景：实盘主网 !ticker@arr 大帧经代理截断/丢帧，缓存可能只剩部分币种；
// 旧逻辑只检查"非空+新鲜"，导致缺失币种永不被筛选（实盘漏单根因）。
func TestFetchTickers_PartialCacheFallsBackToREST(t *testing.T) {
	e, _ := newTestEngine(t)

	// 构造"新鲜但不完整"的缓存：只有 10 个币，lastUpdate 为刚刚
	partial := make([]binance.Ticker, 0, 10)
	for i := 0; i < 10; i++ {
		partial = append(partial, binance.Ticker{
			Symbol:      fmt.Sprintf("TEST%02dUSDT", i),
			LastPrice:   1.0,
			PriceChange: 5.0,
			QuoteVolume: 1e8,
		})
	}
	e.ws.BackfillCache(partial)
	if e.ws.CacheAge() > 30*time.Second {
		t.Fatal("测试前置失败：缓存应新鲜")
	}

	tickers, err := e.fetchTickers(context.Background())
	if err != nil {
		t.Fatalf("fetchTickers 失败: %v", err)
	}
	if len(tickers) < 100 {
		t.Fatalf("部分缓存(10个)应回退 REST 全量刷新，实际返回 %d 个", len(tickers))
	}

	// REST 回填后 WS 缓存应补齐到全量（缺币自愈）
	if ts := e.ws.GetTickers(); len(ts) < 100 {
		t.Fatalf("BackfillCache 后 WS 缓存应补齐到全量，实际 %d 个", len(ts))
	}
}

// newTestEngine 创建测试用 Engine 及关联的临时数据库
// 使用 t.TempDir() 创建临时 SQLite 数据库，DRY_RUN 模式的币安客户端与 WsManager，
// 以及默认策略配置（ScanIntervalSec=1, Timeframe="5m", MinGainPct=5.0 等）。
// 参数:
//   - t: 测试实例，用于创建临时目录和注册清理函数
//
// 返回:
//   - *Engine: 初始化好的策略引擎实例
//   - *storage.DB: 关联的数据库实例（测试结束后自动关闭）
func newTestEngine(t *testing.T) (*Engine, *storage.DB) {
	t.Helper()

	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "test.db")
	db, err := storage.NewDB(dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	client := binance.NewClient("test-key", "test-secret", "DRY_RUN", "", 0)
	ws := binance.NewWsManager("DRY_RUN")
	orderMgr := order.NewManager(client, db)

	cfg := binance.StrategyConfig{
		ScanIntervalSec:          1,
		Timeframe:                "5m",
		MinGainPct:               5.0,
		MinQuoteVolume:           100000,
		TopN:                     3,
		MaxOpenPositions:         5,
		Leverage:                 10,
		PositionMarginUSDT:       5,
		CooldownMin:              5,
		CooldownAfterTrailingMin: -1, // 测试默认统一冷却（0=立即再入，避免零值歧义）
		MaxAddOnsPerSymbol:       1,  // 测试默认 1 次追加（同币最多 2 仓，保持既有测试语义）
		MarginMode:               binance.MarginModeIsolated,
		StopLossPct:              0.10,
		TrailingActivation:       0.05,
		TrailingCallback:         0.03,
	}

	e := NewEngine(cfg, client, ws, db, orderMgr)

	t.Cleanup(func() {
		db.Close()
	})

	return e, db
}

// insertTestPosition 向数据库插入一条 OPEN 状态的测试持仓记录
// 参数:
//   - t: 测试实例
//   - db: 数据库实例
//   - symbol: 交易对（如 "BTCUSDT"）
//   - entryPrice: 入场价格
//
// 返回:
//   - int64: 新插入持仓记录的自增 ID
func insertTestPosition(t *testing.T, db *storage.DB, symbol string, entryPrice float64) int64 {
	t.Helper()

	stopPrice := entryPrice * 0.9
	pos := &storage.Position{
		Symbol:           symbol,
		Side:             "LONG",
		EntryPrice:       entryPrice,
		Amount:           0.001,
		Leverage:         10,
		HighestPrice:     &entryPrice,
		TrailingActive:   false,
		CurrentStopPrice: stopPrice,
		Status:           "OPEN",
		OpenedAt:         time.Now().UnixMilli(),
	}
	id, err := db.InsertPosition(pos)
	if err != nil {
		t.Fatalf("插入测试持仓失败: %v", err)
	}
	return id
}

// insertTestPositionActivated 插入一条移动止盈已激活（TrailingActive=true）的 OPEN 持仓，
// 用于追加仓位"状态判定"测试：价格曾到过入场价*(1+TrailingActivation) 即视为趋势确认。
func insertTestPositionActivated(t *testing.T, db *storage.DB, symbol string, entryPrice float64) int64 {
	t.Helper()

	highest := entryPrice * 1.05 // 假设价格曾冲到 +5%
	pos := &storage.Position{
		Symbol:           symbol,
		Side:             "LONG",
		EntryPrice:       entryPrice,
		Amount:           0.001,
		Leverage:         10,
		HighestPrice:     &highest,
		TrailingActive:   true,
		CurrentStopPrice: highest * 0.97, // 初始跟踪价 = 最高价 * (1 - 回调)
		Status:           "OPEN",
		OpenedAt:         time.Now().UnixMilli(),
	}
	id, err := db.InsertPosition(pos)
	if err != nil {
		t.Fatalf("插入测试持仓失败: %v", err)
	}
	return id
}

// cancelActiveOrders 将该持仓关联的全部活跃委托标记为已取消。
// 用于测试本地兜底平仓路径：真实系统中持仓有活跃止损条件单时，
// monitorPositions 会跳过本地平仓（双触发防护，交由交易所条件单 + SyncOrders 闭环），
// 条件单失效/撤销后本地兜底平仓才生效。
// 参数:
//   - t: 测试实例
//   - db: 数据库实例
func cancelActiveOrders(t *testing.T, db *storage.DB) {
	t.Helper()
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("查询活跃委托失败: %v", err)
	}
	for _, o := range orders {
		if err := db.UpdateOrderStatus(o.ID, "CANCELED", nil, nil); err != nil {
			t.Fatalf("取消委托 ID=%d 失败: %v", o.ID, err)
		}
	}
}

// ========== 一、引擎生命周期测试 ==========

// TestNewEngine 验证 Engine 创建成功
// 检查: Engine 非 nil，window 非 nil，tickCount 初始为 0
func TestNewEngine(t *testing.T) {
	e, _ := newTestEngine(t)

	if e == nil {
		t.Fatal("NewEngine 返回 nil")
	}
	if e.window == nil {
		t.Error("window 不应为 nil")
	}
	if got := e.GetTickCount(); got != 0 {
		t.Errorf("tickCount = %d, 期望 0", got)
	}
}

// TestIsRunning 验证引擎初始状态为未运行
// 检查: 新创建的 Engine 调用 IsRunning() 应返回 false
func TestIsRunning(t *testing.T) {
	e, _ := newTestEngine(t)

	if e.IsRunning() {
		t.Error("初始状态 IsRunning() 应为 false")
	}
}

// TestGetTickCount 验证初始 Tick 计数为 0
// 检查: 新创建的 Engine 调用 GetTickCount() 应返回 0
func TestGetTickCount(t *testing.T) {
	e, _ := newTestEngine(t)

	if got := e.GetTickCount(); got != 0 {
		t.Errorf("GetTickCount() = %d, 期望 0", got)
	}
}

// TestStartStop 验证引擎启动与停止的生命周期
// 流程: goroutine 中 Start(ctx) → 等 100ms → 验证 IsRunning()=true → Stop() → 等 100ms → 验证 IsRunning()=false
func TestStartStop(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	go e.Start(ctx)
	time.Sleep(100 * time.Millisecond)

	if !e.IsRunning() {
		t.Error("Start 后 IsRunning() 应为 true")
	}

	e.Stop()
	time.Sleep(100 * time.Millisecond)

	if e.IsRunning() {
		t.Error("Stop 后 IsRunning() 应为 false")
	}
}

// TestStopBeforeStart 验证 Stop 先于 Start 调度时引擎会直接退出，
// 避免"Start 晚到导致停止请求丢失、引擎永不停止"的竞态。
func TestStopBeforeStart(t *testing.T) {
	e, _ := newTestEngine(t)

	e.Stop()
	done := make(chan struct{})
	go func() {
		e.Start(context.Background())
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Stop 先于 Start 时，Start 应直接退出")
	}
	if e.IsRunning() {
		t.Error("Stop 先于 Start 后引擎不应处于运行状态")
	}
}

// ========== 二、开仓流程测试（openPositions） ==========

// TestOpenPositions_DryRun 验证 DRY_RUN 模式下的完整开仓流程
// 构造 BTCUSDT 候选（涨幅 6%，成交额 200000），入场价 50000，
// 验证: DB 中新增 1 条 OPEN 持仓（Symbol/EntryPrice/Amount/Leverage 正确），
// 以及 2 条活跃委托（止损 + 跟踪止损）
func TestOpenPositions_DryRun(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	// 验证持仓
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 1", len(positions))
	}

	pos := positions[0]
	if pos.Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %q, 期望 BTCUSDT", pos.Symbol)
	}
	if pos.EntryPrice != 50000.0 {
		t.Errorf("EntryPrice = %v, 期望 50000", pos.EntryPrice)
	}
	if pos.Amount <= 0 {
		t.Errorf("Amount = %v, 期望 > 0", pos.Amount)
	}
	if pos.Leverage != 10 {
		t.Errorf("Leverage = %d, 期望 10", pos.Leverage)
	}

	// 验证交易所止损委托已挂出（止损 + 跟踪止损）
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("查询活跃委托失败: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("活跃委托数 = %d, 期望 2（止损+跟踪止损）", len(orders))
	}
}

// TestOpenPositions_MinQuoteVolumeCheck 验证开仓前成交额校验（默认 1000 万 USDT）：
// 候选成交额不达标（900 万 < 1000 万）时决策=拒绝开仓，不产生持仓；
// 候选成交额恰好等于阈值（1000 万）时决策=允许开仓，DRY_RUN 下真实开仓。
// 覆盖用户需求「开仓前金额校验」关键路径与边界值（>= 语义）。
func TestOpenPositions_MinQuoteVolumeCheck(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.cfg.MinQuoteVolume = 10000000 // 新默认阈值 1000 万

	// 1. 不达标候选：900 万 < 1000 万 → 拒绝开仓
	e.openPositions(ctx, []Candidate{
		{Symbol: "LOWVOLUSDT", GainPct: 6.0, QuoteVolume: 9000000},
	}, map[string]float64{"LOWVOLUSDT": 100.0}, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Fatalf("不达标候选开仓数 = %d, 期望 0（900 万 < 1000 万应拒绝开仓）", len(positions))
	}

	// 2. 达标候选：恰好 1000 万整 → 允许开仓
	e.openPositions(ctx, []Candidate{
		{Symbol: "OKVOLUSDT", GainPct: 6.0, QuoteVolume: 10000000},
	}, map[string]float64{"OKVOLUSDT": 100.0}, nil)

	positions, err = db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("达标候选开仓数 = %d, 期望 1（恰好 1000 万应允许开仓）", len(positions))
	}
	if positions[0].Symbol != "OKVOLUSDT" {
		t.Errorf("Symbol = %q, 期望 OKVOLUSDT", positions[0].Symbol)
	}
}

// TestLogQuoteVolumeFilter 验证每 Tick 成交额校验日志（任务 2a/2b 可观测性要求）：
// ① 汇总行输出达标数/被过滤数/阈值；
// ② 接近阈值（>=50%）却被过滤的合约逐条打印判断过程：原始金额→校验规则→限制匹配→决策结果；
// ③ 远离阈值的低流动性币只计入汇总、不逐条打印（防刷屏）。
func TestLogQuoteVolumeFilter(t *testing.T) {
	e, _ := newTestEngine(t)
	e.cfg.MinQuoteVolume = 10000000 // 新默认阈值 1000 万

	// 捕获全局 log 输出（本包测试非并行，defer 恢复输出目标）
	var buf bytes.Buffer
	oldOut := log.Writer()
	log.SetOutput(&buf)
	defer log.SetOutput(oldOut)

	tickers := []binance.Ticker{
		{Symbol: "BIGUSDT", QuoteVolume: 150000000}, // 1.5 亿，达标
		{Symbol: "NEARUSDT", QuoteVolume: 6000000},  // 600 万 >= 50% 阈值，接近但被过滤 → 逐条打印
		{Symbol: "SMALLUSDT", QuoteVolume: 100000},  // 10 万，远离阈值 → 仅计入汇总
	}
	e.logQuoteVolumeFilter(tickers)

	out := buf.String()

	// ① 汇总行
	if !strings.Contains(out, "最小成交额校验") {
		t.Errorf("缺少汇总行，实际输出:\n%s", out)
	}
	if !strings.Contains(out, "达标=1") || !strings.Contains(out, "被过滤=2") || !strings.Contains(out, ">=10000000USDT") {
		t.Errorf("汇总行统计或阈值错误，实际输出:\n%s", out)
	}

	// ② 接近阈值被过滤合约的逐条判断过程
	if !strings.Contains(out, "成交额过滤 NEARUSDT") {
		t.Errorf("缺少 NEARUSDT 逐条明细行，实际输出:\n%s", out)
	}
	if !strings.Contains(out, "原始金额=6000000USDT") ||
		!strings.Contains(out, "校验规则=24h成交额>=10000000USDT") ||
		!strings.Contains(out, "限制匹配=不满足") ||
		!strings.Contains(out, "决策=剔除") {
		t.Errorf("明细行缺少 原始金额/校验规则/限制匹配/决策 字段，实际输出:\n%s", out)
	}

	// ③ 远离阈值的不逐条打印
	if strings.Contains(out, "成交额过滤 SMALLUSDT") {
		t.Errorf("远离阈值的 SMALLUSDT 不应逐条打印，实际输出:\n%s", out)
	}
}

// TestOpenPositions_MaxLimit 验证持仓上限检查
// 设置 MaxOpenPositions=1，预先插入 1 条 OPEN 持仓，
// 再调用 openPositions 传入新候选，验证不会新开仓（仍然只有 1 条）
func TestOpenPositions_MaxLimit(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.MaxOpenPositions = 1

	// 预先插入 1 条 OPEN 持仓，占满名额
	insertTestPosition(t, db, "ETHUSDT", 3000.0)

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	existing, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	e.openPositions(ctx, candidates, priceMap, existing)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Errorf("OPEN 持仓数 = %d, 期望 1（已达上限，不应新开仓）", len(positions))
	}
}

// TestOpenPositions_DuplicateSymbol 验证已持有币种不重复开仓
// 预先插入 BTCUSDT 的 OPEN 持仓，再用 BTCUSDT 候选调用 openPositions，
// 验证不会产生第二条 BTCUSDT 持仓
func TestOpenPositions_DuplicateSymbol(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 预先插入 BTCUSDT 持仓
	insertTestPosition(t, db, "BTCUSDT", 50000.0)

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	existing, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	e.openPositions(ctx, candidates, priceMap, existing)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Errorf("OPEN 持仓数 = %d, 期望 1（不应重复开仓）", len(positions))
	}
}

// TestOpenPositions_EmptyCandidates 验证空候选列表不报错且不产生新持仓
// 传入空的 candidates 和 priceMap，验证 openPositions 正常返回，DB 无新持仓
func TestOpenPositions_EmptyCandidates(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.openPositions(ctx, []Candidate{}, map[string]float64{}, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("OPEN 持仓数 = %d, 期望 0（空候选不应开仓）", len(positions))
	}
}

// ========== 四、runOnce 集成测试 ==========

// TestRunOnce_DryRun 验证 DRY_RUN 模式下 runOnce 的完整执行
// DRY_RUN 模式下 FetchTickers 返回 nil，runOnce 在获取行情后构建空 priceMap，
// 后续筛选、开仓、监控均无实际操作。验证 tickCount 增加到 1 且不 panic
func TestRunOnce_DryRun(t *testing.T) {
	e, _ := newTestEngine(t)
	ctx := context.Background()

	e.runOnce(ctx)

	if got := e.GetTickCount(); got != 1 {
		t.Errorf("GetTickCount() = %d, 期望 1", got)
	}
}

// ========== 五、预热期日志测试 ==========

// TestLogWindowStatus 验证预热期窗口状态摘要日志不 panic
// 向 window 喂入若干价格采样点，构造 tickers 和 priceMap，
// 调用 logWindowStatus 验证正常执行不 panic
func TestLogWindowStatus(t *testing.T) {
	e, _ := newTestEngine(t)

	now := time.Now().UnixMilli()

	// 向窗口喂入几个价格点（模拟预热期）
	e.window.Add("BTCUSDT", now-60000, 49000)
	e.window.Add("BTCUSDT", now-30000, 49500)
	e.window.Add("BTCUSDT", now, 50000)
	e.window.Add("ETHUSDT", now-60000, 2900)
	e.window.Add("ETHUSDT", now, 3000)

	tickers := []binance.Ticker{
		{Symbol: "BTCUSDT", LastPrice: 50000, QuoteVolume: 500000},
		{Symbol: "ETHUSDT", LastPrice: 3000, QuoteVolume: 300000},
	}
	priceMap := map[string]float64{
		"BTCUSDT": 50000,
		"ETHUSDT": 3000,
	}

	// 验证不 panic
	e.logWindowStatus(tickers, priceMap, now)
}

// TestLogCandidates 验证候选币种明细日志不 panic
// 构造若干候选币种，调用 logCandidates 验证正常执行不 panic
func TestLogCandidates(t *testing.T) {
	e, _ := newTestEngine(t)

	now := time.Now().UnixMilli()

	// 向窗口喂入基准点，使 WindowLengthMs 有返回值
	e.window.Add("BTCUSDT", now-300000, 47000)
	e.window.Add("ETHUSDT", now-300000, 2800)

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.38, QuoteVolume: 500000},
		{Symbol: "ETHUSDT", GainPct: 7.14, QuoteVolume: 300000},
	}

	// 验证不 panic
	e.logCandidates(candidates, now)
}

// ========== 六、持仓监控测试（monitorPositions / closePosition） ==========

// insertMonitorPosition 向数据库插入一条自定义风控状态的 OPEN 持仓记录
// 参数:
//   - t: 测试实例
//   - db: 数据库实例
//   - symbol: 交易对
//   - entryPrice: 入场价格
//   - amount: 持仓数量
//   - trailingActive: 跟踪止盈是否已激活
//   - highestPrice: 历史最高价（nil 时默认等于 entryPrice）
//   - stopPrice: 当前止损价
//
// 返回:
//   - int64: 新插入持仓记录的自增 ID
func insertMonitorPosition(t *testing.T, db *storage.DB, symbol string, entryPrice, amount float64, trailingActive bool, highestPrice *float64, stopPrice float64) int64 {
	t.Helper()

	if highestPrice == nil {
		highestPrice = &entryPrice
	}
	pos := &storage.Position{
		Symbol:           symbol,
		Side:             "LONG",
		EntryPrice:       entryPrice,
		Amount:           amount,
		Leverage:         10,
		HighestPrice:     highestPrice,
		TrailingActive:   trailingActive,
		CurrentStopPrice: stopPrice,
		Status:           "OPEN",
		OpenedAt:         time.Now().UnixMilli(),
	}
	id, err := db.InsertPosition(pos)
	if err != nil {
		t.Fatalf("插入测试持仓失败: %v", err)
	}
	return id
}

// TestMonitorPositions_StopLoss 验证止损触发逻辑
// 插入 OPEN 持仓 EntryPrice=100, Amount=1.0，喂入价格 89（低于止损价 100*(1-0.10)=90），
// 调用 monitorPositions 后验证持仓状态变为 CLOSED，平仓原因为 STOP_LOSS
func TestMonitorPositions_StopLoss(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 默认配置 StopLossPct=0.10，止损价 = 100 * 0.90 = 90
	posID := insertMonitorPosition(t, db, "BTCUSDT", 100.0, 1.0, false, nil, 90.0)

	// 喂入价格 89，低于止损价 90
	priceMap := map[string]float64{"BTCUSDT": 89.0}
	e.monitorPositions(ctx, priceMap)

	// 验证持仓已平仓
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	if pos.Status != "CLOSED" {
		t.Errorf("Status = %q, 期望 CLOSED", pos.Status)
	}
	if pos.CloseReason == nil || *pos.CloseReason != "STOP_LOSS" {
		t.Errorf("CloseReason = %v, 期望 STOP_LOSS", pos.CloseReason)
	}
	// 验证盈亏: (89 - 100) * 1.0 = -11
	if pos.RealizedPnl == nil || *pos.RealizedPnl != -11.0 {
		t.Errorf("RealizedPnl = %v, 期望 -11", pos.RealizedPnl)
	}
}

// TestMonitorPositions_TrailingActivation 验证跟踪止盈激活逻辑
// 插入 OPEN 持仓 EntryPrice=100（TrailingActivation=0.10，激活价=110），
// 喂入价格 111（>=110），调用 monitorPositions 后验证持仓仍为 OPEN 但 TrailingActive=true
func TestMonitorPositions_TrailingActivation(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 调整配置: TrailingActivation=0.10 → 激活价 = 100 * 1.10 = 110
	e.cfg.TrailingActivation = 0.10
	e.cfg.TrailingCallback = 0.05

	posID := insertMonitorPosition(t, db, "ETHUSDT", 100.0, 1.0, false, nil, 90.0)

	// 喂入价格 111，高于激活价 110
	priceMap := map[string]float64{"ETHUSDT": 111.0}
	e.monitorPositions(ctx, priceMap)

	// 验证持仓仍为 OPEN
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	if pos.Status != "OPEN" {
		t.Errorf("Status = %q, 期望 OPEN（激活不应平仓）", pos.Status)
	}
	if !pos.TrailingActive {
		t.Error("TrailingActive = false, 期望 true（跟踪止盈应已激活）")
	}
	// 验证最高价更新为 111
	if pos.HighestPrice == nil || *pos.HighestPrice != 111.0 {
		t.Errorf("HighestPrice = %v, 期望 111", pos.HighestPrice)
	}
	// 验证跟踪止损价 ≈ 111 * (1 - 0.05) = 105.45（浮点精度容差）
	expectedStop := 111.0 * (1 - 0.05)
	if !almostEqual(pos.CurrentStopPrice, expectedStop) {
		t.Errorf("CurrentStopPrice = %v, 期望 ≈%v", pos.CurrentStopPrice, expectedStop)
	}
}

// TestMonitorPositions_TrailingTrigger 验证跟踪止盈触发平仓逻辑
// 插入 OPEN 持仓 EntryPrice=100, TrailingActive=true, HighestPrice=120（TrailingCallback=0.05，
// 跟踪止损价=120*0.95=114），喂入价格 113（<=114），验证持仓 CLOSED，原因 TRAILING_STOP
func TestMonitorPositions_TrailingTrigger(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 调整配置: TrailingCallback=0.05
	e.cfg.TrailingCallback = 0.05

	hp := 120.0
	stopPrice := hp * (1 - e.cfg.TrailingCallback) // 120 * 0.95 = 114
	posID := insertMonitorPosition(t, db, "SOLUSDT", 100.0, 1.0, true, &hp, stopPrice)

	// 喂入价格 113，低于跟踪止损价 114
	priceMap := map[string]float64{"SOLUSDT": 113.0}
	e.monitorPositions(ctx, priceMap)

	// 验证持仓已平仓
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	if pos.Status != "CLOSED" {
		t.Errorf("Status = %q, 期望 CLOSED", pos.Status)
	}
	if pos.CloseReason == nil || *pos.CloseReason != "TRAILING_STOP" {
		t.Errorf("CloseReason = %v, 期望 TRAILING_STOP", pos.CloseReason)
	}
	// 验证盈亏: (113 - 100) * 1.0 = 13
	if pos.RealizedPnl == nil || *pos.RealizedPnl != 13.0 {
		t.Errorf("RealizedPnl = %v, 期望 13", pos.RealizedPnl)
	}
}

// TestMonitorPositions_NoAction 验证价格未触发任何平仓条件时持仓保持不变
// 插入 OPEN 持仓 EntryPrice=100（StopLossPct=0.10, TrailingActivation=0.10），
// 喂入价格 105（高于止损价 90，低于激活价 110），验证持仓仍为 OPEN 且状态未变
func TestMonitorPositions_NoAction(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 调整配置: TrailingActivation=0.10 → 激活价 = 110，价格 105 不触发
	e.cfg.TrailingActivation = 0.10

	posID := insertMonitorPosition(t, db, "XRPUSDT", 100.0, 1.0, false, nil, 90.0)

	// 喂入价格 105: 高于止损价 90，低于激活价 110
	priceMap := map[string]float64{"XRPUSDT": 105.0}
	e.monitorPositions(ctx, priceMap)

	// 验证持仓仍为 OPEN 且未激活跟踪止盈
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	if pos.Status != "OPEN" {
		t.Errorf("Status = %q, 期望 OPEN", pos.Status)
	}
	if pos.TrailingActive {
		t.Error("TrailingActive = true, 期望 false（不应激活）")
	}
}

// ========== 七、开仓冷却期与 TopN 过滤测试 ==========

// TestOpenPositions_CooldownConfig 验证冷却期内跳过开仓
// 设置 CooldownMin=60，将 BTCUSDT 加入冷却映射（时间为当前时刻），
// 调用 openPositions 传入 BTCUSDT 候选，验证不会开仓
func TestOpenPositions_CooldownConfig(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 设置冷却期 60 分钟，并记录 BTCUSDT 刚平仓
	e.cfg.CooldownMin = 60
	e.cooldown["BTCUSDT"] = time.Now()

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	// 验证未开仓
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("OPEN 持仓数 = %d, 期望 0（冷却期内不应开仓）", len(positions))
	}
}

// TestCooldownSetViaManagerCloseCallback 验证冷却期闭环修复：
// NewEngine 向委托管理器注册 OnClose 回调，条件单平仓后引擎写入冷却期，
// 同币在冷却期内不再开仓（此前主平仓路径从不通知引擎，导致无限快速追单）。
func TestCooldownSetViaManagerCloseCallback(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	if e.orderMgr == nil || e.orderMgr.OnClose == nil {
		t.Fatal("NewEngine 应注册委托管理器平仓回调（OnClose）")
	}

	// 模拟条件单触发平仓完成：回调被调用 → 引擎写入冷却期（含平仓原因）
	e.orderMgr.OnClose("BTCUSDT", "TRAILING_STOP")
	if _, ok := e.cooldown["BTCUSDT"]; !ok {
		t.Fatal("OnClose 回调后引擎应写入冷却期记录")
	}
	if e.cooldownReason["BTCUSDT"] != "TRAILING_STOP" {
		t.Fatalf("冷却原因 = %q, 期望 TRAILING_STOP", e.cooldownReason["BTCUSDT"])
	}

	// 冷却期内同币不再开仓（CooldownMin=5 分钟，刚平仓必在期内）
	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}
	e.openPositions(ctx, candidates, priceMap, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("OPEN 持仓数 = %d, 期望 0（条件单平仓后冷却期应生效）", len(positions))
	}
}

// TestOpenPositions_CooldownByReason 验证分原因冷却（2026-08-08 回测验证）:
// 移动止盈平仓后按 CooldownAfterTrailingMin 短冷却；止损/其他平仓保持 CooldownMin 完整冷却。
func TestOpenPositions_CooldownByReason(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.cfg.CooldownMin = 60
	e.cfg.CooldownAfterTrailingMin = 15

	btc := []Candidate{{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000}}
	eth := []Candidate{{Symbol: "ETHUSDT", GainPct: 6.0, QuoteVolume: 200000}}
	priceMap := map[string]float64{"BTCUSDT": 50000.0, "ETHUSDT": 3000.0}

	// 场景1: 移动止盈平仓后 5 分钟（<15 分钟冷却）→ 拦截
	e.cooldown["BTCUSDT"] = time.Now()
	e.cooldownReason["BTCUSDT"] = "TRAILING_STOP"
	e.openPositions(ctx, btc, priceMap, nil)
	if ps, _ := db.GetOpenPositions(); len(ps) != 0 {
		t.Fatalf("TRAILING_STOP 后 5 分钟应拦截（冷却 15 分钟）")
	}

	// 场景2: 移动止盈平仓后 16 分钟（已过 15 分钟冷却）→ 放行开仓
	e.cooldown["BTCUSDT"] = time.Now().Add(-16 * time.Minute)
	e.openPositions(ctx, btc, priceMap, nil)
	ps, _ := db.GetOpenPositions()
	if len(ps) != 1 || ps[0].Symbol != "BTCUSDT" {
		t.Fatalf("TRAILING_STOP 后 16 分钟应放行 BTCUSDT, 实际 %+v", ps)
	}

	// 场景3: 止损平仓后 16 分钟（<60 分钟完整冷却）→ 仍拦截
	e.cooldown["ETHUSDT"] = time.Now().Add(-16 * time.Minute)
	e.cooldownReason["ETHUSDT"] = "STOP_LOSS"
	e.openPositions(ctx, eth, priceMap, nil)
	ps2, _ := db.GetOpenPositions()
	for _, p := range ps2 {
		if p.Symbol == "ETHUSDT" {
			t.Fatalf("STOP_LOSS 后 16 分钟不应开 ETHUSDT（完整 60 分钟冷却）")
		}
	}
}

// TestOpenPositions_TopNFiltering 验证 TopN 限制开仓数量
// 设置 TopN=1（通过 MaxOpenPositions=1 限制可用槽位），喂入 3 个候选，
// 验证最终只开 1 个仓位
func TestOpenPositions_TopNFiltering(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 限制最大持仓为 1，使得只有 1 个槽位可用
	e.cfg.MaxOpenPositions = 1

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 8.0, QuoteVolume: 500000},
		{Symbol: "ETHUSDT", GainPct: 7.0, QuoteVolume: 400000},
		{Symbol: "SOLUSDT", GainPct: 6.0, QuoteVolume: 300000},
	}
	priceMap := map[string]float64{
		"BTCUSDT": 50000.0,
		"ETHUSDT": 3000.0,
		"SOLUSDT": 150.0,
	}

	e.openPositions(ctx, candidates, priceMap, nil)

	// 验证只开了 1 个仓位
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Errorf("OPEN 持仓数 = %d, 期望 1（TopN 限制）", len(positions))
	}
	// 验证开的是第一个候选（涨幅最高的 BTCUSDT）
	if len(positions) == 1 && positions[0].Symbol != "BTCUSDT" {
		t.Errorf("开仓 Symbol = %q, 期望 BTCUSDT（涨幅最高优先）", positions[0].Symbol)
	}
}

// TestOpenPositions_BlockedSymbol 验证结构性失败拉黑的币种不再尝试开仓
// 预先将 BTCUSDT 加入 openBlocked（未来 1 小时），openPositions 传入 BTCUSDT 候选，
// 验证不会开仓（DB 无新持仓），杜绝 -2027/-4028 等结构性错误周期性重试刷屏
func TestOpenPositions_BlockedSymbol(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 模拟结构性失败（如 -2027 仓位超限）后拉黑该币种 12 小时
	e.openBlocked["BTCUSDT"] = time.Now().Add(time.Hour)

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("OPEN 持仓数 = %d, 期望 0（拉黑币种不应开仓）", len(positions))
	}
}

// TestOpenPositions_BlockedExpired 验证拉黑到期后币种恢复可开仓
// BTCUSDT 拉黑时间已过（过去时间），openPositions 传入候选应正常开仓
func TestOpenPositions_BlockedExpired(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 拉黑已到期（截止时间为 1 小时前）
	e.openBlocked["BTCUSDT"] = time.Now().Add(-time.Hour)

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Errorf("OPEN 持仓数 = %d, 期望 1（拉黑到期应恢复开仓）", len(positions))
	}
}

// ========== 八、ScreenSliding TopN 边界测试 ==========

// TestScreenSliding_TopNZero 验证 topN=0 表示不限制，返回所有达标候选
// 喂入 3 个达标候选，topN=0，验证全部返回
func TestScreenSliding_TopNZero(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "AUSDT", now, 100)
	feedBaseline(w, "BUSDT", now, 100)
	feedBaseline(w, "CUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "AUSDT", LastPrice: 106, QuoteVolume: 200000},
		{Symbol: "BUSDT", LastPrice: 107, QuoteVolume: 300000},
		{Symbol: "CUSDT", LastPrice: 108, QuoteVolume: 400000},
	}
	priceMap := map[string]float64{"AUSDT": 106, "BUSDT": 107, "CUSDT": 108}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0, nil)
	if len(got) != 3 {
		t.Fatalf("topN=0 时 len = %d, 期望 3（不限制应返回全部）", len(got))
	}
}

// TestScreenSliding_TopNLargerThanCandidates 验证 topN 大于候选数时返回全部候选
// 仅有 3 个达标候选，topN=10，验证返回 3 个（不会报错或填充）
func TestScreenSliding_TopNLargerThanCandidates(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "AUSDT", now, 100)
	feedBaseline(w, "BUSDT", now, 100)
	feedBaseline(w, "CUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "AUSDT", LastPrice: 106, QuoteVolume: 200000},
		{Symbol: "BUSDT", LastPrice: 107, QuoteVolume: 300000},
		{Symbol: "CUSDT", LastPrice: 108, QuoteVolume: 400000},
	}
	priceMap := map[string]float64{"AUSDT": 106, "BUSDT": 107, "CUSDT": 108}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 10, now, false, 0, 0, 0, "sliding", nil, 0, nil)
	if len(got) != 3 {
		t.Fatalf("topN=10 但仅 3 个候选时 len = %d, 期望 3", len(got))
	}
}

// ========== 九、持仓监控边界条件测试 ==========

// TestMonitorPositions_Boundary_ExactStopLoss 验证价格恰好等于止损价时触发止损
// EntryPrice=100, StopLossPct=0.10 → 止损价=90.0，喂入价格恰好 90.0
// 条件为 price <= stopLossPrice，因此等于时应触发平仓
func TestMonitorPositions_Boundary_ExactStopLoss(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 默认配置 StopLossPct=0.10，止损价 = 100 * (1 - 0.10) = 90.0
	posID := insertMonitorPosition(t, db, "BTCUSDT", 100.0, 1.0, false, nil, 90.0)

	// 喂入价格恰好等于止损价 90.0
	priceMap := map[string]float64{"BTCUSDT": 90.0}
	e.monitorPositions(ctx, priceMap)

	// 验证持仓已平仓（price <= stopLossPrice 触发）
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	if pos.Status != "CLOSED" {
		t.Errorf("Status = %q, 期望 CLOSED（价格恰好等于止损价应触发）", pos.Status)
	}
	if pos.CloseReason == nil || *pos.CloseReason != "STOP_LOSS" {
		t.Errorf("CloseReason = %v, 期望 STOP_LOSS", pos.CloseReason)
	}
	// 验证盈亏: (90 - 100) * 1.0 = -10
	if pos.RealizedPnl == nil || *pos.RealizedPnl != -10.0 {
		t.Errorf("RealizedPnl = %v, 期望 -10", pos.RealizedPnl)
	}
}

// TestMonitorPositions_Boundary_ExactTrailingActivation 验证价格恰好等于激活价时触发跟踪止盈激活
// EntryPrice=100, TrailingActivation=0.25 → 激活价=125.0，喂入价格恰好 125.0
// 条件为 price >= activationPrice，因此等于时应激活
// 注: 使用 0.25（二进制精确表示）避免 0.10 的浮点精度问题导致 activationPrice 微偏
func TestMonitorPositions_Boundary_ExactTrailingActivation(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 调整配置: TrailingActivation=0.25 → 激活价 = 100 * (1 + 0.25) = 125.0（精确）
	e.cfg.TrailingActivation = 0.25
	e.cfg.TrailingCallback = 0.05

	posID := insertMonitorPosition(t, db, "ETHUSDT", 100.0, 1.0, false, nil, 90.0)

	// 喂入价格恰好等于激活价 125.0
	priceMap := map[string]float64{"ETHUSDT": 125.0}
	e.monitorPositions(ctx, priceMap)

	// 验证持仓仍为 OPEN 但跟踪止盈已激活
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	if pos.Status != "OPEN" {
		t.Errorf("Status = %q, 期望 OPEN（激活不应平仓）", pos.Status)
	}
	if !pos.TrailingActive {
		t.Error("TrailingActive = false, 期望 true（价格恰好等于激活价应激活）")
	}
	// 验证最高价更新为 125
	if pos.HighestPrice == nil || *pos.HighestPrice != 125.0 {
		t.Errorf("HighestPrice = %v, 期望 125", pos.HighestPrice)
	}
	// 验证跟踪止损价 = 125 * (1 - 0.05) = 118.75
	expectedStop := 125.0 * (1 - 0.05)
	if !almostEqual(pos.CurrentStopPrice, expectedStop) {
		t.Errorf("CurrentStopPrice = %v, 期望 ≈%v", pos.CurrentStopPrice, expectedStop)
	}
}

// TestMonitorPositions_Boundary_ZeroAmount 验证持仓数量为 0 时不崩溃
// Amount=0 的持仓在监控时应正常处理（DRY_RUN 模式下平仓不会因数量为 0 而 panic）
func TestMonitorPositions_Boundary_ZeroAmount(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 插入 Amount=0 的持仓，价格低于止损价以触发平仓路径
	posID := insertMonitorPosition(t, db, "ZEROUSDT", 100.0, 0.0, false, nil, 90.0)

	// 喂入价格 89，低于止损价 90，触发平仓
	priceMap := map[string]float64{"ZEROUSDT": 89.0}

	// 验证不 panic
	e.monitorPositions(ctx, priceMap)

	// 验证持仓已被处理（平仓或保持，关键是不崩溃）
	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos == nil {
		t.Fatal("持仓记录不存在")
	}
	// DRY_RUN 模式下平仓成功，状态应为 CLOSED
	if pos.Status != "CLOSED" {
		t.Errorf("Status = %q, 期望 CLOSED（Amount=0 不应阻止平仓流程）", pos.Status)
	}
}

// ========== 十、开仓边界条件测试 ==========

// TestOpenPositions_Boundary_MaxPositionsZero 验证 MaxOpenPositions=0 时不开任何仓位
// slots = MaxOpenPositions - len(openPositions) = 0 - 0 = 0，应直接返回 nil
func TestOpenPositions_Boundary_MaxPositionsZero(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 设置最大持仓为 0
	e.cfg.MaxOpenPositions = 0

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
		{Symbol: "ETHUSDT", GainPct: 7.0, QuoteVolume: 300000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0, "ETHUSDT": 3000.0}

	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 0 {
		t.Errorf("MaxOpenPositions=0 时 opened len = %d, 期望 0", len(opened))
	}

	// 验证数据库无新持仓
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("OPEN 持仓数 = %d, 期望 0（MaxOpenPositions=0 不应开仓）", len(positions))
	}
}

// ========== 十.一、追加仓位测试（2026-08-04 新增） ==========
// 追加规则：开启 EnableAddOn 时，持仓币再次命中信号（候选已含该币）且移动止盈已激活
// （同币任一持仓 TrailingActive=true，即价格曾到过入场价*(1+TrailingActivation)），
// 允许追加 1 张独立新单（单币最多 2 仓）。用状态而非现价判定（2026-08-04 讨论确认）。

// TestOpenPositions_AddOn_Activated 验证：移动止盈已激活（状态）+ 再次命中信号 → 追加新单，
// 且现价即使已回落到激活价下方仍追加（状态判定的关键行为：冲高回落后追加）
func TestOpenPositions_AddOn_Activated(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	e.cfg.MaxOpenPositions = 10
	insertTestPositionActivated(t, db, "BTCUSDT", 100.0) // 首仓入场 100，价格曾到过 +5% → 移动止盈已激活

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(held) != 1 {
		t.Fatalf("预置持仓数 = %d, 期望 1", len(held))
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	// 现价 103 < 激活价 105：状态已激活，即使现价回落也允许追加（状态判定而非现价判定）
	priceMap := map[string]float64{"BTCUSDT": 103.0}

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 1 {
		t.Fatalf("追加开仓数 = %d, 期望 1", len(opened))
	}
	if opened[0].Symbol != "BTCUSDT" {
		t.Errorf("追加仓 Symbol = %q, 期望 BTCUSDT", opened[0].Symbol)
	}

	// 验证数据库中同币持仓数 = 2（首仓 + 追加仓）
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	count := 0
	for _, p := range positions {
		if p.Symbol == "BTCUSDT" {
			count++
		}
	}
	if count != 2 {
		t.Errorf("BTCUSDT OPEN 持仓数 = %d, 期望 2（首仓 + 1 次追加）", count)
	}
}

// TestOpenPositions_AddOn_NotActivated 验证：移动止盈未激活（TrailingActive=false，价格未到过 +激活%）→ 不加仓
func TestOpenPositions_AddOn_NotActivated(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	insertTestPosition(t, db, "BTCUSDT", 100.0) // 激活价 105

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{"BTCUSDT": 103.0} // 103 < 105（未激活）

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("未激活时不应追加开仓, opened len = %d", len(opened))
	}
}

// TestOpenPositions_AddOn_LimitReached 验证：同币持仓数已达 1+maxAddOnsPerSymbol（2 仓）→ 不再追加
func TestOpenPositions_AddOn_LimitReached(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	e.cfg.MaxOpenPositions = 10
	insertTestPosition(t, db, "BTCUSDT", 100.0)
	insertTestPosition(t, db, "BTCUSDT", 104.0) // 已有 1 次追加 → 同币 2 仓

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("预置持仓数 = %d, 期望 2", len(held))
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{"BTCUSDT": 110.0} // 已激活

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("已达追加上限时不应再开仓, opened len = %d", len(opened))
	}
}

// TestOpenPositions_AddOn_Disabled 验证：EnableAddOn=false（默认）时持仓币一律不重复开仓（原行为）
func TestOpenPositions_AddOn_Disabled(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = false
	insertTestPosition(t, db, "BTCUSDT", 100.0)

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{"BTCUSDT": 106.0} // 已激活但开关关闭

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("关闭追加仓位时不应开仓, opened len = %d", len(opened))
	}
}

// TestOpenPositions_AddOn_CooldownFirst 验证：移动止盈已激活 + 再次命中信号，
// 但该币处于开仓失败冷却期（failedOpen）→ 冷却拦截优先于追加判定，不开仓
func TestOpenPositions_AddOn_CooldownFirst(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	e.cfg.MaxOpenPositions = 10
	insertTestPositionActivated(t, db, "BTCUSDT", 100.0)
	e.failedOpen["BTCUSDT"] = time.Now() // 该币 5 分钟冷却中

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{"BTCUSDT": 103.0} // 状态已激活（与现价无关）

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("冷却期内不应追加开仓, opened len = %d", len(opened))
	}
}

// TestOpenPositions_AddOn_BlockedFirst 验证：移动止盈已激活 + 再次命中信号，
// 但该币被结构性失败拉黑（openBlocked）→ 拉黑拦截优先于追加判定，不开仓
func TestOpenPositions_AddOn_BlockedFirst(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	e.cfg.MaxOpenPositions = 10
	insertTestPositionActivated(t, db, "BTCUSDT", 100.0)
	e.openBlocked["BTCUSDT"] = time.Now().Add(time.Hour) // 拉黑 1 小时

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{"BTCUSDT": 103.0}

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("拉黑期间不应追加开仓, opened len = %d", len(opened))
	}
}

// TestOpenPositions_AddOn_FullSlots 验证：移动止盈已激活 + 再次命中信号，
// 但总持仓已达 MaxOpenPositions（满仓）→ 整个 Tick 跳过，追加仓同样被忽略
func TestOpenPositions_AddOn_FullSlots(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	e.cfg.MaxOpenPositions = 2
	insertTestPositionActivated(t, db, "BTCUSDT", 100.0) // 已激活持仓
	insertTestPosition(t, db, "XRPUSDT", 50.0)           // 普通持仓占满剩余 1 个名额

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(held) != 2 {
		t.Fatalf("预置持仓数 = %d, 期望 2", len(held))
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{"BTCUSDT": 103.0}

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("满仓时不应追加开仓, opened len = %d", len(opened))
	}
}

// TestOpenPositions_AddOn_MissingPrice 验证：移动止盈已激活 + 再次命中信号，
// 但本 Tick 价格缺失（priceMap 无该币）→ 跳过（价格检查优先于追加判定）
func TestOpenPositions_AddOn_MissingPrice(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableAddOn = true
	e.cfg.MaxOpenPositions = 10
	insertTestPositionActivated(t, db, "BTCUSDT", 100.0)

	held, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000, Side: "LONG"},
	}
	priceMap := map[string]float64{} // 本 Tick 无 BTCUSDT 价格

	opened := e.openPositions(ctx, candidates, priceMap, held)
	if len(opened) != 0 {
		t.Fatalf("价格缺失时不应追加开仓, opened len = %d", len(opened))
	}
}

// ========== 十一、DRY_RUN 端到端集成测试 ==========

// TestE2E_DryRun_TrailingStopCycle 验证 DRY_RUN 模式下完整交易闭环（跟踪止盈路径）
// 流程: 滑动窗口喂价 → ScreenSliding 筛选 → openPositions 开仓 →
// monitorPositions 跟踪止盈激活 → monitorPositions 跟踪止盈平仓
// 验证: 候选筛选正确、持仓入库正确、止损委托挂出、跟踪止盈激活与触发、盈亏计算
func TestE2E_DryRun_TrailingStopCycle(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// --- 阶段 1: 喂价 + 筛选 ---
	// 基准价 47（5 分钟前），现价 50 → 涨幅 6.38% >= 阈值 5%
	feedBaseline(e.window, "BTCUSDT", now, 47.0)
	e.window.Add("BTCUSDT", now, 50.0)

	tickers := []binance.Ticker{
		{Symbol: "BTCUSDT", LastPrice: 50.0, QuoteVolume: 500000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50.0}

	candidates := ScreenSliding(e.window, tickers, priceMap,
		e.cfg.MinGainPct, e.cfg.Min24hGainPct, e.cfg.MinQuoteVolume, e.cfg.TopN, now,
		false, 0, 0, 0, "sliding", nil, 0, nil)

	if len(candidates) != 1 {
		t.Fatalf("筛选候选数 = %d, 期望 1", len(candidates))
	}
	if candidates[0].Symbol != "BTCUSDT" {
		t.Fatalf("候选 Symbol = %q, 期望 BTCUSDT", candidates[0].Symbol)
	}
	if candidates[0].Side != "LONG" {
		t.Errorf("候选 Side = %q, 期望 LONG", candidates[0].Side)
	}

	// --- 阶段 2: 开仓 ---
	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 1 {
		t.Fatalf("开仓数 = %d, 期望 1", len(opened))
	}

	// 验证 DB 持仓状态
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 1", len(positions))
	}
	pos := positions[0]
	if pos.EntryPrice != 50.0 {
		t.Errorf("EntryPrice = %v, 期望 50", pos.EntryPrice)
	}
	// Amount = RoundQty(5*10/50) = RoundQty(1.0) = 1.0
	if pos.Amount != 1.0 {
		t.Errorf("Amount = %v, 期望 1.0", pos.Amount)
	}
	if pos.Leverage != 10 {
		t.Errorf("Leverage = %d, 期望 10", pos.Leverage)
	}
	if pos.Status != "OPEN" {
		t.Errorf("Status = %q, 期望 OPEN", pos.Status)
	}
	// 初始止损价 = 50 * (1 - 0.10) = 45
	if !almostEqual(pos.CurrentStopPrice, 45.0) {
		t.Errorf("CurrentStopPrice = %v, 期望 45", pos.CurrentStopPrice)
	}
	if pos.TrailingActive {
		t.Error("TrailingActive = true, 期望 false（开仓时不应激活）")
	}

	// 验证止损委托已挂出（止损 + 跟踪止损）
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("查询活跃委托失败: %v", err)
	}
	if len(orders) != 2 {
		t.Fatalf("活跃委托数 = %d, 期望 2（止损+跟踪止损）", len(orders))
	}

	// --- 阶段 3: 跟踪止盈激活 ---
	// 取消活跃条件单（模拟条件单失效），使本地 monitorPositions 兜底平仓生效；
	// 真实系统中持仓有活跃条件单时本地会跳过（双触发防护，见 TestMonitor_SkipWhenActiveStopOrders）
	cancelActiveOrders(t, db)

	// 价格涨到 55 >= 50*(1+0.05)=52.5 → 激活跟踪止盈
	priceMap2 := map[string]float64{"BTCUSDT": 55.0}
	e.monitorPositions(ctx, priceMap2)

	posAfterActivate, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if posAfterActivate.Status != "OPEN" {
		t.Fatalf("Status = %q, 期望 OPEN（激活不应平仓）", posAfterActivate.Status)
	}
	if !posAfterActivate.TrailingActive {
		t.Fatal("TrailingActive = false, 期望 true（跟踪止盈应已激活）")
	}
	if posAfterActivate.HighestPrice == nil || *posAfterActivate.HighestPrice != 55.0 {
		t.Errorf("HighestPrice = %v, 期望 55", posAfterActivate.HighestPrice)
	}
	// 跟踪止损价 = 55 * (1 - 0.03) = 53.35
	expectedStop := 55.0 * (1 - e.cfg.TrailingCallback)
	if !almostEqual(posAfterActivate.CurrentStopPrice, expectedStop) {
		t.Errorf("CurrentStopPrice = %v, 期望 ≈%v", posAfterActivate.CurrentStopPrice, expectedStop)
	}

	// --- 阶段 4: 跟踪止盈触发平仓 ---
	// 价格回撤到 53 <= 53.35 → 触发跟踪止盈平仓
	priceMap3 := map[string]float64{"BTCUSDT": 53.0}
	e.monitorPositions(ctx, priceMap3)

	posClosed, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if posClosed.Status != "CLOSED" {
		t.Fatalf("Status = %q, 期望 CLOSED", posClosed.Status)
	}
	if posClosed.CloseReason == nil || *posClosed.CloseReason != "TRAILING_STOP" {
		t.Errorf("CloseReason = %v, 期望 TRAILING_STOP", posClosed.CloseReason)
	}
	// 盈亏 = (53 - 50) * 1.0 = 3.0
	if posClosed.RealizedPnl == nil || !almostEqual(*posClosed.RealizedPnl, 3.0) {
		t.Errorf("RealizedPnl = %v, 期望 3.0", posClosed.RealizedPnl)
	}
	if posClosed.ExitPrice == nil || *posClosed.ExitPrice != 53.0 {
		t.Errorf("ExitPrice = %v, 期望 53", posClosed.ExitPrice)
	}

	// 验证平仓后无 OPEN 持仓
	openPositions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(openPositions) != 0 {
		t.Errorf("平仓后 OPEN 持仓数 = %d, 期望 0", len(openPositions))
	}

	// 验证冷却期已记录
	if _, inCD := e.cooldown["BTCUSDT"]; !inCD {
		t.Error("平仓后 BTCUSDT 应进入冷却期")
	}
}

// TestE2E_DryRun_StopLossCycle 验证 DRY_RUN 模式下完整交易闭环（止损路径）
// 流程: 滑动窗口喂价 → ScreenSliding 筛选 → openPositions 开仓 →
// monitorPositions 止损平仓
// 验证: 候选筛选正确、持仓入库正确、止损触发、盈亏计算（亏损）
func TestE2E_DryRun_StopLossCycle(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// --- 阶段 1: 喂价 + 筛选 ---
	// 基准价 47（5 分钟前），现价 50 → 涨幅 6.38% >= 阈值 5%
	feedBaseline(e.window, "ETHUSDT", now, 47.0)
	e.window.Add("ETHUSDT", now, 50.0)

	tickers := []binance.Ticker{
		{Symbol: "ETHUSDT", LastPrice: 50.0, QuoteVolume: 300000},
	}
	priceMap := map[string]float64{"ETHUSDT": 50.0}

	candidates := ScreenSliding(e.window, tickers, priceMap,
		e.cfg.MinGainPct, e.cfg.Min24hGainPct, e.cfg.MinQuoteVolume, e.cfg.TopN, now,
		false, 0, 0, 0, "sliding", nil, 0, nil)

	if len(candidates) != 1 {
		t.Fatalf("筛选候选数 = %d, 期望 1", len(candidates))
	}

	// --- 阶段 2: 开仓 ---
	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 1 {
		t.Fatalf("开仓数 = %d, 期望 1", len(opened))
	}

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 1", len(positions))
	}
	posID := positions[0].ID

	// --- 阶段 3: 止损平仓 ---
	// 取消活跃条件单（模拟条件单失效），使本地 monitorPositions 兜底平仓生效
	cancelActiveOrders(t, db)

	// 价格跌到 44 <= 50*(1-0.10)=45 → 触发止损
	priceMap2 := map[string]float64{"ETHUSDT": 44.0}
	e.monitorPositions(ctx, priceMap2)

	pos, err := db.GetPositionByID(posID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if pos.Status != "CLOSED" {
		t.Fatalf("Status = %q, 期望 CLOSED", pos.Status)
	}
	if pos.CloseReason == nil || *pos.CloseReason != "STOP_LOSS" {
		t.Errorf("CloseReason = %v, 期望 STOP_LOSS", pos.CloseReason)
	}
	// 盈亏 = (44 - 50) * 1.0 = -6.0
	if pos.RealizedPnl == nil || !almostEqual(*pos.RealizedPnl, -6.0) {
		t.Errorf("RealizedPnl = %v, 期望 -6.0", pos.RealizedPnl)
	}
	if pos.ExitPrice == nil || *pos.ExitPrice != 44.0 {
		t.Errorf("ExitPrice = %v, 期望 44", pos.ExitPrice)
	}

	// 验证冷却期已记录
	if _, inCD := e.cooldown["ETHUSDT"]; !inCD {
		t.Error("止损平仓后 ETHUSDT 应进入冷却期")
	}
}

// TestE2E_DryRun_MultiSymbolPipeline 验证 DRY_RUN 模式下多币种完整流水线
// 流程: 3 个币种喂价（涨幅分别为 8%/6%/3%）→ 筛选（TopN=2 过滤低涨幅）→
// 并发开仓 → 验证持仓数量与币种选择正确 → 冷却期内不重复开仓
func TestE2E_DryRun_MultiSymbolPipeline(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// 限制 TopN=2，最多开 2 个仓位
	e.cfg.TopN = 2
	e.cfg.MaxOpenPositions = 5

	// --- 阶段 1: 喂价 + 筛选 ---
	// AAAUSDT: 基准 100 → 现价 108（涨幅 8%，成交额 500000）
	// BUSDT:   基准 100 → 现价 106（涨幅 6%，成交额 400000）
	// CCCUSDT: 基准 100 → 现价 103（涨幅 3%，成交额 300000）→ 不达标
	feedBaseline(e.window, "AAAUSDT", now, 100.0)
	e.window.Add("AAAUSDT", now, 108.0)
	feedBaseline(e.window, "BUSDT", now, 100.0)
	e.window.Add("BUSDT", now, 106.0)
	feedBaseline(e.window, "CCCUSDT", now, 100.0)
	e.window.Add("CCCUSDT", now, 103.0)

	tickers := []binance.Ticker{
		{Symbol: "AAAUSDT", LastPrice: 108.0, QuoteVolume: 500000},
		{Symbol: "BUSDT", LastPrice: 106.0, QuoteVolume: 400000},
		{Symbol: "CCCUSDT", LastPrice: 103.0, QuoteVolume: 300000},
	}
	priceMap := map[string]float64{
		"AAAUSDT": 108.0,
		"BUSDT":   106.0,
		"CCCUSDT": 103.0,
	}

	candidates := ScreenSliding(e.window, tickers, priceMap,
		e.cfg.MinGainPct, e.cfg.Min24hGainPct, e.cfg.MinQuoteVolume, e.cfg.TopN, now,
		false, 0, 0, 0, "sliding", nil, 0, nil)

	// CCCUSDT 涨幅 3% < 5% 被过滤，TopN=2 截取前 2 个
	if len(candidates) != 2 {
		t.Fatalf("筛选候选数 = %d, 期望 2（CCCUSDT 涨幅不达标应被过滤）", len(candidates))
	}
	// 按成交额降序: AAAUSDT(500000) > BUSDT(400000)
	if candidates[0].Symbol != "AAAUSDT" {
		t.Errorf("候选[0] = %q, 期望 AAAUSDT（成交额最高）", candidates[0].Symbol)
	}
	if candidates[1].Symbol != "BUSDT" {
		t.Errorf("候选[1] = %q, 期望 BUSDT", candidates[1].Symbol)
	}

	// --- 阶段 2: 并发开仓 ---
	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 2 {
		t.Fatalf("开仓数 = %d, 期望 2", len(opened))
	}

	// 验证 DB 持仓
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 2", len(positions))
	}

	// 验证开仓的币种正确（按成交额排序）
	symbolSet := make(map[string]bool)
	for _, p := range positions {
		symbolSet[p.Symbol] = true
		if p.Status != "OPEN" {
			t.Errorf("%s Status = %q, 期望 OPEN", p.Symbol, p.Status)
		}
		if p.Leverage != 10 {
			t.Errorf("%s Leverage = %d, 期望 10", p.Symbol, p.Leverage)
		}
	}
	if !symbolSet["AAAUSDT"] || !symbolSet["BUSDT"] {
		t.Errorf("开仓币种 = %v, 期望包含 AAAUSDT 和 BUSDT", symbolSet)
	}
	if symbolSet["CCCUSDT"] {
		t.Error("CCCUSDT 不应被开仓（涨幅不达标）")
	}

	// 验证每个持仓都有 2 条止损委托
	orders, err := db.GetActiveOrders()
	if err != nil {
		t.Fatalf("查询活跃委托失败: %v", err)
	}
	if len(orders) != 4 {
		t.Errorf("活跃委托数 = %d, 期望 4（2 个持仓 × 2 条委托）", len(orders))
	}

	// --- 阶段 3: 冷却期内不重复开仓 ---
	// 模拟 AAAUSDT 平仓后进入冷却期
	e.cooldown["AAAUSDT"] = time.Now()

	// 再次用相同候选调用 openPositions，AAAUSDT 应被冷却期跳过
	opened2 := e.openPositions(ctx, candidates, priceMap, positions)
	// BUSDT 已持有（去重），AAAUSDT 在冷却期 → 无新开仓
	if len(opened2) != 0 {
		t.Errorf("冷却期+去重后开仓数 = %d, 期望 0", len(opened2))
	}
}

// TestE2E_DryRun_OpenToCloseFullLifecycle 验证 DRY_RUN 模式下从开仓到平仓的完整生命周期
// 综合验证: 开仓 → 价格未触发任何条件（持仓保持）→ 跟踪止盈激活 →
// 最高价追踪更新 → 跟踪止盈平仓 → 冷却期生效 → 冷却期内不开仓 → 冷却期过期后可重新开仓
func TestE2E_DryRun_OpenToCloseFullLifecycle(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// 使用较短冷却期便于测试
	e.cfg.CooldownMin = 1

	// --- 阶段 1: 筛选 + 开仓 ---
	feedBaseline(e.window, "SOLUSDT", now, 47.0)
	e.window.Add("SOLUSDT", now, 50.0)

	tickers := []binance.Ticker{
		{Symbol: "SOLUSDT", LastPrice: 50.0, QuoteVolume: 500000},
	}
	priceMap := map[string]float64{"SOLUSDT": 50.0}

	candidates := ScreenSliding(e.window, tickers, priceMap,
		e.cfg.MinGainPct, e.cfg.Min24hGainPct, e.cfg.MinQuoteVolume, e.cfg.TopN, now,
		false, 0, 0, 0, "sliding", nil, 0, nil)
	if len(candidates) != 1 {
		t.Fatalf("筛选候选数 = %d, 期望 1", len(candidates))
	}

	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 1 {
		t.Fatalf("开仓数 = %d, 期望 1", len(opened))
	}
	posID := opened[0].ID

	// --- 阶段 2: 价格平稳，不触发任何条件 ---
	// 取消活跃条件单（模拟条件单失效），使本地 monitorPositions 兜底平仓生效
	cancelActiveOrders(t, db)

	// 价格 51: 高于止损价 45，低于激活价 52.5 → 持仓保持
	e.monitorPositions(ctx, map[string]float64{"SOLUSDT": 51.0})
	pos, _ := db.GetPositionByID(posID)
	if pos.Status != "OPEN" || pos.TrailingActive {
		t.Fatalf("价格平稳时 Status=%q TrailingActive=%v, 期望 OPEN/false", pos.Status, pos.TrailingActive)
	}

	// --- 阶段 3: 跟踪止盈激活 ---
	// 价格 53 >= 52.5 → 激活
	e.monitorPositions(ctx, map[string]float64{"SOLUSDT": 53.0})
	pos, _ = db.GetPositionByID(posID)
	if !pos.TrailingActive {
		t.Fatal("价格 53 应激活跟踪止盈")
	}
	// 跟踪止损价 = 53 * 0.97 = 51.41
	if !almostEqual(pos.CurrentStopPrice, 53.0*0.97) {
		t.Errorf("CurrentStopPrice = %v, 期望 ≈%v", pos.CurrentStopPrice, 53.0*0.97)
	}

	// --- 阶段 4: 最高价追踪更新 ---
	// 价格继续涨到 56 → 更新最高价和跟踪止损价
	e.monitorPositions(ctx, map[string]float64{"SOLUSDT": 56.0})
	pos, _ = db.GetPositionByID(posID)
	if pos.HighestPrice == nil || *pos.HighestPrice != 56.0 {
		t.Errorf("HighestPrice = %v, 期望 56（应追踪最高价）", pos.HighestPrice)
	}
	// 跟踪止损价 = 56 * 0.97 = 54.32
	if !almostEqual(pos.CurrentStopPrice, 56.0*0.97) {
		t.Errorf("CurrentStopPrice = %v, 期望 ≈%v", pos.CurrentStopPrice, 56.0*0.97)
	}

	// --- 阶段 5: 跟踪止盈平仓 ---
	// 价格回撤到 54 <= 54.32 → 触发平仓
	e.monitorPositions(ctx, map[string]float64{"SOLUSDT": 54.0})
	pos, _ = db.GetPositionByID(posID)
	if pos.Status != "CLOSED" {
		t.Fatalf("Status = %q, 期望 CLOSED", pos.Status)
	}
	if pos.CloseReason == nil || *pos.CloseReason != "TRAILING_STOP" {
		t.Errorf("CloseReason = %v, 期望 TRAILING_STOP", pos.CloseReason)
	}
	// 盈亏 = (54 - 50) * 1.0 = 4.0
	if pos.RealizedPnl == nil || !almostEqual(*pos.RealizedPnl, 4.0) {
		t.Errorf("RealizedPnl = %v, 期望 4.0", pos.RealizedPnl)
	}

	// --- 阶段 6: 冷却期内不重复开仓 ---
	if _, inCD := e.cooldown["SOLUSDT"]; !inCD {
		t.Fatal("平仓后 SOLUSDT 应进入冷却期")
	}
	opened2 := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened2) != 0 {
		t.Errorf("冷却期内开仓数 = %d, 期望 0", len(opened2))
	}

	// --- 阶段 7: 冷却期过期后可重新开仓 ---
	e.cooldown["SOLUSDT"] = time.Now().Add(-2 * time.Minute) // 模拟冷却期过期
	opened3 := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened3) != 1 {
		t.Fatalf("冷却期过期后开仓数 = %d, 期望 1", len(opened3))
	}
	if opened3[0].Symbol != "SOLUSDT" {
		t.Errorf("重新开仓 Symbol = %q, 期望 SOLUSDT", opened3[0].Symbol)
	}
}

// TestOpenPositions_Boundary_CooldownExpired 验证冷却期过期后允许重新开仓
// CooldownMin=1，冷却记录设为 2 分钟前（已超过 1 分钟冷却期），应允许开仓
func TestOpenPositions_Boundary_CooldownExpired(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	// 设置冷却期 1 分钟，记录 BTCUSDT 平仓时间为 2 分钟前（已过期）
	e.cfg.CooldownMin = 1
	e.cooldown["BTCUSDT"] = time.Now().Add(-2 * time.Minute)

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	// 验证冷却期过期后成功开仓
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("冷却期过期后应允许开仓，OPEN 持仓数 = %d, 期望 1", len(positions))
	}
	if positions[0].Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %q, 期望 BTCUSDT", positions[0].Symbol)
	}
}

// ========== 十四、双触发防护与本地兜底测试（幽灵持仓根因 + HOMEUSDT 裸奔修复） ==========

// openPositionWithOrders 通过 openPositions 开仓并生成 STOP_MARKET + TRAILING_STOP_MARKET 活跃条件单。
// 返回新持仓记录（含 ID 与风控状态）。
func openPositionWithOrders(t *testing.T, e *Engine, db *storage.DB, symbol string, price float64) storage.Position {
	t.Helper()
	ctx := context.Background()
	candidates := []Candidate{
		{Symbol: symbol, GainPct: 6.0, QuoteVolume: 200000},
	}
	e.openPositions(ctx, candidates, map[string]float64{symbol: price}, nil)
	positions, err := db.GetOpenPositions()
	if err != nil || len(positions) != 1 {
		t.Fatalf("开仓失败: positions=%v err=%v", positions, err)
	}
	if len(e.failedOpen) != 0 {
		t.Fatalf("开仓不应失败，failedOpen = %v", e.failedOpen)
	}
	return positions[0]
}

// TestMonitor_ActiveStopOrders_NoImmediateClose 验证双触发防护的主路径：
// 价格击穿止损位时，本地不立即平仓（给交易所条件单 + SyncOrders 成交机会），
// 防止本地重复平仓遭 -2022（ReduceOnly 无仓可平）产生幽灵持仓。
func TestMonitor_ActiveStopOrders_NoImmediateClose(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.stopFallbackDelay = 30 * time.Second // 默认兜底时长，验证"未超时不平仓"

	pos := openPositionWithOrders(t, e, db, "BTCUSDT", 50.0) // 止损价 = 50*0.9 = 45

	// 价格 44 跌破止损线，条件单仍活跃 → 只开始兜底计时，不平仓
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	after, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after.Status != "OPEN" {
		t.Fatalf("条件单存在且未超时时本地不应平仓，Status = %q, 期望 OPEN", after.Status)
	}
	if _, ok := e.stopBreachSince[pos.ID]; !ok {
		t.Error("价格击穿止损位后应开始兜底计时")
	}
}

// TestMonitor_ActiveStopOrders_FallbackTimeout 验证本地兜底超时平仓：
// 价格持续击穿止损位超过 stopFallbackDelay 后条件单仍未成交 → 主动撤单市价平仓。
// （2026-08-11 HOMEUSDT 条件单全程未触发、持仓从 +18% 裸奔至 -7.3U 事故的修复验证）
func TestMonitor_ActiveStopOrders_FallbackTimeout(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.stopFallbackDelay = 60 * time.Millisecond

	pos := openPositionWithOrders(t, e, db, "BTCUSDT", 50.0) // 止损价 = 45

	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	after, err := db.GetPositionByID(pos.ID)
	if err != nil || after.Status != "OPEN" {
		t.Fatalf("超时前不应平仓: status=%v err=%v", after.Status, err)
	}

	// 1×delay：本地兜底需标记价确认（防针核心）。DRY_RUN 标记价固定 100，未击穿止损 45，
	// 视为疑似插针，本地不平仓（等待交易所条件单 / 强制兜底阀）。
	time.Sleep(80 * time.Millisecond) // 80ms > 60ms 且 < 2×60ms
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	mid, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if mid.Status != "OPEN" {
		t.Fatalf("标记价未确认时本地不应兜底平仓（疑似插针），Status = %q, 期望 OPEN", mid.Status)
	}
	// 2×delay：强制兜底阀（防标记价长期滞后导致保护悬空）→ 平仓
	time.Sleep(70 * time.Millisecond) // 累计 150ms > 2×60ms
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	after2, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after2.Status != "CLOSED" {
		t.Fatalf("超时后本地兜底应平仓，Status = %q, 期望 CLOSED", after2.Status)
	}
	if after2.CloseReason == nil || *after2.CloseReason != "STOP_LOSS" {
		t.Errorf("CloseReason = %v, 期望 STOP_LOSS", after2.CloseReason)
	}
	if _, ok := e.stopBreachSince[pos.ID]; ok {
		t.Error("平仓后兜底计时应清理")
	}
}

// TestMonitor_ActiveStopOrders_FallbackRecoverResets 验证价格恢复后兜底计时重置：
// 瞬时毛刺击穿后价格回到止损位上方，不应累积超时时间，避免误平仓。
func TestMonitor_ActiveStopOrders_FallbackRecoverResets(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.stopFallbackDelay = 50 * time.Millisecond

	pos := openPositionWithOrders(t, e, db, "BTCUSDT", 50.0)

	// 第一次击穿（44 < 45）→ 开始计时
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	if _, ok := e.stopBreachSince[pos.ID]; !ok {
		t.Fatal("首次击穿应开始兜底计时")
	}
	// 价格恢复至止损位上方 → 计时重置
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 46.0})
	if _, ok := e.stopBreachSince[pos.ID]; ok {
		t.Fatal("价格恢复后应重置兜底计时")
	}
	// 再次击穿：需重新计时满 stopFallbackDelay 才平仓
	time.Sleep(20 * time.Millisecond)
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	after, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after.Status != "OPEN" {
		t.Fatalf("重新计时未满不应平仓，Status = %q, 期望 OPEN", after.Status)
	}
	time.Sleep(100 * time.Millisecond)
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.0})
	after2, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after2.Status != "CLOSED" {
		t.Fatalf("重新计时满后应平仓，Status = %q, 期望 CLOSED", after2.Status)
	}
}

// TestMonitor_ActiveStopOrders_FallbackBufferIgnoresGrind 验证兜底缓冲带：
// 价格贴着止损线磨（跌破幅度 <0.3%）不启动兜底计时，避免与正常工作的交易所条件单
// 抢单产生重复平仓/幽灵单（SQD 14:50 贴线 0.01% 案例）；真跌破缓冲带才兜底。
func TestMonitor_ActiveStopOrders_FallbackBufferIgnoresGrind(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.stopFallbackDelay = 50 * time.Millisecond

	pos := openPositionWithOrders(t, e, db, "BTCUSDT", 50.0) // 止损位 45
	// 缓冲带下沿 = 45 * (1-0.003) = 44.865；44.95 属于贴线磨（仅低 0.11%）

	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.95})
	if _, ok := e.stopBreachSince[pos.ID]; ok {
		t.Fatal("贴线磨（缓冲带内）不应启动兜底计时")
	}
	time.Sleep(80 * time.Millisecond)
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.95})
	after, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after.Status != "OPEN" {
		t.Fatalf("贴线磨超时也不应兜底平仓，Status = %q, 期望 OPEN", after.Status)
	}

	// 真跌破缓冲带（44.8 < 44.865）→ 开始计时，超时兜底平仓
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.8})
	if _, ok := e.stopBreachSince[pos.ID]; !ok {
		t.Fatal("真跌破缓冲带后应启动兜底计时")
	}
	// 2×delay（50ms→100ms）：真跌破后强制兜底阀平仓（1×delay 时标记价未确认会被插针守卫拦下）
	time.Sleep(120 * time.Millisecond)
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 44.8})
	after2, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after2.Status != "CLOSED" {
		t.Fatalf("真跌破后超时应兜底平仓，Status = %q, 期望 CLOSED", after2.Status)
	}
}

// TestMonitor_ActiveStopOrders_FallbackTrailing 验证移动止盈已激活时的兜底平仓原因：
// 击穿移动止损位超时 → reason=TRAILING_STOP。
func TestMonitor_ActiveStopOrders_FallbackTrailing(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.stopFallbackDelay = 60 * time.Millisecond

	pos := openPositionWithOrders(t, e, db, "BTCUSDT", 50.0)
	hp := 55.0 // 假设价格曾冲到 +10%
	stop := hp * (1 - e.cfg.TrailingCallback) // 55 * 0.97 = 53.35
	if err := db.UpdateRiskState(pos.ID, &hp, true, stop); err != nil {
		t.Fatalf("更新风控状态失败: %v", err)
	}

	// 1×delay：标记价未确认（DRY_RUN 固定 100 > 跟踪止损 53.35）→ 插针守卫拦下不平仓
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 53.0})
	time.Sleep(80 * time.Millisecond)
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 53.0})
	mid, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if mid.Status != "OPEN" {
		t.Fatalf("标记价未确认时本地不应兜底平仓（疑似插针），Status = %q, 期望 OPEN", mid.Status)
	}
	// 2×delay：强制兜底阀 → 平仓，reason=TRAILING_STOP
	time.Sleep(70 * time.Millisecond)
	e.monitorPositions(ctx, map[string]float64{"BTCUSDT": 53.0})
	after, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after.Status != "CLOSED" {
		t.Fatalf("超时后本地兜底应平仓，Status = %q, 期望 CLOSED", after.Status)
	}
	if after.CloseReason == nil || *after.CloseReason != "TRAILING_STOP" {
		t.Errorf("CloseReason = %v, 期望 TRAILING_STOP", after.CloseReason)
	}
}

// TestMonitor_CancelOrdersRestoresLocalClose 验证条件单撤销后本地兜底恢复即时平仓：
// 这是原有双触发防护的保留行为（条件单失效 → 本地 monitorPositions 兜底生效），
// 且该路径不依赖兜底超时。
func TestMonitor_CancelOrdersRestoresLocalClose(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	e.stopFallbackDelay = time.Hour // 超时设得足够长，验证"撤销条件单"路径不依赖超时

	pos := openPositionWithOrders(t, e, db, "ETHUSDT", 100.0) // 止损价 = 90

	// 取消活跃委托（模拟条件单失效）→ 本地兜底平仓恢复生效
	cancelActiveOrders(t, db)
	e.monitorPositions(ctx, map[string]float64{"ETHUSDT": 89.0})
	after, err := db.GetPositionByID(pos.ID)
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if after.Status != "CLOSED" {
		t.Fatalf("条件单失效后本地兜底应平仓，Status = %q, 期望 CLOSED", after.Status)
	}
	if after.CloseReason == nil || *after.CloseReason != "STOP_LOSS" {
		t.Errorf("CloseReason = %v, 期望 STOP_LOSS", after.CloseReason)
	}
}

// ========== 十五、K 线信号模式（buildKlineOpenMap）测试 ==========

// TestBuildKlineOpenMap 验证 K 线开盘价构建：
// ① 只对粗筛通过的币拉取（成交额 + 24h 涨跌幅）；
// ② DRY_RUN 下 GetKlineOpen 返回固定 100.0；
// ③ 同一 15m 周期内第二次调用命中缓存，结果一致。
func TestBuildKlineOpenMap(t *testing.T) {
	e, _ := newTestEngine(t)
	e.cfg.SignalMode = "kline"
	e.cfg.Timeframe = "15m"
	e.cfg.MinQuoteVolume = 100000
	e.cfg.Min24hGainPct = 5.0
	e.cfg.EnableShort = true

	ctx := context.Background()
	now := time.Now().UnixMilli()

	tickers := []binance.Ticker{
		{Symbol: "AAUSDT", QuoteVolume: 200000, PriceChange: 8.0},  // 粗筛通过（做多）
		{Symbol: "BBUSDT", QuoteVolume: 50000, PriceChange: 8.0},   // 成交额不足，跳过
		{Symbol: "CCUSDT", QuoteVolume: 200000, PriceChange: 2.0},  // 24h 涨幅不足，跳过
		{Symbol: "DDUSDT", QuoteVolume: 200000, PriceChange: -8.0}, // 粗筛通过（做空）
	}

	m := e.buildKlineOpenMap(ctx, tickers, now, nil)
	if len(m) != 2 {
		t.Fatalf("K 线开盘价数量 = %d, 期望 2（仅 AAUSDT + DDUSDT 粗筛通过）", len(m))
	}
	if m["AAUSDT"] != 100.0 {
		t.Errorf("AAUSDT open = %v, 期望 100.0", m["AAUSDT"])
	}
	if m["DDUSDT"] != 100.0 {
		t.Errorf("DDUSDT open = %v, 期望 100.0", m["DDUSDT"])
	}
	if _, ok := m["BBUSDT"]; ok {
		t.Error("BBUSDT 不应在结果中（成交额不足）")
	}
	if _, ok := m["CCUSDT"]; ok {
		t.Error("CCUSDT 不应在结果中（24h 涨幅不足）")
	}

	// 同一周期二次调用：命中缓存，结果一致
	m2 := e.buildKlineOpenMap(ctx, tickers, now+5000, nil)
	if len(m2) != 2 || m2["AAUSDT"] != 100.0 {
		t.Errorf("缓存复用失败: m2 = %v", m2)
	}
}

// ========== 十六、熔断器接入测试 ==========

// TestBreaker_BlocksOpenPositions 验证日亏熔断触发后 openPositions 跳过开仓
// 流程: 注入已触发日熔断的熔断器 → openPositions → 期望 0 个新开仓（DB 无新持仓）
func TestBreaker_BlocksOpenPositions(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// 构造已触发日熔断的熔断器：初始权益 1000，当日亏损 100（10% >= 5% 阈值）
	breaker := risk.NewCircuitBreaker(0.05, 0.15, 5)
	breaker.SetInitialEquity(1000)
	if !breaker.CheckDailyLoss(-100) {
		t.Fatal("CheckDailyLoss(-100) 应触发日熔断（10% >= 5%）")
	}
	e.SetBreaker(breaker)

	// 准备一个达标候选币
	feedBaseline(e.window, "AAAUSDT", now, 100.0)
	e.window.Add("AAAUSDT", now, 108.0)
	candidates := []Candidate{
		{Symbol: "AAAUSDT", GainPct: 8.0, QuoteVolume: 500000},
	}
	priceMap := map[string]float64{"AAAUSDT": 108.0}

	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 0 {
		t.Fatalf("日熔断后开仓数 = %d, 期望 0", len(opened))
	}

	// 验证 DB 无新持仓
	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("日熔断后 OPEN 持仓数 = %d, 期望 0", len(positions))
	}
}

// TestBreaker_NotTriggered_AllowsOpen 验证熔断未触发时正常开仓
// 对照用例：日亏 2%（<5%）未触发，开仓应正常执行
func TestBreaker_NotTriggered_AllowsOpen(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()

	// 构造未触发熔断器：初始权益 1000，当日亏损 20（2% < 5% 阈值）
	breaker := risk.NewCircuitBreaker(0.05, 0.15, 5)
	breaker.SetInitialEquity(1000)
	if breaker.CheckDailyLoss(-20) {
		t.Fatal("CheckDailyLoss(-20) 不应触发日熔断（2% < 5%）")
	}
	e.SetBreaker(breaker)

	feedBaseline(e.window, "BBBUSDT", now, 100.0)
	e.window.Add("BBBUSDT", now, 108.0)
	candidates := []Candidate{
		{Symbol: "BBBUSDT", GainPct: 8.0, QuoteVolume: 500000},
	}
	priceMap := map[string]float64{"BBBUSDT": 108.0}

	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 1 {
		t.Fatalf("未熔断时开仓数 = %d, 期望 1", len(opened))
	}

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Errorf("未熔断时 OPEN 持仓数 = %d, 期望 1", len(positions))
	}
}

// TestBreaker_OnClose_UpdatesDailyLoss 验证条件单平仓回调（实盘主路径）更新日亏熔断
// 背景：此前熔断只在引擎本地平仓（超时/手动）路径更新，交易所条件单平仓从不触发
// 熔断检查，导致实盘每日 5% 亏损熔断几乎永不生效（2026-08-08 实盘 -35U 无刹车事故根因）。
// 流程: 注入熔断器(初始权益1000，阈值5%=-50U) → 数据库写入今日已平仓 -60U →
//
//	模拟条件单平仓回调 onPositionClosed → 熔断应触发，阻止后续开仓
func TestBreaker_OnClose_UpdatesDailyLoss(t *testing.T) {
	e, db := newTestEngine(t)

	breaker := risk.NewCircuitBreaker(0.05, 0.15, 5)
	breaker.SetInitialEquity(1000)
	e.SetBreaker(breaker)

	// 写入一笔今日已平仓亏损记录（-60U = 6% > 5% 阈值）
	id, err := db.InsertPosition(&storage.Position{
		Symbol: "BTCUSDT", Side: "LONG", EntryPrice: 100, Amount: 1,
		Leverage: 10, Status: "OPEN", OpenedAt: time.Now().UnixMilli(),
	})
	if err != nil {
		t.Fatalf("插入持仓失败: %v", err)
	}
	if err := db.ClosePosition(id, "STOP_LOSS", -60.0, nil, 0); err != nil {
		t.Fatalf("平仓失败: %v", err)
	}

	// 模拟交易所条件单平仓通知（实盘主路径）
	e.onPositionClosed("BTCUSDT", "STOP_LOSS")

	if !e.isBreakerBlocked() {
		t.Fatal("条件单平仓后日亏熔断应触发（-60U/1000 = 6% >= 5%）")
	}
}

// TestBreaker_ResetDaily_AfterClose 验证跨天时日熔断自动重置
// 流程: 平仓触发日熔断 → checkBreakerReset 跨天后重置 → 可再次开仓
func TestBreaker_ResetDaily_AfterClose(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	breaker := risk.NewCircuitBreaker(0.05, 0.15, 5)
	breaker.SetInitialEquity(1000)
	breaker.CheckDailyLoss(-100) // 触发日熔断
	e.SetBreaker(breaker)

	// 初始日期熔断应生效
	if !e.isBreakerBlocked() {
		t.Fatal("日熔断后 isBreakerBlocked 应为 true")
	}

	// 先记录今天的日期（模拟引擎运行中），再模拟跨天（明天）：checkBreakerReset 应重置日熔断
	e.checkBreakerReset(time.Now())
	now := time.Now().Add(24 * time.Hour)
	e.checkBreakerReset(now)
	if e.isBreakerBlocked() {
		t.Error("跨天后日熔断应被重置，isBreakerBlocked 应为 false")
	}

	// 重置后应能正常开仓
	feedBaseline(e.window, "CCCUSDT", time.Now().UnixMilli(), 100.0)
	candidates := []Candidate{
		{Symbol: "CCCUSDT", GainPct: 8.0, QuoteVolume: 500000},
	}
	priceMap := map[string]float64{"CCCUSDT": 108.0}
	opened := e.openPositions(ctx, candidates, priceMap, nil)
	if len(opened) != 1 {
		t.Fatalf("日熔断重置后开仓数 = %d, 期望 1", len(opened))
	}
	_ = db
}

// ========== 十七、新币过滤测试 ==========

// TestOpenPositions_NewListingFiltered 验证新币过滤：上市天数 <= 阈值的候选不开仓，老币正常开仓
func TestOpenPositions_NewListingFiltered(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	dayMs := int64(24 * 3600 * 1000)

	e.cfg.EnableNewListingFilter = true
	e.cfg.NewListingMinDays = 60
	e.client.SetOnboardDatesForTest(map[string]int64{
		"NEWUSDT": now - 30*dayMs,  // 上市 30 天：应被过滤
		"OLDUSDT": now - 365*dayMs, // 上市 365 天：正常
	})

	candidates := []Candidate{
		{Symbol: "NEWUSDT", GainPct: 6.0, QuoteVolume: 200000},
		{Symbol: "OLDUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"NEWUSDT": 10.0, "OLDUSDT": 50.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 1（仅 OLDUSDT，NEWUSDT 被新币过滤拦截）", len(positions))
	}
	if positions[0].Symbol != "OLDUSDT" {
		t.Errorf("持仓 Symbol = %q, 期望 OLDUSDT", positions[0].Symbol)
	}
}

// TestOpenPositions_NewListingFilterDisabled 验证关闭过滤后新币正常开仓（开关关闭不影响交易）
func TestOpenPositions_NewListingFilterDisabled(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()
	now := time.Now().UnixMilli()
	dayMs := int64(24 * 3600 * 1000)

	// EnableNewListingFilter 默认 false：即使存在上市日期数据也不过滤
	e.client.SetOnboardDatesForTest(map[string]int64{
		"NEWUSDT": now - 30*dayMs,
		"OLDUSDT": now - 365*dayMs,
	})

	candidates := []Candidate{
		{Symbol: "NEWUSDT", GainPct: 6.0, QuoteVolume: 200000},
		{Symbol: "OLDUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"NEWUSDT": 10.0, "OLDUSDT": 50.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 2 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 2（过滤关闭时新币不拦截）", len(positions))
	}
}

// TestOpenPositions_NewListing_UnknownDateAllowed 验证上市日期未知（exchangeInfo 未加载）时失败放行
func TestOpenPositions_NewListing_UnknownDateAllowed(t *testing.T) {
	e, db := newTestEngine(t)
	ctx := context.Background()

	e.cfg.EnableNewListingFilter = true
	e.cfg.NewListingMinDays = 60
	// onboardDateMap 为空：模拟 exchangeInfo 未加载/加载失败

	candidates := []Candidate{
		{Symbol: "BTCUSDT", GainPct: 6.0, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"BTCUSDT": 50000.0}

	e.openPositions(ctx, candidates, priceMap, nil)

	positions, err := db.GetOpenPositions()
	if err != nil {
		t.Fatalf("查询持仓失败: %v", err)
	}
	if len(positions) != 1 {
		t.Fatalf("OPEN 持仓数 = %d, 期望 1（上市日期未知时放行，不误杀）", len(positions))
	}
}

// TestBuildNewListingBlocked_Logs 验证拦截集合计算与过滤日志（含去重）
func TestBuildNewListingBlocked_Logs(t *testing.T) {
	e, db := newTestEngine(t)
	now := time.Now().UnixMilli()
	dayMs := int64(24 * 3600 * 1000)

	e.cfg.EnableNewListingFilter = true
	e.cfg.NewListingMinDays = 60
	e.client.SetOnboardDatesForTest(map[string]int64{
		"NEWUSDT": now - 10*dayMs,
		"OLDUSDT": now - 365*dayMs,
	})

	tickers := []binance.Ticker{
		{Symbol: "NEWUSDT", LastPrice: 10, PriceChange: 8, QuoteVolume: 500000},
		{Symbol: "OLDUSDT", LastPrice: 50, PriceChange: 8, QuoteVolume: 500000},
	}

	blocked := e.buildNewListingBlocked(tickers, now)
	if !blocked["NEWUSDT"] {
		t.Error("期望 NEWUSDT 被拦截")
	}
	if blocked["OLDUSDT"] {
		t.Error("期望 OLDUSDT 不被拦截")
	}

	logs, err := db.GetRecentLogs(10)
	if err != nil {
		t.Fatalf("查询日志失败: %v", err)
	}
	count := 0
	for _, l := range logs {
		if l.Module == "screener" && l.Symbol == "NEWUSDT" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("NEWUSDT 过滤日志数 = %d, 期望 1", count)
	}

	// 第二次调用：日志不重复（去重）
	e.buildNewListingBlocked(tickers, now)
	logs2, _ := db.GetRecentLogs(10)
	count2 := 0
	for _, l := range logs2 {
		if l.Module == "screener" && l.Symbol == "NEWUSDT" {
			count2++
		}
	}
	if count2 != 1 {
		t.Errorf("二次调用后 NEWUSDT 过滤日志数 = %d, 期望仍为 1（去重）", count2)
	}
}
