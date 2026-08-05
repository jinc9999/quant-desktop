// Package bindings QuantService 绑定层单元测试
package bindings

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/order"
	"quant-desktop/internal/risk"
	"quant-desktop/internal/storage"
)

// newTestService 手动构造测试用 QuantService（绕过 Init 的硬编码路径）
// 使用 t.TempDir() 创建临时数据库，DRY_RUN 模式客户端，并在测试结束后自动清理资源。
// 参数:
//   - t: 测试实例，用于创建临时目录和注册清理函数
//
// 返回:
//   - *QuantService: 初始化好的测试服务实例
func newTestService(t *testing.T) *QuantService {
	t.Helper()

	// 1. 创建临时 DB
	dbPath := filepath.Join(t.TempDir(), "test.db")
	db, err := storage.NewDB(dbPath)
	if err != nil {
		t.Fatalf("创建测试数据库失败: %v", err)
	}

	// 2. 创建 DRY_RUN 模式的 binance.Client
	client := binance.NewClient("", "", "DRY_RUN", "", 0)

	// 3. 创建 WsManager
	ws := binance.NewWsManager("DRY_RUN")

	// 4. 创建 order.Manager
	orderMgr := order.NewManager(client, db)

	// 5. 创建 CircuitBreaker
	breaker := risk.NewCircuitBreaker(0.20, 0.10, 5)

	// 6. 设置默认策略配置
	cfg := binance.DefaultStrategyConfig()

	// 7. 创建可取消的 context
	ctx, cancel := context.WithCancel(context.Background())

	// 8. 组装 QuantService
	svc := &QuantService{
		db:       db,
		client:   client,
		ws:       ws,
		orderMgr: orderMgr,
		breaker:  breaker,
		cfg:      cfg,
		ctx:      ctx,
		cancel:   cancel,
		mode:     "DRY_RUN",
	}

	// 9. 注册清理函数：取消 context 并关闭数据库
	t.Cleanup(func() {
		cancel()
		db.Close()
	})

	return svc
}

// ==================== 一、生命周期测试 ====================

// TestNewQuantService 验证 NewQuantService 创建的服务实例
// 验证点: 返回非 nil、cfg 为默认配置、mode 为 "DRY_RUN"
func TestNewQuantService(t *testing.T) {
	svc := NewQuantService()
	if svc == nil {
		t.Fatal("NewQuantService() 返回 nil")
	}

	defaultCfg := binance.DefaultStrategyConfig()
	if svc.cfg != defaultCfg {
		t.Errorf("cfg = %+v, 期望默认配置 %+v", svc.cfg, defaultCfg)
	}
	if svc.mode != "SIMULATION" {
		t.Errorf("mode = %q, 期望 %q", svc.mode, "SIMULATION")
	}
}

// TestSetCredentials 验证 SetCredentials 切换模式与密钥
// 验证点: 返回包含 "SIMULATION" 的成功消息，GetMode() 返回 "SIMULATION"
func TestSetCredentials(t *testing.T) {
	svc := newTestService(t)

	result := svc.SetCredentials("SIMULATION", "key", "secret")
	if !strings.Contains(result, "SIMULATION") {
		t.Errorf("SetCredentials 返回 %q, 期望包含 %q", result, "SIMULATION")
	}
	if got := svc.GetMode(); got != "SIMULATION" {
		t.Errorf("GetMode() = %q, 期望 %q", got, "SIMULATION")
	}
}

// TestSetCredentials_RunningBlocked 验证策略运行中时禁止切换模式
// 验证点: started=true 时 SetCredentials 返回包含 "运行中" 的错误消息
func TestSetCredentials_RunningBlocked(t *testing.T) {
	svc := newTestService(t)
	svc.started = true

	result := svc.SetCredentials("TESTNET", "key", "secret")
	if !strings.Contains(result, "运行中") {
		t.Errorf("SetCredentials 返回 %q, 期望包含 %q", result, "运行中")
	}
}

// ==================== 二、策略控制测试 ====================

