package license

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// cacheDirFn 可替换（测试注入临时目录）
var cacheDirFn = defaultCacheDir

// defaultCacheDir 返回本地授权缓存目录（%LOCALAPPDATA%\超能战士）
func defaultCacheDir() string {
	base, err := os.UserConfigDir()
	if err != nil || base == "" {
		base = os.TempDir()
	}
	return filepath.Join(base, "超能战士")
}

// cacheDir 返回缓存目录
func cacheDir() string {
	return cacheDirFn()
}

// ensureDir 确保目录存在
func ensureDir(dir string) error {
	return os.MkdirAll(dir, 0o755)
}

// machineGuidRE 解析 reg query 输出中的 MachineGuid
var machineGuidRE = regexp.MustCompile(`(?i)MachineGuid\s+REG_SZ\s+([0-9a-fA-F-]{20,})`)

// readMachineGuid 从 Windows 注册表读取 MachineGuid（跨重装稳定）
func readMachineGuid() string {
	out, err := exec.Command("reg", "query", `HKLM\SOFTWARE\Microsoft\Cryptography`, "/v", "MachineGuid").Output()
	if err != nil {
		log.Printf("[License] 读取 MachineGuid 失败: %v", err)
		return ""
	}
	if m := machineGuidRE.FindStringSubmatch(string(out)); len(m) == 2 {
		return strings.ToUpper(strings.TrimSpace(m[1]))
	}
	return ""
}

// fallbackDeviceID 生成持久化设备号（注册表不可读时兜底，存 %LOCALAPPDATA%）
func fallbackDeviceID() string {
	dir := cacheDir()
	if err := ensureDir(dir); err != nil {
		return randomID()
	}
	path := filepath.Join(dir, "device.id")
	if data, err := os.ReadFile(path); err == nil {
		id := strings.TrimSpace(string(data))
		if len(id) == 32 {
			return id
		}
	}
	id := randomID()
	if err := os.WriteFile(path, []byte(id), 0o600); err != nil {
		log.Printf("[License] 写入设备号失败: %v", err)
	}
	return id
}

// randomID 生成 32 位十六进制随机 ID
func randomID() string {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		// 极端情况下用时间戳兜底（不影响主流程）
		return fmt.Sprintf("%032x", sha256.Sum256([]byte("quant-fallback")))
	}
	return hex.EncodeToString(buf)
}

// DeviceID 返回稳定的设备唯一标识（一机一号核心）
// 优先级：MachineGuid > 本地持久化随机 ID
func DeviceID() string {
	guid := readMachineGuid()
	if guid != "" {
		sum := sha256.Sum256([]byte("quant-device-v1|" + guid))
		return hex.EncodeToString(sum[:16])
	}
	return fallbackDeviceID()
}
