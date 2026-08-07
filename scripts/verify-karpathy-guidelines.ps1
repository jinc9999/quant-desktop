# verify-karpathy-guidelines.ps1
# Verifies that the Karpathy coding guidelines are installed and effective
# for the current project (quant-desktop).
#
# Usage:  powershell -ExecutionPolicy Bypass -File scripts\verify-karpathy-guidelines.ps1
#
# What it checks:
#   1. Project-level: CLAUDE.md exists and contains the 4 principles
#   2. Cursor rule:   .cursor/rules/karpathy-guidelines.mdc exists,
#                     has "alwaysApply: true" and the 4 principles
#   3. Global skill:  TraeCode global skill SKILL.md (optional)

$ErrorActionPreference = 'Stop'
$root  = Split-Path -Parent $PSScriptRoot        # project root (this script lives in scripts/)
$pass  = 0
$fail  = 0

function Check {
    param([string]$Name, [bool]$Ok, [string]$Detail = '')
    if ($Ok) {
        $script:pass++
        Write-Host "[PASS] $Name" -ForegroundColor Green
    }
    else {
        $script:fail++
        Write-Host "[FAIL] $Name" -ForegroundColor Red
        if ($Detail) { Write-Host "       $Detail" -ForegroundColor DarkGray }
    }
}

Write-Host "== 1. Project-level CLAUDE.md ==" -ForegroundColor Cyan
$claudePath = Join-Path $root 'CLAUDE.md'
$claudeExists = Test-Path $claudePath
Check 'CLAUDE.md exists in project root' $claudeExists "Expected: $claudePath"
if ($claudeExists) {
    $content = Get-Content $claudePath -Raw
    foreach ($p in @('1. Think Before Coding', '2. Simplicity First', '3. Surgical Changes', '4. Goal-Driven Execution')) {
        Check "CLAUDE.md contains '$p'" ($content -match [regex]::Escape($p)) 'Principle section missing'
    }
}

Write-Host "`n== 2. Cursor rule (.cursor/rules) ==" -ForegroundColor Cyan
$mdcPath = Join-Path $root '.cursor\rules\karpathy-guidelines.mdc'
$mdcExists = Test-Path $mdcPath
Check '.cursor/rules/karpathy-guidelines.mdc exists' $mdcExists "Expected: $mdcPath"
if ($mdcExists) {
    $content = Get-Content $mdcPath -Raw
    Check 'Rule has "alwaysApply: true"' ($content -match 'alwaysApply:\s*true') 'Missing alwaysApply frontmatter'
    foreach ($p in @('## 1. Think Before Coding', '## 2. Simplicity First', '## 3. Surgical Changes', '## 4. Goal-Driven Execution')) {
        Check "Rule contains '$p'" ($content -match [regex]::Escape($p)) 'Principle section missing'
    }
}

Write-Host "`n== 3. Global skill (TraeCode) ==" -ForegroundColor Cyan
$skillPath = Join-Path $env:USERPROFILE '.trae-cn\builtin\global\skills\karpathy-guidelines\SKILL.md'
$skillExists = Test-Path $skillPath
Check 'Global skill SKILL.md installed' $skillExists "Optional - expected: $skillPath"

Write-Host "`n========== Summary ==========" -ForegroundColor Cyan
Write-Host "Passed: $pass   Failed: $fail"
if ($fail -eq 0) {
    Write-Host 'All installation checks passed. Guidelines are in place.' -ForegroundColor Green
    exit 0
}
else {
    Write-Host 'Some checks failed. Fix the [FAIL] items above and re-run.' -ForegroundColor Yellow
    exit 1
}
