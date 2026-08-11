// Package main 回测策略引擎：与 quant-desktop 实盘默认配置对齐的
// "选币 + 放量确认 + 止盈止损" 完整交易逻辑。
//
// 策略参数（与 quant-desktop 默认配置一致）:
//   - 信号: 15m K 线实体涨幅 >=5%（当前收盘 vs 15m 周期开盘）+ 24h 涨幅 >=5%
//   - 放量: 当前 5m 成交额 >= 前 24 根 5m 平均成交额 x 1.8（2 小时基准窗口）
//   - 山顶过滤: 距 24h 最高/最低价回撤 >9% 不追
//   - 杠杆 10x / 每仓保证金 20U / 最多 5 仓 / 冷却 20 分钟
//   - 止损 8% / 跟踪止盈: 浮盈 3% 激活, 回撤 2% 平仓 / 双向交易
package main

import (
	"fmt"
	"math"
	"sort"
)

// StrategyConfig 回测策略配置（对齐 quant-desktop 默认值）
type StrategyConfig struct {
	MinGainPct           float64 // 15m 实体涨幅阈值(%)
	Min24hGainPct        float64 // 24h 涨幅阈值(%)
	MinQuoteVolume       float64 // 24h 成交额下限(USDT)
	VolumeSurgeThreshold float64 // 放量倍数阈值
	SurgeLookback        int     // 放量基准窗口(K 线根数)
	MaxPullbackPct       float64 // 山顶过滤器回撤上限(%)
	MinTakerBuyPct       float64 // 15m 窗口主动买占比门槛(%)(0 关闭)
	RetracePct           float64 // S01 回踩实验: 信号后回踩深度%(0 关闭)
	RetraceMaxBars       int     // S01 回踩实验: 最长等待 K 线数（超时放弃）
	SizeMode             int     // S01 仓位倾斜: 0=均仓 1=按15m涨幅 2=按放量 3=按主动买 4=组合
	SizeTilt             float64 // 每 1σ 信号强度调整仓位倍数（默认 0.3 = ±30%）
	SizeMin              float64 // 仓位倍数下限（默认 0.5）
	SizeMax              float64 // 仓位倍数上限（默认 1.5）
	PriceSizeTh          float64 // 按币价减仓: 低于该价减半仓（USDT，默认 0.05）
	TakerExitPct         float64 // S01 实验: 浮盈持仓 15m 主动买占比跌破该值提前止盈(%)(0 关闭)
	RankMode             int     // 24h 涨幅排名过滤: 0=关 1=前N% 2=前M名（替代固定 24h 涨幅）
	RankParam            float64 // 排名参数: 模式1=百分位(%) 模式2=名数
	TrendMode            int     // 趋势因子: 0=关 1=EMA50向上 2=价>EMA96 3=4h涨>0 4=4h涨>2% 5=价>4hVWAP
	TopN                 int     // 候选排序取前 N
	MaxOpenPositions     int     // 最大同时持仓数
	Leverage             float64 // 杠杆倍数
	PositionMarginUSDT   float64 // 每仓保证金(USDT)
	CooldownMs           int64   // 平仓后冷却(ms)
	StopLossPct          float64 // 止损比例(0.08 = 8%)
	TrailingActivation   float64 // 跟踪止盈激活涨幅(0.03 = 3%)
	TrailingCallback     float64 // 跟踪止盈回撤比例(0.02 = 2%)
	EnableShort          bool    // 是否允许做空
	OnlyShort            bool    // 仅做空（过滤掉 LONG 信号，用于只做空策略）
	InitialEquity        float64 // 初始权益(USDT)
	ClosedBarConfirm     bool    // 仅用已收盘 15m K 线评估实体信号（等收线，避免盘中追高）
	ExitClose            bool    // 退出检测用片收盘价而非片内高低价（近似 tick 采样，不捕捉片内插针）
	TakeProfitPct        float64 // 固定止盈比例（0 = 关闭；>0 时价格达到该涨幅先止盈，与移动止盈先到先平）
	MaxHoldBars          int     // 最长持仓 5m K 线数（0 = 关闭；超时按片收盘价平仓）
	FeeRate              float64 // 单边手续费率（taker 0.0004 = 0.04%）
	Mode                 string  // 信号范式: "momentum" 追涨 / "mr" 均值回归 / "trend" 趋势跟随

	// 均值回归（mr）参数
	MRDropPct        float64 // 触发条件: 5m K 线实体跌幅 >= 该值(0.03=3%)
	MRTpPct          float64 // 反弹止盈: 涨幅 >= 该值平仓(0.02=2%)
	MRSlPct          float64 // 反弹失败止损: 再跌该值平仓(0.015=1.5%)
	MRMaxHoldBars    int     // 最长持有 5m K 线数（超时平仓）
	MRMinDrawdownPct float64 // 触发条件: 距 24h 最高价最小回撤(0.02=2%，确认已回调)
	MRMaxDrawdownPct float64 // 过滤条件: 距 24h 最高价最大回撤(0.15=15%，悬崖不接)

	// 趋势跟随（trend）参数（5m K 线 EMA 周期）
	TrendFast int // 快线 EMA 周期（96≈8h）
	TrendSlow int // 慢线 EMA 周期（288≈24h）

	// 资金费率套利（funding）参数
	FundTh      float64 // 开仓阈值: 费率 <= -FundTh 做多收取 / >= FundTh 做空收取（0.0005 = 0.05%）
	FundExitTh  float64 // 费率回归平仓阈值: |费率| 回落至该值以下平仓（0.0001 = 0.01%）
	FundMaxHold int     // 最长持有资金费结算周期数（3 = 24h）
	FundSLPct   float64 // 价格止损比例（0.05 = 5%，保护非中性敞口的突发反向行情）

	// ===== v6 异动币策略参数（定义见 docs/superpowers/plans/2026-08-08-v6-refactor-plan.md）=====
	MinPrice                 float64 // L1: 价格 > 该值（USDT，默认 5）
	MaxATRPct                float64 // L1: ATR14 / close × 100 ≤ 该值（默认 8%）
	BBPeriod                 int     // 布林带周期（默认 20）
	BBMult                   float64 // 布林带倍数（默认 1.5σ）
	BBWidthWindow            int     // 布林带宽度分位窗口（默认 288 根 5m = 24h）
	BBWidthMinPct            float64 // L2 硬门槛: 宽度分位 < 该值（默认 30%）
	RSIPeriod                int     // RSI 周期（默认 14）
	RSIMin                   float64 // L2 趋势确认下限（默认 40）
	RSIMax                   float64 // L2 趋势确认上限（默认 70）
	L2VolMult                float64 // L2 量能: (当前+前1根) ≥ 前 N 根均值 × 该值（默认 2.0）
	L2VolLookback            int     // L2 量能前 N 根（默认 5）
	L2MinScore               float64 // L2 原始分门槛（默认 60）
	L3MinScore               float64 // L3 加权总分门槛（默认 70）
	OIZWindow                int     // ΔOI(Z)/成交量 Z 的窗口（默认 24 根 5m）
	TierBigVolume            float64 // 大币 24h 成交额下限（默认 5 亿 USDT）
	TierMidVolume            float64 // 中币 24h 成交额下限（默认 5000 万 USDT）
	OIZBig                   float64 // 大币 ΔOI Z-Score 阈值（默认 2.5）
	OIZMid                   float64 // 中币 ΔOI Z-Score 阈值（默认 2.0）
	OIZSmall                 float64 // 小币 ΔOI Z-Score 阈值（默认 1.5）
	FundVetoBig              float64 // 大币费率过热否决阈值（小数，默认 0.005 = 0.5%）
	FundVetoMid              float64 // 中币费率过热否决阈值（默认 0.01 = 1%）
	FundVetoSmall            float64 // 小币费率过热否决阈值（默认 0.02 = 2%）
	FactorW1                 float64 // L3 因子1 ΔOI 权重（默认 0.35）
	FactorW2                 float64 // L3 因子2 资金费率权重（默认 0.30）
	FactorW3                 float64 // L3 因子3 RSI 权重（默认 0.20）
	FactorW4                 float64 // L3 因子4 盘口深度权重（默认 0.15；回测归零并归一化）
	RiskPct                  float64 // L4 风险预算: 账户×该值（默认 1%）
	SingleCoinMarginPct      float64 // C6 单币保证金上限: 账户×该值（默认 0.5%）
	MaxLeverageExposure      float64 // C6 总敞口上限: 名义价值 ≤ 账户×该值（默认 3x）
	DailyLossPct             float64 // C6 日亏损熔断（默认 2%）
	MaxConsecutiveLosses     int     // C6 连续亏损熔断（默认 5 单）
	SlippageBig              float64 // 滑点: 大币（默认 0.05%）
	SlippageMid              float64 // 滑点: 中币（默认 1%）
	SlippageSmall            float64 // 滑点: 小币（默认 2%）
	ATRDecayPct              float64 // L5 波动率衰减: ATR ≤ 开仓后峰值×该值（默认 0.5）
	ATRDecayMinHoldBars      int     // L5 波动率衰减最小持仓（默认 6 根 = 30 分钟）
	FundReversalMult         float64 // L5 费率反转: 费率 > 入场时×该值（默认 1.5）
	NewListingMinDays        int     // L1 新币过滤: 上市天数 > 该值（默认 60）
	CooldownAfterTrailingMin int     // 实验: 移动止盈平仓后的冷却分钟数（-1=统一用 CooldownMs；0=立即再入）
	EnableAddOn              bool    // 实验: 启用追加仓位（同币移动止盈激活后再命中信号可加仓）
	MaxAddOnsPerSymbol       int     // 实验: 单币最大追加次数（默认 1，即同币最多 1+1 两仓）
	AddOnActPct              float64 // 实验: 追单激活门槛（0=与移动止盈激活一致；>0 要求同币持仓极值达到首仓入场价±该比例才允许追，过滤小冲高追顶）

	// ===== S01 单因子实验开关（默认全关，不改变 S01 现有行为）=====
	FundingVetoEnabled bool    // 实验: 费率过热否决（正费率 ≥ 分级阈值不追）
	FundingCostEnabled bool    // 实验: 持仓期间资金费成本计入盈亏（每 8h 结算点按费率×名义值扣收）
	SectorMax          int     // 实验: 同板块同时持仓上限（0=关闭；OTHERS 不设限）
	VolumeZThreshold   float64 // 实验: 成交量 Z-Score 确认阈值（0=关闭）
	RSIFilterEnabled   bool    // 实验: RSI[RSIMin,RSIMax] 趋势带确认
	Regime             string  // 实验: 市场状态过滤 none/btc24h/btcma/breadth（""=关闭）
	RegimeParam        float64 // 实验: 过滤阈值（btc24h=24h涨幅%门槛；breadth=上涨币占比 0~1）

	// 自适应融合（adaptive）参数: 按 BTC 市场状态动态切换追涨/回踩/做空三模式
	AdaptATRTh           float64 // BTC ATR% 阈值: ATR%<=该值 且 BTC>EMA 判定为回踩模式（0.02=2%）
	AdaptBTCEMA          int     // BTC 状态判断均线周期（5m 粒度，默认 50）
	AdaptDisablePullback bool    // 关闭回踩模式（趋势优先，只追涨/做空）
	AdaptDisableChase    bool    // 关闭追涨模式（震荡优先）
	RBPullGain           float64 // 回踩信号: 24h 涨幅门槛（5.0 = 5%）
	RBEMA                int     // 回踩信号: 支撑均线周期（5m 粒度，默认 20）
	RBShrink             float64 // 回踩信号: 缩量倍数（当前成交额 < 前24根均值×该值，0.7）
	RBStable             int     // 回踩信号: 企稳根数（触及EMA后连续收盘>=EMA 的根数，默认 3）
	RBSL                 float64 // 回踩止损（0.025）
	RBTP                 float64 // 回踩固定止盈（0.05）
	RBAct                float64 // 回踩移动止盈激活（0.02）
	RBCb                 float64 // 回踩移动止盈回调（0.01）
	RBHold               int     // 回踩最长持仓片数（24 = 120min）
	SSL                  float64 // 做空模式止损（0.05）
	STP                  float64 // 做空模式固定止盈（0.08）
	SAct                 float64 // 做空模式移动止盈激活（0.02）
	SCb                  float64 // 做空模式移动止盈回调（0.015）
	SHold                int     // 做空模式最长持仓片数（18 = 90min）
	DailyMax             int     // 单日最大开仓数（0 = 不限）
}

