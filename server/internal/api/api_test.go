package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"quant-server/internal/config"
	"quant-server/internal/security"
	"quant-server/internal/sms"
	"quant-server/internal/store"
)

const testPhone = "13800138000"

func newTestServer(t *testing.T) *httptest.Server {
	t.Helper()
	cfg := config.Config{
		Env: "dev", ListenAddr: ":0", DBDriver: "memory",
		JWTSecret: "test-secret", SMSMode: "mock", MockSMSCode: "123456",
		AdminUsername: "admin", AdminPassword: "Admin@123456",
		AutoTrialPeriod: store.PeriodTrial,
	}
	st := store.NewMemory()
	hash, err := security.HashPassword(cfg.AdminPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateAdmin(context.Background(), cfg.AdminUsername, hash, true); err != nil {
		t.Fatal(err)
	}
	srv := New(cfg, st, sms.Mock{})
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

func doJSON(t *testing.T, method, url, token string, body any) (int, map[string]any) {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatal(err)
		}
	}
	req, err := http.NewRequest(method, url, &buf)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatalf("响应解析失败: %v", err)
	}
	return resp.StatusCode, out
}

func codeOf(m map[string]any) int {
	c, _ := m["code"].(float64)
	return int(c)
}

func dataOf(m map[string]any) map[string]any {
	d, _ := m["data"].(map[string]any)
	return d
}

func TestSendSMS_ValidationAndRateLimit(t *testing.T) {
	ts := newTestServer(t)

	// 非法手机号
	status, out := doJSON(t, "POST", ts.URL+"/api/v1/sms/send", "", map[string]string{"phone": "123"})
	if status != http.StatusBadRequest || codeOf(out) != CodeBadRequest {
		t.Fatalf("非法手机号应被拒绝: status=%d out=%v", status, out)
	}

	// 正常发送（mock 返回 devCode）
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/sms/send", "", map[string]string{"phone": testPhone})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("发送验证码失败: status=%d out=%v", status, out)
	}
	if dev := dataOf(out)["devCode"]; dev != "123456" {
		t.Fatalf("mock 验证码应为 123456，实际 %v", dev)
	}

	// 60 秒内重复发送应被限频
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/sms/send", "", map[string]string{"phone": testPhone})
	if status != http.StatusTooManyRequests || codeOf(out) != CodeRateLimited {
		t.Fatalf("重复发送应被限频: status=%d out=%v", status, out)
	}
}

func TestCodeLogin_BindDevice_SetPassword(t *testing.T) {
	ts := newTestServer(t)

	// 未获取验证码直接登录
	_, out := doJSON(t, "POST", ts.URL+"/api/v1/auth/login", "", map[string]any{
		"phone": testPhone, "code": "123456", "deviceId": "dev-0000000000000001",
	})
	if codeOf(out) == CodeOK {
		t.Fatalf("未获取验证码应登录失败: %v", out)
	}

	// 获取验证码后登录（首次登录自动注册 + 绑定设备）
	doJSON(t, "POST", ts.URL+"/api/v1/sms/send", "", map[string]string{"phone": testPhone})
	status, out := doJSON(t, "POST", ts.URL+"/api/v1/auth/login", "", map[string]any{
		"phone": testPhone, "code": "123456", "deviceId": "dev-0000000000000001",
	})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("验证码登录失败: status=%d out=%v", status, out)
	}
	token := dataOf(out)["token"].(string)
	if token == "" {
		t.Fatal("登录应返回 token")
	}
	if needs := dataOf(out)["needsPassword"]; needs != true {
		t.Fatalf("首次登录应提示设置密码: %v", out)
	}
	if until := dataOf(out)["serviceUntilMs"].(float64); until <= 0 {
		t.Fatalf("首次注册应自动赠送 3 天试用（serviceUntilMs>0）: %v", out)
	}

	// 设置密码
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/auth/password", token, map[string]string{
		"password": "Abc123456",
	})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("设置密码失败: status=%d out=%v", status, out)
	}

	// 密码登录
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/auth/login", "", map[string]any{
		"phone": testPhone, "password": "Abc123456", "deviceId": "dev-0000000000000001",
	})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("密码登录失败: status=%d out=%v", status, out)
	}

	// 同一账号换设备应被拒绝
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/auth/login", "", map[string]any{
		"phone": testPhone, "password": "Abc123456", "deviceId": "dev-0000000000000002",
	})
	if codeOf(out) != CodeDeviceBound {
		t.Fatalf("换设备登录应被拒绝: status=%d out=%v", status, out)
	}

	// 错误密码
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/auth/login", "", map[string]any{
		"phone": testPhone, "password": "WrongPass1", "deviceId": "dev-0000000000000001",
	})
	if codeOf(out) == CodeOK {
		t.Fatalf("错误密码应登录失败: %v", out)
	}
}

