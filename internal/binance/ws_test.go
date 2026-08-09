// Package binance 币安 WebSocket 行情订阅
package binance

import (
	"encoding/json"
	"net/url"
	"sync"
	"testing"
)

// TestWsManagerStopIdempotent 验证 Stop 幂等：多次调用不 panic
// 背景: 修复前 Stop() 直接 close(ws.stopCh)，重复调用会触发
// "close of closed channel" panic（应用退出时 OnShutdown + defer 双重调用即触发）。
// 修复后 stopOnce sync.Once 保证 stopCh 仅被 close 一次。
// 验证点: 连续多次调用 Stop 均不 panic
func TestWsManagerStopIdempotent(t *testing.T) {
	ws := NewWsManager("TESTNET")

	// 模拟应用退出时 OnShutdown 回调 + defer 双重调用，再补一次兜底
	ws.Stop()
	ws.Stop()
	ws.Stop()
}

// TestWsManagerStopConcurrent 验证 Stop 并发安全：多个 goroutine 同时调用不 panic
// 验证点: sync.Once 保证 stopCh 只被 close 一次，并发调用无竞态（配合 -race 运行）
func TestWsManagerStopConcurrent(t *testing.T) {
	ws := NewWsManager("TESTNET")

	var wg sync.WaitGroup
	for range 10 {
		wg.Go(func() {
			ws.Stop()
		})
	}
	wg.Wait()
}

// TestWsMarketTickerUnmarshalMixedTypes 验证全量行情流可解析真实推送。
// Binance 的 !ticker@arr 每条都带 C/L/O/E/n/st 等数字字段，Go 标准库会把
// 数字字段 C 误匹配到价格字段 c，导致整条流解析失败；自定义反序列化应只
// 精确读取需要的字段，且兼容字段偶发为数字的情况。
func TestWsMarketTickerUnmarshalMixedTypes(t *testing.T) {
	raw := []byte(`[{"e":"24hrTicker","E":1786261855766,"s":"BTCUSDT","ps":"BTCUSDT","p":"0.0040100","P":"6.754","w":"0.0613222","c":"0.0633800","Q":"85","o":"0.0593700","h":"0.0651700","l":"0.0582600","v":"21979663668","q":"216168740.5","O":1786261850000,"C":1786261855766,"F":1,"L":2,"n":3,"st":4}]`)
	var events []wsMarketTicker
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatalf("全量行情流解析失败: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("期望 1 个 ticker，实际 %d", len(events))
	}
	got := events[0]
	if got.Symbol != "BTCUSDT" || got.ClosePrice != "0.0633800" || got.PriceChangePercent != "6.754" || got.QuoteVolume != "216168740.5" {
		t.Errorf("字段提取错误: %+v", got)
	}
}

// TestWsMarketTickerUnmarshalNumeric 验证个别字段偶发为数字时也能解析。
func TestWsMarketTickerUnmarshalNumeric(t *testing.T) {
	raw := []byte(`[{"s":"ETHUSDT","c":0.3125,"P":2.1,"q":12345.6,"C":999}]`)
	var events []wsMarketTicker
	if err := json.Unmarshal(raw, &events); err != nil {
		t.Fatalf("数字字段解析失败: %v", err)
	}
	got := events[0]
	if got.ClosePrice != "0.3125" || got.PriceChangePercent != "2.1" || got.QuoteVolume != "12345.6" {
		t.Errorf("数字字段提取错误: %+v", got)
	}
}

// TestWsMarketEndpointSimulation 验证模拟盘使用 Binance demo 域名。
// Windows 侧与 Mac 侧行情不一致时，先确认两边是否连到了同一行情源。
func TestWsMarketEndpointSimulation(t *testing.T) {
	if got := wsMarketEndpoint("SIMULATION"); got != "wss://demo-fstream.binance.com/market/ws" {
		t.Errorf("SIMULATION 应使用 demo 域名，实际 %s", got)
	}
	if got := wsMarketEndpoint("LIVE"); got != "wss://fstream.binance.com/market/ws" {
		t.Errorf("LIVE 应使用主网域名，实际 %s", got)
	}
}

// TestNewWsManagerWithProxy 验证代理配置被 WS 管理器保存。
func TestNewWsManagerWithProxy(t *testing.T) {
	pu, err := url.Parse("socks5://127.0.0.1:10808")
	if err != nil {
		t.Fatal(err)
	}
	ws := NewWsManagerWithProxy("SIMULATION", pu)
	if ws.proxyURL == nil || ws.proxyURL.String() != "socks5://127.0.0.1:10808" {
		t.Errorf("代理未保存: %v", ws.proxyURL)
	}
}
