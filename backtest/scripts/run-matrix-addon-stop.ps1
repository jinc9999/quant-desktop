# 追单专用风控矩阵：首仓保持 3%止损/+2%激活/3%回调，追单单独收紧。
# 口径：A 3.0%×2000万、sl3、act2/cb3、addonmax 2，2024-01~2026-07。
# 指标从 trades.csv / equity.csv 计算。
$ErrorActionPreference = 'Continue'
$csv = 'D:\0001_ba-A - 03\matrix\matrix_addon_stop.csv'
$root = 'D:\0001_ba-A - 03\quant-desktop\backtest'
Set-Location $root
$common = @('-data','data','-mode','momentum','-closed=false','-exit-mode=close','-topn','10','-maxpos','10','-minvol','20000000','-cooldown','30','-trailcd','15','-gain','3','-min24gain','0','-surge','1.2','-sl','3','-tp','0','-act','2','-cb','3','-hold','180','-margin','10','-lev','10','-only-long','-addon','-addonmax','2','-start','2024-01-01','-end','2026-07-31')
$runs = @(
  @{n='addonsl25'; a=@('-addonsl','2.5')},
  @{n='addonsl2'; a=@('-addonsl','2')},
  @{n='addonact15'; a=@('-addontact','1.5')},
  @{n='addonact1'; a=@('-addontact','1')},
  @{n='addoncb25'; a=@('-addoncb','2.5')},
  @{n='addoncb2'; a=@('-addoncb','2')},
  @{n='addon_combo'; a=@('-addonsl','2.5','-addontact','1','-addoncb','2.5')}
)
if (-not (Test-Path (Split-Path $csv))) { New-Item -ItemType Directory -Path (Split-Path $csv) | Out-Null }
'run,param,trades,winrate,pf,net_pnl,max_dd' | Set-Content $csv -Encoding UTF8
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
  return @($cnt, [math]::Round($wins / $cnt * 100, 2), $pf, [math]::Round($net, 0), [math]::Round($maxdd, 2))
}
foreach ($r in $runs) {
  $outName = 'out_fc_A_m20_g30_sl3_' + $r.n
  $args2 = $common + $r.a + @('-out', $outName)
  Write-Host "运行 $($r.n) ..."
  & go run ./cmd/backtest @args2 2>&1 | Out-Null
  $m = Get-Metrics (Join-Path $root $outName)
  "$($r.n),$($r.a -join ' '),$($m[0]),$($m[1]),$($m[2]),$($m[3]),$($m[4])" | Add-Content $csv -Encoding UTF8
  Write-Host "  完成: 笔数=$($m[0]) 胜率=$($m[1]) PF=$($m[2]) 净盈亏=$($m[3]) 回撤=$($m[4])"
}
Write-Host "结果已写入: $csv"