func TestAdminFlow_Grant_Unbind_Expiry(t *testing.T) {
	ts := newTestServer(t)

	// 管理员登录（默认需改密）
	_, out := doJSON(t, "POST", ts.URL+"/api/v1/admin/login", "", map[string]string{
		"username": "admin", "password": "Admin@123456",
	})
	adminToken := dataOf(out)["token"].(string)
	if must := dataOf(out)["mustChangePassword"]; must != true {
		t.Fatalf("初始管理员应强制改密: %v", out)
	}

	// 未改密访问客户列表应被拦截
	status, out := doJSON(t, "GET", ts.URL+"/api/v1/admin/customers", adminToken, nil)
	if status != http.StatusForbidden {
		t.Fatalf("未改密应禁止访问: status=%d out=%v", status, out)
	}

	// 修改密码
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/admin/password", adminToken, map[string]string{
		"oldPassword": "Admin@123456", "newPassword": "Admin@New123",
	})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("改密失败: status=%d out=%v", status, out)
	}
	if nt, ok := dataOf(out)["token"].(string); ok && nt != "" {
		adminToken = nt // 使用新 token（已清除强制改密标记）
	}

	// 创建客户并开通一周
	status, out = doJSON(t, "POST", ts.URL+"/api/v1/admin/customers", adminToken, map[string]any{
		"phone": testPhone, "period": "1w",
	})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("创建客户失败: status=%d out=%v", status, out)
	}
	customerID := int(dataOf(out)["id"].(float64))

	// 客户验证码登录绑定设备
	doJSON(t, "POST", ts.URL+"/api/v1/sms/send", "", map[string]string{"phone": testPhone})
	_, out = doJSON(t, "POST", ts.URL+"/api/v1/auth/login", "", map[string]any{
		"phone": testPhone, "code": "123456", "deviceId": "dev-0000000000000003",
	})
	customerToken := dataOf(out)["token"].(string)
	untilMS := dataOf(out)["serviceUntilMs"].(float64)
	if untilMS <= 0 {
		t.Fatalf("开通一周后 serviceUntilMs 应>0: %v", out)
	}

	// 授权状态接口
	status, out = doJSON(t, "GET", ts.URL+"/api/v1/license", customerToken, nil)
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("查询授权状态失败: status=%d out=%v", status, out)
	}

	// 续费一月（叠加）
	status, out = doJSON(t, "POST", fmt.Sprintf("%s/api/v1/admin/customers/%d/grant", ts.URL, customerID), adminToken, map[string]string{"period": "1m"})
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("续费失败: status=%d out=%v", status, out)
	}
	newUntil := dataOf(out)["serviceUntilMs"].(float64)
	if newUntil <= untilMS {
		t.Fatalf("续费应叠加到期时间: old=%v new=%v", untilMS, newUntil)
	}

	// 非法周期
	status, out = doJSON(t, "POST", fmt.Sprintf("%s/api/v1/admin/customers/%d/grant", ts.URL, customerID), adminToken, map[string]string{"period": "2y"})
	if status != http.StatusBadRequest {
		t.Fatalf("非法周期应被拒绝: status=%d out=%v", status, out)
	}

	// 解绑设备 → 原 token 失效
	status, out = doJSON(t, "POST", fmt.Sprintf("%s/api/v1/admin/customers/%d/unbind-device", ts.URL, customerID), adminToken, nil)
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("解绑失败: status=%d out=%v", status, out)
	}
	status, out = doJSON(t, "GET", ts.URL+"/api/v1/license", customerToken, nil)
	if status != http.StatusUnauthorized {
		t.Fatalf("解绑后旧 token 应失效: status=%d out=%v", status, out)
	}

	// 审计日志
	status, out = doJSON(t, "GET", ts.URL+"/api/v1/admin/audit-logs", adminToken, nil)
	if status != http.StatusOK || codeOf(out) != CodeOK {
		t.Fatalf("查询审计日志失败: status=%d out=%v", status, out)
	}
	items := dataOf(out)["items"].([]any)
	if len(items) == 0 {
		t.Fatal("应有审计日志")
	}
}

