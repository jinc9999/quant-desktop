// 客户端接口：短信验证码、登录/注册、密码设置、授权状态、退出
package api

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"errors"
	"fmt"
	"log"
	"math/rand/v2"
	"net/http"
	"time"

	"quant-server/internal/security"
	"quant-server/internal/store"
)

const (
	smsCodeTTL       = 5 * time.Minute
	smsMaxAttempts   = 5
	smsDailyLimit    = 10
	smsResendMin     = 60 * time.Second
	customerTokenTTL = 7 * 24 * time.Hour
)

type smsSendReq struct {
	Phone string `json:"phone"`
}

func (s *Server) handleSendSMS(w http.ResponseWriter, r *http.Request) {
	var req smsSendReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if !phoneRE.MatchString(req.Phone) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "手机号格式不正确")
		return
	}
	key := "sms:" + req.Phone
	if !s.limiter.Allow(key, smsResendMin, 1) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, "验证码发送过于频繁，请 60 秒后再试")
		return
	}
	todayStart := time.Now().Truncate(24 * time.Hour)
	cnt, err := s.st.CountSMSCodesSince(r.Context(), req.Phone, todayStart)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "服务器繁忙，请稍后再试")
		return
	}
	if cnt >= smsDailyLimit {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, "今日验证码发送次数已达上限")
		return
	}

	code := randomSMSCode(s.cfg.MockSMSCode)
	if err := s.sms.Send(r.Context(), req.Phone, code); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "验证码发送失败: "+err.Error())
		return
	}
	hash := smsCodeHash(code)
	if err := s.st.SaveSMSCode(r.Context(), req.Phone, "login", hash, time.Now().Add(smsCodeTTL)); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "验证码保存失败")
		return
	}
	data := map[string]any{"message": "验证码已发送"}
	if s.cfg.IsMockSMS() {
		_ = s.st.SaveSMSLog(r.Context(), req.Phone, code)
		data["devCode"] = code
	}
	writeOK(w, data)
}

func randomSMSCode(fixed string) string {
	if fixed != "" && len(fixed) == 6 {
		return fixed
	}
	return fmt.Sprintf("%06d", rand.IntN(1000000))
}

func smsCodeHash(code string) string {
	sum := sha256.Sum256([]byte(code))
	return hex.EncodeToString(sum[:])
}

type loginReq struct {
	Phone    string `json:"phone"`
	Code     string `json:"code"`
	Password string `json:"password"`
	DeviceID string `json:"deviceId"`
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var req loginReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if !phoneRE.MatchString(req.Phone) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "手机号格式不正确")
		return
	}
	if len(req.DeviceID) < 16 {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "设备标识缺失，无法完成绑定")
		return
	}

	var customer *store.Customer
	var err error
	passwordMode := req.Password != ""
	codeMode := req.Code != ""
	if codeMode && passwordMode {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "验证码与密码请二选一")
		return
	}

	if codeMode {
		customer, err = s.verifySMSCodeLogin(r, req.Phone, req.Code)
	} else if passwordMode {
		customer, err = s.verifyPasswordLogin(r, req.Phone, req.Password)
	} else {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请提供验证码或密码")
		return
	}
	if err != nil {
		// err 已携带业务错误码
		writeErr(w, http.StatusForbidden, CodeForbidden, err.Error())
		return
	}
	if customer.Status != store.CustomerStatusActive {
		writeErr(w, http.StatusForbidden, CodeForbidden, "账号已停用，请联系管理员")
		return
	}

	// 一机一号：绑定校验
	if err := s.enforceDeviceBinding(r, customer, req.DeviceID); err != nil {
		writeErr(w, http.StatusForbidden, CodeDeviceBound, err.Error())
		return
	}

	needsPassword := customer.PasswordHash == ""
	serviceUntil, err := s.st.GetServiceUntil(r.Context(), customer.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "查询服务状态失败")
		return
	}

	jti := security.RandomToken()
	claims := security.Claims{
		Sub:      customer.ID,
		Role:     "customer",
		JTI:      jti,
		IAT:      time.Now().Unix(),
		EXP:      time.Now().Add(customerTokenTTL).Unix(),
		Phone:    customer.Phone,
		DeviceID: req.DeviceID,
	}
	token, err := security.SignToken(s.cfg.JWTSecret, claims)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "签发登录凭证失败")
		return
	}
	if err := s.st.SaveSession(r.Context(), store.Session{
		JTI: jti, CustomerID: customer.ID, DeviceID: req.DeviceID,
		Role: "customer", ExpiresAt: time.Unix(claims.EXP, 0), CreatedAt: time.Now(),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "保存登录会话失败")
		return
	}

	var untilMS int64
	if !serviceUntil.IsZero() {
		untilMS = serviceUntil.UnixMilli()
	}
	writeOK(w, map[string]any{
		"token":          token,
		"phone":          customer.Phone,
		"deviceId":       req.DeviceID,
		"needsPassword":  needsPassword,
		"serviceUntilMs": untilMS,
		"serverNowMs":    nowMS(),
		"message":        licenseMessage(untilMS),
	})
}

