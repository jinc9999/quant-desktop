// Package strategy 策略引擎
package strategy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/order"
	"quant-desktop/internal/risk"
	"quant-desktop/internal/storage"
)

// openConcurrency 并发开仓的最大并行度。
// 每个仓位开仓含 3 次串行网络往返（开仓 + 2 条止损委托），以有界并发（信号量限流）
// 将其压缩回 Tick 预算内，同时限制并发度以匹配币安下单速率限制，避免触发限频。
const openConcurrency = 6

// failedOpenCooldown 开仓失败后的重试冷却期。
// 同一币种开仓失败后，在此时间内不再重试，避免无限循环报错（如 -2027 仓位超限）。
const failedOpenCooldown = 5 * time.Minute

// openBlockedDuration 结构性开仓失败（该币种在当前配置下无法开仓）的拉黑时长。
// 结构性失败（-2027 仓位超限 / -4028 杠杆无效 / -2019 保证金不足等）短期不可能恢复，
// 若只用 5 分钟短冷却会周期性重试刷屏，拉黑 12 小时后当日基本不再重试。
const openBlockedDuration = 12 * time.Hour

// closeRetryInterval 平仓失败（如 -2023 强平模式）后的重试间隔。
// 强平模式解除需要时间，每 Tick 重试会刷屏报错；3 分钟重试一次兼顾保护与安静。
const closeRetryInterval = 3 * time.Minute

// takerFeeRate 手续费兜底费率（单边 0.05% taker，交易所真实佣金优先，查不到时用）
const takerFeeRate = 0.0005

// structuralOpenErrors 结构性开仓失败错误码集合：
// 命中任一错误码说明该币种在当前配置（杠杆/保证金/仓位规模）下短期内无法开仓，
// 命中后拉黑该币种 openBlockedDuration，避免周期性重试刷屏。
var structuralOpenErrors = map[int64]bool{
	-2027: true, // 仓位张数超限（测试网低价小币张数上限极小，如 <1000 张）
	-4005: true, // 数量超过最大下单量（单笔数量上限极小的币种，减半重试仍失败则拉黑）
	-4028: true, // 杠杆对该币无效（如低价小币最高仅 10x，20x 设置被拒）
	-4164: true, // 名义价值低于最小下限（如 5U）
	-2019: true, // 保证金不足（逐仓占用 / 交易对处于强平模式）
	-4131: true, // 市价单价格 filter 拒绝（测试网薄盘）
	-4411: true, // 未签署 TradFi-Perps 协议（demo 平台需网页端签署，重启应用后恢复）
	-1121: true, // Invalid symbol：模拟盘信号走实盘 fapi、下单走 demo，实盘新币/下架币 demo 无对应合约
}

// orphanScanInterval 孤儿仓位扫描间隔（每 N 个 Tick 执行一次）。
// 检测交易所存在但本地 DB 无记录的"野仓位"，自动收养并纳入管理。
const orphanScanInterval = 10

// openConfirmTicks 开仓待确认的最大复核轮数（约 30 tick ≈ 7.5 分钟）。
// 开仓后查询真实持仓失败时转入延迟复核，超时仍未确认则保持本地记录并告警。
const openConfirmTicks = 30

// Engine 策略引擎主结构
type Engine struct {
	cfg                binance.StrategyConfig
	client             *binance.Client
	ws                 *binance.WsManager
	db                 *storage.DB
	orderMgr           *order.Manager
	breaker            *risk.CircuitBreaker // 熔断器：日亏/回撤达标后停止开新仓
	window             *SlidingWindow
	running            bool
	stopCh             chan struct{}
	mu                 sync.RWMutex
	tickCount          atomic.Int64
	tickErrorCount     atomic.Int64                  // Tick 执行失败累计次数
	startTime          time.Time                     // 引擎启动时间
	cooldown           map[string]time.Time          // symbol -> 平仓时间，冷却期内不再开仓
	cooldownReason     map[string]string             // symbol -> 最近平仓原因（分原因冷却: 移动止盈可短冷却）
	failedOpen         map[string]time.Time          // symbol -> 开仓失败时间，短期内不再重试
	openBlocked        map[string]time.Time          // symbol -> 结构性开仓失败拉黑截止时间（12h，防反复刷屏）
	closeRetry         map[int64]time.Time           // positionID -> 平仓失败重试冷却截止时间（3min，防强平模式刷屏）
	stopBreachSince    map[int64]time.Time           // positionID -> 价格首次击穿有效止损位的时间（交易所条件单兜底计时）
	stopFallbackDelay  time.Duration                 // 条件单存在时，价格击穿止损位后等待条件单成交的最长时间，超时本地兜底平仓
	openConfirmPending map[int64]int                 // positionID -> 剩余复核轮数（开仓后未确认到真实持仓）
	stateMu            sync.Mutex                    // 保护并发开仓 goroutine 对 failedOpen/openBlocked 的读写
	onError            func(context, message string) // 后台错误回调（推送到前端弹窗）
	lastBreakerDay     string                        // 上次熔断检查日期（YYYY-MM-DD），跨天时重置日熔断
	lastTickerRefresh  time.Time                     // 最近一次 REST 全量行情刷新时间（WS 缺币自愈用）
	tickerFullLogged   bool                          // 全量行情加载是否已写入日志（启动首次 + 缺币告警）
	tickerLoadMsg      string                        // 最近一次全量行情加载信息（供前端展示）
	mode               string                        // 引擎所属模式（SIMULATION/LIVE），自动记录每日总结用
	engineCancel       context.CancelFunc            // engineCtx 的取消函数
	stopRequested      bool                          // Stop 早于 Start 时记录停止请求，Start 启动前直接退出
	lastBalanceErrLog  atomic.Int64                  // 最近一次余额查询失败日志时间（Unix 毫秒），防刷屏
	signalDebug        bool                          // 信号判定审计日志开关（QUANT_SIGNAL_DEBUG=1，用于模拟盘/实盘信号分叉排查）

	// klineOpenCache: symbol -> 当前 K 线周期开盘价缓存。
	// K 线开盘价在周期内不变，只需每周期拉取一次，降低 REST 调用量（K 线信号模式用）。
	klineOpenCache map[string]klineOpenEntry

	// smart5m: symbol -> 当前 15m 周期内最大 5m 收盘涨幅%（智慧版仓位因子）。
	// 每次 runOnce 从 klineOpenCache 构建；SmartSizeMode=0 时为 nil（A/B 行为不变）。
	smart5m map[string]float64

	// newListLogged: 已记录过新币过滤日志的合约（每次进程运行去重，防每 Tick 刷屏）
	newListLogged map[string]bool
}

// klineOpenEntry K 线开盘价缓存条目
type klineOpenEntry struct {
	open        float64 // 本周期 K 线开盘价
	periodStart int64   // 本周期起点（Unix 毫秒，按 Timeframe 对齐）
	takerBuyPct float64 // 本周期主动买入占比（%）；-1 表示无数据（仅观测，不改决策）
	gain5mMax   float64 // 智慧版: 本周期内最大 5m 收盘涨幅%（0=未拉取/关闭）
}

// NewEngine 创建策略引擎
// cfg: 策略配置
// client: 币安客户端
// ws: WebSocket 行情管理器
// db: 数据库实例
// orderMgr: 委托生命周期管理器
func NewEngine(cfg binance.StrategyConfig, client *binance.Client, ws *binance.WsManager, db *storage.DB, orderMgr *order.Manager) *Engine {
	// 滑动窗口长度取自 Timeframe 配置（如 "5m" -> 300000ms），
	// 采样间隔取自扫描间隔，用于推导基准匹配容差
	sampleMs := int64(cfg.ScanIntervalSec) * 1000
	e := &Engine{
		cfg:                cfg,
		client:             client,
		ws:                 ws,
		db:                 db,
		orderMgr:           orderMgr,
		window:             NewSlidingWindow(ParseTimeframeMs(cfg.Timeframe), sampleMs),
		stopCh:             make(chan struct{}),
		cooldown:           make(map[string]time.Time),
		cooldownReason:     make(map[string]string),
		failedOpen:         make(map[string]time.Time),
		openBlocked:        make(map[string]time.Time),
		closeRetry:         make(map[int64]time.Time),
		stopBreachSince:    make(map[int64]time.Time),
		stopFallbackDelay:  30 * time.Second, // 约 2 个 tick（ScanIntervalSec=15s），给交易所条件单留出成交窗口
		openConfirmPending: make(map[int64]int),
		klineOpenCache:     make(map[string]klineOpenEntry),
		newListLogged:      make(map[string]bool),
		signalDebug:        os.Getenv("QUANT_SIGNAL_DEBUG") == "1",
	}
	if e.signalDebug && db != nil {
		_ = db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "info",
			Module:    "strategy",
			Message:   "信号判定审计日志已开启（QUANT_SIGNAL_DEBUG=1）：每个 tick 输出近门槛币种的逐项过滤结果",
		})
	}
	// 冷却期闭环修复（2026-08-08）：交易所条件单/回滚平仓完成后，
	// 通知引擎写入冷却期——此前主平仓路径从不写冷却期，导致同币无限快速重复开仓。
	// 熔断闭环修复（2026-08-08）：条件单平仓同样更新日亏熔断——此前熔断只在引擎
	// 本地平仓路径（超时/手动）更新，实盘 95% 平仓为交易所条件单触发，
	// 导致每日 5% 亏损熔断在实盘几乎永不生效（实盘 -35U 无刹车事故根因）。
	// 回调在 runOnce 同一 goroutine 内触发（SyncOrders 同步调用），无需加锁。
	if orderMgr != nil {
		orderMgr.OnClose = e.onPositionClosed
	}
	return e
}

// onPositionClosed 平仓回调（引擎注册给订单管理器）：
// 写冷却期 + 轻量日亏检查。条件单平仓是实盘主路径，回调在 tick goroutine 内同步触发，
// 绝不能在回调内发起网络请求（会拖慢扫描循环）——日亏只用本地 DB 检查，
// 账户回撤熔断改由 runOnce 周期性检查（每 60 tick 一次，约 15 分钟）。
func (e *Engine) onPositionClosed(symbol, reason string) {
	e.cooldown[symbol] = time.Now()
	e.cooldownReason[symbol] = reason
	e.checkDailyLossBreaker()
}

// checkDailyLossBreaker 仅用本地数据库检查日亏熔断（无网络调用，可安全用于平仓回调/tick）
func (e *Engine) checkDailyLossBreaker() {
	if e.isBreakerNil() {
		return
	}
	todayPnl, _, err := e.db.GetTodayPnl()
	if err != nil {
		return
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breaker == nil {
		return
	}
	if e.breaker.CheckDailyLoss(todayPnl) {
		log.Printf("[Strategy] ⛔ 日亏熔断触发：当日盈亏 %.2f USDT，达到日损限制，停止开新仓", todayPnl)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   fmt.Sprintf("日亏熔断触发：当日盈亏 %.2f USDT，停止开新仓", todayPnl),
		})
	}
}

// SetOnError 设置后台错误回调（用于推送错误到前端弹窗）
// fn: 回调函数，context 为操作上下文，message 为错误信息
func (e *Engine) SetOnError(fn func(context, message string)) {
	e.onError = fn
}

// SetBreaker 注入熔断器
// breaker: 熔断器实例（日亏/回撤达标后停止开新仓）；nil 表示不启用熔断
func (e *Engine) SetBreaker(breaker *risk.CircuitBreaker) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.breaker = breaker
}

// checkBreakerReset 检查是否跨天，跨天时重置日熔断并更新熔断日期
// now: 当前时间
func (e *Engine) checkBreakerReset(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breaker == nil {
		return
	}
	day := now.Format("2006-01-02")
	if e.lastBreakerDay != "" && e.lastBreakerDay != day {
		e.breaker.ResetDaily()
		log.Printf("[Strategy] 新的一天（%s），日熔断已重置", day)
	}
	e.lastBreakerDay = day
}

// isBreakerBlocked 返回熔断器是否已阻断开仓
func (e *Engine) isBreakerBlocked() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.breaker != nil && e.breaker.IsBlocked()
}

// updateBreaker 平仓后更新熔断器状态（日亏 + 账户回撤）
// 当日累计亏损达到 dailyLossLimit 或账户回撤达到 maxDrawdown 时触发熔断，
// 后续 Tick 将停止开新仓（已开仓位仍正常监控止损）。
func (e *Engine) updateBreaker(ctx context.Context) {
	if e.isBreakerNil() {
		return
	}

	// 网络/DB 请求放锁外执行，避免长时间持有互斥锁
	todayPnl, _, todayErr := e.db.GetTodayPnl()
	balance, balanceErr := e.client.GetFuturesBalance(ctx)

	e.mu.Lock()
	defer e.mu.Unlock()
	if e.breaker == nil {
		return
	}

	// 当日累计已实现盈亏（含手续费），来自本地数据库统计
	if todayErr == nil && e.breaker.CheckDailyLoss(todayPnl) {
		log.Printf("[Strategy] ⛔ 日亏熔断触发：当日盈亏 %.2f USDT，达到日损限制，停止开新仓", todayPnl)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   fmt.Sprintf("日亏熔断触发：当日盈亏 %.2f USDT，停止开新仓", todayPnl),
		})
	}

	// 账户回撤：当前权益 vs 启动时权益（初始权益由 service 层在启动时设置）
	if balanceErr == nil && balance != nil {
		equity := balance.TotalMarginBalance
		if e.breaker.CheckDrawdown(equity) {
			log.Printf("[Strategy] ⛔ 账户回撤熔断触发：当前权益 %.2f USDT，达到回撤限制，停止开新仓", equity)
			e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "warn",
				Module:    "strategy",
				Message:   fmt.Sprintf("账户回撤熔断触发：当前权益 %.2f USDT，停止开新仓", equity),
			})
		}
	}
}

