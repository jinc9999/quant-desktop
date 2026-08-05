// Package strategy 筛选排名单元测试
package strategy

import (
	"testing"

	"quant-desktop/internal/binance"
)

// feedBaseline 向窗口喂入一个"恰好 5 分钟前"的基准采样点
// w: 目标滑动窗口
// symbol: 交易对
// now: 当前时间（Unix 毫秒）
// price: 基准价格
func feedBaseline(w *SlidingWindow, symbol string, now int64, price float64) {
	w.Add(symbol, now-300000, price)
}

// TestScreenSliding_FilterByGain 验证滑动涨幅低于阈值的候选被过滤
// LOWUSDT 涨幅 3%（<5）被过滤，HIGHUSDT 涨幅 6%（>=5）保留
func TestScreenSliding_FilterByGain(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "LOWUSDT", now, 100)  // 基准 100，现价 103 -> 3%
	feedBaseline(w, "HIGHUSDT", now, 100) // 基准 100，现价 106 -> 6%

	tickers := []binance.Ticker{
		{Symbol: "LOWUSDT", LastPrice: 103, QuoteVolume: 200000},
		{Symbol: "HIGHUSDT", LastPrice: 106, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"LOWUSDT": 103, "HIGHUSDT": 106}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "HIGHUSDT" {
		t.Errorf("Symbol = %q, 期望 HIGHUSDT", got[0].Symbol)
	}
}

// TestScreenSliding_FilterByVolume 验证成交额低于阈值的候选被过滤
// LOWVOLUSDT 成交额 50000（<100000）被过滤，HIGHVOLUSDT 保留
func TestScreenSliding_FilterByVolume(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "LOWVOLUSDT", now, 100)
	feedBaseline(w, "HIGHVOLUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "LOWVOLUSDT", LastPrice: 106, QuoteVolume: 50000},
		{Symbol: "HIGHVOLUSDT", LastPrice: 106, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"LOWVOLUSDT": 106, "HIGHVOLUSDT": 106}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "HIGHVOLUSDT" {
		t.Errorf("Symbol = %q, 期望 HIGHVOLUSDT", got[0].Symbol)
	}
}

// TestScreenSliding_SortByVolume 验证结果按成交额降序排列
// 三个候选成交额不同，期望输出按成交额从大到小
func TestScreenSliding_SortByVolume(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "AUSDT", now, 100)
	feedBaseline(w, "BUSDT", now, 100)
	feedBaseline(w, "CUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "AUSDT", LastPrice: 106, QuoteVolume: 200000},   // 6%, vol=200k
		{Symbol: "BUSDT", LastPrice: 109, QuoteVolume: 500000},   // 9%, vol=500k
		{Symbol: "CUSDT", LastPrice: 107.5, QuoteVolume: 300000}, // 7.5%, vol=300k
	}
	priceMap := map[string]float64{"AUSDT": 106, "BUSDT": 109, "CUSDT": 107.5}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 3 {
		t.Fatalf("len = %d, 期望 3", len(got))
	}

	// 按成交额降序: BUSDT(500k) > CUSDT(300k) > AUSDT(200k)
	wantOrder := []string{"BUSDT", "CUSDT", "AUSDT"}
	for i, want := range wantOrder {
		if got[i].Symbol != want {
			t.Errorf("第 %d 位 Symbol = %q, 期望 %q", i, got[i].Symbol, want)
		}
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].QuoteVolume < got[i].QuoteVolume {
			t.Errorf("排序错误: 位置 %d 成交额 %v < 位置 %d 成交额 %v", i-1, got[i-1].QuoteVolume, i, got[i].QuoteVolume)
		}
	}
}

// TestScreenSliding_TopN 验证 TopN 截取功能
// 5 个候选达标，topN=3 时只返回成交额最大的 3 个
func TestScreenSliding_TopN(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	symbols := []string{"AUSDT", "BUSDT", "CUSDT", "DUSDT", "EUSDT"}
	volumes := []float64{100000, 500000, 300000, 200000, 400000}
	for i, sym := range symbols {
		feedBaseline(w, sym, now, 100)
		_ = volumes[i]
	}

	tickers := make([]binance.Ticker, len(symbols))
	priceMap := make(map[string]float64, len(symbols))
	for i, sym := range symbols {
		tickers[i] = binance.Ticker{Symbol: sym, LastPrice: 110, QuoteVolume: volumes[i]}
		priceMap[sym] = 110
	}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 3, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 3 {
		t.Fatalf("topN=3 时 len = %d, 期望 3", len(got))
	}
	// 成交额最大的 3 个: BUSDT(500k) > EUSDT(400k) > CUSDT(300k)
	wantOrder := []string{"BUSDT", "EUSDT", "CUSDT"}
	for i, want := range wantOrder {
		if got[i].Symbol != want {
			t.Errorf("第 %d 位 Symbol = %q, 期望 %q", i, got[i].Symbol, want)
		}
	}
}

