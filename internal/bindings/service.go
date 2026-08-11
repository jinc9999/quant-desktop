// Package bindings 提供 Wails 绑定层，暴露给前端调用
package bindings

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"sort"
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
	engineWG  sync.WaitGroup   // 等待策略引擎 goroutine 退出（Shutdown 时汇合，避免 db.Close 与 runOnce 竞态）
	mode      string           // 运行模式：SIMULATION | LIVE
	apiKey    string           // 币安 API Key（运行时内存）
	apiSecret string           // 币安 API Secret（运行时内存）
	proxyAddr string           // 用户指定的代理地址
	proxyPort int              // 用户指定的代理端口
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
	// 分原因冷却字段迁移：旧持久化配置缺少 cooldownAfterTrailingMin 键时补默认值（15 分钟）。
	// 数据依据：三年回测 1000万口径 trailcd=0 +6016U vs 统一 60 分 +5053U；实盘 tick 15 秒取 15 分钟
	// 既保留止盈后快速再入趋势的收益，又保留对 15 秒级极端追单的保护（止损后仍严格 60 分钟）。
	if !bytes.Contains([]byte(raw), []byte(`"cooldownAfterTrailingMin"`)) {
		dft := binance.DefaultStrategyConfig()
		saved.CooldownAfterTrailingMin = dft.CooldownAfterTrailingMin
		migrated = true
	}
	// 追加仓次数迁移：旧持久化配置缺少 maxAddOnsPerSymbol 键时补默认值（2 次 = 同币最多 3 仓）。
	// 若不补，反序列化后为 0 会导致追加仓静默失效（EnableAddOn=true 但追加次数为 0）。
	if !bytes.Contains([]byte(raw), []byte(`"maxAddOnsPerSymbol"`)) {
		dft := binance.DefaultStrategyConfig()
		saved.MaxAddOnsPerSymbol = dft.MaxAddOnsPerSymbol
		migrated = true
	}
	// 24h 涨幅排名过滤字段迁移：旧持久化配置缺少 rankMode/rankParam 键时补默认值（关闭 / 前 20%）。
	// 保证升级后旧配置行为不变；用户在新版本界面显式保存的值不会被覆盖。
	if !bytes.Contains([]byte(raw), []byte(`"rankMode"`)) {
		dft := binance.DefaultStrategyConfig()
		saved.RankMode = dft.RankMode
		saved.RankParam = dft.RankParam
		migrated = true
	}
	// 启动预热字段迁移：旧持久化配置缺少 warmupMin 键时补默认值（15 分钟）。
	// 放量确认依赖启动后本地成交量采样窗口（前 13+2 分钟），窗口未满时放量检查 fail-open；
	// 预热保护可避免启动初期少一道放量过滤（A/B 策略共用）。
	if !bytes.Contains([]byte(raw), []byte(`"warmupMin"`)) {
		dft := binance.DefaultStrategyConfig()
		saved.WarmupMin = dft.WarmupMin
		migrated = true
	}
	// 策略标识迁移：旧持久化配置缺少 strategyName/strategyVersion 键时补默认
	// （A/B 版构建期默认值不同，升级后各自显示自己的策略名与定版号）。
	if !bytes.Contains([]byte(raw), []byte(`"strategyName"`)) {
		dft := binance.DefaultStrategyConfig()
		saved.StrategyName = dft.StrategyName
		saved.StrategyVersion = dft.StrategyVersion
		migrated = true
	}
	// 最小成交额参数迁移：旧持久化配置中 24h 成交额下限仍为旧的 10 万 USDT 时，
	// 升级为 1000 万 USDT（2026-08-07 用户要求）；用户显式保存的其他值不会被覆盖。
	if saved.MinQuoteVolume == 100000 {
		saved.MinQuoteVolume = 10000000
		migrated = true
	}
	// S01 v2 参数迁移（2026-08-08 全参数矩阵定稿，三年回测 +20,259U / PF 1.66 / 回撤 5.8%）：
	// 旧默认值精确匹配才迁移为 v2 值；用户显式自定义的值（非旧默认）不被覆盖。
	if saved.StopLossPct == 0.06 {
		saved.StopLossPct = 0.04
		migrated = true
	}
	if saved.TrailingActivation == 0.03 {
		saved.TrailingActivation = 0.02
		migrated = true
	}
	if saved.TrailingCallback == 0.02 {
		saved.TrailingCallback = 0.03
		migrated = true
	}
	if saved.MaxHoldMin == 120 {
		saved.MaxHoldMin = 180
		migrated = true
	}
	if saved.CooldownMin == 60 {
		saved.CooldownMin = 30
		migrated = true
	}
	if saved.VolumeSurgeThreshold == 1.5 {
		saved.VolumeSurgeThreshold = 1.2
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

// syncServerTime 同步币安服务器时间偏移（签名请求防 -1021）。
// 本机时钟漂移（实测快 1 秒）时所有签名请求被拒，前端权益显示 0.00；
// 客户端重建（Init/切模式/改代理）后都要重同步。失败仅记录日志不阻断，
// 余额接口的 -1021 自愈路径会在下次轮询时重同步。
func (s *QuantService) syncServerTime() {
	if s.client == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.client.SyncServerTime(ctx); err != nil {
		log.Printf("[Binding] ⚠ 服务器时间同步失败: %v", err)
	}
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
	s.ws = binance.NewWsManagerWithProxy(s.mode, s.client.ProxyURL())
	// 同步服务器时间：本机时钟漂移会导致签名请求 -1021、余额显示 0.00
	s.syncServerTime()

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
	if s.db == nil {
		return "服务未初始化"
	}
	if mode != "SIMULATION" && mode != "LIVE" {
		return "无效的运行模式"
	}

	// 策略虽已标记停止，但旧引擎 goroutine 可能仍在退出（StopStrategy 不再阻塞等待）。
	// 切换数据库前必须等其完全退出，避免引擎并发读写被关闭的旧库。
	if !s.waitForEngineExit(5 * time.Second) {
		log.Printf("[Binding] 旧策略引擎在 5 秒内未完全退出，继续切换模式")
	}

	modeChanged := s.mode != mode
	// 清理首尾空白字符（Windows 复制粘贴易带入空格/换行，币安视为格式无效 -2014）
	apiKey = strings.TrimSpace(apiKey)
	apiSecret = strings.TrimSpace(apiSecret)

	// 模式切换时：先打开新模式数据库，成功后再切换，避免打开失败后旧库被关闭、
	// 模式字段已变更导致服务处于不一致状态。
	if modeChanged {
		dbPath := storage.DBPathForMode("data", mode)
		newDB, err := storage.NewDB(dbPath)
		if err != nil {
			log.Printf("[Binding] 切换数据库失败: %v", err)
			return "切换数据库失败: " + err.Error()
		}
		if s.db != nil {
			if closeErr := s.db.Close(); closeErr != nil {
				log.Printf("[Binding] 关闭旧数据库失败: %v", closeErr)
			}
		}
		s.db = newDB
		log.Printf("[Binding] 已切换数据库: %s", dbPath)
	}

	s.mode = mode
	s.apiKey = apiKey
	s.apiSecret = apiSecret

	// 持久化凭据到当前模式的数据库（加密存储）
	if apiKey != "" || apiSecret != "" {
		if err := s.db.SaveCredentials(mode, apiKey, apiSecret); err != nil {
			log.Printf("[Binding] 保存凭据失败: %v", err)
		}
	}

	// 停止旧 WS 行情循环（旧引擎已停止时才允许切换模式，这里不会影响新引擎）
	if s.ws != nil {
		s.ws.Stop()
	}

	// 按新模式重建客户端与 WS 管理器（带代理配置）
	s.client = binance.NewClient(apiKey, apiSecret, mode, s.proxyAddr, s.proxyPort)
	s.ws = binance.NewWsManagerWithProxy(mode, s.client.ProxyURL())
	// 同步服务器时间（新客户端 TimeOffset 归零，需重新同步）
	s.syncServerTime()
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
	if s.db == nil {
		return "服务未初始化"
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
	// 同步服务器时间（新客户端 TimeOffset 归零，需重新同步）
	s.syncServerTime()

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

// GetTickerLoadStatus 返回全量行情加载状态（供前端展示行情加载信息）
func (s *QuantService) GetTickerLoadStatus() map[string]string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	msg := ""
	if s.engine != nil {
		msg = s.engine.TickerLoadMsg()
	}
	return map[string]string{"message": msg}
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
func round1(v float64) float64 { return math.Round(v*10) / 10 }
func round0(v float64) float64 { return math.Round(v) }
func safePct(a, b int) float64 {
	if b <= 0 {
		return 0
	}
	return float64(a) / float64(b) * 100
}
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func strVal(m map[string]interface{}, k string) string {
	if m == nil {
		return ""
	}
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func floatVal(m map[string]interface{}, k string) float64 {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return v
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case string:
		var f float64
		fmt.Sscanf(v, "%f", &f)
		return f
	}
	return 0
}

func intVal(m map[string]interface{}, k string) int {
	if m == nil {
		return 0
	}
	switch v := m[k].(type) {
	case float64:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case string:
		var n int
		fmt.Sscanf(v, "%d", &n)
		return n
	}
	return 0
}

// SaveDailySummary 保存每日总结（按 模式+日期+类型 幂等，写审计日志）
func (s *QuantService) SaveDailySummary(input map[string]interface{}) map[string]interface{} {
	res := map[string]interface{}{"ok": false}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		res["message"] = "数据库未初始化"
		return res
	}
	mode, db := s.mode, s.db
	sum := &storage.DailySummary{
		Mode:         mode,
		SummaryDate:  strVal(input, "summaryDate"),
		SummaryType:  strVal(input, "summaryType"),
		MarketNotes:  strVal(input, "marketNotes"),
		CoinAnalysis: strVal(input, "coinAnalysis"),
		Suggestions:  strVal(input, "suggestions"),
		TodayPnl:     floatVal(input, "todayPnl"),
		WinRate:      floatVal(input, "winRate"),
		TradeCount:   intVal(input, "tradeCount"),
		Rating:       intVal(input, "rating"),
		FeatureJSON:  strVal(input, "featureJson"),
	}
	if sum.SummaryType == "" {
		sum.SummaryType = "daily"
	}
	if sum.FeatureJSON == "" {
		sum.FeatureJSON = "{}"
	}
	id, exists, err := db.SaveDailySummary(sum)
	if err != nil {
		res["message"] = "保存失败: " + err.Error()
		return res
	}
	action := "CREATE"
	if exists {
		action = "UPDATE"
	}
	_ = db.InsertAuditLog(mode, action, fmt.Sprintf("daily_summaries/%d", id), sum.SummaryDate)
	res["ok"] = true
	res["id"] = id
	res["message"] = "已保存"
	return res
}

// GetDailySummaries 查询每日总结（按时间范围 + 类型，限当前模式）
func (s *QuantService) GetDailySummaries(dateFrom, dateTo, summaryType string) map[string]interface{} {
	res := map[string]interface{}{"ok": false, "list": []interface{}{}}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		res["message"] = "数据库未初始化"
		return res
	}
	mode, db := s.mode, s.db
	list, err := db.GetDailySummaries(mode, dateFrom, dateTo, summaryType)
	if err != nil {
		res["message"] = err.Error()
		return res
	}
	items := make([]interface{}, 0, len(list))
	for i := range list {
		items = append(items, list[i])
	}
	res["ok"] = true
	res["mode"] = mode
	res["list"] = items
	return res
}

// GetDailySummaryByID 查询单条每日总结（限当前模式）
func (s *QuantService) GetDailySummaryByID(id int64) map[string]interface{} {
	res := map[string]interface{}{"ok": false}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		res["message"] = "数据库未初始化"
		return res
	}
	mode, db := s.mode, s.db
	item, err := db.GetDailySummaryByID(mode, id)
	if err != nil {
		res["message"] = "记录不存在或无权访问"
		return res
	}
	res["ok"] = true
	res["item"] = item
	return res
}