// isBreakerNil 返回熔断器是否为 nil
func (e *Engine) isBreakerNil() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.breaker == nil
}

// emitError 触发错误回调（若已设置）
func (e *Engine) emitError(context, message string) {
	if e.onError != nil {
		e.onError(context, message)
	}
}

// Start 启动策略引擎（阻塞，在 goroutine 中调用）
func (e *Engine) Start(ctx context.Context) {
	engineCtx, engineCancel := context.WithCancel(ctx)
	e.mu.Lock()
	if e.running {
		engineCancel()
		e.mu.Unlock()
		return
	}
	e.engineCancel = engineCancel
	e.running = true
	if e.stopRequested {
		// Stop 可能在 Start goroutine 调度前被调用：直接退出，避免引擎永不停机。
		e.stopRequested = false
		e.running = false
		e.engineCancel = nil
		e.mu.Unlock()
		engineCancel()
		return
	}
	e.startTime = time.Now()
	e.mu.Unlock()
	defer engineCancel()

	log.Printf("[Strategy] 引擎启动，模式: %s, 间隔: %ds", e.cfg.Timeframe, e.cfg.ScanIntervalSec)

	// 同步币安服务器时间：本机时钟漂移时所有签名请求被拒（-1021），
	// 表现为余额 0.00、开仓/平仓/对账间歇失败。策略每次启动都校准一次，
	// 与 service 层启动同步互为双保险（防运行中时钟调整后的漂移）。
	if e.client != nil {
		syncCtx, syncCancel := context.WithTimeout(engineCtx, 5*time.Second)
		if err := e.client.SyncServerTime(syncCtx); err != nil {
			log.Printf("[Strategy] ⚠ 服务器时间同步失败: %v", err)
		}
		syncCancel()
	}

	// 加载交易对精度规则（exchangeInfo），确保下单数量/价格符合交易所要求
	// 必须在补挂止损单之前执行，否则 FormatPrice 回退到 8 位小数导致 -1111 精度错误
	// 网络抖动可能失败，重试 3 次（指数退避），仍失败则依赖开仓前 EnsurePrecision 兜底
	for attempt := 1; attempt <= 3; attempt++ {
		if err := e.client.LoadExchangeInfo(engineCtx); err != nil {
			log.Printf("[Strategy] 加载精度规则失败（第 %d/3 次）: %v", attempt, err)
			if attempt < 3 {
				time.Sleep(time.Duration(attempt) * time.Second)
			}
			continue
		}
		break
	}

	// 启动时与交易所对账，恢复崩溃期间的委托状态
	if e.orderMgr != nil {
		if err := e.orderMgr.RecoverOnStartup(engineCtx); err != nil {
			log.Printf("[Strategy] 启动对账失败: %v", err)
		}
		// 为所有无活跃委托的 OPEN 持仓补挂交易所止损单，确保 Bot 离线期间持仓仍有保护
		e.orderMgr.EnsureOrdersForOpenPositions(engineCtx, e.cfg)
	}

	// 确保账户为双向持仓模式（Hedge Mode）：策略下单硬编码 positionSide=LONG，
	// 单向持仓模式下所有委托会被交易所拒绝（-4061）。失败时记录错误日志告警。
	if err := e.client.EnsureHedgeMode(engineCtx); err != nil {
		log.Printf("[Strategy] 双向持仓模式设置失败（下单将被交易所拒绝 -4061）: %v", err)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "error",
			Module:    "strategy",
			Message:   "双向持仓模式设置失败: " + err.Error(),
		})
	}

	// 记录启动日志
	e.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "strategy",
		Message:   "策略引擎启动",
	})

	// 启动全市场行情流（P2）：单连接覆盖全市场，替代每 Tick 的 REST 轮询。
	// 连接失败或缓存为空时，runOnce 会自动回退到 FetchTickers REST，保证可靠性。
	e.ws.StartAllMarketTicker(engineCtx)

	ticker := time.NewTicker(time.Duration(e.cfg.ScanIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.runOnce(engineCtx)
		case <-e.stopCh:
			log.Println("[Strategy] 引擎停止")
			return
		case <-engineCtx.Done():
			return
		}
	}
}

// Stop 停止策略引擎
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		e.stopRequested = true
		return
	}
	e.running = false
	if e.engineCancel != nil {
		e.engineCancel()
	}
	close(e.stopCh)

	e.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "strategy",
		Message:   "策略引擎停止",
	})
}

// IsRunning 返回引擎是否正在运行
func (e *Engine) IsRunning() bool {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.running
}

// runOnce 执行单个 Tick
// 完整交易闭环：获取行情 → 喂价 → 滑动筛选 → 开仓 → 持仓监控（止损/跟踪止损）
func (e *Engine) runOnce(ctx context.Context) {
	tick := e.tickCount.Add(1)
	start := time.Now()

	// 跨天时重置日熔断（新的一天重新累计日亏）
	e.checkBreakerReset(time.Now())

	// 每小时自动记录当日盈亏历史（每日总结趋势图数据源）
	if tick%240 == 0 {
		e.autoSaveDailySummary()
	}

	// 1. 获取行情（P2：优先 WS 全量缓存，空则回退 REST）
	fetchStart := time.Now()
	tickers, err := e.fetchTickers(ctx)
	if err != nil {
		e.tickErrorCount.Add(1)
		log.Printf("[Strategy] Tick %d 获取行情失败: %v", tick, err)
		return
	}
	fetchDur := time.Since(fetchStart)

	// 2. 构建价格映射（symbol -> 最新价），优先使用 WS 实时缓存
	now := time.Now().UnixMilli()
	priceMap := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		if p, ok := e.ws.GetPrice(t.Symbol); ok && p > 0 {
			priceMap[t.Symbol] = p
		} else if t.LastPrice > 0 {
			priceMap[t.Symbol] = t.LastPrice
		}
	}

	// 3. 向滑动窗口喂入本 tick 的价格采样（含 24h 累计成交额，用于放量确认）
	volumeMap := make(map[string]float64, len(tickers))
	for _, t := range tickers {
		volumeMap[t.Symbol] = t.QuoteVolume
	}
	for symbol, price := range priceMap {
		e.window.Sample(symbol, price, volumeMap[symbol], now)
	}

	// 3.5 预热期窗口状态摘要日志
	e.logWindowStatus(tickers, priceMap, now)

	// 3.6 最小成交额判断过程日志：每 Tick 输出全市场成交额校验统计与示例，
	// 用于确认 1000 万 USDT 限制在粗筛/筛选/开仓各环节真实生效（原始金额→规则→限制→决策）
	e.logQuoteVolumeFilter(tickers)

	// 3.7 新币过滤：计算被拦截合约集合（含一次性过滤日志），筛选层/开仓层共用
	blockedNew := e.buildNewListingBlocked(tickers, now)
	// 3.71 过滤生效确认：每 Tick 输出被拦截合约数量（为 0 时不输出，避免刷屏）
	if len(blockedNew) > 0 {
		log.Printf("[Strategy] Tick %d 新币过滤生效: 拦截 %d 个新上市合约（已从候选筛选中剔除）",
			tick, len(blockedNew))
	}

	// 4. 筛选候选（kline 模式：先粗筛 → 拉当前 K 线开盘价（缓存）→ K 线实体涨幅判定；
	//    sliding 模式：滑动窗口过程涨幅判定）
	screenStart := time.Now()
	confirmWindowMs := int64(e.cfg.ConfirmWindowMin * 60000)
	// 24h 涨幅排名过滤（实验）：在流动性币池内排序，生成通过集合（nil=关闭）
	rankOK := buildRankOK(tickers, e.cfg.MinQuoteVolume, e.cfg.RankMode, e.cfg.RankParam)
	var klineOpen map[string]float64
	if e.cfg.SignalMode == "kline" {
		klineOpen = e.buildKlineOpenMap(ctx, tickers, now, blockedNew)
		// confirmWindowMs 仅供 sliding 模式的价格二次确认使用；kline 模式不触发。
		// 放量确认已移除（2026-08-13 回测验证负贡献），volumeSurgeThreshold 实参传 0 占位。
	}
	// 智慧版：从 K 线缓存构建 5m 爆拉因子表（SmartSizeMode=0 时为 nil，开仓逻辑不受影响）
	if e.cfg.SmartSizeMode > 0 {
		smart5m := make(map[string]float64, len(klineOpen))
		for sym, entry := range e.klineOpenCache {
			smart5m[sym] = entry.gain5mMax
		}
		e.smart5m = smart5m
	} else {
		e.smart5m = nil
	}
	candidates := ScreenSliding(e.window, filterTickers(tickers, blockedNew), priceMap, e.cfg.MinGainPct, e.cfg.Min24hGainPct, e.cfg.MinQuoteVolume, e.cfg.TopN, now,
		e.cfg.EnableShort, confirmWindowMs, e.cfg.ConfirmThreshold, 0,
		e.cfg.SignalMode, klineOpen, e.cfg.MaxPullbackPct, rankOK)
	screenDur := time.Since(screenStart)

	// 4.5 候选明细日志
	e.logCandidates(candidates, now)
	// 4.51 信号判定审计日志（QUANT_SIGNAL_DEBUG=1 时启用）：近门槛币种逐项过滤结果
	if e.signalDebug {
		e.logSignalDebug(tickers, priceMap, klineOpen, blockedNew, rankOK, now)
	}

	// 4.6 同步交易所委托状态（检测止损单是否已触发成交）
	if e.orderMgr != nil {
		if err := e.orderMgr.SyncOrders(ctx, priceMap); err != nil {
			log.Printf("[Strategy] Tick %d 同步委托状态失败: %v", tick, err)
		}
	}

	// 4.62 周期性完整熔断检查（日亏 + 账户回撤）：余额接口是网络请求，不能放在每次
	// 平仓回调里（会拖慢 tick 循环，实盘网络慢时被放大）——每 60 tick（约 15 分钟）
	// 检查一次即可；日亏在平仓回调里已用本地 DB 实时检查。
	if tick%60 == 0 {
		e.updateBreaker(ctx)
	}

	// 4.65 定期扫描孤儿仓位（交易所有但本地 DB 无的持仓），自动收养
	e.scanOrphanPositions(ctx)

	// 4.66 保护委托补齐：OPEN 持仓若没有活跃保护条件单且有实时价，按现价补挂
	// （重启/条件单被交易所取消后的自愈；有现价才能安全钳制触发价，防 -2021 误平）
	if tick <= 1 || tick%20 == 0 {
		e.ensureProtectionOrders(ctx, priceMap)
	}

	// 4.66 延迟复核开仓结果（开仓后未确认到真实持仓的仓位）
	e.confirmPendingOpens(ctx)

	// 4.7 自定义平仓监控（交易所止损单的备用保护）
	// 策略自主判断止损/跟踪止盈条件，达标时市价平仓
	e.monitorPositions(ctx, priceMap)

	// 5. 开仓（P0 并发；P1 复用本 Tick 已查询的持仓，避免重复查询）
	openStart := time.Now()
	openPositions, err := e.db.GetOpenPositions()
	if err != nil {
		e.tickErrorCount.Add(1)
		log.Printf("[Strategy] 查询持仓失败: %v", err)
		return
	}
	opened := e.openPositions(ctx, candidates, priceMap, openPositions)
	openDur := time.Since(openStart)

	log.Printf("[Strategy] Tick %d 完成: %d 候选, 开 %d 仓, 总耗时 %v [行情 %v | 筛选 %v | 开仓 %v]",
		tick, len(candidates), len(opened), time.Since(start).Round(time.Millisecond),
		fetchDur.Round(time.Millisecond), screenDur.Round(time.Millisecond),
		openDur.Round(time.Millisecond))
}

// ensureProtectionOrders 为所有缺少保护条件单的 OPEN 持仓补挂止损/跟踪委托。
// 幂等：hasActiveStopOrders 已只认平仓类条件单（开仓市价单不误判）。
// 无实时价的持仓跳过（保留本地 monitor 保护），有价才挂（computeStopPrices 会按现价钳制触发价）。
func (e *Engine) ensureProtectionOrders(ctx context.Context, priceMap map[string]float64) {
	if e.orderMgr == nil {
		return
	}
	positions, err := e.db.GetOpenPositions()
	if err != nil {
		return
	}
	for i := range positions {
		pos := &positions[i]
		price, ok := priceMap[pos.Symbol]
		if !ok || price <= 0 {
			continue // 无实时价：不挂（防 -2021），本地 monitor 继续保护
		}
		// 失真条件单校正：已有止损触发价与 entry×0.97 偏差 >5%（demo 标记价失真历史问题）→ 撤掉按现价重挂
		if e.hasDistortedStopOrder(pos) {
			log.Printf("[Strategy] %s 持仓#%d 检测到失真止损触发价，取消并按现价 %.6f 重挂", pos.Symbol, pos.ID, price)
			_ = e.orderMgr.CancelRelatedOrders(ctx, pos.ID)
		}
		if e.hasActiveStopOrders(pos.ID) {
			continue
		}
		log.Printf("[Strategy] %s 持仓#%d 无保护委托，按现价 %.6f 补挂止损/跟踪条件单",
			pos.Symbol, pos.ID, price)
		if err := e.orderMgr.PlaceStopOrders(ctx, pos, e.cfg, price); err != nil {
			log.Printf("[Strategy] %s 持仓#%d 补挂条件单失败: %v", pos.Symbol, pos.ID, err)
		}
	}
}

