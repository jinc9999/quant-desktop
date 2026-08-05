// Package binance 币安 REST API 封装
package binance

import (
	"context"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	binance "github.com/adshao/go-binance/v2"
	"github.com/adshao/go-binance/v2/common"
	"github.com/adshao/go-binance/v2/futures"
)

// Client 币安 API 客户端
type Client struct {
	futuresClient *futures.Client
	mode          string // DRY_RUN | SIMULATION | LIVE

	// 精度缓存：symbol -> SymbolPrecision
	precisionMu  sync.RWMutex
	precisionMap map[string]SymbolPrecision
	// 精度规则加载锁：防止并发开仓时重复加载 exchangeInfo
	precisionLoadMu sync.Mutex

	// 杠杆设置缓存：已成功设置目标杠杆的交易对（避免每次开仓重复调用）
	leverageMu  sync.Mutex
	leverageSet map[string]bool

	// 保证金模式缓存：已成功设置目标模式的交易对（避免每次开仓重复调用）
	marginModeMu  sync.Mutex
	marginModeSet map[string]bool

	// DRY_RUN 模式模拟 OrderID 生成器（时间戳×1000+计数器，避免同毫秒重复）
	dryRunOrderID atomic.Int64
}

// detectLocalProxy 自动检测本地代理端口
// 尝试常见代理端口（Clash/V2Ray/SS 等），返回第一个可用的代理 URL
// 如果都不可用则返回 nil（直连）
func detectLocalProxy() *url.URL {
	// 常见本地代理端口：Clash Verge(7897), Clash(7890), V2Ray(1087/1080), SS(1080)
	ports := []int{7897, 7890, 7891, 1087, 1080, 8118}
	for _, port := range ports {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			proxyURL, _ := url.Parse(fmt.Sprintf("http://%s", addr))
			log.Printf("[Binance] 检测到本地代理: %s", addr)
			return proxyURL
		}
	}
	log.Printf("[Binance] 未检测到本地代理，使用直连")
	return nil
}

