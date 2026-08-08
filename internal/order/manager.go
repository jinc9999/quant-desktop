// Package order 委托生命周期管理
package order

import (
	"context"
	"fmt"
	"log"
	"math"
	"math/rand"
	"sync"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/storage"
)

// Manager 委托生命周期管理器
// 职责：委托创建（含重试）、状态同步、崩溃恢复、关联取消
type Manager struct {
	client        *binance.Client
	db            *storage.DB
	maxRetries    int           // 最大重试次数，默认 3
	retryDelay    time.Duration // 初始重试间隔，默认 1s
	mu            sync.Mutex    // 保护并发同步
	lastSyncTime  int64         // 上次同步时间（Unix 毫秒）
	lastSyncError string        // 上次同步错误信息
	// OnClose 平仓回调（引擎注册，用于写冷却期）。
	// 触发时机：条件单触发平仓 / 回滚平仓 / 幽灵清理 完成 DB 关闭后，传入平仓币种。
	// 传入 reason 供引擎按平仓原因区分冷却（如移动止盈后允许快速再入）。
	// 背景：冷却期此前仅在引擎本地平仓路径写入，交易所条件单平仓（主路径）从不通知引擎，
	// 导致同一币种可无限快速重复开仓（实盘曾单币日开 40+ 次），严重偏离回测口径。
	OnClose func(symbol, reason string)
}

// NewManager 创建委托管理器
// 参数:
//   - client: 币安客户端，用于下单、撤单、查询委托状态
//   - db: 数据库实例，用于持久化委托记录和事件流水
//
// 返回:
//   - *Manager: 初始化好的 Manager（maxRetries=3, retryDelay=1s）
func NewManager(client *binance.Client, db *storage.DB) *Manager {
	return &Manager{
		client:     client,
		db:         db,
		maxRetries: 3,
		retryDelay: 1 * time.Second,
	}
}

// notifyClosed 平仓成功后通知引擎（幂等；OnClose 未注册时为空操作）
func (m *Manager) notifyClosed(symbol, reason string) {
	if m.OnClose != nil {
		m.OnClose(symbol, reason)
	}
}