// TestScreenSliding_WarmupIncluded 验证预热期（窗口未满）的币种也能参与筛选
// 仅有 1 分钟前的基准点，现价大涨 50%，预热期以最早点为基准，涨幅达标应入选
func TestScreenSliding_WarmupIncluded(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	// 仅喂入 1 分钟前的点（预热期，窗口未满），现价大涨 50%
	w.Add("NEWUSDT", now-60000, 100)

	tickers := []binance.Ticker{
		{Symbol: "NEWUSDT", LastPrice: 150, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"NEWUSDT": 150}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("预热期币种涨幅达标应入选，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "NEWUSDT" {
		t.Errorf("Symbol = %q, 期望 NEWUSDT", got[0].Symbol)
	}
	// 基准为最早点 100，涨幅 = (150-100)/100*100 = 50%
	if !almostEqual(got[0].GainPct, 50.0) {
		t.Errorf("GainPct = %v, 期望 50.0（预热期用最早点作基准）", got[0].GainPct)
	}
}

// TestScreenSliding_PriceMapPriority 验证现价优先取 priceMap（WS 实时价）
// ticker.LastPrice 与 priceMap 不同时，应以 priceMap 计算涨幅
func TestScreenSliding_PriceMapPriority(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "XUSDT", now, 100)

	tickers := []binance.Ticker{
		// LastPrice 仅涨 1%（不达标），但 priceMap 实时价涨 8%（达标）
		{Symbol: "XUSDT", LastPrice: 101, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"XUSDT": 108}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("应使用 priceMap 实时价计算，len = %d, 期望 1", len(got))
	}
	if !almostEqual(got[0].GainPct, 8.0) {
		t.Errorf("GainPct = %v, 期望 8.0（基于 priceMap）", got[0].GainPct)
	}
}

// TestScreenSliding_EmptyInput 验证空输入返回长度为 0 的切片
// 覆盖: 空切片与 nil 两种情况
func TestScreenSliding_EmptyInput(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)

	if got := ScreenSliding(w, []binance.Ticker{}, nil, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0); len(got) != 0 {
		t.Errorf("空切片输入 len = %d, 期望 0", len(got))
	}
	if got := ScreenSliding(w, nil, nil, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0); len(got) != 0 {
		t.Errorf("nil 输入 len = %d, 期望 0", len(got))
	}
}

// ==================== 边界条件测试 ====================

// TestScreenSliding_Boundary_MinGainExact 验证涨幅恰好等于阈值时应被纳入候选
// 基准价 100，现价 105，涨幅 = (105-100)/100*100 = 5.0%，阈值 5.0%
// 筛选条件为 gain < minGainPct 时跳过，因此 gain == minGainPct 应通过（>= 语义）
func TestScreenSliding_Boundary_MinGainExact(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "EXACTUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "EXACTUSDT", LastPrice: 105, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"EXACTUSDT": 105}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("涨幅恰好等于阈值应被纳入，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "EXACTUSDT" {
		t.Errorf("Symbol = %q, 期望 EXACTUSDT", got[0].Symbol)
	}
	if !almostEqual(got[0].GainPct, 5.0) {
		t.Errorf("GainPct = %v, 期望 5.0", got[0].GainPct)
	}
}

// TestScreenSliding_Boundary_MinVolumeExact 验证成交额恰好等于阈值时应被纳入候选
// 筛选条件为 QuoteVolume < minQuoteVolume 时跳过，因此等于阈值应通过（>= 语义）
func TestScreenSliding_Boundary_MinVolumeExact(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "VOLEXACTUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "VOLEXACTUSDT", LastPrice: 106, QuoteVolume: 100000},
	}
	priceMap := map[string]float64{"VOLEXACTUSDT": 106}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("成交额恰好等于阈值应被纳入，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "VOLEXACTUSDT" {
		t.Errorf("Symbol = %q, 期望 VOLEXACTUSDT", got[0].Symbol)
	}
}