// newProxiedHTTPClient 创建带代理的 HTTP 客户端
func newProxiedHTTPClient(proxyURL *url.URL) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != nil {
		transport.Proxy = http.ProxyURL(proxyURL)
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// NewClient 创建币安客户端
// apiKey: API 密钥
// apiSecret: API 密钥
// mode: 运行模式
// proxyAddr: 用户指定的代理地址（为空则自动检测）
// proxyPort: 用户指定的代理端口（0 则自动检测）
func NewClient(apiKey, apiSecret, mode string, proxyAddr string, proxyPort int) *Client {
	if mode == "SIMULATION" {
		// 币安已将 testnet 迁移到 demo 平台（2025-11 起）
		futures.UseDemo = true
		futures.BaseApiDemoURL = "https://demo-fapi.binance.com"
		futures.BaseWsDemoURL = "wss://demo-fstream.binance.com/ws"
		futures.BaseWsPublicDemoURL = "wss://demo-fstream.binance.com/public/ws"
		futures.BaseWsMarketDemoURL = "wss://demo-fstream.binance.com/market/ws"
		futures.BaseWsPrivateDemoURL = "wss://demo-fstream.binance.com/private/ws"
		futures.BaseWsApiDemoURL = "wss://demo-fstream.binance.com/ws-fapi/v1"
	}

	fut := futures.NewClient(apiKey, apiSecret)

	// 优先使用用户指定的代理，否则自动检测。
	// 指定代理在启动时先做连通性探测：不可达（服务器关机/端口错误/网络隔离）时
	// 自动回退到本地代理检测，避免整个交易客户端因代理不可用而全部请求失败。
	var proxyURL *url.URL
	if proxyAddr != "" && proxyPort > 0 {
		addr := fmt.Sprintf("%s:%d", proxyAddr, proxyPort)
		conn, err := net.DialTimeout("tcp", addr, 3*time.Second)
		if err != nil {
			log.Printf("[Binance] 指定代理 %s 不可达（%v），回退自动检测", addr, err)
			proxyURL = detectLocalProxy()
		} else {
			conn.Close()
			proxyURL, _ = url.Parse("http://" + addr)
			log.Printf("[Binance] 使用用户指定代理: %s", addr)
		}
	} else {
		proxyURL = detectLocalProxy()
	}
	httpClient := newProxiedHTTPClient(proxyURL)
	fut.HTTPClient = httpClient

	// WS 连接使用独立的 gorilla 拨号器，需通过 SetWsProxyUrl 单独设置代理，
	// 否则 WS 会直连币安（在需代理的网络下超时），导致行情流不可用。
	if proxyURL != nil {
		proxyStr := proxyURL.String()
		binance.SetWsProxyUrl(proxyStr)
		futures.SetWsProxyUrl(proxyStr)
	}

	return &Client{
		futuresClient: fut,
		mode:          mode,
		precisionMap:  make(map[string]SymbolPrecision),
		leverageSet:   make(map[string]bool),
		marginModeSet: make(map[string]bool),
	}
}

// LoadExchangeInfo 从币安获取合约交易对精度规则并缓存
// 应在策略启动时调用一次，后续下单使用缓存的精度信息
// ctx: 请求上下文
// 返回 error 错误信息
func (c *Client) LoadExchangeInfo(ctx context.Context) error {
	info, err := c.futuresClient.NewExchangeInfoService().Do(ctx)
	if err != nil {
		return fmt.Errorf("获取 exchangeInfo 失败: %w", err)
	}

	m := make(map[string]SymbolPrecision, len(info.Symbols))
	for _, s := range info.Symbols {
		sp := SymbolPrecision{
			QtyPrecision:   s.QuantityPrecision,
			PricePrecision: s.PricePrecision,
		}
		if ls := s.LotSizeFilter(); ls != nil {
			sp.StepSize = mustParseFloat(ls.StepSize)
			sp.MinQty = mustParseFloat(ls.MinQuantity)
		}
		if pf := s.PriceFilter(); pf != nil {
			sp.TickSize = mustParseFloat(pf.TickSize)
		}
		m[s.Symbol] = sp
	}

	c.precisionMu.Lock()
	c.precisionMap = m
	c.precisionMu.Unlock()

	log.Printf("[Binance] 已加载 %d 个交易对精度规则", len(m))
	return nil
}

// EnsurePrecision 确保指定交易对的精度规则已加载
// 若 precisionMap 为空（启动时加载失败）或缺失该交易对，则重新拉取 exchangeInfo。
// 用于开仓前兜底：避免网络抖动导致精度规则缺失后，FormatQty 回退 3 位小数
// 而交易所要求整数数量（stepSize=1），触发 -1111 精度错误。
// ctx: 请求上下文
// symbol: 交易对（如 "BANKUSDT"）
// 返回 error 错误信息（加载失败时返回，但不阻断调用方）
func (c *Client) EnsurePrecision(ctx context.Context, symbol string) error {
	c.precisionMu.RLock()
	_, ok := c.precisionMap[symbol]
	c.precisionMu.RUnlock()
	if ok {
		return nil
	}
	// 并发开仓时只允许一个 goroutine 重新加载，避免重复请求
	c.precisionLoadMu.Lock()
	defer c.precisionLoadMu.Unlock()
	// 二次检查：等待锁期间可能已被其他 goroutine 加载
	c.precisionMu.RLock()
	_, ok = c.precisionMap[symbol]
	c.precisionMu.RUnlock()
	if ok {
		return nil
	}
	if err := c.LoadExchangeInfo(ctx); err != nil {
		return fmt.Errorf("补加载精度规则失败: %w", err)
	}
	return nil
}

// Mode 返回客户端运行模式（DRY_RUN / SIMULATION / LIVE）
func (c *Client) Mode() string { return c.mode }

// isDryRun 返回是否为 DRY_RUN 模式（本地模拟，不发送真实请求）
func (c *Client) isDryRun() bool { return c.mode == "DRY_RUN" }

// nextDryRunOrderID 生成 DRY_RUN 模式的模拟 OrderID
// 使用 时间戳×1000+计数器 避免同一毫秒内多个委托 ID 重复
func (c *Client) nextDryRunOrderID() int64 {
	seq := c.dryRunOrderID.Add(1)
	return time.Now().UnixMilli()*1000 + (seq % 1000)
}

// GetPrecision 获取指定交易对的精度规则
// symbol: 交易对（如 "BTCUSDT"）
// 返回 SymbolPrecision 精度规则，bool 是否存在
func (c *Client) GetPrecision(symbol string) (SymbolPrecision, bool) {
	c.precisionMu.RLock()
	defer c.precisionMu.RUnlock()
	sp, ok := c.precisionMap[symbol]
	return sp, ok
}

// IsFuturesSymbol 判断交易对是否存在于合约市场。
// 若 precisionMap 为空（exchangeInfo 未加载），返回 true 不做过滤（安全降级）。
// symbol: 交易对（如 "BTCUSDT"）
// 返回 bool 是否为合约交易对
func (c *Client) IsFuturesSymbol(symbol string) bool {
	c.precisionMu.RLock()
	defer c.precisionMu.RUnlock()
	if len(c.precisionMap) == 0 {
		return true // 未加载精度规则时不过滤，避免误杀所有候选
	}
	_, ok := c.precisionMap[symbol]
	return ok
}

// FormatQty 按交易对精度规则格式化数量字符串
// 先按 stepSize 向下取整，再按 qtyPrecision 截断小数位
// symbol: 交易对
// qty: 原始数量
// 返回 格式化后的数量字符串
func (c *Client) FormatQty(symbol string, qty float64) string {
	sp, ok := c.GetPrecision(symbol)
	if !ok {
		// 无精度信息时回退到 3 位小数（保守值）
		return fmt.Sprintf("%.3f", qty)
	}
	// 按 stepSize 向下取整
	if sp.StepSize > 0 {
		qty = math.Floor(qty/sp.StepSize) * sp.StepSize
	}
	// 按 qtyPrecision 截断小数位
	return strconv.FormatFloat(qty, 'f', sp.QtyPrecision, 64)
}

// FormatPrice 按交易对精度规则格式化价格字符串
// 先按 tickSize 向下取整，再按 pricePrecision 截断小数位
// symbol: 交易对
// price: 原始价格
// 返回 格式化后的价格字符串
func (c *Client) FormatPrice(symbol string, price float64) string {
	sp, ok := c.GetPrecision(symbol)
	if !ok {
		// 无精度信息时回退到 8 位小数
		return fmt.Sprintf("%.8f", price)
	}
	// 按 tickSize 向下取整
	if sp.TickSize > 0 {
		price = math.Floor(price/sp.TickSize) * sp.TickSize
	}
	// 按 pricePrecision 截断小数位
	return strconv.FormatFloat(price, 'f', sp.PricePrecision, 64)
}

// RoundQty 按交易对精度规则将数量向下取整，返回取整后的 float64
// 用于在 engine 层统一取整，确保入库数量 = 实际下单数量
// symbol: 交易对
// qty: 原始数量
// 返回 取整后的数量
func (c *Client) RoundQty(symbol string, qty float64) float64 {
	sp, ok := c.GetPrecision(symbol)
	if !ok {
		// 无精度信息时按 3 位小数截断
		return math.Floor(qty*1000) / 1000
	}
	if sp.StepSize > 0 {
		qty = math.Floor(qty/sp.StepSize) * sp.StepSize
	}
	return qty
}

// stripTrailingZeros 去除浮点数字符串末尾的多余零（用于日志可读性）
func stripTrailingZeros(s string) string {
	if !strings.Contains(s, ".") {
		return s
	}
	s = strings.TrimRight(s, "0")
	return strings.TrimRight(s, ".")
}

// isTransientErr 判断错误是否为瞬时网络错误（值得重试）。
// 代理或交易所中途切断连接时常见 EOF / connection reset / timeout，
// 此类错误短暂等待后重试通常可恢复，不应直接判定为失败。
func isTransientErr(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	for _, kw := range []string{"EOF", "connection reset", "TLS handshake timeout", "i/o timeout", "Client.Timeout", "connection refused"} {
		if strings.Contains(msg, kw) {
			return true
		}
	}
	return false
}

// FetchTickers 获取全市场 24h 行情（含重试）。
// 网络瞬时错误（如代理/交易所 EOF）最多重试 3 次，避免单次抖动导致行情缺失。
// 参数:
//   - ctx: 上下文
// 返回:
//   - []Ticker: 过滤后的 USDT 交易对行情列表
//   - error: 重试耗尽仍失败时返回错误
func (c *Client) FetchTickers(ctx context.Context) ([]Ticker, error) {
	const attempts = 3
	var raw []*futures.PriceChangeStats
	var err error
	for i := 0; i < attempts; i++ {
		raw, err = c.futuresClient.NewListPriceChangeStatsService().Do(ctx)
		if err == nil {
			break
		}
		if !isTransientErr(err) || i == attempts-1 {
			return nil, fmt.Errorf("获取行情失败: %w", err)
		}
		log.Printf("[Binance] FetchTickers 瞬时错误，重试 %d/%d: %v", i+1, attempts, err)
		time.Sleep(time.Duration(i+1) * time.Second)
	}

	var result []Ticker
	for _, t := range raw {
		if len(t.Symbol) > 4 && t.Symbol[len(t.Symbol)-4:] == "USDT" {
			result = append(result, Ticker{
				Symbol:      t.Symbol,
				LastPrice:   mustParseFloat(t.LastPrice),
				PriceChange: mustParseFloat(t.PriceChangePercent),
				QuoteVolume: mustParseFloat(t.QuoteVolume),
				HighPrice:   mustParseFloat(t.HighPrice),
				LowPrice:    mustParseFloat(t.LowPrice),
			})
		}
	}
	return result, nil
}

// GetKlineOpen 获取指定交易对「当前未收盘」K 线的开盘价
// 用于 K 线实体实时信号：当前价相对本周期开盘价的涨幅即盘中实时实体涨幅，
// K 线开盘价在周期内不变，可缓存到周期结束，大幅降低 REST 调用量。
// 参数:
//   - ctx: 上下文
//   - symbol: 交易对（如 "BTCUSDT"）
//   - interval: K 线周期（如 "15m"）
//
// 返回:
//   - float64: 当前 K 线开盘价
//   - error: 拉取失败时返回错误（调用方应保守跳过该币，避免假信号）
func (c *Client) GetKlineOpen(ctx context.Context, symbol, interval string) (float64, error) {
	if c.isDryRun() {
		return 100.0, nil // DRY_RUN 返回固定模拟值，保证单测结果可预测
	}
	klines, err := c.futuresClient.NewKlinesService().Symbol(symbol).Interval(interval).Limit(1).Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("获取 %s K线失败: %w", symbol, err)
	}
	if len(klines) == 0 {
		return 0, fmt.Errorf("获取 %s K线失败: 无数据", symbol)
	}
	return mustParseFloat(klines[0].Open), nil
}