// PlaceStopOrders 为持仓挂出止损+跟踪止损委托单
// 同步执行：开仓成功后立即调用，确保止损保护不依赖 Bot 存活
//
// 流程：
//  1. 幂等检查：查询该持仓是否已有活跃委托，有则跳过
//  2. 挂 STOP_MARKET 单（stopPrice = entryPrice * (1 - cfg.StopLossPct)）
//  3. 挂 TRAILING_STOP_MARKET 单（activationPrice = entryPrice * (1 + cfg.TrailingActivation)，callbackRate = cfg.TrailingCallback * 100）
//  4. 每条委托挂出后，写入 orders 表 + order_events（EventCreated）
//  5. 任一失败 → retryWithBackoff 重试
//  6. 重试仍失败 → 回滚：CloseLong 平掉仓位 + CancelRelatedOrders 取消已挂委托 + 告警日志
//
// 参数:
//   - ctx: 请求上下文
//   - pos: 持仓记录（必须已入库，有 ID）
//   - cfg: 策略配置，包含止损和跟踪止损参数
//
// 返回:
//   - error: nil 表示成功，非 nil 表示挂单或回滚过程中出现错误
func (m *Manager) PlaceStopOrders(ctx context.Context, pos *storage.Position, cfg binance.StrategyConfig) error {
	// 1. 幂等检查：该持仓是否已有活跃委托
	existingOrders, err := m.db.GetOrdersByPosition(pos.ID)
	if err != nil {
		return fmt.Errorf("查询持仓委托失败 positionID=%d: %w", pos.ID, err)
	}
	for _, o := range existingOrders {
		if o.Status == binance.OrderStatusNew || o.Status == binance.OrderStatusPartiallyFilled {
			log.Printf("[ORDER] 持仓 %d (%s) 已有活跃委托，跳过挂单", pos.ID, pos.Symbol)
			return nil
		}
	}

	// 2. 挂 STOP_MARKET 止损单（根据持仓方向计算触发价）
	var stopPrice float64
	if pos.Side == "SHORT" {
		stopPrice = pos.EntryPrice * (1 + cfg.StopLossPct) // 做空：价格上涨触发止损
	} else {
		stopPrice = pos.EntryPrice * (1 - cfg.StopLossPct) // 做多：价格下跌触发止损
	}
	var stopResult *binance.OrderResult
	err = m.retryWithBackoff(ctx, func() error {
		var e error
		stopResult, e = m.client.PlaceStopMarket(ctx, pos.Symbol, stopPrice, pos.Side)
		return e
	})
	if err != nil {
		log.Printf("[ORDER] ❌ %s 止损条件单挂出失败，触发回滚 stopPrice=%.6f: %v", pos.Symbol, stopPrice, err)
		m.rollbackPosition(ctx, pos, "止损条件单挂出失败: "+err.Error())
		return fmt.Errorf("挂止损条件单失败（已重试 %d 次）: %w", m.maxRetries, err)
	}

	// 写入止损委托记录
	now := time.Now().UnixMilli()
	stopSide := "SELL"
	if pos.Side == "SHORT" {
		stopSide = "BUY"
	}
	stopOrder := &storage.Order{
		PositionID:      pos.ID,
		ExchangeOrderID: stopResult.OrderID,
		AlgoID:          stopResult.AlgoID,
		Symbol:          pos.Symbol,
		OrderType:       binance.OrderTypeStopMarket,
		Side:            stopSide,
		Status:          stopResult.Status,
		StopPrice:       &stopPrice,
		Amount:          pos.Amount,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	stopOrderID, err := m.db.InsertOrder(stopOrder)
	if err != nil {
		return fmt.Errorf("保存止损委托记录失败: %w", err)
	}

	// 写入 EventCreated 事件
	stopNewStatus := stopResult.Status
	stopMsg := fmt.Sprintf("止损单已创建 触发价=%.6f", stopPrice)
	_ = m.db.InsertOrderEvent(&storage.OrderEvent{
		OrderID:         stopOrderID,
		ExchangeOrderID: stopResult.OrderID,
		EventType:       storage.EventCreated,
		NewStatus:       &stopNewStatus,
		Message:         &stopMsg,
		Timestamp:       now,
	})

	// 3. 挂 TRAILING_STOP_MARKET 跟踪止损单（根据持仓方向计算激活价）
	var activationPrice float64
	if pos.Side == "SHORT" {
		activationPrice = pos.EntryPrice * (1 - cfg.TrailingActivation) // 做空：价格下跌激活
	} else {
		activationPrice = pos.EntryPrice * (1 + cfg.TrailingActivation) // 做多：价格上涨激活
	}
	callbackRate := cfg.TrailingCallback * 100 // API 接受百分比数值，如 3.0 表示 3%
	var trailResult *binance.OrderResult
	err = m.retryWithBackoff(ctx, func() error {
		var e error
		trailResult, e = m.client.PlaceTrailingStop(ctx, pos.Symbol, activationPrice, callbackRate, pos.Amount, pos.Side)
		return e
	})
	if err != nil {
		log.Printf("[ORDER] ❌ %s 跟踪止损条件单挂出失败，触发回滚 activatePrice=%.6f: %v", pos.Symbol, activationPrice, err)
		m.rollbackPosition(ctx, pos, "跟踪止损条件单挂出失败: "+err.Error())
		return fmt.Errorf("挂跟踪止损条件单失败（已重试 %d 次）: %w", m.maxRetries, err)
	}

	// 写入跟踪止损委托记录
	now = time.Now().UnixMilli()
	trailSide := "SELL"
	if pos.Side == "SHORT" {
		trailSide = "BUY"
	}
	trailOrder := &storage.Order{
		PositionID:      pos.ID,
		ExchangeOrderID: trailResult.OrderID,
		AlgoID:          trailResult.AlgoID,
		Symbol:          pos.Symbol,
		OrderType:       binance.OrderTypeTrailingStop,
		Side:            trailSide,
		Status:          trailResult.Status,
		ActivationPrice: &activationPrice,
		CallbackRate:    &callbackRate,
		Amount:          pos.Amount,
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	trailOrderID, err := m.db.InsertOrder(trailOrder)
	if err != nil {
		return fmt.Errorf("保存跟踪止损委托记录失败: %w", err)
	}

	// 写入 EventCreated 事件
	trailNewStatus := trailResult.Status
	trailMsg := fmt.Sprintf("跟踪止损单已创建 激活价=%.6f 回撤=%.2f%%", activationPrice, callbackRate)
	_ = m.db.InsertOrderEvent(&storage.OrderEvent{
		OrderID:         trailOrderID,
		ExchangeOrderID: trailResult.OrderID,
		EventType:       storage.EventCreated,
		NewStatus:       &trailNewStatus,
		Message:         &trailMsg,
		Timestamp:       now,
	})

	// 3.5 挂 TAKE_PROFIT_MARKET 固定止盈单（S01 -tp 10：价格达到入场价*(1+TakeProfitPct) 先止盈）
	// 与跟踪止损先到先平：先触固定止盈价 → 止盈离场；先触发跟踪激活回撤 → 跟踪离场。
	// 止盈单失败不触发回滚（止损/跟踪止损已提供下行保护），仅记录告警日志。
	if cfg.TakeProfitPct > 0 {
		var tpPrice float64
		if pos.Side == "SHORT" {
			tpPrice = pos.EntryPrice * (1 - cfg.TakeProfitPct) // 做空：价格下跌到目标价止盈
		} else {
			tpPrice = pos.EntryPrice * (1 + cfg.TakeProfitPct) // 做多：价格上涨到目标价止盈
		}
		var tpResult *binance.OrderResult
		err = m.retryWithBackoff(ctx, func() error {
			var e error
			tpResult, e = m.client.PlaceTakeProfit(ctx, pos.Symbol, tpPrice, pos.Side)
			return e
		})
		if err != nil {
			log.Printf("[ORDER] ⚠️ %s 固定止盈条件单挂出失败（不影响持仓，跟踪止损仍保护）tpPrice=%.6f: %v", pos.Symbol, tpPrice, err)
			_ = m.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "warn",
				Module:    "order",
				Message:   fmt.Sprintf("固定止盈条件单挂出失败 %s tpPrice=%.6f: %v", pos.Symbol, tpPrice, err),
				Symbol:    pos.Symbol,
				Price:     tpPrice,
				Amount:    pos.Amount,
			})
		} else {
			// 写入固定止盈委托记录（触发价复用 StopPrice 字段）
			tpNow := time.Now().UnixMilli()
			tpSide := "SELL"
			if pos.Side == "SHORT" {
				tpSide = "BUY"
			}
			tpOrder := &storage.Order{
				PositionID:      pos.ID,
				ExchangeOrderID: tpResult.OrderID,
				AlgoID:          tpResult.AlgoID,
				Symbol:          pos.Symbol,
				OrderType:       binance.OrderTypeTakeProfit,
				Side:            tpSide,
				Status:          tpResult.Status,
				StopPrice:       &tpPrice,
				Amount:          pos.Amount,
				CreatedAt:       tpNow,
				UpdatedAt:       tpNow,
			}
			tpOrderID, err := m.db.InsertOrder(tpOrder)
			if err != nil {
				log.Printf("[ORDER] 保存固定止盈委托记录失败: %v", err)
			} else {
				tpNewStatus := tpResult.Status
				tpMsg := fmt.Sprintf("固定止盈单已创建 触发价=%.6f", tpPrice)
				_ = m.db.InsertOrderEvent(&storage.OrderEvent{
					OrderID:         tpOrderID,
					ExchangeOrderID: tpResult.OrderID,
					EventType:       storage.EventCreated,
					NewStatus:       &tpNewStatus,
					Message:         &tpMsg,
					Timestamp:       tpNow,
				})
				log.Printf("[ORDER] %s 固定止盈条件单挂出成功 持仓ID=%d 触发价=%.6f algoId=%d", pos.Symbol, pos.ID, tpPrice, tpResult.AlgoID)
			}
		}
	}

	// 4. 记录交易日志
	_ = m.db.InsertLog(&storage.TradeLog{
		Timestamp: now,
		Level:     "info",
		Module:    "order",
		Message: fmt.Sprintf("止损保护委托已挂出: 止损价=%.6f 跟踪激活价=%.6f 回撤=%.2f%%",
			stopPrice, activationPrice, callbackRate),
		Symbol: pos.Symbol,
		Price:  stopPrice,
		Amount: pos.Amount,
	})

	log.Printf("[ORDER] %s 止损保护委托挂出成功 持仓ID=%d 止损价=%.6f 跟踪激活价=%.6f",
		pos.Symbol, pos.ID, stopPrice, activationPrice)
	return nil
}

