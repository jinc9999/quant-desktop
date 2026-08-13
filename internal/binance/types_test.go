package binance

import "testing"

// TestDefaultStrategyConfig_SmartSize 验证智慧版 5m 爆拉仓位默认值：
// A/B 构建不传 -X 时默认关闭（SmartSizeMode=0），参数落在回测验证区间。
// D 版构建通过 -X defaultSmartSizeMode=1 覆盖为开启。
func TestDefaultStrategyConfig_SmartSize(t *testing.T) {
	cfg := DefaultStrategyConfig()
	if cfg.SmartSizeMode != 0 {
		t.Fatalf("A/B 默认 SmartSizeMode=%d, want 0（智慧版开关默认关闭）", cfg.SmartSizeMode)
	}
	if cfg.SmartSizeHigh != 1.5 {
		t.Fatalf("SmartSizeHigh=%v, want 1.5", cfg.SmartSizeHigh)
	}
	if cfg.SmartSizeLow != 0.7 {
		t.Fatalf("SmartSizeLow=%v, want 0.7", cfg.SmartSizeLow)
	}
	if cfg.SmartSizeBoundary != 2.5 {
		t.Fatalf("SmartSizeBoundary=%v, want 2.5", cfg.SmartSizeBoundary)
	}
}

// TestDefaultStrategyConfig_A 当前 A 实盘参数默认值（D 构建用 -X 覆盖为 gain3/2000万/sl3/min24h0）
func TestDefaultStrategyConfig_A(t *testing.T) {
	cfg := DefaultStrategyConfig()
	if cfg.StrategyName != "币安-魔力进攻A策略" {
		t.Fatalf("默认策略名=%s, want A", cfg.StrategyName)
	}
	if cfg.MinGainPct != 4.0 || cfg.MinQuoteVolume != 10000000 || cfg.StopLossPct != 0.04 {
		t.Fatalf("A 默认参数异常: gain=%v vol=%v sl=%v", cfg.MinGainPct, cfg.MinQuoteVolume, cfg.StopLossPct)
	}
}