// hasDistortedStopOrder 判断持仓已有止损触发价是否失真（与 entry×0.97 偏差 >5%）。
// demo 标记价失真会把止损触发价压到极低（AKEUSDT 案例 entry 的 89%），导致止损形同虚设。
func (e *Engine) hasDistortedStopOrder(pos *storage.Position) bool {
	orders, err := e.db.GetOrdersByPosition(pos.ID)
	if err != nil {
		return false
	}
	expected := pos.EntryPrice * (1 - e.cfg.StopLossPct)
	for _, o := range orders {
		if o.OrderType == "STOP_MARKET" &&
			(o.Status == "NEW" || o.Status == "PARTIALLY_FILLED") &&
			o.StopPrice != nil {
			if math.Abs(*o.StopPrice-expected) > expected*0.05 {
				return true
			}
		}
	}
	return false
}

// fetchTickers 获取全市场行情（P2）。
// 优先使用 WS 全量行情缓存；缓存为空（如 WS 未连接）时回退到 REST 轮询，
// 并将 REST 数据回填到 WS 缓存，保证前端 GetPrice 能获取到实时价格。
// 参数:
//   - ctx: 上下文
//
// 返回:
//   - []binance.Ticker: 全市场行情列表
//   - error: 获取失败时返回错误
func (e *Engine) fetchTickers(ctx context.Context) ([]binance.Ticker, error) {
	// 仅当缓存存在、足够新鲜（30s 内更新过）且覆盖足够全量币种时才信任缓存。
	// 否则说明 WS 行情流断线/卡死，缓存价格已冻结，必须走 REST 刷新，
	// 否则策略会用死价格计算涨幅，导致永远筛不出候选。
	const cacheTTL = 30 * time.Second
	// 完整性门槛：主网 !ticker@arr 推送为大帧（~1MB），经代理可能被截断/丢帧，
	// 缓存只剩部分币种（demo WS 帧完整不受影响；实盘曾漏掉 IOTX/SAGA/PIXEL 等
	// 强势币，整段行情完全缺席，导致漏单）。REST 回填会把缓存补齐到全量。
	const minCacheSymbols = 500 // 全市场约 700 个 USDT 合约，低于此数视为推送不完整
	// 周期性强制全量刷新：即使缓存看似完整，每 5 分钟用 REST 复核一次，自愈任何漂移
	const fullRefreshEvery = 5 * time.Minute
	if ts := e.ws.GetTickers(); len(ts) >= minCacheSymbols && e.ws.CacheAge() < cacheTTL &&
		time.Since(e.lastTickerRefresh) < fullRefreshEvery {
		return ts, nil
	}
	// REST 回退并回填 WS 缓存，确保前端 GetPrice 有数据
	tickers, err := e.client.FetchTickers(ctx)
	if err == nil && len(tickers) > 0 {
		cached := len(e.ws.GetTickers())
		e.ws.BackfillCache(tickers)
		e.lastTickerRefresh = time.Now()
		// 可见性：启动首次全量加载与"WS 缓存不完整被 REST 补齐"都写入交易日志，
		// 缺币问题发生时日志直接可见（实盘漏单事故根因的防线）。
		if !e.tickerFullLogged || cached < minCacheSymbols {
			msg := fmt.Sprintf("全量行情加载: REST %d 个币, WS 缓存 %d 个 → 补齐后 %d 个", len(tickers), cached, len(e.ws.GetTickers()))
			level := "info"
			if cached < minCacheSymbols {
				level = "warn"
				msg = fmt.Sprintf("⚠ 行情缓存不完整(WS %d 个 < %d)，已 REST 补齐至 %d 个", cached, minCacheSymbols, len(e.ws.GetTickers()))
			}
			log.Printf("[Strategy] %s", msg)
			e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     level,
				Module:    "strategy",
				Message:   msg,
			})
			e.mu.Lock()
			e.tickerLoadMsg = msg
			e.mu.Unlock()
			e.tickerFullLogged = true
		}
	}
	return tickers, err
}

// TickerLoadMsg 返回最近一次全量行情加载信息（供前端「行情加载状态」展示）
func (e *Engine) TickerLoadMsg() string {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.tickerLoadMsg
}

// SetMode 设置引擎所属模式（SIMULATION/LIVE），用于自动记录每日总结历史
func (e *Engine) SetMode(mode string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.mode = mode
}

// autoSaveDailySummary 每小时自动记录当日盈亏历史（summary_type=auto，每日总结趋势图数据源）
func (e *Engine) autoSaveDailySummary() {
	if e.db == nil || e.mode == "" {
		return
	}
	positions, err := e.db.GetTodayClosedPositions()
	if err != nil {
		return
	}
	totalPnl, winCount := 0.0, 0
	for _, p := range positions {
		if p.RealizedPnl != nil {
			totalPnl += *p.RealizedPnl
			if *p.RealizedPnl > 0 {
				winCount++
			}
		}
	}
	winRate := 0.0
	if len(positions) > 0 {
		winRate = float64(winCount) / float64(len(positions)) * 100
	}
	sum := &storage.DailySummary{
		Mode:        e.mode,
		SummaryDate: time.Now().Format("2006-01-02"),
		SummaryType: "auto",
		TodayPnl:    totalPnl,
		WinRate:     winRate,
		TradeCount:  len(positions),
		FeatureJSON: "{}",
	}
	if _, _, err := e.db.SaveDailySummary(sum); err != nil {
		log.Printf("[Strategy] 自动记录每日盈亏失败: %v", err)
	}
}

// buildKlineOpenMap 为 K 线信号模式构建「当前周期 K 线开盘价」映射。
// 先对 tickers 做 24h 涨幅 + 成交额粗筛（与 ScreenSliding 双条件一致），只对粗筛通过的
// 少量币拉取 K 线，避免全市场请求；K 线开盘价在周期内不变，缓存命中直接复用，
// 每个币每个周期最多一次 REST 调用。
// 参数:
//   - ctx: 上下文
//   - tickers: 24h 行情列表
//   - now: 当前时间（Unix 毫秒）
//   - blockedNew: 新币过滤拦截集合（被拦截合约跳过 K 线拉取）
//
// 返回:
//   - map[string]float64: symbol -> K 线开盘价（拉取失败的币不入 map，ScreenSliding 将保守跳过）
func (e *Engine) buildKlineOpenMap(ctx context.Context, tickers []binance.Ticker, now int64, blockedNew map[string]bool) map[string]float64 {
	result := make(map[string]float64)
	periodMs := int64(ParseTimeframeMs(e.cfg.Timeframe))
	if periodMs <= 0 {
		return result
	}
	periodStart := now - now%periodMs

	// 粗筛：成交额 + 24h 涨跌幅（与 ScreenSliding 前置过滤一致，缩小拉取范围）
	var symbols []string
	for _, t := range tickers {
		if blockedNew[t.Symbol] {
			continue // 新币过滤：不拉取被拦截合约的 K 线，节省 REST 调用
		}
		if t.QuoteVolume < e.cfg.MinQuoteVolume {
			continue
		}
		if e.cfg.Min24hGainPct > 0 {
			up := t.PriceChange >= e.cfg.Min24hGainPct
			down := e.cfg.EnableShort && t.PriceChange <= -e.cfg.Min24hGainPct
			if !up && !down {
				continue
			}
		}
		symbols = append(symbols, t.Symbol)
	}

	for _, sym := range symbols {
		// 缓存命中：当前周期已拉取过，直接复用
		if entry, ok := e.klineOpenCache[sym]; ok && entry.periodStart == periodStart {
			result[sym] = entry.open
			continue
		}
		ki, err := e.client.GetKlineInfo(ctx, sym, e.cfg.Timeframe)
		if err != nil {
			continue // 拉取失败保守跳过（ScreenSliding 忽略缺失项，不产生假信号）
		}
		takerPct := -1.0
		if ki.QuoteVolume > 0 {
			takerPct = ki.TakerBuyQuote / ki.QuoteVolume * 100
		}
		g5m := 0.0
		if e.cfg.SmartSizeMode > 0 {
			// 智慧版：同一周期内额外拉一次 5m K 线（限 4 根），计算最大 5m 收盘涨幅；
			// 拉取失败置 -1 → SmartSizeMultiplier 按均仓 1.0 处理（避免误入 0.7 倍温和档，
			// 审查 D1：网络抖动时段若按 0 会系统性压低 D 仓位，与回测口径不一致）
			if v, err := e.client.GetKline5mMaxGain(ctx, sym, now); err == nil {
				g5m = v
			} else {
				g5m = -1
				log.Printf("[Strategy][Smart] %s 5m 爆拉因子拉取失败，按均仓处理: %v", sym, err)
			}
		}
		e.klineOpenCache[sym] = klineOpenEntry{open: ki.Open, periodStart: periodStart, takerBuyPct: takerPct, gain5mMax: g5m}
		result[sym] = ki.Open
	}
	return result
}

// buildNewListingBlocked 计算本 Tick 被新币过滤拦截的交易对集合，并记录过滤日志。
// 过滤条件（EnableNewListingFilter && NewListingMinDays>0 时生效）：
//
//	上市天数 <= NewListingMinDays 的合约被拦截（排除新上市合约）。
//
// 日志去重：每个合约每次进程运行只记录一次（newListLogged），避免每 Tick 刷屏。
// 上市日期未知（exchangeInfo 未加载/无数据）的合约不拦截（失败放行，与 IsFuturesSymbol 一致）。
// 参数:
//   - tickers: 全市场行情列表（含被拦截合约，用于日志与后续筛选剔除）
//   - now: 当前时间（Unix 毫秒）
//
// 返回:
//   - map[string]bool: 被拦截的合约集合
func (e *Engine) buildNewListingBlocked(tickers []binance.Ticker, now int64) map[string]bool {
	blocked := make(map[string]bool)
	if !e.cfg.EnableNewListingFilter || e.cfg.NewListingMinDays <= 0 {
		return blocked
	}
	for _, t := range tickers {
		onboard, ok := e.client.GetOnboardDate(t.Symbol)
		if !ok {
			continue
		}
		days := ListingDays(onboard, now)
		if days < 0 || days > e.cfg.NewListingMinDays {
			continue
		}
		blocked[t.Symbol] = true
		if e.newListLogged[t.Symbol] {
			continue
		}
		e.newListLogged[t.Symbol] = true
		dateStr := time.UnixMilli(onboard).Format("2006-01-02")
		log.Printf("[Strategy] 新币过滤: 拦截 %s（上市日期 %s，上市天数 %d 天 <= 阈值 %d 天）",
			t.Symbol, dateStr, days, e.cfg.NewListingMinDays)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: now,
			Level:     "info",
			Module:    "screener",
			Symbol:    t.Symbol,
			Message: fmt.Sprintf("新币过滤: 排除 %s（上市日期 %s，上市天数 %d 天 <= 阈值 %d 天）",
				t.Symbol, dateStr, days, e.cfg.NewListingMinDays),
		})
	}
	return blocked
}

// isNewListing 判断交易对是否因上市天数过短被过滤（开仓层防御性检查）。
// 与 buildNewListingBlocked 判定一致；上市日期未知时不过滤（失败放行）。
// symbol: 交易对（如 "NEWUSDT"）
// now: 当前时间（Unix 毫秒）
// 返回 true=被过滤（禁止开仓）
func (e *Engine) isNewListing(symbol string, now int64) bool {
	if !e.cfg.EnableNewListingFilter || e.cfg.NewListingMinDays <= 0 {
		return false
	}
	onboard, ok := e.client.GetOnboardDate(symbol)
	if !ok {
		return false
	}
	days := ListingDays(onboard, now)
	return days >= 0 && days <= e.cfg.NewListingMinDays
}

// logWindowStatus 打印窗口状态摘要
// 展示每个币种的最大涨幅（当前价 vs 窗口内所有历史价格），便于观察筛选过程。
// tickers: 行情列表
// priceMap: symbol -> 当前价映射
// now: 当前时间（Unix 毫秒）
func (e *Engine) logWindowStatus(tickers []binance.Ticker, priceMap map[string]float64, now int64) {
	activeCount := 0

	for _, t := range tickers {
		current, ok := priceMap[t.Symbol]
		if !ok || current <= 0 {
			continue
		}

		// 计算窗口内最大涨幅
		gain, ready := e.window.MaxGainPct(t.Symbol, current, now)
		if !ready {
			continue // 仅有当前时刻一个采样点，无法对比
		}
		activeCount++

		// 涨幅接近或达到阈值时，打印详细判断过程
		if gain >= e.cfg.MinGainPct*0.8 {
			wLen := e.window.WindowLengthMs(t.Symbol, now)
			log.Printf("[Strategy] Tick %d %s: 窗口=%.0fs 现价=%.6f 最大涨幅=%.2f%% 阈值=%.1f%%",
				e.tickCount.Load(), t.Symbol,
				float64(wLen)/1000, current, gain, e.cfg.MinGainPct)
		}
	}

	if e.tickCount.Load() <= 3 || activeCount < len(tickers) {
		log.Printf("[Strategy] Tick %d 窗口摘要: 可判断=%d 总币种=%d",
			e.tickCount.Load(), activeCount, len(tickers))
	}
}

