// Package license 客户端授权管理：一机一号、服务周期、到期锁定、续费检测。
package license

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	pollInterval      = 5 * time.Minute  // 授权状态轮询周期
	expiryCheckPeriod = 30 * time.Second // 本地到期检查周期
	httpTimeout       = 10 * time.Second
)

// 错误码（与授权服务端约定）
const (
	ErrCodeOK             = 0
	ErrCodeBadRequest     = 4000
	ErrCodeUnauthorized   = 4001
	ErrCodeForbidden      = 4003
	ErrCodeDeviceBound    = 4004 // 账号已绑定其他设备
	ErrCodeCodeInvalid    = 4005 // 验证码错误/过期/尝试超限
	ErrCodeRateLimited    = 4029 // 发送/尝试过于频繁
	ErrCodeServer         = 5000
)

// apiError 服务端统一错误响应
type apiError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (e *apiError) Error() string {
	return e.Message
}

// envelope 服务端统一响应包裹
type envelope struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data"`
}

// Status 授权状态（展示用）
type Status struct {
	LoggedIn          bool   `json:"loggedIn"`
	Phone             string `json:"phone"`
	DeviceID          string `json:"deviceId"`
	ServiceUntilMS    int64  `json:"serviceUntilMs"`
	RemainingSec      int64  `json:"remainingSec"`
	Expired           bool   `json:"expired"`
	Unopened          bool   `json:"unopened"` // 已登录但尚未开通任何服务周期
	NeedsPassword     bool   `json:"needsPassword"`
	Online            bool   `json:"online"`
	ServerUnreachable bool   `json:"serverUnreachable"`
	Profile           string `json:"profile"`
	Message           string `json:"message"`
}

// LoginResult 登录结果
type LoginResult struct {
	Token          string `json:"token"`
	Phone          string `json:"phone"`
	DeviceID       string `json:"deviceId"`
	NeedsPassword  bool   `json:"needsPassword"`
	ServiceUntilMS int64  `json:"serviceUntilMs"`
	ServerNowMS    int64  `json:"serverNowMs"`
	Message        string `json:"message"`
}

// licenseData GET /license 返回体
type licenseData struct {
	Phone          string `json:"phone"`
	DeviceID       string `json:"deviceId"`
	ServiceUntilMS int64  `json:"serviceUntilMs"`
	ServerNowMS    int64  `json:"serverNowMs"`
	NeedsPassword  bool   `json:"needsPassword"`
	Message        string `json:"message"`
}

// Manager 客户端授权管理器
type Manager struct {
	baseURL  string
	deviceID string
	client   *http.Client

	mu    sync.Mutex
	cache Cache

	online      bool
	expiredFlag bool
	loggedIn    bool

	onExpired  func()
	onRenewed  func()
	onOffline  func(bool)
	onUnauthorized func()
	stopCh     chan struct{}
	started    bool
	lastErrMsg string
}

// NewManager 创建授权管理器（加载本地缓存与设备号）
func NewManager(baseURL string) (*Manager, error) {
	// 诊断/测试用：环境变量可覆盖授权服务器地址（正常使用不设置）
	if v := os.Getenv("QUANT_LICENSE_SERVER_URL"); v != "" {
		baseURL = v
	}
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://127.0.0.1:8081"
	}
	cache := LoadCache()
	m := &Manager{
		baseURL:  baseURL,
		deviceID: DeviceID(),
		client:   &http.Client{Timeout: httpTimeout},
		cache:    cache,
		stopCh:   make(chan struct{}),
	}
	m.loggedIn = cache.Token != ""
	if m.cache.Profile == "" {
		m.cache.Profile = "A"
	}
	return m, nil
}

// SetCallbacks 设置到期/续费/网络状态/凭证失效回调
func (m *Manager) SetCallbacks(onExpired, onRenewed func(), onOffline func(bool), onUnauthorized func()) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.onExpired = onExpired
	m.onRenewed = onRenewed
	m.onOffline = onOffline
	m.onUnauthorized = onUnauthorized
}