// DefaultConfig 返回与实盘默认配置一致的策略参数
// 返回: *StrategyConfig 策略配置实例
func DefaultConfig() *StrategyConfig {
	return &StrategyConfig{
		MinGainPct:               5.0,
		Min24hGainPct:            5.0,
		MinQuoteVolume:           50000,
		VolumeSurgeThreshold:     1.8,
		SurgeLookback:            24, // 2 小时基准窗口（24 根 5m）
		MaxPullbackPct:           9.0,
		MinTakerBuyPct:           0,
		RetracePct:               0,
		RetraceMaxBars:           6,
		SizeMode:                 0,
		SizeTilt:                 0.3,
		SizeMin:                  0.5,
		SizeMax:                  1.5,
		PriceSizeTh:              0.05,
		TakerExitPct:             0,
		RankMode:                 0,
		RankParam:                10,
		TrendMode:                0,
		TopN:                     8,
		MaxOpenPositions:         5,
		Leverage:                 10,
		PositionMarginUSDT:       20.0,
		CooldownMs:               20 * 60 * 1000,
		StopLossPct:              0.08,
		TrailingActivation:       0.03,
		TrailingCallback:         0.02,
		EnableShort:              true,
		InitialEquity:            1000.0,
		ClosedBarConfirm:         true,
		TakeProfitPct:            0,
		MaxHoldBars:              0,
		FeeRate:                  0.0004,
		Mode:                     "momentum",
		CooldownAfterTrailingMin: -1, // 统一冷却（分原因冷却实验默认关闭）
		MRDropPct:                0.03,
		MRTpPct:                  0.02,
		MRSlPct:                  0.015,
		MRMaxHoldBars:            24,
		MRMinDrawdownPct:         0.02,
		MRMaxDrawdownPct:         0.15,
		TrendFast:                96,
		TrendSlow:                288,
		FundTh:                   0.0005,
		FundExitTh:               0.0001,
		FundMaxHold:              3,
		FundSLPct:                0.05,
		AdaptATRTh:               0.02,
		AdaptBTCEMA:              50,
		RBPullGain:               5.0,
		RBEMA:                    20,
		RBShrink:                 0.7,
		RBStable:                 3,
		RBSL:                     0.025,
		RBTP:                     0.05,
		RBAct:                    0.02,
		RBCb:                     0.01,
		RBHold:                   24,
		SSL:                      0.05,
		STP:                      0.08,
		SAct:                     0.02,
		SCb:                      0.015,
		SHold:                    18,
		DailyMax:                 0,
	}
}

// WindowBars 24h 窗口 K 线数（5m 周期: 288 根）
const WindowBars = 288

// symbolState 单币种滚动市场状态（按 5m 片推进，环形缓冲）
type symbolState struct {
	closes          [WindowBars]float64 // 环形: 24h 收盘价
	highs           [WindowBars]float64 // 环形: 24h 最高价
	lows            [WindowBars]float64 // 环形: 24h 最低价
	quoteVols       [WindowBars]float64 // 环形: 24h 成交额
	idx             int                 // 环形写入索引（指向最旧槽位）
	filled          int                 // 已写入样本数（<288 为预热期）
	sumVol24        float64             // 24h 累计成交额（滚动维护）
	periodOpen      float64             // 当前 15m 周期开盘价
	periodTS        int64               // 当前 15m 周期起点
	hasPeriod       bool                // 15m 周期是否已初始化
	periodVol       float64             // 当前 15m 周期累计成交量（主动买占比用）
	periodTBB       float64             // 当前 15m 周期累计主动买量（主动买占比用）
	lastClose       int64               // 该币最近平仓时间（冷却用）
	lastCloseReason string              // 该币最近平仓原因（分原因冷却用）
	vols            [WindowBars]float64 // 环形: 24h 成交量（VWAP 计算用）
	trendEma        float64             // 趋势因子: EMA（50 或 96 周期）
	prevTrendEma    float64             // 趋势因子: 上一 EMA（判断向上）
	trendEmaInit    bool                // 趋势因子: EMA 是否已初始化
	fastEma         float64             // 趋势模式: 快线 EMA
	slowEma         float64             // 趋势模式: 慢线 EMA
	prevFast        float64             // 上一根快线 EMA（交叉检测用）
	prevSlow        float64             // 上一根慢线 EMA（交叉检测用）
	emaInit         bool                // EMA 是否已初始化
	rbEma           float64             // 自适应回踩模式: 支撑 EMA（RBEMA 周期）
	rbEmaInit       bool                // 回踩 EMA 是否已初始化
	rbTouched       bool                // 是否已发生过触及 EMA 的回踩
	rbStableCnt     int                 // 触及后连续收盘 >= EMA 的片数（企稳计数）

	// v6 指标状态（仅在 Mode=="v6" 时维护）
	firstTS      int64        // 该币首根 K 线时间（上市日，新币过滤用）
	rsi          float64      // RSI(14)（Wilder）
	rsiInit      bool         // RSI 是否已初始化
	rsiSeedSumG  float64      // RSI 种子期涨幅累计
	rsiSeedSumL  float64      // RSI 种子期跌幅累计
	rsiSeedCnt   int          // RSI 种子期计数
	rsiAvgGain   float64      // Wilder 平均涨幅
	rsiAvgLoss   float64      // Wilder 平均跌幅
	trRing       [14]float64  // TR 环形缓冲（ATR14）
	trIdx        int          // TR 环形写入索引
	trFilled     int          // TR 已写入数
	atr          float64      // ATR14
	bbWidths     [288]float64 // 布林带宽度历史（分位计算）
	bbIdx        int          // 宽度环形写入索引
	bbFilled     int          // 宽度已写入数
	bbWidthPrev  float64      // 上一根已完成 K 线的宽度（突破当根的挤压判定基准）
	bbHasPrev    bool         // 上一根宽度是否已记录
	bbSqueezePct float64      // 上一根宽度在其历史窗口中的百分位（0~1）
}

// Position 一笔持仓记录（含待成交 pending 态）
type Position struct {
	Symbol           string
	Side             string // LONG / SHORT
	EntryTS          int64
	EntryPrice       float64
	Amount           float64 // 数量 = 名义价值 / 入场价
	ExtremePrice     float64 // 跟踪止盈极值（多头=最高价, 空头=最低价）
	TrailingActive   bool    // 是否已激活跟踪止盈
	Pending          bool    // 待下一片开盘成交
	RetracePct       float64 // S01 回踩实验: 回踩深度%（0=信号后立即成交）
	RetraceRef       float64 // 回踩基准价（信号 bar 收盘价）
	RetraceSeenDip   bool    // 是否已出现满足深度的回踩
	RetraceDipHigh   float64 // 回踩 bar 最高价（反弹需收复）
	RetraceConfirmed bool    // 回踩后已收复，下一片开盘成交
	RetraceBars      int     // 已等待 K 线数
	RetraceMax       int     // 最长等待 K 线数（超时放弃）
	FundingCollected float64 // funding 模式: 持有期间累计收取的资金费（USDT）
	FundIntervals    int     // funding 模式: 已收取资金费的结算周期数
	Mode             string  // 自适应模式: chase / pullback / short（开仓时固化）
	SLPct            float64 // 该仓止损比例（按模式固化）
	TPPct            float64 // 该仓固定止盈比例
	ActPct           float64 // 该仓移动止盈激活比例
	CbPct            float64 // 该仓移动止盈回调比例
	HoldBars         int     // 该仓最长持仓片数
	Margin           float64 // 占用保证金（v6 动态仓位；旧模式=固定每仓保证金）
	Notional         float64 // 目标名义价值（v6 动态仓位；旧模式=保证金×杠杆）
	Slippage         float64 // 分级滑点（v6，大/中/小）
	ATRPeak          float64 // 开仓后 ATR14 峰值（波动率衰减退出用）
	EntryATR         float64 // 入场时 ATR14
	EntryFunding     float64 // 入场时资金费率（费率反转退出用）
	Score            float64 // L3 加权总分（置信度因子用）
	Tier             string  // 币种分级 big/mid/small
	ChaseType        string  // 追涨/回踩分类: first/chase/pullback/flat（回测验证用）
}

// btcState BTC 市场状态（自适应融合模式用）: EMA 判断牛熊 + ATR 判断波动
type btcState struct {
	ema        float64     // BTC EMA（AdaptBTCEMA 周期）
	emaInit    bool        // EMA 是否已初始化
	close      float64     // 最近收盘价
	atr        float64     // ATR14（绝对值）
	tr         [14]float64 // TR 环形缓冲
	trIdx      int
	trCnt      int
	closes     [WindowBars]float64 // 24h 收盘环形（btc24h 市场过滤用）
	cIdx       int                 // 环形写入位置
	cCnt       int                 // 已写入根数
	chg24      float64             // BTC 24h 涨幅（%）
	chg24Ready bool                // 24h 窗口是否已满
}

// fundingPoint 单条资金费率结算记录（8h 边界）
type fundingPoint struct {
	ts        int64   // 结算时间戳
	rate      float64 // 资金费率（0.0001 = 0.01%）
	markPrice float64 // 结算时标记价格
}

