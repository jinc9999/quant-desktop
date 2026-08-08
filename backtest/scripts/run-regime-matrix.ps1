# 市场状态过滤单因子实验运行器（S01 单因子优化第二波）
# 基准 = S01 v2 定稿参数（2024-01-01 ~ 2026-07-31）
# 用法: pwsh -File run-regime-matrix.ps1 -Group 1   (1=btc24h/btcma, 2=breadth)
param([int]$Group = 0)
$ErrorActionPreference = 'Stop'
$csv = 'D:\0001_ba-A - 03\matrix\regime_matrix.csv'
$base = @('-data','data','-mode','momentum','-closed=false','-exit-mode=close','-topn','10','-maxpos','10','-minvol','10000000','-cooldown','30','-trailcd','15','-gain','4','-min24gain','4','-surge','1.2','-sl','4','-tp','0','-act','2','-cb','3','-hold','180','-margin','10','-lev','10','-only-long','-addon','-start','2024-01-01','-end','2026-07-31')
$groups = @(
  @(
    @{n='btc24h_m1'; a=@('-regime','btc24h','-regime-param','-1')},
    @{n='btc24h_0';  a=@('-regime','btc24h','-regime-param','0')},
    @{n='btc24h_1';  a=@('-regime','btc24h','-regime-param','1')},
    @{n='btcma';     a=@('-regime','btcma')}
  ),
  @(
    @{n='breadth_40'; a=@('-regime','breadth','-regime-param','0.4')},
    @{n='breadth_50'; a=@('-regime','breadth','-regime-param','0.5')},
    @{n='breadth_60'; a=@('-regime','breadth','-regime-param','0.6')}
  )
)
if (-not (Test-Path (Split-Path $csv))) { New-Item -ItemType Directory -Path (Split-Path $csv) | Out-Null }
if (-not (Test-Path $csv)) { 'run,trades,winrate,pf,pnl,dd,avgwin,avgloss' | Set-Content $csv -Encoding UTF8 }
$runs = if ($Group -ge 1 -and $Group -le $groups.Count) { $groups[$Group-1] } else { $groups | ForEach-Object { $_ } }
foreach ($r in $runs) {
  $args2 = $base + $r.a + @('-out', ('out_reg_' + $r.n))
  $raw = & .\backtest.exe @args2 2>&1 | Out-String
  $raw = $raw -replace "\x1b\[[0-9;]*m", ''
  $trades = [regex]::Match($raw, '总交易数: (\d+)').Groups[1].Value
  $win = [regex]::Match($raw, '胜率: ([\d.]+)').Groups[1].Value
  $pf = [regex]::Match($raw, '盈亏比\(Profit Factor\): ([\d.]+)').Groups[1].Value
  $pnl = [regex]::Match($raw, '累计盈亏: (-?[\d.]+)').Groups[1].Value
  $dd = [regex]::Match($raw, '最大回撤: ([\d.]+)').Groups[1].Value
  $aw = [regex]::Match($raw, '平均盈利: ([\d.]+)').Groups[1].Value
  $al = [regex]::Match($raw, '平均亏损: (-?[\d.]+)').Groups[1].Value
  "$($r.n),$trades,$win,$pf,$pnl,$dd,$aw,$al" | Add-Content $csv -Encoding UTF8
  Write-Host "$($r.n): trades=$trades win=$win pf=$pf pnl=$pnl dd=$dd"
}
