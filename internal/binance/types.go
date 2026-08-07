// Package binance 币安接口数据类型定义
package binance

// Candle K线数据
type Candle struct {
	Timestamp int64   `json:"timestamp"`
	Open      float64 `json:"open"`
	High      float64 `json:"high"`
	Low       float64 `json:"low"`
	Close     float64 `json:"close"`
	Volume    float64 `json:"volume"`
}

// Ticker 24h 行情摘要
type Ticker struct {
	Symbol      string  `json:"symbol"`
	LastPrice   float64 `json:"lastPrice"`
	PriceChange float64 `json:"priceChangePercent"`
	QuoteVolume float64 `json:"quoteVolume"`
	HighPrice   float64 `json:"highPrice"` // 24h 最高价（山顶过滤器用）
	LowPrice    float64 `json:"lowPrice"`  // 24h 最低价（山顶过滤器用）
}

// OrderResult 下单结果
type OrderResult struct {
	OrderID      int64   `json:"orderId"`
	AlgoID       int64   `json:"algoId"` // Algo Order API 返回的条件单 ID（新接口）
	Symbol       string  `json:"symbol"`
	Side         string  `json:"side"`
	Status       string  `json:"status"`
	FilledPrice  float64 `json:"filledPrice"`
	FilledAmount float64 `json:"filledAmount"`
}

// OrderInfo 交易所委托单详情
type OrderInfo struct {
	OrderID         int64   `json:"orderId"`
	AlgoID          int64   `json:"algoId"` // Algo Order API 的条件单 ID
	Symbol          string  `json:"symbol"`
	Type            string  `json:"type"`            // STOP_MARKET / TRAILING_STOP_MARKET
	Side            string  `json:"side"`            // SELL（平多）
	Status          string  `json:"status"`          // NEW / PARTIALLY_FILLED / FILLED / CANCELED / EXPIRED
	AlgoStatus      string  `json:"algoStatus"`      // 条件单状态（NEW / CANCELED / REJECTED / EXPIRED）
	ActualOrderID   int64   `json:"actualOrderId"`   // 条件单触发后生成的实际委托 ID
	StopPrice       float64 `json:"stopPrice"`       // 止损触发价
	ActivationPrice float64 `json:"activationPrice"` // 跟踪止损激活价
	CallbackRate    float64 `json:"callbackRate"`    // 跟踪回撤比例
	FilledPrice     float64 `json:"filledPrice"`     // 成交均价
	FilledAmount    float64 `json:"filledAmount"`    // 成交量
	CreatedAt       int64   `json:"createdAt"`       // 创建时间（Unix 毫秒）
	UpdatedAt       int64   `json:"updatedAt"`       // 更新时间（Unix 毫秒）
}

// 委托类型常量
const (
	OrderTypeStopMarket   = "STOP_MARKET"
	OrderTypeTrailingStop = "TRAILING_STOP_MARKET"
	OrderTypeTakeProfit   = "TAKE_PROFIT_MARKET" // 固定止盈条件单（Algo Order API）
	OrderTypeLimit        = "LIMIT"              // 市价单被 PERCENT_PRICE 拒绝时降级使用的平仓挂单
)

// 委托状态常量
const (
	OrderStatusNew             = "NEW"
	OrderStatusPartiallyFilled = "PARTIALLY_FILLED"
	OrderStatusFilled          = "FILLED"
	OrderStatusCanceled        = "CANCELED"
	OrderStatusExpired         = "EXPIRED"
	OrderStatusRejected        = "REJECTED"
)

// Algo Order 条件单状态常量（Algo Order API 返回的 algoStatus 字段）
const (
	AlgoStatusNew      = "NEW"      // 条件单活跃，等待触发
	AlgoStatusCanceled = "CANCELED" // 条件单已取消
	AlgoStatusRejected = "REJECTED" // 条件单被拒绝
	AlgoStatusExpired  = "EXPIRED"  // 条件单已过期
)

// SymbolPrecision 交易对精度规则（来自 exchangeInfo）
// QtyPrecision: 数量小数位数（如 3 表示最多 3 位小数）
// PricePrecision: 价格小数位数
// StepSize: 数量步长（如 0.001），下单数量必须是 stepSize 的整数倍
// TickSize: 价格步长（如 0.01），下单价格必须是 tickSize 的整数倍
// MinQty: 最小下单数量
type SymbolPrecision struct {
	QtyPrecision   int     `json:"qtyPrecision"`
	PricePrecision int     `json:"pricePrecision"`
	StepSize       float64 `json:"stepSize"`
	TickSize       float64 `json:"tickSize"`
	MinQty         float64 `json:"minQty"`
}