// OpenLong 市价多头开仓
// symbol: 交易对（如 "BTCUSDT"）
// amount: 数量
func (c *Client) OpenLong(ctx context.Context, symbol string, amount float64) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "BUY", Status: "FILLED"}, nil
	}
	qtyStr := c.FormatQty(symbol, amount)

	order, err := c.futuresClient.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeBuy).
		PositionSide(futures.PositionSideTypeLong).
		Type(futures.OrderTypeMarket).
		Quantity(qtyStr).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("开仓失败 %s: %w", symbol, err)
	}

	return &OrderResult{
		OrderID: order.OrderID,
		Symbol:  symbol,
		Side:    "BUY",
		Status:  string(order.Status),
	}, nil
}

// OpenShort 市价开空
// 参数：symbol 交易对，qty 数量
// 返回值：下单结果，错误信息
func (c *Client) OpenShort(ctx context.Context, symbol string, qty float64) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "FILLED"}, nil
	}
	qtyStr := c.FormatQty(symbol, qty)

	order, err := c.futuresClient.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeSell).
		PositionSide(futures.PositionSideTypeShort).
		Type(futures.OrderTypeMarket).
		Quantity(qtyStr).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("开空失败 %s: %w", symbol, err)
	}
	log.Printf("[Binance] ✅ 开空 %s qty=%s orderId=%d", symbol, qtyStr, order.OrderID)
	return &OrderResult{
		OrderID:      order.OrderID,
		Symbol:       order.Symbol,
		Side:         string(order.Side),
		Status:       string(order.Status),
		FilledPrice:  mustParseFloat(order.AvgPrice),
		FilledAmount: mustParseFloat(order.ExecutedQuantity),
	}, nil
}

// CloseLong 市价多头平仓
func (c *Client) CloseLong(ctx context.Context, symbol string, amount float64) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "FILLED"}, nil
	}
	qtyStr := c.FormatQty(symbol, amount)

	order, err := c.futuresClient.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeSell).
		PositionSide(futures.PositionSideTypeLong).
		Type(futures.OrderTypeMarket).
		Quantity(qtyStr).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("平仓失败 %s: %w", symbol, err)
	}

	return &OrderResult{
		OrderID: order.OrderID,
		Symbol:  symbol,
		Side:    "SELL",
		Status:  string(order.Status),
	}, nil
}

// CloseShort 市价平空（买入平仓）
// 参数：symbol 交易对，qty 数量
// 返回值：下单结果，错误信息
func (c *Client) CloseShort(ctx context.Context, symbol string, qty float64) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "BUY", Status: "FILLED"}, nil
	}
	qtyStr := c.FormatQty(symbol, qty)

	order, err := c.futuresClient.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeBuy).
		PositionSide(futures.PositionSideTypeShort).
		Type(futures.OrderTypeMarket).
		Quantity(qtyStr).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("平空失败 %s: %w", symbol, err)
	}
	log.Printf("[Binance] ✅ 平空 %s qty=%s orderId=%d", symbol, qtyStr, order.OrderID)
	return &OrderResult{
		OrderID:      order.OrderID,
		Symbol:       order.Symbol,
		Side:         string(order.Side),
		Status:       string(order.Status),
		FilledPrice:  mustParseFloat(order.AvgPrice),
		FilledAmount: mustParseFloat(order.ExecutedQuantity),
	}, nil
}

// ErrCloseLimitPending 表示该持仓已存在平仓 LIMIT 挂单（防重复挂单，等待成交即可）
var ErrCloseLimitPending = errors.New("已有平仓 LIMIT 挂单等待成交")

