// Package strategy 性能基准测试
// 用于量化验证性能优化效果，运行方式：
//
//	go test -bench=. -benchmem ./internal/strategy/
package strategy

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/order"
	"quant-desktop/internal/storage"
)

// benchSymbolCount 基准测试模拟的交易对数量（贴近币安 USDT 合约真实规模）
const benchSymbolCount = 400

// newBenchEngine 创建基准测试用的 Engine（DRY_RUN 模式 + 临时数据库）
// 参数:
//   - b: 基准测试实例
//
// 返回:
//   - *Engine: 策略引擎实例
//   - *storage.DB: 数据库实例（测试结束自动关闭）
func newBenchEngine(b *testing.B) (*Engine, *storage.DB) {
	b.Helper()

	tmpDir := b.TempDir()
	dbPath := filepath.Join(tmpDir, "bench.db")
	db, err := storage.NewDB(dbPath)
	if err != nil {
		b.Fatalf("创建数据库失败: %v", err)
	}

	client := binance.NewClient("test-key", "test-secret", "DRY_RUN", "", 0)
	ws := binance.NewWsManager("DRY_RUN")
	orderMgr := order.NewManager(client, db)

	cfg := binance.StrategyConfig{
		ScanIntervalSec:    10,
		Timeframe:          "5m",
		MinGainPct:         5.0,
		MinQuoteVolume:     100000,
		TopN:               0,       // 不限制，基准测试需要全量候选
		MaxOpenPositions:   1000000, // 放大上限，避免基准测试中槽位耗尽
		Leverage:           10,
		PositionMarginUSDT: 5,
		CooldownMin:        5,
		MarginMode:         binance.MarginModeIsolated,
		StopLossPct:        0.10,
		TrailingActivation: 0.05,
		TrailingCallback:   0.03,
	}

	e := NewEngine(cfg, client, ws, db, orderMgr)
	b.Cleanup(func() { db.Close() })
	return e, db
}

// benchSymbols 生成 n 个模拟交易对名称
// 参数:
//   - n: 交易对数量
//
// 返回:
//   - []string: 交易对名称列表（如 BENCH000USDT）
func benchSymbols(n int) []string {
	symbols := make([]string, n)
	for i := range symbols {
		symbols[i] = fmt.Sprintf("BENCH%03dUSDT", i)
	}
	return symbols
}

// BenchmarkSlidingWindow_Add 基准测试滑动窗口写入（400 币种轮转采样 + 裁剪）
// 对应性能分析第一节：评估滑动窗口写入路径的 CPU 开销
func BenchmarkSlidingWindow_Add(b *testing.B) {
	w := NewSlidingWindow(300000, 10000)
	symbols := benchSymbols(benchSymbolCount)
	baseTs := time.Now().UnixMilli()

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sym := symbols[i%benchSymbolCount]
		// 时间递增以触发裁剪逻辑
		w.Add(sym, baseTs+int64(i)*1000, 50000+float64(i%100))
	}
}

// BenchmarkSlidingWindow_GainPct 基准测试滑动窗口涨幅计算（含基准点二分查找）
// 先填满 400 币种的窗口，再反复计算涨幅
func BenchmarkSlidingWindow_GainPct(b *testing.B) {
	w := NewSlidingWindow(300000, 10000)
	symbols := benchSymbols(benchSymbolCount)
	now := time.Now().UnixMilli()

	// 预热：为每个币种填入覆盖完整窗口的采样点
	for _, sym := range symbols {
		for ts := now - 300000; ts <= now; ts += 10000 {
			w.Add(sym, ts, 50000)
		}
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		sym := symbols[i%benchSymbolCount]
		w.GainPct(sym, 53000, now)
	}
}

// BenchmarkScreenSliding 基准测试全市场滑动筛选（400 币种）
// 对应性能分析第一节：评估每 Tick 筛选阶段的开销
func BenchmarkScreenSliding(b *testing.B) {
	w := NewSlidingWindow(300000, 10000)
	symbols := benchSymbols(benchSymbolCount)
	now := time.Now().UnixMilli()

	tickers := make([]binance.Ticker, benchSymbolCount)
	priceMap := make(map[string]float64, benchSymbolCount)
	for i, sym := range symbols {
		// 填入基准点，使部分币种涨幅达标
		base := 50000.0
		w.Add(sym, now-300000, base)
		current := base * (1 + float64(i%10)/100.0) // 0%~9% 涨幅分布
		w.Add(sym, now, current)
		tickers[i] = binance.Ticker{Symbol: sym, LastPrice: current, QuoteVolume: 500000}
		priceMap[sym] = current
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0, nil)
	}
}

// BenchmarkOpenPositions_Parallel 基准测试并发开仓（P0）
// DRY_RUN 模式下 OpenLong 无真实网络往返，本基准量化「并发调度 + DB 写入 + 挂止损委托」的开销，
// 用于验证 P0（有界并发）与 P1（synchronous=NORMAL）对入仓流程的优化效果。
// 每次迭代开 30 个仓位（受 openConcurrency=6 限流）。
func BenchmarkOpenPositions_Parallel(b *testing.B) {
	e, _ := newBenchEngine(b)
	ctx := context.Background()

	symbols := benchSymbols(30)
	candidates := make([]Candidate, 30)
	priceMap := make(map[string]float64, 30)
	for i, sym := range symbols {
		candidates[i] = Candidate{Symbol: sym, GainPct: 6.0, QuoteVolume: 500000}
		priceMap[sym] = 50000.0
	}

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		e.openPositions(ctx, candidates, priceMap, nil)
	}
}
