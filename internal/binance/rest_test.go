// Package binance 币安 REST API DRY_RUN 模式单元测试
package binance

import (
	"context"
	"errors"
	"math"
	"testing"

	"github.com/adshao/go-binance/v2/futures"
)

// newDryRunClient 创建 DRY_RUN 模式的测试客户端
// 返回 *Client 用于测试的模拟客户端实例
func newDryRunClient() *Client {
	return NewClient("", "", "DRY_RUN", "", 0)
}

// ==================== 一、Client 创建测试 ====================

// TestNewClient_DryRun 验证 DRY_RUN 模式下客户端正确创建
// 检查 mode 字段为 "DRY_RUN"，futuresClient 非 nil
func TestNewClient_DryRun(t *testing.T) {
	c := NewClient("", "", "DRY_RUN", "", 0)
	if c.mode != "DRY_RUN" {
		t.Errorf("期望 mode=DRY_RUN, 实际=%s", c.mode)
	}
	if c.futuresClient == nil {
		t.Error("futuresClient 不应为 nil")
	}
}

// TestNewClient_Testnet 验证 TESTNET 模式下客户端正确创建
// 检查 mode 字段为 "TESTNET"，futuresClient 非 nil
func TestNewClient_Testnet(t *testing.T) {
	c := NewClient("", "", "TESTNET", "", 0)
	if c.mode != "TESTNET" {
		t.Errorf("期望 mode=TESTNET, 实际=%s", c.mode)
	}
	if c.futuresClient == nil {
		t.Error("futuresClient 不应为 nil")
	}
}

// TestDefaultStrategyConfig 验证默认策略配置各字段值正确
// S01 纯追涨（无门控）锁定参数：15m K 线实体实时确认、双 4% 门槛（K 线 + 24h）、10x 杠杆、
// 10U 保证金、纯多（不做空）、6% 止损、10% 固定止盈、3% 跟踪激活、2% 跟踪回撤、
// 最长持仓 120 分钟、日亏 5% 熔断、回撤 15% 熔断、山顶过滤器 9%
func TestDefaultStrategyConfig(t *testing.T) {
	cfg := DefaultStrategyConfig()

	if cfg.ScanIntervalSec != 15 {
		t.Errorf("ScanIntervalSec: 期望 15, 实际 %d", cfg.ScanIntervalSec)
	}
	if cfg.Timeframe != "15m" {
		t.Errorf("Timeframe: 期望 15m, 实际 %s", cfg.Timeframe)
	}
	if cfg.MinGainPct != 4.0 {
		t.Errorf("MinGainPct: 期望 4.0, 实际 %f", cfg.MinGainPct)
	}
	if cfg.Min24hGainPct != 4.0 {
		t.Errorf("Min24hGainPct: 期望 4.0, 实际 %f", cfg.Min24hGainPct)
	}
	if cfg.MinQuoteVolume != 10000000 {
		t.Errorf("MinQuoteVolume: 期望 10000000, 实际 %f", cfg.MinQuoteVolume)
	}
	if cfg.TopN != 10 {
		t.Errorf("TopN: 期望 10, 实际 %d", cfg.TopN)
	}
	if cfg.MaxOpenPositions != 10 {
		t.Errorf("MaxOpenPositions: 期望 10, 实际 %d", cfg.MaxOpenPositions)
	}
	if cfg.Leverage != 10 {
		t.Errorf("Leverage: 期望 10, 实际 %d", cfg.Leverage)
	}
	if cfg.PositionMarginUSDT != 10.0 {
		t.Errorf("PositionMarginUSDT: 期望 10.0, 实际 %f", cfg.PositionMarginUSDT)
	}
	if cfg.CooldownMin != 30 {
		t.Errorf("CooldownMin: 期望 30（S01 v2，止损后冷却）, 实际 %d", cfg.CooldownMin)
	}
	if cfg.MarginMode != MarginModeIsolated {
		t.Errorf("MarginMode: 期望 ISOLATED, 实际 %s", cfg.MarginMode)
	}
	if cfg.StopLossPct != 0.04 {
		t.Errorf("StopLossPct: 期望 0.04（S01 v2 紧止损 4%%）, 实际 %f", cfg.StopLossPct)
	}
	if cfg.TakeProfitPct != 0 {
		t.Errorf("TakeProfitPct: 期望 0（纯跟踪，2026-08-04 用户否决 10%%固定止盈封顶）, 实际 %f", cfg.TakeProfitPct)
	}
	if cfg.MaxHoldMin != 180 {
		t.Errorf("MaxHoldMin: 期望 180（S01 v2 最长持仓 180 分钟）, 实际 %d", cfg.MaxHoldMin)
	}
	if cfg.TrailingActivation != 0.02 {
		t.Errorf("TrailingActivation: 期望 0.02（S01 v2 更早激活跟踪）, 实际 %f", cfg.TrailingActivation)
	}
	if cfg.TrailingCallback != 0.03 {
		t.Errorf("TrailingCallback: 期望 0.03（S01 v2 松回调让利润奔跑）, 实际 %f", cfg.TrailingCallback)
	}
	if cfg.DailyLossLimitPct != 5.0 {
		t.Errorf("DailyLossLimitPct: 期望 5.0, 实际 %f", cfg.DailyLossLimitPct)
	}
	if cfg.MaxDrawdownPct != 15.0 {
		t.Errorf("MaxDrawdownPct: 期望 15.0, 实际 %f", cfg.MaxDrawdownPct)
	}
	if cfg.EnableShort {
		t.Errorf("EnableShort: 期望 false（S01 纯追涨只做多）, 实际 true")
	}
	if !cfg.EnableAddOn {
		t.Errorf("EnableAddOn: 期望 true（追加仓位开启）, 实际 false")
	}
	if cfg.MaxAddOnsPerSymbol != 2 {
		t.Errorf("MaxAddOnsPerSymbol: 期望 2（同币最多 3 仓）, 实际 %d", cfg.MaxAddOnsPerSymbol)
	}
	if cfg.ConfirmWindowMin != 2.0 {
		t.Errorf("ConfirmWindowMin: 期望 2.0（放量确认窗口 2 分钟）, 实际 %f", cfg.ConfirmWindowMin)
	}
	if cfg.ConfirmThreshold != 0 {
		t.Errorf("ConfirmThreshold: 期望 0（kline 模式关闭价格二次确认）, 实际 %f", cfg.ConfirmThreshold)
	}
	if cfg.VolumeSurgeThreshold != 1.2 {
		t.Errorf("VolumeSurgeThreshold: 期望 1.2（S01 v2 放量确认）, 实际 %f", cfg.VolumeSurgeThreshold)
	}
	if cfg.CooldownAfterTrailingMin != 15 {
		t.Errorf("CooldownAfterTrailingMin: 期望 15（S01 v2 止盈后冷却）, 实际 %d", cfg.CooldownAfterTrailingMin)
	}
	if cfg.SignalMode != "kline" {
		t.Errorf("SignalMode: 期望 kline, 实际 %s", cfg.SignalMode)
	}
	if cfg.MaxPullbackPct != 9.0 {
		t.Errorf("MaxPullbackPct: 期望 9.0, 实际 %f", cfg.MaxPullbackPct)
	}
}