// TestStartStopStrategy 验证策略启动→状态查询→停止→状态查询的完整流程
// 验证点: StartStrategy 成功 → running=true → StopStrategy 成功 → running=false
func TestStartStopStrategy(t *testing.T) {
	svc := newTestService(t)

	// 启动策略
	startResult := svc.StartStrategy()
	if !strings.Contains(startResult, "成功") {
		t.Fatalf("StartStrategy 返回 %q, 期望包含 %q", startResult, "成功")
	}

	// 验证运行状态
	status := svc.GetStrategyStatus()
	if running, ok := status["running"].(bool); !ok || !running {
		t.Errorf("启动后 running = %v, 期望 true", status["running"])
	}

	// 停止策略
	stopResult := svc.StopStrategy()
	if !strings.Contains(stopResult, "成功") {
		t.Fatalf("StopStrategy 返回 %q, 期望包含 %q", stopResult, "成功")
	}

	// 验证停止状态
	status = svc.GetStrategyStatus()
	if running, ok := status["running"].(bool); !ok || running {
		t.Errorf("停止后 running = %v, 期望 false", status["running"])
	}
}

// TestStartStrategy_AlreadyRunning 验证重复启动策略时返回提示
// 验证点: 第二次 StartStrategy 返回包含 "已在运行" 的消息
func TestStartStrategy_AlreadyRunning(t *testing.T) {
	svc := newTestService(t)

	// 第一次启动
	svc.StartStrategy()
	defer svc.StopStrategy()

	// 第二次启动
	result := svc.StartStrategy()
	if !strings.Contains(result, "已在运行") {
		t.Errorf("重复启动返回 %q, 期望包含 %q", result, "已在运行")
	}
}

// TestStopStrategy_NotRunning 验证未启动时停止策略返回提示
// 验证点: StopStrategy 返回包含 "未在运行" 的消息
func TestStopStrategy_NotRunning(t *testing.T) {
	svc := newTestService(t)

	result := svc.StopStrategy()
	if !strings.Contains(result, "未在运行") {
		t.Errorf("StopStrategy 返回 %q, 期望包含 %q", result, "未在运行")
	}
}

// ==================== 三、配置测试 ====================

// TestGetSetConfig 验证配置的读取与更新
// 验证点: GetConfig 返回默认配置 → 修改 MinGainPct=8.0 → SetConfig → GetConfig 验证新值
func TestGetSetConfig(t *testing.T) {
	svc := newTestService(t)

	// 验证默认配置
	cfg := svc.GetConfig()
	defaultCfg := binance.DefaultStrategyConfig()
	if cfg.MinGainPct != defaultCfg.MinGainPct {
		t.Fatalf("默认 MinGainPct = %v, 期望 %v", cfg.MinGainPct, defaultCfg.MinGainPct)
	}

	// 修改并设置新配置
	cfg.MinGainPct = 8.0
	result := svc.SetConfig(cfg)
	if !strings.Contains(result, "已更新") {
		t.Fatalf("SetConfig 返回 %q, 期望包含 %q", result, "已更新")
	}

	// 验证新配置生效
	got := svc.GetConfig()
	if got.MinGainPct != 8.0 {
		t.Errorf("更新后 MinGainPct = %v, 期望 8.0", got.MinGainPct)
	}
}

// TestSetConfig_RunningBlocked 验证策略运行中时禁止修改配置
// 验证点: started=true 时 SetConfig 返回包含 "运行中" 的消息
func TestSetConfig_RunningBlocked(t *testing.T) {
	svc := newTestService(t)
	svc.started = true

	cfg := svc.GetConfig()
	cfg.MinGainPct = 99.0
	result := svc.SetConfig(cfg)
	if !strings.Contains(result, "运行中") {
		t.Errorf("SetConfig 返回 %q, 期望包含 %q", result, "运行中")
	}

	// 验证配置未被修改
	got := svc.GetConfig()
	if got.MinGainPct == 99.0 {
		t.Error("运行中时配置不应被修改")
	}
}

// ==================== 四、数据查询测试 ====================