// logCandidates 打印筛选出的候选币种明细
// candidates: 已按涨幅降序排列的候选列表
// now: 当前时间（Unix 毫秒）
func (e *Engine) logCandidates(candidates []Candidate, now int64) {
	if len(candidates) == 0 {
		return
	}
	log.Printf("[Strategy] Tick %d 筛选结果: %d 个候选达标", e.tickCount.Load(), len(candidates))
	limit := len(candidates)
	if limit > 10 {
		limit = 10
	}
	for i := 0; i < limit; i++ {
		c := candidates[i]
		wLen := e.window.WindowLengthMs(c.Symbol, now)
		log.Printf("[Strategy]   候选 %d: %s %s 涨幅=%.2f%% 成交额=%.0fUSDT 窗口=%.0fs",
			i+1, c.Symbol, c.Side, c.GainPct, c.QuoteVolume, float64(wLen)/1000)
	}
}

// logSignalDebug 信号判定审计日志（仅 QUANT_SIGNAL_DEBUG=1 时启用）：
// 对「接近达标」的做多币种逐项输出各过滤环节的判定结果（15m 涨幅 / 24h 涨幅 / 排名 / 山顶 / 新币），
// 用于定位同一信号在模拟盘触发、实盘未触发（或反之）的分叉点。
// 只做可观测性输出，不改变任何筛选/开仓逻辑。
func (e *Engine) logSignalDebug(tickers []binance.Ticker, priceMap map[string]float64,
	klineOpen map[string]float64, blockedNew map[string]bool, rankOK map[string]bool, now int64) {

	threshold := e.cfg.MinGainPct
	volTh := e.cfg.MinQuoteVolume
	type nearMiss struct {
		sym       string
		gain      float64
		priceChg  float64
		quoteVol  float64
		current   float64
		klineOpen float64
	}
	var list []nearMiss
	for _, t := range tickers {
		if t.QuoteVolume < volTh*0.5 {
			continue // 远离成交额阈值，常态不输出
		}
		current, ok := priceMap[t.Symbol]
		if !ok || current <= 0 {
			current = t.LastPrice
		}
		if current <= 0 {
			continue
		}
		var gain float64
		open := 0.0
		if e.cfg.SignalMode == "kline" {
			o, ok := klineOpen[t.Symbol]
			if !ok || o <= 0 {
				continue
			}
			open = o
			gain = (current - o) / o * 100
		} else {
			g, ready := e.window.MaxGainPct(t.Symbol, current, now)
			if !ready {
				continue
			}
			gain = g
		}
		if gain < threshold*0.5 {
			continue // 距离阈值太远，不逐条输出
		}
		list = append(list, nearMiss{
			sym: t.Symbol, gain: gain, priceChg: t.PriceChange,
			quoteVol: t.QuoteVolume, current: current, klineOpen: open,
		})
	}
	sort.Slice(list, func(i, j int) bool { return list[i].gain > list[j].gain })
	if len(list) > 30 {
		list = list[:30]
	}
	if len(list) == 0 {
		return
	}
	for _, m := range list {
		fail := ""
		if m.gain < threshold {
			fail = "15m涨幅不足"
		}
		if fail == "" && e.cfg.Min24hGainPct > 0 && m.priceChg < e.cfg.Min24hGainPct {
			fail = "24h涨幅不足"
		}
		if fail == "" && rankOK != nil && !rankOK[m.sym] {
			fail = "排名过滤未通过"
		}
		if fail == "" && e.cfg.MaxPullbackPct > 0 && m.current > 0 {
			if h, ok := tickerHigh(tickers, m.sym); ok && h > 0 {
				if (h-m.current)/h*100 > e.cfg.MaxPullbackPct {
					fail = "山顶回撤过大"
				}
			}
		}
		if fail == "" && blockedNew[m.sym] {
			fail = "新币过滤"
		}
		// 主动买占比（仅观测，不改决策）：当前 15m K 线主动买入成交额占比
		takerStr := "无数据"
		if entry, ok := e.klineOpenCache[m.sym]; ok && entry.takerBuyPct >= 0 {
			takerStr = fmt.Sprintf("%.1f%%", entry.takerBuyPct)
		}
		status := "达标→候选"
		if fail != "" {
			status = "被拒(" + fail + ")"
		}
		line := fmt.Sprintf("[SignalDebug] %-14s 15m=%.2f%% 24h=%.2f%% 成交额=%.0f 现价=%g K线开=%g 主动买=%s → %s",
			m.sym, m.gain, m.priceChg, m.quoteVol, m.current, m.klineOpen, takerStr, status)
		log.Printf("%s", line)
		// 同时写入数据库（windowsgui 无控制台，日志必须以 DB 落盘才能采集对比）
		_ = e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "info",
			Module:    "signal-debug",
			Message:   line,
			Symbol:    m.sym,
			Price:     m.current,
		})
	}
	log.Printf("[SignalDebug] Tick %d 近门槛币种审计: %d 条（QUANT_SIGNAL_DEBUG=1）", e.tickCount.Load(), len(list))
}

// tickerHigh 从行情列表取指定币种 24h 最高价（信号审计辅助）
func tickerHigh(tickers []binance.Ticker, symbol string) (float64, bool) {
	for _, t := range tickers {
		if t.Symbol == symbol {
			return t.HighPrice, t.HighPrice > 0
		}
	}
	return 0, false
}

// f64p 返回 float64 指针（可空字段用）
func f64p(v float64) *float64 {
	return &v
}

// logQuoteVolumeFilter 输出最小成交额（24h 累计成交额）校验的判断过程日志。
// 需求背景：用户要求确认 1000 万 USDT 成交额限制在关键路径真实生效，
// 并清晰记录每个被过滤合约的原始金额 → 校验规则 → 限制匹配 → 决策结果。
//
// 输出策略（避免每 Tick 刷屏）：
//  1. 每次 Tick 输出一行汇总：达标数 / 被过滤数 / 阈值；
//  2. 仅对成交额接近阈值（>= 阈值的 50%）却被过滤的合约打印逐条判断过程，
//     远离阈值的低流动性币（常态）只计入汇总不逐条打印。
//
// 实际拦截仍由 buildKlineOpenMap 粗筛与 ScreenSliding 流动性过滤执行，
// 本函数只负责可观测性日志，不改变任何业务逻辑。
func (e *Engine) logQuoteVolumeFilter(tickers []binance.Ticker) {
	threshold := e.cfg.MinQuoteVolume
	if len(tickers) == 0 {
		return
	}
	pass, filtered := 0, 0
	var nearMiss []binance.Ticker // 接近阈值（>= 50%）却被过滤的合约
	for _, t := range tickers {
		if t.QuoteVolume >= threshold {
			pass++
		} else {
			filtered++
			if t.QuoteVolume >= threshold*0.5 {
				nearMiss = append(nearMiss, t)
			}
		}
	}
	// 汇总：原始金额判断总量 → 校验规则 → 限制匹配
	log.Printf("[Strategy] Tick %d 最小成交额校验: 规则=24h成交额>=%.0fUSDT 达标=%d 被过滤=%d 全市场=%d",
		e.tickCount.Load(), threshold, pass, filtered, len(tickers))
	// 逐条判断过程（仅接近阈值的被过滤合约，限制最多 10 条）
	if len(nearMiss) > 0 {
		limit := len(nearMiss)
		if limit > 10 {
			limit = 10
		}
		for i := 0; i < limit; i++ {
			t := nearMiss[i]
			log.Printf("[Strategy]   成交额过滤 %s: 原始金额=%.0fUSDT 校验规则=24h成交额>=%.0fUSDT 限制匹配=不满足 决策=剔除（不入候选）",
				t.Symbol, t.QuoteVolume, threshold)
		}
	}
}

// markOpenFailed 记录开仓失败冷却时间（并发开仓 goroutine 与筛选 goroutine 共享 map，需加锁）。
func (e *Engine) markOpenFailed(symbol string, at time.Time) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.failedOpen[symbol] = at
}

// openFailedCooldownActive 返回该币是否处于开仓失败冷却中；冷却已过期时清除记录。
func (e *Engine) openFailedCooldownActive(symbol string) bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	failTime, ok := e.failedOpen[symbol]
	if !ok {
		return false
	}
	if time.Since(failTime) >= failedOpenCooldown {
		delete(e.failedOpen, symbol)
		return false
	}
	return true
}

// markOpenBlocked 记录结构性开仓失败拉黑截止时间。
func (e *Engine) markOpenBlocked(symbol string, until time.Time) {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	e.openBlocked[symbol] = until
}

// openBlockedActive 返回该币是否仍在结构性拉黑期；已过期时清除记录。
func (e *Engine) openBlockedActive(symbol string) bool {
	e.stateMu.Lock()
	defer e.stateMu.Unlock()
	blockedUntil, ok := e.openBlocked[symbol]
	if !ok {
		return false
	}
	if time.Now().After(blockedUntil) {
		delete(e.openBlocked, symbol)
		return false
	}
	return true
}

