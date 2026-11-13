# Tests for the pure .wslconfig functions in setup.ps1. Runs on any OS with
# PowerShell 7 (no Pester needed):
#
#   pwsh -NoProfile -NonInteractive -File deploy/windows/tests/WslConfig.Tests.ps1
#
# Without a local pwsh, run it in a container with a *native* pwsh (on Apple
# Silicon, mcr.microsoft.com/powershell:latest is an emulated 32-bit arm image
# in which pwsh crashes; install the linux-arm64 tarball into ubuntu:24.04).
# PSScriptAnalyzer is clean except PSAvoidUsingWriteHost (deliberate, colored
# console output) and PSReviewUnusedParameter (script params used via scope).
#
# Exits 1 if any test fails.
Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

# Dot-sourcing defines the functions without running the setup.
. (Join-Path $PSScriptRoot '..' 'setup.ps1')

$script:failures = 0
$script:count = 0
function Test-Case([string]$Name, [scriptblock]$Body) {
    $script:count++
    try { & $Body; Write-Host "ok   $Name" }
    catch {
        $script:failures++
        Write-Host "FAIL $Name`n     $($_.Exception.Message)`n     $($_.ScriptStackTrace -replace "`n", "`n     ")" -ForegroundColor Red
    }
}
function Assert-Equal($Expected, $Actual, [string]$What = '') {
    $e = ($Expected | Out-String).TrimEnd(); $a = ($Actual | Out-String).TrimEnd()
    if ($e -cne $a) { throw "$What`n--- expected ---`n$e`n--- actual ---`n$a" }
}
function Assert-True([bool]$Cond, [string]$What) { if (-not $Cond) { throw $What } }

$settings = Get-DolWslConfig -Memory '40GB' -Processors 12 -Swap '8GB'

Test-Case 'settings put each key in the section WSL reads it from (docs/compat.md)' {
    Assert-Equal @('wsl2', 'experimental', 'general') @($settings.Keys)
    Assert-Equal @('memory', 'processors', 'swap', 'networkingMode') @($settings['wsl2'].Keys)
    Assert-Equal @('sparseVhd', 'autoMemoryReclaim') @($settings['experimental'].Keys)
    Assert-Equal 'gradual' $settings['experimental']['autoMemoryReclaim']
    Assert-Equal '-1' $settings['general']['instanceIdleTimeout']
}

Test-Case 'empty file gets all sections' {
    $r = Merge-IniContent -Lines @() -Settings $settings -Relocate
    Assert-True $r.Changed 'must report a change'
    Assert-Equal @(
        '[wsl2]', 'memory=40GB', 'processors=12', 'swap=8GB', 'networkingMode=mirrored', '',
        '[experimental]', 'sparseVhd=true', 'autoMemoryReclaim=gradual', '',
        '[general]', 'instanceIdleTimeout=-1') $r.Lines
}

Test-Case 'null input (empty file read by Get-Content) works' {
    $r = Merge-IniContent -Lines $null -Settings $settings
    Assert-True ($r.Lines.Count -eq 12) "got $($r.Lines.Count) lines"
}

Test-Case 'second merge is a no-op' {
    $once = (Merge-IniContent -Lines @() -Settings $settings -Relocate).Lines
    $r = Merge-IniContent -Lines $once -Settings $settings -Relocate
    Assert-True (-not $r.Changed) 'must not report a change'
    Assert-Equal $once $r.Lines
}

Test-Case 'existing values are updated in place; comments and unknown keys survive' {
    $in = @(
        '# my WSL settings',
        '[wsl2]',
        'memory=8GB   ',
        '; keep this comment',
        'kernelCommandLine = quiet',
        'localhostForwarding=true',
        '',
        '[interop]',
        'appendWindowsPath=false')
    $r = Merge-IniContent -Lines $in -Settings $settings -Relocate
    Assert-Equal @(
        '# my WSL settings',
        '[wsl2]',
        'memory=40GB',
        '; keep this comment',
        'kernelCommandLine = quiet',
        'localhostForwarding=true',
        'processors=12', 'swap=8GB', 'networkingMode=mirrored',
        '',
        '[interop]',
        'appendWindowsPath=false',
        '',
        '[experimental]', 'sparseVhd=true', 'autoMemoryReclaim=gradual',
        '',
        '[general]', 'instanceIdleTimeout=-1') $r.Lines
}