// Trade 一笔已完成的交易记录
type Trade struct {
	Symbol    string
	Side      string
	EntryTS   int64
	EntryPx   float64
	ExitTS    int64
	ExitPx    float64
	Amount    float64
	PnL       float64
	PnLPct    float64 // 相对名义价值的收益率(%)
	Reason    string  // STOP_LOSS / TRAILING_STOP
	HeldBars  int     // 持仓 K 线数
	ChaseType string  // 追涨/回踩分类
}

// EquityPoint 权益曲线采样点
type EquityPoint struct {
	TS     int64
	Equity float64
}

// Engine 回测引擎
type Engine struct {
	cfg           *StrategyConfig
	states        map[string]*symbolState
	positions     []*Position // 已成交持仓
	pending       []*Position // 待成交开仓
	trades        []*Trade
	equity        float64 // 当前权益 = 初始 + 累计已实现盈亏 - 手续费
	marginInUse   float64 // 占用保证金（开仓增加/平仓释放，用于破产保护）
	fundingIncome float64 // funding 模式: 全部资金费收入（USDT，报告用）
	equityCurve   []EquityPoint
	lastTS        int64         // 上一片时间
	btc           *btcState     // 自适应模式: BTC 市场状态
	dailyCount    map[int64]int // 单日开仓计数（DailyMax 限制用）

	// v6 运行状态
	fundRate         map[string]float64 // 最近已知资金费率（结算间保持）
	fundPrev         map[string]float64 // 上一结算费率（负转正判定）
	notionalInUse    float64            // 当前名义敞口总和（总敞口限制）
	dayStartEquity   float64            // 当日零点权益（日亏基准）
	dayPnl           float64            // 当日已实现盈亏（含手续费）
	dayBlocked       bool               // 日亏熔断: 当日停止开新仓
	lastDay          int64              // 上一个 UTC 日（跨天重置日熔断）
	lossStreak       int                // 连续亏损计数
	lossBlocked      bool               // 连亏熔断: 赢单前停止开新仓
	v6Gates          [8]int64           // v6 信号漏斗各阶段通过数（诊断用）
	v6Skip           [4]int64           // v6 开仓拦截原因统计: 0=熔断拦截 1=仓位/敞口/破产 2=已持仓去重 3=实际成交数
	fundingVetoCount int64              // S01 实验: 费率过热否决的信号数
	takerBlocked     int64              // S01 实验: 主动买占比过滤拦截信号数
	retraceTimeout   int64              // S01 实验: 回踩等待超时放弃数
	retraceFilled    int64              // S01 实验: 回踩确认后成交数
	rankBlocked      int64              // S01 实验: 排名过滤拦截信号数
	trendBlocked     int64              // S01 实验: 趋势因子拦截信号数
	sectorBlocked    int64              // S01 实验: 板块暴露上限拦截信号数
	rankOK           map[string]bool    // 当前时间片通过 24h 涨幅排名的币种
	gain24s          []gainRec          // 当前时间片全部流动性币的 24h 涨幅
	lastEntry        map[string]float64 // 追涨/回踩分类: 每币上一笔入场价
}

// gainRec 单个币种的 24h 涨幅（排名用）
type gainRec struct {
	sym string
	g   float64
}

// NewEngine 创建回测引擎
// 参数:
//   - cfg: 策略配置
//
// 返回:
//   - *Engine: 引擎实例
func NewEngine(cfg *StrategyConfig) *Engine {
	return &Engine{
		cfg:            cfg,
		states:         make(map[string]*symbolState),
		equity:         cfg.InitialEquity,
		fundRate:       make(map[string]float64),
		fundPrev:       make(map[string]float64),
		dayStartEquity: cfg.InitialEquity,
		lastDay:        -1,
		lastEntry:      make(map[string]float64),
	}
}

// bar 单根 5m K 线（回测所需字段）
type bar struct {
	ts       int64
	open     float64
	high     float64
	low      float64
	close    float64
	quoteVol float64
	vol      float64
	tbb      float64
}

// max24h 返回最近 24h 窗口内最高价（全扫描 O(288)）
// 参数:
//   - st: 币种状态
//
// 返回:
//   - float64: 窗口最高价
func max24h(st *symbolState) float64 {
	m := st.highs[0]
	for i := 1; i < WindowBars; i++ {
		if st.highs[i] > m {
			m = st.highs[i]
		}
	}
	return m
}

// min24h 返回最近 24h 窗口内最低价（全扫描 O(288)）
// 参数:
//   - st: 币种状态
//
// 返回:
//   - float64: 窗口最低价
func min24h(st *symbolState) float64 {
	m := st.lows[0]
	for i := 1; i < WindowBars; i++ {
		if st.lows[i] < m {
			m = st.lows[i]
		}
	}
	return m
}

// updateState 用一根新 K 线推进币种滚动状态，并返回是否已过 24h 预热期
// 参数:
//   - symbol: 币种
//   - b: 当前 K 线
//
// 返回:
//   - *symbolState: 更新后的状态
//   - bool: true 表示 24h 窗口已满（filled>=288）
func (e *Engine) updateState(symbol string, b *bar) (*symbolState, bool) {
	st, ok := e.states[symbol]
	if !ok {
		st = &symbolState{}
		e.states[symbol] = st
	}

	// 写入前读取将被覆盖的旧值（filled>=288 时为 288 根前的数据）
	oldClose := st.closes[st.idx]
	oldVol := st.quoteVols[st.idx]

	st.closes[st.idx] = b.close
	st.highs[st.idx] = b.high
	st.lows[st.idx] = b.low
	st.quoteVols[st.idx] = b.quoteVol
	st.vols[st.idx] = b.vol

	// 24h 累计成交额滚动维护
	if st.filled >= WindowBars {
		st.sumVol24 = st.sumVol24 - oldVol + b.quoteVol
	} else {
		st.sumVol24 += b.quoteVol
	}

	// 15m 周期开盘价（当前 K 线所在周期）
	periodTS := b.ts - b.ts%900000
	if !st.hasPeriod || st.periodTS != periodTS {
		st.periodTS = periodTS
		st.periodOpen = b.open
		st.periodVol = 0
		st.periodTBB = 0
		st.hasPeriod = true
	}
	st.periodVol += b.vol
	st.periodTBB += b.tbb

	st.idx = (st.idx + 1) % WindowBars
	st.filled++
	_ = oldClose // 供后续信号计算使用（已在 computeSignal 中按环形取）

	// 趋势模式 EMA 滚动更新（5m K 线粒度，快线 96≈8h / 慢线 288≈24h）
	if e.cfg.Mode == "trend" {
		if !st.emaInit {
			st.emaInit = true
			st.fastEma = b.close
			st.slowEma = b.close
		} else {
			st.prevFast, st.prevSlow = st.fastEma, st.slowEma
			kF := 2.0 / (float64(e.cfg.TrendFast) + 1)
			kS := 2.0 / (float64(e.cfg.TrendSlow) + 1)
			st.fastEma += (b.close - st.fastEma) * kF
			st.slowEma += (b.close - st.slowEma) * kS
		}
	}

	// 自适应模式: 回踩支撑 EMA 持续滚动（EMA 需连续计算，避免模式切换后均线失真）
	if e.cfg.Mode == "adaptive" && e.cfg.RBEMA > 0 {
		if !st.rbEmaInit {
			st.rbEma = b.close
			st.rbEmaInit = true
		} else {
			k := 2.0 / (float64(e.cfg.RBEMA) + 1)
			st.rbEma += (b.close - st.rbEma) * k
		}
	}

	// 趋势因子 EMA（momentum 实验）: EMA50（向上判定）或 EMA96（中期趋势）
	if e.cfg.TrendMode == 1 || e.cfg.TrendMode == 2 {
		span := 50
		if e.cfg.TrendMode == 2 {
			span = 96
		}
		if !st.trendEmaInit {
			st.trendEma = b.close
			st.trendEmaInit = true
		} else {
			k := 2.0 / (float64(span) + 1)
			st.prevTrendEma = st.trendEma
			st.trendEma += (b.close - st.trendEma) * k
		}
	}

	// v6 模式: 维护 RSI/ATR/布林带宽度历史（用于 L1/L2 判定）；
	// momentum 模式仅在启用 RSI 实验时维护 RSI/ATR（成本 O(1)，布林带宽度跳过）
	if e.cfg.Mode == "v6" || (e.cfg.Mode == "momentum" && e.cfg.RSIFilterEnabled) {
		e.updateV6Indicators(st, b)
	}

	return st, st.filled >= WindowBars
}

// prevClose 返回 n 根前的收盘价（环形取，需 filled>=n）
// 参数:
//   - st: 币种状态
//   - n: 前推 K 线数
//
// 返回:
//   - float64: n 根前的收盘价
func prevClose(st *symbolState, n int) float64 {
	j := (st.idx - n%WindowBars + WindowBars) % WindowBars
	return st.closes[j]
}

// avgPrevVol 返回前 n 根（不含当前）的平均成交额
// 参数:
//   - st: 币种状态
//   - n: 基准窗口根数
//   - ready: 是否满足样本数要求（返回 true 表示可比较）
//
// 返回:
//   - float64: 平均成交额
//   - bool: 基准是否就绪
func avgPrevVol(st *symbolState, n int) (float64, bool) {
	if st.filled < n+1 {
		return 0, false
	}
	sum := 0.0
	for i := 1; i <= n; i++ {
		j := (st.idx - i%WindowBars + WindowBars*2) % WindowBars
		sum += st.quoteVols[j]
	}
	if sum <= 0 {
		return 0, false
	}
	return sum / float64(n), true
}

// volumeSurge 计算当前片相对前 n 根平均的放量倍数
// 参数:
//   - st: 币种状态
//   - b: 当前 K 线
//   - n: 基准窗口根数
//
// 返回:
//   - float64: 放量倍数（基准未就绪返回 0）
func volumeSurge(st *symbolState, b *bar, n int) float64 {
	avg, ok := avgPrevVol(st, n)
	if !ok {
		return 0
	}
	return b.quoteVol / avg
}

// updateBTC 用一根 BTC K 线推进市场状态（EMA + ATR14）
// 参数:
//   - b: BTC 当前 K 线
func (e *Engine) updateBTC(b *bar) {
	if e.btc == nil {
		e.btc = &btcState{}
	}
	st := e.btc
	// EMA
	if !st.emaInit {
		st.ema = b.close
		st.emaInit = true
	} else {
		k := 2.0 / (float64(e.cfg.AdaptBTCEMA) + 1)
		st.ema += (b.close - st.ema) * k
	}
	// TR（首片无前收，TR=0）
	tr := 0.0
	if st.trCnt > 0 {
		tr = math.Max(b.high-b.low, math.Max(math.Abs(b.high-st.close), math.Abs(b.low-st.close)))
	}
	if st.trCnt < 14 {
		st.trCnt++
	}
	st.tr[st.trIdx] = tr
	st.trIdx = (st.trIdx + 1) % 14
	// ATR = TR 简单平均
	sum := 0.0
	for i := 0; i < st.trCnt; i++ {
		sum += st.tr[i]
	}
	st.atr = sum / float64(st.trCnt)
	st.close = b.close

	// 24h 涨幅环形（btc24h 市场过滤用）
	st.closes[st.cIdx] = b.close
	st.cIdx = (st.cIdx + 1) % WindowBars
	if st.cCnt < WindowBars {
		st.cCnt++
	}
	if st.cCnt >= WindowBars {
		old := st.closes[st.cIdx] // 当前写入槽即 288 根前的价格
		if old > 0 {
			st.chg24 = (b.close - old) / old * 100
			st.chg24Ready = true
		}
	}
}