// openPositions 根据候选币种执行开仓（P0：有界并发）
// candidates: 已按涨幅降序排列的候选列表
// priceMap: symbol -> 最新价映射
// openPositions: 本 Tick 已查询的持仓（用于上限检查与去重，避免重复查询）
// 逻辑：串行预选出不超过可用槽位的候选（去重 + 精度取整），再以信号量限流并发开仓，
// 将串行 ~18s 压缩回 Tick 预算内，同时限制并发度以匹配币安限频。
// 返回: 本 Tick 成功开仓的持仓列表
func (e *Engine) openPositions(ctx context.Context, candidates []Candidate, priceMap map[string]float64, openPositions []storage.Position) []storage.Position {
	if len(candidates) == 0 {
		return nil
	}
	// 已持有币种集合（去重，避免同一 Tick 内对同一币重复开仓）
	held := make(map[string]bool, len(openPositions))
	// 持仓明细：用于追加仓位判定（首仓入场价 / 同币持仓数 / 方向 / 移动止盈激活状态）
	type posInfo struct {
		count          int
		firstEntry     float64
		side           string
		trailingActive bool // 同币任一持仓移动止盈已激活（价格曾到过入场价*(1+TrailingActivation)）
	}
	posMap := make(map[string]*posInfo, len(openPositions))
	for _, p := range openPositions {
		held[p.Symbol] = true
		pi, ok := posMap[p.Symbol]
		if !ok {
			pi = &posInfo{count: 0, firstEntry: p.EntryPrice, side: p.Side}
			posMap[p.Symbol] = pi
		}
		pi.count++
		if p.TrailingActive {
			pi.trailingActive = true
		}
		// 首仓 = 原仓：持仓按 opened_at DESC 返回（最新在前），
		// 做多以最低入场价为基准（追加仓必然更高价），做空反之。
		if p.Side == "SHORT" {
			if p.EntryPrice > pi.firstEntry {
				pi.firstEntry = p.EntryPrice
			}
		} else {
			if p.EntryPrice < pi.firstEntry {
				pi.firstEntry = p.EntryPrice
			}
		}
	}

	// 可用开仓名额
	slots := e.cfg.MaxOpenPositions - len(openPositions)
	if slots <= 0 {
		if len(candidates) > 0 {
			// 满仓时静默跳过不报错，此日志让"追加仓被忽略"可见（2026-08-04 排查需求）
			log.Printf("[Strategy] 持仓已满（%d/%d），本 Tick 跳过 %d 个候选开仓（含追加仓）",
				len(openPositions), e.cfg.MaxOpenPositions, len(candidates))
		}
		return nil
	}

	// 开仓前可用余额预检查：demo 测试账户余额有限，且开仓后 2 张条件单
	// （STOP_MARKET + TRAILING_STOP_MARKET）会继续占用保证金。
	// 可用余额不足以支撑本 Tick 最大开仓量（含条件单占用余量）时，
	// 跳过整个 Tick 开仓并输出明确日志，避免交易所 -2019 报错刷屏
	// （2026-08-04 用户反馈 -2019 无法根治，此为根治方案）。
	// 余额查询本身也加短超时并保守处理：查询失败时无法确认可用保证金，
	// 继续下单只会把问题抛给交易所（-2019），因此跳过本 Tick 开仓。
	balCtx, balCancel := context.WithTimeout(ctx, 5*time.Second)
	bal, balErr := e.client.GetFuturesBalance(balCtx)
	balCancel()
	if balErr != nil || bal == nil {
		now := time.Now().UnixMilli()
		if last := e.lastBalanceErrLog.Load(); last == 0 || now-last >= 60_000 {
			e.lastBalanceErrLog.Store(now)
			log.Printf("[Strategy] ⛔ 可用余额查询失败，跳过本 Tick 开仓（避免交易所 -2019）: %v", balErr)
		}
		return nil
	}
	// 条件单占用系数 3：主仓 1 份 + 止损条件单 1 份 + 移动止盈条件单 1 份
	// 智慧版：爆拉桶仓位最大 ×SmartSizeHigh，按上限预留余额，避免 -2019
	marginPer := e.cfg.PositionMarginUSDT
	if e.cfg.SmartSizeMode > 0 && e.cfg.SmartSizeHigh > 1 {
		marginPer *= e.cfg.SmartSizeHigh
	}
	need := float64(slots) * marginPer * 3
	if bal.AvailableBalance < need {
		log.Printf("[Strategy] ⛔ 可用余额不足，跳过本 Tick 开仓：可用 %.2f U < 需 %.2f U（%d 仓 × 单仓 %.1f U × 条件单系数 3）",
			bal.AvailableBalance, need, slots, marginPer)
		return nil
	}

	// 熔断检查：日亏/账户回撤达标后停止开新仓（已开仓位仍由 monitorPositions 正常止损）
	if e.isBreakerBlocked() {
		log.Printf("[Strategy] 熔断器已触发，跳过本 Tick 开仓（%d 候选）", len(candidates))
		return nil
	}

	// 串行预选候选：去重 + 计算入场价与数量，最多 slots 个
	type openTask struct {
		symbol     string
		entryPrice float64
		amount     float64
		side       string
	}
	tasks := make([]openTask, 0, slots)
	for _, c := range candidates {
		if len(tasks) >= slots {
			break
		}

		// 合约市场过滤：跳过无合约的交易对（现货有但合约不存在的币种）
		if !e.client.IsFuturesSymbol(c.Symbol) {
			continue
		}

		// 新币过滤（防御性检查）：上市天数 <= 阈值的合约不参与任何开仓（含追加仓）。
		// 与筛选层 filterTickers 双保险：即使候选被其他路径引入也在此拦截。
		if e.isNewListing(c.Symbol, time.Now().UnixMilli()) {
			// 防御层拦截日志：正常流程下候选已被 filterTickers 剔除，此路径仅覆盖
			// 绕过筛选层的场景（如追加仓），记录被拦截合约名称便于核对过滤是否生效
			if onboard, ok := e.client.GetOnboardDate(c.Symbol); ok {
				log.Printf("[Strategy] 新币过滤(开仓防御): 拦截开仓 %s（上市 %d 天 <= 阈值 %d 天）",
					c.Symbol, ListingDays(onboard, time.Now().UnixMilli()), e.cfg.NewListingMinDays)
			} else {
				log.Printf("[Strategy] 新币过滤(开仓防御): 拦截开仓 %s（上市日期未知）", c.Symbol)
			}
			continue
		}

		// 冷却期检查：平仓后 CooldownMin 分钟内不再开同一币种
		if cdTime, inCD := e.cooldown[c.Symbol]; inCD {
			cooldownDuration := time.Duration(e.cfg.CooldownMin) * time.Minute
			// 分原因冷却（2026-08-08 回测验证）: 移动止盈平仓后可快速再入追趋势，
			// 止损/超时等其他平仓保持完整冷却。CooldownAfterTrailingMin<0 时统一冷却。
			if e.cfg.CooldownAfterTrailingMin >= 0 && e.cooldownReason[c.Symbol] == "TRAILING_STOP" {
				cooldownDuration = time.Duration(e.cfg.CooldownAfterTrailingMin) * time.Minute
			}
			if time.Since(cdTime) < cooldownDuration {
				continue
			}
			// 冷却期已过，清除记录
			delete(e.cooldown, c.Symbol)
			delete(e.cooldownReason, c.Symbol)
		}

		// 开仓失败冷却：同一币种开仓失败后 5 分钟内不再重试，
		// 避免 -2027（仓位超限）等错误无限循环
		if e.openFailedCooldownActive(c.Symbol) {
			continue
		}

		// 结构性失败拉黑：该币种在当前配置下短期内无法开仓（-2027/-4028/-2019 等），
		// 拉黑 12 小时，杜绝周期性重试刷屏
		if e.openBlockedActive(c.Symbol) {
			continue
		}

		entryPrice, ok := priceMap[c.Symbol]
		if !ok || entryPrice <= 0 {
			continue
		}

		// 成交额校验（开仓前确认）：候选必须满足 24h 累计成交额下限（默认 1000 万 USDT）。
		// 该限制在筛选层（buildKlineOpenMap 粗筛 + ScreenSliding）已拦截不达标合约，
		// 此处为开仓前的最终确认日志，记录候选原始成交额与校验决策。
		if c.QuoteVolume < e.cfg.MinQuoteVolume {
			log.Printf("[Strategy] 成交额校验(开仓前): %s 原始金额=%.0fUSDT 校验规则=24h成交额>=%.0fUSDT 限制匹配=不满足 决策=拒绝开仓",
				c.Symbol, c.QuoteVolume, e.cfg.MinQuoteVolume)
			continue
		}
		log.Printf("[Strategy] 成交额校验(开仓前): %s 原始金额=%.0fUSDT 校验规则=24h成交额>=%.0fUSDT 限制匹配=满足 决策=允许开仓",
			c.Symbol, c.QuoteVolume, e.cfg.MinQuoteVolume)

		// 已持仓币种：默认不重复开仓（避免双仓位）；
		// 开启追加仓位（EnableAddOn）时，满足以下全部条件才允许追加 1 张新单：
		// ① 移动止盈已激活：同币任一持仓 TrailingActive=true（价格曾到过入场价*(1+TrailingActivation)，
		//    即趋势曾获确认；2026-08-04 讨论确认用状态而非现价，允许冲高回落后仍追加）
		// ② 再次命中信号（本 Tick 候选已含该币，即通过 24h/15m K 线/放量/山顶过滤）
		// ③ 同币持仓数未达 1+cfg.MaxAddOnsPerSymbol 上限、方向与现仓一致（防对冲）
		if pi, isHeld := posMap[c.Symbol]; isHeld {
			if !e.cfg.EnableAddOn {
				log.Printf("[Strategy][AddOn] %s 跳过追加仓：EnableAddOn 关闭", c.Symbol)
				continue
			}
			if pi.count >= 1+e.cfg.MaxAddOnsPerSymbol {
				log.Printf("[Strategy][AddOn] %s 跳过追加仓：已达单币上限（同币 %d 仓，最多 1+%d）", c.Symbol, pi.count, e.cfg.MaxAddOnsPerSymbol)
				continue
			}
			if c.Side != pi.side {
				log.Printf("[Strategy][AddOn] %s 跳过追加仓：方向不一致（候选 %s / 现仓 %s）", c.Symbol, c.Side, pi.side)
				continue
			}
			if !pi.trailingActive {
				log.Printf("[Strategy][AddOn] %s 跳过追加仓：移动止盈未激活（价格未到过首仓入场 %.6f 的 +%.0f%%）",
					c.Symbol, pi.firstEntry, e.cfg.TrailingActivation*100)
				continue
			}
			log.Printf("[Strategy][AddOn] %s 追加仓条件全部满足：移动止盈已激活（首仓入场 %.6f），同币 %d 仓，方向 %s → 开新单",
				c.Symbol, pi.firstEntry, pi.count, c.Side)
		}

		// 智慧版 5m 爆拉仓位：当前 15m 周期内最大 5m 收盘涨幅分桶调整单仓保证金
		// （A 骨架 + 1.5/0.7/2.5 三关验证：全周期 +36%、2025-26 样本外 +39%，见 docs/策略实验结论-2026-08-13.md）
		margin := e.cfg.PositionMarginUSDT
		if e.cfg.SmartSizeMode > 0 {
			if g5m, ok := e.smart5m[c.Symbol]; ok {
				mult := SmartSizeMultiplier(g5m, e.cfg.SmartSizeHigh, e.cfg.SmartSizeLow, e.cfg.SmartSizeBoundary)
				margin *= mult
				if mult != 1 {
					log.Printf("[Strategy][Smart] %s 5m爆拉 %.2f%% → 仓位倍数 %.2fx（单仓 %.2fU → %.2fU）",
						c.Symbol, g5m, mult, e.cfg.PositionMarginUSDT, margin)
				}
			}
		}
		// 计算开仓数量：保证金 * 杠杆 / 入场价，按交易所 stepSize 向下取整
		rawAmount := (margin * float64(e.cfg.Leverage)) / entryPrice
		amount := e.client.RoundQty(c.Symbol, rawAmount)
		if amount <= 0 {
			log.Printf("[Strategy] %s 取整后数量为0，跳过（原始=%.8f）", c.Symbol, rawAmount)
			continue
		}

		held[c.Symbol] = true
		tasks = append(tasks, openTask{symbol: c.Symbol, entryPrice: entryPrice, amount: amount, side: c.Side})
	}

	if len(tasks) == 0 {
		return nil
	}

	// 有界并发开仓：信号量限流（openConcurrency），结果用互斥锁收集
	var (
		wg     sync.WaitGroup
		mu     sync.Mutex
		opened = make([]storage.Position, 0, len(tasks))
		sem    = make(chan struct{}, openConcurrency)
	)
	for _, task := range tasks {
		wg.Add(1)
		sem <- struct{}{}
		go func(t openTask) {
			defer wg.Done()
			defer func() { <-sem }()

			pos, err := e.openOne(ctx, t.symbol, t.entryPrice, t.amount, t.side)
			if err != nil {
				return // openOne 内部已记录日志与回滚
			}
			mu.Lock()
			opened = append(opened, *pos)
			mu.Unlock()
		}(task)
	}
	wg.Wait()

	return opened
}

// openOne 执行单个仓位的完整开仓流程：开多 → 风控初始化 → 入库 → 挂止损保护委托
// symbol: 交易对
// entryPrice: 入场价格
// amount: 开仓数量
// 返回: (*storage.Position 成功开仓的持仓记录（含数据库 ID）, error 任一步骤失败)
// 止损委托失败时，委托管理器内部会回滚（平仓 + 取消已挂委托），本方法返回错误。
func (e *Engine) openOne(ctx context.Context, symbol string, entryPrice, amount float64, side string) (*storage.Position, error) {
	// 开仓前确保精度规则已加载（启动加载失败时的兜底），
	// 避免 FormatQty 回退 3 位小数而交易所要求整数数量导致 -1111
	if err := e.client.EnsurePrecision(ctx, symbol); err != nil {
		log.Printf("[Strategy] 精度规则缺失 %s（开仓可能因精度被拒）: %v", symbol, err)
	}

	// 开仓前确保该交易对杠杆与配置一致（首次设置后缓存）
	// -4028 表示该币种在交易所不支持配置的杠杆（如低价小币最高仅 10x），
	// 此时按配置杠杆计算的仓位数量全错，直接拉黑该币种并跳过开仓，杜绝周期性重试。
	if err := e.client.EnsureLeverage(ctx, symbol, e.cfg.Leverage); err != nil {
		if binance.IsAPIErrorCode(err, -4028) {
			e.markOpenBlocked(symbol, time.Now().Add(openBlockedDuration))
			log.Printf("[Strategy] %s 不支持 %dx 杠杆，拉黑 %s 跳过开仓: %v", symbol, e.cfg.Leverage, openBlockedDuration, err)
			e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "warn",
				Module:    "strategy",
				Message:   fmt.Sprintf("跳过开仓：%s 不支持 %d 倍杠杆", symbol, e.cfg.Leverage),
				Symbol:    symbol,
				Price:     entryPrice,
				Amount:    amount,
			})
			return nil, fmt.Errorf("杠杆 %d 对该币无效: %w", e.cfg.Leverage, err)
		}
		log.Printf("[Strategy] 设置杠杆失败 %s(%dx)，实际杠杆可能与配置不符: %v", symbol, e.cfg.Leverage, err)
	}

	// 开仓前确保该交易对保证金模式与配置一致（逐仓/全仓，首次设置后缓存）
	if err := e.client.EnsureMarginMode(ctx, symbol, e.cfg.MarginMode); err != nil {
		log.Printf("[Strategy] 设置保证金模式失败 %s(%s)，实际模式可能与配置不符: %v", symbol, e.cfg.MarginMode, err)
	}

	// 下单开仓（根据方向选择开多或开空）
	openAmount := amount
	var openRes *binance.OrderResult
	open := func(qty float64) error {
		var res *binance.OrderResult
		var err error
		if side == "SHORT" {
			res, err = e.client.OpenShort(ctx, symbol, qty)
		} else {
			res, err = e.client.OpenLong(ctx, symbol, qty)
		}
		if err == nil {
			openRes = res
		}
		return err
	}
	openErr := open(openAmount)
	// -2027 仓位张数超限 / -4005 数量超过最大下单量：低价小币上限极小
	// （测试网部分币张数 <1000 或单笔数量上限极小），目标数量超限时减半重试一次，
	// 自适应收敛到交易所允许的规模。
	if openErr != nil && (binance.IsAPIErrorCode(openErr, -2027) || binance.IsAPIErrorCode(openErr, -4005)) {
		halved := e.client.RoundQty(symbol, openAmount/2)
		if halved > 0 && halved < openAmount {
			log.Printf("[Strategy] %s 开仓数量超限，数量减半 %.0f→%.0f 重试: %v", symbol, openAmount, halved, openErr)
			if retryErr := open(halved); retryErr == nil {
				openErr = nil
				openAmount = halved
			} else {
				log.Printf("[Strategy] %s 减半重试仍失败（数量=%.0f）: %v", symbol, halved, retryErr)
			}
		}
	}
	if openErr != nil {
		// 结构性失败 vs 瞬时失败分级处理：
		// 结构性失败（该币种在当前配置下无法开仓，如 -2027/-4028/-2019/-4164/-4131）
		// 短期不可能恢复 → 拉黑 12 小时 + 不弹前端窗，杜绝周期性重试刷屏
		code, isAPIErr := binance.APIErrorCode(openErr)
		if isAPIErr && structuralOpenErrors[code] {
			e.markOpenBlocked(symbol, time.Now().Add(openBlockedDuration))
			log.Printf("[Strategy] %s 开仓结构性失败(code=%d)，拉黑 %s 不再重试: %v", symbol, code, openBlockedDuration, openErr)
			// -2027（仓位超限）可能因交易所已有该币种持仓而本地无记录（孤儿仓位），
			// 尝试收养一次纳入本地管理（失败也不阻塞；拉黑已防刷屏）
			if code == -2027 {
				e.adoptOrphanPosition(ctx, symbol)
			}
		} else {
			// 瞬时/其他失败：短冷却 + 前端弹窗提醒
			e.markOpenFailed(symbol, time.Now())
			log.Printf("[Strategy] 开仓失败 %s %s: %v", side, symbol, openErr)
			e.emitError("开仓", fmt.Sprintf("%s %s 开仓失败: %v", side, symbol, openErr))
		}
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "error",
			Module:    "strategy",
			Message:   "开仓失败: " + openErr.Error(),
			Symbol:    symbol,
			Price:     entryPrice,
			Amount:    amount,
		})
		return nil, openErr
	}

	// 初始化风控状态（初始止损价，根据方向计算）
	riskParams := risk.Params{
		Side:               side,
		StopLossPct:        e.cfg.StopLossPct,
		TrailingActivation: e.cfg.TrailingActivation,
		TrailingCallback:   e.cfg.TrailingCallback,
	}
	riskState := riskParams.InitState(entryPrice)

	// 持久化持仓记录
	pos := &storage.Position{
		Symbol:           symbol,
		Side:             side,
		EntryPrice:       entryPrice,
		Amount:           openAmount,
		Leverage:         e.cfg.Leverage,
		HighestPrice:     &entryPrice,
		TrailingActive:   false,
		CurrentStopPrice: riskState.CurrentStopPrice,
		Status:           "OPEN",
		OpenedAt:         time.Now().UnixMilli(),
	}
	// 修复：必须回填数据库自增 ID，否则止损委托关联与回滚都会指向 position_id=0
	id, err := e.db.InsertPosition(pos)
	if err != nil {
		log.Printf("[Strategy] 持仓入库失败 %s: %v", symbol, err)
		return nil, err
	}
	pos.ID = id

	// 开仓市价单入表（记录交易所真实成交价，供滑点对账；仅记录，不改记账逻辑）。
	// 说明：若后续开仓确认失败回滚本地持仓，该市价单记录保留，恰好能还原“开了又回滚”的过程。
	if openRes != nil && openRes.OrderID > 0 {
		orderNow := time.Now().UnixMilli()
		filledPrice := 0.0
		filledAmount := 0.0
		if openRes.FilledPrice > 0 {
			filledPrice = openRes.FilledPrice
		}
		if openRes.FilledAmount > 0 {
			filledAmount = openRes.FilledAmount
		}
		orderSide := "BUY"
		if side == "SHORT" {
			orderSide = "SELL"
		}
		if _, oerr := e.db.InsertOrder(&storage.Order{
			PositionID:      pos.ID,
			ExchangeOrderID: openRes.OrderID,
			Symbol:          symbol,
			OrderType:       "MARKET",
			Side:            orderSide,
			Status:          openRes.Status,
			Amount:          openAmount,
			FilledPrice:     f64p(filledPrice),
			FilledAmount:    f64p(filledAmount),
			CreatedAt:       orderNow,
			UpdatedAt:       orderNow,
		}); oerr != nil {
			log.Printf("[Strategy] 开仓市价单入表失败 %s #%d: %v", symbol, pos.ID, oerr)
		} else if filledPrice > 0 && entryPrice > 0 {
			slip := (filledPrice - entryPrice) / entryPrice * 100
			log.Printf("[Strategy] 开仓成交 %s #%d 信号价=%.6f 成交均价=%.6f 滑点=%+.3f%%",
				symbol, pos.ID, entryPrice, filledPrice, slip)
			_ = e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "info",
				Module:    "strategy",
				Message:   fmt.Sprintf("开仓成交 %s 信号价=%.6f 成交均价=%.6f 滑点=%+.3f%%", symbol, entryPrice, filledPrice, slip),
				Symbol:    symbol,
				Price:     filledPrice,
				Amount:    openAmount,
			})
		}
	}

	// 开仓结果确认（非干跑）：用交易所真实持仓核对数量，防幽灵仓/数量不符
	//（凌晨 GUA/HOME 幽灵单即"本地记开仓、交易所无仓"一类；失败在此回滚，不挂条件单）
	if e.client != nil && e.client.Mode() != "DRY_RUN" {
		if err := e.confirmOpenPosition(ctx, pos, symbol); err != nil {
			return nil, err // confirmOpenPosition 内部已删除本地记录并告警
		}
	}

	// 挂交易所止损保护委托（STOP_MARKET + TRAILING_STOP_MARKET）
	// 这是主保护机制：即使 Bot 崩溃/重启，交易所仍会自动触发止损
	// 挂单失败时 orderMgr 内部会回滚（平仓 + 取消已挂委托）
	if e.orderMgr != nil {
		cur, _ := e.ws.GetPrice(symbol)
		if err := e.orderMgr.PlaceStopOrders(ctx, pos, e.cfg, cur); err != nil {
			log.Printf("[Strategy] 挂止损委托失败 %s 持仓ID=%d: %v", symbol, pos.ID, err)
			e.emitError("挂止损单", fmt.Sprintf("%s 挂止损条件单失败，该仓已自动回滚平仓: %v", symbol, err))
			return nil, err
		}
	}

	log.Printf("[Strategy] 开%s %s: 价格=%.6f 数量=%.6f 止损=%.6f",
		side, symbol, entryPrice, openAmount, riskState.CurrentStopPrice)
	e.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "strategy",
		Message:   fmt.Sprintf("开%s成功", side),
		Symbol:    symbol,
		Price:     entryPrice,
		Amount:    openAmount,
	})

	return pos, nil
}

