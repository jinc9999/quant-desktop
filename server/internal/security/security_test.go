package security

import (
	"strings"
	"testing"
	"time"
)

func TestPasswordHashVerify(t *testing.T) {
	hash, err := HashPassword("Abc123456")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "pbkdf2-sha256$") {
		t.Fatalf("哈希格式异常: %s", hash)
	}
	if !VerifyPassword(hash, "Abc123456") {
		t.Fatal("正确密码应通过校验")
	}
	if VerifyPassword(hash, "Abc123457") {
		t.Fatal("错误密码不应通过校验")
	}
	// 相同密码两次哈希不同（随机盐）
	hash2, _ := HashPassword("Abc123456")
	if hash == hash2 {
		t.Fatal("哈希应带随机盐")
	}
}

func TestPasswordStrength(t *testing.T) {
	cases := []struct {
		pw  string
		ok  bool
	}{
		{"short1", false},        // 过短
		{"abcdefgh", false},      // 无数字
		{"12345678", false},      // 无字母
		{"Abc12345", true},       // 达标
		{"Abc123456789012345678", false}, // 超长（21 位）
	}
	for _, c := range cases {
		err := ValidatePasswordStrength(c.pw)
		if (err == nil) != c.ok {
			t.Fatalf("密码 %q: 期望 ok=%v 实际 %v", c.pw, c.ok, err)
		}
	}
}

func TestJWT_TamperRejected(t *testing.T) {
	claims := Claims{
		Sub: 1, Role: "customer", JTI: "jti-1",
		IAT: time.Now().Unix(), EXP: time.Now().Add(time.Hour).Unix(),
	}
	token, err := SignToken("secret-a", claims)
	if err != nil {
		t.Fatal(err)
	}
	got, err := VerifyToken("secret-a", token)
	if err != nil || got.Sub != 1 || got.Role != "customer" {
		t.Fatalf("合法 token 应通过: %v %v", got, err)
	}
	if _, err := VerifyToken("secret-b", token); err == nil {
		t.Fatal("错误密钥应拒绝")
	}
	// 篡改载荷
	parts := strings.Split(token, ".")
	tampered := parts[0] + ".eyJzdWIiOjJ9." + parts[2]
	if _, err := VerifyToken("secret-a", tampered); err == nil {
		t.Fatal("篡改载荷应拒绝")
	}
	// 过期 token
	expiredClaims := claims
	expiredClaims.EXP = time.Now().Add(-time.Minute).Unix()
	et, _ := SignToken("secret-a", expiredClaims)
	if _, err := VerifyToken("secret-a", et); err == nil {
		t.Fatal("过期 token 应拒绝")
	}
}
