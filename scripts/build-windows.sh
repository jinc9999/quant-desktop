#!/bin/bash
# Windows 打包脚本（macOS 交叉编译）
# 默认使用 Zig 本机交叉编译（推荐）：无需 Docker，产出 bin/quant-desktop.exe（含 SQLite）
#
# 使用方法：
#   ./scripts/build-windows.sh          # 默认：Zig 本机交叉编译（推荐）
#   ./scripts/build-windows.sh docker   # Docker + wails-cross 镜像（需 Docker Desktop 可用）
#   ./scripts/build-windows.sh native   # 原生交叉编译（CGO_ENABLED=0，无 SQLite，禁止分发）
#
# 前置条件：
#   - zig 已安装（brew install zig）
#   - wails3 CLI 已安装（~/.go/bin/wails3）
#   - pnpm 已安装（前端构建）
#   - 已执行: wails3 task common:build:frontend（构建前端产物）

set -e

cd "$(dirname "$0")/.."
PROJECT_ROOT=$(pwd)

echo "=== 量化交易客户端 Windows 打包 ==="
echo "项目目录: $PROJECT_ROOT"

# 确保 wails3 在 PATH 中
export PATH="$PATH:$HOME/go/bin"

MODE="${1:-zig}"

if [ "$MODE" = "native" ]; then
    echo "模式: 原生交叉编译（CGO_ENABLED=0）"
    echo "注意: SQLite 功能将不可用"
    wails3 task windows:build CGO_ENABLED=0
    echo ""
    echo "=== 构建完成 ==="
    ls -lh bin/*.exe
    exit 0
fi

if [ "$MODE" = "docker" ]; then
    echo "模式: Docker 交叉编译（CGO_ENABLED=1，需 Docker Desktop）"
    if ! docker image inspect wails-cross > /dev/null 2>&1; then
        echo "构建 Docker 交叉编译镜像..."
        wails3 task setup:docker
    fi
    wails3 task windows:build CGO_ENABLED=1
    echo ""
    echo "=== 构建完成 ==="
    ls -lh bin/*.exe
    exit 0
fi

# 默认：Zig 本机交叉编译
echo "模式: Zig 本机交叉编译（CGO_ENABLED=1，完整功能）"

if ! command -v zig > /dev/null 2>&1; then
    echo "错误: 未找到 zig，请先执行: brew install zig"
    exit 1
fi

# 1. 构建前端产物（embed 进 exe）
if [ ! -d "frontend/dist" ]; then
    echo "构建前端产物..."
    wails3 task common:build:frontend
else
    echo "frontend/dist 已存在（如需强制重建: rm -rf frontend/dist 后重跑）"
fi

# 2. 生成 Windows 版本资源（info.json → syso，含元数据）
echo "生成 Windows 版本资源（syso）..."
wails3 task windows:generate:syso ARCH=amd64

# 3. Zig 交叉编译
echo "Zig 交叉编译（首次编译 SQLite C 代码约需 1-2 分钟）..."
CGO_ENABLED=1 GOOS=windows GOARCH=amd64 \
    CC="$PROJECT_ROOT/scripts/zcc-windows-amd64" CGO_CFLAGS="-w -fno-sanitize=all" \
    go build -tags production -trimpath -buildvcs=false \
    -ldflags="-w -s -H windowsgui" -o bin/quant-desktop.exe .

# 4. 清理 syso 产物
rm -f wails_windows_amd64.syso

echo ""
echo "=== 构建完成 ==="
ls -lh bin/quant-desktop.exe
echo "验证: file bin/quant-desktop.exe 应显示 PE32+ executable (GUI) x86-64"
