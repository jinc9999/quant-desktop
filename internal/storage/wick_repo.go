package storage

import "time"

// WickSuggestion 插针预防建议（用户提交 + 处理状态）
type WickSuggestion struct {
	ID        int64
	Content   string
	Status    string // pending=待处理 / adopted=已采纳 / rejected=已否决
	Note      string // 处理备注
	CreatedAt int64
	UpdatedAt int64
}

const (
	WickSuggestionPending  = "pending"
	WickSuggestionAdopted  = "adopted"
	WickSuggestionRejected = "rejected"
)

// InsertWickSuggestion 提交一条插针预防建议（默认待处理）
func (db *DB) InsertWickSuggestion(content string) (int64, error) {
	now := time.Now().UnixMilli()
	res, err := db.Conn.Exec(
		`INSERT INTO wick_suggestions (content, status, note, created_at, updated_at)
		 VALUES (?, ?, '', ?, ?)`,
		content, WickSuggestionPending, now, now,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListWickSuggestions 列出全部建议（新→旧）
func (db *DB) ListWickSuggestions() ([]WickSuggestion, error) {
	rows, err := db.Conn.Query(
		`SELECT id, content, status, note, created_at, updated_at
		 FROM wick_suggestions ORDER BY id DESC`,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WickSuggestion
	for rows.Next() {
		var s WickSuggestion
		if err := rows.Scan(&s.ID, &s.Content, &s.Status, &s.Note, &s.CreatedAt, &s.UpdatedAt); err != nil {
			continue
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// UpdateWickSuggestionStatus 更新建议处理状态与备注
func (db *DB) UpdateWickSuggestionStatus(id int64, status, note string) error {
	_, err := db.Conn.Exec(
		`UPDATE wick_suggestions SET status=?, note=?, updated_at=? WHERE id=?`,
		status, note, time.Now().UnixMilli(), id,
	)
	return err
}