// verifySMSCodeLogin 校验验证码并返回客户（首次登录自动创建账号）
func (s *Server) verifySMSCodeLogin(r *http.Request, phone, code string) (*store.Customer, error) {
	rec, err := s.st.GetLatestSMSCode(r.Context(), phone, "login")
	if err != nil {
		return nil, errors.New("请先获取验证码")
	}
	if rec.Used == 1 {
		return nil, errors.New("验证码已使用，请重新获取")
	}
	if time.Now().After(rec.ExpiresAt) {
		return nil, errors.New("验证码已过期，请重新获取")
	}
	if rec.Attempts >= smsMaxAttempts {
		return nil, errors.New("验证码尝试次数过多，请重新获取")
	}
	want, _ := hex.DecodeString(rec.CodeHash)
	got := sha256.Sum256([]byte(code))
	if subtle.ConstantTimeCompare(want, got[:]) != 1 {
		_ = s.st.IncrementSMSCodeAttempts(r.Context(), rec.ID)
		return nil, errors.New("验证码错误")
	}
	_ = s.st.MarkSMSCodeUsed(r.Context(), rec.ID)

	customer, err := s.st.GetCustomerByPhone(r.Context(), phone)
	if errors.Is(err, store.ErrNotFound) {
		// 首次验证码登录自动注册
		customer, err = s.st.CreateCustomer(r.Context(), phone)
		if err == nil && s.cfg.AutoTrialEnabled() {
			// 首次注册自动赠送试用周期（系统赠送，admin_id=0）
			now := time.Now()
			if gerr := s.st.AddGrant(r.Context(), customer.ID, s.cfg.AutoTrialPeriod, 0,
				now, now.Add(store.PeriodDurations[s.cfg.AutoTrialPeriod])); gerr != nil {
				log.Printf("[Auth] 首次注册赠送试用周期失败 phone=%s: %v", phone, gerr)
			} else {
				log.Printf("[Auth] 首次注册自动赠送试用 %s phone=%s", s.cfg.AutoTrialPeriod, phone)
			}
		}
		return customer, err
	}
	return customer, err
}

// verifyPasswordLogin 校验密码登录
func (s *Server) verifyPasswordLogin(r *http.Request, phone, password string) (*store.Customer, error) {
	// 防爆破：同号+IP 每分钟最多 5 次
	key := "login:" + phone + ":" + clientIP(r)
	if !s.limiter.Allow(key, time.Minute, 5) {
		return nil, errors.New("登录尝试过于频繁，请稍后再试")
	}
	customer, err := s.st.GetCustomerByPhone(r.Context(), phone)
	if err != nil {
		return nil, errors.New("账号不存在或密码错误")
	}
	if customer.PasswordHash == "" {
		return nil, errors.New("账号尚未设置密码，请使用验证码登录")
	}
	if !security.VerifyPassword(customer.PasswordHash, password) {
		return nil, errors.New("账号不存在或密码错误")
	}
	return customer, nil
}

// enforceDeviceBinding 一机一号绑定规则
func (s *Server) enforceDeviceBinding(r *http.Request, customer *store.Customer, deviceID string) error {
	dev, err := s.st.GetDeviceByCustomer(r.Context(), customer.ID)
	switch {
	case err == nil:
		if dev.DeviceID != deviceID {
			return errors.New("账号已绑定其他设备，请联系管理员解绑后重试")
		}
		_ = s.st.TouchDevice(r.Context(), customer.ID)
		return nil
	case errors.Is(err, store.ErrNotFound):
		// 设备是否已绑定其他账号（一机一号双向唯一）
		if other, derr := s.st.GetCustomerByDevice(r.Context(), deviceID); derr == nil && other.ID != customer.ID {
			return errors.New("该设备已绑定其他账号")
		}
		if err := s.st.BindDevice(r.Context(), customer.ID, deviceID); err != nil {
			return errors.New("设备绑定失败，请联系管理员")
		}
		return nil
	default:
		return errors.New("设备绑定校验失败，请稍后再试")
	}
}

type setPasswordReq struct {
	Password string `json:"password"`
}

func (s *Server) handleSetPassword(w http.ResponseWriter, r *http.Request) {
	customer := r.Context().Value(ctxCustomer).(*store.Customer)
	var req setPasswordReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if err := security.ValidatePasswordStrength(req.Password); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	hash, err := security.HashPassword(req.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "密码加密失败")
		return
	}
	if err := s.st.SetCustomerPassword(r.Context(), customer.ID, hash); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "密码保存失败")
		return
	}
	writeOK(w, map[string]string{"message": "密码设置成功"})
}

func (s *Server) handleLicense(w http.ResponseWriter, r *http.Request) {
	customer := r.Context().Value(ctxCustomer).(*store.Customer)
	claims := r.Context().Value(ctxClaims).(*security.Claims)
	dev, err := s.st.GetDeviceByCustomer(r.Context(), customer.ID)
	if err == nil && dev.DeviceID != claims.DeviceID {
		writeErr(w, http.StatusForbidden, CodeDeviceBound, "设备不匹配，请重新登录")
		return
	}
	serviceUntil, err := s.st.GetServiceUntil(r.Context(), customer.ID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "查询服务状态失败")
		return
	}
	var untilMS int64
	if !serviceUntil.IsZero() {
		untilMS = serviceUntil.UnixMilli()
	}
	writeOK(w, map[string]any{
		"phone":          customer.Phone,
		"deviceId":       claims.DeviceID,
		"serviceUntilMs": untilMS,
		"serverNowMs":    nowMS(),
		"needsPassword":  customer.PasswordHash == "",
		"message":        licenseMessage(untilMS),
	})
}

func licenseMessage(untilMS int64) string {
	if untilMS <= 0 {
		return "账号尚未开通服务，请联系管理员"
	}
	if time.Now().After(time.UnixMilli(untilMS)) {
		return "服务已到期，请联系管理员续费"
	}
	return "服务正常"
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	claims := r.Context().Value(ctxClaims).(*security.Claims)
	_ = s.st.RevokeSession(r.Context(), claims.JTI)
	writeOK(w, map[string]string{"message": "已退出登录"})
}
