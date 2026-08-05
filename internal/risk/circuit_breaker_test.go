// Package risk 熔断器单元测试
package risk

import "testing"

// TestCheckDailyLoss_Triggered 验证日亏损达到阈值时触发熔断
// 初始权益 1000，dailyLossLimit=0.05，亏损 50 即 5% 达到阈值
func TestCheckDailyLoss_Triggered(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)
	cb.SetInitialEquity(1000)

	if got := cb.CheckDailyLoss(-50); !got {
		t.Errorf("CheckDailyLoss(-50) = false, 期望 true")
	}
}

// TestCheckDailyLoss_NotTriggered 验证日亏损未达阈值时不触发熔断
// 初始权益 1000，dailyLossLimit=0.05，亏损 40 即 4% 未达阈值；盈利也不触发
func TestCheckDailyLoss_NotTriggered(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)
	cb.SetInitialEquity(1000)

	if got := cb.CheckDailyLoss(-40); got {
		t.Errorf("CheckDailyLoss(-40) = true, 期望 false")
	}
	if got := cb.CheckDailyLoss(100); got {
		t.Errorf("CheckDailyLoss(100) = true, 期望 false（盈利不应触发）")
	}
}

// TestCheckDrawdown_Triggered 验证权益回撤达到阈值时触发账户熔断
// 初始权益 1000，maxDrawdown=0.20，当前权益 800 即回撤 20% 达到阈值
func TestCheckDrawdown_Triggered(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)
	cb.SetInitialEquity(1000)

	if got := cb.CheckDrawdown(800); !got {
		t.Errorf("CheckDrawdown(800) = false, 期望 true")
	}
}

// TestCheckDrawdown_NotTriggered 验证权益回撤未达阈值时不触发熔断
// 初始权益 1000，maxDrawdown=0.20，当前权益 850 即回撤 15% 未达阈值
func TestCheckDrawdown_NotTriggered(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)
	cb.SetInitialEquity(1000)

	if got := cb.CheckDrawdown(850); got {
		t.Errorf("CheckDrawdown(850) = true, 期望 false")
	}
}

// TestRecordOHLCVFailure 验证连续失败计数达到阈值时返回 true
// ohlcvThreshold=3，前两次返回 false，第三次返回 true
func TestRecordOHLCVFailure(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)

	if cb.RecordOHLCVFailure() {
		t.Errorf("第 1 次失败 RecordOHLCVFailure = true, 期望 false")
	}
	if cb.RecordOHLCVFailure() {
		t.Errorf("第 2 次失败 RecordOHLCVFailure = true, 期望 false")
	}
	if !cb.RecordOHLCVFailure() {
		t.Errorf("第 3 次失败 RecordOHLCVFailure = false, 期望 true")
	}
}

// TestResetOHLCVFailures 验证重置后失败计数归零
// 累计两次失败后重置，再次失败应重新从 1 计数，未达阈值返回 false
func TestResetOHLCVFailures(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)

	cb.RecordOHLCVFailure()
	cb.RecordOHLCVFailure()
	cb.ResetOHLCVFailures()

	if cb.RecordOHLCVFailure() {
		t.Errorf("重置后第 1 次失败 RecordOHLCVFailure = true, 期望 false")
	}
}

// TestIsBlocked 验证日熔断或账户熔断后 IsBlocked 返回 true
// 覆盖: 初始未熔断、日熔断后、账户熔断后三种状态
func TestIsBlocked(t *testing.T) {
	// 初始状态未熔断
	cb := NewCircuitBreaker(0.05, 0.20, 3)
	cb.SetInitialEquity(1000)
	if cb.IsBlocked() {
		t.Errorf("初始 IsBlocked = true, 期望 false")
	}

	// 日熔断后阻塞
	cbDaily := NewCircuitBreaker(0.05, 0.20, 3)
	cbDaily.SetInitialEquity(1000)
	cbDaily.CheckDailyLoss(-50)
	if !cbDaily.IsBlocked() {
		t.Errorf("日熔断后 IsBlocked = false, 期望 true")
	}

	// 账户熔断后阻塞
	cbAccount := NewCircuitBreaker(0.05, 0.20, 3)
	cbAccount.SetInitialEquity(1000)
	cbAccount.CheckDrawdown(800)
	if !cbAccount.IsBlocked() {
		t.Errorf("账户熔断后 IsBlocked = false, 期望 true")
	}
}

// TestResetDaily 验证重置日熔断后 IsBlocked 反映正确状态
// 日熔断触发后 IsBlocked=true，ResetDaily 后（账户未熔断）IsBlocked=false
func TestResetDaily(t *testing.T) {
	cb := NewCircuitBreaker(0.05, 0.20, 3)
	cb.SetInitialEquity(1000)

	cb.CheckDailyLoss(-50)
	if !cb.IsBlocked() {
		t.Fatalf("日熔断后 IsBlocked = false, 期望 true")
	}

	cb.ResetDaily()
	if cb.IsBlocked() {
		t.Errorf("ResetDaily 后 IsBlocked = true, 期望 false")
	}
}
