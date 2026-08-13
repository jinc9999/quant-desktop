package storage

import "time"

// OpenAttempt 开仓尝试全链路记录（转化率归因核心数据）。
//
// 从「信号候选」到「成交/失败」的每个阶段各写一条，供 market_metrics analyze
// 与每日总结做逐单归因：
//   - 拦截 = 策略规则内未开（该挡）：cooldown / no_active / addon_limit / maxpos /
//     新币 / 成交额 / 排名 / 山顶 / 熔断 等规则性拒绝；
//   - 执行损耗 = 模拟规则可开但实际未成交：执行失败（错误码）/ 余额不足 /
//     数据缺口（K 线拉取失败）/ 激活错配（客户端未采到激活价）/ 信号未触发（tick 采样差）。
//
// 规则：同一（symbol, reason, 5m 周期）只写一条，避免信号持续期内每 tick 刷屏。
type OpenAttempt struct {
	ID         int64
	Ts         int64   // Unix 毫秒
	Tick       int64   // 引擎 tick 序号
	Symbol     string  // 交易对（如 "BTCUSDT"）
	Side       string  // LONG / SHORT
	Stage      string  // candidate / selected / attempted / filled / failed
	Reason     string  // 来源或拒绝原因，见 Reason* 常量
	Gain15     float64 // 15m 周期实体涨幅%（信号触发时）
	KlineOpen  float64 // 15m 周期开盘价
	Gain5m     float64 // 当前 15m 周期最大 5m 收盘涨幅%（-1=无数据）
	Bucket     string  // 爆拉桶 / 中间桶 / 温和桶
	Margin     float64 // 单仓保证金（U）
	Qty        float64 // 下单数量
	ErrorCode  int64   // 交易所错误码（failed 时）
	ErrorMsg   string  // 错误信息（failed 时）
	RetryCount int     // 重试次数
	LatencyMs  int64   // 下单耗时
	PositionID int64   // 成交后对应持仓 ID
}

// Stage 常量（开仓链路阶段）
const (
	StageCandidate = "candidate" // 信号候选（含实时 / 5m 收盘两个来源）
	StageSelected  = "selected"  // 通过全部规则检查、进入并发开仓任务
	StageAttempted = "attempted" // 已发出下单请求
	StageFilled    = "filled"    // 成交
	StageFailed    = "failed"    // 下单失败
)

// Reason 常量（候选来源 / 拒绝原因 / 结果）
const (
	ReasonLiveSignal      = "live_signal"       // 实时 tick 信号候选
	ReasonKlineClose      = "kline_close"       // 5m 收盘对齐信号候选（与回测/漏斗口径一致）
	ReasonMaxPos          = "maxpos"            // 全局 10 仓上限
	ReasonCooldown        = "cooldown"          // 冷却期内
	ReasonNoActive        = "no_active"         // 持仓未激活无法追单（模拟判可追=激活错配）
	ReasonAddonLimit      = "addon_limit"       // 同币追单达上限
	ReasonAddonDisabled   = "addon_disabled"    // 追加仓开关关闭
	ReasonAddonSide       = "addon_side"        // 追单方向与现仓不一致
	ReasonBalance         = "balance"           // 可用余额不足（含逐候选预算不足）
	ReasonBalanceQueryFail = "balance_query_fail" // 余额查询失败，保守跳过本 tick
	ReasonNewListing      = "new_listing"       // 新币过滤
	Reason24hGain         = "24h_gain"          // 24h 涨幅门槛
	ReasonRank            = "rank"              // 24h 涨幅排名过滤
	ReasonPullback        = "pullback"          // 山顶过滤器（距 24h 高点回撤过大）
	ReasonMarkDev         = "mark_dev"          // 标记价可信度过滤：收盘价 vs 标记价收盘价偏差超阈值（防插针）
	ReasonVolume          = "volume"            // 成交额校验不满足
	ReasonFuturesOnly     = "futures_only"      // 无合约交易对
	ReasonRoundZero       = "round_zero"        // 取整后数量为 0
	ReasonOpenFailedCD    = "open_failed_cd"    // 开仓失败冷却中
	ReasonOpenBlocked     = "open_blocked"      // 结构性失败拉黑中
	ReasonKlineMissing    = "kline_missing"     // 15m K 线开盘价拉取失败（数据缺口）
	ReasonBreaker         = "breaker"           // 熔断器已触发
	ReasonSlots           = "slots"             // 槽位/任务数截断（本 tick 已满）
	ReasonNoPrice         = "no_price"          // 无实时价
	ReasonAttempted       = "attempted"         // 已发出下单请求
	ReasonSelected        = "selected"          // 进入任务队列
	ReasonFilled          = "filled"            // 成交
	ReasonFailed          = "failed"            // 失败（错误码见 ErrorCode）
)