// EnsureOrdersForOpenPositions 为所有无活跃委托的 OPEN 持仓补挂止损保护
// 场景：启动时旧委托已被取消/过期，持仓仍为 OPEN 但失去保护
// 参数:
//   - ctx: 请求上下文
//   - cfg: 策略配置
func (m *Manager) EnsureOrdersForOpenPositions(ctx context.Context, cfg binance.StrategyConfig) {
	positions, err := m.db.GetOpenPositions()
	if err != nil {
		log.Printf("[ORDER] 查询 OPEN 持仓失败: %v", err)
		return
	}
	for i := range positions {
		pos := &positions[i]
		orders, err := m.db.GetOrdersByPosition(pos.ID)
		if err != nil {
			continue
		}
		hasActive := false
		for _, o := range orders {
			if o.Status == binance.OrderStatusNew || o.Status == binance.OrderStatusPartiallyFilled {
				hasActive = true
				break
			}
		}
		if !hasActive {
			log.Printf("[ORDER] %s 持仓ID=%d 无活跃委托，补挂止损保护", pos.Symbol, pos.ID)
			if err := m.PlaceStopOrders(ctx, pos, cfg); err != nil {
				log.Printf("[ORDER] %s 补挂止损失败: %v", pos.Symbol, err)
			}
		}
	}
}

