# 一键构建 A/B 客户端（各自图标 + 各自策略标识）
# 用法: powershell -ExecutionPolicy Bypass -File build_clients.ps1 [-Action both|A|B]

param([string]$Action = "both")

$ErrorActionPreference = "Stop"
$root = "D:\0001_ba-A - 03\quant-desktop"
Set-Location $root
$env:CGO_ENABLED = "1"

function Invoke-SysoAndBuild {
    param([string]$Icon, [string]$OutExe, [string]$LdFlags)
    Copy-Item $Icon "build\windows\icon.ico" -Force
    Push-Location "build"
    & wails3 generate syso -arch amd64 -icon windows/icon.ico -manifest windows/wails.exe.manifest -info windows/info.json -out ../wails_windows_amd64.syso
    if ($LASTEXITCODE -ne 0) { throw "syso 生成失败" }
    Pop-Location
    go build -tags production -trimpath -buildvcs=false -ldflags $LdFlags -o $OutExe .
    if ($LASTEXITCODE -ne 0) { throw "go build 失败" }
    Remove-Item "wails_windows_amd64.syso" -Force -ErrorAction SilentlyContinue
}

if ($Action -eq "A" -or $Action -eq "both") {
    Write-Host "=== 构建 A（进攻，琥珀金闪电图标）==="
    Invoke-SysoAndBuild -Icon "build\windows\icon_A.ico" -OutExe "quant-desktop.exe" -LdFlags "-w -s -H windowsgui"
    Write-Host "A 完成: quant-desktop.exe"
}

if ($Action -eq "B" -or $Action -eq "both") {
    Write-Host "=== 构建 B（稳健，科技蓝盾牌图标）==="
    Invoke-SysoAndBuild -Icon "build\windows\icon_B.ico" -OutExe "..\quant-desktop-rank\quant-desktop.exe" `
        -LdFlags "-w -s -H windowsgui -X quant-desktop/internal/binance.defaultRankMode=1 -X quant-desktop/internal/binance.defaultRankParam=10 -X quant-desktop/internal/binance.defaultStrategyName=币安-魔力稳健B策略"
    Write-Host "B 完成: quant-desktop-rank\quant-desktop.exe"
}

Write-Host "全部完成"