// TestScreenSliding_Boundary_TopNExact 验证 topN 恰好等于候选数量时全部返回
// 3 个达标候选，topN=3，应返回全部 3 个（截取条件为 len > topN 才截）
func TestScreenSliding_Boundary_TopNExact(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "AUSDT", now, 100)
	feedBaseline(w, "BUSDT", now, 100)
	feedBaseline(w, "CUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "AUSDT", LastPrice: 106, QuoteVolume: 200000},
		{Symbol: "BUSDT", LastPrice: 107, QuoteVolume: 300000},
		{Symbol: "CUSDT", LastPrice: 108, QuoteVolume: 400000},
	}
	priceMap := map[string]float64{"AUSDT": 106, "BUSDT": 107, "CUSDT": 108}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 3, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 3 {
		t.Fatalf("topN 等于候选数时应全部返回，len = %d, 期望 3", len(got))
	}
}

// TestScreenSliding_Boundary_SingleCandidate 验证仅 1 个候选且 topN=1 时正常返回
// 边界场景：候选数与 topN 均为最小有效值 1
func TestScreenSliding_Boundary_SingleCandidate(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "SOLOUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "SOLOUSDT", LastPrice: 110, QuoteVolume: 500000},
	}
	priceMap := map[string]float64{"SOLOUSDT": 110}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 1, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("单候选 topN=1 应返回 1 个，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "SOLOUSDT" {
		t.Errorf("Symbol = %q, 期望 SOLOUSDT", got[0].Symbol)
	}
}

// TestScreenSliding_Boundary_ZeroPrice 验证当前价为 0 时该币种被跳过
// priceMap 中价格为 0 且 ticker.LastPrice 也为 0，current <= 0 应被过滤
func TestScreenSliding_Boundary_ZeroPrice(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "ZEROUSDT", now, 100)
	feedBaseline(w, "NORMALUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "ZEROUSDT", LastPrice: 0, QuoteVolume: 200000},
		{Symbol: "NORMALUSDT", LastPrice: 106, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"ZEROUSDT": 0, "NORMALUSDT": 106}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("零价格应被跳过，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "NORMALUSDT" {
		t.Errorf("Symbol = %q, 期望 NORMALUSDT（零价格币种不应出现）", got[0].Symbol)
	}
}

// TestScreenSliding_Boundary_NegativeGain 验证价格下跌（负涨幅）时被过滤
// 基准价 100，现价 95，涨幅 = (95-100)/100*100 = -5%，低于阈值 5% 应被过滤
func TestScreenSliding_Boundary_NegativeGain(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "DROPUSDT", now, 100)
	feedBaseline(w, "RISEUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "DROPUSDT", LastPrice: 95, QuoteVolume: 200000},
		{Symbol: "RISEUSDT", LastPrice: 106, QuoteVolume: 200000},
	}
	priceMap := map[string]float64{"DROPUSDT": 95, "RISEUSDT": 106}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("负涨幅应被过滤，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "RISEUSDT" {
		t.Errorf("Symbol = %q, 期望 RISEUSDT（负涨幅币种不应出现）", got[0].Symbol)
	}
}

// ==================== 做空筛选 & 二次确认测试 ====================

// TestScreenSliding_Short 验证做空筛选：跌幅达标时返回 Side="SHORT" 的候选
// 窗口内价格稳定在 100，当前价跌至 88（跌 12%），MaxGainPct 返回 -12% <= -10% 触发做空
func TestScreenSliding_Short(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)
	// 喂入价格：窗口内价格稳定在 100（每 30 秒一个采样点）
	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		w.Sample("DROPUSDT", 100.0, 500000, ts)
	}
	tickers := []binance.Ticker{{Symbol: "DROPUSDT", LastPrice: 88, QuoteVolume: 200000}}
	priceMap := map[string]float64{"DROPUSDT": 88}

	// enableShort=true 时应筛出做空候选
	got := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 0, now, true, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("期望 1 个候选, 实际 %d", len(got))
	}
	if got[0].Side != "SHORT" {
		t.Errorf("Side = %q, 期望 SHORT", got[0].Side)
	}
	if got[0].GainPct < 10 {
		t.Errorf("GainPct = %f, 期望 >= 10", got[0].GainPct)
	}

	// enableShort=false 时不应筛出
	got2 := ScreenSliding(w, tickers, priceMap, 10.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got2) != 0 {
		t.Errorf("enableShort=false 时期望 0 个候选, 实际 %d", len(got2))
	}
}