Test-Case 'keys in the wrong section are commented out (old guides put sparseVhd in [wsl2])' {
    $in = @('[wsl2]', 'sparseVhd=true', 'autoMemoryReclaim=dropcache', 'memory=40GB')
    $r = Merge-IniContent -Lines $in -Settings $settings -Relocate
    Assert-True ($r.Lines -contains '# sparseVhd=true  # moved to [experimental] by docker-offload-lan') 'sparseVhd not relocated'
    Assert-True ($r.Lines -contains '# autoMemoryReclaim=dropcache  # moved to [experimental] by docker-offload-lan') 'autoMemoryReclaim not relocated'
    $exp = [array]::IndexOf($r.Lines, '[experimental]')
    Assert-True ($exp -gt 0 -and $r.Lines[$exp + 1] -eq 'sparseVhd=true') 'sparseVhd missing under [experimental]'
    $again = Merge-IniContent -Lines $r.Lines -Settings $settings -Relocate
    Assert-True (-not $again.Changed) 'relocation must be idempotent'
}

Test-Case 'section and key names match case-insensitively' {
    $in = @('[WSL2]', 'Memory=40GB', 'PROCESSORS=12', 'swap=8GB', 'networkingmode=mirrored',
        '[Experimental]', 'sparsevhd=true', 'autoMemoryReclaim=gradual', '[General]', 'instanceIdleTimeout=-1')
    $r = Merge-IniContent -Lines $in -Settings $settings -Relocate
    Assert-True (-not $r.Changed) "unexpected change:`n$($r.Lines -join "`n")"
}

Test-Case 'duplicate keys: first is set, later ones disabled' {
    $in = @('[wsl2]', 'memory=4GB', 'memory=16GB')
    $r = Merge-IniContent -Lines $in -Settings $settings
    Assert-Equal 'memory=40GB' $r.Lines[1]
    Assert-Equal '# memory=16GB  # duplicate, disabled by docker-offload-lan' $r.Lines[2]
}

Test-Case 'missing keys go before trailing blank lines and the next section' {
    $in = @('[wsl2]', 'memory=40GB', '', '', '[general]', 'instanceIdleTimeout=-1')
    $r = Merge-IniContent -Lines $in -Settings $settings
    Assert-Equal @('[wsl2]', 'memory=40GB', 'processors=12', 'swap=8GB', 'networkingMode=mirrored', '', '',
        '[general]', 'instanceIdleTimeout=-1', '', '[experimental]', 'sparseVhd=true', 'autoMemoryReclaim=gradual') $r.Lines
}

Test-Case 'section header with trailing comment is recognized' {
    $in = @('[wsl2] # VM settings', 'memory=40GB', 'processors=12', 'swap=8GB', 'networkingMode=mirrored')
    $r = Merge-IniContent -Lines $in -Settings ([ordered]@{ wsl2 = $settings['wsl2'] })
    Assert-True (-not $r.Changed) "unexpected change:`n$($r.Lines -join "`n")"
}

Test-Case 'custom sizes are written' {
    $s = Get-DolWslConfig -Memory '24GB' -Processors 8 -Swap '0GB'
    $r = Merge-IniContent -Lines @('[wsl2]', 'memory=40GB') -Settings $s
    Assert-Equal 'memory=24GB' $r.Lines[1]
    Assert-True ($r.Lines -contains 'processors=8') 'processors'
}

Write-Host ''
if ($script:failures -gt 0) { Write-Host "$($script:failures) of $($script:count) test(s) failed" -ForegroundColor Red; exit 1 }
Write-Host "all $($script:count) tests passed"
