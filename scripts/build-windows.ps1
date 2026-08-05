# Windows 原生打包脚本（PowerShell）
# 在 Windows 上直接运行，生成独立 exe
#
# 使用方法：
#   .\scripts\build-windows.ps1
#
# 前置条件：
#   - Go 1.23+ 已安装
#   - Node.js 18+ 和 pnpm 已安装
#   - wails3 CLI 已安装: go install github.com/wailsapp/wails/v3/cmd/wails3@latest
#   - GCC 已安装（TDM-GCC 或 MSYS2，用于 CGO/SQLite）

$ErrorActionPreference = "Stop"

Set-Location $PSScriptRoot\..
$ProjectRoot = Get-Location

Write-Host "=== 量化交易客户端 Windows 打包 ===" -ForegroundColor Cyan
Write-Host "项目目录: $ProjectRoot"

# 检查依赖
Write-Host "`n[1/4] 检查环境..." -ForegroundColor Yellow

$goVersion = go version 2>$null
if (-not $goVersion) { throw "Go 未安装" }
Write-Host "  Go: $goVersion"

$nodeVersion = node --version 2>$null
if (-not $nodeVersion) { throw "Node.js 未安装" }
Write-Host "  Node: $nodeVersion"

$wailsVersion = wails3 version 2>$null
if (-not $wailsVersion) { throw "wails3 CLI 未安装" }
Write-Host "  Wails: $wailsVersion"

# 安装前端依赖
Write-Host "`n[2/4] 安装前端依赖..." -ForegroundColor Yellow
Set-Location frontend
pnpm install
Set-Location $ProjectRoot

# 构建
Write-Host "`n[3/4] 构建 Windows exe..." -ForegroundColor Yellow
$env:CGO_ENABLED = "1"
wails3 task windows:build

# 验证输出
Write-Host "`n[4/4] 验证输出..." -ForegroundColor Yellow
$exePath = "bin\quant-desktop.exe"
if (Test-Path $exePath) {
    $size = (Get-Item $exePath).Length / 1MB
    Write-Host "  构建成功: $exePath ($([math]::Round($size, 2)) MB)" -ForegroundColor Green
} else {
    Write-Host "  错误: 未找到输出文件" -ForegroundColor Red
    exit 1
}

Write-Host "`n=== 打包完成 ===" -ForegroundColor Green
