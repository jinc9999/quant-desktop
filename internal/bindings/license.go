// C 版“超能战士”授权相关绑定方法（A/B 版返回“功能未开放”）
package bindings

import (
	"context"
	"errors"
	"log"
	"time"

	"quant-desktop/internal/binance"
	"quant-desktop/internal/license"
	"quant-desktop/internal/product"

	"github.com/wailsapp/wails/v3/pkg/application"
)

// licenseTimeout 授权请求超时
const licenseTimeout = 12 * time.Second

// GetProductInfo 获取产品变体信息（前端据此切换 UI）
func (s *QuantService) GetProductInfo() product.Info {
	log.Printf("[License] GetProductInfo 被前端调用")
	return product.GetInfo()
}

// LogClientEvent 前端事件上报（页面加载/JS 错误等，用于排查客户端界面问题）
func (s *QuantService) LogClientEvent(message string) string {
	log.Printf("[Frontend] %s", message)
	return "ok"
}

// LicenseSendSmsCode 发送手机验证码
// 返回操作结果提示；失败时返回错误（前端按错误弹窗展示）
func (s *QuantService) LicenseSendSmsCode(phone string) (string, error) {
	if !product.IsC() || s.lic == nil {
		return "", errors.New("功能未开放")
	}
	log.Printf("[License] 发送验证码 phone=%s", phone)
	ctx, cancel := context.WithTimeout(context.Background(), licenseTimeout)
	defer cancel()
	if err := s.lic.SendSmsCode(ctx, phone); err != nil {
		log.Printf("[License] 发送验证码失败 phone=%s: %v", phone, err)
		return "", err
	}
	return "验证码已发送，请查收", nil
}

// LicenseLoginWithCode 验证码登录/注册（首次登录自动创建账号并绑定设备）
func (s *QuantService) LicenseLoginWithCode(phone, code string) (license.LoginResult, error) {
	if !product.IsC() || s.lic == nil {
		return license.LoginResult{}, errors.New("功能未开放")
	}
	ctx, cancel := context.WithTimeout(context.Background(), licenseTimeout)
	defer cancel()
	result, err := s.lic.LoginWithCode(ctx, phone, code)
	if err != nil {
		log.Printf("[License] 验证码登录失败 phone=%s: %v", phone, err)
		return license.LoginResult{}, err
	}
	log.Printf("[License] 验证码登录成功 phone=%s needsPassword=%v", phone, result.NeedsPassword)
	return result, nil
}

// LicenseLoginWithPassword 密码登录
func (s *QuantService) LicenseLoginWithPassword(phone, password string) (license.LoginResult, error) {
	if !product.IsC() || s.lic == nil {
		return license.LoginResult{}, errors.New("功能未开放")
	}
	ctx, cancel := context.WithTimeout(context.Background(), licenseTimeout)
	defer cancel()
	result, err := s.lic.LoginWithPassword(ctx, phone, password)
	if err != nil {
		log.Printf("[License] 密码登录失败 phone=%s: %v", phone, err)
		return license.LoginResult{}, err
	}
	log.Printf("[License] 密码登录成功 phone=%s", phone)
	return result, nil
}

// LicenseSetPassword 首次登录设置密码
func (s *QuantService) LicenseSetPassword(password string) string {
	if !product.IsC() || s.lic == nil {
		return "功能未开放"
	}
	ctx, cancel := context.WithTimeout(context.Background(), licenseTimeout)
	defer cancel()
	if err := s.lic.SetPassword(ctx, password); err != nil {
		return err.Error()
	}
	return "密码设置成功"
}

// LicenseLogout 退出登录
func (s *QuantService) LicenseLogout() string {
	if !product.IsC() || s.lic == nil {
		return "功能未开放"
	}
	ctx, cancel := context.WithTimeout(context.Background(), licenseTimeout)
	defer cancel()
	if err := s.lic.Logout(ctx); err != nil {
		return err.Error()
	}
	return "已退出登录"
}

// LicenseRefresh 手动刷新授权状态
func (s *QuantService) LicenseRefresh() (license.Status, error) {
	if !product.IsC() || s.lic == nil {
		return license.Status{}, errors.New("功能未开放")
	}
	ctx, cancel := context.WithTimeout(context.Background(), licenseTimeout)
	defer cancel()
	return s.lic.SyncNow(ctx)
}