// monitorPositions 每 Tick 检查所有 OPEN 持仓的平仓条件
// 替代币安原生 stop order，由策略自主判断：
//
//  1. 止损: 当前价 <= 入场价 * (1 - StopLossPct) → 市价平仓
//  2. 跟踪止盈激活: 当前价 >= 入场价 * (1 + TrailingActivation) → 标记激活
//  3. 跟踪止盈触发: 已激活且当前价 <= 最高价 * (1 - TrailingCallback) → 市价平仓
//
// 参数:
//   - ctx: 上下文
//   - priceMap: symbol → 最新价映射
func (e *Engine) monitorPositions(ctx context.Context, priceMap map[string]float64) {
	positions, err := e.db.GetOpenPositions()
	if err != nil {
		log.Printf("[Strategy] 查询持仓失败: %v", err)
		return
	}

	// 清理已平仓持仓遗留的兜底计时（防 map 无限增长）
	openIDs := make(map[int64]struct{}, len(positions))
	for i := range positions {
		openIDs[positions[i].ID] = struct{}{}
	}
	for id := range e.stopBreachSince {
		if _, ok := openIDs[id]; !ok {
			delete(e.stopBreachSince, id)
		}
	}

	for i := range positions {
		pos := &positions[i]
		price, ok := priceMap[pos.Symbol]
		if !ok || price <= 0 {
			continue
		}

		// 平仓失败重试冷却：上次平仓失败（如 -2023 强平模式）后 3 分钟内跳过，
		// 避免结构性状态解除前每 Tick 重试刷屏报错
		if retryAt, retrying := e.closeRetry[pos.ID]; retrying {
			if time.Now().Before(retryAt) {
				continue
			}
			delete(e.closeRetry, pos.ID)
		}

		// 最长持仓超时平仓（S01 -hold 120）：优先于一切条件单判断。
		// 即使持仓已有活跃条件单（止损/跟踪/止盈），超时后仍主动市价平仓，
		// 防止行情横盘时仓位长期滞留占用保证金。closePosition 会先撤交易所委托。
		if e.cfg.MaxHoldMin > 0 {
			held := time.Since(time.UnixMilli(pos.OpenedAt))
			if held >= time.Duration(e.cfg.MaxHoldMin)*time.Minute {
				log.Printf("[Strategy] %s 持仓超时 %v >= %d 分钟，市价平仓（MAX_HOLD）", pos.Symbol, held.Round(time.Minute), e.cfg.MaxHoldMin)
				e.closePosition(ctx, pos, price, "MAX_HOLD")
				continue
			}
		}

		// 双套止损防重复触发（幽灵持仓根因修复）：
		// 开仓后交易所挂了 STOP_MARKET + TRAILING_STOP_MARKET 条件单，本地 monitorPositions
		// 再用 WS 价格独立判断止损。价格触及止损时双方几乎同时触发——
		// 交易所条件单先成交，本地再发 ReduceOnly 平仓必遭 -2022（无仓位可平）→ 幽灵持仓。
		// 因此：该持仓已有活跃条件单时，平仓以交易所条件单 + SyncOrders 闭环为主路径
		//（SyncOrders 每 Tick 检测 FILLED → handleFilledOrder 关仓，延迟最多 10 秒）。
		// 本地 monitorPositions 仍保留「击穿超时兜底」（checkStopFallback）：
		// 若价格击穿有效止损位后条件单迟迟未成交（薄盘币标记价格滞后/条件单失效时会发生，
		// 2026-08-11 HOMEUSDT 复盘：条件单全程未触发，持仓从 +18% 裸奔至 -7.3U），
		// 超时后主动撤单并市价平仓，防止保护链悬空。
		if e.hasActiveStopOrders(pos.ID) {
			e.checkStopFallback(ctx, pos, price)
			continue
		}

		if pos.Side == "SHORT" {
			// === 做空逻辑 ===
			// 止损检查: 价格上涨到入场价 * (1 + StopLossPct)
			stopLossPrice := pos.EntryPrice * (1 + e.cfg.StopLossPct)
			if price >= stopLossPrice {
				e.closePosition(ctx, pos, price, "STOP_LOSS")
				continue
			}

			// 跟踪止盈激活检查: 价格跌到入场价 * (1 - TrailingActivation)
			activationPrice := pos.EntryPrice * (1 - e.cfg.TrailingActivation)
			if !pos.TrailingActive && price <= activationPrice {
				hp := price // HighestPrice 复用作最低价
				_ = e.db.UpdateRiskState(pos.ID, &hp, true, hp*(1+e.cfg.TrailingCallback))
				pos.TrailingActive = true
				pos.HighestPrice = &hp
				pos.CurrentStopPrice = hp * (1 + e.cfg.TrailingCallback)
				log.Printf("[Strategy] %s 跟踪止盈激活(%.6f<=激活价%.6f) 初始跟踪价=%.6f",
					pos.Symbol, price, activationPrice, pos.CurrentStopPrice)
				e.db.InsertLog(&storage.TradeLog{
					Timestamp: time.Now().UnixMilli(),
					Level:     "info",
					Module:    "strategy",
					Message:   fmt.Sprintf("跟踪止盈激活 %s price=%.6f stop=%.6f", pos.Symbol, price, pos.CurrentStopPrice),
					Symbol:    pos.Symbol,
					Price:     price,
				})
				continue
			}

			// 跟踪止盈已激活 → 追踪最低价 + 判断是否反弹达阈值
			if pos.TrailingActive {
				if pos.HighestPrice != nil && price < *pos.HighestPrice {
					hp := price
					newStop := price * (1 + e.cfg.TrailingCallback)
					_ = e.db.UpdateRiskState(pos.ID, &hp, true, newStop)
					pos.HighestPrice = &hp
					pos.CurrentStopPrice = newStop
				}
				// 反弹检查: 价格从最低点反弹 TrailingCallback%
				if price >= pos.CurrentStopPrice {
					e.closePosition(ctx, pos, price, "TRAILING_STOP")
					continue
				}
			}
		} else {
			// === 做多逻辑 ===
			// 止损检查: 价格跌破入场价 * (1 - StopLossPct)
			stopLossPrice := pos.EntryPrice * (1 - e.cfg.StopLossPct)
			if price <= stopLossPrice {
				e.closePosition(ctx, pos, price, "STOP_LOSS")
				continue
			}

			// 跟踪止盈激活检查: 价格涨到入场价 * (1 + TrailingActivation)
			activationPrice := pos.EntryPrice * (1 + e.cfg.TrailingActivation)
			if !pos.TrailingActive && price >= activationPrice {
				hp := price
				_ = e.db.UpdateRiskState(pos.ID, &hp, true, hp*(1-e.cfg.TrailingCallback))
				pos.TrailingActive = true
				pos.HighestPrice = &hp
				pos.CurrentStopPrice = hp * (1 - e.cfg.TrailingCallback)
				log.Printf("[Strategy] %s 跟踪止盈激活(%.6f>=激活价%.6f) 初始跟踪价=%.6f",
					pos.Symbol, price, activationPrice, pos.CurrentStopPrice)
				e.db.InsertLog(&storage.TradeLog{
					Timestamp: time.Now().UnixMilli(),
					Level:     "info",
					Module:    "strategy",
					Message:   fmt.Sprintf("跟踪止盈激活 %s price=%.6f stop=%.6f", pos.Symbol, price, pos.CurrentStopPrice),
					Symbol:    pos.Symbol,
					Price:     price,
				})
				continue
			}

			// 跟踪止盈已激活 → 追踪最高价 + 判断是否回撤达阈值
			if pos.TrailingActive {
				if pos.HighestPrice != nil && price > *pos.HighestPrice {
					hp := price
					newStop := price * (1 - e.cfg.TrailingCallback)
					_ = e.db.UpdateRiskState(pos.ID, &hp, true, newStop)
					pos.HighestPrice = &hp
					pos.CurrentStopPrice = newStop
				}
				if price <= pos.CurrentStopPrice {
					e.closePosition(ctx, pos, price, "TRAILING_STOP")
					continue
				}
			}
		}
	}
}