// CloseLongWithMark 按标记价 LIMIT 挂单平多（市价平仓被 PERCENT_PRICE 拒绝时的降级方案）
// 标记价为 PERCENT_PRICE filter 的基准价，以标记价下 LIMIT 单必然通过价格过滤；
// 挂单为 GTC + ReduceOnly，成交后由 SyncOrders → handleFilledOrder 自动完成平仓闭环。
// 防重复：若交易所已存在同方向平仓 LIMIT 挂单，返回 ErrCloseLimitPending（调用方应静默跳过）。
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// amount: 平仓数量
// 返回 *OrderResult 挂单结果，error 错误信息
func (c *Client) CloseLongWithMark(ctx context.Context, symbol string, amount float64) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "FILLED"}, nil
	}
	// 防重复挂单：已存在 SELL LIMIT 挂单则直接返回
	if pending, err := c.hasPendingCloseOrder(ctx, symbol, "SELL"); err == nil && pending {
		return nil, ErrCloseLimitPending
	} else if err != nil {
		log.Printf("[Binance] ⚠️ 检查平仓挂单失败 %s: %v", symbol, err)
	}
	mark, err := c.markPriceOf(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("获取标记价失败 %s: %w", symbol, err)
	}
	order, err := c.futuresClient.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeSell).
		PositionSide(futures.PositionSideTypeLong).
		Type(futures.OrderTypeLimit).
		TimeInForce(futures.TimeInForceTypeGTC).
		Price(c.FormatPrice(symbol, mark)).
		Quantity(c.FormatQty(symbol, amount)).
		ReduceOnly(true).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("降级 LIMIT 平多失败 %s: %w", symbol, err)
	}
	log.Printf("[Binance] ✅ %s 市价平仓被拒(-4131)，已降级 LIMIT 挂单平多 价格=%.6f qty=%s orderId=%d", symbol, mark, c.FormatQty(symbol, amount), order.OrderID)
	return &OrderResult{
		OrderID: order.OrderID,
		Symbol:  symbol,
		Side:    "SELL",
		Status:  string(order.Status),
	}, nil
}

// CloseShortWithMark 按标记价 LIMIT 挂单平空（市价平仓被 PERCENT_PRICE 拒绝时的降级方案）
// 逻辑与 CloseLongWithMark 对称，方向为 BUY SHORT。
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// amount: 平仓数量
// 返回 *OrderResult 挂单结果，error 错误信息
func (c *Client) CloseShortWithMark(ctx context.Context, symbol string, amount float64) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "BUY", Status: "FILLED"}, nil
	}
	// 防重复挂单：已存在 BUY LIMIT 挂单则直接返回
	if pending, err := c.hasPendingCloseOrder(ctx, symbol, "BUY"); err == nil && pending {
		return nil, ErrCloseLimitPending
	} else if err != nil {
		log.Printf("[Binance] ⚠️ 检查平仓挂单失败 %s: %v", symbol, err)
	}
	mark, err := c.markPriceOf(ctx, symbol)
	if err != nil {
		return nil, fmt.Errorf("获取标记价失败 %s: %w", symbol, err)
	}
	order, err := c.futuresClient.NewCreateOrderService().
		Symbol(symbol).
		Side(futures.SideTypeBuy).
		PositionSide(futures.PositionSideTypeShort).
		Type(futures.OrderTypeLimit).
		TimeInForce(futures.TimeInForceTypeGTC).
		Price(c.FormatPrice(symbol, mark)).
		Quantity(c.FormatQty(symbol, amount)).
		ReduceOnly(true).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("降级 LIMIT 平空失败 %s: %w", symbol, err)
	}
	log.Printf("[Binance] ✅ %s 市价平仓被拒(-4131)，已降级 LIMIT 挂单平空 价格=%.6f qty=%s orderId=%d", symbol, mark, c.FormatQty(symbol, amount), order.OrderID)
	return &OrderResult{
		OrderID: order.OrderID,
		Symbol:  symbol,
		Side:    "BUY",
		Status:  string(order.Status),
	}, nil
}

// hasPendingCloseOrder 检查交易所是否已存在同方向的平仓 LIMIT 挂单（防止重复挂单堆积）
// ctx: 请求上下文
// symbol: 交易对
// side: 平仓方向（平多="SELL"，平空="BUY"）
// 返回 bool 是否存在挂单，error 错误信息
func (c *Client) hasPendingCloseOrder(ctx context.Context, symbol, side string) (bool, error) {
	open, err := c.GetOpenOrders(ctx, symbol)
	if err != nil {
		return false, err
	}
	for _, o := range open {
		if o.Symbol == symbol && o.Side == side && o.Type == string(futures.OrderTypeLimit) && o.Status == OrderStatusNew {
			return true, nil
		}
	}
	return false, nil
}

// markPriceOf 获取指定交易对的标记价（从持仓信息 positionRisk 中提取）
// 平仓场景下该币种必存在持仓，持仓信息自带 MarkPrice 字段。
// ctx: 请求上下文
// symbol: 交易对
// 返回 float64 标记价，error 错误信息
func (c *Client) markPriceOf(ctx context.Context, symbol string) (float64, error) {
	risks, err := c.GetPositionRisk(ctx, symbol)
	if err != nil {
		return 0, err
	}
	for _, r := range risks {
		if r.Symbol == symbol && r.MarkPrice > 0 {
			return r.MarkPrice, nil
		}
	}
	return 0, fmt.Errorf("未找到 %s 的持仓标记价", symbol)
}

// PlaceStopMarket 挂出止损市价条件单（Algo Order API）
// 当价格触及 triggerPrice 时，以市价平掉多头仓位
// 2025-12-09 起币安条件单必须走 POST /fapi/v1/algoOrder，原 /fapi/v1/order 返回 -4120
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// stopPrice: 止损触发价格
// side: 方向（LONG/SHORT）
// 返回 *OrderResult 下单结果（含 AlgoID），error 错误信息
func (c *Client) PlaceStopMarket(ctx context.Context, symbol string, stopPrice float64, side string) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), AlgoID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "NEW"}, nil
	}
	priceStr := c.FormatPrice(symbol, stopPrice)

	svc := c.futuresClient.NewCreateAlgoOrderService().
		Symbol(symbol).
		Type(futures.AlgoOrderTypeStopMarket).
		TriggerPrice(priceStr).
		ClosePosition(true).
		WorkingType(futures.WorkingTypeMarkPrice)
	if side == "SHORT" {
		svc.Side(futures.SideTypeBuy).PositionSide(futures.PositionSideTypeShort)
	} else {
		svc.Side(futures.SideTypeSell).PositionSide(futures.PositionSideTypeLong)
	}
	resp, err := svc.Do(ctx)
	if err != nil {
		log.Printf("[Binance] ❌ PlaceStopMarket 失败 %s triggerPrice=%s: %v", symbol, priceStr, err)
		return nil, fmt.Errorf("挂止损条件单失败 %s: %w", symbol, err)
	}

	log.Printf("[Binance] ✅ STOP_MARKET 条件单已创建 %s algoId=%d triggerPrice=%s", symbol, resp.AlgoId, priceStr)
	return &OrderResult{
		AlgoID: resp.AlgoId,
		Symbol: symbol,
		Side:   "SELL",
		Status: string(resp.AlgoStatus),
	}, nil
}

