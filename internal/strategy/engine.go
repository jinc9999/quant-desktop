// Package strategy 策略引擎
package strategy

import (
	"context"
	"errors"
	"fmt"
	"log"
	"sync"
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
}

// orphanScanInterval 孤儿仓位扫描间隔（每 N 个 Tick 执行一次）。
// 检测交易所存在但本地 DB 无记录的"野仓位"，自动收养并纳入管理。
const orphanScanInterval = 10

// Engine 策略引擎主结构
type Engine struct {
	cfg            binance.StrategyConfig
	client         *binance.Client
	ws             *binance.WsManager
	db             *storage.DB
	orderMgr       *order.Manager
	breaker        *risk.CircuitBreaker // 熔断器：日亏/回撤达标后停止开新仓
	window         *SlidingWindow
	running        bool
	stopCh         chan struct{}
	mu             sync.RWMutex
	tickCount      int64
	tickErrorCount int64     // Tick 执行失败累计次数
	startTime      time.Time // 引擎启动时间
	cooldown       map[string]time.Time // symbol -> 平仓时间，冷却期内不再开仓
	cooldownReason map[string]string    // symbol -> 最近平仓原因（分原因冷却: 移动止盈可短冷却）
	failedOpen     map[string]time.Time // symbol -> 开仓失败时间，短期内不再重试
	openBlocked    map[string]time.Time // symbol -> 结构性开仓失败拉黑截止时间（12h，防反复刷屏）
	closeRetry     map[int64]time.Time  // positionID -> 平仓失败重试冷却截止时间（3min，防强平模式刷屏）
	onError        func(context, message string) // 后台错误回调（推送到前端弹窗）
	lastBreakerDay string            // 上次熔断检查日期（YYYY-MM-DD），跨天时重置日熔断
	lastTickerRefresh time.Time      // 最近一次 REST 全量行情刷新时间（WS 缺币自愈用）

	// klineOpenCache: symbol -> 当前 K 线周期开盘价缓存。
	// K 线开盘价在周期内不变，只需每周期拉取一次，降低 REST 调用量（K 线信号模式用）。
	klineOpenCache map[string]klineOpenEntry

	// newListLogged: 已记录过新币过滤日志的合约（每次进程运行去重，防每 Tick 刷屏）
	newListLogged map[string]bool
}