// TestGetKlineOpen_DryRun 验证 DRY_RUN 模式下 GetKlineOpen 返回固定模拟值 100.0
// 保证 K 线信号模式在测试中结果可预测
func TestGetKlineOpen_DryRun(t *testing.T) {
	c := newDryRunClient()
	open, err := c.GetKlineOpen(context.Background(), "BTCUSDT", "15m")
	if err != nil {
		t.Fatalf("GetKlineOpen 返回错误: %v", err)
	}
	if open != 100.0 {
		t.Errorf("open = %v, 期望 100.0（DRY_RUN 固定模拟值）", open)
	}
}

// ==================== 二、行情接口测试（DRY_RUN） ====================

// TestFetchTickers_DryRun 验证 DRY_RUN 模式下 FetchTickers 也获取真实行情
// DRY_RUN 只模拟下单，行情数据来自真实 API（无网络时允许返回错误）
func TestFetchTickers_DryRun(t *testing.T) {
	c := newDryRunClient()
	tickers, err := c.FetchTickers(context.Background())
	// 无网络环境允许返回错误，有网络时应返回非 nil 结果
	if err != nil {
		t.Logf("无网络环境，FetchTickers 返回错误（预期行为）: %v", err)
		return
	}
	// 有网络时，DRY_RUN 模式应返回真实行情数据
	if len(tickers) == 0 {
		t.Errorf("DRY_RUN 模式有网络但返回 0 个 ticker，期望 > 0")
	}
}

// TestWsManager_GetPrice_Empty 验证新创建的 WsManager 无缓存时 GetPrice 返回 ok=false
// 新建的 WsManager priceCache 为空 map，任何 symbol 查询均应返回 false
func TestWsManager_GetPrice_Empty(t *testing.T) {
	ws := NewWsManager("DRY_RUN")
	price, ok := ws.GetPrice("BTCUSDT")
	if ok {
		t.Errorf("期望 ok=false, 实际 ok=true, price=%f", price)
	}
	if price != 0 {
		t.Errorf("期望 price=0, 实际=%f", price)
	}
}

// TestWsManager_GetPrice_Cached 验证手动写入 priceCache 后 GetPrice 返回正确价格
// 由于测试文件与源码同属 package binance，可直接访问私有字段 priceCache
func TestWsManager_GetPrice_Cached(t *testing.T) {
	ws := NewWsManager("DRY_RUN")
	// 直接写入私有缓存（同包可访问）
	ws.mu.Lock()
	ws.priceCache["BTCUSDT"] = 65000.50
	ws.priceCache["ETHUSDT"] = 3200.25
	ws.mu.Unlock()

	price, ok := ws.GetPrice("BTCUSDT")
	if !ok {
		t.Fatal("期望 ok=true, 实际 ok=false")
	}
	if price != 65000.50 {
		t.Errorf("期望 price=65000.50, 实际=%f", price)
	}

	price2, ok2 := ws.GetPrice("ETHUSDT")
	if !ok2 {
		t.Fatal("期望 ok=true, 实际 ok=false")
	}
	if price2 != 3200.25 {
		t.Errorf("期望 price=3200.25, 实际=%f", price2)
	}
}

// ==================== 三、交易接口测试（DRY_RUN） ====================

// TestOpenLong_DryRun 验证 DRY_RUN 模式下 OpenLong 返回模拟成交结果
// 期望返回 OrderResult{Symbol:"BTCUSDT", Status:"FILLED", FilledPrice:0, FilledAmount:0}
func TestOpenLong_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.OpenLong(context.Background(), "BTCUSDT", 0.001)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "BTCUSDT" {
		t.Errorf("Symbol: 期望 BTCUSDT, 实际 %s", result.Symbol)
	}
	if result.Status != "FILLED" {
		t.Errorf("Status: 期望 FILLED, 实际 %s", result.Status)
	}
	if result.FilledPrice != 0 {
		t.Errorf("FilledPrice: 期望 0, 实际 %f", result.FilledPrice)
	}
	if result.FilledAmount != 0 {
		t.Errorf("FilledAmount: 期望 0, 实际 %f", result.FilledAmount)
	}
}

