package storage

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"regexp"
	"time"
)

// DailySummary 每日总结记录（模拟盘/实盘按模式物理分库，mode 字段冗余记录来源）
type DailySummary struct {
	ID           int64   `json:"id"`
	Mode         string  `json:"mode"`
	SummaryDate  string  `json:"summaryDate"`  // YYYY-MM-DD
	SummaryType  string  `json:"summaryType"`  // daily / weekly
	MarketNotes  string  `json:"marketNotes"`  // 市场概况备注
	CoinAnalysis string  `json:"coinAnalysis"` // 单币盈亏分析
	Suggestions  string  `json:"suggestions"`  // 改进建议
	TodayPnl     float64 `json:"todayPnl"`
	WinRate      float64 `json:"winRate"`
	TradeCount   int     `json:"tradeCount"`
	Rating       int     `json:"rating"` // 0-10 市场/策略体验分
	FeatureJSON  string  `json:"featureJson"` // ML 特征扩展字段（JSON）
	CreatedAt    int64   `json:"createdAt"`
	UpdatedAt    int64   `json:"updatedAt"`
	DeletedAt    int64   `json:"deletedAt"`
}

var dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)

// ValidateDailySummary 校验每日总结数据格式与完整性
func ValidateDailySummary(s *DailySummary) error {
	if s == nil {
		return fmt.Errorf("每日总结为空")
	}
	if !dateRe.MatchString(s.SummaryDate) {
		return fmt.Errorf("日期格式必须为 YYYY-MM-DD: %q", s.SummaryDate)
	}
	if s.SummaryType != "daily" && s.SummaryType != "weekly" && s.SummaryType != "auto" {
		return fmt.Errorf("总结类型仅支持 daily/weekly/auto: %q", s.SummaryType)
	}
	if s.Mode != "SIMULATION" && s.Mode != "LIVE" {
		return fmt.Errorf("模式仅支持 SIMULATION/LIVE: %q", s.Mode)
	}
	if s.Rating < 0 || s.Rating > 10 {
		return fmt.Errorf("评分必须在 0-10 之间: %d", s.Rating)
	}
	if s.TradeCount < 0 {
		return fmt.Errorf("交易次数不能为负: %d", s.TradeCount)
	}
	if s.WinRate < 0 || s.WinRate > 100 {
		return fmt.Errorf("胜率必须在 0-100 之间: %.2f", s.WinRate)
	}
	if len(s.MarketNotes) > 20000 || len(s.CoinAnalysis) > 20000 || len(s.Suggestions) > 20000 {
		return fmt.Errorf("文本字段长度超出限制（单字段 ≤20000 字符）")
	}
	if s.FeatureJSON != "" {
		var m map[string]interface{}
		if err := json.Unmarshal([]byte(s.FeatureJSON), &m); err != nil {
			return fmt.Errorf("feature_json 必须为合法 JSON: %v", err)
		}
	}
	// 内容完整性：日期 + 类型 + 至少一项正文（auto=系统自动记录，无正文要求）
	if s.SummaryType != "auto" && s.MarketNotes == "" && s.CoinAnalysis == "" && s.Suggestions == "" {
		return fmt.Errorf("市场概况、单币分析、改进建议至少填写一项")
	}
	return nil
}