// SyncOrders 同步所有活跃委托状态，执行条件单联动逻辑
// 由 Engine.runOnce 每 Tick 调用，传入本 Tick 的 priceMap（symbol→价格）
//
// 核心职责：
//  1. 跟踪止损激活检测：价格 >= 激活价 → 撤销固定止损单 + 更新持仓状态
//  2. 跟踪止损价更新：已激活时追踪最高价，动态上移止损价
//  3. 成交检测：查询交易所委托状态
//  4. 平仓闭环：成交后 ClosePosition + CancelRelatedOrders + 事件记录
//
// 参数:
//   - ctx: 请求上下文
//   - priceMap: 本 Tick 的 symbol→当前价映射
func (m *Manager) SyncOrders(ctx context.Context, priceMap map[string]float64) error {
	m.mu.Lock()
	defer m.mu.Unlock()

	orders, err := m.db.GetActiveOrders()
	if err != nil {
		m.lastSyncError = err.Error()
		return fmt.Errorf("查询活跃委托失败: %w", err)
	}

	// 按持仓分组
	posOrders := make(map[int64][]*storage.Order)
	for i := range orders {
		posOrders[orders[i].PositionID] = append(posOrders[orders[i].PositionID], &orders[i])
	}

	for posID, ords := range posOrders {
		pos, err := m.db.GetPositionByID(posID)
		if err != nil || pos == nil || pos.Status != "OPEN" {
			continue
		}

		price := priceMap[pos.Symbol]

		// 分离固定止损单和跟踪止损单
		var stopOrder, trailingOrder *storage.Order
		for _, o := range ords {
			switch o.OrderType {
			case binance.OrderTypeStopMarket:
				stopOrder = o
			case binance.OrderTypeTrailingStop:
				trailingOrder = o
			}
		}

		// === 1. 跟踪止损激活检测 → 撤固定止损 ===
		trailingActivated := false
		if trailingOrder != nil && !pos.TrailingActive &&
			trailingOrder.ActivationPrice != nil && price > 0 {
			if pos.Side == "SHORT" {
				trailingActivated = price <= *trailingOrder.ActivationPrice // 做空：价格跌到激活价
			} else {
				trailingActivated = price >= *trailingOrder.ActivationPrice // 做多：价格涨到激活价
			}
		}
		if trailingActivated {

			// 更新持仓：标记跟踪已激活
			m.db.UpdateRiskState(posID, &price, true, pos.CurrentStopPrice)
			pos.TrailingActive = true

			// 撤销固定止损单（交易所 + 本地）
			if stopOrder != nil {
				if stopOrder.AlgoID > 0 {
					m.client.CancelAlgoOrder(ctx, stopOrder.AlgoID)
				} else {
					m.client.CancelOrder(ctx, stopOrder.Symbol, stopOrder.ExchangeOrderID)
				}
				m.db.UpdateOrderStatus(stopOrder.ID, binance.OrderStatusCanceled, nil, nil)
				cancelMsg := fmt.Sprintf("跟踪止损激活(价格%.6f>=激活价%.6f)，撤销固定止损单", price, *trailingOrder.ActivationPrice)
				_ = m.db.InsertOrderEvent(&storage.OrderEvent{
					OrderID:         stopOrder.ID,
					ExchangeOrderID: stopOrder.ExchangeOrderID,
					EventType:       storage.EventCanceled,
					Message:         &cancelMsg,
					Timestamp:       time.Now().UnixMilli(),
				})
				log.Printf("[ORDER] %s 跟踪止损激活，撤销固定止损单 ExchangeOrderID=%d", pos.Symbol, stopOrder.ExchangeOrderID)
				// 关键：激活后固定止损单已撤销，置 nil 防止步骤2对其重复"撤旧挂新"，
				// 避免取消已取消的单（-2011）并在激活后重复创建新止损单
				stopOrder = nil
			}
		}

		// === 2. 跟踪止损价动态上移 + 交易所止损单同步更新 ===
		if pos.TrailingActive && price > 0 {
			callbackRate := 0.04
			if trailingOrder != nil && trailingOrder.CallbackRate != nil {
				callbackRate = *trailingOrder.CallbackRate / 100.0
			}
			var newStop float64
			shouldUpdate := false
			if pos.Side == "SHORT" {
				// 做空：跟踪最低价，止损价 = 最低价 * (1 + callback)
				newStop = price * (1 + callbackRate)
				if newStop < pos.CurrentStopPrice {
					shouldUpdate = true
				}
			} else {
				// 做多：跟踪最高价，止损价 = 最高价 * (1 - callback)
				newStop = price * (1 - callbackRate)
				if newStop > pos.CurrentStopPrice {
					shouldUpdate = true
				}
			}
			if shouldUpdate {
				m.db.UpdateRiskState(posID, &price, true, newStop)
				pos.CurrentStopPrice = newStop
				pos.HighestPrice = &price

				// 同步更新交易所止损单触发价（撤旧挂新）
				if stopOrder != nil && stopOrder.AlgoID > 0 {
					// 仅当变动超过 0.1% 时才更新，避免频繁撤挂触发限频
					oldStop := stopOrder.StopPrice
					if oldStop == nil || math.Abs(newStop-*oldStop)/newStop > 0.001 {
						result, updateErr := m.client.UpdateStopMarketPrice(ctx, stopOrder.AlgoID, pos.Symbol, newStop, pos.Side)
						if updateErr != nil {
							log.Printf("[ORDER] ❌ %s 动态更新止损价失败 algoId=%d: %v", pos.Symbol, stopOrder.AlgoID, updateErr)
						} else {
							// 更新本地委托记录的 AlgoID 和 StopPrice
							_ = m.db.UpdateOrderAlgoID(stopOrder.ID, result.AlgoID, newStop)
							stopOrder.AlgoID = result.AlgoID
							stopOrder.StopPrice = &newStop
							log.Printf("[ORDER] ✅ %s 止损价已动态更新 → %.6f newAlgoId=%d", pos.Symbol, newStop, result.AlgoID)
						}
					}
				}
			}
		}

		// === 3. 成交检测 ===
		m.syncExchangeOrders(ctx, ords)
	}

	m.lastSyncTime = time.Now().UnixMilli()
	m.lastSyncError = ""
	return nil
}

// syncExchangeOrders SIMULATION/LIVE 模式下查询交易所委托状态
// 条件单（AlgoID > 0）走 Algo Order API 查询，旧委托走标准查询接口
func (m *Manager) syncExchangeOrders(ctx context.Context, ords []*storage.Order) {
	for _, localOrder := range ords {
		var info *binance.OrderInfo
		var err error

		if localOrder.AlgoID > 0 {
			// 条件单：走 Algo Order API 查询
			info, err = m.client.GetAlgoOrderStatus(ctx, localOrder.AlgoID)
			if err != nil {
				log.Printf("[ORDER] ❌ 查询条件单状态失败 %s algoId=%d: %v",
					localOrder.Symbol, localOrder.AlgoID, err)
				continue
			}
			// 条件单触发后会生成实际委托，若 AlgoStatus 不再是 NEW 且有 ActualOrderID，
			// 则进一步查询实际委托状态以判断是否 FILLED
			if info.AlgoStatus != binance.AlgoStatusNew && info.ActualOrderID > 0 {
				actualInfo, actualErr := m.client.GetOrderStatus(ctx, localOrder.Symbol, info.ActualOrderID)
				if actualErr == nil {
					info.Status = actualInfo.Status
					info.FilledPrice = actualInfo.FilledPrice
					info.FilledAmount = actualInfo.FilledAmount
				} else {
					log.Printf("[ORDER] ⚠️ 查询条件单触发的实际委托失败 %s actualOrderID=%d: %v",
						localOrder.Symbol, info.ActualOrderID, actualErr)
				}
			}
		} else {
			// 旧委托：走标准查询接口
			info, err = m.client.GetOrderStatus(ctx, localOrder.Symbol, localOrder.ExchangeOrderID)
			if err != nil {
				log.Printf("[ORDER] ❌ 查询委托状态失败 %s ExchangeOrderID=%d: %v",
					localOrder.Symbol, localOrder.ExchangeOrderID, err)
				continue
			}
		}

		if info.Status == localOrder.Status {
			continue
		}

		var filledPrice, filledAmount *float64
		if info.FilledPrice > 0 {
			filledPrice = &info.FilledPrice
		}
		if info.FilledAmount > 0 {
			filledAmount = &info.FilledAmount
		}

		if err := m.db.UpdateOrderStatus(localOrder.ID, info.Status, filledPrice, filledAmount); err != nil {
			log.Printf("[ORDER] 更新委托状态失败 ID=%d: %v", localOrder.ID, err)
			continue
		}

		oldStatus := localOrder.Status
		newStatus := info.Status
		_ = m.db.InsertOrderEvent(&storage.OrderEvent{
			OrderID:         localOrder.ID,
			ExchangeOrderID: localOrder.ExchangeOrderID,
			EventType:       storage.EventStatusChange,
			OldStatus:       &oldStatus,
			NewStatus:       &newStatus,
			Timestamp:       time.Now().UnixMilli(),
		})

		if info.Status == binance.OrderStatusFilled {
			m.handleFilledOrder(ctx, localOrder, info)
		}
	}
}