// TestCloseLong_DryRun 验证 DRY_RUN 模式下 CloseLong 返回模拟成交结果
// 期望返回 OrderResult{Symbol:"BTCUSDT", Status:"FILLED", FilledPrice:0, FilledAmount:0}
func TestCloseLong_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.CloseLong(context.Background(), "BTCUSDT", 0.001)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "BTCUSDT" {
		t.Errorf("Symbol: 期望 BTCUSDT, 实际 %s", result.Symbol)
	}
	if result.Status != "FILLED" {
		t.Errorf("Status: 期望 FILLED, 实际 %s", result.Status)
	}
	if result.FilledPrice != 0 {
		t.Errorf("FilledPrice: 期望 0, 实际 %f", result.FilledPrice)
	}
	if result.FilledAmount != 0 {
		t.Errorf("FilledAmount: 期望 0, 实际 %f", result.FilledAmount)
	}
}

// TestPlaceStopMarket_DryRun 验证 DRY_RUN 模式下 PlaceStopMarket 返回新挂单结果
// 期望返回 OrderResult{Symbol:"BTCUSDT", Status:"NEW", OrderID>0}
func TestPlaceStopMarket_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.PlaceStopMarket(context.Background(), "BTCUSDT", 60000.0, 0.001, "LONG")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "BTCUSDT" {
		t.Errorf("Symbol: 期望 BTCUSDT, 实际 %s", result.Symbol)
	}
	if result.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", result.Status)
	}
	if result.OrderID <= 0 {
		t.Errorf("OrderID: 期望 >0, 实际 %d", result.OrderID)
	}
}

// TestPlaceTrailingStop_DryRun 验证 DRY_RUN 模式下 PlaceTrailingStop 返回新挂单结果
// 期望返回 OrderResult{Symbol:"ETHUSDT", Status:"NEW", OrderID>0}
func TestPlaceTrailingStop_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.PlaceTrailingStop(context.Background(), "ETHUSDT", 3500.0, 3.0, 1.5, "LONG")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "ETHUSDT" {
		t.Errorf("Symbol: 期望 ETHUSDT, 实际 %s", result.Symbol)
	}
	if result.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", result.Status)
	}
	if result.OrderID <= 0 {
		t.Errorf("OrderID: 期望 >0, 实际 %d", result.OrderID)
	}
}

// TestGetPositionMode_DryRun 验证 DRY_RUN 模式下 GetPositionMode 返回 true（假定双向持仓）
func TestGetPositionMode_DryRun(t *testing.T) {
	c := newDryRunClient()
	dual, err := c.GetPositionMode(context.Background())
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if !dual {
		t.Errorf("DRY_RUN 期望 dual=true, 实际 false")
	}
}

// TestEnsureHedgeMode_DryRun 验证 DRY_RUN 模式下 EnsureHedgeMode 直接通过（返回 nil）
func TestEnsureHedgeMode_DryRun(t *testing.T) {
	c := newDryRunClient()
	if err := c.EnsureHedgeMode(context.Background()); err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
}

// TestSetLeverage_DryRun 验证 DRY_RUN 模式下 SetLeverage 为空操作（返回 nil）
func TestSetLeverage_DryRun(t *testing.T) {
	c := newDryRunClient()
	if err := c.SetLeverage(context.Background(), "BTCUSDT", 10); err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
}

// TestEnsureLeverage_DryRun 验证 DRY_RUN 模式下 EnsureLeverage 为空操作且可重复调用（缓存逻辑不报错）
func TestEnsureLeverage_DryRun(t *testing.T) {
	c := newDryRunClient()
	for i := 0; i < 2; i++ {
		if err := c.EnsureLeverage(context.Background(), "BTCUSDT", 10); err != nil {
			t.Fatalf("第 %d 次调用期望 err=nil, 实际=%v", i+1, err)
		}
	}
}

// TestSetMarginType_DryRun 验证 DRY_RUN 模式下 SetMarginType 为空操作（返回 nil）
func TestSetMarginType_DryRun(t *testing.T) {
	c := newDryRunClient()
	if err := c.SetMarginType(context.Background(), "BTCUSDT", MarginModeIsolated); err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
}

// TestEnsureMarginMode_DryRun 验证 DRY_RUN 模式下 EnsureMarginMode 为空操作且可重复调用（缓存逻辑不报错）
func TestEnsureMarginMode_DryRun(t *testing.T) {
	c := newDryRunClient()
	for i := 0; i < 2; i++ {
		if err := c.EnsureMarginMode(context.Background(), "BTCUSDT", MarginModeIsolated); err != nil {
			t.Fatalf("第 %d 次调用期望 err=nil, 实际=%v", i+1, err)
		}
	}
}

// TestMarginModeConstants 验证保证金模式常量值正确
func TestMarginModeConstants(t *testing.T) {
	if MarginModeIsolated != "ISOLATED" {
		t.Errorf("MarginModeIsolated: 期望 ISOLATED, 实际 %s", MarginModeIsolated)
	}
	if MarginModeCross != "CROSSED" {
		t.Errorf("MarginModeCross: 期望 CROSSED, 实际 %s", MarginModeCross)
	}
}

// TestCancelOrder_DryRun 验证 DRY_RUN 模式下 CancelOrder 返回 nil（无错误）
// DRY_RUN 模式不执行真实撤单，直接返回成功
func TestCancelOrder_DryRun(t *testing.T) {
	c := newDryRunClient()
	err := c.CancelOrder(context.Background(), "BTCUSDT", 12345)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
}

