// Package binance 币安 REST API 封装
package binance

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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
	"golang.org/x/net/proxy"
)

// Client 币安 API 客户端
type Client struct {
	futuresClient *futures.Client
	mode          string // DRY_RUN | SIMULATION | LIVE

	// 实际选中的代理 URL（NewClient 候选链探测后的最终结果，nil=直连）。
	// 仅供「测试连接」诊断报告使用，不参与请求逻辑。
	proxyURL *url.URL

	// 精度缓存：symbol -> SymbolPrecision
	precisionMu  sync.RWMutex
	precisionMap map[string]SymbolPrecision
	// 精度规则加载锁：防止并发开仓时重复加载 exchangeInfo
	precisionLoadMu sync.Mutex

	// 上市日期缓存：symbol -> onboardDate（Unix 毫秒，来自 exchangeInfo）
	onboardDateMu  sync.RWMutex
	onboardDateMap map[string]int64

	// 杠杆设置缓存：已成功设置目标杠杆的交易对（避免每次开仓重复调用）
	leverageMu  sync.Mutex
	leverageSet map[string]bool

	// 保证金模式缓存：已成功设置目标模式的交易对（避免每次开仓重复调用）
	marginModeMu  sync.Mutex
	marginModeSet map[string]bool

	// DRY_RUN 模式模拟 OrderID 生成器（时间戳×1000+计数器，避免同毫秒重复）
	dryRunOrderID atomic.Int64
}

// localProxyPorts 常见本地代理端口（按优先级）：
// Clash Verge(7897), Clash(7890/7891), V2Ray(1087/1080), SS(1080), 用户设定 V2Ray(10808)
var localProxyPorts = []int{7897, 7890, 7891, 1087, 1080, 8118, 10808}

// remoteProxyCandidates 远端兜底代理（用户设定 2026-08-05）：
// 仅当本机无可用代理时才尝试；每级都做 HTTP 实测，转发不可用的不会误选。
var remoteProxyCandidates = []struct {
	addr    string
	timeout time.Duration
}{
	{"45.251.241.89:49988", 5 * time.Second},
}

// probeProxy 探测代理地址实际支持的协议，并返回带 scheme 的可用代理 URL：
// 依次实测 HTTP 与 SOCKS5 两种协议（通过代理请求币安公开时间接口，200 才算可用），
// 仅 TCP 通而无法转发 HTTPS 的"死代理"（如端口开放但服务异常）不会被误选。
// addr: 代理地址（host:port）
// mode: 运行模式（SIMULATION 用 demo 域名实测，其余用实盘域名）
// timeout: 单次协议探测超时
// 返回带协议前缀的代理 URL；两种协议均不可用时返回 nil（由调用方决定降级）
func probeProxy(addr, mode string, timeout time.Duration) *url.URL {
	// 1) TCP 握手（快速过滤未监听的端口）
	conn, err := net.DialTimeout("tcp", addr, timeout)
	if err != nil {
		return nil
	}
	conn.Close()

	// 用合约平台真实存在的时间接口做实测（/api/v1/time 在 fapi 域名上不存在，
	// 会返回 CloudFront 403 导致误判代理不可用；实测 fapi/demo-fapi 的 /fapi/v1/time 均返回 200）
	testURL := "https://fapi.binance.com/fapi/v1/time"
	if mode == "SIMULATION" {
		testURL = "https://demo-fapi.binance.com/fapi/v1/time"
	}

	// 2) 先试 HTTP 代理（绝大多数本地代理客户端默认 HTTP 协议）
	if u, _ := url.Parse("http://" + addr); testProxyClient(newProxiedHTTPClient(u), testURL, timeout) {
		return u
	}
	// 3) 再试 SOCKS5 代理（v2rayN 默认 10808 为 Socks 端口，Clash 等也常提供）
	if u, _ := url.Parse("socks5://" + addr); testProxyClient(newProxiedHTTPClient(u), testURL, timeout) {
		return u
	}
	return nil
}