// handleFilledOrder 处理已成交委托的平仓闭环
// 当 SyncOrders 检测到委托状态变为 FILLED 时调用
//
// 流程：
//  1. 根据 OrderType 确定平仓原因（STOP_MARKET → STOP_LOSS，TRAILING_STOP_MARKET → TRAILING_STOP）
//  2. ClosePosition 标记持仓已平仓（PnL 简化为 0，后续可扩展）
//  3. CancelRelatedOrders 取消关联的另一条委托
//  4. InsertOrderEvent(EventTriggered) 记录触发事件
//  5. InsertLog 记录平仓日志
//
// 参数:
//   - ctx: 请求上下文
//   - localOrder: 本地委托记录
//   - info: 交易所返回的最新委托详情
func (m *Manager) handleFilledOrder(ctx context.Context, localOrder *storage.Order, info *binance.OrderInfo) {
	// 根据委托类型确定平仓原因
	reason := "STOP_LOSS"
	if localOrder.OrderType == binance.OrderTypeTrailingStop {
		reason = "TRAILING_STOP"
	}
	if localOrder.OrderType == binance.OrderTypeTakeProfit {
		reason = "TAKE_PROFIT" // 固定止盈条件单触发
	}
	if localOrder.OrderType == binance.OrderTypeLimit {
		reason = "LIMIT_STOP" // 市价平仓被 PERCENT_PRICE 拒绝后的降级 LIMIT 平仓
	}

	// 计算实际 PnL：查询持仓入场价和数量（根据方向计算）
	var pnl float64
	pos, err := m.db.GetPositionByID(localOrder.PositionID)
	wasOpen := pos != nil && pos.Status == "OPEN"
	if err == nil && pos != nil && pos.Status == "OPEN" {
		if pos.Side == "SHORT" {
			pnl = (pos.EntryPrice - info.FilledPrice) * pos.Amount
		} else {
			pnl = (info.FilledPrice - pos.EntryPrice) * pos.Amount
		}
	}

	// 出场价取平仓委托成交均价
	exitPrice := info.FilledPrice

	// 查询平仓手续费（失败时降级为 0，不阻断平仓）
	fee, err := m.client.GetOrderFee(ctx, localOrder.Symbol, localOrder.ExchangeOrderID)
	if err != nil {
		log.Printf("[ORDER] 查询手续费失败 positionID=%d: %v", localOrder.PositionID, err)
		fee = 0
	}

	// 平仓（幂等：仅 OPEN 状态生效）
	if err := m.db.ClosePosition(localOrder.PositionID, reason, pnl, &exitPrice, fee); err != nil {
		log.Printf("[ORDER] 平仓更新失败 positionID=%d: %v", localOrder.PositionID, err)
	} else if wasOpen {
		m.notifyClosed(localOrder.Symbol, reason)
	}

	// 取消关联的另一条委托
	if err := m.CancelRelatedOrders(ctx, localOrder.PositionID); err != nil {
		log.Printf("[ORDER] 取消关联委托失败 positionID=%d: %v", localOrder.PositionID, err)
	}

	// 写入触发事件
	triggerMsg := fmt.Sprintf("委托触发平仓 reason=%s filledPrice=%.6f", reason, info.FilledPrice)
	_ = m.db.InsertOrderEvent(&storage.OrderEvent{
		OrderID:         localOrder.ID,
		ExchangeOrderID: localOrder.ExchangeOrderID,
		EventType:       storage.EventTriggered,
		Message:         &triggerMsg,
		Price:           &info.FilledPrice,
		Timestamp:       time.Now().UnixMilli(),
	})

	// 记录平仓日志
	_ = m.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "order",
		Message:   fmt.Sprintf("条件单触发平仓 %s reason=%s filledPrice=%.6f", localOrder.Symbol, reason, info.FilledPrice),
		Symbol:    localOrder.Symbol,
		Price:     info.FilledPrice,
		Amount:    localOrder.Amount,
	})

	log.Printf("[ORDER] %s 条件单触发平仓 持仓ID=%d reason=%s 成交价=%.6f",
		localOrder.Symbol, localOrder.PositionID, reason, info.FilledPrice)
}

