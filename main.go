package main

// 前端构建产物（frontend/dist，由 pnpm build 生成）由下面的 embed 指令嵌入
import (
	"embed"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"quant-desktop/internal/bindings"

	"github.com/wailsapp/wails/v3/pkg/application"
)

//go:embed frontend/dist/*
var assets embed.FS

// acquireSingleInstanceLock 单实例锁：保证同一时刻只有一个客户端窗口。
// 锁文件存于系统临时目录、内容为 PID；启动时若锁被存活进程占用则直接退出。
// 背景（2026-08-04 定调）：曾因 go run 孤儿进程 / 开发版应用共存同时出现 3 个窗口，
// 多实例还会并发写同一个 SQLite 库。此后无论用何种方式启动，都只能有一个实例。
func acquireSingleInstanceLock() (func(), error) {
	lockPath := filepath.Join(os.TempDir(), "quant-desktop.lock")
	// 已有锁文件：检查持有进程是否存活（进程存活探测按平台实现：Unix 用信号 0，Windows 用 OpenProcess）
	if data, err := os.ReadFile(lockPath); err == nil {
		if pid, perr := strconv.Atoi(strings.TrimSpace(string(data))); perr == nil {
			if isProcessAlive(pid) {
				return nil, fmt.Errorf("检测到量化交易客户端已在运行（PID %d），请勿重复启动", pid)
			}
		}
		// 持有进程已退出（崩溃残留的锁），抢占重建
		os.Remove(lockPath)
	}
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, fmt.Errorf("创建单实例锁失败: %v", err)
	}
	fmt.Fprintf(f, "%d", os.Getpid())
	f.Close()
	return func() { os.Remove(lockPath) }, nil
}

func main() {
	// 单实例锁：重复启动直接退出，防止多窗口/多实例并发写库
	releaseLock, err := acquireSingleInstanceLock()
	if err != nil {
		log.Fatal(err)
	}
	defer releaseLock()

	// 创建量化服务
	quantService := bindings.NewQuantService()
	if err := quantService.Init(); err != nil {
		log.Fatalf("初始化失败: %v", err)
	}
	defer quantService.Shutdown()

	// 创建 Wails 应用
	app := application.New(application.Options{
		Name:        "量化交易客户端",
		Description: "币安合约量化交易桌面客户端",
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
	})

	// 将 app 引用注入 QuantService，用于后台错误事件推送
	quantService.SetApp(app)

	// 创建主窗口
	// 使用默认标题栏，避免 macOS 窗口控制按钮与网页内容重叠
	app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:  "量化交易客户端",
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
