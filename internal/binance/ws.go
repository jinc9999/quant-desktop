// Package binance 币安 WebSocket 行情订阅
package binance

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	"golang.org/x/net/proxy"
)

// WsManager WebSocket 行情管理器
type WsManager struct {
	mode        string
	proxyURL    *url.URL
	stopCh      chan struct{}
	mu          sync.RWMutex
	priceCache  map[string]float64 // symbol -> last price
	tickerCache map[string]Ticker  // symbol -> 24h 行情摘要（来自全量行情流）
	lastUpdate  atomic.Int64       // 最后一次更新缓存的 Unix 毫秒时间，用于新鲜度判断
	startOnce   sync.Once          // 保证重连循环 goroutine 只启动一次（循环内部自动重连）
	stopOnce    sync.Once          // 保证 stopCh 只关闭一次，避免重复 Stop() 时 close 已关闭 channel 触发 panic
}

// NewWsManager 创建 WebSocket 管理器
func NewWsManager(mode string) *WsManager {
	return NewWsManagerWithProxy(mode, nil)
}

// NewWsManagerWithProxy 创建 WebSocket 管理器，并为行情流拨号指定代理。
func NewWsManagerWithProxy(mode string, proxyURL *url.URL) *WsManager {
	return &WsManager{
		mode:        mode,
		proxyURL:    proxyURL,
		stopCh:      make(chan struct{}),
		priceCache:  make(map[string]float64),
		tickerCache: make(map[string]Ticker),
	}
}

// wsMarketEndpoint 返回指定模式的全市场行情流地址。
// SIMULATION 使用 demo 域名，LIVE/DRY_RUN 使用主网域名（DRY_RUN 实际不发起连接）。
func wsMarketEndpoint(mode string) string {
	if mode == "SIMULATION" {
		return "wss://demo-fstream.binance.com/market/ws"
	}
	return "wss://fstream.binance.com/market/ws"
}

// wsMarketTicker 全市场 !ticker@arr 推送中的单币字段（仅取客户端需要的字段）。
// 自定义反序列化：Binance 推送里还有 C/L/O/E/n/st 等数字字段，Go 标准库的
// 字段匹配会把 C 误判为 c（收盘价），导致整条全量流解析失败；这里只精确取
// 需要的四个字段，并兼容个别字段偶发为数字的情况。
type wsMarketTicker struct {
	Symbol             string
	ClosePrice         string
	PriceChangePercent string
	QuoteVolume        string
}

func (t *wsMarketTicker) UnmarshalJSON(data []byte) error {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(data, &fields); err != nil {
		return err
	}
	fieldString := func(key string) (string, error) {
		raw, ok := fields[key]
		if !ok || string(raw) == "null" {
			return "", nil
		}
		var s string
		if err := json.Unmarshal(raw, &s); err == nil {
			return s, nil
		}
		var f float64
		if err := json.Unmarshal(raw, &f); err == nil {
			return strconv.FormatFloat(f, 'f', -1, 64), nil
		}
		var i int64
		if err := json.Unmarshal(raw, &i); err == nil {
			return strconv.FormatInt(i, 10), nil
		}
		return "", fmt.Errorf("字段 %s 既不是字符串也不是数字: %s", key, raw)
	}
	var err error
	if t.Symbol, err = fieldString("s"); err != nil {
		return err
	}
	if t.ClosePrice, err = fieldString("c"); err != nil {
		return err
	}
	if t.PriceChangePercent, err = fieldString("P"); err != nil {
		return err
	}
	if t.QuoteVolume, err = fieldString("q"); err != nil {
		return err
	}
	return nil
}

// GetPrice 获取缓存的最新价格
func (ws *WsManager) GetPrice(symbol string) (float64, bool) {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	price, ok := ws.priceCache[symbol]
	return price, ok
}

// StartAllMarketTicker 启动全市场行情流（!ticker@arr）的重连循环。
// 通过 startOnce 保证重连 goroutine 只启动一次；goroutine 内部在连接断开后
// 自动按指数退避重连，避免一次断线导致行情缓存永久冻结。
// 参数:
//   - ctx: 上下文，取消时停止订阅与重连
func (ws *WsManager) StartAllMarketTicker(ctx context.Context) {
	ws.startOnce.Do(func() {
		go ws.runAllMarketTickerLoop(ctx)
	})
}

// wsDialer 为行情流拨号创建 Dialer：HTTP(S) 代理走 Proxy，SOCKS5 代理走拨号器。
func wsDialer(proxyURL *url.URL) *websocket.Dialer {
	dialer := *websocket.DefaultDialer
	if proxyURL == nil {
		return &dialer
	}
	switch proxyURL.Scheme {
	case "socks5", "socks5h":
		if pd, err := proxy.SOCKS5("tcp", proxyURL.Host, nil, proxy.Direct); err == nil {
			if cd, ok := pd.(proxy.ContextDialer); ok {
				dialer.NetDialContext = cd.DialContext
			} else {
				dialer.NetDial = func(network, addr string) (net.Conn, error) {
					return pd.Dial(network, addr)
				}
			}
		}
	default:
		dialer.Proxy = http.ProxyURL(proxyURL)
	}
	return &dialer
}

