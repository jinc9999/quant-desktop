// Package store 授权服务数据层（MySQL 生产 / 内存开发测试）
package store

import (
	"context"
	"errors"
	"time"
)

// 客户状态
const (
	CustomerStatusDisabled = 0
	CustomerStatusActive   = 1
)

// 服务周期（标准化选项）
const (
	PeriodTrial = "3d"
	PeriodWeek   = "1w"
	PeriodMonth  = "1m"
	PeriodHalf   = "6m"
	PeriodYear   = "1y"
)

// PeriodDurations 各周期时长
var PeriodDurations = map[string]time.Duration{
	PeriodTrial: 3 * 24 * time.Hour,
	PeriodWeek:  7 * 24 * time.Hour,
	PeriodMonth: 30 * 24 * time.Hour,
	PeriodHalf:  182 * 24 * time.Hour,
	PeriodYear:  365 * 24 * time.Hour,
}

// ValidPeriod 校验服务周期
func ValidPeriod(p string) bool {
	_, ok := PeriodDurations[p]
	return ok
}

// ErrNotFound 记录不存在
var ErrNotFound = errors.New("记录不存在")

// Customer 客户
type Customer struct {
	ID           int64
	Phone        string
	PasswordHash string
	Status       int
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// Device 设备绑定（一机一号：customer_id 与 device_id 均唯一）
type Device struct {
	CustomerID int64
	DeviceID   string
	BoundAt    time.Time
	LastSeenAt time.Time
}

// Grant 服务开通/续费记录
type Grant struct {
	ID         int64
	CustomerID int64
	Period     string
	StartAt    time.Time
	EndAt      time.Time
	AdminID    int64
	CreatedAt  time.Time
}

// SMSCode 验证码记录
type SMSCode struct {
	ID        int64
	Phone     string
	Purpose   string
	CodeHash  string
	ExpiresAt time.Time
	Attempts  int
	Used      int
	CreatedAt time.Time
}

// SMSLog 短信发送日志（mock 模式记录明文验证码供联调查看）
type SMSLog struct {
	ID        int64
	Phone     string
	Code      string
	CreatedAt time.Time
}

// Session 登录会话（支持服务端撤销）
type Session struct {
	JTI        string
	CustomerID int64
	DeviceID   string
	Role       string
	ExpiresAt  time.Time
	Revoked    bool
	CreatedAt  time.Time
}

// Admin 管理员
type Admin struct {
	ID                 int64
	Username           string
	PasswordHash       string
	MustChangePassword bool
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

// AuditLog 审计日志
type AuditLog struct {
	ID         int64
	AdminID    int64
	Action     string
	TargetType string
	TargetID   string
	Detail     string
	IP         string
	CreatedAt  time.Time
}

// Store 数据层接口（mysql / memory 两套实现）
type Store interface {
	Close() error
	Ping(ctx context.Context) error

	// 客户
	GetCustomerByPhone(ctx context.Context, phone string) (*Customer, error)
	GetCustomerByID(ctx context.Context, id int64) (*Customer, error)
	CreateCustomer(ctx context.Context, phone string) (*Customer, error)
	SetCustomerPassword(ctx context.Context, id int64, hash string) error
	SetCustomerStatus(ctx context.Context, id int64, status int) error
	ListCustomers(ctx context.Context, search string, limit, offset int) ([]Customer, int, error)

	// 设备
	BindDevice(ctx context.Context, customerID int64, deviceID string) error
	GetDeviceByCustomer(ctx context.Context, customerID int64) (*Device, error)
	GetCustomerByDevice(ctx context.Context, deviceID string) (*Customer, error)
	UnbindDevice(ctx context.Context, customerID int64) error
	TouchDevice(ctx context.Context, customerID int64) error

	// 服务周期
	AddGrant(ctx context.Context, customerID int64, period string, adminID int64, startAt, endAt time.Time) error
	GetServiceUntil(ctx context.Context, customerID int64) (time.Time, error)
	ListGrants(ctx context.Context, customerID int64) ([]Grant, error)

	// 验证码
	SaveSMSCode(ctx context.Context, phone, purpose, codeHash string, expiresAt time.Time) error
	GetLatestSMSCode(ctx context.Context, phone, purpose string) (*SMSCode, error)
	MarkSMSCodeUsed(ctx context.Context, id int64) error
	IncrementSMSCodeAttempts(ctx context.Context, id int64) error
	CountSMSCodesSince(ctx context.Context, phone string, since time.Time) (int, error)

	// 短信日志（mock）
	SaveSMSLog(ctx context.Context, phone, code string) error
	ListSMSLogs(ctx context.Context, phone string, limit int) ([]SMSLog, error)

	// 会话
	SaveSession(ctx context.Context, s Session) error
	GetSession(ctx context.Context, jti string) (*Session, error)
	RevokeSession(ctx context.Context, jti string) error
	RevokeCustomerSessions(ctx context.Context, customerID int64) error

	// 管理员
	GetAdminByUsername(ctx context.Context, username string) (*Admin, error)
	GetAdminByID(ctx context.Context, id int64) (*Admin, error)
	CreateAdmin(ctx context.Context, username, passwordHash string, mustChange bool) (*Admin, error)
	UpdateAdminPassword(ctx context.Context, id int64, hash string, mustChange bool) error
	CountAdmins(ctx context.Context) (int, error)

	// 审计
	AddAuditLog(ctx context.Context, l AuditLog) error
	ListAuditLogs(ctx context.Context, limit, offset int) ([]AuditLog, int, error)
}
