// Package storage 交易日志数据访问层
package storage

// TradeLog 交易日志记录
type TradeLog struct {
	ID         int64   `json:"id"`
	Timestamp  int64   `json:"timestamp"`
	Level      string  `json:"level"`
	Module     string  `json:"module"`
	Message    string  `json:"message"`
	Symbol     string  `json:"symbol"`
	Price      float64 `json:"price"`
	Amount     float64 `json:"amount"`
	PositionID int64   `json:"positionId"`
}

// InsertLog 插入日志记录
// 参数:
//   - log: 待插入的日志对象
//
// 返回: error 插入失败时返回错误
func (db *DB) InsertLog(log *TradeLog) error {
	_, err := db.Conn.Exec(
		`INSERT INTO trade_logs (timestamp, level, module, message, symbol, price, amount, position_id)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		log.Timestamp, log.Level, log.Module, log.Message,
		log.Symbol, log.Price, log.Amount, log.PositionID,
	)
	return err
}

// GetRecentLogs 获取最近的日志记录
// 参数:
//   - limit: 返回最大条数，<=0 时默认 100
//
// 返回:
//   - []TradeLog: 按 timestamp 降序排列的日志列表
//   - error: 查询失败时返回错误
func (db *DB) GetRecentLogs(limit int) ([]TradeLog, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := db.Conn.Query(
		`SELECT id, timestamp, level, module, message, symbol, price, amount, position_id
		 FROM trade_logs ORDER BY timestamp DESC LIMIT ?`, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []TradeLog
	for rows.Next() {
		var l TradeLog
		if err := rows.Scan(&l.ID, &l.Timestamp, &l.Level, &l.Module,
			&l.Message, &l.Symbol, &l.Price, &l.Amount, &l.PositionID); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}

// GetTradeLogsByPositionID 查询指定持仓关联的交易日志
// 参数:
//   - positionID: 持仓 ID
//   - limit: 返回最大条数，<=0 时默认 100
//
// 返回:
//   - []TradeLog: 按 timestamp 降序排列的日志列表
//   - error: 查询失败时返回错误
func (db *DB) GetTradeLogsByPositionID(positionID int64, limit int) ([]TradeLog, error) {
	if limit <= 0 {
		limit = 100
	}

	rows, err := db.Conn.Query(
		`SELECT id, timestamp, level, module, message, symbol, price, amount, position_id
		 FROM trade_logs WHERE position_id = ? ORDER BY timestamp DESC LIMIT ?`,
		positionID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var logs []TradeLog
	for rows.Next() {
		var l TradeLog
		if err := rows.Scan(&l.ID, &l.Timestamp, &l.Level, &l.Module,
			&l.Message, &l.Symbol, &l.Price, &l.Amount, &l.PositionID); err != nil {
			return nil, err
		}
		logs = append(logs, l)
	}
	return logs, nil
}
