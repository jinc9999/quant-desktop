// 管理端接口：管理员登录/改密、客户管理、开通/续费、解绑、审计
package api

import (
	"net/http"
	"strconv"
	"time"

	"quant-server/internal/security"
	"quant-server/internal/store"
)

const adminTokenTTL = 24 * time.Hour

type adminLoginReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

func (s *Server) handleAdminLogin(w http.ResponseWriter, r *http.Request) {
	var req adminLoginReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if !s.limiter.Allow("admin:"+clientIP(r), time.Minute, 10) {
		writeErr(w, http.StatusTooManyRequests, CodeRateLimited, "尝试过于频繁，请稍后再试")
		return
	}
	admin, err := s.st.GetAdminByUsername(r.Context(), req.Username)
	if err != nil || !security.VerifyPassword(admin.PasswordHash, req.Password) {
		writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "用户名或密码错误")
		return
	}
	jti := security.RandomToken()
	claims := security.Claims{
		Sub: admin.ID, Role: "admin", JTI: jti,
		IAT: time.Now().Unix(), EXP: time.Now().Add(adminTokenTTL).Unix(),
		MustChange: admin.MustChangePassword,
	}
	token, err := security.SignToken(s.cfg.JWTSecret, claims)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "签发登录凭证失败")
		return
	}
	if err := s.st.SaveSession(r.Context(), store.Session{
		JTI: jti, CustomerID: admin.ID, Role: "admin",
		ExpiresAt: time.Unix(claims.EXP, 0), CreatedAt: time.Now(),
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "保存登录会话失败")
		return
	}
	writeOK(w, map[string]any{
		"token":              token,
		"username":           admin.Username,
		"mustChangePassword": admin.MustChangePassword,
	})
}

type adminPasswordReq struct {
	OldPassword string `json:"oldPassword"`
	NewPassword string `json:"newPassword"`
}

func (s *Server) handleAdminPassword(w http.ResponseWriter, r *http.Request) {
	admin := r.Context().Value(ctxAdmin).(*store.Admin)
	var req adminPasswordReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if !security.VerifyPassword(admin.PasswordHash, req.OldPassword) {
		writeErr(w, http.StatusForbidden, CodeForbidden, "原密码错误")
		return
	}
	if err := security.ValidatePasswordStrength(req.NewPassword); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, err.Error())
		return
	}
	hash, err := security.HashPassword(req.NewPassword)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "密码加密失败")
		return
	}
	if err := s.st.UpdateAdminPassword(r.Context(), admin.ID, hash, false); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "密码保存失败")
		return
	}
	// 签发新 token（清除强制改密标记），前端替换旧 token
	jti := security.RandomToken()
	claims := security.Claims{
		Sub: admin.ID, Role: "admin", JTI: jti,
		IAT: time.Now().Unix(), EXP: time.Now().Add(adminTokenTTL).Unix(),
	}
	newToken, err := security.SignToken(s.cfg.JWTSecret, claims)
	if err == nil {
		_ = s.st.SaveSession(r.Context(), store.Session{
			JTI: jti, CustomerID: admin.ID, Role: "admin",
			ExpiresAt: time.Unix(claims.EXP, 0), CreatedAt: time.Now(),
		})
	}
	_ = s.audit(r, admin.ID, "admin.change_password", "admin", strconv.FormatInt(admin.ID, 10), "管理员修改密码")
	writeOK(w, map[string]any{"message": "密码修改成功", "token": newToken})
}

func (s *Server) handleAdminListCustomers(w http.ResponseWriter, r *http.Request) {
	admin := r.Context().Value(ctxAdmin).(*store.Admin)
	q := r.URL.Query()
	search := q.Get("search")
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	customers, total, err := s.st.ListCustomers(r.Context(), search, pageSize, (page-1)*pageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "查询客户失败")
		return
	}
	items := make([]map[string]any, 0, len(customers))
	for _, c := range customers {
		until, _ := s.st.GetServiceUntil(r.Context(), c.ID)
		var untilMS int64
		if !until.IsZero() {
			untilMS = until.UnixMilli()
		}
		deviceID := ""
		if dev, derr := s.st.GetDeviceByCustomer(r.Context(), c.ID); derr == nil {
			deviceID = dev.DeviceID
		}
		items = append(items, map[string]any{
			"id":             c.ID,
			"phone":          c.Phone,
			"status":         c.Status,
			"needsPassword":  c.PasswordHash == "",
			"serviceUntilMs": untilMS,
			"deviceId":       deviceID,
			"createdAt":      c.CreatedAt.UnixMilli(),
		})
	}
	_ = s.audit(r, admin.ID, "admin.list_customers", "customer", "", "查询客户列表")
	writeOK(w, map[string]any{"items": items, "total": total, "page": page, "pageSize": pageSize})
}

