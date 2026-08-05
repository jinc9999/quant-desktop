// Package storage 凭据加密存储
// 使用 AES-256-GCM 对 API Key/Secret 进行加密后存入 SQLite，
// 加密密钥由机器特征（hostname + username）派生，确保仅本机可解密。
package storage

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"os"
	"os/user"
	"strings"
)

// deriveKey 从机器特征派生 32 字节 AES 密钥
// 使用 hostname + username + 固定盐值，经 SHA-256 哈希得到密钥
// 返回 32 字节密钥
func deriveKey() []byte {
	hostname, _ := os.Hostname()
	u, _ := user.Current()
	username := ""
	if u != nil {
		username = u.Username
	}
	raw := hostname + "|" + username + "|quant-desktop-salt-v1"
	h := sha256.Sum256([]byte(raw))
	return h[:]
}

// Encrypt 使用 AES-256-GCM 加密明文
// plaintext: 待加密的明文字符串
// 返回 base64 编码的密文（nonce + ciphertext），error 错误信息
func Encrypt(plaintext string) (string, error) {
	if plaintext == "" {
		return "", nil
	}
	key := deriveKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("创建 AES cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM 失败: %w", err)
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("生成 nonce 失败: %w", err)
	}
	ciphertext := gcm.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

// Decrypt 使用 AES-256-GCM 解密密文
// encoded: base64 编码的密文（nonce + ciphertext）
// 返回解密后的明文字符串，error 错误信息
func Decrypt(encoded string) (string, error) {
	if encoded == "" {
		return "", nil
	}
	data, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("base64 解码失败: %w", err)
	}
	key := deriveKey()
	block, err := aes.NewCipher(key)
	if err != nil {
		return "", fmt.Errorf("创建 AES cipher 失败: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return "", fmt.Errorf("创建 GCM 失败: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(data) < nonceSize {
		return "", fmt.Errorf("密文数据过短")
	}
	nonce, ciphertext := data[:nonceSize], data[nonceSize:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return "", fmt.Errorf("解密失败: %w", err)
	}
	return string(plaintext), nil
}

// GetKeyValue 从 strategy_config 表中获取指定键的值
// 参数:
//   - key: 配置键名
//
// 返回:
//   - string: 对应的值；键不存在时返回空字符串
//   - error: 查询失败时返回错误（键不存在不视为错误）
func (db *DB) GetKeyValue(key string) (string, error) {
	var value string
	err := db.Conn.QueryRow(`SELECT value FROM strategy_config WHERE key = ?`, key).Scan(&value)
	if err != nil {
		if err.Error() == "sql: no rows in result set" {
			return "", nil
		}
		return "", err
	}
	return value, nil
}

// SetKeyValue 向 strategy_config 表中设置键值对（存在则更新，不存在则插入）
// 参数:
//   - key: 配置键名
//   - value: 配置值
//
// 返回: error 操作失败时返回错误
func (db *DB) SetKeyValue(key, value string) error {
	_, err := db.Conn.Exec(
		`INSERT INTO strategy_config (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`,
		key, value,
	)
	return err
}

// SaveCredentials 加密并保存指定模式的 API 凭据
// mode: 运行模式（SIMULATION / LIVE）
// apiKey: API Key 明文
// apiSecret: API Secret 明文
// 返回 error 错误信息
func (db *DB) SaveCredentials(mode, apiKey, apiSecret string) error {
	encKey, err := Encrypt(apiKey)
	if err != nil {
		return fmt.Errorf("加密 API Key 失败: %w", err)
	}
	encSecret, err := Encrypt(apiSecret)
	if err != nil {
		return fmt.Errorf("加密 API Secret 失败: %w", err)
	}

	tx, err := db.Conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO strategy_config (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	if _, err := stmt.Exec("cred:"+mode+":apiKey", encKey); err != nil {
		return err
	}
	if _, err := stmt.Exec("cred:"+mode+":apiSecret", encSecret); err != nil {
		return err
	}

	return tx.Commit()
}

// LoadCredentials 加载并解密指定模式的 API 凭据
// mode: 运行模式（SIMULATION / LIVE）
// 返回 apiKey 明文, apiSecret 明文, error 错误信息
func (db *DB) LoadCredentials(mode string) (string, string, error) {
	var encKey, encSecret string

	err := db.Conn.QueryRow(`SELECT value FROM strategy_config WHERE key = ?`, "cred:"+mode+":apiKey").Scan(&encKey)
	if err != nil && err.Error() != "sql: no rows in result set" {
		return "", "", fmt.Errorf("查询 API Key 失败: %w", err)
	}

	err = db.Conn.QueryRow(`SELECT value FROM strategy_config WHERE key = ?`, "cred:"+mode+":apiSecret").Scan(&encSecret)
	if err != nil && err.Error() != "sql: no rows in result set" {
		return "", "", fmt.Errorf("查询 API Secret 失败: %w", err)
	}

	apiKey, err := Decrypt(encKey)
	if err != nil {
		return "", "", fmt.Errorf("解密 API Key 失败: %w", err)
	}
	apiSecret, err := Decrypt(encSecret)
	if err != nil {
		return "", "", fmt.Errorf("解密 API Secret 失败: %w", err)
	}

	return apiKey, apiSecret, nil
}

// SaveProxyConfig 保存代理配置
// address: 代理服务器地址（如 127.0.0.1）
// port: 代理端口号（如 7890），0 表示不使用代理
// 返回 error 错误信息
func (db *DB) SaveProxyConfig(address string, port int) error {
	tx, err := db.Conn.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	stmt, err := tx.Prepare(`INSERT INTO strategy_config (key, value) VALUES (?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value`)
	if err != nil {
		return err
	}
	defer stmt.Close()

	if _, err := stmt.Exec("proxy:address", address); err != nil {
		return err
	}
	if _, err := stmt.Exec("proxy:port", fmt.Sprintf("%d", port)); err != nil {
		return err
	}

	return tx.Commit()
}

// LoadProxyConfig 加载代理配置
// 返回 address 代理地址, port 代理端口, error 错误信息
func (db *DB) LoadProxyConfig() (string, int, error) {
	var address, portStr string

	err := db.Conn.QueryRow(`SELECT value FROM strategy_config WHERE key = ?`, "proxy:address").Scan(&address)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		return "", 0, fmt.Errorf("查询代理地址失败: %w", err)
	}

	err = db.Conn.QueryRow(`SELECT value FROM strategy_config WHERE key = ?`, "proxy:port").Scan(&portStr)
	if err != nil && !strings.Contains(err.Error(), "no rows") {
		return "", 0, fmt.Errorf("查询代理端口失败: %w", err)
	}

	port := 0
	if portStr != "" {
		fmt.Sscanf(portStr, "%d", &port)
	}

	return address, port, nil
}
