// Package strategy 滑动窗口价格追踪
package strategy

import (
	"sort"
	"strconv"
	"sync"
)

// PricePoint 单个价格采样点
type PricePoint struct {
	Price     float64
	Timestamp int64
	Volume    float64 // 24h 累计成交额（USDT），用于计算成交量放大倍数
}

// SlidingWindow 滑动窗口价格追踪器
// 为每个交易对维护一段时间序列，用于计算"恰好 N 分钟前"的滑动涨幅。
//
// 与按整点切片的 K 线不同：K 线在 10:07 检查时只覆盖 10:05 起的 2 分钟涨幅，
// 窗口长度随检查时刻忽长忽短。滑动窗口的基准永远是"当前时刻 - 窗口长度"那个瞬间的价格，
// 因此无论何时检查，窗口长度都恒定为完整的 N 分钟。
type SlidingWindow struct {
	windowMs int64 // 窗口长度（毫秒），如 5 分钟 = 300000
	maxGapMs int64 // 基准点匹配容差（毫秒），超过则视为窗口未就绪
	mu       sync.RWMutex
	series   map[string][]PricePoint // symbol -> 按时间升序的采样序列
}

// NewSlidingWindow 创建滑动窗口
// windowMs: 窗口长度（毫秒）
// sampleMs: 采样间隔（毫秒），用于推导基准匹配容差
// 返回: 初始化好的滑动窗口实例
func NewSlidingWindow(windowMs, sampleMs int64) *SlidingWindow {
	// 容差取一个采样间隔：正常情况下最近采样点距目标时刻不超过半个间隔，
	// 留一个完整间隔的余量以容忍 tick 调度抖动
	maxGap := sampleMs
	if maxGap <= 0 {
		maxGap = 10000
	}
	return &SlidingWindow{
		windowMs: windowMs,
		maxGapMs: maxGap,
		series:   make(map[string][]PricePoint),
	}
}

// Add 记录一个价格采样点，并裁剪超出窗口范围的旧数据
// symbol: 交易对
// ts: 采样时间（Unix 毫秒）
// price: 采样价格（<=0 时忽略，避免污染序列）
func (w *SlidingWindow) Add(symbol string, ts int64, price float64) {
	if price <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()

	points := append(w.series[symbol], PricePoint{Price: price, Timestamp: ts})

	// 裁剪：仅保留 [ts-窗口-容差, ts] 范围内的点，避免内存无限增长
	cutoff := ts - w.windowMs - w.maxGapMs
	idx := sort.Search(len(points), func(i int) bool {
		return points[i].Timestamp >= cutoff
	})
	if idx > 0 {
		points = points[idx:]
	}
	w.series[symbol] = points
}

// Sample 记录一个价格采样点（含成交量），并裁剪超出窗口范围的旧数据
// symbol: 交易对
// price: 采样价格
// volume: 24h 累计成交额（USDT）
// now: 采样时间（Unix 毫秒）
func (w *SlidingWindow) Sample(symbol string, price float64, volume float64, now int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	series := w.series[symbol]
	series = append(series, PricePoint{Price: price, Timestamp: now, Volume: volume})

	cutoff := now - w.windowMs
	start := 0
	for start < len(series) && series[start].Timestamp < cutoff {
		start++
	}
	if start > 0 {
		series = series[start:]
	}
	w.series[symbol] = series
}

// BackfillCache 回填缓存：仅当 symbol 尚无采样数据时写入初始点
// symbol: 交易对
// price: 初始价格
// volume: 24h 累计成交额（USDT）
// now: 当前时间（Unix 毫秒）
func (w *SlidingWindow) BackfillCache(symbol string, price float64, volume float64, now int64) {
	w.mu.Lock()
	defer w.mu.Unlock()

	if len(w.series[symbol]) > 0 {
		return
	}
	w.series[symbol] = []PricePoint{{Price: price, Timestamp: now, Volume: volume}}
}

// baselinePrice 返回 symbol 在 now 时刻的基准价格
// symbol: 交易对
// now: 当前时间（Unix 毫秒）
// 返回: (基准价格, 是否可判断)。ok=false 仅当没有任何历史采样点时。
func (w *SlidingWindow) baselinePrice(symbol string, now int64) (price float64, ok bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	pt, ok := w.baselinePoint(symbol, now)
	if !ok {
		return 0, false
	}
	return pt.Price, true
}