// klineOpenEntry K 线开盘价缓存条目
type klineOpenEntry struct {
	open        float64 // 本周期 K 线开盘价
	periodStart int64   // 本周期起点（Unix 毫秒，按 Timeframe 对齐）
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
		cfg:      cfg,
		client:   client,
		ws:       ws,
		db:       db,
		orderMgr: orderMgr,
		window:   NewSlidingWindow(ParseTimeframeMs(cfg.Timeframe), sampleMs),
		stopCh:   make(chan struct{}),
		cooldown:   make(map[string]time.Time),
		cooldownReason: make(map[string]string),
		failedOpen: make(map[string]time.Time),
		openBlocked: make(map[string]time.Time),
		closeRetry:  make(map[int64]time.Time),
		klineOpenCache: make(map[string]klineOpenEntry),
		newListLogged:  make(map[string]bool),
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
	e.mu.Lock()
	if e.running {
		e.mu.Unlock()
		return
	}
	e.running = true
	e.startTime = time.Now()
	e.mu.Unlock()

	log.Printf("[Strategy] 引擎启动，模式: %s, 间隔: %ds", e.cfg.Timeframe, e.cfg.ScanIntervalSec)

	// 加载交易对精度规则（exchangeInfo），确保下单数量/价格符合交易所要求
	// 必须在补挂止损单之前执行，否则 FormatPrice 回退到 8 位小数导致 -1111 精度错误
	// 网络抖动可能失败，重试 3 次（指数退避），仍失败则依赖开仓前 EnsurePrecision 兜底
	for attempt := 1; attempt <= 3; attempt++ {
		if err := e.client.LoadExchangeInfo(ctx); err != nil {
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
		if err := e.orderMgr.RecoverOnStartup(ctx); err != nil {
			log.Printf("[Strategy] 启动对账失败: %v", err)
		}
		// 为所有无活跃委托的 OPEN 持仓补挂交易所止损单，确保 Bot 离线期间持仓仍有保护
		e.orderMgr.EnsureOrdersForOpenPositions(ctx, e.cfg)
	}

	// 确保账户为双向持仓模式（Hedge Mode）：策略下单硬编码 positionSide=LONG，
	// 单向持仓模式下所有委托会被交易所拒绝（-4061）。失败时记录错误日志告警。
	if err := e.client.EnsureHedgeMode(ctx); err != nil {
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
	e.ws.StartAllMarketTicker(ctx)

	ticker := time.NewTicker(time.Duration(e.cfg.ScanIntervalSec) * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			e.runOnce(ctx)
		case <-e.stopCh:
			log.Println("[Strategy] 引擎停止")
			return
		case <-ctx.Done():
			return
		}
	}
}

// Stop 停止策略引擎
func (e *Engine) Stop() {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.running {
		return
	}
	e.running = false
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
	e.tickCount++
	start := time.Now()

	// 跨天时重置日熔断（新的一天重新累计日亏）
	e.checkBreakerReset(time.Now())

	// 1. 获取行情（P2：优先 WS 全量缓存，空则回退 REST）
	fetchStart := time.Now()
	tickers, err := e.fetchTickers(ctx)
	if err != nil {
		e.tickErrorCount++
		log.Printf("[Strategy] Tick %d 获取行情失败: %v", e.tickCount, err)
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
			e.tickCount, len(blockedNew))
	}

	// 4. 筛选候选（kline 模式：先粗筛 → 拉当前 K 线开盘价（缓存）→ K 线实体涨幅判定；
	//    sliding 模式：滑动窗口过程涨幅判定）
	screenStart := time.Now()
	confirmWindowMs := int64(e.cfg.ConfirmWindowMin * 60000)
	var klineOpen map[string]float64
	if e.cfg.SignalMode == "kline" {
		klineOpen = e.buildKlineOpenMap(ctx, tickers, now, blockedNew)
		// confirmWindowMs 在 kline 模式保留：供放量确认使用（最近 N 分钟成交量 vs 之前窗口）。
		// 价格二次确认（ConfirmThreshold）对 kline 模式保持关闭，由 screener 内 !klineMode 守卫保证。
	}
	candidates := ScreenSliding(e.window, filterTickers(tickers, blockedNew), priceMap, e.cfg.MinGainPct, e.cfg.Min24hGainPct, e.cfg.MinQuoteVolume, e.cfg.TopN, now,
		e.cfg.EnableShort, confirmWindowMs, e.cfg.ConfirmThreshold, e.cfg.VolumeSurgeThreshold,
		e.cfg.SignalMode, klineOpen, e.cfg.MaxPullbackPct)
	screenDur := time.Since(screenStart)

	// 4.5 候选明细日志
	e.logCandidates(candidates, now)

	// 4.6 同步交易所委托状态（检测止损单是否已触发成交）
	if e.orderMgr != nil {
		if err := e.orderMgr.SyncOrders(ctx, priceMap); err != nil {
			log.Printf("[Strategy] Tick %d 同步委托状态失败: %v", e.tickCount, err)
		}
	}

	// 4.62 周期性完整熔断检查（日亏 + 账户回撤）：余额接口是网络请求，不能放在每次
	// 平仓回调里（会拖慢 tick 循环，实盘网络慢时被放大）——每 60 tick（约 15 分钟）
	// 检查一次即可；日亏在平仓回调里已用本地 DB 实时检查。
	if e.tickCount%60 == 0 {
		e.updateBreaker(ctx)
	}

	// 4.65 定期扫描孤儿仓位（交易所有但本地 DB 无的持仓），自动收养
	e.scanOrphanPositions(ctx)

	// 4.7 自定义平仓监控（交易所止损单的备用保护）
	// 策略自主判断止损/跟踪止盈条件，达标时市价平仓
	e.monitorPositions(ctx, priceMap)

	// 5. 开仓（P0 并发；P1 复用本 Tick 已查询的持仓，避免重复查询）
	openStart := time.Now()
	openPositions, err := e.db.GetOpenPositions()
	if err != nil {
		e.tickErrorCount++
		log.Printf("[Strategy] 查询持仓失败: %v", err)
		return
	}
	opened := e.openPositions(ctx, candidates, priceMap, openPositions)
	openDur := time.Since(openStart)

	log.Printf("[Strategy] Tick %d 完成: %d 候选, 开 %d 仓, 总耗时 %v [行情 %v | 筛选 %v | 开仓 %v]",
		e.tickCount, len(candidates), len(opened), time.Since(start).Round(time.Millisecond),
		fetchDur.Round(time.Millisecond), screenDur.Round(time.Millisecond),
		openDur.Round(time.Millisecond))
}

