package strategy

// SmartSizeMultiplier 智慧版仓位倍数：按当前 15m 周期内最大 5m 收盘涨幅分桶。
//
//	>=boundary（默认 2.5%）爆拉   → ×high（默认 1.5）
//	2%~boundary 较强             → ×1.0（均仓）
//	<2% 温和                    → ×low（默认 0.7）
//
// 回测验证（2026-08-13，A 骨架 + 滑点 0.1 + 手续费 0.05%）：
// 全周期 +36%（+30,752U → +41,949U）、2024 选参 +27%、2025-26 样本外 +39%、
// 扰动 6 组无悬崖。5m 爆拉是实测有效的质量因子（暴力单笔 +1.01U vs 温和 +0.45U）。
// 注意：g5m<0（无数据/拉取失败）按均仓处理，宁可少赚不放大风险。
func SmartSizeMultiplier(gain5m, high, low, boundary float64) float64 {
	if gain5m < 0 {
		return 1
	}
	if high <= 0 {
		high = 1.5
	}
	if low <= 0 {
		low = 0.7
	}
	if boundary <= 0 {
		boundary = 2.5
	}
	switch {
	case gain5m >= boundary:
		return high
	case gain5m >= 2:
		return 1.0
	default:
		return low
	}
}