// TestGetOrderStatus_DryRun 验证 DRY_RUN 模式下 GetOrderStatus 返回模拟委托信息
// 期望返回 OrderInfo{OrderID:12345, Symbol:"BTCUSDT", Status:"NEW"}
func TestGetOrderStatus_DryRun(t *testing.T) {
	c := newDryRunClient()
	info, err := c.GetOrderStatus(context.Background(), "BTCUSDT", 12345)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if info.OrderID != 12345 {
		t.Errorf("OrderID: 期望 12345, 实际 %d", info.OrderID)
	}
	if info.Symbol != "BTCUSDT" {
		t.Errorf("Symbol: 期望 BTCUSDT, 实际 %s", info.Symbol)
	}
	if info.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", info.Status)
	}
}

// TestGetOpenOrders_DryRun 验证 DRY_RUN 模式下 GetOpenOrders 返回空切片且无错误
// DRY_RUN 模式无真实挂单，返回长度为 0 的切片
func TestGetOpenOrders_DryRun(t *testing.T) {
	c := newDryRunClient()
	orders, err := c.GetOpenOrders(context.Background(), "BTCUSDT")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if orders == nil {
		t.Fatal("期望返回空切片, 实际为 nil")
	}
	if len(orders) != 0 {
		t.Errorf("期望 len=0, 实际=%d", len(orders))
	}
}

// ==================== 四、边界条件测试 ====================

// TestOpenLong_ZeroAmount 验证 DRY_RUN 模式下 amount=0 仍返回成功
// DRY_RUN 模式不做参数校验，即使数量为零也模拟成交
func TestOpenLong_ZeroAmount(t *testing.T) {
	c := newDryRunClient()
	result, err := c.OpenLong(context.Background(), "BTCUSDT", 0)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Status != "FILLED" {
		t.Errorf("Status: 期望 FILLED, 实际 %s", result.Status)
	}
}

// TestPlaceStopMarket_ZeroPrice 验证 DRY_RUN 模式下 stopPrice=0 仍返回成功
// DRY_RUN 模式不做参数校验，即使触发价为零也模拟挂单成功
func TestPlaceStopMarket_ZeroPrice(t *testing.T) {
	c := newDryRunClient()
	result, err := c.PlaceStopMarket(context.Background(), "BTCUSDT", 0, 0.001, "LONG")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", result.Status)
	}
	if result.OrderID <= 0 {
		t.Errorf("OrderID: 期望 >0, 实际 %d", result.OrderID)
	}
}

// ==================== 五、类型常量测试 ====================

// TestOrderTypeConstants 验证委托类型常量值正确
// OrderTypeStopMarket 应为 "STOP_MARKET"，OrderTypeTrailingStop 应为 "TRAILING_STOP_MARKET"
func TestOrderTypeConstants(t *testing.T) {
	if OrderTypeStopMarket != "STOP_MARKET" {
		t.Errorf("OrderTypeStopMarket: 期望 STOP_MARKET, 实际 %s", OrderTypeStopMarket)
	}
	if OrderTypeTrailingStop != "TRAILING_STOP_MARKET" {
		t.Errorf("OrderTypeTrailingStop: 期望 TRAILING_STOP_MARKET, 实际 %s", OrderTypeTrailingStop)
	}
}

// TestOrderStatusConstants 验证 6 个委托状态常量值正确
// 包括 NEW、PARTIALLY_FILLED、FILLED、CANCELED、EXPIRED、REJECTED
func TestOrderStatusConstants(t *testing.T) {
	tests := []struct {
		name     string
		got      string
		expected string
	}{
		{"OrderStatusNew", OrderStatusNew, "NEW"},
		{"OrderStatusPartiallyFilled", OrderStatusPartiallyFilled, "PARTIALLY_FILLED"},
		{"OrderStatusFilled", OrderStatusFilled, "FILLED"},
		{"OrderStatusCanceled", OrderStatusCanceled, "CANCELED"},
		{"OrderStatusExpired", OrderStatusExpired, "EXPIRED"},
		{"OrderStatusRejected", OrderStatusRejected, "REJECTED"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.got != tt.expected {
				t.Errorf("%s: 期望 %s, 实际 %s", tt.name, tt.expected, tt.got)
			}
		})
	}
}

// ==================== 六、精度缓存与查询测试 ====================

// TestLoadExchangeInfo_DryRun 验证 DRY_RUN 模式下精度缓存机制正确工作
// 手动填充 precisionMap 模拟 LoadExchangeInfo 加载结果（DRY_RUN 无网络时不实际调用 API），
// 验证 GetPrecision 能正确返回 BTCUSDT/ETHUSDT 的默认精度规则
func TestLoadExchangeInfo_DryRun(t *testing.T) {
	c := newDryRunClient()
	// 模拟 LoadExchangeInfo 加载精度数据
	c.precisionMu.Lock()
	c.precisionMap = map[string]SymbolPrecision{
		"BTCUSDT": {QtyPrecision: 3, PricePrecision: 2, StepSize: 0.001, TickSize: 0.01, MinQty: 0.001},
		"ETHUSDT": {QtyPrecision: 3, PricePrecision: 2, StepSize: 0.001, TickSize: 0.01, MinQty: 0.001},
	}
	c.precisionMu.Unlock()

	// 验证 BTCUSDT 精度
	sp, ok := c.GetPrecision("BTCUSDT")
	if !ok {
		t.Fatal("期望 BTCUSDT 存在于 precisionMap, 实际不存在")
	}
	if sp.QtyPrecision != 3 {
		t.Errorf("BTCUSDT QtyPrecision: 期望 3, 实际 %d", sp.QtyPrecision)
	}
	if sp.PricePrecision != 2 {
		t.Errorf("BTCUSDT PricePrecision: 期望 2, 实际 %d", sp.PricePrecision)
	}
	if sp.StepSize != 0.001 {
		t.Errorf("BTCUSDT StepSize: 期望 0.001, 实际 %f", sp.StepSize)
	}
	if sp.TickSize != 0.01 {
		t.Errorf("BTCUSDT TickSize: 期望 0.01, 实际 %f", sp.TickSize)
	}

	// 验证 ETHUSDT 精度
	sp2, ok2 := c.GetPrecision("ETHUSDT")
	if !ok2 {
		t.Fatal("期望 ETHUSDT 存在于 precisionMap, 实际不存在")
	}
	if sp2.QtyPrecision != 3 {
		t.Errorf("ETHUSDT QtyPrecision: 期望 3, 实际 %d", sp2.QtyPrecision)
	}
	if sp2.PricePrecision != 2 {
		t.Errorf("ETHUSDT PricePrecision: 期望 2, 实际 %d", sp2.PricePrecision)
	}
}