// CancelRelatedOrders 取消某持仓关联的所有活跃委托
// 场景：手动平仓、止损触发后取消另一条、开仓回滚
// 取消失败仅记录日志，不中断流程
//
// 参数:
//   - ctx: 请求上下文
//   - positionID: 持仓 ID
//
// 返回:
//   - error: 查询持仓委托失败时返回错误，单个委托取消失败仅记录日志
func (m *Manager) CancelRelatedOrders(ctx context.Context, positionID int64) error {
	orders, err := m.db.GetOrdersByPosition(positionID)
	if err != nil {
		return fmt.Errorf("查询持仓委托失败 positionID=%d: %w", positionID, err)
	}

	for i := range orders {
		o := &orders[i]
		// 仅取消活跃委托
		if o.Status != binance.OrderStatusNew && o.Status != binance.OrderStatusPartiallyFilled {
			continue
		}

		// 条件单（AlgoID > 0）走 Algo Order API 取消，旧委托走标准取消接口
		var cancelErr error
		if o.AlgoID > 0 {
			cancelErr = m.client.CancelAlgoOrder(ctx, o.AlgoID)
		} else {
			cancelErr = m.client.CancelOrder(ctx, o.Symbol, o.ExchangeOrderID)
		}
		if cancelErr != nil {
			log.Printf("[ORDER] ❌ 取消委托失败 %s algoId=%d exchangeOrderID=%d: %v", o.Symbol, o.AlgoID, o.ExchangeOrderID, cancelErr)
			continue
		}

		// 更新本地状态为 CANCELED
		_ = m.db.UpdateOrderStatus(o.ID, binance.OrderStatusCanceled, nil, nil)

		oldStatus := o.Status
		newStatus := binance.OrderStatusCanceled
		cancelMsg := "关联取消"
		_ = m.db.InsertOrderEvent(&storage.OrderEvent{
			OrderID:         o.ID,
			ExchangeOrderID: o.ExchangeOrderID,
			EventType:       storage.EventCanceled,
			OldStatus:       &oldStatus,
			NewStatus:       &newStatus,
			Message:         &cancelMsg,
			Timestamp:       time.Now().UnixMilli(),
		})

		log.Printf("[ORDER] 已取消委托 %s ExchangeOrderID=%d (持仓ID=%d)", o.Symbol, o.ExchangeOrderID, positionID)
	}

	return nil
}

// RecoverOnStartup Bot 重启时与交易所对账
// 确保本地委托状态与交易所一致，处理崩溃期间的状态漂移
//
// 流程：
//  1. 查询交易所所有 OPEN 委托（client.GetOpenOrders("")）
//  2. 查询本地所有活跃委托（GetActiveOrders）
//  3. 对账：
//     - 本地有、交易所无 → 标记为 CANCELED + InsertOrderEvent(EventRecovery)
//     - 两边都有 → 同步最新状态
//  4. 记录对账结果日志
//
// 参数:
//   - ctx: 请求上下文
//
// 返回:
//   - error: 查询交易所或本地委托失败时返回错误
func (m *Manager) RecoverOnStartup(ctx context.Context) error {
	// 1. 查询交易所所有 OPEN 普通委托
	exchangeOrders, err := m.client.GetOpenOrders(ctx, "")
	if err != nil {
		return fmt.Errorf("查询交易所委托失败: %w", err)
	}

	// 1b. 查询交易所所有 OPEN 条件单（Algo Order API）
	algoOrders, algoErr := m.client.GetOpenAlgoOrders(ctx, "")
	if algoErr != nil {
		log.Printf("[ORDER] ⚠️ 启动对账：条件单查询失败，跳过条件单对账: %v", algoErr)
		algoOrders = nil
	}

	// 2. 查询本地所有活跃委托
	localOrders, err := m.db.GetActiveOrders()
	if err != nil {
		return fmt.Errorf("查询本地活跃委托失败: %w", err)
	}

	// 构建交易所委托索引（ExchangeOrderID → OrderInfo）
	exchangeMap := make(map[int64]binance.OrderInfo, len(exchangeOrders))
	for _, o := range exchangeOrders {
		exchangeMap[o.OrderID] = o
	}
	// 构建条件单索引（AlgoID → OrderInfo）
	algoMap := make(map[int64]binance.OrderInfo, len(algoOrders))
	for _, o := range algoOrders {
		algoMap[o.AlgoID] = o
	}

	// 3. 逐条对账
	syncCount := 0
	mismatchCount := 0
	for i := range localOrders {
		local := &localOrders[i]

		// 条件单（AlgoID > 0）走条件单索引匹配
		if local.AlgoID > 0 {
			if info, exists := algoMap[local.AlgoID]; exists {
				if info.Status != local.Status {
					_ = m.db.UpdateOrderStatus(local.ID, info.Status, nil, nil)
					oldStatus := local.Status
					newStatus := info.Status
					msg := "启动对账：条件单状态同步"
					_ = m.db.InsertOrderEvent(&storage.OrderEvent{
						OrderID:         local.ID,
						ExchangeOrderID: local.ExchangeOrderID,
						EventType:       storage.EventRecovery,
						OldStatus:       &oldStatus,
						NewStatus:       &newStatus,
						Message:         &msg,
						Timestamp:       time.Now().UnixMilli(),
					})
					syncCount++
				}
			} else {
				_ = m.db.UpdateOrderStatus(local.ID, binance.OrderStatusCanceled, nil, nil)
				oldStatus := local.Status
				newStatus := binance.OrderStatusCanceled
				msg := "启动对账：交易所无此条件单，标记为已取消"
				_ = m.db.InsertOrderEvent(&storage.OrderEvent{
					OrderID:         local.ID,
					ExchangeOrderID: local.ExchangeOrderID,
					EventType:       storage.EventRecovery,
					OldStatus:       &oldStatus,
					NewStatus:       &newStatus,
					Message:         &msg,
					Timestamp:       time.Now().UnixMilli(),
				})
				mismatchCount++
			}
			continue
		}

		// 旧委托走普通委托索引匹配
		if info, exists := exchangeMap[local.ExchangeOrderID]; exists {
			// 两边都有 → 同步最新状态
			if info.Status != local.Status {
				_ = m.db.UpdateOrderStatus(local.ID, info.Status, nil, nil)
				oldStatus := local.Status
				newStatus := info.Status
				msg := "启动对账：状态同步"
				_ = m.db.InsertOrderEvent(&storage.OrderEvent{
					OrderID:         local.ID,
					ExchangeOrderID: local.ExchangeOrderID,
					EventType:       storage.EventRecovery,
					OldStatus:       &oldStatus,
					NewStatus:       &newStatus,
					Message:         &msg,
					Timestamp:       time.Now().UnixMilli(),
				})
				syncCount++
			}
		} else {
			// 本地有、交易所无 → 标记为 CANCELED
			_ = m.db.UpdateOrderStatus(local.ID, binance.OrderStatusCanceled, nil, nil)
			oldStatus := local.Status
			newStatus := binance.OrderStatusCanceled
			msg := "启动对账：交易所无此委托，标记为已取消"
			_ = m.db.InsertOrderEvent(&storage.OrderEvent{
				OrderID:         local.ID,
				ExchangeOrderID: local.ExchangeOrderID,
				EventType:       storage.EventRecovery,
				OldStatus:       &oldStatus,
				NewStatus:       &newStatus,
				Message:         &msg,
				Timestamp:       time.Now().UnixMilli(),
			})
			mismatchCount++
		}
	}

	// 4. 记录对账结果日志
	summary := fmt.Sprintf("启动对账完成: 本地活跃 %d 条, 交易所普通委托 %d 条, 条件单 %d 条, 状态同步 %d 条, 标记取消 %d 条",
		len(localOrders), len(exchangeOrders), len(algoOrders), syncCount, mismatchCount)
	log.Printf("[ORDER] %s", summary)
	_ = m.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "info",
		Module:    "order",
		Message:   summary,
	})

	return nil
}

