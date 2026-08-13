// Package security 密码哈希（PBKDF2-SHA256，Go 1.24+ 标准库）与 JWT（HMAC-SHA256）
package security

import (
	"crypto/hmac"
	"crypto/pbkdf2"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

const (
	pbkdf2Iterations = 600000
	keyLen           = 32
)

// HashPassword 生成 PBKDF2 密码哈希，格式：pbkdf2-sha256$迭代$盐$哈希
func HashPassword(password string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	dk, err := pbkdf2.Key(sha256.New, password, salt, pbkdf2Iterations, keyLen)
	if err != nil {
		return "", err
	}
	return fmt.Sprintf("pbkdf2-sha256$%d$%s$%s",
		pbkdf2Iterations,
		base64.RawStdEncoding.EncodeToString(salt),
		base64.RawStdEncoding.EncodeToString(dk)), nil
}

// VerifyPassword 校验密码（常数时间比较）
func VerifyPassword(hash, password string) bool {
	parts := strings.Split(hash, "$")
	if len(parts) != 4 || parts[0] != "pbkdf2-sha256" {
		return false
	}
	iter, err := strconv.Atoi(parts[1])
	if err != nil || iter <= 0 {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[2])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[3])
	if err != nil {
		return false
	}
	got, err := pbkdf2.Key(sha256.New, password, salt, iter, len(want))
	if err != nil {
		return false
	}
	return subtle.ConstantTimeCompare(got, want) == 1
}

// ValidatePasswordStrength 密码强度：8-20 位且同时包含字母与数字
func ValidatePasswordStrength(password string) error {
	if len(password) < 8 || len(password) > 20 {
		return errors.New("密码长度需为 8-20 位")
	}
	hasLetter := false
	hasDigit := false
	for _, r := range password {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z':
			hasLetter = true
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	if !hasLetter || !hasDigit {
		return errors.New("密码需同时包含字母和数字")
	}
	return nil
}

// Claims JWT 载荷
type Claims struct {
	Sub        int64  `json:"sub"`        // 用户/管理员 ID
	Role       string `json:"role"`       // customer | admin
	JTI        string `json:"jti"`        // 会话 ID
	IAT        int64  `json:"iat"`        // 签发时间（秒）
	EXP        int64  `json:"exp"`        // 过期时间（秒）
	Phone      string `json:"phone,omitempty"`
	DeviceID   string `json:"device,omitempty"`
	MustChange bool   `json:"mcp,omitempty"` // 管理员首次登录强制改密
}

type jwtHeader struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
}

func b64url(b []byte) string {
	return base64.RawURLEncoding.EncodeToString(b)
}

func b64urlDecode(s string) ([]byte, error) {
	return base64.RawURLEncoding.DecodeString(s)
}

// SignToken 签发 HS256 JWT
func SignToken(secret string, c Claims) (string, error) {
	h, err := json.Marshal(jwtHeader{Alg: "HS256", Typ: "JWT"})
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	unsigned := b64url(h) + "." + b64url(payload)
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	sig := b64url(mac.Sum(nil))
	return unsigned + "." + sig, nil
}

// VerifyToken 校验并解析 JWT
func VerifyToken(secret, token string) (*Claims, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return nil, errors.New("token 格式错误")
	}
	unsigned := parts[0] + "." + parts[1]
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(unsigned))
	want := mac.Sum(nil)
	got, err := b64urlDecode(parts[2])
	if err != nil || len(got) != len(want) || hmac.Equal(got, want) == false {
		return nil, errors.New("token 签名无效")
	}
	payload, err := b64urlDecode(parts[1])
	if err != nil {
		return nil, errors.New("token 载荷损坏")
	}
	var c Claims
	if err := json.Unmarshal(payload, &c); err != nil {
		return nil, errors.New("token 载荷解析失败")
	}
	if c.EXP > 0 && time.Now().Unix() > c.EXP {
		return nil, errors.New("token 已过期")
	}
	return &c, nil
}

// RandomToken 生成随机会话 ID / 验证码辅助
func RandomToken() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 36)
	}
	return hex.EncodeToString(buf)
}