// fetchTickers 获取全市场行情（P2）。
// 优先使用 WS 全量行情缓存；缓存为空（如 WS 未连接）时回退到 REST 轮询，
// 并将 REST 数据回填到 WS 缓存，保证前端 GetPrice 能获取到实时价格。
// 参数:
//   - ctx: 上下文
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
		e.ws.BackfillCache(tickers)
		e.lastTickerRefresh = time.Now()
	}
	return tickers, err
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
		open, err := e.client.GetKlineOpen(ctx, sym, e.cfg.Timeframe)
		if err != nil {
			continue // 拉取失败保守跳过（ScreenSliding 忽略缺失项，不产生假信号）
		}
		e.klineOpenCache[sym] = klineOpenEntry{open: open, periodStart: periodStart}
		result[sym] = open
	}
	return result
}

// buildNewListingBlocked 计算本 Tick 被新币过滤拦截的交易对集合，并记录过滤日志。
// 过滤条件（EnableNewListingFilter && NewListingMinDays>0 时生效）：
//   上市天数 <= NewListingMinDays 的合约被拦截（排除新上市合约）。
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
				e.tickCount, t.Symbol,
				float64(wLen)/1000, current, gain, e.cfg.MinGainPct)
		}
	}

	if e.tickCount <= 3 || activeCount < len(tickers) {
		log.Printf("[Strategy] Tick %d 窗口摘要: 可判断=%d 总币种=%d",
			e.tickCount, activeCount, len(tickers))
	}
}

