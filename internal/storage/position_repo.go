// Package storage 持仓数据访问层
package storage

import (
	"database/sql"
	"errors"
	"time"
)

// Position 持仓记录
type Position struct {
	ID               int64    `json:"id"`
	Symbol           string   `json:"symbol"`
	Side             string   `json:"side"`
	EntryPrice       float64  `json:"entryPrice"`
	Amount           float64  `json:"amount"`
	Leverage         int      `json:"leverage"`
	HighestPrice     *float64 `json:"highestPrice"`
	TrailingActive   bool     `json:"trailingActive"`
	CurrentStopPrice float64  `json:"currentStopPrice"`
	Status           string   `json:"status"`
	OpenedAt         int64    `json:"openedAt"`
	ClosedAt         *int64   `json:"closedAt"`
	CloseReason      *string  `json:"closeReason"`
	RealizedPnl      *float64 `json:"realizedPnl"`
	ExitPrice        *float64 `json:"exitPrice"` // 出场价（平仓成交价，未平仓为 nil）
	Fee              float64  `json:"fee"`       // 平仓手续费（USDT，未平仓为 0）
}

// InsertPosition 插入持仓记录，返回 ID
func (db *DB) InsertPosition(pos *Position) (int64, error) {
	result, err := db.Conn.Exec(
		`INSERT INTO positions (symbol, side, entry_price, amount, leverage,
		 highest_price, trailing_active, current_stop_price, status, opened_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		pos.Symbol, pos.Side, pos.EntryPrice, pos.Amount, pos.Leverage,
		pos.HighestPrice, pos.TrailingActive, pos.CurrentStopPrice,
		pos.Status, pos.OpenedAt,
	)
	if err != nil {
		return 0, err
	}
	return result.LastInsertId()
}

// GetOpenPositions 获取所有 OPEN 状态的持仓
func (db *DB) GetOpenPositions() ([]Position, error) {
	rows, err := db.Conn.Query(
		`SELECT id, symbol, side, entry_price, amount, leverage,
		 highest_price, trailing_active, current_stop_price, status, opened_at
		 FROM positions WHERE status = 'OPEN' ORDER BY opened_at DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	positions := []Position{}
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.ID, &p.Symbol, &p.Side, &p.EntryPrice,
			&p.Amount, &p.Leverage, &p.HighestPrice, &p.TrailingActive,
			&p.CurrentStopPrice, &p.Status, &p.OpenedAt); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, nil
}

// ClosePosition 标记持仓为已平仓（幂等：仅 OPEN 状态可平仓）
// 参数:
//   - id: 持仓 ID
//   - reason: 平仓原因（STOP_LOSS / TRAILING_STOP / ROLLBACK 等）
//   - pnl: 已实现盈亏（USDT）
//   - exitPrice: 出场价（平仓成交价，可为 nil 表示未知）
//   - fee: 平仓手续费（USDT）
//
// 返回: error；若持仓已非 OPEN 状态，SQL 不影响任何行（无错误但无效果）
func (db *DB) ClosePosition(id int64, reason string, pnl float64, exitPrice *float64, fee float64) error {
	now := time.Now().UnixMilli()
	_, err := db.Conn.Exec(
		`UPDATE positions SET status='CLOSED', close_reason=?, realized_pnl=?, exit_price=?, fee=?, closed_at=? WHERE id=? AND status='OPEN'`,
		reason, pnl, exitPrice, fee, now, id,
	)
	return err
}

// GetPositionByID 根据 ID 查询单条持仓
// 返回: *Position 持仓记录（不存在时返回 nil, nil）, error 错误信息
func (db *DB) GetPositionByID(id int64) (*Position, error) {
	var p Position
	err := db.Conn.QueryRow(
		`SELECT id, symbol, side, entry_price, amount, leverage,
		 highest_price, trailing_active, current_stop_price, status,
		 opened_at, closed_at, close_reason, realized_pnl, exit_price, fee
		 FROM positions WHERE id=?`,
		id,
	).Scan(&p.ID, &p.Symbol, &p.Side, &p.EntryPrice,
		&p.Amount, &p.Leverage, &p.HighestPrice, &p.TrailingActive,
		&p.CurrentStopPrice, &p.Status, &p.OpenedAt, &p.ClosedAt,
		&p.CloseReason, &p.RealizedPnl, &p.ExitPrice, &p.Fee)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &p, nil
}

// UpdateRiskState 更新持仓风控状态（仅 OPEN 状态可更新）
func (db *DB) UpdateRiskState(id int64, highestPrice *float64, trailingActive bool, stopPrice float64) error {
	_, err := db.Conn.Exec(
		`UPDATE positions SET highest_price=?, trailing_active=?, current_stop_price=? WHERE id=? AND status='OPEN'`,
		highestPrice, trailingActive, stopPrice, id,
	)
	return err
}

// GetTodayPnl 聚合查询今日已平仓盈亏
// 返回: totalPnl 今日已实现盈亏总和, closedCount 今日平仓次数, err 错误信息
func (db *DB) GetTodayPnl() (float64, int, error) {
	// 计算今日零点（本地时区）的 Unix 毫秒时间戳
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	startMs := todayStart.UnixMilli()

	var totalPnl float64
	var closedCount int
	err := db.Conn.QueryRow(
		`SELECT COALESCE(SUM(realized_pnl), 0), COUNT(*) FROM positions
		 WHERE status = 'CLOSED' AND closed_at >= ?`,
		startMs,
	).Scan(&totalPnl, &closedCount)
	if err != nil {
		return 0, 0, err
	}
	return totalPnl, closedCount, nil
}

