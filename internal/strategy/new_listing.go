// Package strategy 新币过滤逻辑（上市天数阈值过滤）
package strategy

import "quant-desktop/internal/binance"

// ListingDays 计算合约上市天数（自然日，向下取整）。
// onboardDate: 交易所返回的上市日期（Unix 毫秒）
// now: 当前时间（Unix 毫秒）
// 返回: 上市天数；onboardDate<=0 或 now 早于上市日期（数据异常）时返回 -1
func ListingDays(onboardDate int64, now int64) int {
	if onboardDate <= 0 || now < onboardDate {
		return -1
	}
	return int((now - onboardDate) / int64(24*3600*1000))
}

// filterTickers 剔除被新币过滤拦截的交易对（仅用于传入 ScreenSliding 的列表）。
// 注意：priceMap 与滑动窗口采样仍保留全量合约，保证已有持仓（含新币上的）
// 的价格监控与止损不受影响（需求 6）。
func filterTickers(tickers []binance.Ticker, blocked map[string]bool) []binance.Ticker {
	if len(blocked) == 0 {
		return tickers
	}
	out := make([]binance.Ticker, 0, len(tickers))
	for _, t := range tickers {
		if !blocked[t.Symbol] {
			out = append(out, t)
		}
	}
	return out
}
