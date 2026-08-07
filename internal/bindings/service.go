// Package bindings 提供 Wails 绑定层，暴露给前端调用
package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/order"
	"quant-desktop/internal/risk"
	"quant-desktop/internal/storage"
	"quant-desktop/internal/strategy"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// QuantService 量化交易服务（暴露给前端）
type QuantService struct {
	db        *storage.DB
	client    *binance.Client
	ws        *binance.WsManager
	engine    *strategy.Engine
	orderMgr  *order.Manager
	breaker   *risk.CircuitBreaker
	cfg       binance.StrategyConfig
	ctx       context.Context
	cancel    context.CancelFunc
	mu        sync.RWMutex
	started   bool
	engineWG  sync.WaitGroup // 等待策略引擎 goroutine 退出（Shutdown 时汇合，避免 db.Close 与 runOnce 竞态）
	mode      string // 运行模式：SIMULATION | LIVE
	apiKey    string // 币安 API Key（运行时内存）
	apiSecret string // 币安 API Secret（运行时内存）
	proxyAddr string // 用户指定的代理地址
	proxyPort int    // 用户指定的代理端口
	app       *application.App // Wails 应用引用（用于事件推送）
}

// strategyCfgKey strategy_config 表中持久化策略配置的键名
// 整份 StrategyConfig 以 JSON 序列化后存储，按当前模式库隔离（SIMULATION/LIVE 各自独立）
const strategyCfgKey = "strategy:cfg"

// migratePersistedStrategyConfig 对旧版持久化策略配置执行升级迁移。
// 处理两类历史数据：
//  1. 新币过滤字段缺失（旧版 JSON 无 enableNewListingFilter 键）：补默认值（开启 + 60 天）；
//  2. 最小成交额仍为旧值 10 万 USDT（2026-08-07 用户要求 10 万 → 1000 万）：升级为 1000 万。
//
// 用户显式保存的其他值不受影响（仅精确匹配旧默认值才迁移）。
// 返回: 是否发生迁移、迁移后的配置、解析错误（JSON 无法解析时返回错误，由调用方回退默认值）。
func migratePersistedStrategyConfig(raw string) (bool, binance.StrategyConfig, error) {
	var saved binance.StrategyConfig
	if err := json.Unmarshal([]byte(raw), &saved); err != nil {
		return false, saved, err
	}
	migrated := false
	// 新币过滤字段迁移：旧持久化配置 JSON 缺少新键（升级前保存），
	// 补上默认值（开启 + 60 天），保证升级后保护自动生效；
	// 用户显式保存的 false/0（JSON 含新键）不会被覆盖。
	if !bytes.Contains([]byte(raw), []byte(`"enableNewListingFilter"`)) {
		dft := binance.DefaultStrategyConfig()
		saved.EnableNewListingFilter = dft.EnableNewListingFilter
		saved.NewListingMinDays = dft.NewListingMinDays
		migrated = true
	}
	// 最小成交额参数迁移：旧持久化配置中 24h 成交额下限仍为旧的 10 万 USDT 时，
	// 升级为 1000 万 USDT（2026-08-07 用户要求）；用户显式保存的其他值不会被覆盖。
	if saved.MinQuoteVolume == 100000 {
		saved.MinQuoteVolume = 10000000
		migrated = true
	}
	return migrated, saved, nil
}

// defaultProxyAddr / defaultProxyPort 内置默认代理配置（用户设定 2026-08-05）。
// 新环境（如 Windows 首装）库中无保存代理时自动使用；不可达时 NewClient 自动
// 沿内置候选链（本地 10808 → 远端 45.251.241.89 → 本地检测）继续探测，不会因代理而死。
const (
	defaultProxyAddr = "127.0.0.1"
	defaultProxyPort = 10808
)

// NewQuantService 创建量化服务实例
func NewQuantService() *QuantService {
	return &QuantService{
		cfg:  binance.DefaultStrategyConfig(),
		mode: "SIMULATION",
	}
}

// SetApp 注入 Wails 应用引用（在 main.go 中创建 app 后调用）
// 用于后台错误事件推送到前端弹窗
func (s *QuantService) SetApp(app *application.App) {
	s.app = app
}