// PlaceTrailingStop 挂出跟踪止损条件单（Algo Order API）
// 当价格达到 activatePrice 后，按 callbackRate 比例回撤时触发平仓
// 2025-12-09 起币安条件单必须走 POST /fapi/v1/algoOrder，原 /fapi/v1/order 返回 -4120
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// activationPrice: 跟踪止损激活价格
// callbackRate: 跟踪回撤比例（如 3.0 表示 3%）
// amount: 平仓数量
// 返回 *OrderResult 下单结果（含 AlgoID），error 错误信息
func (c *Client) PlaceTrailingStop(ctx context.Context, symbol string, activationPrice float64, callbackRate float64, amount float64, side string) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "NEW"}, nil
	}
	actPriceStr := c.FormatPrice(symbol, activationPrice)
	qtyStr := c.FormatQty(symbol, amount)
	// callbackRate 精度固定为 1 位小数（币安要求 0.1-5.0 之间，步长 0.1）
	cbStr := fmt.Sprintf("%.1f", callbackRate)

	svc := c.futuresClient.NewCreateAlgoOrderService().
		Symbol(symbol).
		Type(futures.AlgoOrderTypeTrailingStopMarket).
		ActivatePrice(actPriceStr).
		CallbackRate(cbStr).
		Quantity(qtyStr).
		WorkingType(futures.WorkingTypeMarkPrice)
	if side == "SHORT" {
		svc.Side(futures.SideTypeBuy).PositionSide(futures.PositionSideTypeShort)
	} else {
		svc.Side(futures.SideTypeSell).PositionSide(futures.PositionSideTypeLong)
	}
	resp, err := svc.Do(ctx)
	if err != nil {
		log.Printf("[Binance] ❌ PlaceTrailingStop 失败 %s activatePrice=%s callbackRate=%s: %v", symbol, actPriceStr, cbStr, err)
		return nil, fmt.Errorf("挂跟踪止损条件单失败 %s: %w", symbol, err)
	}

	log.Printf("[Binance] ✅ TRAILING_STOP_MARKET 条件单已创建 %s algoId=%d activatePrice=%s callbackRate=%s", symbol, resp.AlgoId, actPriceStr, cbStr)
	return &OrderResult{
		AlgoID: resp.AlgoId,
		Symbol: symbol,
		Side:   "SELL",
		Status: string(resp.AlgoStatus),
	}, nil
}

// PlaceTakeProfit 挂出固定止盈市价条件单（Algo Order API）
// 当价格达到 takeProfitPrice（入场价*(1+TakeProfitPct)）时，以市价平掉持仓
// 与跟踪止损先到先平：价格先触固定止盈价 → 止盈离场；先触发跟踪激活回撤 → 跟踪离场
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// takeProfitPrice: 止盈触发价格
// side: 方向（LONG/SHORT）
// 返回 *OrderResult 下单结果（含 AlgoID），error 错误信息
func (c *Client) PlaceTakeProfit(ctx context.Context, symbol string, takeProfitPrice float64, side string) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), AlgoID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "NEW"}, nil
	}
	tpPriceStr := c.FormatPrice(symbol, takeProfitPrice)

	svc := c.futuresClient.NewCreateAlgoOrderService().
		Symbol(symbol).
		Type(futures.AlgoOrderTypeTakeProfitMarket).
		TriggerPrice(tpPriceStr).
		ClosePosition(true).
		WorkingType(futures.WorkingTypeMarkPrice)
	if side == "SHORT" {
		svc.Side(futures.SideTypeBuy).PositionSide(futures.PositionSideTypeShort)
	} else {
		svc.Side(futures.SideTypeSell).PositionSide(futures.PositionSideTypeLong)
	}
	resp, err := svc.Do(ctx)
	if err != nil {
		log.Printf("[Binance] ❌ PlaceTakeProfit 失败 %s triggerPrice=%s: %v", symbol, tpPriceStr, err)
		return nil, fmt.Errorf("挂固定止盈条件单失败 %s: %w", symbol, err)
	}

	log.Printf("[Binance] ✅ TAKE_PROFIT_MARKET 条件单已创建 %s algoId=%d triggerPrice=%s", symbol, resp.AlgoId, tpPriceStr)
	return &OrderResult{
		AlgoID: resp.AlgoId,
		Symbol: symbol,
		Side:   "SELL",
		Status: string(resp.AlgoStatus),
	}, nil
}

// UpdateStopMarketPrice 更新止损单触发价（撤旧挂新）
// Algo Order API 无修改接口，需先取消旧单再创建新单
// 参数：oldAlgoID 旧条件单 ID，symbol 交易对，newTriggerPrice 新触发价，side 方向（LONG/SHORT）
// 返回值：新单结果，错误信息
func (c *Client) UpdateStopMarketPrice(ctx context.Context, oldAlgoID int64, symbol string, newTriggerPrice float64, side string) (*OrderResult, error) {
	if c.isDryRun() {
		return &OrderResult{OrderID: c.nextDryRunOrderID(), AlgoID: c.nextDryRunOrderID(), Symbol: symbol, Side: "SELL", Status: "NEW"}, nil
	}
	// 先取消旧单
	if err := c.CancelAlgoOrder(ctx, oldAlgoID); err != nil {
		log.Printf("[Binance] ⚠️ UpdateStopMarketPrice 取消旧单失败 %s algoId=%d: %v（继续创建新单）", symbol, oldAlgoID, err)
	}
	// 再创建新单
	result, err := c.PlaceStopMarket(ctx, symbol, newTriggerPrice, side)
	if err != nil {
		return nil, fmt.Errorf("更新止损价失败 %s: %w", symbol, err)
	}
	log.Printf("[Binance] ✅ 止损价已更新 %s oldAlgoID=%d → newAlgoID=%d price=%.6f", symbol, oldAlgoID, result.AlgoID, newTriggerPrice)
	return result, nil
}