// btcMode 判定当前自适应模式: 回踩(pullback) / 追涨(chase) / 做空(short)
// 规则: BTC>EMA50 且 ATR%<=阈值 → 回踩; BTC>EMA50 且 ATR%>阈值 → 追涨; BTC<=EMA50 → 做空
// 返回:
//   - string: "chase" / "pullback" / "short"
func (e *Engine) btcMode() string {
	if e.btc == nil || !e.btc.emaInit || e.btc.trCnt < 5 {
		return "chase" // 数据不足默认追涨
	}
	bull := e.btc.close > e.btc.ema
	atrPct := e.btc.atr / e.btc.close * 100
	if bull && atrPct <= e.cfg.AdaptATRTh*100 && !e.cfg.AdaptDisablePullback {
		return "pullback"
	}
	if bull && !e.cfg.AdaptDisableChase {
		return "chase"
	}
	return "short"
}

// regimeOK 市场状态过滤（S01 单因子实验，默认关闭）
// 支持:
//   - btc24h: BTC 24h 涨幅 >= RegimeParam(%) 才允许开仓
//   - btcma:  BTC 收盘 >= EMA（牛熊门控）才允许开仓
//   - breadth: 全市场 24h 上涨币占比 >= RegimeParam(0~1) 才允许开仓
func (e *Engine) regimeOK() bool {
	switch e.cfg.Regime {
	case "btc24h":
		return e.btc != nil && e.btc.chg24Ready && e.btc.chg24 >= e.cfg.RegimeParam
	case "btcma":
		return e.btc != nil && e.btc.emaInit && e.btc.close >= e.btc.ema
	case "breadth":
		up, valid := 0, 0
		for _, st := range e.states {
			if st.filled < WindowBars {
				continue
			}
			old := prevClose(st, WindowBars)
			if old <= 0 {
				continue
			}
			valid++
			if st.closes[(st.idx-1+WindowBars)%WindowBars] > old {
				up++
			}
		}
		if valid == 0 {
			return false
		}
		return float64(up)/float64(valid) >= e.cfg.RegimeParam
	}
	return true
}

// signalPullback 回踩信号: 24h 涨幅达标 + 价格回踩 EMA 支撑 + 缩量 + 连续企稳
// 参数:
//   - st: 币种状态
//   - b: 当前 K 线
//
// 返回:
//   - string: "LONG" 或 ""（无信号）
func (e *Engine) signalPullback(st *symbolState, b *bar) string {
	cfg := e.cfg
	// 触达/企稳跟踪: 触及 EMA 支撑 → 重置企稳计数; 收盘站上 EMA → 计数+1; 跌破 → 清零
	if b.low <= st.rbEma*(1+0.005) {
		st.rbTouched = true
		st.rbStableCnt = 0
	} else if b.close >= st.rbEma {
		st.rbStableCnt++
	} else {
		st.rbStableCnt = 0
	}
	if !st.rbTouched {
		return ""
	}
	// 24h 涨幅（上升趋势中的回踩才做多）
	oldClose := prevClose(st, WindowBars)
	if oldClose <= 0 {
		return ""
	}
	gain24 := (b.close - oldClose) / oldClose * 100
	if gain24 < cfg.RBPullGain {
		return ""
	}
	// 企稳根数与缩量
	if st.rbStableCnt < cfg.RBStable {
		return ""
	}
	surge := volumeSurge(st, b, cfg.SurgeLookback)
	if surge <= 0 || surge >= cfg.RBShrink {
		return ""
	}
	return "LONG"
}

// computeSignal 评估当前片是否产生开仓信号
// 条件（全部满足才开仓）:
//  1. 24h 窗口已满（预热期跳过）
//  2. 15m K 线实体涨幅 >= MinGainPct
//  3. 24h 涨幅 >= Min24hGainPct
//  4. 24h 成交额 >= MinQuoteVolume
//  5. 放量倍数 >= VolumeSurgeThreshold
//  6. 山顶过滤: 距 24h 极值回撤 <= MaxPullbackPct
//  7. 冷却期已过
//
// 参数:
//   - st: 币种状态
//   - b: 当前 K 线
//   - ready24: 24h 窗口是否已满
//
// 返回:
//   - string: 信号方向 "LONG"/"SHORT"，无信号返回 ""
func (e *Engine) computeSignal(st *symbolState, b *bar, ready24 bool) (string, string) {
	cfg := e.cfg
	if !ready24 || !st.hasPeriod || st.periodOpen <= 0 {
		return "", ""
	}

	// 冷却检查（分原因: 移动止盈平仓后可用更短冷却，默认-1=统一 CooldownMs）
	cd := cfg.CooldownMs
	if st.lastCloseReason == "TRAILING_STOP" && cfg.CooldownAfterTrailingMin >= 0 {
		cd = int64(cfg.CooldownAfterTrailingMin) * 60 * 1000
	}
	if st.lastClose > 0 && b.ts-st.lastClose < cd {
		return "", ""
	}

	// 24h 成交额下限（三种范式共用流动性过滤）
	if st.sumVol24 < cfg.MinQuoteVolume {
		return "", ""
	}

	oldClose := prevClose(st, WindowBars)
	if oldClose <= 0 {
		return "", ""
	}

	switch cfg.Mode {
	case "funding":
		// 资金费率套利: 信号由 processFunding 在资金费结算片直接产生，此处不产生 K 线信号
		return "", ""

	case "v6":
		// v6 信号链由 OnBar 专用分支（v6Signal）产生，此处不产生信号
		return "", ""

	case "mr":
		// 均值回归: 单根 5m K 线实体暴跌后赌反弹
		// LONG: 5m 实体跌 >= MRDropPct（恐慌）且 24h 仍上涨（上行趋势中的回调）、
		//       距 24h 最高点回撤处于 [MRMinDrawdownPct, MRMaxDrawdownPct]（已回调但未崩塌）
		body := (b.close - b.open) / b.open * 100
		gain24 := (b.close - oldClose) / oldClose * 100
		h, l := max24h(st), min24h(st)
		if body <= -cfg.MRDropPct*100 && gain24 > 0 && h > 0 && !cfg.OnlyShort {
			dd := (h - b.close) / h * 100
			if dd >= cfg.MRMinDrawdownPct*100 && dd <= cfg.MRMaxDrawdownPct*100 {
				return "LONG", ""
			}
		}
		if cfg.EnableShort && body >= cfg.MRDropPct*100 && gain24 < 0 && l > 0 {
			up := (b.close - l) / b.close * 100
			if up >= cfg.MRMinDrawdownPct*100 && up <= cfg.MRMaxDrawdownPct*100 {
				return "SHORT", ""
			}
		}
		return "", ""

	case "trend":
		// 趋势跟随: 快线 EMA 上穿慢线做多 / 下穿做空（仅交叉当根产生信号）
		if !st.emaInit || st.filled < cfg.TrendSlow {
			return "", ""
		}
		crossUp := st.prevFast <= st.prevSlow && st.fastEma > st.slowEma
		crossDn := st.prevFast >= st.prevSlow && st.fastEma < st.slowEma
		if crossUp && !cfg.OnlyShort {
			return "LONG", ""
		}
		if cfg.EnableShort && crossDn {
			return "SHORT", ""
		}
		return "", ""

	case "adaptive":
		// 自适应融合: 按 BTC 市场状态（EMA 牛熊 + ATR 波动）动态切换信号模式
		mode := e.btcMode()
		switch mode {
		case "short":
			// 做空模式（BTC<=EMA）: 反向动量信号（15m 实体跌 + 24h 跌 + 放量 + 距24h低点回撤过滤）
			if !cfg.EnableShort {
				return "", ""
			}
			if cfg.ClosedBarConfirm && (b.ts+300000)%900000 != 0 {
				return "", ""
			}
			gain15 := (b.close - st.periodOpen) / st.periodOpen * 100
			gain24 := (b.close - oldClose) / oldClose * 100
			if gain15 > -cfg.MinGainPct || gain24 > -cfg.Min24hGainPct {
				return "", ""
			}
			if volumeSurge(st, b, cfg.SurgeLookback) < cfg.VolumeSurgeThreshold {
				return "", ""
			}
			l := min24h(st)
			if l > 0 && (b.close-l)/b.close*100 > cfg.MaxPullbackPct {
				return "", ""
			}
			return "SHORT", "short"

		case "pullback":
			// 回踩模式（BTC>EMA 且低波动）: 回踩 EMA 支撑企稳后做多
			if side := e.signalPullback(st, b); side == "LONG" {
				return "LONG", "pullback"
			}
			return "", ""

		default: // chase 追涨模式（BTC>EMA 且波动放大）: 原 momentum 做多逻辑
			if cfg.ClosedBarConfirm && (b.ts+300000)%900000 != 0 {
				return "", ""
			}
			gain15 := (b.close - st.periodOpen) / st.periodOpen * 100
			gain24 := (b.close - oldClose) / oldClose * 100
			if gain15 < cfg.MinGainPct || gain24 < cfg.Min24hGainPct {
				return "", ""
			}
			if volumeSurge(st, b, cfg.SurgeLookback) < cfg.VolumeSurgeThreshold {
				return "", ""
			}
			h := max24h(st)
			if h > 0 && (h-b.close)/h*100 > cfg.MaxPullbackPct {
				return "", ""
			}
			return "LONG", "chase"
		}

	default: // momentum 追涨（原逻辑）
		// 已收盘确认模式: 仅评估每个 15m 周期的最后一根 5m K 线
		if cfg.ClosedBarConfirm && (b.ts+300000)%900000 != 0 {
			return "", ""
		}
		gain15 := (b.close - st.periodOpen) / st.periodOpen * 100
		gain24 := (b.close - oldClose) / oldClose * 100
		side := ""
		if gain15 >= cfg.MinGainPct && gain24 >= cfg.Min24hGainPct {
			side = "LONG"
		} else if cfg.EnableShort && gain15 <= -cfg.MinGainPct && gain24 <= -cfg.Min24hGainPct {
			side = "SHORT"
		}
		if side == "" || (cfg.OnlyShort && side == "LONG") {
			return "", ""
		}
		// 放量确认
		if volumeSurge(st, b, cfg.SurgeLookback) < cfg.VolumeSurgeThreshold {
			return "", ""
		}
		// 山顶过滤器
		if side == "LONG" {
			h := max24h(st)
			if h > 0 && (h-b.close)/h*100 > cfg.MaxPullbackPct {
				return "", ""
			}
		} else {
			l := min24h(st)
			if l > 0 && (b.close-l)/b.close*100 > cfg.MaxPullbackPct {
				return "", ""
			}
		}
		// 最低币价过滤：低价币点差宽、滑点大（实盘 JCT/CYS 极端滑点根因）
		if cfg.MinPrice > 0 && b.close < cfg.MinPrice {
			return "", ""
		}
		// 主动买占比（S01 实验）：当前 15m 窗口主动买量/总成交量 >= 门槛（0=关闭）
		if cfg.MinTakerBuyPct > 0 {
			if st.periodVol <= 0 || st.periodTBB/st.periodVol*100 < cfg.MinTakerBuyPct {
				e.takerBlocked++
				return "", ""
			}
		}
		return side, ""
	}
}