// TestGetPrecision 验证 GetPrecision 对已知和未知交易对的返回行为
// 已知交易对 BTCUSDT 应返回 PricePrecision=2, QtyPrecision=3；
// 未知交易对应返回零值 SymbolPrecision 和 ok=false
func TestGetPrecision(t *testing.T) {
	c := newDryRunClient()
	c.precisionMu.Lock()
	c.precisionMap = map[string]SymbolPrecision{
		"BTCUSDT": {QtyPrecision: 3, PricePrecision: 2, StepSize: 0.001, TickSize: 0.01},
	}
	c.precisionMu.Unlock()

	// 已知交易对：验证精度值正确
	sp, ok := c.GetPrecision("BTCUSDT")
	if !ok {
		t.Fatal("期望 ok=true, 实际 ok=false")
	}
	if sp.PricePrecision != 2 {
		t.Errorf("PricePrecision: 期望 2, 实际 %d", sp.PricePrecision)
	}
	if sp.QtyPrecision != 3 {
		t.Errorf("QtyPrecision: 期望 3, 实际 %d", sp.QtyPrecision)
	}

	// 未知交易对：验证返回零值和 false
	sp2, ok2 := c.GetPrecision("UNKNOWNSYMBOL")
	if ok2 {
		t.Error("期望未知交易对 ok=false, 实际 ok=true")
	}
	if sp2.PricePrecision != 0 || sp2.QtyPrecision != 0 || sp2.StepSize != 0 || sp2.TickSize != 0 {
		t.Errorf("期望零值 SymbolPrecision, 实际 %+v", sp2)
	}
}

// TestIsFuturesSymbol 验证 IsFuturesSymbol 在精度规则已加载后的判断逻辑
// "BTCUSDT" 存在于 precisionMap 应返回 true；
// "BTCUSD_PERP" 和 "UNKNOWN" 不存在应返回 false
func TestIsFuturesSymbol(t *testing.T) {
	c := newDryRunClient()
	c.precisionMu.Lock()
	c.precisionMap = map[string]SymbolPrecision{
		"BTCUSDT": {QtyPrecision: 3, PricePrecision: 2},
		"ETHUSDT": {QtyPrecision: 3, PricePrecision: 2},
	}
	c.precisionMu.Unlock()

	if !c.IsFuturesSymbol("BTCUSDT") {
		t.Error("期望 BTCUSDT 为合约交易对(true), 实际 false")
	}
	if c.IsFuturesSymbol("BTCUSD_PERP") {
		t.Error("期望 BTCUSD_PERP 非合约交易对(false), 实际 true")
	}
	if c.IsFuturesSymbol("UNKNOWN") {
		t.Error("期望 UNKNOWN 非合约交易对(false), 实际 true")
	}
}

// ==================== 七、格式化与取整工具函数测试 ====================

// TestFormatQty 验证 FormatQty 按交易对精度规则正确格式化数量字符串
// 测试三种精度场景：3 位小数截断、整数（0 位小数）、6 位小数补零
func TestFormatQty(t *testing.T) {
	c := newDryRunClient()
	c.precisionMu.Lock()
	c.precisionMap = map[string]SymbolPrecision{
		"BTCUSDT":  {QtyPrecision: 3, PricePrecision: 2, StepSize: 0.001, TickSize: 0.01},
		"XRPUSDT":  {QtyPrecision: 0, PricePrecision: 4, StepSize: 1, TickSize: 0.0001},
		"DOGEUSDT": {QtyPrecision: 6, PricePrecision: 6, StepSize: 0.000001, TickSize: 0.000001},
	}
	c.precisionMu.Unlock()

	tests := []struct {
		name     string
		symbol   string
		qty      float64
		expected string
	}{
		{"3位小数截断", "BTCUSDT", 1.23456, "1.234"},
		{"整数无小数", "XRPUSDT", 1.0, "1"},
		{"6位小数补零", "DOGEUSDT", 0.001, "0.001000"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.FormatQty(tt.symbol, tt.qty)
			if got != tt.expected {
				t.Errorf("FormatQty(%s, %v): 期望 %q, 实际 %q", tt.symbol, tt.qty, tt.expected, got)
			}
		})
	}
}

// TestFormatPrice 验证 FormatPrice 按交易对精度规则正确格式化价格字符串
// 测试 2 位小数截断和整数（0 位小数）两种场景
func TestFormatPrice(t *testing.T) {
	c := newDryRunClient()
	c.precisionMu.Lock()
	c.precisionMap = map[string]SymbolPrecision{
		"BTCUSDT": {QtyPrecision: 3, PricePrecision: 2, StepSize: 0.001, TickSize: 0.01},
		"XRPUSDT": {QtyPrecision: 0, PricePrecision: 0, StepSize: 1, TickSize: 1},
	}
	c.precisionMu.Unlock()

	tests := []struct {
		name     string
		symbol   string
		price    float64
		expected string
	}{
		{"2位小数截断", "BTCUSDT", 50000.123, "50000.12"},
		{"整数无小数", "XRPUSDT", 100, "100"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.FormatPrice(tt.symbol, tt.price)
			if got != tt.expected {
				t.Errorf("FormatPrice(%s, %v): 期望 %q, 实际 %q", tt.symbol, tt.price, tt.expected, got)
			}
		})
	}
}