type createCustomerReq struct {
	Phone  string `json:"phone"`
	Period string `json:"period"` // 可选：首次开通周期
}

func (s *Server) handleAdminCreateCustomer(w http.ResponseWriter, r *http.Request) {
	admin := r.Context().Value(ctxAdmin).(*store.Admin)
	var req createCustomerReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if !phoneRE.MatchString(req.Phone) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "手机号格式不正确")
		return
	}
	if req.Period != "" && !store.ValidPeriod(req.Period) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "服务周期必须为一周/一月/半年/一年")
		return
	}
	customer, err := s.st.GetCustomerByPhone(r.Context(), req.Phone)
	if err == nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "该手机号已存在客户账号")
		return
	}
	customer, err = s.st.CreateCustomer(r.Context(), req.Phone)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "创建客户失败")
		return
	}
	if req.Period != "" {
		now := time.Now()
		if err := s.st.AddGrant(r.Context(), customer.ID, req.Period, admin.ID, now, now.Add(store.PeriodDurations[req.Period])); err != nil {
			writeErr(w, http.StatusInternalServerError, CodeServer, "开通服务失败")
			return
		}
	}
	_ = s.audit(r, admin.ID, "admin.create_customer", "customer", strconv.FormatInt(customer.ID, 10),
		"创建客户 "+req.Phone+" 初始周期 "+req.Period)
	writeOK(w, map[string]any{"id": customer.ID, "phone": customer.Phone})
}

func (s *Server) handleAdminCustomerDetail(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "客户 ID 无效")
		return
	}
	customer, err := s.st.GetCustomerByID(r.Context(), id)
	if err != nil {
		writeErr(w, http.StatusNotFound, CodeNotFound, "客户不存在")
		return
	}
	until, _ := s.st.GetServiceUntil(r.Context(), id)
	var untilMS int64
	if !until.IsZero() {
		untilMS = until.UnixMilli()
	}
	deviceID := ""
	boundAt := int64(0)
	if dev, derr := s.st.GetDeviceByCustomer(r.Context(), id); derr == nil {
		deviceID = dev.DeviceID
		boundAt = dev.BoundAt.UnixMilli()
	}
	grants, _ := s.st.ListGrants(r.Context(), id)
	writeOK(w, map[string]any{
		"id":             customer.ID,
		"phone":          customer.Phone,
		"status":         customer.Status,
		"needsPassword":  customer.PasswordHash == "",
		"serviceUntilMs": untilMS,
		"deviceId":       deviceID,
		"deviceBoundAt":  boundAt,
		"grants":         grants,
		"createdAt":      customer.CreatedAt.UnixMilli(),
	})
}

type grantReq struct {
	Period string `json:"period"`
}

func (s *Server) handleAdminGrant(w http.ResponseWriter, r *http.Request) {
	admin := r.Context().Value(ctxAdmin).(*store.Admin)
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "客户 ID 无效")
		return
	}
	var req grantReq
	if err := decodeJSON(r, &req); err != nil {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "请求格式错误")
		return
	}
	if !store.ValidPeriod(req.Period) {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "服务周期必须为一周/一月/半年/一年")
		return
	}
	if _, err := s.st.GetCustomerByID(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, CodeNotFound, "客户不存在")
		return
	}
	now := time.Now()
	until, _ := s.st.GetServiceUntil(r.Context(), id)
	base := now
	if until.After(base) {
		base = until // 续费自动叠加
	}
	if err := s.st.AddGrant(r.Context(), id, req.Period, admin.ID, base, base.Add(store.PeriodDurations[req.Period])); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "开通/续费失败")
		return
	}
	newUntil, _ := s.st.GetServiceUntil(r.Context(), id)
	_ = s.audit(r, admin.ID, "admin.grant", "customer", strconv.FormatInt(id, 10),
		"开通/续费周期 "+req.Period+" 新到期时间 "+newUntil.Format("2006-01-02 15:04:05"))
	writeOK(w, map[string]any{
		"message":        "服务已开通，到期时间 " + newUntil.Format("2006-01-02 15:04:05"),
		"serviceUntilMs": newUntil.UnixMilli(),
	})
}