// checkStopFallback 交易所条件单存在时的本地兜底保护（防"裸奔"）。
//
// 背景（2026-08-11 HOMEUSDT 复盘）：开仓后本地按最新价判定移动止盈激活并撤销固定止损，
// 但交易所侧条件单按标记价格触发，薄盘币标记价格未跟上 → 条件单全程未触发；
// 而 monitorPositions 因"存在活跃条件单"跳过本地止损，持仓从 +18% 一路裸奔到 -7.3U，
// 最后由 MAX_HOLD 强制平仓。本函数补上最后一道防线：
//
//  1. 价格真跌破当前有效止损位（固定止损或移动止损）缓冲带（0.3%）后开始计时；
//  2. 计时超过 stopFallbackDelay（默认 30s，约 2 个 tick）条件单仍未成交 → 主动撤单市价平仓；
//  3. 价格回到缓冲带内则重置计时，贴线磨（差 0.01%）与插针弹回不触发，
//     避免与正常工作的交易所条件单抢单造成重复平仓/幽灵单（SQD 14:50 案例）。
//
// 双触发竞态由 closePosition 的 -2022 幽灵清理兜底：若交易所条件单恰好先成交，
// 本地市价平仓会收到 -2022，按 GHOST 幂等处理，不会产生反向仓位。
func (e *Engine) checkStopFallback(ctx context.Context, pos *storage.Position, price float64) {
	stopPrice := pos.CurrentStopPrice
	if stopPrice <= 0 {
		return
	}

	// 兜底缓冲带：价格需真跌破止损位 0.3% 以上才计入兜底计时。
	// 交易所条件单按标记价格触发，本地用最新价判断——贴线磨时标记价通常尚未越过止损线，
	// 条件单马上会正常触发，本地不应抢跑（抢跑会产生重复平仓/幽灵单）。
	const fallbackBuffer = 0.003
	breached := false
	if pos.Side == "SHORT" {
		breached = price >= stopPrice*(1+fallbackBuffer) // 做空：价格真涨破止损缓冲带
	} else {
		breached = price <= stopPrice*(1-fallbackBuffer) // 做多：价格真跌破止损缓冲带
	}
	if !breached {
		delete(e.stopBreachSince, pos.ID)
		return
	}

	first, ok := e.stopBreachSince[pos.ID]
	if !ok {
		e.stopBreachSince[pos.ID] = time.Now()
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   fmt.Sprintf("⚠ %s 持仓#%d 价格击穿止损位 %.6f（当前 %.6f），等待交易所条件单成交（%s 后本地兜底平仓）", pos.Symbol, pos.ID, stopPrice, price, e.stopFallbackDelay.Round(time.Second)),
			Symbol:    pos.Symbol,
			Price:     price,
			Amount:    pos.Amount,
		})
		return
	}
	if time.Since(first) < e.stopFallbackDelay {
		return
	}

	delete(e.stopBreachSince, pos.ID)
	reason := "STOP_LOSS"
	if pos.TrailingActive {
		reason = "TRAILING_STOP"
	}
	log.Printf("[Strategy] ⚠ %s 持仓#%d 条件单 %s 未成交，本地兜底平仓（价格 %.6f 击穿止损位 %.6f, reason=%s）",
		pos.Symbol, pos.ID, e.stopFallbackDelay.Round(time.Second), price, stopPrice, reason)
	e.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "warn",
		Module:    "strategy",
		Message:   fmt.Sprintf("本地兜底平仓 %s 持仓#%d reason=%s 价格=%.6f 止损位=%.6f（条件单 %s 未成交）", pos.Symbol, pos.ID, reason, price, stopPrice, e.stopFallbackDelay.Round(time.Second)),
		Symbol:    pos.Symbol,
		Price:     price,
		Amount:    pos.Amount,
	})
	e.closePosition(ctx, pos, price, reason)
}

// confirmOpenPosition 开仓结果确认：开仓后用交易所真实持仓核对数量。
// 比较口径：交易所该币该方向的总持仓 vs 本地同币同方向全部 OPEN 数量合计
// （同币 3 仓在交易所聚合为一个持仓，必须按合计核对）。
//   - 数量一致（容差 0.5%）→ 正常；
//   - 交易所无仓 → 开仓未落地，删除本地记录并返回错误（调用方回滚）；
//   - 数量不符 → 按交易所真实数量修正本仓；
//   - 查询失败 → 转入 openConfirmPending 延迟复核，不阻塞后续流程。
func (e *Engine) confirmOpenPosition(ctx context.Context, pos *storage.Position, symbol string) error {
	var actual float64
	var qErr error
	for attempt := 0; attempt < 3; attempt++ {
		var positions []binance.ExchangePosition
		positions, qErr = e.client.GetPositionRisk(ctx, symbol)
		if qErr != nil {
			time.Sleep(time.Duration(attempt+1) * 400 * time.Millisecond)
			continue
		}
		for _, p := range positions {
			if p.Symbol == symbol && p.PositionSide == pos.Side && p.PositionAmt != 0 {
				actual = math.Abs(p.PositionAmt)
			}
		}
		qErr = nil
		break
	}
	if qErr != nil {
		e.openConfirmPending[pos.ID] = openConfirmTicks
		msg := fmt.Sprintf("开仓待确认 %s 持仓#%d：查询真实持仓失败，%d 个 tick 内自动复核", symbol, pos.ID, openConfirmTicks)
		log.Printf("[Strategy] ⚠ %s", msg)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   msg,
			Symbol:    symbol,
			Amount:    pos.Amount,
		})
		return nil
	}

	totalLocal := e.totalOpenAmount(symbol, pos.Side)
	if actual <= 0 {
		msg := fmt.Sprintf("开仓确认失败 %s 持仓#%d：交易所无真实持仓，回滚本地记录", symbol, pos.ID)
		log.Printf("[Strategy] ❌ %s", msg)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   msg,
			Symbol:    symbol,
			Amount:    pos.Amount,
		})
		_ = e.db.DeletePosition(pos.ID)
		delete(e.openConfirmPending, pos.ID)
		return fmt.Errorf("开仓确认失败：交易所无持仓 %s", symbol)
	}

	tol := math.Max(1e-8, totalLocal*0.005)
	if math.Abs(actual-totalLocal) <= tol {
		delete(e.openConfirmPending, pos.ID)
		log.Printf("[Strategy] ✅ 开仓确认 %s 持仓#%d：数量一致 %.6f", symbol, pos.ID, actual)
		return nil
	}
	// 数量不符：按交易所真实数量修正本仓（同币合计口径）
	newAmt := pos.Amount + (actual - totalLocal)
	if newAmt <= 0 {
		msg := fmt.Sprintf("开仓确认失败 %s 持仓#%d：交易所持仓 %.6f < 本地合计 %.6f，回滚本仓", symbol, pos.ID, actual, totalLocal)
		log.Printf("[Strategy] ❌ %s", msg)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   msg,
			Symbol:    symbol,
			Amount:    pos.Amount,
		})
		_ = e.db.DeletePosition(pos.ID)
		delete(e.openConfirmPending, pos.ID)
		return fmt.Errorf("开仓确认失败：交易所持仓不足 %s", symbol)
	}
	if err := e.db.UpdatePositionAmount(pos.ID, newAmt); err != nil {
		log.Printf("[Strategy] ⚠ 开仓确认 %s 持仓#%d 数量修正写入失败: %v", symbol, pos.ID, err)
	}
	pos.Amount = newAmt
	delete(e.openConfirmPending, pos.ID)
	log.Printf("[Strategy] ⚠ 开仓确认 %s 持仓#%d：数量修正 %.6f → %.6f（交易所合计 %.6f）", symbol, pos.ID, pos.Amount-(actual-totalLocal), newAmt, actual)
	e.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "warn",
		Module:    "strategy",
		Message:   fmt.Sprintf("开仓确认 %s 持仓#%d：数量修正为 %.6f（交易所真实持仓）", symbol, pos.ID, newAmt),
		Symbol:    symbol,
		Amount:    newAmt,
	})
	return nil
}

// confirmPendingOpens 延迟复核：每 tick 处理开仓后未能立即确认的持仓。
func (e *Engine) confirmPendingOpens(ctx context.Context) {
	if len(e.openConfirmPending) == 0 {
		return
	}
	for id, remain := range e.openConfirmPending {
		pos, err := e.db.GetPositionByID(id)
		if err != nil || pos == nil || pos.Status != "OPEN" {
			delete(e.openConfirmPending, id)
			continue
		}
		if err := e.confirmOpenPosition(ctx, pos, pos.Symbol); err != nil {
			// 复核发现交易所无仓/不足：先撤条件单再删本地记录
			if e.orderMgr != nil {
				_ = e.orderMgr.CancelRelatedOrders(ctx, pos.ID)
			}
			_ = e.db.DeletePosition(pos.ID)
			delete(e.openConfirmPending, id)
			continue
		}
		remain--
		if remain <= 0 {
			delete(e.openConfirmPending, id)
			log.Printf("[Strategy] ⚠ 开仓确认超时 %s 持仓#%d：仍未能与交易所对账，保持本地记录", pos.Symbol, pos.ID)
		} else {
			e.openConfirmPending[id] = remain
		}
	}
}

// totalOpenAmount 本地同币同方向当前 OPEN 持仓数量合计（开仓确认的比对基准）。
func (e *Engine) totalOpenAmount(symbol, side string) float64 {
	var total float64
	_ = e.db.Conn.QueryRow(
		`SELECT COALESCE(SUM(amount),0) FROM positions WHERE status='OPEN' AND symbol=? AND side=?`,
		symbol, side,
	).Scan(&total)
	return total
}

// hasActiveStopOrders 判断持仓是否已有活跃的交易所止损条件单（NEW / PARTIALLY_FILLED）。
// monitorPositions 用它跳过本地平仓：价格触及止损时交易所条件单先成交，
// 本地再平会遭 -2022（ReduceOnly 无仓可平）→ 幽灵持仓；有活跃条件单时平仓
// 完全由交易所条件单 + SyncOrders 闭环承担。
// 参数:
//   - positionID: 持仓 ID
//
// 返回:
//   - bool: true 表示已有活跃条件单（本地不应重复平仓）；查询失败返回 false（保守允许本地兜底平仓）
func (e *Engine) hasActiveStopOrders(positionID int64) bool {
	orders, err := e.db.GetOrdersByPosition(positionID)
	if err != nil {
		return false // 查询失败视为无活跃条件单，保留本地兜底保护
	}
	for _, o := range orders {
		// 仅平仓类条件单算"活跃保护委托"；开仓市价单（MARKET）状态可能短暂为 NEW，
		// 误判会导致本地监控跳过止损/跟踪（同 order 包 isProtectionOrder 口径）。
		if (o.Status == "NEW" || o.Status == "PARTIALLY_FILLED") && isProtectionOrderType(o.OrderType) {
			return true
		}
	}
	return false
}

// isProtectionOrderType 平仓保护类条件单判断（与 order 包口径一致，避免 import 循环）
func isProtectionOrderType(orderType string) bool {
	switch orderType {
	case "STOP_MARKET", "TRAILING_STOP_MARKET", "TAKE_PROFIT_MARKET", "LIMIT":
		return true
	default:
		return false
	}
}

