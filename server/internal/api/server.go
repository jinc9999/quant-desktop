// Package api 授权服务 HTTP API 与 Web 管理后台
package api

import (
	"context"
	"encoding/json"
	"log"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"quant-server/internal/config"
	"quant-server/internal/security"
	"quant-server/internal/sms"
	"quant-server/internal/store"
)

// 业务错误码（与客户端约定）
const (
	CodeOK           = 0
	CodeBadRequest   = 4000
	CodeUnauthorized = 4001
	CodeForbidden    = 4003
	CodeDeviceBound  = 4004
	CodeCodeInvalid  = 4005
	CodeRateLimited  = 4029
	CodeNotFound     = 4040
	CodeServer       = 5000
)

var phoneRE = regexp.MustCompile(`^1[3-9]\d{9}$`)

// Server API 服务
type Server struct {
	cfg     config.Config
	st      store.Store
	sms     sms.Sender
	limiter *RateLimiter
}

// New 创建 API 服务
func New(cfg config.Config, st store.Store, sender sms.Sender) *Server {
	return &Server{
		cfg:     cfg,
		st:      st,
		sms:     sender,
		limiter: NewRateLimiter(),
	}
}

// ctxKey 上下文键
type ctxKey string

const (
	ctxCustomer = ctxKey("customer")
	ctxAdmin    = ctxKey("admin")
	ctxClaims   = ctxKey("claims")
)

// Handler 返回路由
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	// 健康检查
	mux.HandleFunc("GET /healthz", s.handleHealth)

	// 客户端 API
	mux.HandleFunc("POST /api/v1/sms/send", s.handleSendSMS)
	mux.HandleFunc("POST /api/v1/auth/login", s.handleLogin)
	mux.HandleFunc("POST /api/v1/auth/password", s.requireCustomer(s.handleSetPassword))
	mux.HandleFunc("GET /api/v1/license", s.requireCustomer(s.handleLicense))
	mux.HandleFunc("POST /api/v1/auth/logout", s.requireCustomer(s.handleLogout))

	// 管理端 API
	mux.HandleFunc("POST /api/v1/admin/login", s.handleAdminLogin)
	mux.HandleFunc("POST /api/v1/admin/password", s.requireAdmin(s.handleAdminPassword))
	mux.HandleFunc("GET /api/v1/admin/customers", s.requireAdmin(s.handleAdminListCustomers))
	mux.HandleFunc("POST /api/v1/admin/customers", s.requireAdmin(s.handleAdminCreateCustomer))
	mux.HandleFunc("GET /api/v1/admin/customers/{id}", s.requireAdmin(s.handleAdminCustomerDetail))
	mux.HandleFunc("POST /api/v1/admin/customers/{id}/grant", s.requireAdmin(s.handleAdminGrant))
	mux.HandleFunc("POST /api/v1/admin/customers/{id}/unbind-device", s.requireAdmin(s.handleAdminUnbindDevice))
	mux.HandleFunc("POST /api/v1/admin/customers/{id}/disable", s.requireAdmin(s.handleAdminDisable))
	mux.HandleFunc("POST /api/v1/admin/customers/{id}/enable", s.requireAdmin(s.handleAdminEnable))
	mux.HandleFunc("GET /api/v1/admin/sms-codes", s.requireAdmin(s.handleAdminSMSCodes))
	mux.HandleFunc("GET /api/v1/admin/audit-logs", s.requireAdmin(s.handleAdminAuditLogs))

	// 管理后台静态页面
	mux.HandleFunc("GET /", s.handleAdminWeb)
	mux.HandleFunc("GET /static/", s.handleAdminWeb)

	return s.recoverMiddleware(s.logMiddleware(mux))
}

// ---- 通用工具 ----

// envelope 统一响应
type envelope struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func writeJSON(w http.ResponseWriter, status, code int, message string, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(envelope{Code: code, Message: message, Data: data})
}

func writeOK(w http.ResponseWriter, data any) {
	writeJSON(w, http.StatusOK, CodeOK, "ok", data)
}

func writeErr(w http.ResponseWriter, status, code int, message string) {
	writeJSON(w, status, code, message, nil)
}

func decodeJSON(r *http.Request, out any) error {
	defer r.Body.Close()
	dec := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 1<<20))
	dec.DisallowUnknownFields()
	return dec.Decode(out)
}