// emitBackendError 通过 Wails 事件推送后台错误到前端
// context: 操作上下文（如"开仓"、"挂止损单"、"平仓"）
// message: 错误详情
func (s *QuantService) emitBackendError(errContext, message string) {
	if s.app == nil {
		return
	}
	s.app.Event.EmitEvent(&application.CustomEvent{
		Name: "backend-error",
		Data: map[string]string{
			"context": errContext,
			"message": message,
		},
	})
}

// Init 初始化服务（在应用启动时调用）
func (s *QuantService) Init() error {
	// 初始化数据库（按模式使用独立文件：quant_simulation.db / quant_live.db）
	dbPath := storage.DBPathForMode("data", s.mode)
	db, err := storage.NewDB(dbPath)
	if err != nil {
		return err
	}
	s.db = db
	log.Printf("[Binding] 使用数据库: %s", dbPath)

	// 加载上次「应用到项目」保存的策略配置（如有），覆盖默认值；
	// 各模式数据库独立，持久化配置天然按模式隔离
	if v, err := db.GetKeyValue(strategyCfgKey); err == nil && v != "" {
		migrated, migratedCfg, err := migratePersistedStrategyConfig(v)
		if err != nil {
			log.Printf("[Binding] 持久化策略配置解析失败（使用默认值）: %v", err)
		} else {
			s.cfg = migratedCfg
			// 升级迁移发生（新币过滤缺键 / 旧最小成交额 10 万）时回写数据库保持存储一致
			if migrated {
				if data, err := json.Marshal(migratedCfg); err == nil {
					if err := db.SetKeyValue(strategyCfgKey, string(data)); err != nil {
						log.Printf("[Binding] 回写迁移后策略配置失败: %v", err)
					}
				}
				log.Printf("[Binding] 已迁移持久化策略配置（最小成交额 → 1000 万 USDT 等）")
			}
			log.Printf("[Binding] 已加载持久化策略配置（应用到项目）")
		}
	}

	// 加载保存的代理配置；库中无保存值时使用内置默认代理（用户设定 2026-08-05）。
	// 默认代理在新环境（如 Windows 首装）首次启动即自动生效；
	// 若默认代理不可达，NewClient 会自动回退本地代理检测，不会因代理配置而死。
	proxyAddr, proxyPort, err := db.LoadProxyConfig()
	if err != nil || proxyAddr == "" || proxyPort <= 0 {
		if err != nil {
			log.Printf("[Binding] 加载代理配置失败（使用内置默认代理 %s:%d）: %v", defaultProxyAddr, defaultProxyPort, err)
		}
		s.proxyAddr = defaultProxyAddr
		s.proxyPort = defaultProxyPort
	} else {
		s.proxyAddr = proxyAddr
		s.proxyPort = proxyPort
		log.Printf("[Binding] 已加载代理配置: %s:%d", proxyAddr, proxyPort)
	}

	// 加载当前模式保存的凭据（如有）
	if savedKey, savedSecret, err := db.LoadCredentials(s.mode); err == nil {
		if savedKey != "" {
			s.apiKey = savedKey
			s.apiSecret = savedSecret
			log.Printf("[Binding] 已加载模式 %s 的保存凭据", s.mode)
		}
	}

	// 初始化币安客户端（按当前模式 + 代理配置）
	s.client = binance.NewClient(s.apiKey, s.apiSecret, s.mode, s.proxyAddr, s.proxyPort)
	s.ws = binance.NewWsManager(s.mode)

	// 初始化委托管理器
	s.orderMgr = order.NewManager(s.client, db)

	// 初始化熔断器（基于当前配置；启动策略时会按最新配置重建）
	// 注意：配置中的百分比需转小数（如 5.0 表示 5% -> 0.05）
	s.breaker = risk.NewCircuitBreaker(s.cfg.DailyLossLimitPct/100, s.cfg.MaxDrawdownPct/100, 5)

	s.ctx, s.cancel = context.WithCancel(context.Background())

	// 启动健康监控（每 30 分钟自动检查 + 修复）
	s.startHealthMonitor()

	return nil
}