// DeleteDailySummary 软删除每日总结（写审计日志）
func (s *QuantService) DeleteDailySummary(id int64) map[string]interface{} {
	res := map[string]interface{}{"ok": false}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		res["message"] = "数据库未初始化"
		return res
	}
	mode, db := s.mode, s.db
	ok, err := db.DeleteDailySummary(mode, id)
	if err != nil || !ok {
		res["message"] = "删除失败或记录不存在"
		return res
	}
	_ = db.InsertAuditLog(mode, "DELETE", fmt.Sprintf("daily_summaries/%d", id), "")
	res["ok"] = true
	res["message"] = "已删除"
	return res
}

// GetDailySummary 生成「每日总结」：市场趋势 + 单币盈亏 + 改进建议
// computeMarketSummary 计算全市场 24h 概况（模拟盘/实盘共用）
func (s *QuantService) computeMarketSummary() map[string]interface{} {
	m := map[string]interface{}{}
	if s.client == nil {
		return m
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tickers, tErr := s.client.FetchTickers(ctx)
	if tErr != nil || len(tickers) == 0 {
		return m
	}
	up, down, total := 0, 0, 0
	var sumChg, sumVol float64
	chgs := make([]float64, 0, len(tickers))
	type mv struct {
		Symbol string
		Change float64
		Volume float64
	}
	gainers, losers := []mv{}, []mv{}
	for _, t := range tickers {
		total++
		sumChg += t.PriceChange
		sumVol += t.QuoteVolume
		chgs = append(chgs, t.PriceChange)
		if t.PriceChange > 0 {
			up++
		} else if t.PriceChange < 0 {
			down++
		}
		if t.QuoteVolume >= 1e7 && marketChangePlausible(t.PriceChange, t.QuoteVolume) {
			if len(gainers) < 8 || t.PriceChange > gainers[len(gainers)-1].Change {
				gainers = append(gainers, mv{t.Symbol, t.PriceChange, t.QuoteVolume})
				sort.Slice(gainers, func(i, j int) bool { return gainers[i].Change > gainers[j].Change })
				if len(gainers) > 8 {
					gainers = gainers[:8]
				}
			}
			if len(losers) < 8 || t.PriceChange < losers[len(losers)-1].Change {
				losers = append(losers, mv{t.Symbol, t.PriceChange, t.QuoteVolume})
				sort.Slice(losers, func(i, j int) bool { return losers[i].Change < losers[j].Change })
				if len(losers) > 8 {
					losers = losers[:8]
				}
			}
		}
	}
	median := 0.0
	if len(chgs) > 0 {
		sort.Float64s(chgs)
		median = chgs[len(chgs)/2]
	}
	gainList := make([]map[string]interface{}, 0, len(gainers))
	for _, g := range gainers {
		gainList = append(gainList, map[string]interface{}{"symbol": g.Symbol, "change": round2(g.Change), "volume": round0(g.Volume)})
	}
	lossList := make([]map[string]interface{}, 0, len(losers))
	for _, l := range losers {
		lossList = append(lossList, map[string]interface{}{"symbol": l.Symbol, "change": round2(l.Change), "volume": round0(l.Volume)})
	}
	return map[string]interface{}{
		"total":            total,
		"up":               up,
		"down":             down,
		"medianChange":     round2(median),
		"avgChange":        round2(sumChg / float64(maxInt(total, 1))),
		"totalQuoteVolume": round0(sumVol),
		"topGainers":       gainList,
		"topLosers":        lossList,
	}
}

// marketChangePlausible 展示层合理性校验：24h 涨幅离谱（|x|>1000%）必须有足够成交额支撑，
// 否则视为行情接口坏数据（低流动性币 24h 涨跌幅/成交额偶发异常，如 AZTEC +131300%），
// 不进涨跌幅榜。仅影响每日总结展示，不参与任何交易判定。
func marketChangePlausible(change, quoteVolume float64) bool {
	abs := math.Abs(change)
	if abs > 3000 {
		// 离谱涨幅（>3000%）一律视为行情接口坏数据/新上市噪音，不进涨跌幅榜。
		// 坏数据常连成交额一起虚高（AZTEC +65450% 案例），成交量门槛拦不住，必须硬封顶。
		return false
	}
	if abs > 1000 {
		// 千分级涨幅需 ≥ 1 亿 USDT 成交额才可信（真实币圈狂热行情的最低量级）
		return quoteVolume >= 1e8
	}
	return true
}

// computeModeSummary 计算指定模式的今日交易总结（单币聚合 + 规则化建议）
func computeModeSummary(db *storage.DB, mode string, market map[string]interface{}) map[string]interface{} {
	positions := []storage.Position{}
	if db != nil {
		positions, _ = db.GetTodayClosedPositions()
	}
	type coinAgg struct {
		Symbol  string
		Trades  int
		PnL     float64
		Wins    int
		HoldMin float64
		WinPct  float64
		LossPct float64
	}
	byCoin := map[string]*coinAgg{}
	order := []string{}
	for _, p := range positions {
		pnl := 0.0
		if p.RealizedPnl != nil {
			pnl = *p.RealizedPnl
		}
		ca, ok := byCoin[p.Symbol]
		if !ok {
			ca = &coinAgg{Symbol: p.Symbol}
			byCoin[p.Symbol] = ca
			order = append(order, p.Symbol)
		}
		ca.Trades++
		ca.PnL += pnl
		if pnl > 0 {
			ca.Wins++
		}
		if p.OpenedAt > 0 && p.ClosedAt != nil && *p.ClosedAt > p.OpenedAt {
			ca.HoldMin += float64(*p.ClosedAt-p.OpenedAt) / 60000
		}
		if p.EntryPrice > 0 && p.ExitPrice != nil && *p.ExitPrice > 0 {
			pct := (*p.ExitPrice - p.EntryPrice) / p.EntryPrice * 100
			if pnl > 0 {
				ca.WinPct += pct
			} else {
				ca.LossPct += pct
			}
		}
	}
	sort.Slice(order, func(i, j int) bool { return byCoin[order[i]].PnL > byCoin[order[j]].PnL })
	coinList := []map[string]interface{}{}
	for _, sym := range order {
		ca := byCoin[sym]
		coinList = append(coinList, map[string]interface{}{
			"symbol":     ca.Symbol,
			"trades":     ca.Trades,
			"pnl":        round2(ca.PnL),
			"winRate":    round1(safePct(ca.Wins, ca.Trades)),
			"avgHoldMin": round1(ca.HoldMin / float64(maxInt(ca.Trades, 1))),
			"avgWinPct":  round2(ca.WinPct / float64(maxInt(ca.Wins, 1))),
			"avgLossPct": round2(ca.LossPct / float64(maxInt(ca.Trades-ca.Wins, 1))),
		})
	}
	totalPnl, winCount, stopCount, trailCount, trailLoss := 0.0, 0, 0, 0, 0
	for _, p := range positions {
		pnl := 0.0
		if p.RealizedPnl != nil {
			pnl = *p.RealizedPnl
		}
		totalPnl += pnl
		if pnl > 0 {
			winCount++
		}
		if p.CloseReason != nil && *p.CloseReason == "STOP_LOSS" {
			stopCount++
		}
		if p.CloseReason != nil && *p.CloseReason == "TRAILING_STOP" {
			trailCount++
			if pnl <= 0 {
				trailLoss++
			}
		}
	}
	sugg := []string{}
	if len(positions) == 0 {
		sugg = append(sugg, "今日暂无已平仓交易，等待行情与信号即可。")
	} else {
		wr := safePct(winCount, len(positions))
		if wr < 45 {
			sugg = append(sugg, fmt.Sprintf("今日胜率偏低（%.1f%%），追涨在冲高回落行情中易吃止损；日亏熔断已兜底，建议保持小仓观察。", wr))
		} else {
			sugg = append(sugg, fmt.Sprintf("今日胜率 %.1f%%，盈亏结构健康，维持现有 S01 v2 参数。", wr))
		}
		if trailCount > 0 && float64(trailLoss)/float64(trailCount) > 0.3 {
			sugg = append(sugg, fmt.Sprintf("跟踪止盈小亏占比偏高（%d/%d），进场多处于浅冲阶段；请确认行情加载完整（启动日志应显示约 700 币）。", trailLoss, trailCount))
		}
		if stopCount > 8 {
			sugg = append(sugg, fmt.Sprintf("今日止损 %d 笔，检查是否频繁追高；单笔止损 4%% 内为正常成本，避免因单日波动改参数。", stopCount))
		}
	}
	if up, ok := market["up"].(int); ok {
		if down, ok := market["down"].(int); ok && down > up {
			sugg = append(sugg, fmt.Sprintf("市场偏弱：上涨 %d / 下跌 %d，追涨胜率通常下降，可考虑适当降低开仓频率。", up, down))
		}
	}
	sugg = append(sugg, "风险提示：实盘仅用验证过的 S01 v2 参数，任何参数改动必须先回测后实盘。")
	return map[string]interface{}{
		"trades": map[string]interface{}{
			"closedCount": len(positions),
			"todayPnl":    round2(totalPnl),
			"winRate":     round1(safePct(winCount, len(positions))),
			"stopCount":   stopCount,
			"trailCount":  trailCount,
			"byCoin":      coinList,
		},
		"suggestions": sugg,
	}
}

// GetDailySummary 生成「每日总结」：市场概况（全局）+ 模拟盘/实盘双模式交易总结与建议
func (s *QuantService) GetDailySummary() map[string]interface{} {
	market := s.computeMarketSummary()

	s.mu.RLock()
	defer s.mu.RUnlock()
	cur := s.mode
	if s.db == nil {
		return map[string]interface{}{
			"currentMode": cur,
			"market":      market,
			"modes":       map[string]interface{}{},
		}
	}
	modes := map[string]interface{}{}
	for _, m := range []string{"SIMULATION", "LIVE"} {
		if m == cur {
			modes[m] = computeModeSummary(s.db, m, market)
			continue
		}
		tmp, err := storage.OpenReadOnly(storage.DBPathForMode("data", m))
		if err != nil {
			modes[m] = map[string]interface{}{
				"trades":      map[string]interface{}{"closedCount": 0, "todayPnl": 0, "winRate": 0, "byCoin": []interface{}{}},
				"suggestions": []string{"该模式数据库不可用"},
			}
			continue
		}
		modes[m] = computeModeSummary(tmp, m, market)
		tmp.Close()
	}
	return map[string]interface{}{
		"currentMode": cur,
		"market":      market,
		"modes":       modes,
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
	if s.db == nil || s.client == nil || s.ws == nil {
		return "服务未初始化"
	}
	// 前一个引擎可能刚被 Stop，但 goroutine 尚未退出（例如正在等待网络调用返回）。
	// 等待其完全退出后再创建新引擎，避免新旧引擎并发运行导致 DB/行情竞态。
	if !s.waitForEngineExit(5 * time.Second) {
		return "上一策略引擎仍在退出，请稍后重试"
	}

	// 创建引擎（集成委托管理器）
	s.engine = strategy.NewEngine(s.cfg, s.client, s.ws, s.db, s.orderMgr)
	s.engine.SetMode(s.mode)
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
	warmup := int64(0)
	if s.engine != nil {
		tick = s.engine.GetTickCount()
		warmup = s.engine.GetWarmupRemainingSec()
	}
	log.Printf("[Binding] GetStrategyStatus called: running=%v tick=%d warmup=%ds", s.started, tick, warmup)

	status := map[string]interface{}{
		"running":            s.started,
		"tickCount":          tick,
		"warmupRemainingSec": warmup, // 启动预热剩余秒数（0=不在预热期）
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
		data["tickerLoadMsg"] = s.engine.TickerLoadMsg()
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
	if s.client != nil {
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
	} else {
		data["totalWalletBalance"] = float64(0)
		data["totalUnrealizedPnl"] = float64(0)
		data["totalMarginBalance"] = float64(0)
		data["availableBalance"] = float64(0)
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

// GetTradeStats 获取已平仓持仓的全量汇总统计（当前模式数据库）。
// 背景：成交页此前用最近 200 条列表在端上计算"总"卡片，数字随窗口截断失真；
// 本接口由数据库直接聚合全部已平仓记录，保证"总平仓/总净盈亏/总手续费"真实。
// 胜率口径：盈利 /（盈利+亏损），零盈亏（多为对账幽灵单）不计入分母。
func (s *QuantService) GetTradeStats() map[string]interface{} {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.db == nil {
		return map[string]interface{}{
			"count": 0, "netPnl": 0.0, "wins": 0, "losses": 0, "zeros": 0, "totalFee": 0.0, "winRate": 0.0,
		}
	}
	stats, err := s.db.GetClosedStats()
	if err != nil {
		log.Printf("[Binding] 查询成交统计失败: %v", err)
		return map[string]interface{}{
			"count": 0, "netPnl": 0.0, "wins": 0, "losses": 0, "zeros": 0, "totalFee": 0.0, "winRate": 0.0,
		}
	}
	decided := stats.Wins + stats.Losses
	winRate := 0.0
	if decided > 0 {
		winRate = float64(stats.Wins) / float64(decided) * 100
	}
	return map[string]interface{}{
		"count":    stats.Count,
		"netPnl":   round2(stats.NetPnl),
		"wins":     stats.Wins,
		"losses":   stats.Losses,
		"zeros":    stats.Zeros,
		"totalFee": round2(stats.TotalFee),
		"winRate":  round1(winRate),
	}
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
	s.mu.RLock()
	defer s.mu.RUnlock()
	if s.orderMgr == nil {
		return map[string]interface{}{
			"activeCount":   0,
			"lastSyncTime":  int64(0),
			"lastSyncError": "",
		}
	}
	return s.orderMgr.GetSyncStatus()
}

// waitForEngineExit 等待所有策略引擎 goroutine 退出，超时返回 false。
// 不持有 s.mu 的引擎 goroutine 不需要该锁，因此可在持有锁时调用。
func (s *QuantService) waitForEngineExit(timeout time.Duration) bool {
	done := make(chan struct{})
	go func() {
		s.engineWG.Wait()
		close(done)
	}()
	select {
	case <-done:
		return true
	case <-time.After(timeout):
		return false
	}
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