// TestRoundQty 验证 RoundQty 按 stepSize 向下取整的正确性
// 测试正常截断、边界截断、不足最小步长归零三种场景
func TestRoundQty(t *testing.T) {
	c := newDryRunClient()
	c.precisionMu.Lock()
	c.precisionMap = map[string]SymbolPrecision{
		"BTCUSDT": {QtyPrecision: 3, StepSize: 0.001},
		"ETHUSDT": {QtyPrecision: 2, StepSize: 0.01},
	}
	c.precisionMu.Unlock()

	tests := []struct {
		name     string
		symbol   string
		qty      float64
		expected float64
	}{
		{"正常截断stepSize=0.001", "BTCUSDT", 1.23456, 1.234},
		{"边界截断stepSize=0.01", "ETHUSDT", 1.999, 1.99},
		{"不足步长归零", "BTCUSDT", 0.0001, 0},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := c.RoundQty(tt.symbol, tt.qty)
			if math.Abs(got-tt.expected) > 1e-9 {
				t.Errorf("RoundQty(%s, %v): 期望 %v, 实际 %v", tt.symbol, tt.qty, tt.expected, got)
			}
		})
	}
}

// TestStripTrailingZeros 验证 stripTrailingZeros 正确去除浮点数字符串末尾多余零
// 测试含小数点去零、无小数点不变、小数部分全零三种场景
func TestStripTrailingZeros(t *testing.T) {
	tests := []struct {
		name     string
		input    string
		expected string
	}{
		{"去除末尾零", "1.230000", "1.23"},
		{"无小数点不变", "100", "100"},
		{"小数部分去零", "0.10000", "0.1"},
		{"整数带小数点", "5.000", "5"},
		{"无尾零不变", "3.14", "3.14"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := stripTrailingZeros(tt.input)
			if got != tt.expected {
				t.Errorf("stripTrailingZeros(%q): 期望 %q, 实际 %q", tt.input, tt.expected, got)
			}
		})
	}
}

// ==================== 八、错误判断工具函数测试 ====================

// TestIsTransientErr 验证 isTransientErr 对各类网络错误的判断逻辑
// EOF/connection reset/timeout 为瞬时错误(true)；
// context canceled/nil/通用错误为非瞬时错误(false)
func TestIsTransientErr(t *testing.T) {
	tests := []struct {
		name     string
		err      error
		expected bool
	}{
		{"EOF错误", errors.New("unexpected EOF"), true},
		{"连接重置", errors.New("read tcp: connection reset by peer"), true},
		{"IO超时", errors.New("i/o timeout"), true},
		{"TLS握手超时", errors.New("TLS handshake timeout"), true},
		{"连接拒绝", errors.New("connection refused"), true},
		{"上下文取消", errors.New("context canceled"), false},
		{"nil错误", nil, false},
		{"通用错误", errors.New("some unknown error"), false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := isTransientErr(tt.err)
			if got != tt.expected {
				t.Errorf("isTransientErr(%v): 期望 %v, 实际 %v", tt.err, tt.expected, got)
			}
		})
	}
}

// ==================== 九、DRY_RUN 模式补充接口测试 ====================

// TestGetOrderFee_DryRun 验证 DRY_RUN 模式下 GetOrderFee 返回零手续费且无错误
// DRY_RUN 模式无真实成交，手续费固定为 0
func TestGetOrderFee_DryRun(t *testing.T) {
	c := newDryRunClient()
	fee, err := c.GetOrderFee(context.Background(), "BTCUSDT", 12345)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if fee != 0 {
		t.Errorf("期望 fee=0, 实际=%f", fee)
	}
}

// TestGetFuturesBalance_DryRun 验证 DRY_RUN 模式下 GetFuturesBalance 返回模拟余额
// 期望 TotalWalletBalance=10000, AvailableBalance=10000
func TestGetFuturesBalance_DryRun(t *testing.T) {
	c := newDryRunClient()
	balance, err := c.GetFuturesBalance(context.Background())
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if balance.TotalWalletBalance != 10000 {
		t.Errorf("TotalWalletBalance: 期望 10000, 实际 %f", balance.TotalWalletBalance)
	}
	if balance.AvailableBalance != 10000 {
		t.Errorf("AvailableBalance: 期望 10000, 实际 %f", balance.AvailableBalance)
	}
}

// TestSyncServerTime_DryRun 验证 DRY_RUN 模式下 SyncServerTime 为空操作（返回 nil）
// DRY_RUN 不访问网络，时间同步仅在 SIMULATION/LIVE 模式生效
func TestSyncServerTime_DryRun(t *testing.T) {
	c := newDryRunClient()
	if err := c.SyncServerTime(context.Background()); err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
}

// TestSetPositionMode_DryRun 验证 DRY_RUN 模式下 SetPositionMode 为空操作（返回 nil）
// DRY_RUN 模式不执行真实持仓模式切换
func TestSetPositionMode_DryRun(t *testing.T) {
	c := newDryRunClient()
	if err := c.SetPositionMode(context.Background(), true); err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if err := c.SetPositionMode(context.Background(), false); err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
}

// ==================== 十、OrderID 生成器测试 ====================

