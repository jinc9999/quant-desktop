// Package bindings 健康监控与自动修复
package bindings

import (
	"fmt"
	"log"
	"time"

	"quant-desktop/internal/storage"
	"quant-desktop/internal/strategy"
)

// 健康监控常量
const (
	// healthCheckInterval 健康检查周期（30 分钟）
	healthCheckInterval = 30 * time.Minute
	// tickErrorRateThreshold Tick 错误率告警阈值（超过此比例记录告警）
	tickErrorRateThreshold = 0.20
	// minTicksForRateCheck 计算错误率所需的最小 Tick 数（避免启动初期误报）
	minTicksForRateCheck = 10
)

// startHealthMonitor 启动健康监控 goroutine
// 每 30 分钟执行一次全面检查，发现异常自动修复并记录日志。
// 通过 s.ctx 取消时自动退出。
func (s *QuantService) startHealthMonitor() {
	go func() {
		ticker := time.NewTicker(healthCheckInterval)
		defer ticker.Stop()

		log.Printf("[HealthMonitor] 已启动，检查周期: %v", healthCheckInterval)

		for {
			select {
			case <-ticker.C:
				s.runHealthCheck()
			case <-s.ctx.Done():
				log.Printf("[HealthMonitor] 收到停止信号，退出")
				return
			}
		}
	}()
}

// runHealthCheck 执行一次全面健康检查
// 检查项: 引擎存活状态、Tick 错误率、WS 行情流状态
// 发现异常时自动执行预定义修复操作
func (s *QuantService) runHealthCheck() {
	s.mu.RLock()
	started := s.started
	var tickCount, tickErrorCount int64
	var engineAlive bool
	if s.engine != nil {
		tickCount = s.engine.GetTickCount()
		tickErrorCount = s.engine.GetTickErrorCount()
		engineAlive = s.engine.IsRunning()
	}
	wsHealthy := true
	if s.ws != nil {
		// 缓存非空且 60s 内更新过才算健康；冻结的旧缓存视为不健康，触发告警
		wsHealthy = len(s.ws.GetTickers()) > 0 && s.ws.CacheAge() < 60*time.Second
	}
	s.mu.RUnlock()

	log.Printf("[HealthMonitor] 开始健康检查: started=%v engineAlive=%v tick=%d errors=%d wsHealthy=%v",
		started, engineAlive, tickCount, tickErrorCount, wsHealthy)

	// 检查 1: 引擎崩溃检测与自动重启
	if started && !engineAlive {
		log.Printf("[HealthMonitor] 检测到引擎已停止但策略标记为运行中，执行自动重启")
		s.logHealthEvent("warn", "引擎异常停止，触发自动重启")
		s.restartEngine()
		return // 重启后跳过后续检查，等待下一周期
	}

	// 检查 2: Tick 错误率告警
	if tickCount >= minTicksForRateCheck {
		errorRate := float64(tickErrorCount) / float64(tickCount)
		if errorRate > tickErrorRateThreshold {
			msg := fmt.Sprintf("Tick 错误率过高: %.1f%% (%d/%d)，建议检查网络或代理配置",
				errorRate*100, tickErrorCount, tickCount)
			log.Printf("[HealthMonitor] %s", msg)
			s.logHealthEvent("warn", msg)
		}
	}

	// 检查 3: WS 行情流断连检测
	if started && !wsHealthy {
		msg := "WS 全量行情流无数据，当前回退到 REST 轮询（性能降级）"
		log.Printf("[HealthMonitor] %s", msg)
		s.logHealthEvent("warn", msg)
	}

	// 全部正常
	if started && engineAlive {
		log.Printf("[HealthMonitor] 健康检查通过: 引擎正常运行")
	}
}

// restartEngine 自动重启策略引擎
// 先标记停止，再重新创建引擎并启动，全程写入日志
func (s *QuantService) restartEngine() {
	s.mu.Lock()
	defer s.mu.Unlock()

	// 清理旧引擎
	if s.engine != nil {
		s.engine.Stop()
	}
	// 等待旧引擎 goroutine 完全退出（Stop 会取消引擎子上下文，正常应在数秒内返回）
	if !s.waitForEngineExit(10 * time.Second) {
		log.Printf("[HealthMonitor] 旧引擎在 10 秒内未完全退出，继续创建新引擎")
	}

	// 重新创建并启动引擎
	s.engine = strategy.NewEngine(s.cfg, s.client, s.ws, s.db, s.orderMgr)
	s.engineWG.Add(1)
	go func() {
		defer s.engineWG.Done()
		s.engine.Start(s.ctx)
	}()
	s.started = true

	s.logHealthEvent("info", "策略引擎已自动重启")
	log.Printf("[HealthMonitor] 引擎自动重启完成")
}

// logHealthEvent 将健康监控事件写入交易日志表
// level: 日志级别（info/warn/error）
// message: 事件描述
func (s *QuantService) logHealthEvent(level, message string) {
	if s.db == nil {
		return
	}
	s.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     level,
		Module:    "health_monitor",
		Message:   message,
	})
}
