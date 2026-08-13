# 滑点 × 最低币价矩阵：量化真实利润底线（A 策略当前参数）。
# 口径: gain3 × 2000万 × sl3/act2/cb3 × surge1.2 × addonmax2 × 手续费0.05% 单边 × 10U/仓×10倍 × 10仓
# 区间: 2024-01-01 ~ 数据末尾（2026-08-12）。
# 用法: .\run-slip-matrix.ps1 -Which slip000_mp0,slip005_mp0 -Csv matrix_slip_1.csv
#  -Which 为空或 all 时跑全部 13 个组合。
param(
  [string]$Which = 'all',
  [string]$Csv = 'D:\0001_ba-A - 03\matrix\matrix_slip_minprice.csv'
)

$ErrorActionPreference = 'Continue'
$root = 'D:\0001_ba-A - 03\quant-desktop\backtest'
Set-Location $root

$common = @('-data','data','-mode','momentum','-closed=false','-exit-mode=close',
  '-topn','10','-maxpos','10','-minvol','20000000','-cooldown','30','-trailcd','15',
  '-gain','3','-min24gain','0','-surge','1.2','-sl','3','-tp','0','-act','2','-cb','3',
  '-hold','180','-margin','10','-lev','10','-only-long','-addon','-addonmax','2',
  '-fee','0.05','-start','2024-01-01')

$all = @(
  @{n='slip000_mp0';   a=@('-slip','0','-minprice','0')},
  @{n='slip005_mp0';   a=@('-slip','0.05','-minprice','0')},
  @{n='slip010_mp0';   a=@('-slip','0.1','-minprice','0')},
  @{n='slip020_mp0';   a=@('-slip','0.2','-minprice','0')},
  @{n='slip005_mp005'; a=@('-slip','0.05','-minprice','0.05')},
  @{n='slip010_mp005'; a=@('-slip','0.1','-minprice','0.05')},
  @{n='slip020_mp005'; a=@('-slip','0.2','-minprice','0.05')},
  @{n='slip005_mp01';  a=@('-slip','0.05','-minprice','0.1')},
  @{n='slip010_mp01';  a=@('-slip','0.1','-minprice','0.1')},
  @{n='slip020_mp01';  a=@('-slip','0.2','-minprice','0.1')},
  @{n='slip005_mp02';  a=@('-slip','0.05','-minprice','0.2')},
  @{n='slip010_mp02';  a=@('-slip','0.1','-minprice','0.2')},
  @{n='slip020_mp02';  a=@('-slip','0.2','-minprice','0.2')}
)

if ($Which -eq 'all') {
  $runs = $all
} else {
  $names = $Which -split ',' | ForEach-Object { $_.Trim() }
  $runs = $all | Where-Object { $names -contains $_.n }
}

if (-not (Test-Path (Split-Path $csv))) { New-Item -ItemType Directory -Path (Split-Path $csv) | Out-Null }
'run,slip,minprice,trades,winrate,pf,net_pnl,max_dd,avg_pnl' | Set-Content $csv -Encoding UTF8

function Get-Metrics([string]$dir) {
  $t = Import-Csv (Join-Path $dir 'trades.csv')
  $e = Import-Csv (Join-Path $dir 'equity.csv')
  $cnt = $t.Count
  $wins = ($t | Where-Object { [double]$_.pnl -gt 0 }).Count
  $g = ($t | Where-Object { [double]$_.pnl -gt 0 } | Measure-Object pnl -Sum).Sum
  $l = -($t | Where-Object { [double]$_.pnl -lt 0 } | Measure-Object pnl -Sum).Sum
  $pf = if ($l -gt 0) { [math]::Round($g / $l, 2) } else { 0 }
  $vals = @($e | ForEach-Object { [double]$_.equity })
  $peak = $vals[0]; $maxdd = 0.0
  foreach ($v in $vals) {
    if ($v -gt $peak) { $peak = $v }
    if ($peak -gt 0) { $dd = ($peak - $v) / $peak * 100; if ($dd -gt $maxdd) { $maxdd = $dd } }
  }
  $net = $vals[-1] - $vals[0]
  return @($cnt, [math]::Round($wins / $cnt * 100, 2), $pf, [math]::Round($net, 0), [math]::Round($maxdd, 2), [math]::Round($net / $cnt, 3))
}

foreach ($r in $runs) {
  $outName = 'out_slipA_' + $r.n
  $args2 = $common + $r.a + @('-out', $outName)
  Write-Host "运行 $($r.n) ..."
  & .\backtest.exe @args2 2>&1 | Out-Null
  $m = Get-Metrics (Join-Path $root $outName)
  $slip = $r.a[1]; $mp = $r.a[3]
  "$($r.n),$slip,$mp,$($m[0]),$($m[1]),$($m[2]),$($m[3]),$($m[4]),$($m[5])" | Add-Content $csv -Encoding UTF8
  Write-Host "  完成: 笔数=$($m[0]) 胜率=$($m[1]) PF=$($m[2]) 净盈亏=$($m[3]) 回撤=$($m[4])"
}
Write-Host "结果已写入: $csv"
