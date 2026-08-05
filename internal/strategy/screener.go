// Package strategy 筛选排名逻辑
package strategy

import (
	"sort"

	"quant-desktop/internal/binance"
)

// Candidate 候选币种
type Candidate struct {
	Symbol      string  `json:"symbol"`
	GainPct     float64 `json:"gainPct"`     // 滑动窗口涨幅百分比（绝对值）
	QuoteVolume float64 `json:"quoteVolume"` // 24h 成交额（流动性过滤）
	Side        string  `json:"side"`        // "LONG" 或 "SHORT"
}

// ScreenSliding 筛选并排名候选币种，支持两种信号模式：
//
//	sliding 模式：滑动窗口过程涨幅（当前价 vs 窗口内任意历史点，易受插针影响，旧版）
//	kline 模式：当前 15m K 线实体涨幅（当前价 vs 本周期开盘价，真上涨确认，
//	           K 线开盘价在周期内不变，由调用方拉取并缓存后传入 klineOpen）
//
// window: 滑动窗口追踪器（sliding 模式提供基准价与涨幅；kline 模式仅用于日志展示）
// tickers: 24h 行情列表（提供成交额、24h 涨跌幅、24h 最高/最低价与回退现价）
// priceMap: symbol -> 当前价（优先 WS 实时价）
// minGainPct: 最小涨幅阈值（百分比；sliding=窗口涨幅，kline=K 线实体涨幅）
// min24hGainPct: 最小 24h 涨幅阈值（百分比，>0 时启用双条件过滤）
// minQuoteVolume: 最小成交额阈值
// topN: 候选数量上限（按成交额降序截取前 N 个，<=0 表示不限制）
// now: 当前时间（Unix 毫秒）
// enableShort: 是否启用做空筛选
// confirmWindowMs: 短窗口确认时长（毫秒），0=关闭二次确认
// confirmThreshold: 短窗口涨幅确认阈值（%），0=关闭
// volumeSurgeThreshold: 成交量放大倍数阈值，0=关闭
// signalMode: 信号模式（"kline" 或 "sliding"，其他值按 sliding 处理）
// klineOpen: symbol -> 当前 K 线开盘价（kline 模式用；缺失的币保守跳过）
// maxPullbackPct: 山顶过滤器（%）：当前价距 24h 最高/最低价回撤超过该值不追，0=关闭
// 返回: 按成交额降序排列的候选列表（最多 topN 个）
func ScreenSliding(window *SlidingWindow, tickers []binance.Ticker, priceMap map[string]float64,
	minGainPct, min24hGainPct, minQuoteVolume float64, topN int, now int64,
	enableShort bool, confirmWindowMs int64, confirmThreshold, volumeSurgeThreshold float64,
	signalMode string, klineOpen map[string]float64, maxPullbackPct float64) []Candidate {
	var candidates []Candidate
	klineMode := signalMode == "kline"

	for _, t := range tickers {
		// 流动性过滤：成交额不足直接跳过
		if t.QuoteVolume < minQuoteVolume {
			continue
		}

		// 当前价：优先 priceMap（WS 实时），回退 ticker.LastPrice
		current, ok := priceMap[t.Symbol]
		if !ok || current <= 0 {
			current = t.LastPrice
		}
		if current <= 0 {
			continue
		}

		// 计算涨幅：
		//   kline 模式：当前价 vs 本周期 K 线开盘价（K 线开盘价在周期内不变，缺失时保守跳过）
		//   sliding 模式：当前价 vs 窗口内任意历史点的最大过程涨幅
		var gain float64
		var ready bool
		if klineMode {
			open, ok := klineOpen[t.Symbol]
			if !ok || open <= 0 {
				continue // K 线开盘价缺失（拉取失败/未拉取），保守跳过，避免假信号
			}
			gain = (current - open) / open * 100
			ready = true
		} else {
			gain, ready = window.MaxGainPct(t.Symbol, current, now)
		}
		if !ready {
			continue
		}

		// 判断方向：做多（涨幅达标）或做空（跌幅达标）
		var side string
		var absGain float64
		if gain >= minGainPct {
			side = "LONG"
			absGain = gain
		} else if enableShort && gain <= -minGainPct {
			side = "SHORT"
			absGain = -gain
		} else {
			continue
		}

		// 24h 涨幅过滤（双条件之一）：PriceChange 为 24h 涨跌幅百分比
		// 做多要求 24h 涨幅达标；做空要求 24h 跌幅达标
		if min24hGainPct > 0 {
			if side == "LONG" && t.PriceChange < min24hGainPct {
				continue // 做多：24h 涨幅不足，排除
			}
			if side == "SHORT" && t.PriceChange > -min24hGainPct {
				continue // 做空：24h 跌幅不足，排除
			}
		}

		// 山顶过滤器：做多时不追「已从 24h 最高价大幅回落」的币（可能接飞刀），
		// 做空时不追「已从 24h 最低价大幅反弹」的币
		if maxPullbackPct > 0 {
			if side == "LONG" && t.HighPrice > 0 {
				if (t.HighPrice-current)/t.HighPrice*100 > maxPullbackPct {
					continue
				}
			}
			if side == "SHORT" && t.LowPrice > 0 {
				if (current-t.LowPrice)/current*100 > maxPullbackPct {
					continue
				}
			}
		}

		// 二次确认（仅 sliding 模式）：短窗口涨幅 + 成交量放大
		if !klineMode && confirmThreshold > 0 && confirmWindowMs > 0 {
			recentGain, recentReady := window.RecentGainPct(t.Symbol, current, now, confirmWindowMs)
			if !recentReady {
				continue // 数据不足，保守跳过
			}
			if side == "LONG" && recentGain < confirmThreshold {
				continue // 做多：短窗口涨幅不够
			}
			if side == "SHORT" && recentGain > -confirmThreshold {
				continue // 做空：短窗口跌幅不够
			}
		}

		// 放量确认（两种信号模式均生效）：最近 confirmWindowMs 窗口的成交量速率
		// >= 之前窗口的 N 倍才追（防无量假突破）。kline 模式同样可用：
		// 滑动窗口始终在采样，volume 数据齐全。
		if volumeSurgeThreshold > 0 && confirmWindowMs > 0 {
			// 之前窗口 = 总窗口 - 短窗口
			priorMs := window.windowMs - confirmWindowMs
			if priorMs > 0 {
				surge, surgeReady := window.RecentVolumeSurge(t.Symbol, now, confirmWindowMs, priorMs)
				if surgeReady && surge < volumeSurgeThreshold {
					continue // 成交量不够，跳过
				}
			}
		}

		candidates = append(candidates, Candidate{
			Symbol:      t.Symbol,
			GainPct:     absGain,
			QuoteVolume: t.QuoteVolume,
			Side:        side,
		})
	}

	// 按成交额降序排列（优先选择流动性最好的币种，降低滑点风险）
	sort.Slice(candidates, func(i, j int) bool {
		return candidates[i].QuoteVolume > candidates[j].QuoteVolume
	})

	// Top N 截取：只保留成交额最大的前 N 个候选
	if topN > 0 && len(candidates) > topN {
		candidates = candidates[:topN]
	}

	return candidates
}
