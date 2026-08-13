// Package sms 短信通道：mock（开发/演示）与阿里云（预留实现）
package sms

import (
	"context"
	"fmt"
	"log"
)

// Sender 短信发送接口
type Sender interface {
	// Send 发送验证码；失败返回错误
	Send(ctx context.Context, phone, code string) error
	// Name 通道名
	Name() string
}

// Mock 模拟短信：验证码打印到服务端日志，方便本地联调
type Mock struct{}

func (Mock) Name() string { return "mock" }

func (Mock) Send(_ context.Context, phone, code string) error {
	log.Printf("[SMS][MOCK] 发送验证码 phone=%s code=%s（模拟通道，未真正下发）", phone, code)
	return nil
}

// Aliyun 阿里云短信（需配置 AccessKey/签名/模板；未配置时返回明确错误）
type Aliyun struct {
	AccessKeyID     string
	AccessKeySecret string
	SignName        string
	TemplateCode    string
}

func (a Aliyun) Name() string { return "aliyun" }

func (a Aliyun) Send(_ context.Context, phone, code string) error {
	if a.AccessKeyID == "" || a.AccessKeySecret == "" || a.SignName == "" || a.TemplateCode == "" {
		return fmt.Errorf("阿里云短信未配置完整（AccessKey/签名/模板），无法发送到 %s", phone)
	}
	// TODO(生产): 接入阿里云短信 API（dysmsapi）。
	// 参考: https://help.aliyun.com/zh/sms/
	return fmt.Errorf("阿里云短信通道尚未接入 SDK，请先在配置中提供凭据并完成接入")
}

// New 按配置创建短信发送器
func New(mode string, aliyunCfg Aliyun) Sender {
	switch mode {
	case "aliyun":
		return aliyunCfg
	default:
		return Mock{}
	}
}