// SetCredentials 设置运行模式与 API 密钥（运行时配置）
// mode: 运行模式 SIMULATION | LIVE
// apiKey: 币安 API Key
// apiSecret: 币安 API Secret
// 返回: 操作结果提示。策略运行中时禁止切换，需先停止。
// 模式切换时会自动切换到对应的独立数据库文件（数据完全隔离）。
func (s *QuantService) SetCredentials(mode, apiKey, apiSecret string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return "策略运行中，请先停止再切换模式"
	}
	if mode != "SIMULATION" && mode != "LIVE" {
		return "无效的运行模式"
	}

	modeChanged := s.mode != mode
	s.mode = mode
	// 清理首尾空白字符（Windows 复制粘贴易带入空格/换行，币安视为格式无效 -2014）
	s.apiKey = strings.TrimSpace(apiKey)
	s.apiSecret = strings.TrimSpace(apiSecret)

	// 模式切换时：关闭旧数据库，打开新模式对应的独立数据库
	if modeChanged {
		if s.db != nil {
			s.db.Close()
		}
		dbPath := storage.DBPathForMode("data", mode)
		newDB, err := storage.NewDB(dbPath)
		if err != nil {
			log.Printf("[Binding] 切换数据库失败: %v", err)
			return "切换数据库失败: " + err.Error()
		}
		s.db = newDB
		// 重建委托管理器（依赖 db）
		s.orderMgr = order.NewManager(s.client, newDB)
		log.Printf("[Binding] 已切换数据库: %s", dbPath)
	}

	// 持久化凭据到当前模式的数据库（加密存储）
	if apiKey != "" || apiSecret != "" {
		if err := s.db.SaveCredentials(mode, apiKey, apiSecret); err != nil {
			log.Printf("[Binding] 保存凭据失败: %v", err)
		}
	}

	// 按新模式重建客户端与 WS 管理器（带代理配置）
	s.client = binance.NewClient(apiKey, apiSecret, mode, s.proxyAddr, s.proxyPort)
	s.ws = binance.NewWsManager(mode)
	// 重建委托管理器（使用新 client）
	s.orderMgr = order.NewManager(s.client, s.db)

	s.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "binding",
		Message:   "运行模式已切换为 " + mode,
	})

	return "模式已切换为 " + mode
}

// GetSavedCredentials 获取指定模式已保存的凭据（解密后返回）
// mode: 运行模式
// 返回 map 包含 apiKey 和 apiSecret
// 注意：每个模式使用独立数据库，查询非当前模式时会临时打开对应数据库。
func (s *QuantService) GetSavedCredentials(mode string) map[string]string {
	empty := map[string]string{"apiKey": "", "apiSecret": ""}

	s.mu.RLock()
	defer s.mu.RUnlock()

	// 如果查询的是当前模式，直接从当前 db 读取
	if mode == s.mode {
		if s.db == nil {
			return empty
		}
		apiKey, apiSecret, err := s.db.LoadCredentials(mode)
		if err != nil {
			log.Printf("[Binding] 加载凭据失败: %v", err)
			return empty
		}
		return map[string]string{"apiKey": apiKey, "apiSecret": apiSecret}
	}

	// 查询非当前模式：临时打开对应模式的数据库
	dbPath := storage.DBPathForMode("data", mode)
	tempDB, err := storage.NewDB(dbPath)
	if err != nil {
		log.Printf("[Binding] 打开模式 %s 数据库失败: %v", mode, err)
		return empty
	}
	defer tempDB.Close()

	apiKey, apiSecret, err := tempDB.LoadCredentials(mode)
	if err != nil {
		log.Printf("[Binding] 加载模式 %s 凭据失败: %v", mode, err)
		return empty
	}
	return map[string]string{"apiKey": apiKey, "apiSecret": apiSecret}
}