// logCandidates 打印筛选出的候选币种明细
// candidates: 已按涨幅降序排列的候选列表
// now: 当前时间（Unix 毫秒）
func (e *Engine) logCandidates(candidates []Candidate, now int64) {
	if len(candidates) == 0 {
		return
	}
	log.Printf("[Strategy] Tick %d 筛选结果: %d 个候选达标", e.tickCount, len(candidates))
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
		e.tickCount, threshold, pass, filtered, len(tickers))
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
	if bal, balErr := e.client.GetFuturesBalance(ctx); balErr == nil && bal != nil {
		// 条件单占用系数 3：主仓 1 份 + 止损条件单 1 份 + 移动止盈条件单 1 份
		need := float64(slots) * e.cfg.PositionMarginUSDT * 3
		if bal.AvailableBalance < need {
			log.Printf("[Strategy] ⛔ 可用余额不足，跳过本 Tick 开仓：可用 %.2f U < 需 %.2f U（%d 仓 × 单仓 %.1f U × 条件单系数 3）",
				bal.AvailableBalance, need, slots, e.cfg.PositionMarginUSDT)
			return nil
		}
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
		if failTime, failed := e.failedOpen[c.Symbol]; failed {
			if time.Since(failTime) < failedOpenCooldown {
				continue
			}
			delete(e.failedOpen, c.Symbol)
		}

		// 结构性失败拉黑：该币种在当前配置下短期内无法开仓（-2027/-4028/-2019 等），
		// 拉黑 12 小时，杜绝周期性重试刷屏
		if blockedUntil, blocked := e.openBlocked[c.Symbol]; blocked {
			if time.Now().Before(blockedUntil) {
				continue
			}
			delete(e.openBlocked, c.Symbol)
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

		// 计算开仓数量：保证金 * 杠杆 / 入场价，按交易所 stepSize 向下取整
		rawAmount := (e.cfg.PositionMarginUSDT * float64(e.cfg.Leverage)) / entryPrice
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
			e.openBlocked[symbol] = time.Now().Add(openBlockedDuration)
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
	open := func(qty float64) error {
		if side == "SHORT" {
			_, err := e.client.OpenShort(ctx, symbol, qty)
			return err
		}
		_, err := e.client.OpenLong(ctx, symbol, qty)
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
			e.openBlocked[symbol] = time.Now().Add(openBlockedDuration)
			log.Printf("[Strategy] %s 开仓结构性失败(code=%d)，拉黑 %s 不再重试: %v", symbol, code, openBlockedDuration, openErr)
			// -2027（仓位超限）可能因交易所已有该币种持仓而本地无记录（孤儿仓位），
			// 尝试收养一次纳入本地管理（失败也不阻塞；拉黑已防刷屏）
			if code == -2027 {
				e.adoptOrphanPosition(ctx, symbol)
			}
		} else {
			// 瞬时/其他失败：短冷却 + 前端弹窗提醒
			e.failedOpen[symbol] = time.Now()
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

	// 挂交易所止损保护委托（STOP_MARKET + TRAILING_STOP_MARKET）
	// 这是主保护机制：即使 Bot 崩溃/重启，交易所仍会自动触发止损
	// 挂单失败时 orderMgr 内部会回滚（平仓 + 取消已挂委托）
	if e.orderMgr != nil {
		if err := e.orderMgr.PlaceStopOrders(ctx, pos, e.cfg); err != nil {
			log.Printf("[Strategy] 挂止损委托失败 %s 持仓ID=%d: %v", symbol, pos.ID, err)
			e.emitError("挂止损单", fmt.Sprintf("%s 挂止损条件单失败: %v", symbol, err))
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
		// 因此：该持仓已有活跃条件单时，本地平仓逻辑完全交由交易所条件单 + SyncOrders 闭环
		//（SyncOrders 每 Tick 检测 FILLED → handleFilledOrder 关仓，延迟最多 10 秒）。
		// 本地 monitorPositions 仅作为「条件单缺失/挂出失败」时的兜底保护。
		if e.hasActiveStopOrders(pos.ID) {
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
		if o.Status == "NEW" || o.Status == "PARTIALLY_FILLED" {
			return true
		}
	}
	return false
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
	if pos.Side == "SHORT" {
		_, closeErr = e.client.CloseShort(ctx, pos.Symbol, pos.Amount)
	} else {
		_, closeErr = e.client.CloseLong(ctx, pos.Symbol, pos.Amount)
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

	// 计算盈亏: 做多=(出场-入场)*数量, 做空=(入场-出场)*数量
	exitPrice := currentPrice
	var pnl float64
	if pos.Side == "SHORT" {
		pnl = (pos.EntryPrice - exitPrice) * pos.Amount
	} else {
		pnl = (exitPrice - pos.EntryPrice) * pos.Amount
	}

	// 更新数据库
	_ = e.db.ClosePosition(pos.ID, reason, pnl, &exitPrice, 0)

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
			if err := e.orderMgr.PlaceStopOrders(ctx, pos, e.cfg); err != nil {
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
	if e.tickCount%orphanScanInterval != 0 {
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
	return e.tickCount
}

// GetTickErrorCount 返回 Tick 执行失败的累计次数
func (e *Engine) GetTickErrorCount() int64 {
	return e.tickErrorCount
}

// GetStartTime 返回引擎启动时间
func (e *Engine) GetStartTime() time.Time {
	e.mu.RLock()
	defer e.mu.RUnlock()
	return e.startTime
}