// testProxyClient 通过指定 HTTP 客户端请求测试 URL，返回是否成功（HTTP 200）
// client: 已配置代理的 HTTP 客户端
// testURL: 探测用的币安时间接口地址
// timeout: 请求超时
// 返回是否可用
func testProxyClient(client *http.Client, testURL string, timeout time.Duration) bool {
	client.Timeout = timeout + 2*time.Second
	ctx, cancel := context.WithTimeout(context.Background(), timeout+2*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, testURL, nil)
	if err != nil {
		return false
	}
	resp, err := client.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// detectLocalProxy 检测本机可用代理（本地优先：延迟低、更稳）
// 遍历常见本地端口，第一个通过 TCP+HTTP/SOCKS5 实测的即返回；全部不可用返回 nil（直连）
// mode: 运行模式（用于实测域名选择）
func detectLocalProxy(mode string) *url.URL {
	for _, port := range localProxyPorts {
		addr := fmt.Sprintf("127.0.0.1:%d", port)
		if u := probeProxy(addr, mode, 1500*time.Millisecond); u != nil {
			log.Printf("[Binance] 检测到可用本地代理: %s（协议: %s）", addr, u.Scheme)
			return u
		}
	}
	log.Printf("[Binance] 未检测到可用本地代理")
	return nil
}

// newProxiedHTTPClient 创建带代理的 HTTP 客户端
// 支持两种代理协议：http/https（标准 Proxy 机制）与 socks5（x/net/proxy 拨号器）；
// proxyURL 为 nil 时使用直连
// proxyURL: 代理地址（含协议前缀，如 http://127.0.0.1:7897 或 socks5://127.0.0.1:10808）
func newProxiedHTTPClient(proxyURL *url.URL) *http.Client {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != nil {
		switch proxyURL.Scheme {
		case "socks5", "socks5h":
			// SOCKS5 代理：标准库 http.Transport 的 Proxy 机制不支持 socks5，
			// 需用 x/net/proxy 建立 SOCKS5 拨号器并通过 DialContext 注入
			d, err := proxy.SOCKS5("tcp", proxyURL.Host, nil, proxy.Direct)
			if err == nil {
				transport.Proxy = nil
				transport.DialContext = (&socks5Dialer{d: d}).DialContext
			}
		default: // http / https
			transport.Proxy = http.ProxyURL(proxyURL)
		}
	}
	return &http.Client{
		Transport: transport,
		Timeout:   30 * time.Second,
	}
}

// socks5Dialer 将 x/net/proxy 的 Dialer 适配为 http.Transport.DialContext
// 优先使用底层拨号器的 DialContext（支持 context 取消）；不支持的退化为普通 Dial
type socks5Dialer struct {
	d proxy.Dialer
}

// DialContext 拨号方法（实现 http.Transport.DialContext 签名）
// ctx: 拨号上下文
// network: 网络类型（tcp）
// addr: 目标地址（host:port）
// 返回已建立的连接或错误
func (s *socks5Dialer) DialContext(ctx context.Context, network, addr string) (net.Conn, error) {
	if cd, ok := s.d.(proxy.ContextDialer); ok {
		return cd.DialContext(ctx, network, addr)
	}
	return s.d.Dial(network, addr)
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
	} else {
		// 关键：UseDemo 是 go-binance 的包级全局变量。若此前切过模拟盘，
		// 它会残留为 true，导致实盘(LIVE)/DRY_RUN 的 REST 与 WS 全部误发到
		// demo-fapi.binance.com——实盘 Key 在测试网返回 -2015（Invalid API-key）。
		// 非模拟盘模式必须显式重置，REST 与 WS 才回到主网域名。
		futures.UseDemo = false
	}

	fut := futures.NewClient(apiKey, apiSecret)

	// 代理解析优先级（每个候选都做 TCP+HTTP/SOCKS5 实测，转发不可用的"死代理"不会误选）：
	//   1. 用户指定代理（前端设置/数据库保存，最高优先）
	//   2. 本地常见端口自动检测（7897/7890/10808 等，延迟低更稳）
	//   3. 远端兜底代理（45.251.241.89:49988，用户设定 2026-08-05）
	//   4. 直连
	var proxyURL *url.URL
	if proxyAddr != "" && proxyPort > 0 {
		addr := fmt.Sprintf("%s:%d", proxyAddr, proxyPort)
		if u := probeProxy(addr, mode, 3*time.Second); u != nil {
			proxyURL = u
			log.Printf("[Binance] 使用用户指定代理: %s（协议: %s）", addr, u.Scheme)
		} else {
			log.Printf("[Binance] 指定代理 %s 验证失败（TCP/HTTP/SOCKS5 实测），尝试本地自动检测", addr)
		}
	}
	if proxyURL == nil {
		proxyURL = detectLocalProxy(mode)
	}
	if proxyURL == nil {
		for _, rp := range remoteProxyCandidates {
			// 与用户指定代理相同则跳过（已实测过）
			if proxyAddr != "" && proxyPort > 0 && rp.addr == fmt.Sprintf("%s:%d", proxyAddr, proxyPort) {
				continue
			}
			if u := probeProxy(rp.addr, mode, rp.timeout); u != nil {
				proxyURL = u
				log.Printf("[Binance] 使用远端兜底代理: %s（协议: %s）", rp.addr, u.Scheme)
				break
			}
			log.Printf("[Binance] 远端代理 %s 验证失败，尝试下一个", rp.addr)
		}
	}
	if proxyURL == nil {
		log.Printf("[Binance] 未找到可用代理，使用直连")
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
		futuresClient:  fut,
		mode:           mode,
		proxyURL:       proxyURL,
		precisionMap:   make(map[string]SymbolPrecision),
		onboardDateMap: make(map[string]int64),
		leverageSet:    make(map[string]bool),
		marginModeSet:  make(map[string]bool),
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
	om := make(map[string]int64, len(info.Symbols))
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
		// 顺带缓存上市日期（新币过滤用）；onboardDate 缺失/为 0 的合约不缓存
		if s.OnboardDate > 0 {
			om[s.Symbol] = s.OnboardDate
		}
	}

	c.precisionMu.Lock()
	c.precisionMap = m
	c.precisionMu.Unlock()
	c.onboardDateMu.Lock()
	c.onboardDateMap = om
	c.onboardDateMu.Unlock()

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

// GetOnboardDate 获取交易对上市日期（Unix 毫秒）。
// 数据来自启动时 LoadExchangeInfo 缓存的 exchangeInfo.onboardDate；
// 返回 ok=false 表示未知（exchangeInfo 未加载/加载失败/该币无上市日期数据）。
// symbol: 交易对（如 "BTCUSDT"）
// 返回上市日期（Unix 毫秒）与是否存在
func (c *Client) GetOnboardDate(symbol string) (int64, bool) {
	c.onboardDateMu.RLock()
	defer c.onboardDateMu.RUnlock()
	if len(c.onboardDateMap) == 0 {
		return 0, false
	}
	d, ok := c.onboardDateMap[symbol]
	return d, ok
}

// SetOnboardDatesForTest 仅供测试注入上市日期数据（新币过滤逻辑测试用），不参与生产流程。
// dates: symbol -> 上市日期（Unix 毫秒）；nil 表示清空（回到未加载状态）
func (c *Client) SetOnboardDatesForTest(dates map[string]int64) {
	c.onboardDateMu.Lock()
	if dates == nil {
		c.onboardDateMap = make(map[string]int64)
	} else {
		c.onboardDateMap = dates
	}
	c.onboardDateMu.Unlock()
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

// TestConnection 验证 API Key 认证是否可用（只读，绝不下单）。
// 以标准 HMAC-SHA256 签名请求账户只读接口 positionSide/dual，
// 直接读取币安原始响应，返回完整诊断结果，便于在 Windows 上一键定位认证问题：
//   - ok: 认证是否成功（true/false）
//   - mode: 当前运行模式
//   - domain: 实际请求的币安域名（fapi.binance.com=实盘 / demo-fapi.binance.com=测试网）
//   - proxy: 实际使用的代理链路（用户指定/自动检测/直连）
//   - network: 网络链路自检结果（无签名公开接口，区分"网络/代理问题"与"Key 问题"）
//   - exit_ip: 经当前代理链获取的出口公网 IP（供与币安 IP 白名单对比）
//   - message: 成功摘要或失败时的完整错误原文（含 code/msg/request ip）
func (c *Client) TestConnection(ctx context.Context) map[string]string {
	result := map[string]string{"ok": "false", "mode": c.mode}

	if c.futuresClient == nil {
		result["message"] = "客户端未初始化"
		return result
	}
	// 诊断信息先取（不依赖网络）
	result["domain"] = c.futuresClient.BaseURL
	if c.proxyURL != nil {
		result["proxy"] = c.proxyURL.String()
	} else {
		result["proxy"] = "直连（无代理）"
	}
	if c.isDryRun() {
		result["message"] = "DRY_RUN 模式无真实凭据，无法测试连接"
		return result
	}
	if c.futuresClient.APIKey == "" || c.futuresClient.SecretKey == "" {
		result["message"] = "尚未填写 API Key / Secret"
		return result
	}

	// 1) 网络链路自检：请求同一域名下的无签名公开接口。
	//    失败 = 网络/代理问题（与 Key 无关）；成功 = 链路通，认证失败才是 Key/白名单问题。
	timeURL := strings.TrimRight(c.futuresClient.BaseURL, "/") + "/fapi/v1/time"
	if req, err := http.NewRequestWithContext(ctx, http.MethodGet, timeURL, nil); err == nil {
		if r, err := c.futuresClient.HTTPClient.Do(req); err != nil {
			result["network"] = "公开接口请求失败: " + err.Error()
		} else {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				result["network"] = "网络链路正常（公开接口返回 200）"
			} else {
				result["network"] = fmt.Sprintf("公开接口返回 HTTP %d（链路异常）", r.StatusCode)
			}
		}
	}

	// 2) 出口公网 IP（经同一代理链，供与币安 IP 白名单对比；失败不阻断主流程）
	if ip := c.publicIP(ctx); ip != "" {
		result["exit_ip"] = ip
	}

	// 3) 标准签名请求（与币安官方一致）：timestamp + HMAC-SHA256
	ts := time.Now().UnixMilli()
	query := fmt.Sprintf("timestamp=%d", ts)
	mac := hmac.New(sha256.New, []byte(c.futuresClient.SecretKey))
	mac.Write([]byte(query))
	sig := hex.EncodeToString(mac.Sum(nil))
	url := fmt.Sprintf("%s/fapi/v1/positionSide/dual?%s&signature=%s", c.futuresClient.BaseURL, query, sig)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		result["message"] = "构造请求失败: " + err.Error()
		return result
	}
	req.Header.Set("X-MBX-APIKEY", c.futuresClient.APIKey)

	resp, err := c.futuresClient.HTTPClient.Do(req)
	if err != nil {
		result["message"] = "网络请求失败: " + err.Error()
		return result
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)

	if resp.StatusCode == http.StatusOK {
		result["ok"] = "true"
		var v struct {
			DualSidePosition bool `json:"dualSidePosition"`
		}
		_ = json.Unmarshal(body, &v)
		msg := fmt.Sprintf("认证成功！双向持仓模式=%v", v.DualSidePosition)
		if bal, berr := c.GetFuturesBalance(ctx); berr == nil && bal != nil {
			msg += fmt.Sprintf("，保证金余额=%.2f USDT", bal.TotalMarginBalance)
		}
		result["message"] = msg
	} else {
		// 解析币安原始错误，提取 code/msg/request ip（-2015 时币安会返回其看到的出口 IP）
		var e struct {
			Code      int64  `json:"code"`
			Msg       string `json:"msg"`
			RequestIP string `json:"request ip"`
		}
		_ = json.Unmarshal(body, &e)
		if e.Code != 0 {
			msg := fmt.Sprintf("币安错误 %d: %s", e.Code, e.Msg)
			if e.RequestIP != "" {
				msg += fmt.Sprintf("（币安看到出口 IP: %s）", e.RequestIP)
			}
			result["message"] = msg
		} else {
			result["message"] = string(body)
		}
	}
	return result
}

// publicIP 经当前代理链获取出口公网 IP（仅诊断用）。
// 使用 HTTPS 接口 api.ipify.org（返回纯 IP）；失败返回空字符串，不阻断测试主流程。
// ctx: 请求上下文
// 返回出口 IP 字符串（失败为空）
func (c *Client) publicIP(ctx context.Context) string {
	ctx2, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx2, http.MethodGet, "https://api.ipify.org", nil)
	if err != nil {
		return ""
	}
	resp, err := c.futuresClient.HTTPClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	b, _ := io.ReadAll(resp.Body)
	ip := strings.TrimSpace(string(b))
	if ip == "" || len(ip) > 64 {
		return ""
	}
	return ip
}
