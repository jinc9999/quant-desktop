#!/bin/bash
# Windows 打包脚本（macOS 交叉编译）
# 由于 go-sqlite3 依赖 CGO，从 macOS 交叉编译 Windows 需要 Docker
#
# 使用方法：
#   ./scripts/build-windows.sh          # 使用 Docker 交叉编译
#   ./scripts/build-windows.sh native   # 仅编译（CGO_ENABLED=0，不含 SQLite）
#
# 前置条件：
#   - Docker 已安装并运行
#   - wails3 CLI 已安装
#   - 执行过: wails3 task setup:docker（首次）

set -e

cd "$(dirname "$0")/.."
PROJECT_ROOT=$(pwd)

echo "=== 量化交易客户端 Windows 打包 ==="
echo "项目目录: $PROJECT_ROOT"

# 确保 wails3 在 PATH 中
export PATH="$PATH:$HOME/go/bin"

if [ "$1" = "native" ]; then
    echo "模式: 原生交叉编译（CGO_ENABLED=0）"
    echo "注意: SQLite 功能将不可用"
    wails3 task windows:build CGO_ENABLED=0
else
    echo "模式: Docker 交叉编译（CGO_ENABLED=1，完整功能）"
    
    # 检查 Docker 镜像是否存在
    if ! docker image inspect wails-cross > /dev/null 2>&1; then
        echo "构建 Docker 交叉编译镜像..."
        wails3 task setup:docker
    fi
    
    wails3 task windows:build CGO_ENABLED=1
fi

echo ""
echo "=== 构建完成 ==="
echo "输出文件: bin/quant-desktop.exe"
ls -lh bin/*.exe 2>/dev/null || echo "（未找到 exe 文件，请检查构建日志）"