// CancelOrder 取消指定委托单
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// orderID: 委托单 ID
// 返回 error 错误信息
func (c *Client) CancelOrder(ctx context.Context, symbol string, orderID int64) error {
	if c.isDryRun() {
		return nil
	}
	_, err := c.futuresClient.NewCancelOrderService().
		Symbol(symbol).
		OrderID(orderID).
		Do(ctx)
	if err != nil {
		return fmt.Errorf("取消委托失败 %s OrderID=%d: %w", symbol, orderID, err)
	}

	return nil
}

// GetOrderStatus 查询单个委托状态
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// orderID: 委托单 ID
// 返回 *OrderInfo 委托详情，error 错误信息
func (c *Client) GetOrderStatus(ctx context.Context, symbol string, orderID int64) (*OrderInfo, error) {
	if c.isDryRun() {
		return &OrderInfo{OrderID: orderID, Symbol: symbol, Status: "NEW"}, nil
	}
	order, err := c.futuresClient.NewGetOrderService().
		Symbol(symbol).
		OrderID(orderID).
		Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("查询委托失败 %s OrderID=%d: %w", symbol, orderID, err)
	}

	return &OrderInfo{
		OrderID:      order.OrderID,
		Symbol:       order.Symbol,
		Type:         string(order.Type),
		Side:         string(order.Side),
		Status:       string(order.Status),
		StopPrice:    mustParseFloat(order.StopPrice),
		FilledPrice:  mustParseFloat(order.AvgPrice),
		FilledAmount: mustParseFloat(order.ExecutedQuantity),
		CreatedAt:    order.Time,
		UpdatedAt:    order.UpdateTime,
	}, nil
}

// GetOrderFee 查询某委托的累计手续费（USDT）
// 通过成交明细接口（/fapi/v1/userTrades）拉取该委托的全部成交，累加每笔 commission。
// 参数:
//   - ctx: 请求上下文
//   - symbol: 交易对（如 "BTCUSDT"）
//   - orderID: 交易所委托 ID
//
// 返回:
//   - float64: 累计手续费（USDT，可能为 0）
//   - error: 查询失败时返回错误
func (c *Client) GetOrderFee(ctx context.Context, symbol string, orderID int64) (float64, error) {
	if c.isDryRun() {
		return 0, nil
	}
	trades, err := c.futuresClient.NewListAccountTradeService().
		Symbol(symbol).
		OrderID(orderID).
		Do(ctx)
	if err != nil {
		return 0, fmt.Errorf("查询成交明细失败 %s OrderID=%d: %w", symbol, orderID, err)
	}

	var total float64
	for _, t := range trades {
		total += mustParseFloat(t.Commission)
	}
	return total, nil
}

// GetPositionMode 查询账户持仓模式
// ctx: 请求上下文
// 返回 bool true=双向持仓(Hedge Mode, dualSidePosition=true)/false=单向持仓(One-way Mode)，error 错误信息
func (c *Client) GetPositionMode(ctx context.Context) (bool, error) {
	if c.isDryRun() {
		return true, nil // DRY_RUN 假定双向持仓
	}
	res, err := c.futuresClient.NewGetPositionModeService().Do(ctx)
	if err != nil {
		return false, fmt.Errorf("查询持仓模式失败: %w", err)
	}
	return res.DualSidePosition, nil
}

// SetPositionMode 设置账户持仓模式
// ctx: 请求上下文
// dual: true=双向持仓(Hedge Mode), false=单向持仓(One-way Mode)
// 返回 error 错误信息
// 说明: 账户存在持仓时切换模式会被交易所拒绝。
func (c *Client) SetPositionMode(ctx context.Context, dual bool) error {
	if c.isDryRun() {
		return nil
	}
	if err := c.futuresClient.NewChangePositionModeService().DualSide(dual).Do(ctx); err != nil {
		return fmt.Errorf("设置持仓模式失败(dual=%v): %w", dual, err)
	}
	return nil
}

// EnsureHedgeMode 确保账户处于双向持仓模式（Hedge Mode）
// 策略下单硬编码 positionSide=LONG，仅在双向持仓模式下有效（单向持仓会报 -4061）；
// 若当前为单向持仓则自动切换为双向持仓。
// ctx: 请求上下文
// 返回 error 查询或切换失败时返回（账户存在单向持仓仓位时切换会被交易所拒绝）
func (c *Client) EnsureHedgeMode(ctx context.Context) error {
	dual, err := c.GetPositionMode(ctx)
	if err != nil {
		return err
	}
	if dual {
		return nil
	}
	return c.SetPositionMode(ctx, true)
}

// SetLeverage 设置指定交易对的初始杠杆
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// leverage: 杠杆倍数（如 10）
// 返回 error 错误信息
// 说明: 确保交易所实际杠杆与策略配置一致，
// 避免真实保证金/盈亏与前端展示口径不符。
func (c *Client) SetLeverage(ctx context.Context, symbol string, leverage int) error {
	if c.isDryRun() {
		return nil
	}
	if _, err := c.futuresClient.NewChangeLeverageService().Symbol(symbol).Leverage(leverage).Do(ctx); err != nil {
		return fmt.Errorf("设置杠杆失败 %s(%dx): %w", symbol, leverage, err)
	}
	return nil
}

// EnsureLeverage 确保指定交易对已设置目标杠杆（每个交易对仅设置一次，成功后缓存）
// 适用于动态发现交易对的策略：在首次开仓前调用，避免每次开仓重复请求交易所。
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// leverage: 目标杠杆倍数
// 返回 error 设置失败时返回（失败不缓存，下次仍会重试）
func (c *Client) EnsureLeverage(ctx context.Context, symbol string, leverage int) error {
	c.leverageMu.Lock()
	done := c.leverageSet[symbol]
	c.leverageMu.Unlock()
	if done {
		return nil
	}
	if err := c.SetLeverage(ctx, symbol, leverage); err != nil {
		return err
	}
	c.leverageMu.Lock()
	c.leverageSet[symbol] = true
	c.leverageMu.Unlock()
	return nil
}

