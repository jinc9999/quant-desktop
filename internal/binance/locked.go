package binance

// LockedProfileConfig 返回 C 版“超能战士”锁定策略配置。
// 参数为 2026-08-12 上下文快照中的 A/B 实盘配置（数据库为准），
// 编译进二进制后不提供任何界面/接口查看或修改。
// profile: "A"=进攻模式 / "B"=稳健模式，其他值按 A 处理。
func LockedProfileConfig(profile string) StrategyConfig {
	base := StrategyConfig{
		StrategyVersion:         "C1.0_20260812",
		ScanIntervalSec:         15,
		Timeframe:               "15m",
		WarmupMin:               15,   // 启动预热 15 分钟
		TopN:                    10,   // 候选数量 Top 10
		MaxOpenPositions:        15,   // 最大同时持仓 15
		Leverage:                10,   // 10x 杠杆
		PositionMarginUSDT:      10.0, // 每仓保证金 10U
		CooldownMin:             30,   // 止损/超时平仓后冷却 30 分钟
		CooldownAfterTrailingMin: 15,  // 移动止盈平仓后冷却 15 分钟
		MarginMode:              MarginModeIsolated,
		TrailingActivation:      0.02, // 移动止损激活 +2%
		TrailingCallback:        0.03, // 激活后回撤 3% 平仓
		DailyLossLimitPct:       5.0,  // 日亏 5% 熔断
		MaxDrawdownPct:          15.0, // 账户回撤 15% 熔断
		EnableShort:             false,
		EnableAddOn:             true, // 追加仓位开启
		MaxAddOnsPerSymbol:      2,    // 同币最多 1+2=3 仓
		ConfirmWindowMin:        2.0,
		ConfirmThreshold:        0,
		VolumeSurgeThreshold:    1.2,
		SignalMode:              "kline",
		MaxPullbackPct:          9.0,
		TakeProfitPct:           0,    // 固定止盈关闭（纯跟踪）
		MaxHoldMin:              180,  // 最长持仓 180 分钟
		EnableNewListingFilter:  true, // 新币过滤
		NewListingMinDays:       60,
	}

	switch profile {
	case "B":
		base.StrategyName = "超能战士·稳健模式"
		base.MinGainPct = 4.0        // 入场 15m 涨幅 4%
		base.Min24hGainPct = 4.0     // 24h 涨幅 4%
		base.RankMode = 1            // 24h 涨幅排名过滤：前 N%
		base.RankParam = 10          // 前 10%
		base.MinQuoteVolume = 10000000 // 24h 成交额 >= 1000 万
		base.StopLossPct = 0.04      // 固定止损 4%
	default:
		base.StrategyName = "超能战士·进攻模式"
		base.MinGainPct = 3.0        // 入场 15m 涨幅 3%
		base.Min24hGainPct = 4.0     // 24h 涨幅 4%
		base.RankMode = 0            // 排名过滤关闭
		base.RankParam = 20
		base.MinQuoteVolume = 20000000 // 24h 成交额 >= 2000 万
		base.StopLossPct = 0.03      // 固定止损 3%
	}
	return base
}