// fillPending 用当前片开盘价成交所有待开仓单
// 参数:
//   - bars: 当前片全部币种 K 线
//   - ts: 当前片时间戳
func (e *Engine) fillPending(bars map[string]*bar, ts int64) {
	still := e.pending[:0]
	for _, p := range e.pending {
		b, ok := bars[p.Symbol]
		if !ok {
			// 该币本片无数据，且未超时（3 小时）则保留待下一片
			if ts-p.EntryTS <= 3*60*60*1000 {
				still = append(still, p)
			}
			continue
		}
		// S01 回踩入场实验: 信号后不立即成交，等回踩到位 + 5m 阳线收复回踩 bar 高点
		if p.RetracePct > 0 {
			if !p.RetraceConfirmed {
				p.RetraceBars++
				if !p.RetraceSeenDip {
					if b.low <= p.RetraceRef*(1-p.RetracePct/100) {
						p.RetraceSeenDip = true
						p.RetraceDipHigh = b.high
					}
				} else if b.close >= p.RetraceDipHigh {
					p.RetraceConfirmed = true
				}
				if !p.RetraceConfirmed {
					if p.RetraceBars > p.RetraceMax {
						e.retraceTimeout++ // 超时未确认，放弃
						continue
					}
					still = append(still, p)
					continue
				}
				// 本片收盘才确认，必须等下一片开盘成交（防未来函数）
				still = append(still, p)
				continue
			}
			e.retraceFilled++
		}
		p.EntryTS = ts
		p.EntryPrice = b.open
		// v6: 按分级滑点恶化入场价（做多加价 / 做空减价）
		if p.Slippage > 0 {
			if p.Side == "SHORT" {
				p.EntryPrice = b.open * (1 - p.Slippage)
			} else {
				p.EntryPrice = b.open * (1 + p.Slippage)
			}
		}
		// 追涨/回踩分类: 入场价 vs 该币上一笔入场价（同一引擎内按时间序）
		if prev, ok := e.lastEntry[p.Symbol]; ok {
			if p.EntryPrice > prev {
				p.ChaseType = "chase"
			} else if p.EntryPrice < prev {
				p.ChaseType = "pullback"
			} else {
				p.ChaseType = "flat"
			}
		} else {
			p.ChaseType = "first"
		}
		if p.Notional <= 0 {
			p.Notional = e.cfg.PositionMarginUSDT * e.cfg.Leverage
		}
		p.Amount = p.Notional / p.EntryPrice
		if p.Amount <= 0 {
			continue
		}
		p.ExtremePrice = b.open
		p.Pending = false
		// 破产保护: 权益不足以支付该仓保证金时放弃成交（账户已接近爆仓）
		if p.Margin <= 0 {
			p.Margin = e.cfg.PositionMarginUSDT
		}
		if e.equity-e.marginInUse < p.Margin {
			continue
		}
		// 开仓手续费（按名义价值 taker 费率）
		e.equity -= p.Amount * p.EntryPrice * e.cfg.FeeRate
		e.marginInUse += p.Margin
		e.notionalInUse += p.Amount * p.EntryPrice
		// v6: 固化入场时 ATR 与资金费率（波动率衰减 / 费率反转退出基准）
		if st := e.states[p.Symbol]; st != nil {
			p.ATRPeak = st.atr
			p.EntryATR = st.atr
		}
		if v, ok := e.fundRate[p.Symbol]; ok {
			p.EntryFunding = v
		}
		e.lastEntry[p.Symbol] = p.EntryPrice
		if e.cfg.Mode == "v6" {
			e.v6Skip[3]++
		}
		e.positions = append(e.positions, p)
	}
	e.pending = still
}

// openPositions 对全部候选按成交额降序取 TopN，未持仓且未待成交的依次开仓直到仓位满
// 参数:
//   - candidates: 候选列表（symbol + 当日成交额用于排序）
//   - now: 当前片时间
func (e *Engine) openPositions(candidates []candidate, now int64) {
	held := map[string]bool{}
	for _, p := range e.positions {
		held[p.Symbol] = true
	}
	for _, p := range e.pending {
		held[p.Symbol] = true
	}

	// 单日最大开仓数限制（DailyMax>0 时当天开满即不再开仓）
	if e.cfg.DailyMax > 0 {
		day := now / 86400000
		if e.dailyCount == nil {
			e.dailyCount = make(map[int64]int)
		}
		if e.dailyCount[day] >= e.cfg.DailyMax {
			return
		}
	}

	// S01 仓位倾斜实验: 按信号强度因子 z 标准化调整每仓保证金（默认均仓）
	mult := make([]float64, len(candidates))
	if e.cfg.SizeMode == 5 {
		// 按币价减仓（脚毛币滑点大）: <PriceSizeTh 半仓，<0.2U 七五折，其余均仓
		for i, c := range candidates {
			px := c.ref
			if px <= 0 {
				if st := e.states[c.symbol]; st != nil {
					px = ringAt(&st.closes, st.idx, 0)
				}
			}
			m := 1.0
			switch {
			case px > 0 && px < e.cfg.PriceSizeTh:
				m = 0.5
			case px >= e.cfg.PriceSizeTh && px < 0.2:
				m = 0.75
			}
			mult[i] = math.Max(e.cfg.SizeMin, math.Min(e.cfg.SizeMax, m))
		}
	} else if e.cfg.SizeMode > 0 {
		vals := make([]float64, len(candidates))
		for i, c := range candidates {
			switch e.cfg.SizeMode {
			case 1:
				vals[i] = c.gain15
			case 2:
				vals[i] = c.surge
			case 3:
				vals[i] = c.taker
			case 4:
				vals[i] = c.gain15 + c.surge*3 + c.taker*0.2
			default:
				vals[i] = 0
			}
		}
		mean, std := meanStd(vals)
		for i := range candidates {
			m := 1.0
			if std > 1e-9 {
				m = 1 + e.cfg.SizeTilt*(vals[i]-mean)/std
			}
			mult[i] = math.Max(e.cfg.SizeMin, math.Min(e.cfg.SizeMax, m))
		}
	} else {
		for i := range candidates {
			mult[i] = 1
		}
	}

	for i, c := range candidates {
		if len(e.positions)+len(e.pending) >= e.cfg.MaxOpenPositions {
			break
		}
		// S01 实验: 板块暴露上限（同板块同时持仓 < SectorMax 才可开新币；OTHERS/未知不设限，加仓不受限）
		if e.cfg.SectorMax > 0 && !held[c.symbol] {
			if sec := symbolSector[c.symbol]; sec != "" && sec != "OTHERS" {
				cnt := 0
				for _, p := range e.positions {
					if symbolSector[p.Symbol] == sec {
						cnt++
					}
				}
				for _, p := range e.pending {
					if symbolSector[p.Symbol] == sec {
						cnt++
					}
				}
				if cnt >= e.cfg.SectorMax {
					e.sectorBlocked++
					continue
				}
			}
		}
		if held[c.symbol] {
			if e.cfg.Mode == "v6" {
				e.v6Skip[2]++
				continue
			}
			// 追加仓位（与实盘 EnableAddOn 语义一致）: 同币移动止盈已激活且再次命中信号
			if !e.canAddOn(c) {
				continue
			}
		}
		// 破产保护: 可用权益（权益-占用保证金）不足以开一仓则停止开仓
		// （v6 动态仓位由 v6Sizing 自行校验，不在此用固定保证金截断）
		if e.cfg.Mode != "v6" && e.equity-e.marginInUse < e.cfg.PositionMarginUSDT {
			break
		}
		p := &Position{
			Symbol:  c.symbol,
			Side:    c.side,
			EntryTS: now,
			Pending: true,
		}
		// 按信号模式固化退出参数（adaptive: chase/pullback/short 各有独立风控；非 adaptive 用配置默认）
		switch c.mode {
		case "v6":
			// v6 风控熔断: 日亏 / 连亏达标后不再开新仓
			if e.dayBlocked || e.lossBlocked {
				e.v6Skip[0]++
				continue
			}
			entryPrice := 0.0
			if st := e.states[c.symbol]; st != nil {
				entryPrice = ringAt(&st.closes, st.idx, 0)
			}
			if entryPrice <= 0 {
				continue
			}
			notional, margin, amount, ok := e.v6Sizing(c.symbol, c.score, c.tier, entryPrice)
			if !ok {
				e.v6Skip[1]++
				continue
			}
			p.Notional = notional
			p.Margin = margin
			p.Amount = amount
			p.Slippage = e.v6Slippage(c.tier)
			p.Score = c.score
			p.Tier = c.tier
			p.SLPct, p.TPPct, p.ActPct, p.CbPct, p.HoldBars = e.cfg.StopLossPct, 0, e.cfg.TrailingActivation, e.cfg.TrailingCallback, e.cfg.MaxHoldBars
		case "pullback":
			p.SLPct, p.TPPct, p.ActPct, p.CbPct, p.HoldBars = e.cfg.RBSL, e.cfg.RBTP, e.cfg.RBAct, e.cfg.RBCb, e.cfg.RBHold
		case "short":
			p.SLPct, p.TPPct, p.ActPct, p.CbPct, p.HoldBars = e.cfg.SSL, e.cfg.STP, e.cfg.SAct, e.cfg.SCb, e.cfg.SHold
		default: // chase 或非 adaptive
			p.SLPct, p.TPPct, p.ActPct, p.CbPct, p.HoldBars = e.cfg.StopLossPct, e.cfg.TakeProfitPct, e.cfg.TrailingActivation, e.cfg.TrailingCallback, e.cfg.MaxHoldBars
			p.Margin = e.cfg.PositionMarginUSDT * mult[i]
			p.Notional = p.Margin * e.cfg.Leverage
		}
		// S01 回踩入场实验: momentum 模式信号后等待回踩+收复才成交
		if e.cfg.Mode == "momentum" && e.cfg.RetracePct > 0 && c.ref > 0 {
			p.RetracePct = e.cfg.RetracePct
			p.RetraceRef = c.ref
			p.RetraceMax = e.cfg.RetraceMaxBars
		}
		p.Mode = c.mode
		e.pending = append(e.pending, p)
		if e.cfg.DailyMax > 0 {
			e.dailyCount[now/86400000]++
		}
		held[c.symbol] = true
	}
}