// SetMarginType 设置指定交易对的保证金模式（逐仓/全仓）
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// marginType: 保证金模式（ISOLATED=逐仓, CROSSED=全仓）
// 返回 error 错误信息
// 说明: 币安对已设置相同模式的交易对返回错误码 -4046（"No need to change margin type"），
// 此错误视为成功（幂等操作）。
func (c *Client) SetMarginType(ctx context.Context, symbol string, marginType string) error {
	if c.isDryRun() {
		return nil
	}
	err := c.futuresClient.NewChangeMarginTypeService().
		Symbol(symbol).
		MarginType(futures.MarginType(marginType)).
		Do(ctx)
	if err != nil {
		// -4046: "No need to change margin type" — 已是目标模式，视为成功
		if strings.Contains(err.Error(), "-4046") {
			return nil
		}
		return fmt.Errorf("设置保证金模式失败 %s(%s): %w", symbol, marginType, err)
	}
	return nil
}

// EnsureMarginMode 确保指定交易对已设置目标保证金模式（每个交易对仅设置一次，成功后缓存）
// 参考 EnsureLeverage 的缓存机制，避免每次开仓重复请求交易所。
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"）
// marginType: 目标保证金模式（ISOLATED/CROSSED）
// 返回 error 设置失败时返回（失败不缓存，下次仍会重试）
func (c *Client) EnsureMarginMode(ctx context.Context, symbol string, marginType string) error {
	c.marginModeMu.Lock()
	done := c.marginModeSet[symbol]
	c.marginModeMu.Unlock()
	if done {
		return nil
	}
	if err := c.SetMarginType(ctx, symbol, marginType); err != nil {
		return err
	}
	c.marginModeMu.Lock()
	c.marginModeSet[symbol] = true
	c.marginModeMu.Unlock()
	return nil
}

// GetOpenOrders 查询未成交委托列表
// ctx: 请求上下文
// symbol: 交易对（如 "BTCUSDT"），为空时查询所有交易对
// 返回 []OrderInfo 未成交委托列表，error 错误信息
func (c *Client) GetOpenOrders(ctx context.Context, symbol string) ([]OrderInfo, error) {
	if c.isDryRun() {
		return []OrderInfo{}, nil
	}
	svc := c.futuresClient.NewListOpenOrdersService()
	if symbol != "" {
		svc.Symbol(symbol)
	}

	orders, err := svc.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("查询未成交委托失败: %w", err)
	}

	result := make([]OrderInfo, 0, len(orders))
	for _, order := range orders {
		result = append(result, OrderInfo{
			OrderID:      order.OrderID,
			Symbol:       order.Symbol,
			Type:         string(order.Type),
			Side:         string(order.Side),
			Status:       string(order.Status),
			StopPrice:    mustParseFloat(order.StopPrice),
			FilledPrice:  mustParseFloat(order.AvgPrice),
			FilledAmount: mustParseFloat(order.ExecutedQuantity),
			CreatedAt:    order.Time,
			UpdatedAt:    order.UpdateTime,
		})
	}
	return result, nil
}

// mustParseFloat 安全解析浮点数
func mustParseFloat(s string) float64 {
	var f float64
	fmt.Sscanf(s, "%f", &f)
	return f
}

// GetFuturesBalance 获取合约账户余额信息
// SIMULATION/LIVE 调用币安合约 API
// ctx: 请求上下文
// 返回: *AccountBalance 账户余额信息, error 错误信息
func (c *Client) GetFuturesBalance(ctx context.Context) (*AccountBalance, error) {
	if c.isDryRun() {
		return &AccountBalance{TotalWalletBalance: 10000, AvailableBalance: 10000}, nil
	}
	const attempts = 3
	var balances []*futures.Balance
	var err error
	for i := 0; i < attempts; i++ {
		balances, err = c.futuresClient.NewGetBalanceService().Do(ctx)
		if err == nil {
			break
		}
		if !isTransientErr(err) || i == attempts-1 {
			return nil, fmt.Errorf("获取账户余额失败: %w", err)
		}
		log.Printf("[Binance] GetFuturesBalance 瞬时错误，重试 %d/%d: %v", i+1, attempts, err)
		time.Sleep(time.Duration(i+1) * time.Second)
	}

	// 查找 USDT 资产
	for _, b := range balances {
		if b.Asset == "USDT" {
			wallet := mustParseFloat(b.Balance)
			unrealized := mustParseFloat(b.CrossUnPnl)
			available := mustParseFloat(b.AvailableBalance)
			return &AccountBalance{
				TotalWalletBalance: wallet,
				TotalUnrealizedPnl: unrealized,
				TotalMarginBalance: wallet + unrealized,
				AvailableBalance:   available,
			}, nil
		}
	}

	return &AccountBalance{}, nil
}

// GetPositionRisk 查询交易所持仓风险信息
// 返回所有持仓（含未持仓的空条目），调用方需自行过滤 PositionAmt != 0 的条目。
// ctx: 请求上下文
// symbol: 交易对（空字符串查询全部）
// 返回: []ExchangePosition 持仓列表, error 错误信息
func (c *Client) GetPositionRisk(ctx context.Context, symbol string) ([]ExchangePosition, error) {
	if c.isDryRun() {
		return nil, nil
	}
	svc := c.futuresClient.NewGetPositionRiskService()
	if symbol != "" {
		svc.Symbol(symbol)
	}
	risks, err := svc.Do(ctx)
	if err != nil {
		return nil, fmt.Errorf("查询持仓风险失败: %w", err)
	}
	result := make([]ExchangePosition, 0, len(risks))
	for _, r := range risks {
		amt := mustParseFloat(r.PositionAmt)
		if amt == 0 {
			continue // 跳过无持仓条目
		}
		lev, _ := strconv.Atoi(r.Leverage)
		result = append(result, ExchangePosition{
			Symbol:           r.Symbol,
			PositionSide:     r.PositionSide,
			PositionAmt:      amt,
			EntryPrice:       mustParseFloat(r.EntryPrice),
			MarkPrice:        mustParseFloat(r.MarkPrice),
			UnRealizedProfit: mustParseFloat(r.UnRealizedProfit),
			Leverage:         lev,
			LiquidationPrice: mustParseFloat(r.LiquidationPrice),
			MarginType:       r.MarginType,
		})
	}
	return result, nil
}