// DeviceID 返回设备唯一标识
func (m *Manager) DeviceID() string {
	return m.deviceID
}

// ServerURL 返回授权服务器地址
func (m *Manager) ServerURL() string {
	return m.baseURL
}

// IsLoggedIn 是否已登录（有本地 token）
func (m *Manager) IsLoggedIn() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.loggedIn
}

// IsExpired 是否已到期（基于校准时间）
func (m *Manager) IsExpired() bool {
	return m.Status().Expired
}

// IsUnopened 是否已登录但尚未开通服务周期
func (m *Manager) IsUnopened() bool {
	return m.Status().Unopened
}

// LastError 最近一次同步/操作错误信息
func (m *Manager) LastError() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.lastErrMsg
}

// Profile 当前 A/B 模式
func (m *Manager) Profile() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cache.Profile == "" {
		return "A"
	}
	return m.cache.Profile
}

// SetProfile 保存 A/B 模式选择（仅 C 版使用）
func (m *Manager) SetProfile(profile string) error {
	if profile != "A" && profile != "B" {
		return errors.New("模式必须是 A 或 B")
	}
	m.mu.Lock()
	m.cache.Profile = profile
	err := m.cache.Save()
	m.mu.Unlock()
	return err
}

// effectiveNow 校准当前时间（本地 + 服务器偏移）
func (m *Manager) effectiveNow() time.Time {
	return time.Now().Add(time.Duration(m.cache.ServerOffsetMS) * time.Millisecond)
}

// Status 计算当前授权状态
func (m *Manager) Status() Status {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.buildStatusLocked()
}

func (m *Manager) buildStatusLocked() Status {
	now := m.effectiveNow()
	st := Status{
		LoggedIn:          m.loggedIn,
		Phone:             m.cache.Phone,
		DeviceID:          m.deviceID,
		ServiceUntilMS:    m.cache.ServiceUntilMS,
		Online:            m.online,
		ServerUnreachable: m.loggedIn && !m.online,
		NeedsPassword:     m.cache.NeedsPassword,
		Profile:           m.cache.Profile,
	}
	if m.lastErrMsg != "" {
		st.Message = m.lastErrMsg
	}
	if m.cache.ServiceUntilMS > 0 {
		until := time.UnixMilli(m.cache.ServiceUntilMS)
		st.Expired = now.After(until)
		remaining := until.Sub(now)
		if remaining > 0 {
			st.RemainingSec = int64(remaining.Seconds())
		}
	} else if m.loggedIn {
		// 已登录但无到期时间：未开通服务。
		// 注意：不作为“到期锁定”，否则首次登录设置密码的流程会被锁屏挤掉。
		st.Unopened = true
		st.Message = "账号尚未开通服务，请联系管理员"
	}
	if st.Profile == "" {
		st.Profile = "A"
	}
	return st
}