// meanStd 计算切片均值与标准差（仓位倾斜 z 标准化用）
func meanStd(vals []float64) (float64, float64) {
	if len(vals) == 0 {
		return 0, 0
	}
	sum := 0.0
	for _, v := range vals {
		sum += v
	}
	mean := sum / float64(len(vals))
	var s float64
	for _, v := range vals {
		d := v - mean
		s += d * d
	}
	return mean, math.Sqrt(s / float64(len(vals)))
}

// takerRatio 返回当前 15m 窗口主动买占比（%）
func (e *Engine) takerRatio(symbol string) float64 {
	st := e.states[symbol]
	if st == nil || st.periodVol <= 0 {
		return 0
	}
	return st.periodTBB / st.periodVol * 100
}

// computeRank 按 24h 涨幅对当前片流动性币排序，生成排名通过集合（前 N% 或前 M 名）
func (e *Engine) computeRank() {
	e.rankOK = make(map[string]bool, len(e.gain24s))
	n := len(e.gain24s)
	if n == 0 {
		return
	}
	sort.Slice(e.gain24s, func(i, j int) bool { return e.gain24s[i].g > e.gain24s[j].g })
	limit := 0
	if e.cfg.RankMode == 1 {
		limit = int(math.Ceil(float64(n) * e.cfg.RankParam / 100))
	} else if e.cfg.RankMode == 2 {
		limit = int(e.cfg.RankParam)
	}
	if limit < 1 {
		limit = 1
	}
	if limit > n {
		limit = n
	}
	for i := 0; i < limit; i++ {
		e.rankOK[e.gain24s[i].sym] = true
	}
}

// vwap4h 返回最近 48 根 5m（4 小时）的成交量加权均价
func (e *Engine) vwap4h(st *symbolState) float64 {
	if st.filled < 48 {
		return 0
	}
	ci := (st.idx - 1 + WindowBars) % WindowBars
	var qv, v float64
	for i := 0; i < 48; i++ {
		j := (ci - i + WindowBars*2) % WindowBars
		qv += st.quoteVols[j]
		v += st.vols[j]
	}
	if v <= 0 {
		return 0
	}
	return qv / v
}

// trendOK 趋势因子判定（入场时刻资产是否处于上升趋势，返回 true 表示放行）
func (e *Engine) trendOK(st *symbolState, b *bar) bool {
	switch e.cfg.TrendMode {
	case 1: // EMA50 向上且现价在其上方
		if !st.trendEmaInit || st.trendEma <= 0 {
			return true // 预热期放行，避免误伤
		}
		return b.close > st.trendEma && st.trendEma > st.prevTrendEma
	case 2: // 现价 > EMA96（≈8h 中期趋势）
		if !st.trendEmaInit || st.trendEma <= 0 {
			return true
		}
		return b.close > st.trendEma
	case 3: // 4h 涨幅 > 0
		old := prevClose(st, 48)
		return old > 0 && b.close > old
	case 4: // 4h 涨幅 > 2%
		old := prevClose(st, 48)
		return old > 0 && (b.close-old)/old > 0.02
	case 5: // 现价 > 4h VWAP
		vwap := e.vwap4h(st)
		return vwap > 0 && b.close > vwap
	}
	return true
}

// sortCandidates 将候选按成交额降序排序（放量大的优先）
// 参数:
//   - cands: 候选切片（原地排序）
func sortCandidates(cands []candidate) {
	sort.Slice(cands, func(i, j int) bool {
		return cands[i].volume > cands[j].volume
	})
}

// canAddOn 判断是否允许追加仓位（与实盘 EnableAddOn 语义一致）:
// 开启追加 + 同币存在移动止盈已激活的持仓 + 同币持仓数未达 1+MaxAddOnsPerSymbol + 方向一致。
func (e *Engine) canAddOn(c candidate) bool {
	if !e.cfg.EnableAddOn || e.cfg.MaxAddOnsPerSymbol <= 0 {
		return false
	}
	count := 0
	trailing := false
	// 追单门槛：AddOnActPct>0 时要求同币持仓极值达到 首仓入场价×(1±AddOnActPct)
	//（多头用最高价、空头用最低价），过滤"小冲高即追顶"；=0 时沿用移动止盈激活状态。
	threshold := e.cfg.AddOnActPct
	for _, p := range e.positions {
		if p.Symbol != c.symbol {
			continue
		}
		count++
		if p.Side == c.side {
			if threshold > 0 {
				if (p.Side == "LONG" && p.ExtremePrice >= p.EntryPrice*(1+threshold)) ||
					(p.Side == "SHORT" && p.ExtremePrice <= p.EntryPrice*(1-threshold)) {
					trailing = true
				}
			} else if p.TrailingActive {
				trailing = true
			}
		}
	}
	for _, p := range e.pending {
		if p.Symbol == c.symbol {
			count++
		}
	}
	return trailing && count < 1+e.cfg.MaxAddOnsPerSymbol
}

// candidate 一个开仓候选
type candidate struct {
	symbol string
	side   string
	volume float64 // 用于排序的成交额
	mode   string  // 自适应模式: chase/pullback/short（非 adaptive 为空）
	score  float64 // v6: L3 加权总分
	tier   string  // v6: 币种分级 big/mid/small
	ref    float64 // 信号 bar 收盘价（回踩实验基准）
	gain15 float64 // 15m 实体涨幅%（仓位倾斜用）
	surge  float64 // 放量倍数（仓位倾斜用）
	taker  float64 // 15m 主动买占比%（仓位倾斜用）
}

// closePosition 平仓结算一笔持仓（含资金费收入并入 PnL）
// 保守口径: 收益 = 价格盈亏 + 持有期间收取的资金费 - 平仓手续费；同时释放占用保证金。
// 参数:
//   - p: 持仓
//   - b: 当前片 K 线（提供成交时间戳）
//   - exitPx: 平仓价格
//   - reason: 平仓原因
func (e *Engine) closePosition(p *Position, b *bar, exitPx float64, reason string) {
	var pnl float64
	if p.Side == "LONG" {
		pnl = (exitPx - p.EntryPrice) * p.Amount
	} else {
		pnl = (p.EntryPrice - exitPx) * p.Amount
	}
	pnl += p.FundingCollected
	e.equity += pnl
	e.equity -= exitPx * p.Amount * e.cfg.FeeRate // 平仓手续费（按成交名义价值）
	e.marginInUse -= p.Margin                     // 释放保证金
	e.notionalInUse -= p.Amount * p.EntryPrice    // 释放名义敞口
	if e.cfg.Mode == "v6" {
		// 日亏 / 连亏熔断状态更新（净盈亏 = 价格盈亏 - 平仓手续费）
		net := pnl - exitPx*p.Amount*e.cfg.FeeRate
		e.dayPnl += net
		if net < 0 {
			e.lossStreak++
		} else {
			e.lossStreak = 0
		}
		if e.lossStreak >= e.cfg.MaxConsecutiveLosses && !e.lossBlocked {
			e.lossBlocked = true
			fmt.Printf("[V6] 连续亏损 %d 笔，停止开新仓（赢单重置）\n", e.lossStreak)
		}
		if e.dayStartEquity > 0 && e.dayPnl <= -e.cfg.DailyLossPct*e.dayStartEquity && !e.dayBlocked {
			e.dayBlocked = true
			fmt.Printf("[V6] 当日净亏损 %.2fU 达 %.1f%% 限制，当日停止开新仓\n",
				e.dayPnl, e.cfg.DailyLossPct*100)
		}
	}
	e.trades = append(e.trades, &Trade{
		Symbol:    p.Symbol,
		Side:      p.Side,
		EntryTS:   p.EntryTS,
		EntryPx:   p.EntryPrice,
		ExitTS:    b.ts,
		ExitPx:    exitPx,
		Amount:    p.Amount,
		PnL:       pnl,
		PnLPct:    pnl / (p.EntryPrice * p.Amount) * 100,
		Reason:    reason,
		HeldBars:  int((b.ts - p.EntryTS) / 300000),
		ChaseType: p.ChaseType,
	})
	if st, ok := e.states[p.Symbol]; ok {
		st.lastClose = b.ts
		st.lastCloseReason = reason
	}
}

// processFunding 处理当前片的资金费率事件（仅 funding 范式生效）
// 处理顺序:
//  1. 持仓结算: 本片有该币资金费结算点时收取资金费（rate × 标记价 × 数量），
//     并评估费率回归平仓（FUND_EXIT）与最大持有周期平仓（FUND_TIME）
//  2. 开仓候选: 费率极端（<= -FundTh 做多收取 / >= FundTh 做空收取）且通过
//     24h 预热、流动性、冷却过滤时产生候选（按 24h 成交额排序）
//
// 参数:
//   - bars: 当前片全部币种 K 线
//   - fundings: 当前片资金费结算点（symbol -> 费率记录）
//   - ts: 当前片时间戳
//
// 返回:
//   - []candidate: 开仓候选列表
func (e *Engine) processFunding(bars map[string]*bar, fundings map[string]fundingPoint, ts int64) []candidate {
	if e.cfg.Mode != "funding" {
		return nil
	}

	// 1. 持仓: 收取资金费 + 费率回归/超时平仓
	kept := e.positions[:0]
	for _, p := range e.positions {
		fp, ok := fundings[p.Symbol]
		if !ok {
			kept = append(kept, p)
			continue
		}
		b, ok := bars[p.Symbol]
		if !ok {
			kept = append(kept, p)
			continue
		}
		// 收取资金费（按结算时标记价 × 持仓数量；新版费率文件无 markPrice 列，回退用该片收盘价）
		// 币安惯例: 正费率 = 多头付空头 → 多头收取 = -rate×名义值，空头收取 = +rate×名义值
		px := fp.markPrice
		if px <= 0 {
			px = b.close
		}
		paid := fp.rate * px * p.Amount
		if p.Side == "LONG" {
			paid = -paid
		}
		p.FundingCollected += paid
		e.fundingIncome += paid
		p.FundIntervals++

		exitPx, reason := 0.0, ""
		if p.Side == "LONG" && fp.rate >= -e.cfg.FundExitTh {
			exitPx, reason = b.close, "FUND_EXIT" // 空头不再付高额费率，回归即收工
		} else if p.Side == "SHORT" && fp.rate <= e.cfg.FundExitTh {
			exitPx, reason = b.close, "FUND_EXIT"
		} else if p.FundIntervals >= e.cfg.FundMaxHold {
			exitPx, reason = b.close, "FUND_TIME" // 费率未回归但已到最长持有周期
		}
		if reason != "" {
			e.closePosition(p, b, exitPx, reason)
			continue
		}
		kept = append(kept, p)
	}
	e.positions = kept

	// 2. 未持仓: 费率极端时产生开仓候选
	held := map[string]bool{}
	for _, p := range e.positions {
		held[p.Symbol] = true
	}
	for _, p := range e.pending {
		held[p.Symbol] = true
	}
	var cands []candidate
	for sym, fp := range fundings {
		if held[sym] {
			continue
		}
		st, ok := e.states[sym]
		if !ok || st.filled < WindowBars { // 24h 预热期跳过
			continue
		}
		if st.sumVol24 < e.cfg.MinQuoteVolume { // 流动性过滤
			continue
		}
		if st.lastClose > 0 && ts-st.lastClose < e.cfg.CooldownMs { // 冷却
			continue
		}
		side := ""
		switch {
		case fp.rate <= -e.cfg.FundTh && !e.cfg.OnlyShort:
			side = "LONG" // 深度负费率: 空头付费，做多收取
		case e.cfg.EnableShort && fp.rate >= e.cfg.FundTh:
			side = "SHORT" // 深度正费率: 多头付费，做空收取
		}
		if side == "" {
			continue
		}
		cands = append(cands, candidate{symbol: sym, side: side, volume: st.sumVol24})
	}
	return cands
}

