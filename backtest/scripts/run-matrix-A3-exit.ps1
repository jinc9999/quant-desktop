# A 策略（3.0% × 2000万，24h 门槛 0）出场参数聚焦矩阵：
# 移动激活(act) × 移动回调(cb) × 固定止损(sl)，验证 4%→3% 后出场参数最优区间是否漂移。
# 基准与策略实验结论 13.1 的 A 3.0%×2000万 一致：39,871 笔 / +28,640U / PF 1.59 / DD 4.73%。
$ErrorActionPreference = 'Continue'
$csv = 'D:\0001_ba-A - 03\matrix\matrix_A3_exit.csv'
$base = @('-data','data','-mode','momentum','-closed=false','-exit-mode=close','-topn','10','-maxpos','10','-minvol','20000000','-cooldown','30','-trailcd','15','-gain','3','-min24gain','0','-surge','1.2','-sl','4','-tp','0','-act','2','-cb','3','-hold','180','-margin','10','-lev','10','-only-long','-addon','-addonmax','2','-start','2024-01-01','-end','2026-07-31')
$runs = @(
  @{n='base'; a=@()},
  @{n='act15'; a=@('-act','1.5')},
  @{n='act25'; a=@('-act','2.5')},
  @{n='cb25'; a=@('-cb','2.5')},
  @{n='cb35'; a=@('-cb','3.5')},
  @{n='act15_cb25'; a=@('-act','1.5','-cb','2.5')},
  @{n='act15_cb35'; a=@('-act','1.5','-cb','3.5')},
  @{n='act25_cb25'; a=@('-act','2.5','-cb','2.5')},
  @{n='act25_cb35'; a=@('-act','2.5','-cb','3.5')},
  @{n='sl3'; a=@('-sl','3')},
  @{n='sl35'; a=@('-sl','3.5')},
  @{n='sl45'; a=@('-sl','4.5')},
  @{n='sl5'; a=@('-sl','5')}
)
if (-not (Test-Path (Split-Path $csv))) { New-Item -ItemType Directory -Path (Split-Path $csv) | Out-Null }
'run,param,trades,winrate,pf,pnl,dd' | Set-Content $csv -Encoding UTF8
foreach ($r in $runs) {
  $args2 = $base + $r.a + @('-out', ('out_fc_A_m20_g30_' + $r.n))
  $raw = & go run ./cmd/backtest @args2 2>&1 | Out-String
  $raw = $raw -replace "\x1b\[[0-9;]*m", ''
  $trades = [regex]::Match($raw, '总交易数: (\d+)').Groups[1].Value
  $win = [regex]::Match($raw, '胜率: ([\d.]+)').Groups[1].Value
  $pf = [regex]::Match($raw, '盈亏比\(Profit Factor\): ([\d.]+)').Groups[1].Value
  $pnl = [regex]::Match($raw, '累计盈亏: (-?[\d.]+)').Groups[1].Value
  $dd = [regex]::Match($raw, '最大回撤: ([\d.]+)').Groups[1].Value
  $param = ($r.a -join ' ')
  "$($r.n),$param,$trades,$win,$pf,$pnl,$dd" | Add-Content $csv -Encoding UTF8
  Write-Host "$($r.n): trades=$trades win=$win pf=$pf pnl=$pnl dd=$dd"
}
Write-Host "结果已写入: $csv"
