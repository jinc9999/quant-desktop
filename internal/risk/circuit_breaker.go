// Package risk 熔断器
package risk

import (
	"sync"
	"time"
)

// BreakerType 熔断类型
type BreakerType string

const (
	BreakerDailyLoss   BreakerType = "DAILY_LOSS"
	BreakerMaxDrawdown BreakerType = "MAX_DRAWDOWN"
	BreakerOHLCV       BreakerType = "OHLCV_FAILURE"
)

// CircuitBreaker 熔断器
type CircuitBreaker struct {
	mu             sync.RWMutex
	dailyActive    bool
	accountActive  bool
	ohlcvFailures  int
	ohlcvThreshold int
	dailyLossLimit float64
	maxDrawdown    float64
	initialEquity  float64
	triggeredAt    time.Time
}

// NewCircuitBreaker 创建熔断器
func NewCircuitBreaker(dailyLossLimit, maxDrawdown float64, ohlcvThreshold int) *CircuitBreaker {
	return &CircuitBreaker{
		dailyLossLimit: dailyLossLimit,
		maxDrawdown:    maxDrawdown,
		ohlcvThreshold: ohlcvThreshold,
	}
}

// SetInitialEquity 设置初始权益
func (cb *CircuitBreaker) SetInitialEquity(equity float64) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.initialEquity = equity
}

// CheckDailyLoss 检查日亏损熔断
// currentPnl: 当日已实现盈亏
func (cb *CircuitBreaker) CheckDailyLoss(currentPnl float64) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.dailyActive {
		return true
	}

	if cb.initialEquity > 0 && currentPnl < 0 {
		lossPct := -currentPnl / cb.initialEquity
		if lossPct >= cb.dailyLossLimit {
			cb.dailyActive = true
			cb.triggeredAt = time.Now()
			return true
		}
	}
	return false
}

// CheckDrawdown 检查账户回撤熔断
// currentEquity: 当前权益
func (cb *CircuitBreaker) CheckDrawdown(currentEquity float64) bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()

	if cb.accountActive {
		return true
	}

	if cb.initialEquity > 0 && currentEquity > 0 {
		drawdown := (cb.initialEquity - currentEquity) / cb.initialEquity
		if drawdown >= cb.maxDrawdown {
			cb.accountActive = true
			cb.triggeredAt = time.Now()
			return true
		}
	}
	return false
}

// RecordOHLCVFailure 记录 OHLCV 拉取失败
func (cb *CircuitBreaker) RecordOHLCVFailure() bool {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.ohlcvFailures++
	return cb.ohlcvFailures >= cb.ohlcvThreshold
}

// ResetOHLCVFailures 重置 OHLCV 失败计数
func (cb *CircuitBreaker) ResetOHLCVFailures() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.ohlcvFailures = 0
}

// IsBlocked 返回是否被任何熔断阻塞
func (cb *CircuitBreaker) IsBlocked() bool {
	cb.mu.RLock()
	defer cb.mu.RUnlock()
	return cb.dailyActive || cb.accountActive
}

// ResetDaily 重置日熔断（新的一天）
func (cb *CircuitBreaker) ResetDaily() {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	cb.dailyActive = false
}