// monitorPositions 用当前片 high/low 监控持仓，触发止损/跟踪止盈/信号反转则平仓
// 三种信号范式使用各自的退出规则:
//   - momentum: 固定止损 + 跟踪止盈（原逻辑）
//   - mr: 反弹止盈(MR_TP) / 反弹失败止损(MR_SL) / 超时平仓(MR_TIME)
//   - trend: 固定止损 + 跟踪止盈 + 反向 EMA 交叉(TREND_EXIT)
//
// 保守撮合: 同片内止损与其他条件同时触达时按止损处理；跳空时按 open 成交。
// 参数:
//   - bars: 当前片全部币种 K 线（symbol -> bar）
func (e *Engine) monitorPositions(bars map[string]*bar) {
	kept := e.positions[:0]
	for _, p := range e.positions {
		b, ok := bars[p.Symbol]
		if !ok {
			kept = append(kept, p)
			continue
		}
		var exitPx float64
		reason := ""
		held := int((b.ts - p.EntryTS) / 300000)

		switch e.cfg.Mode {
		case "funding":
			// 资金费率套利: 仅价格止损（费率退出已在 processFunding 处理）
			if p.Side == "LONG" {
				stop := p.EntryPrice * (1 - e.cfg.FundSLPct)
				if b.low <= stop {
					exitPx = min2(b.open, stop)
					reason = "FUND_SL"
				}
			} else { // SHORT
				stop := p.EntryPrice * (1 + e.cfg.FundSLPct)
				if b.high >= stop {
					exitPx = max2(b.open, stop)
					reason = "FUND_SL"
				}
			}

		case "v6":
			// v6 退出层（纯多头）: 硬止损 / 超时 / ATR 衰减 / 费率反转 / 移动止盈
			st := e.states[p.Symbol]
			if st != nil && st.atr > p.ATRPeak {
				p.ATRPeak = st.atr // 波动率衰减基准: 开仓后 ATR14 峰值
			}
			if p.Side == "LONG" {
				stop := p.EntryPrice * (1 - p.SLPct)
				switch {
				case b.low <= stop: // 硬止损（片内触发按止损价成交，含滑点）
					exitPx = min2(b.open, stop) * (1 - p.Slippage)
					reason = "STOP_LOSS"
				case p.HoldBars > 0 && held >= p.HoldBars: // 120 分钟超时
					exitPx = b.close * (1 - p.Slippage)
					reason = "MAX_HOLD"
				case st != nil && st.atr > 0 && p.ATRPeak > 0 &&
					st.atr <= p.ATRPeak*e.cfg.ATRDecayPct && held >= e.cfg.ATRDecayMinHoldBars:
					exitPx = b.close * (1 - p.Slippage)
					reason = "ATR_DECAY"
				case p.Tier != "" && p.EntryFunding > 0 && e.fundRate[p.Symbol] > e.v6Veto(p.Tier) &&
					e.fundRate[p.Symbol] > p.EntryFunding*e.cfg.FundReversalMult:
					exitPx = b.close * (1 - p.Slippage)
					reason = "FUND_REVERSAL"
				case b.ts > p.EntryTS: // 移动止盈 3% 激活 + 2% 回调
					if b.high > p.ExtremePrice {
						p.ExtremePrice = b.high
					}
					if !p.TrailingActive && p.ExtremePrice >= p.EntryPrice*(1+p.ActPct) {
						p.TrailingActive = true
					}
					if p.TrailingActive {
						trail := p.ExtremePrice * (1 - p.CbPct)
						if b.low <= trail {
							exitPx = min2(b.open, trail) * (1 - p.Slippage)
							reason = "TRAILING_STOP"
						}
					}
				}
			}

		case "mr":
			// 均值回归退出
			if p.Side == "LONG" {
				tp := p.EntryPrice * (1 + e.cfg.MRTpPct)
				sl := p.EntryPrice * (1 - e.cfg.MRSlPct)
				switch {
				case b.low <= sl:
					exitPx = min2(b.open, sl)
					reason = "MR_SL"
				case b.high >= tp:
					exitPx = tp
					if b.open >= tp {
						exitPx = b.open // 跳空高开直接按开盘成交
					}
					reason = "MR_TP"
				case held >= e.cfg.MRMaxHoldBars:
					exitPx = b.close
					reason = "MR_TIME"
				}
			} else { // SHORT
				tp := p.EntryPrice * (1 - e.cfg.MRTpPct)
				sl := p.EntryPrice * (1 + e.cfg.MRSlPct)
				switch {
				case b.high >= sl:
					exitPx = max2(b.open, sl)
					reason = "MR_SL"
				case b.low <= tp:
					exitPx = tp
					if b.open <= tp {
						exitPx = b.open
					}
					reason = "MR_TP"
				case held >= e.cfg.MRMaxHoldBars:
					exitPx = b.close
					reason = "MR_TIME"
				}
			}

		case "trend":
			// 趋势跟随退出: 反向 EMA 交叉优先于止损/跟踪评估
			st := e.states[p.Symbol]
			crossDn := st != nil && st.emaInit && st.filled >= e.cfg.TrendSlow &&
				st.prevFast >= st.prevSlow && st.fastEma < st.slowEma
			crossUp := st != nil && st.emaInit && st.filled >= e.cfg.TrendSlow &&
				st.prevFast <= st.prevSlow && st.fastEma > st.slowEma
			if p.Side == "LONG" {
				stop := p.EntryPrice * (1 - e.cfg.StopLossPct)
				tp := p.EntryPrice * (1 + e.cfg.TakeProfitPct)
				switch {
				case b.low <= stop:
					exitPx = min2(b.open, stop)
					reason = "STOP_LOSS"
				case e.cfg.TakeProfitPct > 0 && b.high >= tp:
					exitPx = tp
					if b.open >= tp {
						exitPx = b.open
					}
					reason = "TAKE_PROFIT"
				case e.cfg.MaxHoldBars > 0 && held >= e.cfg.MaxHoldBars:
					exitPx = b.close
					reason = "MAX_HOLD"
				case crossDn:
					exitPx = b.close
					reason = "TREND_EXIT"
				case b.ts > p.EntryTS:
					if b.high > p.ExtremePrice {
						p.ExtremePrice = b.high
					}
					if !p.TrailingActive && p.ExtremePrice >= p.EntryPrice*(1+e.cfg.TrailingActivation) {
						p.TrailingActive = true
					}
					if p.TrailingActive {
						trail := p.ExtremePrice * (1 - e.cfg.TrailingCallback)
						if b.low <= trail {
							exitPx = min2(b.open, trail)
							reason = "TRAILING_STOP"
						}
					}
				}
			} else { // SHORT
				stop := p.EntryPrice * (1 + e.cfg.StopLossPct)
				tp := p.EntryPrice * (1 - e.cfg.TakeProfitPct)
				switch {
				case b.high >= stop:
					exitPx = max2(b.open, stop)
					reason = "STOP_LOSS"
				case e.cfg.TakeProfitPct > 0 && b.low <= tp:
					exitPx = tp
					if b.open <= tp {
						exitPx = b.open
					}
					reason = "TAKE_PROFIT"
				case e.cfg.MaxHoldBars > 0 && held >= e.cfg.MaxHoldBars:
					exitPx = b.close
					reason = "MAX_HOLD"
				case crossUp:
					exitPx = b.close
					reason = "TREND_EXIT"
				case b.ts > p.EntryTS:
					if b.low < p.ExtremePrice {
						p.ExtremePrice = b.low
					}
					if !p.TrailingActive && p.ExtremePrice <= p.EntryPrice*(1-e.cfg.TrailingActivation) {
						p.TrailingActive = true
					}
					if p.TrailingActive {
						trail := p.ExtremePrice * (1 + e.cfg.TrailingCallback)
						if b.high >= trail {
							exitPx = max2(b.open, trail)
							reason = "TRAILING_STOP"
						}
					}
				}
			}

		case "adaptive": // 自适应: 按开仓时固化的模式参数退出（chase/pullback/short 各有独立风控）
			fallthrough
		default: // momentum: 固定止损 + 跟踪止盈（参数已按模式固化到 Position）
			if p.Side == "LONG" {
				stop := p.EntryPrice * (1 - p.SLPct)
				tp := p.EntryPrice * (1 + p.TPPct)
				if e.cfg.ExitClose {
					// 收盘价模式（近似 aooo 的 tick 采样）: 仅当片收盘价触发，不捕捉片内插针
					if b.close <= stop {
						exitPx = min2(b.open, stop)
						reason = "STOP_LOSS"
					} else if p.TPPct > 0 && b.close >= tp {
						exitPx = max2(b.open, tp)
						reason = "TAKE_PROFIT"
					} else if p.HoldBars > 0 && held >= p.HoldBars {
						exitPx = b.close
						reason = "MAX_HOLD"
					} else if b.ts > p.EntryTS { // 开仓当片不评估跟踪止盈
						if b.high > p.ExtremePrice {
							p.ExtremePrice = b.high
						}
						if !p.TrailingActive && p.ExtremePrice >= p.EntryPrice*(1+p.ActPct) {
							p.TrailingActive = true
						}
						if p.TrailingActive {
							trail := p.ExtremePrice * (1 - p.CbPct)
							if b.close <= trail {
								exitPx = min2(b.open, trail)
								reason = "TRAILING_STOP"
							} else if e.cfg.TakerExitPct > 0 && b.close > p.EntryPrice &&
								e.takerRatio(p.Symbol) < e.cfg.TakerExitPct {
								exitPx = b.close
								reason = "TAKER_EXIT"
							}
						}
					}
				} else if b.low <= stop {
					exitPx = min2(b.open, stop)
					reason = "STOP_LOSS"
				} else if p.TPPct > 0 && b.high >= tp {
					exitPx = tp
					if b.open >= tp {
						exitPx = b.open // 跳空高开直接按开盘成交
					}
					reason = "TAKE_PROFIT"
				} else if p.HoldBars > 0 && held >= p.HoldBars {
					exitPx = b.close
					reason = "MAX_HOLD"
				} else if b.ts > p.EntryTS { // 开仓当片不评估跟踪止盈（避免 0 盈亏虚假平仓）
					if b.high > p.ExtremePrice {
						p.ExtremePrice = b.high
					}
					if !p.TrailingActive && p.ExtremePrice >= p.EntryPrice*(1+p.ActPct) {
						p.TrailingActive = true
					}
					if p.TrailingActive {
						trail := p.ExtremePrice * (1 - p.CbPct)
						if b.low <= trail {
							exitPx = min2(b.open, trail)
							reason = "TRAILING_STOP"
						} else if e.cfg.TakerExitPct > 0 && b.close > p.EntryPrice &&
							e.takerRatio(p.Symbol) < e.cfg.TakerExitPct {
							exitPx = b.close
							reason = "TAKER_EXIT"
						}
					}
				}
			} else { // SHORT
				stop := p.EntryPrice * (1 + p.SLPct)
				tp := p.EntryPrice * (1 - p.TPPct)
				if e.cfg.ExitClose {
					// 收盘价模式（近似 aooo 的 tick 采样）
					if b.close >= stop {
						exitPx = max2(b.open, stop)
						reason = "STOP_LOSS"
					} else if p.TPPct > 0 && b.close <= tp {
						exitPx = min2(b.open, tp)
						reason = "TAKE_PROFIT"
					} else if p.HoldBars > 0 && held >= p.HoldBars {
						exitPx = b.close
						reason = "MAX_HOLD"
					} else if b.ts > p.EntryTS {
						if b.low < p.ExtremePrice {
							p.ExtremePrice = b.low
						}
						if !p.TrailingActive && p.ExtremePrice <= p.EntryPrice*(1-p.ActPct) {
							p.TrailingActive = true
						}
						if p.TrailingActive {
							trail := p.ExtremePrice * (1 + p.CbPct)
							if b.close >= trail {
								exitPx = max2(b.open, trail)
								reason = "TRAILING_STOP"
							}
						}
					}
				} else if b.high >= stop {
					exitPx = max2(b.open, stop)
					reason = "STOP_LOSS"
				} else if p.TPPct > 0 && b.low <= tp {
					exitPx = tp
					if b.open <= tp {
						exitPx = b.open // 跳空低开直接按开盘成交
					}
					reason = "TAKE_PROFIT"
				} else if p.HoldBars > 0 && held >= p.HoldBars {
					exitPx = b.close
					reason = "MAX_HOLD"
				} else if b.ts > p.EntryTS { // 开仓当片不评估跟踪止盈
					if b.low < p.ExtremePrice {
						p.ExtremePrice = b.low
					}
					if !p.TrailingActive && p.ExtremePrice <= p.EntryPrice*(1-p.ActPct) {
						p.TrailingActive = true
					}
					if p.TrailingActive {
						trail := p.ExtremePrice * (1 + p.CbPct)
						if b.high >= trail {
							exitPx = max2(b.open, trail)
							reason = "TRAILING_STOP"
						}
					}
				}
			}
		}

		if reason == "" {
			kept = append(kept, p)
			continue
		}

		// 平仓结算（复用 closePosition，资金费收入一并并入 PnL）
		e.closePosition(p, b, exitPx, reason)
	}
	e.positions = kept
}