// TestScreenSliding_ConfirmThreshold 验证短窗口二次确认过滤
// 长窗口涨幅达标（10%），但短窗口（3 分钟）涨幅不足 3%，应被过滤
func TestScreenSliding_ConfirmThreshold(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)
	// 喂入价格：前 12 分钟从 100 稳步上涨到 111.5
	for i := 0; i < 24; i++ {
		ts := now - int64(30-i)*30000
		price := 100 + float64(i)*0.5 // 100 → 111.5
		w.Sample("CONFUSDT", price, 500000, ts)
	}
	// 最后 3 分钟价格回落到 110（短窗口涨幅不足）
	w.Sample("CONFUSDT", 110, 500000, now-90000)
	w.Sample("CONFUSDT", 109.5, 500000, now-60000)
	w.Sample("CONFUSDT", 110, 500000, now-30000)

	tickers := []binance.Ticker{{Symbol: "CONFUSDT", LastPrice: 110, QuoteVolume: 200000}}
	priceMap := map[string]float64{"CONFUSDT": 110}

	// 关闭确认（threshold=0）：应通过（长窗口 MaxGainPct=10% >= 5%）
	got := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) == 0 {
		t.Fatal("关闭确认时期望有候选")
	}

	// 开启确认（threshold=3%，窗口=3分钟）：短窗口涨幅不足应被过滤
	// RecentGainPct 基准为 ts=790000 处的 111.5，gain=(110-111.5)/111.5*100≈-1.35% < 3%
	got2 := ScreenSliding(w, tickers, priceMap, 5.0, 0, 100000, 0, now, false, 180000, 3.0, 0, "sliding", nil, 0)
	if len(got2) != 0 {
		t.Errorf("短窗口涨幅不足时期望 0 个候选, 实际 %d", len(got2))
	}
}

// ==================== 双条件筛选（15m + 24h 涨幅）测试 ====================

// TestScreenSliding_24hGainFilter 验证 24h 涨幅过滤（双条件筛选）
// 场景：15m 涨幅均达标（6%），但 24h 涨幅不同：
//   - GOOD24USDT: 24h 涨幅 8%（>=5%）→ 保留
//   - BAD24USDT:  24h 涨幅 2%（<5%）→ 过滤
func TestScreenSliding_24hGainFilter(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "GOOD24USDT", now, 100) // 基准 100，现价 106 -> 15m 涨幅 6%
	feedBaseline(w, "BAD24USDT", now, 100)  // 基准 100，现价 106 -> 15m 涨幅 6%

	tickers := []binance.Ticker{
		{Symbol: "GOOD24USDT", LastPrice: 106, QuoteVolume: 200000, PriceChange: 8.0}, // 24h +8%
		{Symbol: "BAD24USDT", LastPrice: 106, QuoteVolume: 200000, PriceChange: 2.0},  // 24h +2%
	}
	priceMap := map[string]float64{"GOOD24USDT": 106, "BAD24USDT": 106}

	// 启用 24h 过滤（min24hGainPct=5.0）：仅 24h 涨幅达标的保留
	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("len = %d, 期望 1（仅 24h 涨幅达标的候选保留）", len(got))
	}
	if got[0].Symbol != "GOOD24USDT" {
		t.Errorf("Symbol = %q, 期望 GOOD24USDT（24h 涨幅 8%% 应保留，BAD24USDT 2%% 应被过滤）", got[0].Symbol)
	}
}