// IsAPIErrorCode 判断 error 是否为指定 code 的币安 API 错误
// 用于区分可恢复错误（如 -2027 仓位超限）与不可恢复错误
// 使用 errors.As 穿透 fmt.Errorf %w 包装层，避免包装后无法识别
func IsAPIErrorCode(err error, code int64) bool {
	var apiErr *common.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code == code
	}
	return false
}

// APIErrorCode 提取币安 API 错误的错误码
// err: 待检查的错误（可被 fmt.Errorf %w 包装）
// 返回: (错误码, 是否为币安 API 错误)；非币安错误返回 (0, false)
func APIErrorCode(err error) (int64, bool) {
	var apiErr *common.APIError
	if errors.As(err, &apiErr) {
		return apiErr.Code, true
	}
	return 0, false
}

// CancelAlgoOrder 取消指定条件单（Algo Order API）
// ctx: 请求上下文
// algoID: 条件单 ID（PlaceStopMarket / PlaceTrailingStop 返回的 AlgoID）
// 返回 error 错误信息
func (c *Client) CancelAlgoOrder(ctx context.Context, algoID int64) error {
	if c.isDryRun() {
		return nil
	}
	_, err := c.futuresClient.NewCancelAlgoOrderService().
		AlgoID(algoID).
		Do(ctx)
	if err != nil {
		log.Printf("[Binance] ❌ CancelAlgoOrder 失败 algoId=%d: %v", algoID, err)
		return fmt.Errorf("取消条件单失败 algoId=%d: %w", algoID, err)
	}
	log.Printf("[Binance] ✅ 条件单已取消 algoId=%d", algoID)
	return nil
}

// GetAlgoOrderStatus 查询单个条件单状态（Algo Order API）
// 网络瞬时错误（代理/交易所 EOF、连接重置等）最多重试 3 次，
// 避免单次连接中断导致同一条件单每 Tick 反复查询失败。
// ctx: 请求上下文
// algoID: 条件单 ID
// 返回 *OrderInfo 条件单详情（含 AlgoStatus、ActualOrderID），error 错误信息
func (c *Client) GetAlgoOrderStatus(ctx context.Context, algoID int64) (*OrderInfo, error) {
	if c.isDryRun() {
		return &OrderInfo{AlgoID: algoID, AlgoStatus: AlgoStatusNew, Status: OrderStatusNew}, nil
	}

	const attempts = 3
	var resp *futures.GetAlgoOrderResp
	var err error
	for i := 0; i < attempts; i++ {
		resp, err = c.futuresClient.NewGetAlgoOrderService().
			AlgoID(algoID).
			Do(ctx)
		if err == nil {
			break
		}
		if !isTransientErr(err) || i == attempts-1 {
			log.Printf("[Binance] ❌ GetAlgoOrderStatus 失败 algoId=%d: %v", algoID, err)
			return nil, fmt.Errorf("查询条件单失败 algoId=%d: %w", algoID, err)
		}
		log.Printf("[Binance] ⚠️ GetAlgoOrderStatus 瞬时错误 algoId=%d，重试 %d/%d: %v", algoID, i+1, attempts, err)
		// 重试间隔短于行情重试：SyncOrders 每 Tick 可能查询多个条件单，避免挤占 10s Tick 预算
		time.Sleep(time.Duration(i+1) * 500 * time.Millisecond)
	}

	// 将 algoStatus 映射到通用 Status：
	// NEW → NEW（活跃）；CANCELED/REJECTED/EXPIRED → 对应状态
	status := OrderStatusNew
	switch resp.AlgoStatus {
	case futures.AlgoOrderStatusTypeCanceled:
		status = OrderStatusCanceled
	case futures.AlgoOrderStatusTypeRejected:
		status = OrderStatusRejected
	case futures.AlgoOrderStatusTypeExpired:
		status = OrderStatusExpired
	}

	return &OrderInfo{
		AlgoID:        resp.AlgoId,
		Symbol:        resp.Symbol,
		Type:          string(resp.OrderType),
		Side:          string(resp.Side),
		Status:        status,
		AlgoStatus:    string(resp.AlgoStatus),
		ActualOrderID: mustParseInt64(resp.ActualOrderId),
		StopPrice:     mustParseFloat(resp.TriggerPrice),
		FilledPrice:   mustParseFloat(resp.ActualPrice),
		CreatedAt:     resp.CreateTime,
		UpdatedAt:     resp.UpdateTime,
	}, nil
}

// GetOpenAlgoOrders 查询当前活跃的条件单列表（Algo Order API）
// ctx: 请求上下文
// symbol: 交易对（空字符串查询全部）
// 返回 []OrderInfo 条件单列表，error 错误信息
func (c *Client) GetOpenAlgoOrders(ctx context.Context, symbol string) ([]OrderInfo, error) {
	if c.isDryRun() {
		return []OrderInfo{}, nil
	}
	svc := c.futuresClient.NewListOpenAlgoOrdersService()
	if symbol != "" {
		svc.Symbol(symbol)
	}
	resp, err := svc.Do(ctx)
	if err != nil {
		log.Printf("[Binance] ❌ GetOpenAlgoOrders 失败: %v", err)
		return nil, fmt.Errorf("查询活跃条件单失败: %w", err)
	}

	result := make([]OrderInfo, 0, len(resp))
	for _, o := range resp {
		result = append(result, OrderInfo{
			AlgoID:        o.AlgoId,
			Symbol:        o.Symbol,
			Type:          string(o.OrderType),
			Side:          string(o.Side),
			Status:        OrderStatusNew,
			AlgoStatus:    string(o.AlgoStatus),
			ActualOrderID: mustParseInt64(o.ActualOrderId),
			StopPrice:     mustParseFloat(o.TriggerPrice),
			CreatedAt:     o.CreateTime,
			UpdatedAt:     o.UpdateTime,
		})
	}
	return result, nil
}

// mustParseInt64 安全解析 int64（字符串 → int64）
func mustParseInt64(s string) int64 {
	var n int64
	fmt.Sscanf(s, "%d", &n)
	return n
}
