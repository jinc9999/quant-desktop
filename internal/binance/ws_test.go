// Package binance 币安 WebSocket 行情订阅
package binance

import (
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
