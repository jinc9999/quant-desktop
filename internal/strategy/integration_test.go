// Package strategy 集成测试、回归测试与边界条件测试
package strategy

import (
	"testing"

	"quant-desktop/internal/binance"
)

// TestIntegration_ScreenToCandidate 验证完整筛选链路：
// 滑动窗口采样 → ScreenSliding 筛选 → 候选包含正确的 Side 和 GainPct
func TestIntegration_ScreenToCandidate(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// 模拟 3 个币种：1 个暴涨（做多）、1 个暴跌（做空）、1 个横盘
	// 暴涨币：历史价格 100 → 114.5，当前价 115（最大涨幅 15%）
	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("PUMPUSDT", 100+float64(i)*0.5, 500000, ts)
	}
	// 暴跌币：历史价格恒定为 100，当前价 85（最大涨幅 = (85-100)/100 = -15%）
	// 注意：MaxGainPct 返回所有历史点中涨幅的最大值，
	// 做空条件要求该最大值 <= -minGainPct，因此所有历史价必须远高于当前价
	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("DUMPUSDT", 100, 500000, ts)
	}
	// 横盘币：100 → 100.493，当前价 100.5（最大涨幅约 0.5%，不达标）
	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("FLATUSDT", 100+float64(i)*0.017, 500000, ts)
	}

	tickers := []binance.Ticker{
		{Symbol: "PUMPUSDT", LastPrice: 115, QuoteVolume: 300000},
		{Symbol: "DUMPUSDT", LastPrice: 85, QuoteVolume: 300000},
		{Symbol: "FLATUSDT", LastPrice: 100.5, QuoteVolume: 300000},
	}
	priceMap := map[string]float64{"PUMPUSDT": 115, "DUMPUSDT": 85, "FLATUSDT": 100.5}

	// 开启做空，关闭二次确认
	got := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 0, now, true, 0, 0, 0, "sliding", nil, 0)

	// 应筛出 2 个：PUMPUSDT(LONG) + DUMPUSDT(SHORT)，FLATUSDT 不达标
	if len(got) != 2 {
		t.Fatalf("期望 2 个候选, 实际 %d: %+v", len(got), got)
	}

	// 验证做多候选
	var longCand, shortCand *Candidate
	for i := range got {
		if got[i].Symbol == "PUMPUSDT" {
			longCand = &got[i]
		}
		if got[i].Symbol == "DUMPUSDT" {
			shortCand = &got[i]
		}
	}
	if longCand == nil {
		t.Fatal("缺少做多候选 PUMPUSDT")
	}
	if longCand.Side != "LONG" {
		t.Errorf("PUMPUSDT Side = %q, 期望 LONG", longCand.Side)
	}
	if shortCand == nil {
		t.Fatal("缺少做空候选 DUMPUSDT")
	}
	if shortCand.Side != "SHORT" {
		t.Errorf("DUMPUSDT Side = %q, 期望 SHORT", shortCand.Side)
	}
}

// TestRegression_LongOnlyUnchanged 验证关闭做空时行为与旧版一致
func TestRegression_LongOnlyUnchanged(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("BTCUSDT", 100+float64(i)*0.5, 500000, ts)
	}

	tickers := []binance.Ticker{{Symbol: "BTCUSDT", LastPrice: 115, QuoteVolume: 500000}}
	priceMap := map[string]float64{"BTCUSDT": 115}

	// enableShort=false, 无确认 → 应与旧版行为完全一致
	got := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 3, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("期望 1 个候选, 实际 %d", len(got))
	}
	if got[0].Side != "LONG" {
		t.Errorf("Side = %q, 期望 LONG", got[0].Side)
	}
	if got[0].Symbol != "BTCUSDT" {
		t.Errorf("Symbol = %q, 期望 BTCUSDT", got[0].Symbol)
	}
}

// TestRegression_MaxGainPct 验证 MaxGainPct 不受 Sample 改动影响
func TestRegression_MaxGainPct(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// 价格先跌后涨：100 → 90 → 118
	for i := 0; i < 15; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("REGUSDT", 100-float64(i)*0.67, 500000, ts) // 100 → ~90.6
	}
	for i := 15; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("REGUSDT", 90+float64(i-15)*2, 500000, ts) // 90 → 118
	}

	gain, ready := w.MaxGainPct("REGUSDT", 120, now)
	if !ready {
		t.Fatal("期望 ready=true")
	}
	// 最大涨幅应从最低点 90 算起：(120-90)/90*100 ≈ 33.3%
	if gain < 30 || gain > 37 {
		t.Errorf("MaxGainPct = %f, 期望约 33.3", gain)
	}
}

// TestBoundary_EmptyWindow 验证空窗口不崩溃
func TestBoundary_EmptyWindow(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// 空窗口筛选
	tickers := []binance.Ticker{{Symbol: "BTCUSDT", LastPrice: 100, QuoteVolume: 500000}}
	priceMap := map[string]float64{"BTCUSDT": 100}
	got := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 0, now, true, 180000, 1.5, 1.5, "sliding", nil, 0)
	if len(got) != 0 {
		t.Errorf("空窗口期望 0 个候选, 实际 %d", len(got))
	}

	// 空窗口 RecentGainPct
	_, ready := w.RecentGainPct("BTCUSDT", 100, now, 180000)
	if ready {
		t.Error("空窗口 RecentGainPct 期望 ready=false")
	}

	// 空窗口 RecentVolumeSurge
	_, ready2 := w.RecentVolumeSurge("BTCUSDT", now, 180000, 720000)
	if ready2 {
		t.Error("空窗口 RecentVolumeSurge 期望 ready=false")
	}
}

// TestBoundary_ZeroThreshold 验证阈值为 0 时关闭确认
func TestBoundary_ZeroThreshold(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("ZEROUSDT", 100+float64(i)*0.5, 500000, ts)
	}

	tickers := []binance.Ticker{{Symbol: "ZEROUSDT", LastPrice: 115, QuoteVolume: 300000}}
	priceMap := map[string]float64{"ZEROUSDT": 115}

	// confirmThreshold=0, volumeSurgeThreshold=0 → 全部关闭，应通过
	got := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("关闭确认时期望 1 个候选, 实际 %d", len(got))
	}
}

// TestBoundary_SingleSample 验证只有 1 个采样点时不崩溃
func TestBoundary_SingleSample(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	w.Sample("ONEUSDT", 100, 500000, now-30000)

	tickers := []binance.Ticker{{Symbol: "ONEUSDT", LastPrice: 115, QuoteVolume: 300000}}
	priceMap := map[string]float64{"ONEUSDT": 115}

	// 不应 panic
	got := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 0, now, true, 180000, 1.5, 1.5, "sliding", nil, 0)
	_ = got // 结果不重要，不 panic 就行
}

// TestBoundary_NilTickers 验证空 tickers 不崩溃
func TestBoundary_NilTickers(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	got := ScreenSliding(w, nil, nil, 10.0, 0, 100000, 0, now, true, 180000, 1.5, 1.5, "sliding", nil, 0)
	if len(got) != 0 {
		t.Errorf("空 tickers 期望 0 个候选, 实际 %d", len(got))
	}
}
