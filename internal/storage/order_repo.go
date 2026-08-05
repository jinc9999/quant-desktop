// Package storage 委托数据访问层
package storage

import (
	"database/sql"
	"strings"
	"time"
)

// Order 委托记录，与 orders 表一一对应
type Order struct {
	ID              int64    `json:"id"`
	PositionID      int64    `json:"positionId"`
	ExchangeOrderID int64    `json:"exchangeOrderId"`
	AlgoID          int64    `json:"algoId"` // Algo Order API 的条件单 ID（0 表示旧接口委托）
	Symbol          string   `json:"symbol"`
	OrderType       string   `json:"orderType"`
	Side            string   `json:"side"`
	Status          string   `json:"status"`
	StopPrice       *float64 `json:"stopPrice"`
	ActivationPrice *float64 `json:"activationPrice"`
	CallbackRate    *float64 `json:"callbackRate"`
	Amount          float64  `json:"amount"`
	FilledPrice     *float64 `json:"filledPrice"`
	FilledAmount    *float64 `json:"filledAmount"`
	CreatedAt       int64    `json:"createdAt"`
	UpdatedAt       int64    `json:"updatedAt"`
}

// InsertOrder 插入委托记录，返回自增 ID
// 参数:
//   - order: 待插入的委托记录
//
// 返回:
//   - int64: 新记录的自增 ID
//   - error: 插入失败时返回错误
func (db *DB) InsertOrder(order *Order) (int64, error) {
	result, err := db.Conn.Exec(
		`INSERT INTO orders (position_id, exchange_order_id, algo_id, symbol, order_type, side,
		 status, stop_price, activation_price, callback_rate, amount,
		 filled_price, filled_amount, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		order.PositionID, order.ExchangeOrderID, order.AlgoID, order.Symbol, order.OrderType, order.Side,
		order.Status, order.StopPrice, order.ActivationPrice, order.CallbackRate, order.Amount,
		order.FilledPrice, order.FilledAmount, order.CreatedAt, order.UpdatedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetOrdersByPosition 查询某持仓关联的所有委托
// 参数:
//   - positionID: 持仓 ID
//
// 返回:
//   - []Order: 按 created_at 降序排列的委托列表
//   - error: 查询失败时返回错误
func (db *DB) GetOrdersByPosition(positionID int64) ([]Order, error) {
	rows, err := db.Conn.Query(
		`SELECT id, position_id, exchange_order_id, algo_id, symbol, order_type, side,
		 status, stop_price, activation_price, callback_rate, amount,
		 filled_price, filled_amount, created_at, updated_at
		 FROM orders WHERE position_id = ? ORDER BY created_at DESC`,
		positionID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// GetOrdersByPositionIDs 批量查询多个持仓关联的委托（单次 SQL，避免逐持仓查询产生 N+1）
// 参数:
//   - positionIDs: 持仓 ID 列表；为空时直接返回空列表，不发起查询
//
// 返回:
//   - []Order: 按 created_at 降序排列的委托列表
//   - error: 查询失败时返回错误
func (db *DB) GetOrdersByPositionIDs(positionIDs []int64) ([]Order, error) {
	if len(positionIDs) == 0 {
		return nil, nil
	}
	// 构造 IN 占位符（SQLite 单次查询参数上限默认 999，持仓规模远低于此）
	placeholders := strings.TrimSuffix(strings.Repeat("?,", len(positionIDs)), ",")
	args := make([]interface{}, len(positionIDs))
	for i, id := range positionIDs {
		args[i] = id
	}
	query := `SELECT id, position_id, exchange_order_id, algo_id, symbol, order_type, side,
		 status, stop_price, activation_price, callback_rate, amount,
		 filled_price, filled_amount, created_at, updated_at
		 FROM orders WHERE position_id IN (` + placeholders + `) ORDER BY created_at DESC`
	rows, err := db.Conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// GetActiveOrders 查询所有活跃委托（状态为 NEW 或 PARTIALLY_FILLED）
// 返回:
//   - []Order: 按 created_at 降序排列的活跃委托列表
//   - error: 查询失败时返回错误
func (db *DB) GetActiveOrders() ([]Order, error) {
	rows, err := db.Conn.Query(
		`SELECT id, position_id, exchange_order_id, algo_id, symbol, order_type, side,
		 status, stop_price, activation_price, callback_rate, amount,
		 filled_price, filled_amount, created_at, updated_at
		 FROM orders WHERE status IN ('NEW', 'PARTIALLY_FILLED') ORDER BY created_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// GetAllOrders 查询委托列表，支持按状态过滤
// 参数:
//   - statusFilter: 状态过滤条件。空字符串返回全部；"ACTIVE" 返回活跃委托；其他值按 status 精确匹配
//
// 返回:
//   - []Order: 按 created_at 降序排列的委托列表
//   - error: 查询失败时返回错误
func (db *DB) GetAllOrders(statusFilter string) ([]Order, error) {
	var query string
	var args []interface{}

	base := `SELECT id, position_id, exchange_order_id, algo_id, symbol, order_type, side,
		 status, stop_price, activation_price, callback_rate, amount,
		 filled_price, filled_amount, created_at, updated_at
		 FROM orders`

	switch statusFilter {
	case "":
		query = base + ` ORDER BY created_at DESC`
	case "ACTIVE":
		query = base + ` WHERE status IN ('NEW', 'PARTIALLY_FILLED') ORDER BY created_at DESC`
	default:
		query = base + ` WHERE status = ? ORDER BY created_at DESC`
		args = append(args, statusFilter)
	}

	rows, err := db.Conn.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var orders []Order
	for rows.Next() {
		order, err := scanOrder(rows)
		if err != nil {
			return nil, err
		}
		orders = append(orders, order)
	}
	return orders, nil
}

// UpdateOrderStatus 更新委托状态和成交信息，同时更新 updated_at 为当前时间
// 参数:
//   - id: 委托记录 ID
//   - status: 新状态值
//   - filledPrice: 成交价格（可为 nil 表示不更新）
//   - filledAmount: 成交数量（可为 nil 表示不更新）
//
// 返回:
//   - error: 更新失败时返回错误
func (db *DB) UpdateOrderStatus(id int64, status string, filledPrice, filledAmount *float64) error {
	now := time.Now().UnixMilli()
	_, err := db.Conn.Exec(
		`UPDATE orders SET status=?, filled_price=?, filled_amount=?, updated_at=? WHERE id=?`,
		status, filledPrice, filledAmount, now, id,
	)
	return err
}

// GetOrderByExchangeID 按交易所委托 ID 查询单条委托记录
// 参数:
//   - exchangeOrderID: 交易所返回的委托 ID
//
// 返回:
//   - *Order: 查询到的委托记录；未找到时返回 nil, nil
//   - error: 查询失败时返回错误
func (db *DB) GetOrderByExchangeID(exchangeOrderID int64) (*Order, error) {
	row := db.Conn.QueryRow(
		`SELECT id, position_id, exchange_order_id, algo_id, symbol, order_type, side,
		 status, stop_price, activation_price, callback_rate, amount,
		 filled_price, filled_amount, created_at, updated_at
		 FROM orders WHERE exchange_order_id = ?`,
		exchangeOrderID,
	)

	var order Order
	err := row.Scan(&order.ID, &order.PositionID, &order.ExchangeOrderID, &order.AlgoID, &order.Symbol,
		&order.OrderType, &order.Side, &order.Status, &order.StopPrice,
		&order.ActivationPrice, &order.CallbackRate, &order.Amount,
		&order.FilledPrice, &order.FilledAmount, &order.CreatedAt, &order.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &order, nil
}

// UpdateOrderAlgoID 更新委托的 AlgoID 和 StopPrice（动态上移止损价后同步）
// 参数:
//   - id: 本地委托记录 ID
//   - algoID: 新的 Algo Order ID
//   - stopPrice: 新的止损触发价
//
// 返回:
//   - error: 更新失败时返回错误
func (db *DB) UpdateOrderAlgoID(id int64, algoID int64, stopPrice float64) error {
	_, err := db.Conn.Exec(
		`UPDATE orders SET algo_id = ?, stop_price = ?, updated_at = ? WHERE id = ?`,
		algoID, stopPrice, time.Now().UnixMilli(), id,
	)
	return err
}

// scanOrder 从结果集中扫描一行数据到 Order 结构体
// 参数:
//   - rows: 数据库查询结果集
//
// 返回:
//   - Order: 扫描得到的委托记录
//   - error: 扫描失败时返回错误
func scanOrder(rows *sql.Rows) (Order, error) {
	var o Order
	err := rows.Scan(&o.ID, &o.PositionID, &o.ExchangeOrderID, &o.AlgoID, &o.Symbol,
		&o.OrderType, &o.Side, &o.Status, &o.StopPrice,
		&o.ActivationPrice, &o.CallbackRate, &o.Amount,
		&o.FilledPrice, &o.FilledAmount, &o.CreatedAt, &o.UpdatedAt)
	return o, err
}