// TestGetPositions_Empty 验证空库查询持仓返回空切片
// 验证点: 无错误、返回切片长度为 0
func TestGetPositions_Empty(t *testing.T) {
	svc := newTestService(t)

	positions, err := svc.GetPositions()
	if err != nil {
		t.Fatalf("GetPositions 返回错误: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("空库持仓数 = %d, 期望 0", len(positions))
	}
}

// TestGetLogs_Empty 验证空库查询日志返回空切片
// 验证点: 无错误、返回切片长度为 0
func TestGetLogs_Empty(t *testing.T) {
	svc := newTestService(t)

	logs, err := svc.GetLogs(100)
	if err != nil {
		t.Fatalf("GetLogs 返回错误: %v", err)
	}
	if len(logs) != 0 {
		t.Errorf("空库日志数 = %d, 期望 0", len(logs))
	}
}

// TestGetOrders_Empty 验证空库查询委托返回空切片
// 验证点: 无错误、返回切片长度为 0
func TestGetOrders_Empty(t *testing.T) {
	svc := newTestService(t)

	orders, err := svc.GetOrders("")
	if err != nil {
		t.Fatalf("GetOrders 返回错误: %v", err)
	}
	if len(orders) != 0 {
		t.Errorf("空库委托数 = %d, 期望 0", len(orders))
	}
}

// TestGetOrderEvents_Empty 验证空库查询委托事件返回空切片
// 验证点: 无错误、返回切片长度为 0
func TestGetOrderEvents_Empty(t *testing.T) {
	svc := newTestService(t)

	events, err := svc.GetOrderEvents(0, 100)
	if err != nil {
		t.Fatalf("GetOrderEvents 返回错误: %v", err)
	}
	if len(events) != 0 {
		t.Errorf("空库事件数 = %d, 期望 0", len(events))
	}
}

// ==================== 五、委托操作测试 ====================

// TestCancelOrder_NotFound 验证取消不存在的委托返回提示
// 验证点: CancelOrder(999999) 返回包含 "未找到" 的消息
func TestCancelOrder_NotFound(t *testing.T) {
	svc := newTestService(t)

	result := svc.CancelOrder(999999)
	if !strings.Contains(result, "未找到") {
		t.Errorf("CancelOrder 返回 %q, 期望包含 %q", result, "未找到")
	}
}

// TestGetOrderSyncStatus 验证委托同步状态返回必要的键
// 验证点: 返回 map 包含 activeCount、lastSyncTime、lastSyncError 三个键
func TestGetOrderSyncStatus(t *testing.T) {
	svc := newTestService(t)

	status := svc.GetOrderSyncStatus()

	requiredKeys := []string{"activeCount", "lastSyncTime", "lastSyncError"}
	for _, key := range requiredKeys {
		if _, ok := status[key]; !ok {
			t.Errorf("GetOrderSyncStatus 返回的 map 缺少键 %q", key)
		}
	}
}

// ==================== 六、代理配置测试 ====================

// TestSetGetProxyConfig 验证界面设置代理的完整流程
// 验证点: SetProxyConfig 保存成功 → GetProxyConfig 返回正确值 → 客户端已重建
func TestSetGetProxyConfig(t *testing.T) {
	svc := newTestService(t)

	// 设置代理
	result := svc.SetProxyConfig("127.0.0.1", 7890)
	if !strings.Contains(result, "127.0.0.1:7890") {
		t.Fatalf("SetProxyConfig 返回 %q, 期望包含 %q", result, "127.0.0.1:7890")
	}

	// 读取代理配置
	cfg := svc.GetProxyConfig()
	if cfg["address"] != "127.0.0.1" {
		t.Errorf("address = %v, 期望 %q", cfg["address"], "127.0.0.1")
	}
	if cfg["port"] != 7890 {
		t.Errorf("port = %v, 期望 %d", cfg["port"], 7890)
	}

	// 验证客户端已重建（非 nil）
	if svc.client == nil {
		t.Error("SetProxyConfig 后 client 不应为 nil")
	}
}

// TestSetProxyConfig_RunningBlocked 验证策略运行中时禁止修改代理
// 验证点: started=true 时 SetProxyConfig 返回包含 "运行中" 的消息
func TestSetProxyConfig_RunningBlocked(t *testing.T) {
	svc := newTestService(t)
	svc.started = true

	result := svc.SetProxyConfig("10.0.0.1", 1080)
	if !strings.Contains(result, "运行中") {
		t.Errorf("SetProxyConfig 返回 %q, 期望包含 %q", result, "运行中")
	}
}

// TestClearProxyConfig 验证清除代理配置（恢复自动检测）
// 验证点: 设置代理后清除 → GetProxyConfig 返回空值
func TestClearProxyConfig(t *testing.T) {
	svc := newTestService(t)

	// 先设置
	svc.SetProxyConfig("127.0.0.1", 7890)

	// 再清除
	result := svc.SetProxyConfig("", 0)
	if !strings.Contains(result, "自动检测") {
		t.Fatalf("清除代理返回 %q, 期望包含 %q", result, "自动检测")
	}

	cfg := svc.GetProxyConfig()
	if cfg["address"] != "" {
		t.Errorf("清除后 address = %v, 期望空字符串", cfg["address"])
	}
}

// ==================== 七、仪表盘数据测试 ====================

// TestGetDashboardData 验证仪表盘聚合接口返回完整数据
// 验证点: 返回 map 包含所有必要键，DRY_RUN 模式下余额为模拟值
func TestGetDashboardData(t *testing.T) {
	svc := newTestService(t)

	data := svc.GetDashboardData()

	requiredKeys := []string{
		"running", "mode", "tickCount", "tickErrorCount",
		"startTime", "runtimeSeconds", "todayPnl", "todayClosedCount",
		"openPositionCount", "totalWalletBalance", "totalUnrealizedPnl",
		"totalMarginBalance", "availableBalance",
		"topN", "cooldownMin", "marginMode",
	}
	for _, key := range requiredKeys {
		if _, ok := data[key]; !ok {
			t.Errorf("GetDashboardData 缺少键 %q", key)
		}
	}

	// DRY_RUN 模式下余额应为模拟值 10000
	if bal, ok := data["totalWalletBalance"].(float64); !ok || bal != 10000.0 {
		t.Errorf("DRY_RUN 钱包余额 = %v, 期望 10000", data["totalWalletBalance"])
	}

	// 未启动时 running 应为 false
	if running, ok := data["running"].(bool); !ok || running {
		t.Errorf("未启动时 running = %v, 期望 false", data["running"])
	}
}

// TestGetDashboardData_Running 验证策略运行中时仪表盘数据正确
// 验证点: 启动策略后 running=true, tickCount>=0
func TestGetDashboardData_Running(t *testing.T) {
	svc := newTestService(t)

	svc.StartStrategy()
	defer svc.StopStrategy()

	data := svc.GetDashboardData()
	if running, ok := data["running"].(bool); !ok || !running {
		t.Errorf("运行中 running = %v, 期望 true", data["running"])
	}
	if mode, ok := data["mode"].(string); !ok || mode != "DRY_RUN" {
		t.Errorf("mode = %v, 期望 DRY_RUN", data["mode"])
	}
}

// TestGetClosedPositions_Empty 验证空库查询平仓记录返回空切片
func TestGetClosedPositions_Empty(t *testing.T) {
	svc := newTestService(t)

	positions, err := svc.GetClosedPositions(10)
	if err != nil {
		t.Fatalf("GetClosedPositions 返回错误: %v", err)
	}
	if len(positions) != 0 {
		t.Errorf("空库平仓记录数 = %d, 期望 0", len(positions))
	}
}

// ==================== 八、Shutdown 测试 ====================

// TestShutdown 验证 Shutdown 不 panic 且 db 被置为 nil
// 验证点: 调用 Shutdown 无 panic、Shutdown 后 s.db == nil
func TestShutdown(t *testing.T) {
	svc := newTestService(t)

	// 调用 Shutdown 不应 panic
	svc.Shutdown()

	// 验证 db 被置为 nil
	if svc.db != nil {
		t.Error("Shutdown 后 db 应为 nil")
	}
}

// TestShutdown_DoubleCall 模拟应用正常退出时 OnShutdown 回调 + defer 双重调用场景
// 背景: 修复前 main.go 同时注册 defer quantService.Shutdown() 与 app.OnShutdown，
// Wails v3 退出时序为: cleanup() 先同步执行 OnShutdown 回调（第一次 Shutdown），
// Run() 返回后 main() 的 defer 再次执行 Shutdown（第二次）。第二次调用走到
// WsManager.Stop() 对已关闭的 stopCh 重复 close，触发 "close of closed channel" panic。
// 修复后 WsManager.Stop() 由 stopOnce sync.Once 保证幂等。
// 验证点: 连续两次调用 Shutdown 均不 panic，db 已释放
func TestShutdown_DoubleCall(t *testing.T) {
	svc := newTestService(t)

	// ① 模拟 cleanup() 中 OnShutdown 回调（第一次 Shutdown，close(stopCh) 成功）
	svc.Shutdown()
	// ② 模拟 Run() 返回后 main() 的 defer（第二次 Shutdown，修复前此处 panic）
	svc.Shutdown()
	// ③ 再补一次，验证多次调用仍安全
	svc.Shutdown()

	if svc.db != nil {
		t.Error("Shutdown 后 db 应为 nil")
	}
}

// ==================== 九、健康监控测试 ====================

// TestHealthCheck_Normal 验证正常运行时健康检查不触发修复
// 验证点: 策略运行中调用 runHealthCheck 不 panic，状态不变
func TestHealthCheck_Normal(t *testing.T) {
	svc := newTestService(t)

	svc.StartStrategy()
	defer svc.StopStrategy()

	// 健康检查不应 panic
	svc.runHealthCheck()

	// 策略应仍在运行
	if !svc.started {
		t.Error("健康检查后策略应仍在运行")
	}
}

// TestHealthCheck_EngineCrash 验证引擎崩溃后自动重启
// 验证点: 手动停止引擎但保持 started=true，健康检查应自动重启
func TestHealthCheck_EngineCrash(t *testing.T) {
	svc := newTestService(t)

	svc.StartStrategy()
	defer svc.StopStrategy()

	// 模拟引擎崩溃：直接停止引擎但不更新 started 标记
	svc.mu.Lock()
	if svc.engine != nil {
		svc.engine.Stop()
	}
	// started 仍为 true，模拟崩溃场景
	svc.mu.Unlock()

	// 等待引擎 goroutine 退出
	time.Sleep(100 * time.Millisecond)

	// 执行健康检查，应触发自动重启
	svc.runHealthCheck()

	// 验证策略已重启
	svc.mu.RLock()
	running := svc.started
	engineAlive := svc.engine != nil && svc.engine.IsRunning()
	svc.mu.RUnlock()

	if !running {
		t.Error("自动重启后 started 应为 true")
	}
	if !engineAlive {
		t.Error("自动重启后引擎应处于运行状态")
	}
}

// TestHealthCheck_NotStarted 验证策略未启动时健康检查不触发重启
// 验证点: started=false 时 runHealthCheck 不应启动策略
func TestHealthCheck_NotStarted(t *testing.T) {
	svc := newTestService(t)

	// 未启动策略，执行健康检查
	svc.runHealthCheck()

	// 策略不应被自动启动
	if svc.started {
		t.Error("策略未启动时，健康检查不应自动启动策略")
	}
}

// TestLogHealthEvent 验证健康事件写入日志表
// 验证点: logHealthEvent 写入后可通过 GetLogs 查询到
func TestLogHealthEvent(t *testing.T) {
	svc := newTestService(t)

	svc.logHealthEvent("warn", "测试健康事件")

	logs, err := svc.GetLogs(10)
	if err != nil {
		t.Fatalf("GetLogs 失败: %v", err)
	}
	found := false
	for _, l := range logs {
		if l.Module == "health_monitor" && l.Message == "测试健康事件" {
			found = true
			break
		}
	}
	if !found {
		t.Error("日志表中未找到健康监控事件记录")
	}
}

// ==================== 十、新增配置字段测试 ====================

// TestGetDashboardData_NewFields 验证仪表盘数据包含新增配置字段及正确默认值
// 验证点: topN=10, cooldownMin=60, marginMode="ISOLATED"（S01 纯追涨默认配置值）
func TestGetDashboardData_NewFields(t *testing.T) {
	svc := newTestService(t)

	data := svc.GetDashboardData()

	// 验证 topN 字段存在且为默认值 10
	topN, ok := data["topN"]
	if !ok {
		t.Fatal("GetDashboardData 缺少键 topN")
	}
	if topN != 10 {
		t.Errorf("topN = %v, 期望 10", topN)
	}

	// 验证 cooldownMin 字段存在且为默认值 60
	cooldownMin, ok := data["cooldownMin"]
	if !ok {
		t.Fatal("GetDashboardData 缺少键 cooldownMin")
	}
	if cooldownMin != 60 {
		t.Errorf("cooldownMin = %v, 期望 60", cooldownMin)
	}

	// 验证 marginMode 字段存在且为默认值 ISOLATED
	marginMode, ok := data["marginMode"]
	if !ok {
		t.Fatal("GetDashboardData 缺少键 marginMode")
	}
	if marginMode != "ISOLATED" {
		t.Errorf("marginMode = %v, 期望 ISOLATED", marginMode)
	}
}

// TestSetConfig_NewFields 验证通过 SetConfig 设置新增字段后 GetConfig 返回正确值
// 验证点: 设置 TopN=5, CooldownMin=30, MarginMode="CROSSED" 后读取一致
func TestSetConfig_NewFields(t *testing.T) {
	svc := newTestService(t)

	cfg := svc.GetConfig()
	cfg.TopN = 5
	cfg.CooldownMin = 30
	cfg.MarginMode = "CROSSED"

	result := svc.SetConfig(cfg)
	if !strings.Contains(result, "已更新") {
		t.Fatalf("SetConfig 返回 %q, 期望包含 %q", result, "已更新")
	}

	got := svc.GetConfig()
	if got.TopN != 5 {
		t.Errorf("TopN = %d, 期望 5", got.TopN)
	}
	if got.CooldownMin != 30 {
		t.Errorf("CooldownMin = %d, 期望 30", got.CooldownMin)
	}
	if got.MarginMode != "CROSSED" {
		t.Errorf("MarginMode = %q, 期望 CROSSED", got.MarginMode)
	}
}

// TestGetConfig_Defaults 验证 GetConfig 返回所有新增字段的正确默认值
// 验证点: TopN=10, CooldownMin=60, MarginMode="ISOLATED"（S01 纯追涨默认配置值）
func TestGetConfig_Defaults(t *testing.T) {
	svc := newTestService(t)

	cfg := svc.GetConfig()

	if cfg.TopN != 10 {
		t.Errorf("默认 TopN = %d, 期望 10", cfg.TopN)
	}
	if cfg.CooldownMin != 60 {
		t.Errorf("默认 CooldownMin = %d, 期望 60", cfg.CooldownMin)
	}
	if cfg.MarginMode != "ISOLATED" {
		t.Errorf("默认 MarginMode = %q, 期望 ISOLATED", cfg.MarginMode)
	}
	// S01 新增退出机制默认值
	if cfg.TakeProfitPct != 0 {
		t.Errorf("默认 TakeProfitPct = %f, 期望 0（纯跟踪，固定止盈关闭）", cfg.TakeProfitPct)
	}
	if cfg.MaxHoldMin != 120 {
		t.Errorf("默认 MaxHoldMin = %d, 期望 120（最长持仓 120 分钟）", cfg.MaxHoldMin)
	}
	if cfg.EnableShort {
		t.Errorf("默认 EnableShort = true, 期望 false（S01 纯追涨只做多）")
	}
	if !cfg.EnableAddOn {
		t.Errorf("默认 EnableAddOn = false, 期望 true（追加仓位开启）")
	}
	// 同时验证其他核心默认值未被破坏
	if cfg.ScanIntervalSec != 15 {
		t.Errorf("默认 ScanIntervalSec = %d, 期望 15", cfg.ScanIntervalSec)
	}
	if cfg.Timeframe != "15m" {
		t.Errorf("默认 Timeframe = %q, 期望 15m", cfg.Timeframe)
	}
	if cfg.MaxOpenPositions != 10 {
		t.Errorf("默认 MaxOpenPositions = %d, 期望 10", cfg.MaxOpenPositions)
	}
	if cfg.SignalMode != "kline" {
		t.Errorf("默认 SignalMode = %q, 期望 kline", cfg.SignalMode)
	}
	if cfg.MaxPullbackPct != 9.0 {
		t.Errorf("默认 MaxPullbackPct = %f, 期望 9.0", cfg.MaxPullbackPct)
	}
}

// ==================== 十一、集成测试 ====================

// TestIntegration_ConfigToEngine 验证配置从 Service 层设置后能正确传递到引擎与仪表盘
// 流程: SetConfig(TopN=2, CooldownMin=30, MarginMode=CROSSED) → GetConfig 验证一致 →
// GetDashboardData 验证仪表盘反映新配置值
func TestIntegration_ConfigToEngine(t *testing.T) {
	svc := newTestService(t)

	// 设置新配置
	cfg := svc.GetConfig()
	cfg.TopN = 2
	cfg.CooldownMin = 30
	cfg.MarginMode = binance.MarginModeCross

	result := svc.SetConfig(cfg)
	if !strings.Contains(result, "已更新") {
		t.Fatalf("SetConfig 返回 %q, 期望包含 %q", result, "已更新")
	}

	// 验证 GetConfig 返回一致的值
	got := svc.GetConfig()
	if got.TopN != 2 {
		t.Errorf("GetConfig TopN = %d, 期望 2", got.TopN)
	}
	if got.CooldownMin != 30 {
		t.Errorf("GetConfig CooldownMin = %d, 期望 30", got.CooldownMin)
	}
	if got.MarginMode != binance.MarginModeCross {
		t.Errorf("GetConfig MarginMode = %q, 期望 %q", got.MarginMode, binance.MarginModeCross)
	}

	// 验证 GetDashboardData 反映新配置
	data := svc.GetDashboardData()
	if data["topN"] != 2 {
		t.Errorf("Dashboard topN = %v, 期望 2", data["topN"])
	}
	if data["cooldownMin"] != 30 {
		t.Errorf("Dashboard cooldownMin = %v, 期望 30", data["cooldownMin"])
	}
	if data["marginMode"] != binance.MarginModeCross {
		t.Errorf("Dashboard marginMode = %v, 期望 %q", data["marginMode"], binance.MarginModeCross)
	}
}

// TestIntegration_SetConfigWhileRunning 验证策略运行中禁止修改配置，停止后允许修改
// 流程: StartStrategy(DRY_RUN) → SetConfig 返回错误 → StopStrategy → SetConfig 成功
func TestIntegration_SetConfigWhileRunning(t *testing.T) {
	svc := newTestService(t)

	// 启动策略（DRY_RUN 模式）
	startResult := svc.StartStrategy()
	if !strings.Contains(startResult, "成功") {
		t.Fatalf("StartStrategy 返回 %q, 期望包含 %q", startResult, "成功")
	}
	defer svc.StopStrategy()

	// 运行中修改配置应被拒绝
	cfg := svc.GetConfig()
	cfg.TopN = 99
	result := svc.SetConfig(cfg)
	if result != "策略运行中，请先停止再修改配置" {
		t.Errorf("运行中 SetConfig 返回 %q, 期望 %q", result, "策略运行中，请先停止再修改配置")
	}

	// 验证配置未被修改
	got := svc.GetConfig()
	if got.TopN == 99 {
		t.Error("运行中配置不应被修改")
	}

	// 停止策略
	stopResult := svc.StopStrategy()
	if !strings.Contains(stopResult, "成功") {
		t.Fatalf("StopStrategy 返回 %q, 期望包含 %q", stopResult, "成功")
	}

	// 停止后修改配置应成功
	result = svc.SetConfig(cfg)
	if !strings.Contains(result, "已更新") {
		t.Errorf("停止后 SetConfig 返回 %q, 期望包含 %q", result, "已更新")
	}

	// 验证配置已更新
	got = svc.GetConfig()
	if got.TopN != 99 {
		t.Errorf("停止后 TopN = %d, 期望 99", got.TopN)
	}
}

// TestIntegration_FullLifecycle 验证完整生命周期：设置凭据→启动→验证运行→停止→验证停止→修改配置→验证配置
// 流程: SetCredentials(SIMULATION) → StartStrategy → running=true → StopStrategy →
// running=false → SetConfig → 配置变更生效
func TestIntegration_FullLifecycle(t *testing.T) {
	svc := newTestService(t)

	// 将模式设为 SIMULATION 以避免 SetCredentials 触发数据库切换（保持使用临时 DB）
	svc.mode = "SIMULATION"

	// 1. 设置凭据（SIMULATION 模式）
	credResult := svc.SetCredentials("SIMULATION", "test-key", "test-secret")
	if !strings.Contains(credResult, "SIMULATION") {
		t.Fatalf("SetCredentials 返回 %q, 期望包含 %q", credResult, "SIMULATION")
	}

	// 2. 启动策略
	startResult := svc.StartStrategy()
	if !strings.Contains(startResult, "成功") {
		t.Fatalf("StartStrategy 返回 %q, 期望包含 %q", startResult, "成功")
	}

	// 3. 验证运行状态
	status := svc.GetStrategyStatus()
	if running, ok := status["running"].(bool); !ok || !running {
		t.Errorf("启动后 running = %v, 期望 true", status["running"])
	}

	// 4. 停止策略
	stopResult := svc.StopStrategy()
	if !strings.Contains(stopResult, "成功") {
		t.Fatalf("StopStrategy 返回 %q, 期望包含 %q", stopResult, "成功")
	}

	// 5. 验证停止状态
	status = svc.GetStrategyStatus()
	if running, ok := status["running"].(bool); !ok || running {
		t.Errorf("停止后 running = %v, 期望 false", status["running"])
	}

	// 6. 修改配置
	cfg := svc.GetConfig()
	cfg.TopN = 7
	cfg.CooldownMin = 15
	cfg.MarginMode = binance.MarginModeIsolated
	cfgResult := svc.SetConfig(cfg)
	if !strings.Contains(cfgResult, "已更新") {
		t.Fatalf("SetConfig 返回 %q, 期望包含 %q", cfgResult, "已更新")
	}

	// 7. 验证配置变更生效
	got := svc.GetConfig()
	if got.TopN != 7 {
		t.Errorf("TopN = %d, 期望 7", got.TopN)
	}
	if got.CooldownMin != 15 {
		t.Errorf("CooldownMin = %d, 期望 15", got.CooldownMin)
	}
	if got.MarginMode != binance.MarginModeIsolated {
		t.Errorf("MarginMode = %q, 期望 %q", got.MarginMode, binance.MarginModeIsolated)
	}
}

// ==================== 十二、配置持久化（应用到项目）测试 ====================

// TestPersistConfig_Roundtrip 验证 PersistConfig 写库回读一致（「应用到项目」）
// 验证点: 持久化后 GetConfig 返回新配置；库中 strategy:cfg 反序列化结果与提交一致
// （该键是应用重启后 Init 自动加载的数据源，保证重启后配置不丢失）
func TestPersistConfig_Roundtrip(t *testing.T) {
	svc := newTestService(t)

	// 提交一份与默认不同的配置
	cfg := binance.DefaultStrategyConfig()
	cfg.MaxOpenPositions = 7
	cfg.EnableAddOn = false
	cfg.TakeProfitPct = 0.1

	msg := svc.PersistConfig(cfg)
	if !strings.Contains(msg, "应用到项目") {
		t.Errorf("PersistConfig 返回 %q, 期望包含 %q", msg, "应用到项目")
	}

	// 内存配置同步更新
	if got := svc.GetConfig(); got != cfg {
		t.Errorf("GetConfig 配置 = %+v, 期望 %+v", got, cfg)
	}

	// 库中数据可反序列化为相同配置（模拟重启后 Init 加载的数据源）
	raw, err := svc.db.GetKeyValue(strategyCfgKey)
	if err != nil {
		t.Fatalf("读取持久化配置失败: %v", err)
	}
	var loaded binance.StrategyConfig
	if err := json.Unmarshal([]byte(raw), &loaded); err != nil {
		t.Fatalf("反序列化持久化配置失败: %v", err)
	}
	if loaded != cfg {
		t.Errorf("库中配置 = %+v, 期望 %+v", loaded, cfg)
	}
}

// TestPersistConfig_Running 验证策略运行中持久化不中断运行，提示下次启动生效
// 验证点: started=true 时 PersistConfig 仍成功写库，返回提示包含"下次启动"
func TestPersistConfig_Running(t *testing.T) {
	svc := newTestService(t)

	svc.started = true
	defer func() { svc.started = false }()

	cfg := binance.DefaultStrategyConfig()
	cfg.MaxOpenPositions = 6

	msg := svc.PersistConfig(cfg)
	if !strings.Contains(msg, "下次启动") {
		t.Errorf("运行中 PersistConfig 返回 %q, 期望提示下次启动生效", msg)
	}

	// 配置仍已持久化
	raw, err := svc.db.GetKeyValue(strategyCfgKey)
	if err != nil || raw == "" {
		t.Fatalf("运行中持久化失败: raw=%q err=%v", raw, err)
	}
}