func (s *Server) handleAdminUnbindDevice(w http.ResponseWriter, r *http.Request) {
	admin := r.Context().Value(ctxAdmin).(*store.Admin)
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "客户 ID 无效")
		return
	}
	if _, err := s.st.GetCustomerByID(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, CodeNotFound, "客户不存在")
		return
	}
	_ = s.st.UnbindDevice(r.Context(), id)
	_ = s.st.RevokeCustomerSessions(r.Context(), id)
	_ = s.audit(r, admin.ID, "admin.unbind_device", "customer", strconv.FormatInt(id, 10), "解绑设备（该账号需重新登录绑定）")
	writeOK(w, map[string]string{"message": "已解绑设备"})
}

func (s *Server) handleAdminDisable(w http.ResponseWriter, r *http.Request) {
	s.setCustomerStatus(w, r, store.CustomerStatusDisabled, "停用")
}

func (s *Server) handleAdminEnable(w http.ResponseWriter, r *http.Request) {
	s.setCustomerStatus(w, r, store.CustomerStatusActive, "启用")
}

func (s *Server) setCustomerStatus(w http.ResponseWriter, r *http.Request, status int, label string) {
	admin := r.Context().Value(ctxAdmin).(*store.Admin)
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusBadRequest, CodeBadRequest, "客户 ID 无效")
		return
	}
	if _, err := s.st.GetCustomerByID(r.Context(), id); err != nil {
		writeErr(w, http.StatusNotFound, CodeNotFound, "客户不存在")
		return
	}
	if err := s.st.SetCustomerStatus(r.Context(), id, status); err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "状态更新失败")
		return
	}
	if status == store.CustomerStatusDisabled {
		_ = s.st.RevokeCustomerSessions(r.Context(), id)
	}
	_ = s.audit(r, admin.ID, "admin.set_status", "customer", strconv.FormatInt(id, 10), label+"客户")
	writeOK(w, map[string]string{"message": "已" + label + "客户"})
}

func (s *Server) handleAdminSMSCodes(w http.ResponseWriter, r *http.Request) {
	if !s.cfg.IsMockSMS() {
		writeOK(w, map[string]any{"items": []any{}, "message": "非模拟模式不提供验证码查看"})
		return
	}
	phone := r.URL.Query().Get("phone")
	logs, err := s.st.ListSMSLogs(r.Context(), phone, 100)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "查询短信记录失败")
		return
	}
	items := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		items = append(items, map[string]any{
			"id": l.ID, "phone": l.Phone, "code": l.Code, "createdAt": l.CreatedAt.UnixMilli(),
		})
	}
	writeOK(w, map[string]any{"items": items})
}

func (s *Server) handleAdminAuditLogs(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	page, _ := strconv.Atoi(q.Get("page"))
	pageSize, _ := strconv.Atoi(q.Get("pageSize"))
	if page < 1 {
		page = 1
	}
	if pageSize < 1 || pageSize > 100 {
		pageSize = 20
	}
	logs, total, err := s.st.ListAuditLogs(r.Context(), pageSize, (page-1)*pageSize)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, CodeServer, "查询审计日志失败")
		return
	}
	items := make([]map[string]any, 0, len(logs))
	for _, l := range logs {
		items = append(items, map[string]any{
			"id": l.ID, "adminId": l.AdminID, "action": l.Action,
			"targetType": l.TargetType, "targetId": l.TargetID,
			"detail": l.Detail, "ip": l.IP, "createdAt": l.CreatedAt.UnixMilli(),
		})
	}
	writeOK(w, map[string]any{"items": items, "total": total, "page": page, "pageSize": pageSize})
}

// audit 写审计日志（失败仅记日志不阻断主流程）
func (s *Server) audit(r *http.Request, adminID int64, action, targetType, targetID, detail string) error {
	return s.st.AddAuditLog(r.Context(), store.AuditLog{
		AdminID: adminID, Action: action, TargetType: targetType,
		TargetID: targetID, Detail: detail, IP: clientIP(r),
	})
}

func pathID(r *http.Request) (int64, bool) {
	v := r.PathValue("id")
	id, err := strconv.ParseInt(v, 10, 64)
	return id, err == nil && id > 0
}
