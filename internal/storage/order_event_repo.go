// Package storage 委托事件数据访问层
package storage

import (
	"database/sql"
)

// 委托事件类型常量
const (
	EventCreated      = "CREATED"
	EventStatusChange = "STATUS_CHANGE"
	EventTriggered    = "TRIGGERED"
	EventCanceled     = "CANCELED"
	EventSyncMismatch = "SYNC_MISMATCH"
	EventRecovery     = "RECOVERY"
)

// OrderEvent 委托事件记录
type OrderEvent struct {
	ID              int64    `json:"id"`
	OrderID         int64    `json:"orderId"`
	ExchangeOrderID int64    `json:"exchangeOrderId"`
	EventType       string   `json:"eventType"`
	OldStatus       *string  `json:"oldStatus"`
	NewStatus       *string  `json:"newStatus"`
	Price           *float64 `json:"price"`
	Message         *string  `json:"message"`
	Timestamp       int64    `json:"timestamp"`
}

// InsertOrderEvent 插入委托事件记录
// event: 待插入的事件对象
// 返回: 插入失败时返回错误
func (db *DB) InsertOrderEvent(event *OrderEvent) error {
	_, err := db.Conn.Exec(
		`INSERT INTO order_events (order_id, exchange_order_id, event_type,
		 old_status, new_status, price, message, timestamp)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		event.OrderID, event.ExchangeOrderID, event.EventType,
		event.OldStatus, event.NewStatus, event.Price,
		event.Message, event.Timestamp,
	)
	return err
}

// GetOrderEvents 查询指定委托的事件流水，按 timestamp 降序排列
// orderID: 委托 ID，传 0 时查询全部委托的事件
// limit: 返回最大条数，<=0 时默认 100
// 返回: 事件列表和可能的错误
func (db *DB) GetOrderEvents(orderID int64, limit int) ([]OrderEvent, error) {
	if limit <= 0 {
		limit = 100
	}

	var rows *sql.Rows
	var err error
	if orderID == 0 {
		rows, err = db.Conn.Query(
			`SELECT id, order_id, exchange_order_id, event_type,
			 old_status, new_status, price, message, timestamp
			 FROM order_events ORDER BY timestamp DESC LIMIT ?`, limit,
		)
	} else {
		rows, err = db.Conn.Query(
			`SELECT id, order_id, exchange_order_id, event_type,
			 old_status, new_status, price, message, timestamp
			 FROM order_events WHERE order_id = ? ORDER BY timestamp DESC LIMIT ?`,
			orderID, limit,
		)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OrderEvent
	for rows.Next() {
		e, err := scanOrderEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// GetRecentOrderEvents 查询最近的事件流水，按 timestamp 降序排列
// limit: 返回最大条数，<=0 时默认 100
// 返回: 事件列表和可能的错误
func (db *DB) GetRecentOrderEvents(limit int) ([]OrderEvent, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := db.Conn.Query(
		`SELECT id, order_id, exchange_order_id, event_type,
		 old_status, new_status, price, message, timestamp
		 FROM order_events ORDER BY timestamp DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OrderEvent
	for rows.Next() {
		e, err := scanOrderEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// GetOrderEventsByPositionID 查询指定持仓关联的所有委托事件（通过 orders 表关联）
// 参数:
//   - positionID: 持仓 ID
//   - limit: 返回最大条数，<=0 时默认 100
//
// 返回:
//   - []OrderEvent: 按 timestamp 降序排列的事件列表
//   - error: 查询失败时返回错误
func (db *DB) GetOrderEventsByPositionID(positionID int64, limit int) ([]OrderEvent, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := db.Conn.Query(
		`SELECT oe.id, oe.order_id, oe.exchange_order_id, oe.event_type,
		 oe.old_status, oe.new_status, oe.price, oe.message, oe.timestamp
		 FROM order_events oe
		 INNER JOIN orders o ON oe.order_id = o.id
		 WHERE o.position_id = ?
		 ORDER BY oe.timestamp DESC LIMIT ?`,
		positionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var events []OrderEvent
	for rows.Next() {
		e, err := scanOrderEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, e)
	}
	return events, nil
}

// scanOrderEvent 从查询结果行中扫描一条委托事件记录
// rows: 数据库查询结果集
// 返回: 扫描得到的 OrderEvent 对象和可能的错误
func scanOrderEvent(rows *sql.Rows) (OrderEvent, error) {
	var e OrderEvent
	err := rows.Scan(&e.ID, &e.OrderID, &e.ExchangeOrderID, &e.EventType,
		&e.OldStatus, &e.NewStatus, &e.Price, &e.Message, &e.Timestamp)
	return e, err
}