func clientIP(r *http.Request) string {
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		return strings.TrimSpace(strings.Split(xff, ",")[0])
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

func nowMS() int64 {
	return time.Now().UnixMilli()
}

// ---- 中间件 ----

func (s *Server) logMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: 200}
		next.ServeHTTP(sw, r)
		log.Printf("[HTTP] %s %s %d %s %s",
			clientIP(r), r.Method, sw.status, r.URL.Path, time.Since(start).Round(time.Millisecond))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("[PANIC] %v", rec)
				writeErr(w, http.StatusInternalServerError, CodeServer, "服务器内部错误")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

// bearerToken 提取 Authorization: Bearer xxx
func bearerToken(r *http.Request) string {
	h := r.Header.Get("Authorization")
	if len(h) > 7 && strings.EqualFold(h[:7], "Bearer ") {
		return strings.TrimSpace(h[7:])
	}
	return ""
}

func (s *Server) requireCustomer(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := security.VerifyToken(s.cfg.JWTSecret, bearerToken(r))
		if err != nil || claims.Role != "customer" {
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "登录已失效，请重新登录")
			return
		}
		sess, err := s.st.GetSession(r.Context(), claims.JTI)
		if err != nil || sess.Revoked || sess.CustomerID != claims.Sub {
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "会话已失效，请重新登录")
			return
		}
		c, err := s.st.GetCustomerByID(r.Context(), claims.Sub)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "账号不存在")
			return
		}
		if c.Status != store.CustomerStatusActive {
			writeErr(w, http.StatusForbidden, CodeForbidden, "账号已停用，请联系管理员")
			return
		}
		ctx := context.WithValue(r.Context(), ctxCustomer, c)
		ctx = context.WithValue(ctx, ctxClaims, claims)
		next(w, r.WithContext(ctx))
	}
}

func (s *Server) requireAdmin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		claims, err := security.VerifyToken(s.cfg.JWTSecret, bearerToken(r))
		if err != nil || claims.Role != "admin" {
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "管理员登录已失效，请重新登录")
			return
		}
		sess, err := s.st.GetSession(r.Context(), claims.JTI)
		if err != nil || sess.Revoked {
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "会话已失效，请重新登录")
			return
		}
		a, err := s.getAdminByID(r.Context(), claims.Sub)
		if err != nil {
			writeErr(w, http.StatusUnauthorized, CodeUnauthorized, "管理员不存在")
			return
		}
		if a.MustChangePassword && r.URL.Path != "/api/v1/admin/password" {
			writeErr(w, http.StatusForbidden, CodeForbidden, "请先修改初始密码")
			return
		}
		ctx := context.WithValue(r.Context(), ctxAdmin, a)
		ctx = context.WithValue(ctx, ctxClaims, claims)
		next(w, r.WithContext(ctx))
	}
}

// getAdminByID 内存/MySQL 通用按 ID 查询管理员
func (s *Server) getAdminByID(ctx context.Context, id int64) (*store.Admin, error) {
	// 通过用户名清单轮询的方式不可行，Store 增加按 ID 查询能力
	return s.st.GetAdminByID(ctx, id)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if err := s.st.Ping(r.Context()); err != nil {
		writeErr(w, http.StatusServiceUnavailable, CodeServer, "数据库不可用")
		return
	}
	writeOK(w, map[string]string{"status": "ok"})
}

// RateLimiter 简易内存限频器（按 key 计数窗口）
type RateLimiter struct {
	mu    sync.Mutex
	hits  map[string][]time.Time
	clean time.Time
}

// NewRateLimiter 创建限频器
func NewRateLimiter() *RateLimiter {
	return &RateLimiter{hits: map[string][]time.Time{}}
}

// Allow 判断是否放行；窗口内最多 max 次
func (l *RateLimiter) Allow(key string, window time.Duration, max int) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	if now.Sub(l.clean) > 10*time.Minute {
		for k, v := range l.hits {
			if len(v) == 0 || now.Sub(v[len(v)-1]) > time.Hour {
				delete(l.hits, k)
			}
		}
		l.clean = now
	}
	list := l.hits[key]
	cutoff := now.Add(-window)
	var kept []time.Time
	for _, t := range list {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= max {
		l.hits[key] = kept
		return false
	}
	l.hits[key] = append(kept, now)
	return true
}