// RuleBlockReasons 策略规则内未开（该挡）的原因集合，供归因工具直接判定「拦截」。
var RuleBlockReasons = map[string]bool{
	ReasonMaxPos:          true,
	ReasonCooldown:        true,
	ReasonAddonLimit:      true,
	ReasonAddonDisabled:   true,
	ReasonAddonSide:       true,
	ReasonNewListing:      true,
	Reason24hGain:         true,
	ReasonRank:            true,
	ReasonPullback:        true,
	ReasonMarkDev:         true,
	ReasonVolume:          true,
	ReasonFuturesOnly:     true,
	ReasonRoundZero:       true,
	ReasonOpenFailedCD:    true,
	ReasonOpenBlocked:     true,
	ReasonBreaker:         true,
	ReasonSlots:           true,
	ReasonNoPrice:         true,
	ReasonBalanceQueryFail: true,
}

// InsertOpenAttempt 写入一条开仓尝试记录（幂等由调用方去重）。
func (db *DB) InsertOpenAttempt(a *OpenAttempt) error {
	_, err := db.Conn.Exec(`INSERT INTO open_attempts
		(ts, tick, symbol, side, stage, reason, gain15, kline_open, gain5m, bucket,
		 margin, qty, error_code, error_msg, retry_count, latency_ms, position_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.Ts, a.Tick, a.Symbol, a.Side, a.Stage, a.Reason, a.Gain15, a.KlineOpen,
		a.Gain5m, a.Bucket, a.Margin, a.Qty, a.ErrorCode, a.ErrorMsg,
		a.RetryCount, a.LatencyMs, a.PositionID)
	return err
}

// GetOpenAttemptsSince 读取某时刻之后的全部开仓尝试记录（时间升序）。
func (db *DB) GetOpenAttemptsSince(ts int64) ([]OpenAttempt, error) {
	rows, err := db.Conn.Query(`SELECT id, ts, tick, symbol, side, stage, reason,
		ifnull(gain15,0), ifnull(kline_open,0), ifnull(gain5m,-1), ifnull(bucket,''),
		ifnull(margin,0), ifnull(qty,0), ifnull(error_code,0), ifnull(error_msg,''),
		ifnull(retry_count,0), ifnull(latency_ms,0), ifnull(position_id,0)
		FROM open_attempts WHERE ts >= ? ORDER BY ts`, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OpenAttempt
	for rows.Next() {
		var a OpenAttempt
		if err := rows.Scan(&a.ID, &a.Ts, &a.Tick, &a.Symbol, &a.Side, &a.Stage, &a.Reason,
			&a.Gain15, &a.KlineOpen, &a.Gain5m, &a.Bucket, &a.Margin, &a.Qty,
			&a.ErrorCode, &a.ErrorMsg, &a.RetryCount, &a.LatencyMs, &a.PositionID); err != nil {
			continue
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// InsertEngineHeartbeat 写入引擎心跳（策略运行在线证明，供漏斗「客户端未运行」归因）。
// runOnce 每 4 tick（约 1 分钟）写一条；间隔超过阈值视为离线。
func (db *DB) InsertEngineHeartbeat(mode string, tick int64) error {
	_, err := db.Conn.Exec(`INSERT INTO engine_heartbeat (ts, mode, tick) VALUES (?, ?, ?)`,
		time.Now().UnixMilli(), mode, tick)
	return err
}

// GetEngineHeartbeatsSince 读取某时刻之后的心跳时间戳（升序）。
func (db *DB) GetEngineHeartbeatsSince(ts int64) ([]int64, error) {
	rows, err := db.Conn.Query(`SELECT ts FROM engine_heartbeat WHERE ts >= ? ORDER BY ts`, ts)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var t int64
		if err := rows.Scan(&t); err != nil {
			continue
		}
		out = append(out, t)
	}
	return out, rows.Err()
}
