// MySQL 版数据层（生产环境）
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"errors"
	"fmt"
	"strings"
	"time"

	_ "github.com/go-sql-driver/mysql"
)

// MySQL 数据层
type MySQL struct {
	db *sql.DB
}

// schemaSQL 内嵌建表脚本
//go:embed schema.sql
var schemaSQL string

// NewMySQL 打开并迁移 MySQL 数据库
func NewMySQL(dsn string) (*MySQL, error) {
	if !strings.Contains(dsn, "multiStatements=") {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		dsn += sep + "multiStatements=true&parseTime=true&charset=utf8mb4"
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(20)
	db.SetMaxIdleConns(5)
	db.SetConnMaxLifetime(5 * time.Minute)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("连接 MySQL 失败（请检查 DB_DSN 与数据库是否就绪）: %w", err)
	}
	if _, err := db.ExecContext(ctx, schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("初始化数据库表失败: %w", err)
	}
	return &MySQL{db: db}, nil
}

func (m *MySQL) Close() error {
	return m.db.Close()
}

func (m *MySQL) Ping(ctx context.Context) error {
	return m.db.PingContext(ctx)
}

func scanCustomer(row interface{ Scan(...any) error }) (*Customer, error) {
	var c Customer
	if err := row.Scan(&c.ID, &c.Phone, &c.PasswordHash, &c.Status, &c.CreatedAt, &c.UpdatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	return &c, nil
}

const customerCols = "id, phone, password_hash, status, created_at, updated_at"

func (m *MySQL) GetCustomerByPhone(ctx context.Context, phone string) (*Customer, error) {
	return scanCustomer(m.db.QueryRowContext(ctx, "SELECT "+customerCols+" FROM customers WHERE phone = ?", phone))
}

func (m *MySQL) GetCustomerByID(ctx context.Context, id int64) (*Customer, error) {
	return scanCustomer(m.db.QueryRowContext(ctx, "SELECT "+customerCols+" FROM customers WHERE id = ?", id))
}

func (m *MySQL) CreateCustomer(ctx context.Context, phone string) (*Customer, error) {
	now := time.Now()
	res, err := m.db.ExecContext(ctx,
		"INSERT INTO customers (phone, password_hash, status, created_at, updated_at) VALUES (?, '', ?, ?, ?)",
		phone, CustomerStatusActive, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return m.GetCustomerByID(ctx, id)
}

func (m *MySQL) SetCustomerPassword(ctx context.Context, id int64, hash string) error {
	_, err := m.db.ExecContext(ctx, "UPDATE customers SET password_hash = ?, updated_at = ? WHERE id = ?", hash, time.Now(), id)
	return err
}

func (m *MySQL) SetCustomerStatus(ctx context.Context, id int64, status int) error {
	_, err := m.db.ExecContext(ctx, "UPDATE customers SET status = ?, updated_at = ? WHERE id = ?", status, time.Now(), id)
	return err
}

func (m *MySQL) ListCustomers(ctx context.Context, search string, limit, offset int) ([]Customer, int, error) {
	where := ""
	args := []any{}
	if search != "" {
		where = "WHERE phone LIKE ?"
		args = append(args, "%"+search+"%")
	}
	var total int
	if err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM customers "+where, args...).Scan(&total); err != nil {
		return nil, 0, err
	}
	args = append(args, limit, offset)
	rows, err := m.db.QueryContext(ctx, "SELECT "+customerCols+" FROM customers "+where+" ORDER BY id DESC LIMIT ? OFFSET ?", args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Customer
	for rows.Next() {
		c, err := scanCustomer(rows)
		if err != nil {
			return nil, 0, err
		}
		out = append(out, *c)
	}
	return out, total, rows.Err()
}

func (m *MySQL) BindDevice(ctx context.Context, customerID int64, deviceID string) error {
	now := time.Now()
	_, err := m.db.ExecContext(ctx,
		"INSERT INTO devices (customer_id, device_id, bound_at, last_seen_at) VALUES (?, ?, ?, ?)",
		customerID, deviceID, now, now)
	return err
}

func (m *MySQL) GetDeviceByCustomer(ctx context.Context, customerID int64) (*Device, error) {
	var d Device
	err := m.db.QueryRowContext(ctx,
		"SELECT customer_id, device_id, bound_at, last_seen_at FROM devices WHERE customer_id = ?", customerID).
		Scan(&d.CustomerID, &d.DeviceID, &d.BoundAt, &d.LastSeenAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func (m *MySQL) GetCustomerByDevice(ctx context.Context, deviceID string) (*Customer, error) {
	var id int64
	err := m.db.QueryRowContext(ctx, "SELECT customer_id FROM devices WHERE device_id = ?", deviceID).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m.GetCustomerByID(ctx, id)
}

func (m *MySQL) UnbindDevice(ctx context.Context, customerID int64) error {
	_, err := m.db.ExecContext(ctx, "DELETE FROM devices WHERE customer_id = ?", customerID)
	return err
}

func (m *MySQL) TouchDevice(ctx context.Context, customerID int64) error {
	_, err := m.db.ExecContext(ctx, "UPDATE devices SET last_seen_at = ? WHERE customer_id = ?", time.Now(), customerID)
	return err
}

func (m *MySQL) AddGrant(ctx context.Context, customerID int64, period string, adminID int64, startAt, endAt time.Time) error {
	_, err := m.db.ExecContext(ctx,
		"INSERT INTO service_grants (customer_id, period, start_at, end_at, admin_id, created_at) VALUES (?, ?, ?, ?, ?, ?)",
		customerID, period, startAt, endAt, adminID, time.Now())
	return err
}

func (m *MySQL) GetServiceUntil(ctx context.Context, customerID int64) (time.Time, error) {
	var end time.Time
	err := m.db.QueryRowContext(ctx,
		"SELECT COALESCE(MAX(end_at), '1970-01-01 00:00:00') FROM service_grants WHERE customer_id = ?", customerID).Scan(&end)
	if err != nil {
		return time.Time{}, err
	}
	if end.IsZero() || end.Year() <= 1970 {
		return time.Time{}, nil
	}
	return end, nil
}

func (m *MySQL) ListGrants(ctx context.Context, customerID int64) ([]Grant, error) {
	rows, err := m.db.QueryContext(ctx,
		"SELECT id, customer_id, period, start_at, end_at, admin_id, created_at FROM service_grants WHERE customer_id = ? ORDER BY id DESC",
		customerID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Grant
	for rows.Next() {
		var g Grant
		if err := rows.Scan(&g.ID, &g.CustomerID, &g.Period, &g.StartAt, &g.EndAt, &g.AdminID, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (m *MySQL) SaveSMSCode(ctx context.Context, phone, purpose, codeHash string, expiresAt time.Time) error {
	_, err := m.db.ExecContext(ctx,
		"INSERT INTO sms_codes (phone, purpose, code_hash, expires_at, created_at) VALUES (?, ?, ?, ?, ?)",
		phone, purpose, codeHash, expiresAt, time.Now())
	return err
}

func (m *MySQL) GetLatestSMSCode(ctx context.Context, phone, purpose string) (*SMSCode, error) {
	var c SMSCode
	err := m.db.QueryRowContext(ctx,
		"SELECT id, phone, purpose, code_hash, expires_at, attempts, used, created_at FROM sms_codes WHERE phone = ? AND purpose = ? ORDER BY id DESC LIMIT 1",
		phone, purpose).
		Scan(&c.ID, &c.Phone, &c.Purpose, &c.CodeHash, &c.ExpiresAt, &c.Attempts, &c.Used, &c.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func (m *MySQL) MarkSMSCodeUsed(ctx context.Context, id int64) error {
	_, err := m.db.ExecContext(ctx, "UPDATE sms_codes SET used = 1 WHERE id = ?", id)
	return err
}

func (m *MySQL) IncrementSMSCodeAttempts(ctx context.Context, id int64) error {
	_, err := m.db.ExecContext(ctx, "UPDATE sms_codes SET attempts = attempts + 1 WHERE id = ?", id)
	return err
}

func (m *MySQL) CountSMSCodesSince(ctx context.Context, phone string, since time.Time) (int, error) {
	var n int
	err := m.db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM sms_codes WHERE phone = ? AND created_at >= ?", phone, since).Scan(&n)
	return n, err
}

func (m *MySQL) SaveSMSLog(ctx context.Context, phone, code string) error {
	_, err := m.db.ExecContext(ctx, "INSERT INTO sms_logs (phone, code, created_at) VALUES (?, ?, ?)", phone, code, time.Now())
	return err
}

func (m *MySQL) ListSMSLogs(ctx context.Context, phone string, limit int) ([]SMSLog, error) {
	args := []any{limit}
	where := ""
	if phone != "" {
		where = "WHERE phone = ?"
		args = []any{phone, limit}
	}
	rows, err := m.db.QueryContext(ctx, "SELECT id, phone, code, created_at FROM sms_logs "+where+" ORDER BY id DESC LIMIT ?", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SMSLog
	for rows.Next() {
		var l SMSLog
		if err := rows.Scan(&l.ID, &l.Phone, &l.Code, &l.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (m *MySQL) SaveSession(ctx context.Context, s Session) error {
	_, err := m.db.ExecContext(ctx,
		"INSERT INTO auth_sessions (jti, customer_id, device_id, role, expires_at, revoked, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		s.JTI, s.CustomerID, s.DeviceID, s.Role, s.ExpiresAt, boolToInt(s.Revoked), s.CreatedAt)
	return err
}

func (m *MySQL) GetSession(ctx context.Context, jti string) (*Session, error) {
	var s Session
	var revoked int
	err := m.db.QueryRowContext(ctx,
		"SELECT jti, customer_id, device_id, role, expires_at, revoked, created_at FROM auth_sessions WHERE jti = ?", jti).
		Scan(&s.JTI, &s.CustomerID, &s.DeviceID, &s.Role, &s.ExpiresAt, &revoked, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	s.Revoked = revoked == 1
	return &s, nil
}

func (m *MySQL) RevokeSession(ctx context.Context, jti string) error {
	_, err := m.db.ExecContext(ctx, "UPDATE auth_sessions SET revoked = 1 WHERE jti = ?", jti)
	return err
}

func (m *MySQL) RevokeCustomerSessions(ctx context.Context, customerID int64) error {
	_, err := m.db.ExecContext(ctx, "UPDATE auth_sessions SET revoked = 1 WHERE customer_id = ? AND role = 'customer'", customerID)
	return err
}

func (m *MySQL) GetAdminByUsername(ctx context.Context, username string) (*Admin, error) {
	var a Admin
	var must int
	err := m.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, must_change_password, created_at, updated_at FROM admin_users WHERE username = ?", username).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &must, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.MustChangePassword = must == 1
	return &a, nil
}

func (m *MySQL) GetAdminByID(ctx context.Context, id int64) (*Admin, error) {
	var a Admin
	var must int
	err := m.db.QueryRowContext(ctx,
		"SELECT id, username, password_hash, must_change_password, created_at, updated_at FROM admin_users WHERE id = ?", id).
		Scan(&a.ID, &a.Username, &a.PasswordHash, &must, &a.CreatedAt, &a.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	a.MustChangePassword = must == 1
	return &a, nil
}

func (m *MySQL) CreateAdmin(ctx context.Context, username, hash string, mustChange bool) (*Admin, error) {
	now := time.Now()
	res, err := m.db.ExecContext(ctx,
		"INSERT INTO admin_users (username, password_hash, must_change_password, created_at, updated_at) VALUES (?, ?, ?, ?, ?)",
		username, hash, boolToInt(mustChange), now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Admin{ID: id, Username: username, PasswordHash: hash, MustChangePassword: mustChange, CreatedAt: now, UpdatedAt: now}, nil
}

func (m *MySQL) UpdateAdminPassword(ctx context.Context, id int64, hash string, mustChange bool) error {
	_, err := m.db.ExecContext(ctx,
		"UPDATE admin_users SET password_hash = ?, must_change_password = ?, updated_at = ? WHERE id = ?",
		hash, boolToInt(mustChange), time.Now(), id)
	return err
}

func (m *MySQL) CountAdmins(ctx context.Context) (int, error) {
	var n int
	err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM admin_users").Scan(&n)
	return n, err
}

func (m *MySQL) AddAuditLog(ctx context.Context, l AuditLog) error {
	_, err := m.db.ExecContext(ctx,
		"INSERT INTO audit_logs (admin_id, action, target_type, target_id, detail, ip, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)",
		l.AdminID, l.Action, l.TargetType, l.TargetID, l.Detail, l.IP, time.Now())
	return err
}

func (m *MySQL) ListAuditLogs(ctx context.Context, limit, offset int) ([]AuditLog, int, error) {
	var total int
	if err := m.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM audit_logs").Scan(&total); err != nil {
		return nil, 0, err
	}
	rows, err := m.db.QueryContext(ctx,
		"SELECT id, admin_id, action, target_type, target_id, detail, ip, created_at FROM audit_logs ORDER BY id DESC LIMIT ? OFFSET ?",
		limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []AuditLog
	for rows.Next() {
		var l AuditLog
		if err := rows.Scan(&l.ID, &l.AdminID, &l.Action, &l.TargetType, &l.TargetID, &l.Detail, &l.IP, &l.CreatedAt); err != nil {
			return nil, 0, err
		}
		out = append(out, l)
	}
	return out, total, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
