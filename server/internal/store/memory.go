// 内存版数据层：本地开发（-env=dev 默认）与自动化测试使用，行为与 MySQL 版一致
package store

import (
	"context"
	"sort"
	"strings"
	"sync"
	"time"
)

// Memory 内存实现
type Memory struct {
	mu        sync.Mutex
	customers map[int64]*Customer
	devices   map[int64]*Device
	byDevice  map[string]int64
	byPhone   map[string]int64
	grants    map[int64][]Grant
	codes     []SMSCode
	smsLogs   []SMSLog
	sessions  map[string]*Session
	admins    map[int64]*Admin
	byAdmin   map[string]int64
	audit     []AuditLog
	seq       int64
}

// NewMemory 创建内存数据层
func NewMemory() *Memory {
	return &Memory{
		customers: map[int64]*Customer{},
		devices:   map[int64]*Device{},
		byDevice:  map[string]int64{},
		byPhone:   map[string]int64{},
		grants:    map[int64][]Grant{},
		sessions:  map[string]*Session{},
		admins:    map[int64]*Admin{},
		byAdmin:   map[string]int64{},
	}
}

func (m *Memory) nextID() int64 {
	m.seq++
	return m.seq
}

// Close 内存实现无需释放
func (m *Memory) Close() error { return nil }

// Ping 内存实现恒可用
func (m *Memory) Ping(ctx context.Context) error { return nil }

func (m *Memory) GetCustomerByPhone(_ context.Context, phone string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byPhone[phone]
	if !ok {
		return nil, ErrNotFound
	}
	c := *m.customers[id]
	return &c, nil
}

func (m *Memory) GetCustomerByID(_ context.Context, id int64) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *c
	return &cp, nil
}

func (m *Memory) CreateCustomer(_ context.Context, phone string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byPhone[phone]; ok {
		return nil, ErrDuplicate
	}
	now := time.Now()
	c := &Customer{
		ID:        m.nextID(),
		Phone:     phone,
		Status:    CustomerStatusActive,
		CreatedAt: now,
		UpdatedAt: now,
	}
	m.customers[c.ID] = c
	m.byPhone[phone] = c.ID
	cp := *c
	return &cp, nil
}

func (m *Memory) SetCustomerPassword(_ context.Context, id int64, hash string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok {
		return ErrNotFound
	}
	c.PasswordHash = hash
	c.UpdatedAt = time.Now()
	return nil
}

func (m *Memory) SetCustomerStatus(_ context.Context, id int64, status int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.customers[id]
	if !ok {
		return ErrNotFound
	}
	c.Status = status
	c.UpdatedAt = time.Now()
	return nil
}

func (m *Memory) ListCustomers(_ context.Context, search string, limit, offset int) ([]Customer, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var all []Customer
	for _, c := range m.customers {
		if search == "" || strings.Contains(c.Phone, search) {
			all = append(all, *c)
		}
	}
	sort.Slice(all, func(i, j int) bool { return all[i].ID > all[j].ID })
	total := len(all)
	if offset > total {
		offset = total
	}
	end := offset + limit
	if end > total {
		end = total
	}
	if limit <= 0 {
		limit = 20
	}
	return all[offset:end], total, nil
}

func (m *Memory) BindDevice(_ context.Context, customerID int64, deviceID string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if existing, ok := m.byDevice[deviceID]; ok && existing != customerID {
		return ErrDeviceTaken
	}
	now := time.Now()
	m.devices[customerID] = &Device{CustomerID: customerID, DeviceID: deviceID, BoundAt: now, LastSeenAt: now}
	m.byDevice[deviceID] = customerID
	return nil
}

func (m *Memory) GetDeviceByCustomer(_ context.Context, customerID int64) (*Device, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[customerID]
	if !ok {
		return nil, ErrNotFound
	}
	dp := *d
	return &dp, nil
}

func (m *Memory) GetCustomerByDevice(_ context.Context, deviceID string) (*Customer, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byDevice[deviceID]
	if !ok {
		return nil, ErrNotFound
	}
	c := *m.customers[id]
	return &c, nil
}

func (m *Memory) UnbindDevice(_ context.Context, customerID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[customerID]
	if !ok {
		return ErrNotFound
	}
	delete(m.byDevice, d.DeviceID)
	delete(m.devices, customerID)
	return nil
}

func (m *Memory) TouchDevice(_ context.Context, customerID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	d, ok := m.devices[customerID]
	if !ok {
		return ErrNotFound
	}
	d.LastSeenAt = time.Now()
	return nil
}

func (m *Memory) AddGrant(_ context.Context, customerID int64, period string, adminID int64, startAt, endAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.grants[customerID] = append(m.grants[customerID], Grant{
		ID: m.nextID(), CustomerID: customerID, Period: period,
		StartAt: startAt, EndAt: endAt, AdminID: adminID, CreatedAt: time.Now(),
	})
	return nil
}

func (m *Memory) GetServiceUntil(_ context.Context, customerID int64) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	grants := m.grants[customerID]
	if len(grants) == 0 {
		return time.Time{}, nil
	}
	var maxEnd time.Time
	for _, g := range grants {
		if g.EndAt.After(maxEnd) {
			maxEnd = g.EndAt
		}
	}
	return maxEnd, nil
}