// GetLicenseStatus 获取当前授权状态（登录页/服务状态页/启动门禁共用）
func (s *QuantService) GetLicenseStatus() (license.Status, error) {
	if !product.IsC() || s.lic == nil {
		return license.Status{}, errors.New("功能未开放")
	}
	st := s.lic.Status()
	log.Printf("[License] GetLicenseStatus 返回: loggedIn=%v expired=%v phone=%s",
		st.LoggedIn, st.Expired, st.Phone)
	return st, nil
}

// SetActiveProfile 切换 A/B 模式（C 版专用，运行中禁止切换）
func (s *QuantService) SetActiveProfile(profile string) string {
	if !product.IsC() {
		return "功能未开放"
	}
	if profile != "A" && profile != "B" {
		return "模式必须是 A（进攻）或 B（稳健）"
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return "策略运行中，请先停止再切换模式"
	}
	s.activeProfile = profile
	s.cfg = binance.LockedProfileConfig(profile)
	if s.lic != nil {
		if err := s.lic.SetProfile(profile); err != nil {
			log.Printf("[License] 保存模式选择失败: %v", err)
		}
	}
	return "已切换到" + profileDisplayName(profile)
}

// GetActiveProfile 获取当前 A/B 模式
func (s *QuantService) GetActiveProfile() string {
	if !product.IsC() {
		return "A"
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.activeProfile
}

// initLicenseManager 初始化 C 版授权管理器（参数锁定 + 回调注册）
func (s *QuantService) initLicenseManager() error {
	lm, err := license.NewManager(product.LicenseServerURL)
	if err != nil {
		return err
	}
	s.lic = lm
	s.activeProfile = lm.Profile()
	s.cfg = binance.LockedProfileConfig(s.activeProfile)
	lm.SetCallbacks(s.handleLicenseExpired, s.handleLicenseRenewed, s.handleLicenseOffline, s.handleLicenseUnauthorized)
	log.Printf("[License] 授权管理器初始化完成：服务器=%s 设备=%s 模式=%s",
		lm.ServerURL(), lm.DeviceID(), s.activeProfile)
	return nil
}

// handleLicenseExpired 到期回调：停止策略 + 锁定 + 推送前端事件
func (s *QuantService) handleLicenseExpired() {
	log.Printf("[License] 服务已到期：停止策略并锁定核心功能")
	s.mu.Lock()
	s.licenseLocked = true
	started := s.started
	s.mu.Unlock()

	s.emitLicenseEvent("license:expired", "服务已到期，请联系管理员续费")
	if started {
		_ = s.StopStrategy()
	}
}

// handleLicenseRenewed 续费成功回调：解锁 + 推送前端事件
func (s *QuantService) handleLicenseRenewed() {
	log.Printf("[License] 服务已续费，恢复正常使用")
	s.mu.Lock()
	s.licenseLocked = false
	s.mu.Unlock()
	s.emitLicenseEvent("license:renewed", "服务已续费，功能已恢复")
}

// handleLicenseOffline 授权服务器连通性回调
func (s *QuantService) handleLicenseOffline(offline bool) {
	msg := "授权服务器连接正常"
	if offline {
		msg = "授权服务器连接异常，当前使用本地缓存"
	}
	s.emitLicenseEvent("license:offline", msg)
}

// handleLicenseUnauthorized 凭证失效回调：通知前端回登录页
func (s *QuantService) handleLicenseUnauthorized() {
	log.Printf("[License] 登录凭证已失效，请重新登录")
	s.emitLicenseEvent("license:unauthorized", "登录已失效，请重新登录")
}

// emitLicenseEvent 推送授权事件到前端
func (s *QuantService) emitLicenseEvent(name, message string) {
	if s.app == nil {
		return
	}
	s.app.Event.EmitEvent(&application.CustomEvent{
		Name: name,
		Data: map[string]string{"message": message},
	})
}

// profileDisplayName 模式展示名
func profileDisplayName(profile string) string {
	if profile == "B" {
		return "稳健模式"
	}
	return "进攻模式"
}