// AccountBalance 账户余额信息
type AccountBalance struct {
	TotalWalletBalance float64 `json:"totalWalletBalance"` // 钱包总余额（USDT）
	TotalUnrealizedPnl float64 `json:"totalUnrealizedPnl"` // 未实现盈亏（USDT）
	TotalMarginBalance float64 `json:"totalMarginBalance"` // 保证金余额（钱包余额 + 未实现盈亏）
	AvailableBalance   float64 `json:"availableBalance"`   // 可用余额（USDT）
}

// ExchangePosition 交易所持仓信息（来自 positionRisk 接口）
type ExchangePosition struct {
	Symbol           string  `json:"symbol"`
	PositionSide     string  `json:"positionSide"`     // LONG / SHORT / BOTH
	PositionAmt      float64 `json:"positionAmt"`      // 持仓数量（正=多，负=空）
	EntryPrice       float64 `json:"entryPrice"`       // 开仓均价
	MarkPrice        float64 `json:"markPrice"`        // 标记价格
	UnRealizedProfit float64 `json:"unRealizedProfit"` // 未实现盈亏
	Leverage         int     `json:"leverage"`         // 杠杆倍数
	LiquidationPrice float64 `json:"liquidationPrice"` // 强平价格
	MarginType       string  `json:"marginType"`       // isolated / cross
}

// 保证金模式常量
const (
	MarginModeIsolated = "ISOLATED" // 逐仓模式：每个仓位独立保证金，互不影响
	MarginModeCross    = "CROSSED"  // 全仓模式：所有仓位共享保证金（币安 API 值为 CROSSED）
)

// StrategyConfig 策略配置参数
type StrategyConfig struct {
	ScanIntervalSec        int     `json:"scanIntervalSec"`
	Timeframe              string  `json:"timeframe"`
	MinGainPct             float64 `json:"minGainPct"`
	Min24hGainPct          float64 `json:"min24hGainPct"` // 24h 涨幅门槛（%），双条件筛选：同时满足 15m 与 24h 涨幅才入选
	MinQuoteVolume         float64 `json:"minQuoteVolume"`
	TopN                   int     `json:"topN"` // 候选数量上限，按成交额排序后只取前 N（0=不限制）
	MaxOpenPositions       int     `json:"maxOpenPositions"`
	Leverage               int     `json:"leverage"`
	PositionMarginUSDT     float64 `json:"positionMarginUsdt"`
	CooldownMin            int     `json:"cooldownMin"` // 冷却期（分钟），平仓后 N 分钟内不再开同一币
	MarginMode             string  `json:"marginMode"`  // 保证金模式：ISOLATED（逐仓）/ CROSSED（全仓）
	StopLossPct            float64 `json:"stopLossPct"`
	TrailingActivation     float64 `json:"trailingActivation"`
	TrailingCallback       float64 `json:"trailingCallback"`
	DailyLossLimitPct      float64 `json:"dailyLossLimitPct"`      // 日损限制（%）：当日累计亏损达到此比例后停止开新仓
	MaxDrawdownPct         float64 `json:"maxDrawdownPct"`         // 最大回撤（%）：账户从近期高点回撤达到此比例后全面熔断
	EnableShort            bool    `json:"enableShort"`            // 是否启用做空
	EnableAddOn            bool    `json:"enableAddOn"`            // 是否启用追加仓位：持仓币移动止盈激活（现价>=首仓入场价*(1+TrailingActivation)）且再次命中信号时追加 1 张独立新单
	ConfirmWindowMin       float64 `json:"confirmWindowMin"`       // 短窗口确认时长（分钟），0=关闭
	ConfirmThreshold       float64 `json:"confirmThreshold"`       // 短窗口涨幅确认阈值（%），0=关闭
	VolumeSurgeThreshold   float64 `json:"volumeSurgeThreshold"`   // 成交量放大倍数阈值，0=关闭
	SignalMode             string  `json:"signalMode"`             // 信号模式：kline=15m K线实体实时检测（当前价相对K线开盘价），sliding=滑动窗口过程涨幅
	MaxPullbackPct         float64 `json:"maxPullbackPct"`         // 山顶过滤器（%）：当前价距 24h 最高/最低价回撤超过该值不追（0=关闭）
	TakeProfitPct          float64 `json:"takeProfitPct"`          // 固定止盈比例（0=关闭）：价格达到入场价*(1+该比例)先止盈，与跟踪止盈先到先平
	MaxHoldMin             int     `json:"maxHoldMin"`             // 最长持仓分钟数（0=关闭）：超过后按当前价市价平仓，防止仓位长期滞留
	EnableNewListingFilter bool    `json:"enableNewListingFilter"` // 新币过滤开关：过滤上市天数 <= NewListingMinDays 的新上市合约（默认开启）
	NewListingMinDays      int     `json:"newListingMinDays"`      // 新币过滤天数阈值（天）：上市天数小于等于该值的合约不参与任何开仓（默认 60，0=关闭）
}