// baselinePoint 返回 symbol 在 now 时刻的基准采样点（调用方需持有读锁）
//
// 基准点选取规则：
//  1. 满窗口：存在接近 (now - windowMs) 的采样点时，取最接近者，窗口长度恒为完整窗口。
//  2. 预热期：尚无足够旧的采样点时，取"最早可用的采样点"作基准，
//     窗口长度从采样间隔逐渐增长到完整窗口。这样预热期内也能立即判断涨幅，
//     例如第 60 秒用第 0 秒的价格作基准，无需等满 5 分钟。
//
// 返回: (基准采样点, 是否可判断)。ok=false 仅当序列为空或仅有当前时刻一个点。
func (w *SlidingWindow) baselinePoint(symbol string, now int64) (PricePoint, bool) {
	points := w.series[symbol]
	if len(points) == 0 {
		return PricePoint{}, false
	}

	target := now - w.windowMs
	// 找到第一个 ts >= target 的点，再与它前一个点比较谁更接近 target
	idx := sort.Search(len(points), func(i int) bool {
		return points[i].Timestamp >= target
	})

	var best PricePoint
	switch {
	case idx == 0:
		// 所有点都晚于 target（预热期，最早的点也不够旧）
		best = points[0]
	case idx == len(points):
		// 所有点都早于 target（取最新的一个）
		best = points[len(points)-1]
	default:
		// 比较 idx 与 idx-1 哪个更接近 target
		if abs64(points[idx].Timestamp-target) <= abs64(points[idx-1].Timestamp-target) {
			best = points[idx]
		} else {
			best = points[idx-1]
		}
	}

	// 满窗口：基准点足够接近目标时刻，直接使用
	if abs64(best.Timestamp-target) <= w.maxGapMs {
		return best, true
	}

	// 预热期：用最早可用的采样点作基准，窗口长度逐渐增长
	oldest := points[0]
	if oldest.Timestamp >= now {
		// 仅有当前时刻一个采样点，无法形成价格对比
		return PricePoint{}, false
	}
	return oldest, true
}

// WindowLengthMs 返回 symbol 在 now 时刻的有效窗口长度（毫秒）
// 满窗口时约为 windowMs；预热期为"最早采样点到 now"的时长，逐渐增长。
// symbol: 交易对
// now: 当前时间（Unix 毫秒）
// 返回: 有效窗口毫秒数；0 表示尚无法判断（无历史采样点）。
func (w *SlidingWindow) WindowLengthMs(symbol string, now int64) int64 {
	w.mu.RLock()
	defer w.mu.RUnlock()

	pt, ok := w.baselinePoint(symbol, now)
	if !ok {
		return 0
	}
	return now - pt.Timestamp
}

// GainPct 计算 symbol 在 now 时刻的滑动窗口涨幅百分比
// symbol: 交易对
// current: 当前价格
// now: 当前时间（Unix 毫秒）
// 返回: (涨幅百分比, 是否就绪)。公式: (当前价 - 基准价) / 基准价 * 100。
func (w *SlidingWindow) GainPct(symbol string, current float64, now int64) (gain float64, ok bool) {
	baseline, ok := w.baselinePrice(symbol, now)
	if !ok || baseline <= 0 || current <= 0 {
		return 0, false
	}
	return (current - baseline) / baseline * 100, true
}

// MaxGainPct 计算 symbol 在窗口内的最大涨幅百分比。
// 遍历窗口内所有历史价格点（排除当前时刻的采样），
// 计算 (current - historicalPrice) / historicalPrice * 100，返回最大值。
// 只要有至少一个早于 now 的采样点即返回 ok=true，无需等待窗口满。
//
// 示例：窗口内有 [100, 98, 105]，当前价 110
//
//	涨幅分别为 10%、12.2%、4.8%，返回 12.2%
func (w *SlidingWindow) MaxGainPct(symbol string, current float64, now int64) (maxGain float64, ok bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	points := w.series[symbol]
	if len(points) == 0 || current <= 0 {
		return 0, false
	}

	found := false
	for _, p := range points {
		// 跳过当前时刻的采样点（刚 Add 进去的）
		if p.Timestamp >= now || p.Price <= 0 {
			continue
		}
		gain := (current - p.Price) / p.Price * 100
		if !found || gain > maxGain {
			maxGain = gain
			found = true
		}
	}
	return maxGain, found
}

