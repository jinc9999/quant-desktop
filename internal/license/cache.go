package license

import (
	"encoding/json"
	"log"
	"os"
	"path/filepath"
)

// cacheFileName 授权缓存文件名
const cacheFileName = "license.json"

// Cache 本地授权缓存（服务端签名 token + 到期时间 + 时间偏移）
type Cache struct {
	Phone          string `json:"phone"`
	DeviceID       string `json:"deviceId"`
	Token          string `json:"token"`
	ServiceUntilMS int64  `json:"serviceUntilMs"`
	// ServerOffsetMS 记录最近一次同步时（服务器时间 - 本地时间）的偏移，
	// 到期判定用校准时间，防止修改本机时间绕过授权。
	ServerOffsetMS int64  `json:"serverOffsetMs"`
	LastSyncMS     int64  `json:"lastSyncMs"`
	NeedsPassword  bool   `json:"needsPassword"`
	Profile        string `json:"profile"`
}

// cachePath 返回缓存文件完整路径
func cachePath() string {
	return filepath.Join(cacheDir(), cacheFileName)
}

// LoadCache 加载本地授权缓存（不存在或损坏时返回空缓存，不报错）
func LoadCache() Cache {
	var c Cache
	data, err := os.ReadFile(cachePath())
	if err != nil {
		return c
	}
	if err := json.Unmarshal(data, &c); err != nil {
		log.Printf("[License] 授权缓存解析失败（按未登录处理）: %v", err)
		return Cache{}
	}
	if c.Profile == "" {
		c.Profile = "A"
	}
	return c
}

// Save 原子写入缓存（临时文件 + 重命名，避免写一半损坏）
func (c *Cache) Save() error {
	dir := cacheDir()
	if err := ensureDir(dir); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, cacheFileName+".tmp")
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, cachePath())
}