// SetProxyConfig 保存代理配置并重建客户端
// address: 代理服务器地址
// port: 代理端口号
// 返回: 操作结果提示
func (s *QuantService) SetProxyConfig(address string, port int) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return "策略运行中，请先停止再修改代理配置"
	}

	s.proxyAddr = address
	s.proxyPort = port

	// 持久化代理配置
	if err := s.db.SaveProxyConfig(address, port); err != nil {
		log.Printf("[Binding] 保存代理配置失败: %v", err)
		return "保存代理配置失败: " + err.Error()
	}

	// 重建客户端以应用新代理
	s.client = binance.NewClient(s.apiKey, s.apiSecret, s.mode, s.proxyAddr, s.proxyPort)

	if address != "" && port > 0 {
		log.Printf("[Binding] 代理已设置为 %s:%d", address, port)
		return fmt.Sprintf("代理已设置为 %s:%d", address, port)
	}
	log.Printf("[Binding] 代理已清除，使用自动检测")
	return "代理已清除，使用自动检测"
}

// GetProxyConfig 获取当前代理配置
// 返回 map 包含 address 和 port
func (s *QuantService) GetProxyConfig() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return map[string]interface{}{
		"address": s.proxyAddr,
		"port":    s.proxyPort,
	}
}

// TestConnection 测试当前 API Key 认证是否可用（只读，绝不下单）。
// 直接以标准签名请求账户只读接口，返回完整诊断结果（含 request ip）。
// 前端「测试连接」按钮调用，用于快速定位 Key 认证问题。
// 返回: map 包含 ok / mode / message
func (s *QuantService) TestConnection() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.client == nil {
		return map[string]string{"ok": "false", "message": "客户端未初始化"}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	return s.client.TestConnection(ctx)
}

// GetMode 获取当前运行模式
func (s *QuantService) GetMode() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.mode
}

// StartStrategy 启动策略
func (s *QuantService) StartStrategy() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return "策略已在运行中"
	}

	// 创建引擎（集成委托管理器）
	s.engine = strategy.NewEngine(s.cfg, s.client, s.ws, s.db, s.orderMgr)
	s.engine.SetOnError(s.emitBackendError)

	// 按最新配置重建熔断器（用户可能已修改 DailyLossLimitPct/MaxDrawdownPct），
	// 并注入引擎。配置中的百分比需转小数（如 5.0 表示 5% -> 0.05）。
	s.breaker = risk.NewCircuitBreaker(s.cfg.DailyLossLimitPct/100, s.cfg.MaxDrawdownPct/100, 5)
	if balance, err := s.client.GetFuturesBalance(s.ctx); err == nil && balance != nil {
		// 初始权益 = 钱包余额 + 未实现盈亏（账户回撤熔断的基准）
		s.breaker.SetInitialEquity(balance.TotalMarginBalance)
	}
	s.engine.SetBreaker(s.breaker)

	// 在 goroutine 中启动（WaitGroup 用于 Shutdown 时汇合，防止引擎退出前 db 被关闭）
	s.engineWG.Add(1)
	go func() {
		defer s.engineWG.Done()
		s.engine.Start(s.ctx)
	}()

	s.started = true
	s.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "binding",
		Message:   "策略已启动",
	})

	return "策略启动成功"
}

// StopStrategy 停止策略
func (s *QuantService) StopStrategy() string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if !s.started {
		return "策略未在运行"
	}

	if s.engine != nil {
		s.engine.Stop()
	}

	s.started = false
	s.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "binding",
		Message:   "策略已停止",
	})

	return "策略停止成功"
}

// GetStrategyStatus 获取策略运行状态
func (s *QuantService) GetStrategyStatus() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	tick := int64(0)
	if s.engine != nil {
		tick = s.engine.GetTickCount()
	}
	log.Printf("[Binding] GetStrategyStatus called: running=%v tick=%d", s.started, tick)

	status := map[string]interface{}{
		"running":   s.started,
		"tickCount": tick,
	}
	return status
}