// GetTotalRealizedPnl 查询全部已实现盈亏
// 返回: 所有已平仓持仓的盈亏总额
func (db *DB) GetTotalRealizedPnl() (float64, error) {
	var pnl float64
	err := db.Conn.QueryRow(
		`SELECT COALESCE(SUM(realized_pnl),0) FROM positions WHERE status='CLOSED'`,
	).Scan(&pnl)
	return pnl, err
}

// UpdatePositionHighestPrice 更新持仓最高价（仅 OPEN 状态可更新）
// 参数:
//   - id: 持仓 ID
//   - highestPrice: 新的最高价
//
// 返回: error 更新失败时返回错误
func (db *DB) UpdatePositionHighestPrice(id int64, highestPrice float64) error {
	_, err := db.Conn.Exec(
		`UPDATE positions SET highest_price=? WHERE id=? AND status='OPEN'`,
		highestPrice, id,
	)
	return err
}

// UpdatePositionTrailingActivated 更新持仓跟踪止盈激活状态（仅 OPEN 状态可更新）
// 参数:
//   - id: 持仓 ID
//   - activated: 是否激活跟踪止盈
//
// 返回: error 更新失败时返回错误
func (db *DB) UpdatePositionTrailingActivated(id int64, activated bool) error {
	_, err := db.Conn.Exec(
		`UPDATE positions SET trailing_active=? WHERE id=? AND status='OPEN'`,
		activated, id,
	)
	return err
}

// DeletePosition 删除持仓记录
// 参数:
//   - id: 持仓 ID
//
// 返回: error 删除失败时返回错误
func (db *DB) DeletePosition(id int64) error {
	_, err := db.Conn.Exec(`DELETE FROM positions WHERE id=?`, id)
	return err
}

// UpdatePositionAmount 修正 OPEN 持仓数量（开仓结果确认：交易所真实成交数量回写）
func (db *DB) UpdatePositionAmount(id int64, amount float64) error {
	_, err := db.Conn.Exec(`UPDATE positions SET amount=? WHERE id=? AND status='OPEN'`, amount, id)
	return err
}

// GetClosedPositions 获取已平仓持仓记录（按平仓时间降序）
// limit: 返回条数上限（<=0 时默认 50）
// 返回: []Position 已平仓持仓列表, error 错误信息
func (db *DB) GetClosedPositions(limit int) ([]Position, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := db.Conn.Query(
		`SELECT id, symbol, side, entry_price, amount, leverage,
		 highest_price, trailing_active, current_stop_price, status,
		 opened_at, closed_at, close_reason, realized_pnl, exit_price, fee
		 FROM positions WHERE status = 'CLOSED' ORDER BY closed_at DESC LIMIT ?`,
		limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	positions := []Position{}
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.ID, &p.Symbol, &p.Side, &p.EntryPrice,
			&p.Amount, &p.Leverage, &p.HighestPrice, &p.TrailingActive,
			&p.CurrentStopPrice, &p.Status, &p.OpenedAt, &p.ClosedAt,
			&p.CloseReason, &p.RealizedPnl, &p.ExitPrice, &p.Fee); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, nil
}

// ClosedStats 已平仓持仓的全量汇总统计（供「已完成交易」页顶部卡片使用）
type ClosedStats struct {
	Count    int64   // 总平仓笔数（含零盈亏/对账幽灵单）
	NetPnl   float64 // 净盈亏合计（USDT）
	Wins     int64   // 盈利笔数（realized_pnl > 0）
	Losses   int64   // 亏损笔数（realized_pnl < 0）
	Zeros    int64   // 零盈亏笔数（realized_pnl = 0，多为对账幽灵单/数据缺失）
	TotalFee float64 // 手续费合计（USDT）
}

// GetClosedStats 汇总所有已平仓持仓（不做条数截断）。
// 背景：成交页此前只取最近 200 条在端上统计，卡片显示的"总"实际是窗口数，
// 与全量数据不符（如 A 库全量 745 笔 +776.61U，最近 200 笔仅 +65.57U）。
func (db *DB) GetClosedStats() (ClosedStats, error) {
	var s ClosedStats
	err := db.Conn.QueryRow(
		`SELECT COUNT(*),
		        COALESCE(SUM(realized_pnl), 0),
		        COALESCE(SUM(CASE WHEN realized_pnl > 0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN realized_pnl < 0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(CASE WHEN realized_pnl = 0 THEN 1 ELSE 0 END), 0),
		        COALESCE(SUM(fee), 0)
		 FROM positions WHERE status = 'CLOSED'`,
	).Scan(&s.Count, &s.NetPnl, &s.Wins, &s.Losses, &s.Zeros, &s.TotalFee)
	if err != nil {
		return s, err
	}
	return s, nil
}

// GetTodayClosedPositions 返回今日（本地零点起）已平仓记录，供「每日总结」使用
func (db *DB) GetTodayClosedPositions() ([]Position, error) {
	now := time.Now()
	todayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	rows, err := db.Conn.Query(
		`SELECT id, symbol, side, entry_price, amount, leverage,
		 highest_price, trailing_active, current_stop_price, status,
		 opened_at, closed_at, close_reason, realized_pnl, exit_price, fee
		 FROM positions WHERE status = 'CLOSED' AND closed_at >= ?
		 ORDER BY closed_at DESC`,
		todayStart.UnixMilli(),
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	positions := []Position{}
	for rows.Next() {
		var p Position
		if err := rows.Scan(&p.ID, &p.Symbol, &p.Side, &p.EntryPrice,
			&p.Amount, &p.Leverage, &p.HighestPrice, &p.TrailingActive,
			&p.CurrentStopPrice, &p.Status, &p.OpenedAt, &p.ClosedAt,
			&p.CloseReason, &p.RealizedPnl, &p.ExitPrice, &p.Fee); err != nil {
			return nil, err
		}
		positions = append(positions, p)
	}
	return positions, nil
}
