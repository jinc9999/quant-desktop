package main

// 前端构建产物（frontend/dist，由 pnpm build 生成）由下面的 embed 指令嵌入
import (
	"embed"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"quant-desktop/internal/bindings"
	"quant-desktop/internal/product"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed frontend/dist/*
var assets embed.FS

// acquireSingleInstanceLock 单实例锁：保证同一份可执行文件在同一启动模式下只有一个实例在运行。
// 锁文件位于系统临时目录，文件名由可执行文件完整路径哈希生成（同一副本共用一把锁，
// 不同路径的副本各自独立、互不干扰）；文件内容为 PID + 可执行文件路径。
// modeKey: "sim"/"live"/"any"。固定模式启动时模拟盘与实盘各占一把锁，
// 可在同一台电脑并行（数据库按模式独立，互不冲突）；未固定时 "any"，行为与旧版一致。
// 启动时若锁被存活进程占用则直接退出；持有进程已退出（崩溃残留的锁）则抢占重建。
// 背景（2026-08-04 定调 + 2026-08-07 加固）：曾因 go run 孤儿进程 / 开发版应用共存
// 同时出现 3 个窗口，多实例并发写同一个 SQLite 库；后又因手动复制旧版 exe 到
// D:\9999 运行，旧实例占用全局唯一锁（只存 PID）阻止新构建启动。
// 锁绑定可执行文件路径后，不同位置的副本互不影响，彻底解决跨副本锁冲突。
func acquireSingleInstanceLock(modeKey string) (func(), error) {
	// 以可执行文件完整路径区分实例：同一路径的副本共享一把锁（防多开），
	// 不同路径的副本（如 D:\9999 旧版）各自持有独立锁，不再互相阻塞。
	exe, err := os.Executable()
	if err != nil {
		exe = os.Args[0]
	}
	absExe, err := filepath.Abs(exe)
	if err != nil {
		absExe = exe
	}
	lockName := fmt.Sprintf("quant-desktop-%08x-%s.lock", fnv32(absExe), modeKey)
	lockPath := filepath.Join(os.TempDir(), lockName)

	// 已有锁文件：检查持有进程是否存活（进程存活探测按平台实现：Unix 用信号 0，Windows 用 OpenProcess）
	if data, err := os.ReadFile(lockPath); err == nil {
		lines := strings.SplitN(string(data), "\n", 2)
		if pid, perr := strconv.Atoi(strings.TrimSpace(lines[0])); perr == nil {
			if isProcessAlive(pid) {
				holder := ""
				if len(lines) == 2 {
					holder = "，占用实例: " + strings.TrimSpace(lines[1])
				}
				return nil, fmt.Errorf("检测到量化交易客户端已在运行（PID %d%s），请勿重复启动", pid, holder)
			}
		}
		// 持有进程已退出（崩溃残留的锁），抢占重建
		os.Remove(lockPath)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建单实例锁失败: %v", err)
	}
	fmt.Fprintf(f, "%d\n%s", os.Getpid(), absExe)
	f.Close()
	return func() { os.Remove(lockPath) }, nil
}

// fnv32 计算字符串的 32 位 FNV-1a 哈希，用于把可执行文件完整路径映射为锁文件名。
func fnv32(s string) uint32 {
	h := fnv.New32a()
	h.Write([]byte(s))
	return h.Sum32()
}

// startupModeFromEnv 读取 QUANT_START_MODE 环境变量，返回规范模式与锁标识。
// 用于同一台电脑同时运行同策略的模拟盘与实盘（例如 A 模拟盘 + A 实盘 + B 模拟盘 + B 实盘）。
// 返回值: (mode, modeKey)。mode 为空表示未固定（保持可切换）；modeKey 用于区分单实例锁。
func startupModeFromEnv() (mode, modeKey string) {
	switch strings.ToUpper(strings.TrimSpace(os.Getenv("QUANT_START_MODE"))) {
	case "SIMULATION":
		return "SIMULATION", "sim"
	case "LIVE":
		return "LIVE", "live"
	default:
		return "", "any"
	}
}

// debugBrowserArgs 诊断用 WebView2 附加参数（默认关闭）。
// 设置环境变量 QUANT_DEBUG_WEBVIEW=1 时开启远程调试端口，便于排查前端页面问题；
// 正常使用不开启，不构成任何暴露。
func debugBrowserArgs() []string {
	if os.Getenv("QUANT_DEBUG_WEBVIEW") == "1" {
		return []string{"--remote-debugging-port=9222"}
	}
	return nil
}

// initFileLog 把日志同时输出到文件（%APPDATA%\超能战士\client.log），便于排查客户端问题。
// 生产构建为 windowsgui 子系统，无控制台，落盘日志是唯一可追溯的运行记录。
func initFileLog() *os.File {
	base := os.Getenv("APPDATA")
	if base == "" {
		return nil
	}
	// 审查修复 G3（2026-08-13）：A/B/D/C 各自独立日志目录，避免四开时日志互相污染
	name := product.GetInfo().ProductName
	if name == "" {
		name = "量化交易客户端"
	}
	dir := filepath.Join(base, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil
	}
	f, err := os.OpenFile(filepath.Join(dir, "client.log"),
		os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil
	}
	// 注意顺序：文件在前。GUI 子系统下 os.Stderr 不可写，若 stderr 在前，
	// MultiWriter 会因 stderr 报错而停止，导致日志写不进文件。
	log.SetOutput(io.MultiWriter(f, os.Stderr))
	log.Printf("[Main] 日志文件已开启: %s", filepath.Join(dir, "client.log"))
	return f
}

func main() {
	logFile := initFileLog()
	if logFile != nil {
		defer logFile.Close()
	}

	// 启动模式固定（可选）：QUANT_START_MODE=SIMULATION/LIVE
	startMode, modeKey := startupModeFromEnv()

	// 单实例锁（按 exe 路径 + 启动模式）：防止同模式重复启动写同一个库；
	// 固定模式后模拟盘/实盘各占一把锁，可同机并行。
	releaseLock, err := acquireSingleInstanceLock(modeKey)
	if err != nil {
		log.Fatal(err)
	}
	defer releaseLock()
	log.Printf("[Main] 启动 %s 变体（%s）", product.Variant, product.GetInfo().ProductName)

	// 创建量化服务
	quantService := bindings.NewQuantServiceWithMode(startMode)
	if err := quantService.Init(); err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer quantService.Shutdown()

	// 创建 Wails 应用
	appTitle := "量化交易客户端"
	if product.IsC() {
		appTitle = product.GetInfo().ProductName // 超能战士
	}
	// 固定模式启动时标题附加模式标识，便于同时开 4 个窗口时区分
	if startMode == "SIMULATION" {
		appTitle += "（模拟盘）"
	} else if startMode == "LIVE" {
		appTitle += "（实盘）"
	}
	app := application.New(application.Options{
		Name:        appTitle,
		Description: "币安合约量化策略客户端（" + appTitle + "）",
		Services: []application.Service{
			application.NewService(quantService),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			// 关闭窗口后应用不退出，策略继续后台运行
			ApplicationShouldTerminateAfterLastWindowClosed: false,
		},
		Windows: application.WindowsOptions{
			// 诊断用：QUANT_DEBUG_WEBVIEW=1 时开启 WebView2 远程调试端口（默认关闭）
			AdditionalBrowserArgs: debugBrowserArgs(),
		},
	})

	// 将 app 引用注入 QuantService，用于后台错误事件推送
	quantService.SetApp(app)

	// 创建主窗口
	// 使用默认标题栏，避免 macOS 窗口控制按钮与网页内容重叠
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  appTitle,
		Width:  1280,
		Height: 800,
		Mac: application.MacWindow{
			TitleBar: application.MacTitleBarDefault,
		},
		BackgroundColour: application.NewRGB(20, 22, 27), // #14161b
		URL:              "/",
	})

	// 运行应用
	// 注：正常退出时由 main 返回后的 defer quantService.Shutdown() 执行资源清理；
	// 运行失败路径 log.Fatalf 内部调用 os.Exit(1) 不触发 defer，故先显式调用 Shutdown()。
	if err := app.Run(); err != nil {
		quantService.Shutdown()
		log.Fatalf("应用运行失败: %v", err)
	}
}