// wsMarketTickerServe 连接全市场行情流，收到数组推送时调用 handler。
// 返回 doneC（连接断开或主动停止后关闭）与 stopC（调用方用于主动关闭连接）。
// 不读取 go-binance 的包级 UseDemo 全局，避免与运行时模式切换产生数据竞态；
// 且 stopC 被关闭时真正断开底层连接，避免停止策略后残留读 goroutine。
func wsMarketTickerServe(endpoint string, dialer *websocket.Dialer, handler func([]wsMarketTicker)) (doneC, stopC chan struct{}, err error) {
	conn, _, err := dialer.Dial(endpoint, nil)
	if err != nil {
		return nil, nil, err
	}
	doneC = make(chan struct{})
	stopC = make(chan struct{})
	go func() {
		defer close(doneC)
		defer conn.Close()
		go func() {
			select {
			case <-stopC:
				conn.Close()
			case <-doneC:
			}
		}()
		for {
			_, message, err := conn.ReadMessage()
			if err != nil {
				return
			}
			var events []wsMarketTicker
			if err := json.Unmarshal(message, &events); err != nil {
				log.Printf("[WS] 全量行情流解析失败: %v", err)
				continue
			}
			handler(events)
		}
	}()
	return doneC, stopC, nil
}

// runAllMarketTickerLoop 全量行情流重连循环。
// 连接断开或启动失败时按指数退避（1s→30s 封顶）自动重连，
// 直到 stopCh 关闭或 ctx 取消才退出。每次成功写入缓存会刷新 lastUpdate。
func (ws *WsManager) runAllMarketTickerLoop(ctx context.Context) {
	backoff := time.Second
	const maxBackoff = 30 * time.Second
	endpoint := wsMarketEndpoint(ws.mode) + "/!ticker@arr"
	for {
		// 启动前检查停止信号
		select {
		case <-ws.stopCh:
			return
		case <-ctx.Done():
			return
		default:
		}

		doneC, stopC, err := wsMarketTickerServe(endpoint, wsDialer(ws.proxyURL), func(events []wsMarketTicker) {
			// 一次推送为全市场数组，单次加锁批量更新，降低锁竞争
			ws.mu.Lock()
			for _, e := range events {
				last := mustParseFloat(e.ClosePrice)
				ws.priceCache[e.Symbol] = last
				ws.tickerCache[e.Symbol] = Ticker{
					Symbol:      e.Symbol,
					LastPrice:   last,
					PriceChange: mustParseFloat(e.PriceChangePercent),
					QuoteVolume: mustParseFloat(e.QuoteVolume),
				}
			}
			ws.mu.Unlock()
			ws.lastUpdate.Store(time.Now().UnixMilli())
		})

		if err != nil {
			log.Printf("[WS] 全量行情流启动失败: %v，%v 后重连", err, backoff)
		} else {
			log.Printf("[WS] 全量行情流已连接")
			backoff = time.Second // 连接成功，重置退避
			// 阻塞直到连接断开或收到停止信号
			select {
			case <-doneC:
				log.Printf("[WS] 全量行情流断开，%v 后重连", backoff)
			case <-ws.stopCh:
				close(stopC)
				return
			case <-ctx.Done():
				close(stopC)
				return
			}
		}

		// 退避等待，期间可被停止信号中断
		select {
		case <-time.After(backoff):
		case <-ws.stopCh:
			return
		case <-ctx.Done():
			return
		}
		backoff *= 2
		if backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

// GetTickers 返回缓存的全市场行情快照。
// 返回:
//   - []Ticker: 缓存为空时返回 nil（调用方据此回退到 REST）
func (ws *WsManager) GetTickers() []Ticker {
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	if len(ws.tickerCache) == 0 {
		return nil
	}
	out := make([]Ticker, 0, len(ws.tickerCache))
	for _, t := range ws.tickerCache {
		out = append(out, t)
	}
	return out
}

// BackfillCache 将 REST 获取的行情数据回填到 WS 缓存。
// 当 WS 连接失败或未启动时，策略引擎通过 REST 获取行情后调用此方法，
// 确保前端 GetPrice 能获取到实时价格（标记价格、盈亏、回报率计算依赖此缓存）。
// 参数:
//   - tickers: REST 获取的全市场行情列表
func (ws *WsManager) BackfillCache(tickers []Ticker) {
	ws.mu.Lock()
	defer ws.mu.Unlock()
	for _, t := range tickers {
		ws.priceCache[t.Symbol] = t.LastPrice
		ws.tickerCache[t.Symbol] = t
	}
	ws.lastUpdate.Store(time.Now().UnixMilli())
}

// CacheAge 返回缓存距最后一次更新的时长。
// 用于判断行情缓存是否新鲜：WS 断线或卡死时该值会持续增长，
// 调用方据此回退到 REST 刷新，避免使用冻结的旧价格。
// 返回: 缓存年龄；从未更新时返回 1 小时（视为过期）
func (ws *WsManager) CacheAge() time.Duration {
	ts := ws.lastUpdate.Load()
	if ts == 0 {
		return time.Hour
	}
	return time.Since(time.UnixMilli(ts))
}

// Stop 停止 WebSocket 订阅（幂等：多次调用安全，stopCh 仅关闭一次）
func (ws *WsManager) Stop() {
	ws.stopOnce.Do(func() {
		close(ws.stopCh)
	})
}