// GetDashboardData 获取仪表盘聚合数据（单次调用返回全部展示数据）
// 聚合账户状态、权益、时间信息、错误统计和盈利数据，减少前端轮询次数
// 返回: map 包含全部仪表盘数据字段
func (s *QuantService) GetDashboardData() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()

	data := map[string]interface{}{
		"running": s.started,
		"mode":    s.mode,
	}

	// 引擎运行数据
	var tickCount, tickErrorCount int64
	var startTimeMs int64
	var runtimeSec int64
	if s.engine != nil {
		tickCount = s.engine.GetTickCount()
		tickErrorCount = s.engine.GetTickErrorCount()
		st := s.engine.GetStartTime()
		if !st.IsZero() {
			startTimeMs = st.UnixMilli()
			runtimeSec = int64(time.Since(st).Seconds())
		}
	}
	data["tickCount"] = tickCount
	data["tickErrorCount"] = tickErrorCount
	data["startTime"] = startTimeMs
	data["runtimeSeconds"] = runtimeSec
	data["scanIntervalSec"] = s.cfg.ScanIntervalSec
	data["timeframe"] = s.cfg.Timeframe
	data["topN"] = s.cfg.TopN
	data["cooldownMin"] = s.cfg.CooldownMin
	data["marginMode"] = s.cfg.MarginMode

	// 今日盈亏
	if s.db == nil {
		data["todayPnl"] = 0.0
		data["todayClosedCount"] = 0
	} else {
		todayPnl, closedCount, err := s.db.GetTodayPnl()
		if err != nil {
			log.Printf("[Binding] 查询今日盈亏失败: %v", err)
		}
		data["todayPnl"] = todayPnl
		data["todayClosedCount"] = closedCount
	}

	// 当前持仓数
	if s.db != nil {
		openPositions, err := s.db.GetOpenPositions()
		if err != nil {
			log.Printf("[Binding] 查询持仓失败: %v", err)
			data["openPositionCount"] = 0
		} else {
			data["openPositionCount"] = len(openPositions)
		}
	}

	// 账户余额：调用交易所 API（带超时保护）
	{
		ctx, cancel := context.WithTimeout(s.ctx, 3*time.Second)
		defer cancel()
		balance, err := s.client.GetFuturesBalance(ctx)
		if err != nil {
			log.Printf("[Binding] 获取账户余额失败: %v", err)
			data["totalWalletBalance"] = float64(0)
			data["totalUnrealizedPnl"] = float64(0)
			data["totalMarginBalance"] = float64(0)
			data["availableBalance"] = float64(0)
		} else {
			data["totalWalletBalance"] = balance.TotalWalletBalance
			data["totalUnrealizedPnl"] = balance.TotalUnrealizedPnl
			data["totalMarginBalance"] = balance.TotalMarginBalance
			data["availableBalance"] = balance.AvailableBalance
		}
	}

	return data
}

// GetClosedPositions 获取已平仓持仓记录
// limit: 返回条数上限
// 返回: 按平仓时间降序的持仓列表
func (s *QuantService) GetClosedPositions(limit int) ([]storage.Position, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	return s.db.GetClosedPositions(limit)
}

// GetConfig 获取当前策略配置
func (s *QuantService) GetConfig() binance.StrategyConfig {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.cfg
}

// SetConfig 更新策略配置
// cfg: 新的策略配置
// 返回: 操作结果提示。策略运行中时禁止修改，需先停止。
func (s *QuantService) SetConfig(cfg binance.StrategyConfig) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.started {
		return "策略运行中，请先停止再修改配置"
	}

	s.cfg = cfg
	return "配置已更新"
}

// PersistConfig 将策略配置持久化到当前模式数据库（前端「应用到项目」）
// cfg: 新的策略配置
// 返回: 操作结果提示。持久化仅写入数据库，不影响正在运行的策略，将在下次启动策略时生效；
// 应用重启后由 Init 自动加载该配置覆盖默认值。
func (s *QuantService) PersistConfig(cfg binance.StrategyConfig) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.db == nil {
		return "服务已关闭"
	}

	data, err := json.Marshal(cfg)
	if err != nil {
		return "配置序列化失败: " + err.Error()
	}
	if err := s.db.SetKeyValue(strategyCfgKey, string(data)); err != nil {
		log.Printf("[Binding] 持久化策略配置失败: %v", err)
		return "应用到项目失败: " + err.Error()
	}

	s.cfg = cfg
	log.Printf("[Binding] 策略配置已应用到项目（持久化）")
	if s.started {
		return "配置已应用到项目，将在下次启动策略时生效"
	}
	return "配置已应用到项目"
}