// SaveDailySummary 按 (mode, summary_date, summary_type) 幂等保存（存在则更新）
func (db *DB) SaveDailySummary(s *DailySummary) (int64, bool, error) {
	if err := ValidateDailySummary(s); err != nil {
		return 0, false, err
	}
	now := time.Now().UnixMilli()
	var id int64
	err := db.Conn.QueryRow(
		`SELECT id FROM daily_summaries WHERE mode=? AND summary_date=? AND summary_type=? AND deleted_at=0`,
		s.Mode, s.SummaryDate, s.SummaryType,
	).Scan(&id)
	if err != nil && err != sql.ErrNoRows {
		return 0, false, err
	}
	exists := err == nil
	if exists {
		_, err = db.Conn.Exec(
			`UPDATE daily_summaries SET market_notes=?, coin_analysis=?, suggestions=?,
			 today_pnl=?, win_rate=?, trade_count=?, rating=?, feature_json=?, updated_at=?
			 WHERE id=?`,
			s.MarketNotes, s.CoinAnalysis, s.Suggestions, s.TodayPnl, s.WinRate,
			s.TradeCount, s.Rating, s.FeatureJSON, now, id,
		)
	} else {
		res, ierr := db.Conn.Exec(
			`INSERT INTO daily_summaries
			 (mode, summary_date, summary_type, market_notes, coin_analysis, suggestions,
			  today_pnl, win_rate, trade_count, rating, feature_json, created_at, updated_at)
			 VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			s.Mode, s.SummaryDate, s.SummaryType, s.MarketNotes, s.CoinAnalysis, s.Suggestions,
			s.TodayPnl, s.WinRate, s.TradeCount, s.Rating, s.FeatureJSON, now, now,
		)
		if ierr != nil {
			return 0, false, ierr
		}
		id, _ = res.LastInsertId()
		return id, false, nil
	}
	return id, exists, err
}

// GetDailySummaries 按时间范围与类型查询（默认返回当前模式，deleted_at=0）
func (db *DB) GetDailySummaries(mode, dateFrom, dateTo, summaryType string) ([]DailySummary, error) {
	q := `SELECT id, mode, summary_date, summary_type, market_notes, coin_analysis, suggestions,
		 today_pnl, win_rate, trade_count, rating, feature_json, created_at, updated_at, deleted_at
		 FROM daily_summaries WHERE mode=? AND deleted_at=0`
	args := []interface{}{mode}
	if dateFrom != "" {
		q += ` AND summary_date >= ?`
		args = append(args, dateFrom)
	}
	if dateTo != "" {
		q += ` AND summary_date <= ?`
		args = append(args, dateTo)
	}
	if summaryType != "" && summaryType != "all" {
		q += ` AND summary_type = ?`
		args = append(args, summaryType)
	}
	q += ` ORDER BY summary_date DESC, id DESC`
	rows, err := db.Conn.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []DailySummary{}
	for rows.Next() {
		var s DailySummary
		if err := rows.Scan(&s.ID, &s.Mode, &s.SummaryDate, &s.SummaryType, &s.MarketNotes,
			&s.CoinAnalysis, &s.Suggestions, &s.TodayPnl, &s.WinRate, &s.TradeCount,
			&s.Rating, &s.FeatureJSON, &s.CreatedAt, &s.UpdatedAt, &s.DeletedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// GetDailySummaryByID 按 ID 查询（限当前模式）
func (db *DB) GetDailySummaryByID(mode string, id int64) (*DailySummary, error) {
	var s DailySummary
	err := db.Conn.QueryRow(
		`SELECT id, mode, summary_date, summary_type, market_notes, coin_analysis, suggestions,
		 today_pnl, win_rate, trade_count, rating, feature_json, created_at, updated_at, deleted_at
		 FROM daily_summaries WHERE id=? AND mode=? AND deleted_at=0`,
		id, mode,
	).Scan(&s.ID, &s.Mode, &s.SummaryDate, &s.SummaryType, &s.MarketNotes,
		&s.CoinAnalysis, &s.Suggestions, &s.TodayPnl, &s.WinRate, &s.TradeCount,
		&s.Rating, &s.FeatureJSON, &s.CreatedAt, &s.UpdatedAt, &s.DeletedAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// DeleteDailySummary 软删除（deleted_at 置位，保留历史可审计）
func (db *DB) DeleteDailySummary(mode string, id int64) (bool, error) {
	res, err := db.Conn.Exec(
		`UPDATE daily_summaries SET deleted_at=? WHERE id=? AND mode=? AND deleted_at=0`,
		time.Now().UnixMilli(), id, mode,
	)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// InsertAuditLog 写入数据操作审计日志
func (db *DB) InsertAuditLog(mode, action, target, detail string) error {
	_, err := db.Conn.Exec(
		`INSERT INTO data_audit_log (mode, action, target, detail, created_at) VALUES (?,?,?,?,?)`,
		mode, action, target, detail, time.Now().UnixMilli(),
	)
	return err
}