// OnBar 处理一个时间片（所有有数据的币种 K 线）。
// 处理顺序: 成交 pending -> 更新状态/评估信号（funding 范式走资金费逻辑）-> 开仓 -> 监控持仓 -> 记录权益曲线
// 参数:
//   - bars: 该片全部币种 K 线
//   - fundings: 该片资金费结算点（symbol -> 记录；仅 funding 范式使用，其余范式传 nil）
//   - ts: 该片时间戳
func (e *Engine) OnBar(bars map[string]*bar, fundings map[string]fundingPoint, ts int64) {
	if ts == e.lastTS {
		return // 防御: 同一片重复调用
	}
	e.lastTS = ts

	// 1. 成交上一片待开仓单（用当前片开盘价，避免未来函数）
	e.fillPending(bars, ts)

	// 1.5 资金费率推进（结算点才变化，结算间保持最近值；无费率数据时为空 map 无副作用）
	for sym, fp := range fundings {
		e.fundPrev[sym] = e.fundRate[sym]
		e.fundRate[sym] = fp.rate
	}
	// S01 实验: momentum 模式持仓资金费成本（每 8h 结算点收费，平仓时并入 PnL）
	// 币安惯例: 正费率 = 多头付空头 → 多头 -rate×名义值，空头 +rate×名义值
	if e.cfg.FundingCostEnabled && e.cfg.Mode != "funding" {
		for _, p := range e.positions {
			fp, ok := fundings[p.Symbol]
			if !ok {
				continue
			}
			b, ok := bars[p.Symbol]
			if !ok {
				continue
			}
			px := fp.markPrice
			if px <= 0 {
				px = b.close
			}
			paid := fp.rate * px * p.Amount
			if p.Side == "LONG" {
				paid = -paid
			}
			p.FundingCollected += paid
			e.fundingIncome += paid
		}
	}

	// 2. 更新状态并评估信号（使用本片收盘数据）
	var cands []candidate
	if e.cfg.Mode == "v6" {
		// v6 模式: 逐币推进状态并评估 L1→L2→L3 信号链
		// 日熔断跨天重置（UTC 日）
		day := ts / 86400000
		if day != e.lastDay {
			e.dayStartEquity = e.equity
			e.dayPnl = 0
			e.dayBlocked = false
			e.lastDay = day
		}
		for sym, b := range bars {
			st, ready := e.updateState(sym, b)
			if side, score, tier, ok := e.v6Signal(sym, st, b, ready); ok {
				cands = append(cands, candidate{symbol: sym, side: side, volume: b.quoteVol, mode: "v6", score: score, tier: tier})
			}
		}
	} else if e.cfg.Mode == "funding" {
		// funding 模式: 先推进全部币种状态（资金费开仓依赖 24h 窗口与流动性），
		// 再处理资金费事件（收取/退出/开仓候选）
		for sym, b := range bars {
			e.updateState(sym, b)
		}
		cands = e.processFunding(bars, fundings, ts)
	} else {
		// 自适应模式: 先推进 BTC 市场状态（模式判定依赖 BTC EMA/ATR）
		// 市场状态过滤实验（S01 单因子）: momentum 模式同样推进 BTC 状态供 btc24h/btcma 判定
		if e.cfg.Mode == "adaptive" || (e.cfg.Regime != "" && e.cfg.Regime != "none") {
			if b, ok := bars["BTCUSDT"]; ok {
				e.updateBTC(b)
			}
		}
		// 市场状态过滤: 整片先判定一次（breadth 用上一片已更新状态，5m 滞后可忽略）
		regimePass := e.regimeOK()
		// 24h 涨幅排名过滤: 先推进全部状态并收集排名数据（两遍循环，避免重复推进）
		if e.cfg.RankMode > 0 {
			e.gain24s = e.gain24s[:0]
			for sym, b := range bars {
				st, ready := e.updateState(sym, b)
				if !ready || st.sumVol24 < e.cfg.MinQuoteVolume {
					continue
				}
				old := prevClose(st, WindowBars)
				if old > 0 {
					e.gain24s = append(e.gain24s, gainRec{sym: sym, g: (b.close - old) / old})
				}
			}
			e.computeRank()
		}
		for sym, b := range bars {
			var st *symbolState
			var ready bool
			if e.cfg.RankMode > 0 {
				st = e.states[sym]
				if st == nil {
					continue
				}
				ready = st.filled >= WindowBars
			} else {
				st, ready = e.updateState(sym, b)
			}
			if !regimePass {
				continue
			}
			if side, mode := e.computeSignal(st, b, ready); side != "" {
				// S01 实验: 24h 涨幅排名过滤（替代固定 24h 涨幅）
				if e.cfg.RankMode > 0 && !e.rankOK[sym] {
					e.rankBlocked++
					continue
				}
				// S01 实验: 趋势因子（替代固定 24h 涨幅）
				if !e.trendOK(st, b) {
					e.trendBlocked++
					continue
				}
				// S01 单因子实验（默认全关）: 费率过热否决 / 成交量 Z 确认 / RSI 趋势带
				if e.cfg.FundingVetoEnabled && e.fundingVetoed(sym, st.sumVol24) {
					e.fundingVetoCount++
					continue
				}
				if e.cfg.VolumeZThreshold > 0 && v6VolumeZ(st, e.cfg) < e.cfg.VolumeZThreshold {
					continue
				}
				if e.cfg.RSIFilterEnabled && (st.rsi < e.cfg.RSIMin || st.rsi > e.cfg.RSIMax) {
					continue
				}
				tk := 0.0
				if st.periodVol > 0 {
					tk = st.periodTBB / st.periodVol * 100
				}
				cands = append(cands, candidate{
					symbol: sym, side: side, volume: b.quoteVol, mode: mode, ref: b.close,
					gain15: (b.close - st.periodOpen) / st.periodOpen * 100,
					surge:  volumeSurge(st, b, e.cfg.SurgeLookback),
					taker:  tk,
				})
			}
		}
	}

	// 3. 候选按成交额降序取 TopN 开仓
	sortCandidates(cands)
	if len(cands) > e.cfg.TopN {
		cands = cands[:e.cfg.TopN]
	}
	e.openPositions(cands, ts)

	// 4. 监控持仓
	e.monitorPositions(bars)

	// 5. 记录权益曲线
	e.equityCurve = append(e.equityCurve, EquityPoint{TS: ts, Equity: e.equity})
}

// Finalize 回测结束时强制平掉所有未平持仓（按最后已知价格估算，不计入回撤分析用收益曲线）
// 参数:
//   - lastBars: 最后一个时间片的 K 线（提供最后价格）
func (e *Engine) Finalize(lastBars map[string]*bar) {
	for _, p := range e.positions {
		b, ok := lastBars[p.Symbol]
		if !ok {
			continue
		}
		// 强制按最后收盘价平仓（closePosition 自动并入资金费收入与手续费）
		e.closePosition(p, b, b.close, "FORCE_CLOSE")
	}
	e.positions = nil
}

// min2 返回两个浮点数中的较小值
func min2(a, b float64) float64 {
	if a < b {
		return a
	}
	return b
}

// max2 返回两个浮点数中的较大值
func max2(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