// GetSyncStatus 返回同步状态摘要
// 用于前端展示或健康检查
//
// 返回:
//   - map[string]interface{}: 包含以下键：
//     activeCount (int) — 当前活跃委托数量
//     lastSyncTime (int64) — 上次同步时间（Unix 毫秒，0 表示尚未同步）
//     lastSyncError (string) — 上次同步错误信息（空字符串表示无错误）
func (m *Manager) GetSyncStatus() map[string]interface{} {
	m.mu.Lock()
	defer m.mu.Unlock()

	activeOrders, _ := m.db.GetActiveOrders()
	return map[string]interface{}{
		"activeCount":   len(activeOrders),
		"lastSyncTime":  m.lastSyncTime,
		"lastSyncError": m.lastSyncError,
	}
}

// retryWithBackoff 指数退避重试
// 初始间隔 m.retryDelay，每次翻倍，加随机抖动（±20%），最多重试 m.maxRetries 次
// 所有 error 均触发重试
//
// 参数:
//   - ctx: 请求上下文，取消时立即停止重试
//   - fn: 要重试的操作，返回 nil 表示成功
//
// 返回:
//   - error: 全部失败时返回最后一次错误，成功时返回 nil
func (m *Manager) retryWithBackoff(ctx context.Context, fn func() error) error {
	var err error
	for attempt := 0; attempt <= m.maxRetries; attempt++ {
		if err = fn(); err == nil {
			return nil
		}

		// 最后一次尝试失败，不再等待
		if attempt >= m.maxRetries {
			break
		}

		// 计算退避时间：retryDelay * 2^attempt，加 ±20% 随机抖动
		backoff := float64(m.retryDelay) * math.Pow(2, float64(attempt))
		jitter := 1.0 + rand.Float64()*0.4 - 0.2 // [0.8, 1.2]
		waitDuration := time.Duration(backoff * jitter)

		log.Printf("[ORDER] 操作失败，第 %d/%d 次重试，等待 %v: %v", attempt+1, m.maxRetries, waitDuration, err)

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(waitDuration):
		}
	}
	return err
}