func (m *Memory) ListGrants(_ context.Context, customerID int64) ([]Grant, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	grants := m.grants[customerID]
	out := make([]Grant, len(grants))
	copy(out, grants)
	return out, nil
}

func (m *Memory) SaveSMSCode(_ context.Context, phone, purpose, codeHash string, expiresAt time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.codes = append(m.codes, SMSCode{
		ID: m.nextID(), Phone: phone, Purpose: purpose, CodeHash: codeHash,
		ExpiresAt: expiresAt, CreatedAt: time.Now(),
	})
	return nil
}

func (m *Memory) GetLatestSMSCode(_ context.Context, phone, purpose string) (*SMSCode, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.codes) - 1; i >= 0; i-- {
		if m.codes[i].Phone == phone && m.codes[i].Purpose == purpose {
			c := m.codes[i]
			return &c, nil
		}
	}
	return nil, ErrNotFound
}

func (m *Memory) MarkSMSCodeUsed(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.codes {
		if m.codes[i].ID == id {
			m.codes[i].Used = 1
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) IncrementSMSCodeAttempts(_ context.Context, id int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.codes {
		if m.codes[i].ID == id {
			m.codes[i].Attempts++
			return nil
		}
	}
	return ErrNotFound
}

func (m *Memory) CountSMSCodesSince(_ context.Context, phone string, since time.Time) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, c := range m.codes {
		if c.Phone == phone && c.CreatedAt.After(since) {
			n++
		}
	}
	return n, nil
}

func (m *Memory) SaveSMSLog(_ context.Context, phone, code string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.smsLogs = append(m.smsLogs, SMSLog{ID: m.nextID(), Phone: phone, Code: code, CreatedAt: time.Now()})
	return nil
}

func (m *Memory) ListSMSLogs(_ context.Context, phone string, limit int) ([]SMSLog, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []SMSLog
	for i := len(m.smsLogs) - 1; i >= 0; i-- {
		l := m.smsLogs[i]
		if phone == "" || l.Phone == phone {
			out = append(out, l)
			if len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (m *Memory) SaveSession(_ context.Context, s Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	cp := s
	m.sessions[s.JTI] = &cp
	return nil
}

func (m *Memory) GetSession(_ context.Context, jti string) (*Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[jti]
	if !ok {
		return nil, ErrNotFound
	}
	cp := *s
	return &cp, nil
}

func (m *Memory) RevokeSession(_ context.Context, jti string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[jti]; ok {
		s.Revoked = true
	}
	return nil
}

func (m *Memory) RevokeCustomerSessions(_ context.Context, customerID int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, s := range m.sessions {
		if s.CustomerID == customerID {
			s.Revoked = true
		}
	}
	return nil
}

func (m *Memory) GetAdminByUsername(_ context.Context, username string) (*Admin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	id, ok := m.byAdmin[username]
	if !ok {
		return nil, ErrNotFound
	}
	a := *m.admins[id]
	return &a, nil
}

func (m *Memory) GetAdminByID(_ context.Context, id int64) (*Admin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.admins[id]
	if !ok {
		return nil, ErrNotFound
	}
	ap := *a
	return &ap, nil
}

func (m *Memory) CreateAdmin(_ context.Context, username, hash string, mustChange bool) (*Admin, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.byAdmin[username]; ok {
		return nil, ErrDuplicate
	}
	now := time.Now()
	a := &Admin{ID: m.nextID(), Username: username, PasswordHash: hash, MustChangePassword: mustChange, CreatedAt: now, UpdatedAt: now}
	m.admins[a.ID] = a
	m.byAdmin[username] = a.ID
	ap := *a
	return &ap, nil
}

func (m *Memory) UpdateAdminPassword(_ context.Context, id int64, hash string, mustChange bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	a, ok := m.admins[id]
	if !ok {
		return ErrNotFound
	}
	a.PasswordHash = hash
	a.MustChangePassword = mustChange
	a.UpdatedAt = time.Now()
	return nil
}

func (m *Memory) CountAdmins(_ context.Context) (int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.admins), nil
}

func (m *Memory) AddAuditLog(_ context.Context, l AuditLog) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	l.ID = m.nextID()
	l.CreatedAt = time.Now()
	m.audit = append(m.audit, l)
	return nil
}

func (m *Memory) ListAuditLogs(_ context.Context, limit, offset int) ([]AuditLog, int, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	total := len(m.audit)
	var out []AuditLog
	for i := total - 1 - offset; i >= 0 && len(out) < limit; i-- {
		out = append(out, m.audit[i])
	}
	return out, total, nil
}

// ErrDuplicate 唯一约束冲突
var ErrDuplicate = &dupError{}

type dupError struct{}

func (*dupError) Error() string { return "记录已存在" }

// ErrDeviceTaken 设备已被其他账号绑定
var ErrDeviceTaken = &deviceTakenError{}

type deviceTakenError struct{}

func (*deviceTakenError) Error() string { return "设备已被其他账号绑定" }