// DefaultStrategyConfig 返回默认策略配置（S01 纯追涨·无门控，2026-08-04 锁定）
// 回测口径：-mode momentum -closed=false -exit-mode=close -topn 10 -maxpos 5 -minvol 100000
//
//	-cooldown 60 -gain 4 -min24gain 4 -surge 1.5 -sl 6 -tp 10 -act 3 -cb 2
//	-hold 120 -margin 10 -lev 10 -only-long
//
// 回测绩效（2024-01~2026-08 全 567 币 5m 数据）：21,274 笔、胜率 55.3%、PF 1.16、
// +4449U / +445% / 回撤 12.3%，三年分段全部盈利。EMA 门控验证为负优化，故锁定无门控版本。
// 变更（2026-08-04 生效）：① 固定止盈 TakeProfitPct 0.10→0（-tp 0 纯跟踪）。原因：10% 固定止盈
// 到点即平，截断 3% 激活的移动止盈在 10% 之后的利润奔跑空间，用户否决该封顶设计。
// ② 最大持仓 MaxOpenPositions 5→10（用户要求扩大持仓容量）。
// ①② 均未做三年回测验证（S01 基线口径为 -tp 10 -maxpos 5 的 +4449U），模拟盘实盘验证中。
// 实盘映射：15m K 线实体实时确认、24h 涨幅 >= 4% 双条件、放量确认 1.5x / 2 分钟窗口、
// Top 10 候选、10x 杠杆 / 10U 保证金、60 分钟冷却、纯多（EnableShort=false）、6% 固定止损、
// 3% 跟踪激活 + 2% 跟踪回撤（纯跟踪，无固定止盈封顶）、最长持仓 120 分钟超时平仓、
// 日亏 5% 熔断、账户回撤 15% 熔断、山顶过滤器 9%
func DefaultStrategyConfig() StrategyConfig {
	return StrategyConfig{
		ScanIntervalSec:        15,
		Timeframe:              "15m",
		MinGainPct:             4.0,
		Min24hGainPct:          4.0, // 双条件筛选：24h 涨幅 >= 4% 且 15m K 线涨幅 >= 4%
		MinQuoteVolume:         10000000, // 24h 成交额下限 1000 万 USDT（2026-08-07 用户要求 10 万→1000 万，过滤小市值低流动性币）
		TopN:                   10,
		MaxOpenPositions:       10,   // 最大同时持仓 10（2026-08-04 用户要求 5→10）
		Leverage:               10,   // 10x 杠杆
		PositionMarginUSDT:     10.0, // 每仓保证金 10U
		CooldownMin:            60,
		MarginMode:             MarginModeIsolated,
		StopLossPct:            0.06,    // 6% 固定止损（10x 爆仓线约 10%，止损早于爆仓）
		TrailingActivation:     0.03,    // 3% 涨幅激活移动止损（让利润奔跑）
		TrailingCallback:       0.02,    // 激活后回撤 2% 平仓（保护已得利润）
		DailyLossLimitPct:      5.0,     // 日亏 5% 熔断停手（已接入引擎）
		MaxDrawdownPct:         15.0,    // 账户回撤 15% 全面熔断（已接入引擎）
		EnableShort:            false,   // S01 纯追涨：只做多，不做空
		EnableAddOn:            true,    // 追加仓位：移动止盈激活 + 再次命中信号 → 追加 1 张独立新单（2026-08-04 用户要求）
		ConfirmWindowMin:       2.0,     // 放量确认窗口 2 分钟：最近 2 分钟成交量速率 vs 之前 13 分钟
		ConfirmThreshold:       0,       // 价格二次确认对 kline 模式关闭（K 线实体确认已过滤噪音）
		VolumeSurgeThreshold:   1.5,     // 放量确认 1.5x：成交量放大不足 1.5 倍不追（防无量假突破）
		SignalMode:             "kline", // 15m K 线实体实时检测（默认）
		MaxPullbackPct:         9.0,     // 距 24h 最高/最低回撤超 9% 不追
		TakeProfitPct:          0,       // 固定止盈 0=关闭（纯跟踪，2026-08-04 用户否决 10% 封顶）；>0 时价格达到入场价*(1+该比例) 先止盈
		MaxHoldMin:             120,     // 最长持仓 120 分钟：超时按当前价平仓
		EnableNewListingFilter: true,    // 新币过滤：排除上市 60 天内的新合约（无历史数据、波动剧烈、追涨风险高）
		NewListingMinDays:      60,
	}
}