// TestNextDryRunOrderID_Uniqueness 验证 nextDryRunOrderID 连续调用 100 次生成的 ID 均唯一
// 使用 map 去重检测，确保同一客户端实例不会产生重复 OrderID
func TestNextDryRunOrderID_Uniqueness(t *testing.T) {
	c := newDryRunClient()
	seen := make(map[int64]bool, 100)
	for i := 0; i < 100; i++ {
		id := c.nextDryRunOrderID()
		if id <= 0 {
			t.Fatalf("第 %d 次生成的 OrderID=%d, 期望 >0", i+1, id)
		}
		if seen[id] {
			t.Fatalf("第 %d 次生成的 OrderID=%d 重复", i+1, id)
		}
		seen[id] = true
	}
}

// TestOpenShort_DryRun 验证 DRY_RUN 模式下 OpenShort 返回模拟结果
func TestOpenShort_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.OpenShort(context.Background(), "BTCUSDT", 0.001)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "BTCUSDT" {
		t.Errorf("Symbol: 期望 BTCUSDT, 实际 %s", result.Symbol)
	}
	if result.Side != "SELL" {
		t.Errorf("Side: 期望 SELL, 实际 %s", result.Side)
	}
	if result.Status != "FILLED" {
		t.Errorf("Status: 期望 FILLED, 实际 %s", result.Status)
	}
	if result.OrderID <= 0 {
		t.Errorf("OrderID: 期望 >0, 实际 %d", result.OrderID)
	}
}

// TestCloseShort_DryRun 验证 DRY_RUN 模式下 CloseShort 返回模拟结果
func TestCloseShort_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.CloseShort(context.Background(), "ETHUSDT", 0.5)
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "ETHUSDT" {
		t.Errorf("Symbol: 期望 ETHUSDT, 实际 %s", result.Symbol)
	}
	if result.Side != "BUY" {
		t.Errorf("Side: 期望 BUY, 实际 %s", result.Side)
	}
	if result.Status != "FILLED" {
		t.Errorf("Status: 期望 FILLED, 实际 %s", result.Status)
	}
}

// TestPlaceStopMarket_Short_DryRun 验证 DRY_RUN 模式下做空止损单
func TestPlaceStopMarket_Short_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.PlaceStopMarket(context.Background(), "BTCUSDT", 70000.0, 0.001, "SHORT")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", result.Status)
	}
	if result.AlgoID <= 0 {
		t.Errorf("AlgoID: 期望 >0, 实际 %d", result.AlgoID)
	}
}

// TestPlaceTrailingStop_Short_DryRun 验证 DRY_RUN 模式下做空跟踪止损单
func TestPlaceTrailingStop_Short_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.PlaceTrailingStop(context.Background(), "ETHUSDT", 3000.0, 2.0, 1.5, "SHORT")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", result.Status)
	}
}

// TestUpdateStopMarketPrice_DryRun 验证 DRY_RUN 模式下更新止损价
func TestUpdateStopMarketPrice_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.UpdateStopMarketPrice(context.Background(), 12345, "BTCUSDT", 65000.0, 0.001, "LONG")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Symbol != "BTCUSDT" {
		t.Errorf("Symbol: 期望 BTCUSDT, 实际 %s", result.Symbol)
	}
	if result.AlgoID <= 0 {
		t.Errorf("AlgoID: 期望 >0, 实际 %d", result.AlgoID)
	}
}

// TestPlaceTakeProfit_DryRun 验证 DRY_RUN 模式下固定止盈条件单
func TestPlaceTakeProfit_DryRun(t *testing.T) {
	c := newDryRunClient()
	result, err := c.PlaceTakeProfit(context.Background(), "BTCUSDT", 77000.0, "LONG")
	if err != nil {
		t.Fatalf("期望 err=nil, 实际=%v", err)
	}
	if result.Status != "NEW" {
		t.Errorf("Status: 期望 NEW, 实际 %s", result.Status)
	}
	if result.AlgoID <= 0 {
		t.Errorf("AlgoID: 期望 >0, 实际 %d", result.AlgoID)
	}
}

// TestDefaultStrategyConfig_NewFields 验证新增配置字段的默认值（S01 锁定后）
func TestDefaultStrategyConfig_NewFields(t *testing.T) {
	cfg := DefaultStrategyConfig()
	if cfg.EnableShort {
		t.Errorf("EnableShort: 期望 false（S01 纯追涨只做多）, 实际 %v", cfg.EnableShort)
	}
	if !cfg.EnableAddOn {
		t.Errorf("EnableAddOn: 期望 true（追加仓位开启）, 实际 %v", cfg.EnableAddOn)
	}
	if cfg.ConfirmWindowMin != 2.0 {
		t.Errorf("ConfirmWindowMin: 期望 2.0（放量确认窗口 2 分钟）, 实际 %f", cfg.ConfirmWindowMin)
	}
	if cfg.ConfirmThreshold != 0 {
		t.Errorf("ConfirmThreshold: 期望 0（kline 模式关闭价格二次确认）, 实际 %f", cfg.ConfirmThreshold)
	}
	if cfg.VolumeSurgeThreshold != 1.2 {
		t.Errorf("VolumeSurgeThreshold: 期望 1.2（S01 v2 放量确认）, 实际 %f", cfg.VolumeSurgeThreshold)
	}
	if cfg.SignalMode != "kline" {
		t.Errorf("SignalMode: 期望 kline, 实际 %s", cfg.SignalMode)
	}
	if cfg.MaxPullbackPct != 9.0 {
		t.Errorf("MaxPullbackPct: 期望 9.0, 实际 %f", cfg.MaxPullbackPct)
	}
	if cfg.TakeProfitPct != 0 {
		t.Errorf("TakeProfitPct: 期望 0（纯跟踪，固定止盈关闭）, 实际 %f", cfg.TakeProfitPct)
	}
	if cfg.MaxHoldMin != 180 {
		t.Errorf("MaxHoldMin: 期望 180（S01 v2 最长持仓 180 分钟）, 实际 %d", cfg.MaxHoldMin)
	}
	if cfg.CooldownAfterTrailingMin != 15 {
		t.Errorf("CooldownAfterTrailingMin: 期望 15（S01 v2 止盈后冷却）, 实际 %d", cfg.CooldownAfterTrailingMin)
	}
	if !cfg.EnableNewListingFilter {
		t.Errorf("EnableNewListingFilter: 期望 true（新币过滤默认开启）, 实际 %v", cfg.EnableNewListingFilter)
	}
	if cfg.NewListingMinDays != 60 {
		t.Errorf("NewListingMinDays: 期望 60（默认过滤 60 天内新币）, 实际 %d", cfg.NewListingMinDays)
	}
}