// closePosition 执行平仓（市价卖出）并更新数据库
// 参数:
//   - ctx: 上下文
//   - pos: 持仓记录
//   - currentPrice: 触发平仓时的当前价
//   - reason: 平仓原因（STOP_LOSS / TRAILING_STOP）
func (e *Engine) closePosition(ctx context.Context, pos *storage.Position, currentPrice float64, reason string) {
	// 关键：先撤销交易所止损委托，再市价平仓
	// 如果先平仓再撤单，STOP_MARKET/TRAILING_STOP_MARKET 仍活跃，
	// 在双向持仓模式下会反向开空仓，造成意外亏损
	if e.orderMgr != nil {
		_ = e.orderMgr.CancelRelatedOrders(ctx, pos.ID)
	}

	// 市价平仓（根据方向选择平多或平空）
	var closeErr error
	var closeRes *binance.OrderResult
	if pos.Side == "SHORT" {
		closeRes, closeErr = e.client.CloseShort(ctx, pos.Symbol, pos.Amount)
	} else {
		closeRes, closeErr = e.client.CloseLong(ctx, pos.Symbol, pos.Amount)
	}
	if closeErr != nil {
		// -2022（ReduceOnly Order is rejected）说明交易所已无此持仓：
		// 本地 DB 与交易所状态漂移（幽灵持仓），若保留 OPEN 会每 Tick 重试平仓刷屏报错。
		// 向交易所确认无持仓后，将本地持仓标记为 GHOST 已平仓，保证本地状态与交易所一致。
		if binance.IsAPIErrorCode(closeErr, -2022) {
			exchangePositions, qErr := e.client.GetPositionRisk(ctx, pos.Symbol)
			positionExists := false
			if qErr == nil {
				for _, ep := range exchangePositions {
					if ep.Symbol == pos.Symbol && ep.PositionAmt != 0 {
						positionExists = true
						break
					}
				}
			}
			// 仅当确认查询成功且交易所确无持仓时，才判定为幽灵持仓
			if qErr == nil && !positionExists {
				_ = e.db.ClosePosition(pos.ID, "GHOST", 0, nil, 0)
				e.cooldown[pos.Symbol] = time.Now()
				e.cooldownReason[pos.Symbol] = "GHOST"
				log.Printf("[Strategy] 幽灵持仓已清理 %s 持仓ID=%d reason=%s: 交易所无此持仓，本地标记为 GHOST 已平仓", pos.Symbol, pos.ID, reason)
				e.db.InsertLog(&storage.TradeLog{
					Timestamp: time.Now().UnixMilli(),
					Level:     "warn",
					Module:    "strategy",
					Message:   fmt.Sprintf("幽灵持仓已清理 %s reason=%s: 交易所无此持仓", pos.Symbol, reason),
					Symbol:    pos.Symbol,
					Price:     currentPrice,
					Amount:    pos.Amount,
				})
				return
			}
			// -2022 但确认查询失败（网络抖动）：仓位极可能已被交易所条件单平掉，
			// 用 30 秒短重试尽快再次确认，而不是等 3 分钟长重试；
			// 不弹错误窗（避免 -2022 无仓位可平刷屏）。
			e.closeRetry[pos.ID] = time.Now().Add(30 * time.Second)
			log.Printf("[Strategy] 幽灵持仓待确认 %s 持仓ID=%d reason=%s: 交易所无仓(-2022)但确认查询失败，30s 后复查", pos.Symbol, pos.ID, reason)
			e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "warn",
				Module:    "strategy",
				Message:   fmt.Sprintf("幽灵持仓待确认 %s reason=%s: -2022 无仓但确认查询失败，30s 后复查", pos.Symbol, reason),
				Symbol:    pos.Symbol,
				Price:     currentPrice,
				Amount:    pos.Amount,
			})
			return
		}

		// -4131（市价单被 PERCENT_PRICE filter 拒绝）：测试网薄盘/高波动下常见，
		// 此时市价单无法成交，若放弃会导致止损保护失效、亏损扩大（历史案例 COTI 延误 50 分钟）。
		// 降级为 LIMIT 单按标记价挂单平仓：标记价为 filter 基准价必然通过价格过滤，
		// 挂单成交后由 SyncOrders → handleFilledOrder 自动完成平仓闭环。
		if binance.IsAPIErrorCode(closeErr, -4131) && e.orderMgr != nil {
			limitErr := e.orderMgr.PlaceCloseLimitOrder(ctx, pos)
			if limitErr == nil {
				log.Printf("[Strategy] %s 持仓ID=%d: 市价平仓被拒(-4131)，已降级 LIMIT 挂单平仓（价格=标记价）", pos.Symbol, pos.ID)
				e.db.InsertLog(&storage.TradeLog{
					Timestamp: time.Now().UnixMilli(),
					Level:     "warn",
					Module:    "strategy",
					Message:   fmt.Sprintf("市价平仓被拒(-4131)，已降级 LIMIT 挂单平仓 %s", pos.Symbol),
					Symbol:    pos.Symbol,
					Price:     currentPrice,
					Amount:    pos.Amount,
				})
				return
			}
			if errors.Is(limitErr, binance.ErrCloseLimitPending) {
				// 交易所已有平仓挂单等待成交：静默跳过，避免每 Tick 重复挂单与刷屏
				return
			}
			log.Printf("[Strategy] ❌ 降级 LIMIT 平仓失败 %s 持仓ID=%d reason=%s: %v", pos.Symbol, pos.ID, reason, limitErr)
			e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "error",
				Module:    "strategy",
				Message:   fmt.Sprintf("降级 LIMIT 平仓失败 %s reason=%s: %v", pos.Symbol, reason, limitErr),
				Symbol:    pos.Symbol,
				Price:     currentPrice,
				Amount:    pos.Amount,
			})
		}

		// 记录平仓失败重试冷却：强平模式（-2023）等结构性状态解除前，
		// 每 Tick 重试会刷屏报错；3 分钟后重试，兼顾保护与安静
		e.closeRetry[pos.ID] = time.Now().Add(closeRetryInterval)
		log.Printf("[Strategy] 平仓失败 %s 持仓ID=%d reason=%s: %v（%s 后重试）", pos.Symbol, pos.ID, reason, closeErr, closeRetryInterval)
		e.emitError("平仓", fmt.Sprintf("%s 平仓失败(%s): %v", pos.Symbol, reason, closeErr))
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "error",
			Module:    "strategy",
			Message:   fmt.Sprintf("平仓失败 %s reason=%s: %v", pos.Symbol, reason, closeErr),
			Symbol:    pos.Symbol,
			Price:     currentPrice,
			Amount:    pos.Amount,
		})
		return
	}

	// 本地平仓异常校验：成交价相对入场跌幅 >8%（理论止损 -3%）→ 红色告警日志
	if pos.EntryPrice > 0 {
		lossPct := 0.0
		if pos.Side == "LONG" {
			lossPct = (pos.EntryPrice - currentPrice) / pos.EntryPrice * 100
		} else {
			lossPct = (currentPrice - pos.EntryPrice) / pos.EntryPrice * 100
		}
		if lossPct > 8 {
			msg := fmt.Sprintf("⚠ 异常平仓 %s 持仓#%d reason=%s 价格=%.6f 相对入场跌幅=%.2f%%（理论止损 %.2f%%）",
				pos.Symbol, pos.ID, reason, currentPrice, lossPct, e.cfg.StopLossPct*100)
			log.Printf("[Strategy] ❌ %s", msg)
			_ = e.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(), Level: "error", Module: "strategy",
				Message: msg, Symbol: pos.Symbol, Price: currentPrice, Amount: pos.Amount, PositionID: pos.ID,
			})
		}
	}

	// 计算盈亏: 做多=(出场-入场)*数量, 做空=(入场-出场)*数量
	exitPrice := currentPrice
	var pnl float64
	if pos.Side == "SHORT" {
		pnl = (pos.EntryPrice - exitPrice) * pos.Amount
	} else {
		pnl = (exitPrice - pos.EntryPrice) * pos.Amount
	}

	// 手续费：优先按真实成交单查询交易所佣金（仅平仓侧佣金，与条件单路径口径一致）；
	// 查不到时按平仓名义价值×费率兜底。
	fee := 0.0
	if closeRes != nil && closeRes.OrderID > 0 {
		if f, ferr := e.client.GetOrderFee(ctx, pos.Symbol, closeRes.OrderID); ferr == nil {
			fee = f
		}
	}
	if fee <= 0 {
		fee = pos.Amount * exitPrice * takerFeeRate
	}

	// 更新数据库
	_ = e.db.ClosePosition(pos.ID, reason, pnl, &exitPrice, fee)

	msg := fmt.Sprintf("平仓 %s reason=%s exit=%.6f pnl=%.2f", pos.Symbol, reason, exitPrice, pnl)
	log.Printf("[Strategy] %s 持仓ID=%d", msg, pos.ID)
	e.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "strategy",
		Message:   msg,
		Symbol:    pos.Symbol,
		Price:     exitPrice,
		Amount:    pos.Amount,
	})

	// 记录冷却期：平仓后 CooldownMin 分钟内不再开同一币种
	e.cooldown[pos.Symbol] = time.Now()
	e.cooldownReason[pos.Symbol] = reason

	// 平仓后更新熔断状态（日亏累计 + 账户回撤）
	e.updateBreaker(ctx)
}

// adoptOrphanPosition 收养孤儿仓位：交易所存在但本地 DB 无记录的持仓
// 当开仓返回 -2027（仓位超限）时调用，说明交易所已有该币种持仓。
// 查询交易所持仓信息，若存在则写入本地 DB 并补挂止损单，纳入正常管理。
// 参数:
//   - ctx: 上下文
//   - symbol: 交易对
func (e *Engine) adoptOrphanPosition(ctx context.Context, symbol string) {
	positions, err := e.client.GetPositionRisk(ctx, symbol)
	if err != nil {
		log.Printf("[Strategy] 收养孤儿仓位失败 %s: 查询交易所持仓失败: %v", symbol, err)
		return
	}

	for _, ep := range positions {
		if ep.Symbol != symbol || ep.PositionAmt == 0 {
			continue
		}
		// 检查本地 DB 是否已有该持仓（防止重复收养）
		existing, _ := e.db.GetOpenPositions()
		alreadyTracked := false
		for _, p := range existing {
			if p.Symbol == symbol {
				alreadyTracked = true
				break
			}
		}
		if alreadyTracked {
			log.Printf("[Strategy] %s 已在本地 DB 中，无需收养", symbol)
			return
		}

		// 写入本地 DB（根据持仓数量正负判断方向）
		amount := ep.PositionAmt
		side := "LONG"
		if amount < 0 {
			amount = -amount
			side = "SHORT"
		}
		entryPrice := ep.EntryPrice
		riskParams := risk.Params{
			Side:               side,
			StopLossPct:        e.cfg.StopLossPct,
			TrailingActivation: e.cfg.TrailingActivation,
			TrailingCallback:   e.cfg.TrailingCallback,
		}
		riskState := riskParams.InitState(entryPrice)
		pos := &storage.Position{
			Symbol:           symbol,
			Side:             side,
			EntryPrice:       entryPrice,
			Amount:           amount,
			Leverage:         ep.Leverage,
			HighestPrice:     &entryPrice,
			TrailingActive:   false,
			CurrentStopPrice: riskState.CurrentStopPrice,
			Status:           "OPEN",
			OpenedAt:         time.Now().UnixMilli(),
		}
		id, err := e.db.InsertPosition(pos)
		if err != nil {
			log.Printf("[Strategy] 收养孤儿仓位入库失败 %s: %v", symbol, err)
			return
		}
		pos.ID = id

		// 补挂交易所止损单
		if e.orderMgr != nil {
			cur, _ := e.ws.GetPrice(symbol)
			if err := e.orderMgr.PlaceStopOrders(ctx, pos, e.cfg, cur); err != nil {
				log.Printf("[Strategy] 收养孤儿仓位挂止损单失败 %s ID=%d: %v", symbol, id, err)
				e.emitError("挂止损单", fmt.Sprintf("%s 孤儿仓位补挂止损条件单失败: %v", symbol, err))
			}
		}

		log.Printf("[Strategy] 收养孤儿仓位成功 %s: 数量=%.6f 入场价=%.6f ID=%d", symbol, amount, entryPrice, id)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   fmt.Sprintf("收养孤儿仓位 %s 数量=%.6f 入场价=%.6f", symbol, amount, entryPrice),
			Symbol:    symbol,
			Price:     entryPrice,
			Amount:    amount,
		})
		return
	}
	log.Printf("[Strategy] %s 交易所无持仓，-2027 可能为瞬时错误", symbol)
}

// scanOrphanPositions 定期扫描孤儿仓位（交易所有但本地 DB 无的持仓）
// 每 orphanScanInterval 个 Tick 执行一次，防止因 DB 写入失败或 Bot 崩溃导致的野仓位无人管理。
// 同时做反向对账：本地 OPEN 但交易所已无持仓的幽灵仓位标记 GHOST 清理，
// 避免每 Tick 平仓重试被交易所拒绝（-2022）导致刷屏报错。
// 参数:
//   - ctx: 上下文
func (e *Engine) scanOrphanPositions(ctx context.Context) {
	if e.tickCount.Load()%orphanScanInterval != 0 {
		return
	}
	// DRY_RUN 模式无真实交易所状态，跳过对账
	if e.client.Mode() == "DRY_RUN" {
		return
	}

	exchangePositions, err := e.client.GetPositionRisk(ctx, "")
	if err != nil {
		log.Printf("[Strategy] 孤儿仓位扫描失败: %v", err)
		return
	}

	localPositions, err := e.db.GetOpenPositions()
	if err != nil {
		return
	}
	localSet := make(map[string]bool, len(localPositions))
	for _, p := range localPositions {
		localSet[p.Symbol] = true
	}

	// 交易所持仓集合（仅含数量非 0 的持仓）
	exchangeSet := make(map[string]bool, len(exchangePositions))
	for _, ep := range exchangePositions {
		if ep.PositionAmt == 0 {
			continue // 无持仓跳过
		}
		exchangeSet[ep.Symbol] = true
	}

	// 1. 正向收养：交易所有但本地无 → 收养
	for _, ep := range exchangePositions {
		if ep.PositionAmt == 0 {
			continue // 无持仓跳过
		}
		if localSet[ep.Symbol] {
			continue // 已在本地管理
		}
		log.Printf("[Strategy] 发现孤儿仓位 %s: 数量=%.6f 入场价=%.6f，尝试收养", ep.Symbol, ep.PositionAmt, ep.EntryPrice)
		e.adoptOrphanPosition(ctx, ep.Symbol)
	}

	// 2. 反向对账：本地 OPEN 但交易所无持仓 → 幽灵仓位，标记 GHOST 清理
	for _, p := range localPositions {
		if exchangeSet[p.Symbol] {
			continue // 交易所仍有持仓，正常管理
		}
		// 先撤销该持仓的挂单（交易所侧可能已失效）
		if e.orderMgr != nil {
			_ = e.orderMgr.CancelRelatedOrders(ctx, p.ID)
		}
		_ = e.db.ClosePosition(p.ID, "GHOST", 0, nil, 0)
		e.cooldown[p.Symbol] = time.Now()
		e.cooldownReason[p.Symbol] = "GHOST"
		log.Printf("[Strategy] 幽灵持仓已清理 %s 持仓ID=%d: 交易所无此持仓，本地标记为 GHOST 已平仓", p.Symbol, p.ID)
		e.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "warn",
			Module:    "strategy",
			Message:   fmt.Sprintf("幽灵持仓已清理 %s: 交易所无此持仓（孤儿扫描反向对账）", p.Symbol),
			Symbol:    p.Symbol,
			Price:     p.EntryPrice,
			Amount:    p.Amount,
		})
	}
}

// GetTickCount 返回已执行的 Tick 数
func (e *Engine) GetTickCount() int64 {
	return e.tickCount.Load()
}

// GetTickErrorCount 返回 Tick 执行失败的累计次数
func (e *Engine) GetTickErrorCount() int64 {
	return e.tickErrorCount.Load()
}

// GetStartTime 返回引擎启动时间
func (e *Engine) GetStartTime() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.startTime
}

// GetWarmupRemainingSec 返回启动预热剩余秒数。
// 放量确认已移除（2026-08-13 回测验证负贡献），恒为 0，保留方法兼容前端状态字段。
func (e *Engine) GetWarmupRemainingSec() int64 {
	return 0
}