// TestScreenSliding_DualCriteria 验证双条件同时满足才入选（端到端语义）
// 场景：
//   - DUALUSDT:  15m 涨幅 6%（>=5%）且 24h 涨幅 6%（>=5%）→ 保留
//   - NO15MUSDT: 15m 涨幅 3%（<5%）且 24h 涨幅 9%（>=5%）→ 过滤（15m 不达标）
//   - NO24HUSDT: 15m 涨幅 7%（>=5%）且 24h 涨幅 1%（<5%）→ 过滤（24h 不达标）
func TestScreenSliding_DualCriteria(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "DUALUSDT", now, 100)
	feedBaseline(w, "NO15MUSDT", now, 100)
	feedBaseline(w, "NO24HUSDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "DUALUSDT", LastPrice: 106, QuoteVolume: 200000, PriceChange: 6.0},  // 15m +6%, 24h +6%
		{Symbol: "NO15MUSDT", LastPrice: 103, QuoteVolume: 200000, PriceChange: 9.0}, // 15m +3%, 24h +9%
		{Symbol: "NO24HUSDT", LastPrice: 107, QuoteVolume: 200000, PriceChange: 1.0}, // 15m +7%, 24h +1%
	}
	priceMap := map[string]float64{"DUALUSDT": 106, "NO15MUSDT": 103, "NO24HUSDT": 107}

	// 双条件：15m 涨幅 >= 5% 且 24h 涨幅 >= 5%
	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, false, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("len = %d, 期望 1（仅 DUALUSDT 同时满足双条件）", len(got))
	}
	if got[0].Symbol != "DUALUSDT" {
		t.Errorf("Symbol = %q, 期望 DUALUSDT", got[0].Symbol)
	}
}

// TestScreenSliding_24hGainFilterShort 验证做空方向 24h 跌幅过滤
// 场景：15m 跌幅均达标（-12%），但 24h 涨跌幅不同：
//   - SHORT24USDT: 24h 跌幅 -8%（<= -5%）→ 保留
//   - NO24HUSDT:   24h 跌幅 -2%（> -5%）→ 过滤（24h 跌幅不足）
func TestScreenSliding_24hGainFilterShort(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(300000, 10000)
	feedBaseline(w, "SHORT24USDT", now, 100)
	feedBaseline(w, "NOSHORT24USDT", now, 100)

	tickers := []binance.Ticker{
		{Symbol: "SHORT24USDT", LastPrice: 88, QuoteVolume: 200000, PriceChange: -8.0}, // 24h -8%
		{Symbol: "NOSHORT24USDT", LastPrice: 88, QuoteVolume: 200000, PriceChange: -2.0}, // 24h -2%
	}
	priceMap := map[string]float64{"SHORT24USDT": 88, "NOSHORT24USDT": 88}

	got := ScreenSliding(w, tickers, priceMap, 10.0, 5.0, 100000, 0, now, true, 0, 0, 0, "sliding", nil, 0)
	if len(got) != 1 {
		t.Fatalf("len = %d, 期望 1（仅 24h 跌幅达标的做空候选保留）", len(got))
	}
	if got[0].Symbol != "SHORT24USDT" {
		t.Errorf("Symbol = %q, 期望 SHORT24USDT（24h 跌幅 -8%% 应保留）", got[0].Symbol)
	}
}

// ========== K 线信号模式（kline）测试 ==========

// TestScreenKlineMode_Basic 验证 kline 模式：以当前价 vs K 线开盘价计算实体涨幅，
// 做多（涨幅 >= 5%）与做空（跌幅 >= 5%）均能触发，且 24h 双条件仍生效。
// 注意：kline 模式不依赖滑动窗口（window 可为空），无需预热。
func TestScreenKlineMode_Basic(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(900000, 10000) // 15m 窗口（kline 模式下不参与信号计算）

	tickers := []binance.Ticker{
		// K线实体 +6%（105/100-1），24h +8% → 做多入选
		{Symbol: "LONGKUSDT", LastPrice: 105, QuoteVolume: 200000, PriceChange: 8.0},
		// K线实体 +6%，但 24h 仅 +2% < 5% → 24h 双条件排除
		{Symbol: "NO24KUSDT", LastPrice: 105, QuoteVolume: 200000, PriceChange: 2.0},
		// K线实体 -6%（94/100-1），24h -8% → 做空入选
		{Symbol: "SHORTKUSDT", LastPrice: 94, QuoteVolume: 200000, PriceChange: -8.0},
		// K线实体 +3% < 5% → 涨幅不足排除
		{Symbol: "WEAKKUSDT", LastPrice: 103, QuoteVolume: 200000, PriceChange: 8.0},
	}
	priceMap := map[string]float64{
		"LONGKUSDT": 105, "NO24KUSDT": 105, "SHORTKUSDT": 94, "WEAKKUSDT": 103,
	}
	klineOpen := map[string]float64{
		"LONGKUSDT": 100, "NO24KUSDT": 100, "SHORTKUSDT": 100, "WEAKKUSDT": 100,
	}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, true, 0, 0, 0, "kline", klineOpen, 0)
	if len(got) != 2 {
		t.Fatalf("len = %d, 期望 2（LONGKUSDT + SHORTKUSDT）", len(got))
	}
	sides := map[string]string{}
	for _, c := range got {
		sides[c.Symbol] = c.Side
	}
	if sides["LONGKUSDT"] != "LONG" {
		t.Errorf("LONGKUSDT Side = %q, 期望 LONG（K线实体 +6%% 且 24h +8%%）", sides["LONGKUSDT"])
	}
	if sides["SHORTKUSDT"] != "SHORT" {
		t.Errorf("SHORTKUSDT Side = %q, 期望 SHORT（K线实体 -6%% 且 24h -8%%）", sides["SHORTKUSDT"])
	}
}