// ==================== 四、TestConnection 测试 ====================

// TestTestConnection_DryRun 验证 DRY_RUN 模式无法测试连接（无真实凭据），
// 且诊断字段 domain/proxy 正常输出（不发起网络请求）。
func TestTestConnection_DryRun(t *testing.T) {
	c := newDryRunClient()
	r := c.TestConnection(context.Background())
	if r["ok"] != "false" {
		t.Errorf("ok: 期望 false, 实际 %s", r["ok"])
	}
	if r["message"] != "DRY_RUN 模式无真实凭据，无法测试连接" {
		t.Errorf("message 不符: %s", r["message"])
	}
	if r["domain"] == "" {
		t.Error("domain 不应为空")
	}
	if r["proxy"] == "" {
		t.Error("proxy 不应为空（应为直连或代理描述）")
	}
}

// TestTestConnection_NoCredentials 验证 SIMULATION 模式未填 Key 时的提示。
func TestTestConnection_NoCredentials(t *testing.T) {
	// UseDemo/BaseApiDemoURL 为包级全局变量，恢复现场避免影响其他测试
	oldUseDemo, oldBase := futures.UseDemo, futures.BaseApiDemoURL
	t.Cleanup(func() { futures.UseDemo, futures.BaseApiDemoURL = oldUseDemo, oldBase })

	c := NewClient("", "", "SIMULATION", "", 0)
	r := c.TestConnection(context.Background())
	if r["ok"] != "false" {
		t.Errorf("ok: 期望 false, 实际 %s", r["ok"])
	}
	if r["message"] != "尚未填写 API Key / Secret" {
		t.Errorf("message 不符: %s", r["message"])
	}
	// 诊断字段：模拟盘应指向 demo 域名
	if r["domain"] != "https://demo-fapi.binance.com" {
		t.Errorf("domain: 期望 demo-fapi.binance.com, 实际 %s", r["domain"])
	}
}

// TestTestConnection_NilClient 验证客户端未初始化时安全返回。
func TestTestConnection_NilClient(t *testing.T) {
	c := &Client{}
	r := c.TestConnection(context.Background())
	if r["ok"] != "false" {
		t.Errorf("ok: 期望 false, 实际 %s", r["ok"])
	}
	if r["message"] != "客户端未初始化" {
		t.Errorf("message 不符: %s", r["message"])
	}
}

// TestNewClient_Live_ResetsDemoFlag 回归测试：全局 UseDemo 残留 bug。
// 场景：用户先切模拟盘（UseDemo=true），再切回实盘——NewClient 必须重置 UseDemo，
// 否则 REST/WS 全部误发到 demo-fapi.binance.com，实盘 Key 在测试网返回 -2015。
func TestNewClient_Live_ResetsDemoFlag(t *testing.T) {
	// 保存现场，测试后恢复，避免影响其他测试
	oldUseDemo := futures.UseDemo
	t.Cleanup(func() { futures.UseDemo = oldUseDemo })

	// 模拟用户先切模拟盘：全局 UseDemo 被置为 true
	NewClient("", "", "SIMULATION", "", 0)
	if !futures.UseDemo {
		t.Fatal("前置条件不成立：模拟盘应设置 UseDemo=true")
	}

	// 再切实盘：必须重置 UseDemo，BaseURL 回到主网
	c := NewClient("key", "secret", "LIVE", "", 0)
	if futures.UseDemo {
		t.Error("LIVE 模式必须重置全局 UseDemo=false，否则 REST/WS 误发测试网")
	}
	if c.futuresClient.BaseURL != "https://fapi.binance.com" {
		t.Errorf("LIVE 模式 BaseURL 应为 https://fapi.binance.com, 实际 %s", c.futuresClient.BaseURL)
	}
}

// ==================== 五、新币过滤 onboardDate 测试 ====================

// TestGetOnboardDate 验证 GetOnboardDate 返回缓存的上市日期（未知返回 ok=false）
func TestGetOnboardDate(t *testing.T) {
	c := newDryRunClient()
	c.onboardDateMu.Lock()
	c.onboardDateMap = map[string]int64{
		"NEWUSDT": 1753000000000,
		"OLDUSDT": 1600000000000,
	}
	c.onboardDateMu.Unlock()

	if d, ok := c.GetOnboardDate("NEWUSDT"); !ok || d != 1753000000000 {
		t.Errorf("期望 NEWUSDT 上市日期 1753000000000, 实际 %d ok=%v", d, ok)
	}
	if d, ok := c.GetOnboardDate("OLDUSDT"); !ok || d != 1600000000000 {
		t.Errorf("期望 OLDUSDT 上市日期 1600000000000, 实际 %d ok=%v", d, ok)
	}
	if _, ok := c.GetOnboardDate("UNKNOWN"); ok {
		t.Error("期望 UNKNOWN ok=false, 实际 ok=true")
	}
}
