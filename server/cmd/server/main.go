// 超能战士授权服务端入口
// 用法示例：
//   dev（内存库 + 模拟短信，开箱即用）:
//     go run ./cmd/server
//   prod（MySQL + 模拟短信）:
//     ENV=prod DB_DRIVER=mysql DB_DSN="user:pass@tcp(127.0.0.1:3306)/quant_server" \
//     JWT_SECRET=... go run ./cmd/server
package main

import (
	"context"
	"log"
	"net/http"
	"time"

	"quant-server/internal/api"
	"quant-server/internal/config"
	"quant-server/internal/security"
	"quant-server/internal/sms"
	"quant-server/internal/store"
)

func main() {
	cfg := config.Load()

	st, err := openStore(cfg)
	if err != nil {
		log.Fatalf("数据层初始化失败: %v", err)
	}
	defer st.Close()

	if err := bootstrapAdmin(cfg, st); err != nil {
		log.Fatalf("初始化管理员失败: %v", err)
	}

	sender := sms.New(cfg.SMSMode, sms.Aliyun{
		AccessKeyID:     cfg.AliyunAccessKeyID,
		AccessKeySecret: cfg.AliyunAccessKeySecret,
		SignName:        cfg.AliyunSignName,
		TemplateCode:    cfg.AliyunTemplateCode,
	})

	srv := api.New(cfg, st, sender)
	httpServer := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           srv.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	log.Printf("超能战士授权服务启动 env=%s db=%s sms=%s addr=%s",
		cfg.Env, cfg.DBDriver, cfg.SMSMode, cfg.ListenAddr)
	if cfg.IsProd() && cfg.JWTSecret == "quant-server-dev-secret-change-me" {
		log.Printf("⚠ 生产环境请务必通过 JWT_SECRET 设置随机密钥")
	}
	if err := httpServer.ListenAndServe(); err != nil {
		log.Fatalf("服务退出: %v", err)
	}
}

func openStore(cfg config.Config) (store.Store, error) {
	switch cfg.DBDriver {
	case "mysql":
		if cfg.DBDSN == "" {
			log.Fatalf("使用 MySQL 时必须设置 DB_DSN（格式: user:pass@tcp(host:port)/dbname）")
		}
		return store.NewMySQL(cfg.DBDSN)
	case "memory":
		log.Printf("使用内存数据层（仅限开发/测试；生产环境请配置 MySQL）")
		return store.NewMemory(), nil
	default:
		log.Fatalf("不支持的 DB_DRIVER: %s（可选 mysql / memory）", cfg.DBDriver)
		return nil, nil
	}
}

// bootstrapAdmin 首次启动创建初始管理员（强制首次登录改密）
func bootstrapAdmin(cfg config.Config, st store.Store) error {
	ctx := context.Background()
	n, err := st.CountAdmins(ctx)
	if err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	hash, err := security.HashPassword(cfg.AdminPassword)
	if err != nil {
		return err
	}
	if _, err := st.CreateAdmin(ctx, cfg.AdminUsername, hash, true); err != nil {
		return err
	}
	log.Printf("已创建初始管理员 %s（首次登录需修改密码）", cfg.AdminUsername)
	if !cfg.IsProd() {
		log.Printf("开发模式初始密码: %s（生产环境请通过 ADMIN_PASSWORD 设置并立即修改）", cfg.AdminPassword)
	}
	return nil
}
