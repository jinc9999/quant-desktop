// Package config 授权服务配置（环境变量驱动）
package config

import (
	"os"
	"strconv"

	"quant-server/internal/store"
)

// Config 服务配置
type Config struct {
	Env          string // dev | prod
	ListenAddr   string
	DBDriver     string // mysql | memory（memory 仅限 dev/测试）
	DBDSN        string
	JWTSecret    string
	// 初始管理员（仅首次启动建库时生效）
	AdminUsername string
	AdminPassword string
	// 短信
	SMSMode   string // mock | aliyun
	AliyunAccessKeyID     string
	AliyunAccessKeySecret string
	AliyunSignName        string
	AliyunTemplateCode    string
	// 模拟验证码固定值（mock 模式，测试用；空则随机 6 位）
	MockSMSCode string
	// AutoTrialPeriod 客户首次验证码注册自动赠送的服务周期（如 3d）；
	// 空或非法值表示不自动赠送
	AutoTrialPeriod string
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func getenvInt(key string, def int) int {
	if v := os.Getenv(key); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}

// Load 从环境变量加载配置（dev 默认值保证开箱即用）
func Load() Config {
	env := getenv("ENV", "dev")
	cfg := Config{
		Env:               env,
		ListenAddr:        ":" + strconv.Itoa(getenvInt("PORT", 8081)),
		DBDriver:          getenv("DB_DRIVER", ""),
		DBDSN:             os.Getenv("DB_DSN"),
		JWTSecret:         getenv("JWT_SECRET", "quant-server-dev-secret-change-me"),
		AdminUsername:     getenv("ADMIN_USERNAME", "admin"),
		AdminPassword:     getenv("ADMIN_PASSWORD", "Admin@123456"),
		SMSMode:           getenv("SMS_MODE", "mock"),
		AliyunAccessKeyID: os.Getenv("ALIYUN_ACCESS_KEY_ID"),
		AliyunAccessKeySecret: os.Getenv("ALIYUN_ACCESS_KEY_SECRET"),
		AliyunSignName:        os.Getenv("ALIYUN_SIGN_NAME"),
		AliyunTemplateCode:    os.Getenv("ALIYUN_TEMPLATE_CODE"),
		MockSMSCode:           getenv("MOCK_SMS_CODE", "123456"),
		AutoTrialPeriod:       getenv("AUTO_TRIAL_PERIOD", "3d"),
	}
	if !store.ValidPeriod(cfg.AutoTrialPeriod) {
		cfg.AutoTrialPeriod = ""
	}
	if cfg.DBDriver == "" {
		if env == "prod" {
			cfg.DBDriver = "mysql"
		} else {
			cfg.DBDriver = "memory"
		}
	}
	if env == "prod" && cfg.DBDriver == "memory" {
		cfg.DBDriver = "mysql"
	}
	return cfg
}

// IsProd 是否生产环境
func (c Config) IsProd() bool {
	return c.Env == "prod"
}

// IsMockSMS 是否模拟短信
func (c Config) IsMockSMS() bool {
	return c.SMSMode == "mock"
}

// AutoTrialEnabled 是否启用首次注册自动试用
func (c Config) AutoTrialEnabled() bool {
	return store.ValidPeriod(c.AutoTrialPeriod)
}