// doJSON 发送 JSON 请求并解析统一响应
func (m *Manager) doJSON(ctx context.Context, method, path, token string, body, out any) error {
	var rdr io.Reader
	if body != nil {
		data, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(data)
	}
	req, err := http.NewRequestWithContext(ctx, method, m.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := m.client.Do(req)
	if err != nil {
		return fmt.Errorf("无法连接授权服务器: %v", err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return err
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return fmt.Errorf("授权服务器响应异常: %v", err)
	}
	if env.Code != ErrCodeOK {
		msg := env.Message
		if msg == "" {
			msg = "授权服务器返回错误"
		}
		return &apiError{Code: env.Code, Message: msg}
	}
	if out != nil && len(env.Data) > 0 {
		if err := json.Unmarshal(env.Data, out); err != nil {
			return fmt.Errorf("授权数据解析失败: %v", err)
		}
	}
	return nil
}

// SendSmsCode 发送验证码
func (m *Manager) SendSmsCode(ctx context.Context, phone string) error {
	type req struct {
		Phone string `json:"phone"`
	}
	var data struct {
		Message string `json:"message"`
	}
	err := m.doJSON(ctx, http.MethodPost, "/api/v1/sms/send", "", req{Phone: phone}, &data)
	if err != nil {
		m.mu.Lock()
		m.lastErrMsg = err.Error()
		m.mu.Unlock()
	}
	return err
}

// applyLoginResult 登录成功后写入缓存
func (m *Manager) applyLoginResult(r LoginResult) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.cache.Phone = r.Phone
	m.cache.DeviceID = m.deviceID
	m.cache.Token = r.Token
	m.cache.NeedsPassword = r.NeedsPassword
	m.cache.ServiceUntilMS = r.ServiceUntilMS
	if r.ServerNowMS > 0 {
		m.cache.ServerOffsetMS = r.ServerNowMS - time.Now().UnixMilli()
	}
	m.cache.LastSyncMS = time.Now().UnixMilli()
	m.loggedIn = r.Token != ""
	m.expiredFlag = false
	m.lastErrMsg = ""
	m.online = true
	return m.cache.Save()
}

// LoginWithCode 验证码登录/注册（首次登录自动创建账号并绑定设备）
func (m *Manager) LoginWithCode(ctx context.Context, phone, code string) (LoginResult, error) {
	type req struct {
		Phone    string `json:"phone"`
		Code     string `json:"code"`
		DeviceID string `json:"deviceId"`
	}
	var r LoginResult
	err := m.doJSON(ctx, http.MethodPost, "/api/v1/auth/login", "", req{Phone: phone, Code: code, DeviceID: m.deviceID}, &r)
	if err != nil {
		m.mu.Lock()
		m.lastErrMsg = err.Error()
		m.mu.Unlock()
		return r, err
	}
	if err := m.applyLoginResult(r); err != nil {
		return r, err
	}
	return r, nil
}

// LoginWithPassword 密码登录
func (m *Manager) LoginWithPassword(ctx context.Context, phone, password string) (LoginResult, error) {
	type req struct {
		Phone    string `json:"phone"`
		Password string `json:"password"`
		DeviceID string `json:"deviceId"`
	}
	var r LoginResult
	err := m.doJSON(ctx, http.MethodPost, "/api/v1/auth/login", "", req{Phone: phone, Password: password, DeviceID: m.deviceID}, &r)
	if err != nil {
		m.mu.Lock()
		m.lastErrMsg = err.Error()
		m.mu.Unlock()
		return r, err
	}
	if err := m.applyLoginResult(r); err != nil {
		return r, err
	}
	return r, nil
}

// SetPassword 首次登录设置密码
func (m *Manager) SetPassword(ctx context.Context, password string) error {
	m.mu.Lock()
	token := m.cache.Token
	m.mu.Unlock()
	if token == "" {
		return errors.New("请先登录")
	}
	type req struct {
		Password string `json:"password"`
	}
	if err := m.doJSON(ctx, http.MethodPost, "/api/v1/auth/password", token, req{Password: password}, nil); err != nil {
		m.mu.Lock()
		m.lastErrMsg = err.Error()
		m.mu.Unlock()
		return err
	}
	m.mu.Lock()
	m.cache.NeedsPassword = false
	err := m.cache.Save()
	m.mu.Unlock()
	return err
}

// Logout 退出登录（服务端撤销会话 + 清空本地缓存）
func (m *Manager) Logout(ctx context.Context) error {
	m.mu.Lock()
	token := m.cache.Token
	m.mu.Unlock()
	if token != "" {
		_ = m.doJSON(ctx, http.MethodPost, "/api/v1/auth/logout", token, nil, nil)
	}
	m.mu.Lock()
	m.cache = Cache{Profile: m.cache.Profile}
	m.loggedIn = false
	m.online = false
	m.lastErrMsg = ""
	err := m.cache.Save()
	m.mu.Unlock()
	return err
}

// SyncNow 立即与服务器同步授权状态（登录后启动/手动刷新/周期轮询共用）
func (m *Manager) SyncNow(ctx context.Context) (Status, error) {
	m.mu.Lock()
	token := m.cache.Token
	m.mu.Unlock()
	if token == "" {
		return m.Status(), errors.New("未登录")
	}

	var d licenseData
	err := m.doJSON(ctx, http.MethodGet, "/api/v1/license", token, nil, &d)

	m.mu.Lock()
	defer m.mu.Unlock()

	prevExpired := m.expiredFlag
	if err != nil {
		// 服务端明确拒绝凭证（401/会话失效）：清除本地登录态，回到登录页，
		// 而不是当作“服务器不可达”继续用过期凭证空转轮询。
		var apiErr *apiError
		if errors.As(err, &apiErr) && apiErr.Code == ErrCodeUnauthorized {
			m.cache = Cache{Profile: m.cache.Profile}
			m.loggedIn = false
			m.online = true
			m.lastErrMsg = "登录已失效，请重新登录"
			_ = m.cache.Save()
			if m.onUnauthorized != nil {
				go m.onUnauthorized()
			}
			return m.buildStatusLocked(), err
		}
		m.online = false
		m.lastErrMsg = "授权服务器连接异常，当前使用本地缓存，到期后需联网续期"
		st := m.buildStatusLocked()
		if m.onOffline != nil {
			m.onOffline(true)
		}
		return st, nil // 服务器不可达不视为致命错误，使用缓存兜底
	}

	m.online = true
	m.lastErrMsg = d.Message
	m.cache.Phone = d.Phone
	m.cache.ServiceUntilMS = d.ServiceUntilMS
	m.cache.NeedsPassword = d.NeedsPassword
	if d.ServerNowMS > 0 {
		m.cache.ServerOffsetMS = d.ServerNowMS - time.Now().UnixMilli()
	}
	m.cache.LastSyncMS = time.Now().UnixMilli()
	_ = m.cache.Save()

	st := m.buildStatusLocked()
	m.expiredFlag = st.Expired
	if st.Expired && !prevExpired {
		if m.onExpired != nil {
			go m.onExpired()
		}
	} else if !st.Expired && prevExpired {
		m.expiredFlag = false
		if m.onRenewed != nil {
			go m.onRenewed()
		}
	}
	if m.onOffline != nil {
		m.onOffline(false)
	}
	return st, nil
}

// Start 启动后台任务：周期同步 + 本地到期检查
func (m *Manager) Start(ctx context.Context) {
	m.mu.Lock()
	if m.started {
		m.mu.Unlock()
		return
	}
	m.started = true
	m.mu.Unlock()

	go func() {
		// 启动后先异步同步一次（有 token 才真正请求）
		if m.IsLoggedIn() {
			syncCtx, cancel := context.WithTimeout(ctx, httpTimeout)
			_, _ = m.SyncNow(syncCtx)
			cancel()
		}
		poll := time.NewTicker(pollInterval)
		check := time.NewTicker(expiryCheckPeriod)
		defer poll.Stop()
		defer check.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-m.stopCh:
				return
			case <-poll.C:
				if !m.IsLoggedIn() {
					continue
				}
				syncCtx, cancel := context.WithTimeout(ctx, httpTimeout)
				_, _ = m.SyncNow(syncCtx)
				cancel()
			case <-check.C:
				m.checkExpiryLocal()
			}
		}
	}()
}

// checkExpiryLocal 本地到期检查（不依赖网络）
func (m *Manager) checkExpiryLocal() {
	m.mu.Lock()
	st := m.buildStatusLocked()
	prev := m.expiredFlag
	m.expiredFlag = st.Expired
	onExpired := m.onExpired
	onRenewed := m.onRenewed
	m.mu.Unlock()

	if st.Expired && !prev {
		if onExpired != nil {
			go onExpired()
		}
	} else if !st.Expired && prev && st.LoggedIn {
		if onRenewed != nil {
			go onRenewed()
		}
	}
}

// Stop 停止后台任务
func (m *Manager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.started {
		select {
		case <-m.stopCh:
		default:
			close(m.stopCh)
		}
		m.started = false
	}
}
