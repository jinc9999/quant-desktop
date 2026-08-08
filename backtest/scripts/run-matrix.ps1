# 参数矩阵运行器: 在 1000万口径 + 追加仓 + 止盈后15分钟冷却 的基线上逐参数变体
# 用法: powershell -File run-matrix.ps1 -Group 1..6  (0=全部)
param([int]$Group = 0)
$ErrorActionPreference = 'Stop'
$csv = 'D:\0001_ba-A - 03\matrix\matrix.csv'
$base = @('-data','data','-mode','momentum','-closed=false','-exit-mode=close','-topn','10','-maxpos','10','-minvol','10000000','-cooldown','60','-trailcd','15','-gain','4','-min24gain','4','-surge','1.5','-sl','6','-tp','0','-act','3','-cb','2','-hold','120','-margin','10','-lev','10','-only-long','-addon','-start','2024-01-01','-end','2026-07-31')
$groups = @(
  @(@{n='base'; a=@()}, @{n='gain_3'; a=@('-gain','3')}, @{n='gain_5'; a=@('-gain','5')}, @{n='gain_6'; a=@('-gain','6')}, @{n='min24_3'; a=@('-min24gain','3')}, @{n='min24_8'; a=@('-min24gain','8')}),
  @(@{n='minvol_5m'; a=@('-minvol','5000000')}, @{n='minvol_20m'; a=@('-minvol','20000000')}, @{n='minvol_50m'; a=@('-minvol','50000000')}, @{n='topn_5'; a=@('-topn','5')}, @{n='topn_15'; a=@('-topn','15')}),
  @(@{n='maxpos_5'; a=@('-maxpos','5')}, @{n='maxpos_15'; a=@('-maxpos','15')}, @{n='cooldown_30'; a=@('-cooldown','30')}, @{n='cooldown_120'; a=@('-cooldown','120')}, @{n='sl_4'; a=@('-sl','4')}),
  @(@{n='sl_8'; a=@('-sl','8')}, @{n='sl_10'; a=@('-sl','10')}, @{n='act_2'; a=@('-act','2')}, @{n='act_4'; a=@('-act','4')}, @{n='act_5'; a=@('-act','5')}),
  @(@{n='cb_15'; a=@('-cb','1.5')}, @{n='cb_3'; a=@('-cb','3')}, @{n='hold_60'; a=@('-hold','60')}, @{n='hold_90'; a=@('-hold','90')}, @{n='hold_180'; a=@('-hold','180')}),
  @(@{n='surge_12'; a=@('-surge','1.2')}, @{n='surge_20'; a=@('-surge','2.0')}, @{n='pullback_5'; a=@('-pullback','5')}, @{n='pullback_12'; a=@('-pullback','12')}, @{n='tp_5'; a=@('-tp','5')}, @{n='tp_10'; a=@('-tp','10')})
)
if (-not (Test-Path (Split-Path $csv))) { New-Item -ItemType Directory -Path (Split-Path $csv) | Out-Null }
if (-not (Test-Path $csv)) { 'run,trades,winrate,pf,pnl,dd' | Set-Content $csv -Encoding UTF8 }
$runs = if ($Group -ge 1 -and $Group -le $groups.Count) { $groups[$Group-1] } else { $groups | ForEach-Object { $_ } }
foreach ($r in $runs) {
  $args2 = $base + $r.a + @('-out', ('out_mx_' + $r.n))
  $raw = & go run ./cmd/backtest @args2 2>&1 | Out-String
  $raw = $raw -replace "\x1b\[[0-9;]*m", ''
  $trades = [regex]::Match($raw, '总交易数: (\d+)').Groups[1].Value
  $win = [regex]::Match($raw, '胜率: ([\d.]+)').Groups[1].Value
  $pf = [regex]::Match($raw, '盈亏比\(Profit Factor\): ([\d.]+)').Groups[1].Value
  $pnl = [regex]::Match($raw, '累计盈亏: (-?[\d.]+)').Groups[1].Value
  $dd = [regex]::Match($raw, '最大回撤: ([\d.]+)').Groups[1].Value
  "$($r.n),$trades,$win,$pf,$pnl,$dd" | Add-Content $csv -Encoding UTF8
  Write-Host "$($r.n): trades=$trades win=$win pf=$pf pnl=$pnl dd=$dd"
}
