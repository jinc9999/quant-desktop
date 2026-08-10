# A 策略（3.0% × 2000万）出场参数细化 + 严格 walk-forward：
#   A. 全周期止损 0.1 加密（2.8/2.9/3.1/3.2/3.3/3.4，3.0 已有）；
#   B. 只用 2024 数据重跑完整出场矩阵（样本内选参），选出 2024 最优后
#      再到 2025-26 上验证（样本外）——严格流程，防止全周期选参偷看答案。
# 指标从 trades.csv / equity.csv 计算（不解析控制台，避免编码问题）。
$ErrorActionPreference = 'Continue'
$csv = 'D:\0001_ba-A - 03\matrix\matrix_A3_exit_fine_wf.csv'
$root = 'D:\0001_ba-A - 03\quant-desktop\backtest'
Set-Location $root
$common = @('-data','data','-mode','momentum','-closed=false','-exit-mode=close','-topn','10','-maxpos','10','-minvol','20000000','-cooldown','30','-trailcd','15','-gain','3','-min24gain','0','-surge','1.2','-sl','4','-tp','0','-act','2','-cb','3','-hold','180','-margin','10','-lev','10','-only-long','-addon','-addonmax','2')
$runs = @(
  @{n='sl28'; a=@('-sl','2.8'); period='full'},
  @{n='sl29'; a=@('-sl','2.9'); period='full'},
  @{n='sl31'; a=@('-sl','3.1'); period='full'},
  @{n='sl32'; a=@('-sl','3.2'); period='full'},
  @{n='sl33'; a=@('-sl','3.3'); period='full'},
  @{n='sl34'; a=@('-sl','3.4'); period='full'},
  @{n='wf_base'; a=@(); period='2024'},
  @{n='wf_act15'; a=@('-act','1.5'); period='2024'},
  @{n='wf_act25'; a=@('-act','2.5'); period='2024'},
  @{n='wf_cb25'; a=@('-cb','2.5'); period='2024'},
  @{n='wf_cb35'; a=@('-cb','3.5'); period='2024'},
  @{n='wf_act15_cb25'; a=@('-act','1.5','-cb','2.5'); period='2024'},
  @{n='wf_act15_cb35'; a=@('-act','1.5','-cb','3.5'); period='2024'},
  @{n='wf_act25_cb25'; a=@('-act','2.5','-cb','2.5'); period='2024'},
  @{n='wf_act25_cb35'; a=@('-act','2.5','-cb','3.5'); period='2024'},
  @{n='wf_sl3'; a=@('-sl','3'); period='2024'},
  @{n='wf_sl35'; a=@('-sl','3.5'); period='2024'},
  @{n='wf_sl45'; a=@('-sl','4.5'); period='2024'},
  @{n='wf_sl5'; a=@('-sl','5'); period='2024'}
)
if (-not (Test-Path (Split-Path $csv))) { New-Item -ItemType Directory -Path (Split-Path $csv) | Out-Null }
'run,period,trades,winrate,pf,net_pnl,max_dd' | Set-Content $csv -Encoding UTF8
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
  $period = $r.period
  if ($period -eq 'full') { $range = @('-start','2024-01-01','-end','2026-07-31') } else { $range = @('-start','2024-01-01','-end','2024-12-31') }
  $outName = if ($period -eq 'full') { 'out_fc_A_m20_g30_' + $r.n } else { 'out_fc_A_m20_g30_' + $r.n }
  $args2 = $common + $r.a + $range + @('-out', $outName)
  Write-Host "运行 $($r.n) ($period) ..."
  & go run ./cmd/backtest @args2 2>&1 | Out-Null
  $m = Get-Metrics (Join-Path $root $outName)
  "$($r.n),$period,$($m[0]),$($m[1]),$($m[2]),$($m[3]),$($m[4])" | Add-Content $csv -Encoding UTF8
  Write-Host "  完成: 笔数=$($m[0]) 胜率=$($m[1]) PF=$($m[2]) 净盈亏=$($m[3]) 回撤=$($m[4])"
}
Write-Host "结果已写入: $csv"