// TestScreenKlineMode_MissingOpen 验证 kline 模式下 K 线开盘价缺失的币保守跳过
//（拉取失败/未拉取时不产生假信号）
func TestScreenKlineMode_MissingOpen(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(900000, 10000)

	tickers := []binance.Ticker{
		{Symbol: "HASOPENUSDT", LastPrice: 105, QuoteVolume: 200000, PriceChange: 8.0},
		{Symbol: "NOOPENUSDT", LastPrice: 105, QuoteVolume: 200000, PriceChange: 8.0}, // klineOpen 缺失
	}
	priceMap := map[string]float64{"HASOPENUSDT": 105, "NOOPENUSDT": 105}
	klineOpen := map[string]float64{"HASOPENUSDT": 100}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, true, 0, 0, 0, "kline", klineOpen, 0)
	if len(got) != 1 {
		t.Fatalf("len = %d, 期望 1（NOOPENUSDT 因 K 线开盘价缺失应被跳过）", len(got))
	}
	if got[0].Symbol != "HASOPENUSDT" {
		t.Errorf("Symbol = %q, 期望 HASOPENUSDT", got[0].Symbol)
	}
}

// TestScreenKlineMode_MaxPullback 验证山顶过滤器：
// 做多时距 24h 最高价回撤超过阈值不追（可能接飞刀）；
// 做空时距 24h 最低价反弹超过阈值不追；未超阈值的正常入选。
func TestScreenKlineMode_MaxPullback(t *testing.T) {
	now := int64(1000000)
	w := NewSlidingWindow(900000, 10000)

	tickers := []binance.Ticker{
		// 当前 105，24h 最高 120（回撤 12.5% > 9%）→ 做多被山顶过滤器排除
		{Symbol: "TOPHIGHUSDT", LastPrice: 105, QuoteVolume: 200000, PriceChange: 8.0, HighPrice: 120, LowPrice: 95},
		// 当前 105，24h 最高 110（回撤 4.5% <= 9%）→ 做多正常入选
		{Symbol: "OKHIGHUSDT", LastPrice: 105, QuoteVolume: 200000, PriceChange: 8.0, HighPrice: 110, LowPrice: 95},
		// 当前 94，24h 最低 85（反弹 10.6% > 9%）→ 做空被山顶过滤器排除
		{Symbol: "TOPLOWUSDT", LastPrice: 94, QuoteVolume: 200000, PriceChange: -8.0, HighPrice: 105, LowPrice: 85},
		// 当前 94，24h 最低 90（反弹 4.4% <= 9%）→ 做空正常入选
		{Symbol: "OKLOWUSDT", LastPrice: 94, QuoteVolume: 200000, PriceChange: -8.0, HighPrice: 105, LowPrice: 90},
	}
	priceMap := map[string]float64{
		"TOPHIGHUSDT": 105, "OKHIGHUSDT": 105, "TOPLOWUSDT": 94, "OKLOWUSDT": 94,
	}
	klineOpen := map[string]float64{
		"TOPHIGHUSDT": 100, "OKHIGHUSDT": 100, "TOPLOWUSDT": 100, "OKLOWUSDT": 100,
	}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, true, 0, 0, 0, "kline", klineOpen, 9.0)
	if len(got) != 2 {
		t.Fatalf("len = %d, 期望 2（OKHIGHUSDT + OKLOWUSDT，超回撤的被过滤）", len(got))
	}
	symbols := map[string]bool{}
	for _, c := range got {
		symbols[c.Symbol] = true
	}
	if symbols["TOPHIGHUSDT"] {
		t.Error("TOPHIGHUSDT 不应入选：距 24h 最高回撤 12.5% > 9%（山顶过滤器）")
	}
	if symbols["TOPLOWUSDT"] {
		t.Error("TOPLOWUSDT 不应入选：距 24h 最低反弹 10.6% > 9%（山顶过滤器）")
	}
	if !symbols["OKHIGHUSDT"] {
		t.Error("OKHIGHUSDT 应入选：距 24h 最高回撤 4.5% <= 9%")
	}
	if !symbols["OKLOWUSDT"] {
		t.Error("OKLOWUSDT 应入选：距 24h 最低反弹 4.4% <= 9%")
	}
}