// RecentGainPct 计算最近 recentMs 毫秒内的涨幅百分比
// 用于二次确认：长窗口筛出候选后，短窗口确认动量仍在
// symbol: 交易对
// current: 当前价格
// now: 当前时间（Unix 毫秒）
// recentMs: 短窗口长度（毫秒），如 3 分钟 = 180000
// 返回: (涨幅百分比, 是否就绪)
//   - 就绪条件：存在至少一个早于 (now - recentMs) 的采样点
//   - 涨幅 = (current - 短窗口基准价) / 短窗口基准价 * 100
func (w *SlidingWindow) RecentGainPct(symbol string, current float64, now int64, recentMs int64) (float64, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	series := w.series[symbol]
	if len(series) == 0 || current <= 0 {
		return 0, false
	}

	// 找到最接近 (now - recentMs) 的采样点作为短窗口基准
	cutoff := now - recentMs
	var base PricePoint
	found := false
	for _, p := range series {
		if p.Timestamp <= cutoff {
			base = p
			found = true
		} else {
			break
		}
	}
	if !found {
		return 0, false
	}
	if base.Price <= 0 {
		return 0, false
	}
	return (current - base.Price) / base.Price * 100, true
}

// RecentVolumeSurge 计算最近 recentMs 的成交量相对之前 priorMs 窗口的放大倍数
// 通过 24h 累计成交额的差值计算每个区间的实际成交量
// symbol: 交易对
// now: 当前时间（Unix 毫秒）
// recentMs: 最近窗口长度（毫秒），如 3 分钟 = 180000
// priorMs: 之前窗口长度（毫秒），如 12 分钟 = 720000
// 返回: (放大倍数, 是否就绪)
//   - 就绪条件：两个窗口内各有至少 2 个采样点
//   - 倍数 = 最近窗口平均每秒成交量 / 之前窗口平均每秒成交量
func (w *SlidingWindow) RecentVolumeSurge(symbol string, now int64, recentMs, priorMs int64) (float64, bool) {
	w.mu.RLock()
	defer w.mu.RUnlock()

	series := w.series[symbol]
	if len(series) < 4 {
		return 0, false
	}

	recentCutoff := now - recentMs
	priorCutoff := now - priorMs

	// 计算最近窗口的成交量（累计成交额差值）
	// 边界取「最后一个 ts <= cutoff 的采样点」的累计值：series 按时间升序，
	// 持续覆盖赋值后得到的即窗口起点的累计成交额（而非序列最旧点）
	var recentVolStart, recentVolEnd float64
	var recentStartFound, recentEndFound bool
	for _, p := range series {
		if p.Timestamp <= recentCutoff {
			recentVolStart = p.Volume
			recentStartFound = true
		}
		if p.Timestamp <= now {
			recentVolEnd = p.Volume
			recentEndFound = true
		}
	}
	if !recentStartFound || !recentEndFound {
		return 0, false
	}
	recentVol := recentVolEnd - recentVolStart
	if recentVol <= 0 || recentMs <= 0 {
		return 0, false
	}
	recentRate := recentVol / float64(recentMs) // 每毫秒成交量

	// 计算之前窗口的成交量
	var priorVolStart, priorVolEnd float64
	var priorStartFound, priorEndFound bool
	for _, p := range series {
		if p.Timestamp <= priorCutoff {
			priorVolStart = p.Volume
			priorStartFound = true
		}
		if p.Timestamp <= recentCutoff {
			priorVolEnd = p.Volume
			priorEndFound = true
		}
	}
	if !priorStartFound || !priorEndFound {
		return 0, false
	}
	priorVol := priorVolEnd - priorVolStart
	if priorVol <= 0 || priorMs <= 0 {
		return 0, false
	}
	priorRate := priorVol / float64(priorMs) // 每毫秒成交量

	return recentRate / priorRate, true
}

// ParseTimeframeMs 将周期字符串解析为毫秒
// tf: 周期字符串，支持 s/m/h/d 后缀（如 "5m"、"1h"）
// 返回: 对应的毫秒数；无法识别时返回默认 5 分钟（300000）
func ParseTimeframeMs(tf string) int64 {
	const defaultMs = int64(300000)
	if len(tf) < 2 {
		return defaultMs
	}
	unit := tf[len(tf)-1]
	n, err := strconv.ParseInt(tf[:len(tf)-1], 10, 64)
	if err != nil || n <= 0 {
		return defaultMs
	}
	switch unit {
	case 's':
		return n * 1000
	case 'm':
		return n * 60 * 1000
	case 'h':
		return n * 60 * 60 * 1000
	case 'd':
		return n * 24 * 60 * 60 * 1000
	default:
		return defaultMs
	}
}

// abs64 返回 int64 的绝对值
// x: 输入整数
// 返回: x 的绝对值
func abs64(x int64) int64 {
	if x < 0 {
		return -x
	}
	return x
}