func TestAdminAuth_WrongPassword(t *testing.T) {
	ts := newTestServer(t)
	_, out := doJSON(t, "POST", ts.URL+"/api/v1/admin/login", "", map[string]string{
		"username": "admin", "password": "wrong",
	})
	if codeOf(out) != CodeUnauthorized {
		t.Fatalf("错误密码应被拒绝: %v", out)
	}
}

func TestServiceGrantPeriods(t *testing.T) {
	st := store.NewMemory()
	ctx := context.Background()
	c, err := st.CreateCustomer(ctx, testPhone)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	if err := st.AddGrant(ctx, c.ID, store.PeriodWeek, 0, now, now.Add(store.PeriodDurations[store.PeriodWeek])); err != nil {
		t.Fatal(err)
	}
	until, err := st.GetServiceUntil(ctx, c.ID)
	if err != nil {
		t.Fatal(err)
	}
	if until.Sub(now) < 6*24*time.Hour {
		t.Fatalf("一周周期到期时间异常: %v", until)
	}
	// 叠加续期
	if err := st.AddGrant(ctx, c.ID, store.PeriodMonth, 0, until, until.Add(store.PeriodDurations[store.PeriodMonth])); err != nil {
		t.Fatal(err)
	}
	until2, _ := st.GetServiceUntil(ctx, c.ID)
	if !until2.After(until) {
		t.Fatal("续费应自动叠加")
	}
	for _, p := range []string{store.PeriodTrial, store.PeriodWeek, store.PeriodMonth, store.PeriodHalf, store.PeriodYear} {
		if !store.ValidPeriod(p) {
			t.Fatalf("周期 %s 应合法", p)
		}
	}
	if store.ValidPeriod("2y") {
		t.Fatal("2y 应非法")
	}
}

func TestAdminWebServed(t *testing.T) {
	ts := newTestServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "客户管理系统") {
		t.Fatalf("管理后台首页内容异常: %s", string(body)[:min(len(body), 120)])
	}
	// 静态资源必须返回真实文件内容（防止 404 回退成 index.html 导致白屏）
	for _, path := range []string{"/vendor/vue.global.prod.js", "/vendor/element-plus.min.js", "/app.js", "/vendor/element-plus.css"} {
		r, err := http.Get(ts.URL + path)
		if err != nil {
			t.Fatalf("请求 %s 失败: %v", path, err)
		}
		b, _ := io.ReadAll(r.Body)
		r.Body.Close()
		if r.StatusCode != http.StatusOK || len(b) < 1000 {
			t.Fatalf("静态资源 %s 返回异常: status=%d len=%d", path, r.StatusCode, len(b))
		}
		if strings.HasPrefix(string(b), "<!DOCTYPE") {
			t.Fatalf("静态资源 %s 被错误回退成 index.html", path)
		}
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