// GetPositions 获取所有持仓
func (s *QuantService) GetPositions() ([]storage.Position, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	return s.db.GetOpenPositions()
}

// GetLogs 获取最近日志
func (s *QuantService) GetLogs(limit int) ([]storage.TradeLog, error) {
	if limit <= 0 {
		limit = 100
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	return s.db.GetRecentLogs(limit)
}

// GetOrders 获取委托单列表
// statusFilter: 状态过滤（空=全部, "ACTIVE"=仅活跃, 其他=精确匹配）
// 返回: 按 created_at 降序的委托列表
func (s *QuantService) GetOrders(statusFilter string) ([]storage.Order, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	return s.db.GetAllOrders(statusFilter)
}

// GetOrderEvents 获取委托事件流水
// orderID: 委托ID（0=全部）
// limit: 条数上限（<=0 默认 100）
// 返回: 按 timestamp 降序的事件列表
func (s *QuantService) GetOrderEvents(orderID int64, limit int) ([]storage.OrderEvent, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return nil, fmt.Errorf("服务已关闭")
	}
	return s.db.GetOrderEvents(orderID, limit)
}

// CancelOrder 手动取消委托
// orderID: 本地委托 ID
// 返回: 操作结果提示
func (s *QuantService) CancelOrder(orderID int64) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 查询本地委托
	if s.db == nil {
		return "服务已关闭"
	}
	orders, err := s.db.GetActiveOrders()
	if err != nil {
		return "查询委托失败: " + err.Error()
	}

	var target *storage.Order
	for i := range orders {
		if orders[i].ID == orderID {
			target = &orders[i]
			break
		}
	}
	if target == nil {
		return "未找到活跃委托"
	}

	// 调用交易所取消
	if err := s.client.CancelOrder(s.ctx, target.Symbol, target.ExchangeOrderID); err != nil {
		return "取消失败: " + err.Error()
	}

	// 更新本地状态
	s.db.UpdateOrderStatus(target.ID, binance.OrderStatusCanceled, nil, nil)
	s.db.InsertOrderEvent(&storage.OrderEvent{
		OrderID:         target.ID,
		ExchangeOrderID: target.ExchangeOrderID,
		EventType:       storage.EventCanceled,
		OldStatus:       &target.Status,
		NewStatus:       strPtr(binance.OrderStatusCanceled),
		Message:         strPtr("用户手动取消"),
		Timestamp:       time.Now().UnixMilli(),
	})

	return "委托已取消"
}

// GetOrderSyncStatus 获取委托同步状态摘要
// 返回: {activeCount, lastSyncTime, lastSyncError}
func (s *QuantService) GetOrderSyncStatus() map[string]interface{} {
	if s.orderMgr == nil {
		return map[string]interface{}{
			"activeCount":   0,
			"lastSyncTime":  int64(0),
			"lastSyncError": "",
		}
	}
	return s.orderMgr.GetSyncStatus()
}

// strPtr 返回字符串指针（辅助函数）
// s: 输入字符串
// 返回: 指向 s 的指针
func strPtr(s string) *string {
	return &s
}

// Shutdown 关闭服务，释放资源
// 幂等：多次/并发调用安全。全程持有 s.mu 串行化与业务方法的 db 访问，
// 并等待引擎 goroutine 退出后再关闭 db，避免 db.Close 与运行中的 runOnce 竞态。
func (s *QuantService) Shutdown() {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.cancel != nil {
		s.cancel()
	}
	if s.engine != nil {
		s.engine.Stop()
	}
	// 等待引擎 goroutine 退出（引擎循环在 stopCh/ctx.Done 后退出，runOnce 有界执行，不会死锁）
	s.engineWG.Wait()
	if s.ws != nil {
		s.ws.Stop()
	}
	if s.db != nil {
		s.db.Close()
		s.db = nil
	}
}