// rollbackPosition 挂单失败时的回滚操作
// 平掉仓位 + 取消已挂委托 + 写入告警日志
// 关键：平仓失败时不标记 DB 为 CLOSED，保留 OPEN 状态由孤儿扫描兜底，
// 避免交易所仍有仓位但本地已关闭的数据不一致。
//
// 参数:
//   - ctx: 请求上下文
//   - pos: 需要回滚的持仓记录
//   - reason: 回滚原因描述
func (m *Manager) rollbackPosition(ctx context.Context, pos *storage.Position, reason string) {
	log.Printf("[ORDER] ⚠️ 告警: 挂单失败回滚 %s 持仓ID=%d 原因=%s", pos.Symbol, pos.ID, reason)

	// 平掉仓位（带重试，根据方向选择平多或平空）
	closeErr := m.retryWithBackoff(ctx, func() error {
		if pos.Side == "SHORT" {
			_, err := m.client.CloseShort(ctx, pos.Symbol, pos.Amount)
			return err
		}
		_, err := m.client.CloseLong(ctx, pos.Symbol, pos.Amount)
		return err
	})

	if closeErr != nil {
		// -2022（ReduceOnly Order is rejected）：交易所已无该方向可平持仓
		// （条件单已先成交 / 开仓单实际未成交）。此时"无仓可平" = 平仓等价成功，
		// 直接标记 GHOST 关闭，避免保留 OPEN 等待孤儿扫描造成数 Tick 的不一致窗口
		// 与误导性错误日志（此前会误报"回滚平仓失败"并产生幽灵持仓）。
		if binance.IsAPIErrorCode(closeErr, -2022) {
			if err := m.CancelRelatedOrders(ctx, pos.ID); err != nil {
				log.Printf("[ORDER] 回滚取消委托失败 positionID=%d: %v", pos.ID, err)
			}
			if err := m.db.ClosePosition(pos.ID, "GHOST", 0, nil, 0); err != nil {
				log.Printf("[ORDER] 回滚标记 GHOST 失败 positionID=%d: %v", pos.ID, err)
			} else {
				m.notifyClosed(pos.Symbol, "GHOST")
			}
			log.Printf("[ORDER] 回滚平仓 -2022：交易所已无 %s 仓位，本地标记 GHOST 关闭", pos.Symbol)
			_ = m.db.InsertLog(&storage.TradeLog{
				Timestamp: time.Now().UnixMilli(),
				Level:     "warn",
				Module:    "order",
				Message:   fmt.Sprintf("回滚平仓 -2022：交易所已无 %s 仓位，标记 GHOST 关闭", pos.Symbol),
				Symbol:    pos.Symbol,
				Price:     pos.EntryPrice,
				Amount:    pos.Amount,
			})
			return
		}

		// 其他错误：不标记 DB 为 CLOSED，保留 OPEN 状态
		// 孤儿仓位扫描（scanOrphanPositions）会在后续 Tick 中发现并收养
		log.Printf("[ORDER] ❌ 回滚平仓最终失败 %s: %v（持仓保留 OPEN，等待孤儿扫描兜底）", pos.Symbol, closeErr)
		_ = m.db.InsertLog(&storage.TradeLog{
			Timestamp: time.Now().UnixMilli(),
			Level:     "error",
			Module:    "order",
			Message:   fmt.Sprintf("回滚平仓失败 %s: %v（仓位仍在交易所，等待孤儿扫描）", pos.Symbol, closeErr),
			Symbol:    pos.Symbol,
			Price:     pos.EntryPrice,
			Amount:    pos.Amount,
		})
		// 取消已挂委托（即使平仓失败，止损单也应取消，避免后续误触发）
		if err := m.CancelRelatedOrders(ctx, pos.ID); err != nil {
			log.Printf("[ORDER] 回滚取消委托失败 positionID=%d: %v", pos.ID, err)
		}
		return
	}

	// 平仓成功：取消已挂委托
	if err := m.CancelRelatedOrders(ctx, pos.ID); err != nil {
		log.Printf("[ORDER] 回滚取消委托失败 positionID=%d: %v", pos.ID, err)
	}

	// 更新持仓状态（回滚无真实成交，出场价为 nil、手续费为 0）
	if err := m.db.ClosePosition(pos.ID, "ROLLBACK", 0, nil, 0); err != nil {
		log.Printf("[ORDER] 回滚更新持仓状态失败 positionID=%d: %v", pos.ID, err)
	} else {
		m.notifyClosed(pos.Symbol, "ROLLBACK")
	}

	// 写入告警日志
	_ = m.db.InsertLog(&storage.TradeLog{
		Timestamp: time.Now().UnixMilli(),
		Level:     "error",
		Module:    "order",
		Message:   fmt.Sprintf("挂单失败回滚: %s 原因=%s", pos.Symbol, reason),
		Symbol:    pos.Symbol,
		Price:     pos.EntryPrice,
		Amount:    pos.Amount,
	})
}

// PlaceCloseLimitOrder 市价平仓被 PERCENT_PRICE 拒绝(-4131)时的降级方案：按标记价 LIMIT 挂单平仓
// 标记价为 filter 基准价，LIMIT 单必然通过价格过滤，避免止损触发后无法离场导致亏损扩大。
// 挂单成功写入本地委托记录，成交后由 SyncOrders → handleFilledOrder 自动完成平仓闭环；
// 交易所已存在平仓挂单时返回 binance.ErrCloseLimitPending（调用方应静默跳过，防止重复挂单）。
// 参数:
//   - ctx: 请求上下文
//   - pos: 待平仓的持仓记录
//
// 返回 error 错误信息（含 binance.ErrCloseLimitPending 防重复信号）
func (m *Manager) PlaceCloseLimitOrder(ctx context.Context, pos *storage.Position) error {
	var result *binance.OrderResult
	var err error
	if pos.Side == "SHORT" {
		result, err = m.client.CloseShortWithMark(ctx, pos.Symbol, pos.Amount)
	} else {
		result, err = m.client.CloseLongWithMark(ctx, pos.Symbol, pos.Amount)
	}
	if err != nil {
		return err
	}

	// 登记委托记录（成交后由 syncExchangeOrders 同步状态并触发平仓闭环）
	side := "SELL"
	if pos.Side == "SHORT" {
		side = "BUY"
	}
	now := time.Now().UnixMilli()
	_, err = m.db.InsertOrder(&storage.Order{
		PositionID:      pos.ID,
		ExchangeOrderID: result.OrderID,
		Symbol:          pos.Symbol,
		OrderType:       binance.OrderTypeLimit,
		Side:            side,
		Status:          result.Status,
		Amount:          pos.Amount,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	return err
}
