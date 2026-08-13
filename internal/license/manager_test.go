package license

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// testEnv 模拟授权服务端
type testEnv struct {
	ts            *httptest.Server
	serviceUntil  int64
	serverNow     int64
	mu            sync.Mutex
}

func newTestEnv(t *testing.T, serviceUntil int64) *testEnv {
	t.Helper()
	e := &testEnv{serviceUntil: serviceUntil, serverNow: time.Now().UnixMilli()}
	e.ts = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		e.mu.Lock()
		until := e.serviceUntil
		now := e.serverNow
		e.mu.Unlock()
		switch r.URL.Path {
		case "/api/v1/auth/login":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]any{
					"token": "test-token", "phone": "13800138000", "deviceId": "test-device",
					"needsPassword": false, "serviceUntilMs": until, "serverNowMs": now,
				},
			})
		case "/api/v1/license":
			_ = json.NewEncoder(w).Encode(map[string]any{
				"code": 0, "message": "ok",
				"data": map[string]any{
					"phone": "13800138000", "deviceId": "test-device",
					"serviceUntilMs": until, "serverNowMs": now, "needsPassword": false,
					"message": "服务正常",
				},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
			_ = json.NewEncoder(w).Encode(map[string]any{"code": 4040, "message": "not found"})
		}
	}))
	t.Cleanup(e.ts.Close)
	return e
}