// TestScreenKlineMode_VolumeSurge 验证 kline 模式放量确认生效（放量达标 → 入选）
// 最近 2 分钟每 30 秒累计成交额增加 3000，之前 13 分钟每 30 秒增加 1000：
//   recentVol = 38000-29000 = 9000（2 分钟）
//   priorVol  = 29000-5000  = 24000（13 分钟）
//   surge = (9000/120000) / (24000/780000) ≈ 2.44 >= 1.8 → 放量达标
// K 线实体涨幅 (107-100)/100 = 7% >= 5%，应入选
func TestScreenKlineMode_VolumeSurge(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	// 之前 13 分钟：每 30 秒成交量 1000
	cumVol := 0.0
	for i := 0; i < 26; i++ {
		ts := now - int64(30-i)*30000
		cumVol += 1000
		w.Sample("SURGEUSDT", 100.0, cumVol, ts)
	}
	// 最近 2 分钟：每 30 秒成交量 3000（放量 3 倍）
	for i := 0; i < 4; i++ {
		ts := now - int64(4-i)*30000
		cumVol += 3000
		w.Sample("SURGEUSDT", 100.0, cumVol, ts)
	}

	tickers := []binance.Ticker{
		{Symbol: "SURGEUSDT", LastPrice: 107, QuoteVolume: 300000, PriceChange: 6.0},
	}
	priceMap := map[string]float64{"SURGEUSDT": 107}
	klineOpen := map[string]float64{"SURGEUSDT": 100}

	// kline 模式 + 放量确认（2 分钟窗口，1.8 倍阈值）
	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, true, 120000, 0, 1.8, "kline", klineOpen, 0)
	if len(got) != 1 {
		t.Fatalf("放量达标应入选，len = %d, 期望 1", len(got))
	}
	if got[0].Symbol != "SURGEUSDT" {
		t.Errorf("Symbol = %q, 期望 SURGEUSDT", got[0].Symbol)
	}
}

// TestScreenKlineMode_VolumeSurge_Filter 验证 kline 模式放量不足被过滤
// 全程均匀放量（每 30 秒 1000）：surge ≈ 0.89 < 1.8，K 线涨幅达标也应被过滤
// （防无量假突破：价格在涨但没有资金流入，不值得追）
func TestScreenKlineMode_VolumeSurge_Filter(t *testing.T) {
	w := NewSlidingWindow(15*60*1000, 30000)
	now := int64(1000000)

	cumVol := 0.0
	for i := 0; i < 30; i++ {
		ts := now - int64(30-i)*30000
		cumVol += 1000
		w.Sample("FLATUSDT", 100.0, cumVol, ts)
	}

	tickers := []binance.Ticker{
		{Symbol: "FLATUSDT", LastPrice: 107, QuoteVolume: 300000, PriceChange: 6.0},
	}
	priceMap := map[string]float64{"FLATUSDT": 107}
	klineOpen := map[string]float64{"FLATUSDT": 100}

	got := ScreenSliding(w, tickers, priceMap, 5.0, 5.0, 100000, 0, now, true, 120000, 0, 1.8, "kline", klineOpen, 0)
	if len(got) != 0 {
		t.Errorf("放量不足应被过滤，len = %d, 期望 0", len(got))
	}
}
