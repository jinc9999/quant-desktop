# 一键构建 A/B/C 客户端（各自图标 + 各自策略标识）
# 用法: powershell -ExecutionPolicy Bypass -File build_clients.ps1 [-Action both|A|B|C]

param([string]$Action = "both")

$ErrorActionPreference = "Stop"
$root = "D:\0001_ba-A - 03\quant-desktop"
Set-Location $root
$env:CGO_ENABLED = "1"

function Invoke-FrontendBuild {
    param([switch]$C)
    Push-Location "frontend"
    if ($C) {
        Write-Host "--- 构建 C 版前端（VITE_PRODUCT_VARIANT=C，隐藏策略配置 + 登录/授权界面）---"
        & pnpm build -- --mode c
    } else {
        Write-Host "--- 构建 A/B 版前端（默认模式，含策略配置页）---"
        & pnpm build
    }
    if ($LASTEXITCODE -ne 0) { throw "前端构建失败" }
    Pop-Location
}

function Invoke-Bindings {
    Write-Host "--- 重新生成前端绑定 ---"
    & wails3 generate bindings -clean=true -ts -i
    if ($LASTEXITCODE -ne 0) { throw "绑定生成失败" }
}

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
    Invoke-Bindings
    Invoke-FrontendBuild
    Invoke-SysoAndBuild -Icon "build\windows\icon_A.ico" -OutExe "quant-desktop.exe" -LdFlags "-w -s -H windowsgui"
    Write-Host "A 完成: quant-desktop.exe"
}

if ($Action -eq "B" -or $Action -eq "both") {
    Write-Host "=== 构建 B（稳健，科技蓝盾牌图标）==="
    Invoke-Bindings
    Invoke-FrontendBuild
    Invoke-SysoAndBuild -Icon "build\windows\icon_B.ico" -OutExe "..\quant-desktop-rank\quant-desktop.exe" `
        -LdFlags "-w -s -H windowsgui -X quant-desktop/internal/binance.defaultRankMode=1 -X quant-desktop/internal/binance.defaultRankParam=10 -X quant-desktop/internal/binance.defaultStrategyName=币安-魔力稳健B策略"
    Write-Host "B 完成: quant-desktop-rank\quant-desktop.exe"
}

if ($Action -eq "D" -or $Action -eq "both") {
    Write-Host "=== 构建 D（智慧版，A 骨架 + 5m 爆拉仓位）==="
    Invoke-Bindings
    Invoke-FrontendBuild
    $dOutDir = "..\quant-desktop-smart"
    New-Item -ItemType Directory -Force -Path $dOutDir | Out-Null
    New-Item -ItemType Directory -Force -Path "$dOutDir\data" | Out-Null
    Invoke-SysoAndBuild -Icon "build\windows\icon_A.ico" -OutExe "$dOutDir\quant-desktop.exe" `
        -LdFlags "-w -s -H windowsgui -X quant-desktop/internal/product.ProductName=币安-魔力智慧D策略 -X quant-desktop/internal/binance.defaultStrategyName=币安-魔力智慧D策略 -X quant-desktop/internal/binance.defaultStrategyVersion=V1.0_202608132200 -X quant-desktop/internal/binance.defaultSmartSizeMode=1 -X quant-desktop/internal/binance.defaultMinGainPct=3 -X quant-desktop/internal/binance.defaultMin24hGainPct=0 -X quant-desktop/internal/binance.defaultMinQuoteVolume=20000000 -X quant-desktop/internal/binance.defaultStopLossPct=3"
    Write-Host "D 完成: quant-desktop-smart\quant-desktop.exe"
}

if ($Action -eq "C") {
    Write-Host "=== 构建 C（超能战士，红金闪电图标）==="
    Invoke-Bindings
    Invoke-FrontendBuild -C
    $cOutDir = "..\超能战士"
    New-Item -ItemType Directory -Force -Path $cOutDir | Out-Null
    Invoke-SysoAndBuild -Icon "build\windows\icon_C.ico" -OutExe "$cOutDir\超能战士.exe" `
        -LdFlags "-w -s -H windowsgui -X quant-desktop/internal/product.Variant=C -X quant-desktop/internal/product.ProductName=超能战士"
    Write-Host "C 完成: 超能战士\超能战士.exe"
}

Write-Host "全部完成"