func newTestManager(t *testing.T, url string) *Manager {
	t.Helper()
	dir := t.TempDir()
	cacheDirFn = func() string { return dir }
	m, err := NewManager(url)
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestLoginAndStatus(t *testing.T) {
	until := time.Now().Add(7 * 24 * time.Hour).UnixMilli()
	env := newTestEnv(t, until)
	m := newTestManager(t, env.ts.URL)

	result, err := m.LoginWithCode(context.Background(), "13800138000", "123456")
	if err != nil {
		t.Fatalf("登录失败: %v", err)
	}
	if result.Token == "" || result.ServiceUntilMS != until {
		t.Fatalf("登录结果异常: %+v", result)
	}
	if !m.IsLoggedIn() {
		t.Fatal("应处于已登录状态")
	}
	st := m.Status()
	if !st.LoggedIn || st.Expired || st.RemainingSec <= 0 {
		t.Fatalf("状态异常: %+v", st)
	}
}

func TestExpiryTriggersCallback(t *testing.T) {
	expired := time.Now().Add(-time.Hour).UnixMilli()
	env := newTestEnv(t, expired)
	m := newTestManager(t, env.ts.URL)

	called := make(chan string, 2)
	m.SetCallbacks(func() { called <- "expired" }, func() { called <- "renewed" }, nil, nil)
	if _, err := m.LoginWithCode(context.Background(), "13800138000", "123456"); err != nil {
		t.Fatal(err)
	}
	if !m.IsExpired() {
		t.Fatal("服务已到期应判定为到期")
	}
	// 同步触发到期回调
	_, _ = m.SyncNow(context.Background())
	select {
	case ev := <-called:
		if ev != "expired" {
			t.Fatalf("应触发 expired 回调，实际 %s", ev)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("到期回调未触发")
	}
}

func TestRenewalClearsExpired(t *testing.T) {
	env := newTestEnv(t, time.Now().Add(-time.Hour).UnixMilli())
	m := newTestManager(t, env.ts.URL)
	m.SetCallbacks(func() {}, nil, nil, nil)
	if _, err := m.LoginWithCode(context.Background(), "13800138000", "123456"); err != nil {
		t.Fatal(err)
	}
	if !m.IsExpired() {
		t.Fatal("应已到期")
	}
	// 管理员续费 → 服务端到期时间延长
	env.mu.Lock()
	env.serviceUntil = time.Now().Add(30 * 24 * time.Hour).UnixMilli()
	env.mu.Unlock()
	_, err := m.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("同步失败: %v", err)
	}
	if m.IsExpired() {
		t.Fatal("续费后不应到期")
	}
}

func TestOfflineUsesCachedLicense(t *testing.T) {
	// 预写缓存：token + 未来到期时间
	dir := t.TempDir()
	cacheDirFn = func() string { return dir }
	cache := Cache{
		Phone: "13800138000", Token: "cached-token",
		ServiceUntilMS: time.Now().Add(24 * time.Hour).UnixMilli(),
		Profile:        "B",
	}
	if err := cache.Save(); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager("http://127.0.0.1:1") // 不可达地址
	if err != nil {
		t.Fatal(err)
	}
	st, err := m.SyncNow(context.Background())
	if err != nil {
		t.Fatalf("服务器不可达不应返回错误: %v", err)
	}
	if !st.ServerUnreachable || st.Expired {
		t.Fatalf("应使用缓存兜底且未到期: %+v", st)
	}
	if st.Profile != "B" {
		t.Fatalf("缓存模式应为 B: %+v", st)
	}
}

func TestProfilePersist(t *testing.T) {
	dir := t.TempDir()
	cacheDirFn = func() string { return dir }
	m, err := NewManager("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if err := m.SetProfile("B"); err != nil {
		t.Fatal(err)
	}
	m2, err := NewManager("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	if m2.Profile() != "B" {
		t.Fatalf("模式选择应持久化，实际 %s", m2.Profile())
	}
}

func TestTamperedCacheWithoutToken(t *testing.T) {
	dir := t.TempDir()
	cacheDirFn = func() string { return dir }
	cache := Cache{Phone: "13800138000", Token: "", ServiceUntilMS: time.Now().Add(365 * 24 * time.Hour).UnixMilli()}
	if err := cache.Save(); err != nil {
		t.Fatal(err)
	}
	m, _ := NewManager("http://127.0.0.1:1")
	if m.IsLoggedIn() {
		t.Fatal("无 token 的缓存不应视为已登录")
	}
	if m.IsExpired() {
		t.Fatal("未登录状态不应显示到期锁定（登录页兜底）")
	}
}

func TestUnopenedNotExpired(t *testing.T) {
	// 已登录但未开通服务：应标记“未开通”，不能当“到期锁定”（否则首次登录设置密码流程会被锁屏挤掉）
	dir := t.TempDir()
	cacheDirFn = func() string { return dir }
	cache := Cache{Phone: "13800138000", Token: "cached-token", ServiceUntilMS: 0, Profile: "A"}
	if err := cache.Save(); err != nil {
		t.Fatal(err)
	}
	m, err := NewManager("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}
	st := m.Status()
	if !st.Unopened {
		t.Fatalf("未开通服务应标记 unopened: %+v", st)
	}
	if st.Expired {
		t.Fatalf("未开通服务不应视为到期锁定: %+v", st)
	}
	if !m.IsUnopened() {
		t.Fatal("IsUnopened 应为 true")
	}
}

func TestUnauthorizedClearsSession(t *testing.T) {
	// 服务端返回 401（凭证失效）：客户端应清除本地登录态并触发回调，而不是空转轮询
	dir := t.TempDir()
	cacheDirFn = func() string { return dir }
	cache := Cache{
		Phone: "13800138000", Token: "stale-token",
		ServiceUntilMS: time.Now().Add(24 * time.Hour).UnixMilli(), Profile: "A",
	}
	if err := cache.Save(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_ = json.NewEncoder(w).Encode(map[string]any{"code": ErrCodeUnauthorized, "message": "登录已失效，请重新登录"})
	}))
	t.Cleanup(ts.Close)

	m, err := NewManager(ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	unauth := make(chan struct{}, 1)
	m.SetCallbacks(nil, nil, nil, func() { unauth <- struct{}{} })
	if !m.IsLoggedIn() {
		t.Fatal("预置缓存应处于已登录状态")
	}
	_, err = m.SyncNow(context.Background())
	if err == nil {
		t.Fatal("401 同步应返回错误")
	}
	if m.IsLoggedIn() {
		t.Fatal("401 后应清除本地登录态")
	}
	select {
	case <-unauth:
	case <-time.After(2 * time.Second):
		t.Fatal("应触发凭证失效回调")
	}
}
