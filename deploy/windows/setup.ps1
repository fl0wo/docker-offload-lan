<#
.SYNOPSIS
    Prepares a Windows 10/11 machine as a docker-offload-lan node (WSL2 + Ubuntu 24.04).

.DESCRIPTION
    Idempotent: safe to re-run; every step prints [changed], [unchanged], [skipped]
    or [warn]. Supports -WhatIf (prints what would change, changes nothing).

      1. Checks Windows (10 2004+ / 11) and virtualization; picks WSL networking:
         mirrored on Windows 11 22H2+, NAT on Windows 10 (-Networking).
      2. Installs WSL and the Ubuntu-24.04 distro if missing (--no-launch); runs wsl --update.
      3. Merges (never overwrites) %USERPROFILE%\.wslconfig, with a backup:
           [wsl2]          memory, processors, swap, networkingMode=mirrored
           [experimental]  sparseVhd=true, autoMemoryReclaim=gradual
           [general]       instanceIdleTimeout=-1   (never stop the idle distro)
         Keys found in the wrong section (e.g. sparseVhd under [wsl2], from older
         guides) are commented out.
      4. Adds a Hyper-V firewall rule: inbound TCP 2222 to the WSL VM only.
      5. Registers a scheduled task that starts the distro at boot and keeps it running.
      6. Optional -NoSleep: never sleep/hibernate on AC power.
      7. Warns if something on Windows already listens on TCP 2222.
      8. Prints the LAN IP and the next step (install.sh inside WSL, then dol init on the Mac).

    Independent community project; not affiliated with Docker, Inc.

.EXAMPLE
    # In an elevated PowerShell:
    Set-ExecutionPolicy -Scope Process Bypass
    .\setup.ps1 -WhatIf
    .\setup.ps1 -NoSleep
#>
[CmdletBinding(SupportsShouldProcess = $true)]
param(
    [string]$Distro = 'Ubuntu-24.04',
    [ValidatePattern('^\d+(MB|GB)$')][string]$Memory = '40GB',
    [ValidateRange(1, 256)][int]$Processors = 12,
    [ValidatePattern('^\d+(MB|GB)$')][string]$Swap = '8GB',
    [ValidateRange(1, 65535)][int]$SshPort = 2222,
    # Firewall profiles for the Hyper-V rule. Private keeps the port closed on public networks.
    [ValidateSet('Any', 'Private', 'Domain', 'Public')][string[]]$FirewallProfiles = @('Private', 'Domain'),
    [switch]$NoSleep,
    [switch]$NoKeepAliveTask,
    # auto: mirrored on Windows 11 22H2+ (build 22621), nat otherwise (Windows 10).
    # nat: the Mac reaches WSL through Windows' OpenSSH server as a jump host
    # (ssh ProxyJump to localhost:$SshPort); no Hyper-V firewall rule (ADR-0036).
    [ValidateSet('auto', 'mirrored', 'nat')][string]$Networking = 'auto',
    # nat only: public key (file or "ssh-ed25519 ..." string) of the Mac for the
    # restricted jump user. Omit to skip creating the jump user.
    [string]$JumpPublicKey,
    [ValidatePattern('^[a-z][a-z0-9-]{1,19}$')][string]$JumpUser = 'dol-jump',
    # nat only: restrict Windows' OpenSSH firewall rule to these addresses (e.g. the Mac's IP).
    [string[]]$AllowFrom,
    # Run `wsl --shutdown` at the end when .wslconfig changed (stops running distros).
    [switch]$Restart
)

Set-StrictMode -Version 3.0
$ErrorActionPreference = 'Stop'

# --- constants (docs/compat.md section 3) ---
$script:WslVmCreatorId = '{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}'
$script:FirewallRuleName = "dol-ssh-$SshPort"
$script:TaskName = 'docker-offload-lan keep WSL alive'

function Write-Step {
    param([ValidateSet('changed', 'unchanged', 'skipped', 'warn', 'would change')][string]$State, [string]$Message)
    $color = @{ 'changed' = 'Green'; 'unchanged' = 'DarkGray'; 'skipped' = 'DarkGray'; 'warn' = 'Yellow'; 'would change' = 'Cyan' }[$State]
    Write-Host ("[{0}] {1}" -f $State, $Message) -ForegroundColor $color
}

# ---------------------------------------------------------------------------
# INI handling for .wslconfig. Pure functions (no I/O) so they are testable
# on any OS with pwsh: see deploy/windows/tests/WslConfig.Tests.ps1.
# ---------------------------------------------------------------------------

function Merge-IniContent {
    <#
    .SYNOPSIS
        Merges settings into INI text, preserving comments, order, unknown keys
        and line endings of untouched lines.
    .PARAMETER Lines
        Existing file content as lines (may be empty).
    .PARAMETER Settings
        Ordered hashtable: section name -> ordered hashtable of key -> value.
    .PARAMETER Relocate
        Keys that belong only to the section given in Settings: occurrences in
        any other section are commented out.
    .OUTPUTS
        PSCustomObject with Lines (string[]) and Changed (bool).
    #>
    [CmdletBinding()]
    param(
        [AllowEmptyCollection()][AllowEmptyString()][string[]]$Lines = @(),
        [Parameter(Mandatory)][System.Collections.IDictionary]$Settings,
        [switch]$Relocate
    )
    if ($null -eq $Lines) { $Lines = @() }
    $out = [System.Collections.Generic.List[string]]::new()
    foreach ($l in $Lines) { $out.Add([string]$l) }

    # Which section owns each managed key (case-insensitive).
    $owner = @{}
    foreach ($sec in $Settings.Keys) {
        foreach ($k in $Settings[$sec].Keys) { $owner[$k.ToLowerInvariant()] = $sec.ToLowerInvariant() }
    }

    $sectionRe = '^\s*\[([^\]]+)\]\s*(?:[#;].*)?$'
    $keyRe = '^\s*([A-Za-z0-9_.-]+)\s*=\s*(.*?)\s*$'

    # Pass 1: update keys in place; comment out keys in the wrong section.
    $current = ''
    $seen = @{}  # "section|key" -> $true
    for ($i = 0; $i -lt $out.Count; $i++) {
        $line = $out[$i]
        if ($line -match $sectionRe) { $current = $Matches[1].Trim().ToLowerInvariant(); continue }
        if ($line -match '^\s*[#;]' -or $line -notmatch $keyRe) { continue }
        $key = $Matches[1]; $val = $Matches[2]
        $lk = $key.ToLowerInvariant()
        if (-not $owner.ContainsKey($lk)) { continue }
        if ($owner[$lk] -ne $current) {
            if ($Relocate) {
                $out[$i] = "# $line  # moved to [$($owner[$lk])] by docker-offload-lan"
            }
            continue
        }
        $secName = ($Settings.Keys | Where-Object { $_.ToLowerInvariant() -eq $current } | Select-Object -First 1)
        $wantKey = ($Settings[$secName].Keys | Where-Object { $_.ToLowerInvariant() -eq $lk } | Select-Object -First 1)
        $want = [string]$Settings[$secName][$wantKey]
        if ($seen.ContainsKey("$current|$lk")) {
            # Duplicate key: WSL uses one of them; keep ours unambiguous.
            $out[$i] = "# $line  # duplicate, disabled by docker-offload-lan"
            continue
        }
        $seen["$current|$lk"] = $true
        if ($val -ne $want) { $out[$i] = "$key=$want" }
    }

    # Pass 2: append missing keys to their (last) section, or add the section.
    foreach ($sec in $Settings.Keys) {
        $ls = $sec.ToLowerInvariant()
        $missing = @($Settings[$sec].Keys | Where-Object { -not $seen.ContainsKey("$ls|$($_.ToLowerInvariant())") })
        if ($missing.Count -eq 0) { continue }
        $start = -1
        for ($i = 0; $i -lt $out.Count; $i++) {
            if ($out[$i] -match $sectionRe -and $Matches[1].Trim().ToLowerInvariant() -eq $ls) { $start = $i }
        }
        $newLines = @($missing | ForEach-Object { "$_=$($Settings[$sec][$_])" })
        if ($start -lt 0) {
            if ($out.Count -gt 0 -and $out[$out.Count - 1].Trim() -ne '') { $out.Add('') }
            $out.Add("[$sec]")
            foreach ($n in $newLines) { $out.Add($n) }
            continue
        }
        # End of that section: before the next section header, minus trailing blanks.
        $end = $out.Count
        for ($i = $start + 1; $i -lt $out.Count; $i++) {
            if ($out[$i] -match $sectionRe) { $end = $i; break }
        }
        while ($end -gt $start + 1 -and $out[$end - 1].Trim() -eq '') { $end-- }
        $out.InsertRange($end, [string[]]$newLines)
    }

    $changed = ($out.Count -ne $Lines.Count)
    if (-not $changed) {
        for ($i = 0; $i -lt $out.Count; $i++) { if ($out[$i] -cne $Lines[$i]) { $changed = $true; break } }
    }
    [pscustomobject]@{ Lines = [string[]]$out.ToArray(); Changed = $changed }
}

function Get-DolWslConfig {
    param([string]$Memory, [int]$Processors, [string]$Swap, [string]$NetworkingMode = 'mirrored')
    $s = [ordered]@{}
    $s['wsl2'] = [ordered]@{
        memory     = $Memory
        processors = "$Processors"
        swap       = $Swap
    }
    # NAT is WSL's default; only mirrored needs to be written.
    if ($NetworkingMode -eq 'mirrored') { $s['wsl2']['networkingMode'] = 'mirrored' }
    # Still experimental per docs/compat.md (2026-11).
    $s['experimental'] = [ordered]@{
        sparseVhd         = 'true'
        autoMemoryReclaim = 'gradual'
    }
    # -1 disables the idle shutdown of the distro (default 15000 ms).
    $s['general'] = [ordered]@{
        instanceIdleTimeout = '-1'
    }
    $s
}

# ---------------------------------------------------------------------------
# Steps (Windows only)
# ---------------------------------------------------------------------------

function Assert-Admin {
    $id = [Security.Principal.WindowsIdentity]::GetCurrent()
    if (-not ([Security.Principal.WindowsPrincipal]$id).IsInRole([Security.Principal.WindowsBuiltInRole]::Administrator)) {
        throw 'Run this script in an elevated PowerShell (Run as administrator).'
    }
}

function Test-Prerequisite {
    $build = [Environment]::OSVersion.Version.Build
    $script:NetMode = Resolve-NetworkingMode -Requested $Networking -Build $build
    $os = if ($build -ge 22000) { 'Windows 11' } else { 'Windows 10' }
    Write-Step unchanged "$os build $build, WSL networking: $($script:NetMode)"
    $cs = Get-CimInstance Win32_ComputerSystem
    if ($cs.HypervisorPresent) {
        Write-Step unchanged 'virtualization: hypervisor present'
    } else {
        $cpu = Get-CimInstance Win32_Processor | Select-Object -First 1
        if (-not $cpu.VirtualizationFirmwareEnabled) {
            throw 'Virtualization is disabled in the firmware (BIOS/UEFI). Enable SVM (AMD) or VT-x (Intel), then re-run.'
        }
        Write-Step warn 'virtualization enabled in firmware, hypervisor not running yet (WSL installation enables it; a reboot may be needed)'
    }
}

# Pure: which networking mode to use (tested in WslConfig.Tests.ps1).
function Resolve-NetworkingMode {
    param([string]$Requested, [int]$Build)
    if ($Build -lt 19041) { throw "WSL 2 needs Windows 10 version 2004 (build 19041) or newer; this is build $Build. Run Windows Update." }
    switch ($Requested) {
        'mirrored' {
            if ($Build -lt 22621) { throw "Mirrored WSL networking needs Windows 11 22H2 or newer (build 22621+); this is build $Build. Use -Networking nat (Windows' OpenSSH as jump host)." }
            return 'mirrored'
        }
        'nat' { return 'nat' }
        default { if ($Build -ge 22621) { return 'mirrored' } else { return 'nat' } }
    }
}

# Pure: the authorized_keys line for the jump user: forwarding to the WSL sshd
# only; no shell, pty, agent or X11 (restrict), then re-allow port forwarding.
function Get-JumpAuthorizedKeyLine {
    param([string]$PublicKey, [int]$Port)
    $k = $PublicKey.Trim()
    if ($k -notmatch '^(ssh-ed25519|ssh-rsa|ecdsa-sha2-nistp(256|384|521)|sk-ssh-ed25519@openssh\.com) [A-Za-z0-9+/=]+( .*)?$') {
        throw "JumpPublicKey does not look like an OpenSSH public key: $k"
    }
    'restrict,port-forwarding,permitopen="localhost:{0}",permitopen="127.0.0.1:{0}" {1}' -f $Port, $k
}

# Pure: the sshd_config block for the jump user (Windows OpenSSH).
function Get-JumpSshdBlock {
    param([string]$User, [int]$Port)
    @(
        '# docker-offload-lan jump user (begin) - managed by setup.ps1'
        "Match User $User"
        '    AuthorizedKeysFile __PROGRAMDATA__/ssh/dol_jump_authorized_keys'
        '    AllowTcpForwarding local'
        "    PermitOpen localhost:$Port 127.0.0.1:$Port"
        '    PermitTTY no'
        '    X11Forwarding no'
        '    AllowAgentForwarding no'
        '    ForceCommand cmd.exe /c exit 1'
        '# docker-offload-lan jump user (end)'
    )
}

function Get-WslDistroName {
    $env:WSL_UTF8 = '1'  # wsl.exe prints UTF-16 otherwise
    $out = & wsl.exe --list --quiet 2>$null
    if ($LASTEXITCODE -ne 0) { return @() }
    @($out | ForEach-Object { $_.Trim([char]0, ' ') } | Where-Object { $_ })
}

function Install-WslDistro {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    $wsl = Get-Command wsl.exe -ErrorAction SilentlyContinue
    $distros = if ($wsl) { Get-WslDistroName } else { @() }
    if ($distros -contains $Distro) {
        Write-Step unchanged "WSL distro $Distro installed"
    } elseif ($PSCmdlet.ShouldProcess($Distro, 'wsl --install --no-launch')) {
        & wsl.exe --install -d $Distro --no-launch
        if ($LASTEXITCODE -ne 0) { throw "wsl --install -d $Distro failed (exit $LASTEXITCODE). If it asks for a reboot, reboot and re-run this script." }
        Write-Step changed "WSL distro $Distro installed (open it once with ``wsl -d $Distro`` to create your Linux user)"
        $script:NeedsFirstLaunch = $true
    } else {
        Write-Step 'would change' "install WSL distro $Distro"
    }
    if ($PSCmdlet.ShouldProcess('WSL', 'wsl --update')) {
        $u = & wsl.exe --update 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Write-Step warn "wsl --update failed: $($u.Trim())" }
        elseif ($u -match 'already|bereits|latest|most recent') { Write-Step unchanged 'WSL is up to date' }
        else { Write-Step changed "WSL updated: $($u.Trim())" }
    }
}

function Update-WslConfig {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    $path = Join-Path $env:USERPROFILE '.wslconfig'
    $lines = if (Test-Path $path) { [string[]]@(Get-Content -LiteralPath $path) } else { [string[]]@() }
    $settings = Get-DolWslConfig -Memory $Memory -Processors $Processors -Swap $Swap -NetworkingMode $script:NetMode
    $r = Merge-IniContent -Lines $lines -Settings $settings -Relocate
    if (-not $r.Changed) { Write-Step unchanged ".wslconfig ($path)"; return $false }
    if ($PSCmdlet.ShouldProcess($path, 'merge docker-offload-lan settings into .wslconfig')) {
        if (Test-Path $path) {
            $bak = "$path.bak-dol-$(Get-Date -Format yyyyMMdd-HHmmss)"
            Copy-Item -LiteralPath $path -Destination $bak
            Write-Host "  backup: $bak"
        }
        # WSL reads UTF-8 (no BOM needed); keep it ASCII-clean.
        [IO.File]::WriteAllLines($path, $r.Lines, [Text.UTF8Encoding]::new($false))
        Write-Step changed ".wslconfig ($path): memory=$Memory processors=$Processors swap=$Swap networking=$($script:NetMode), sparseVhd, autoMemoryReclaim=gradual, instanceIdleTimeout=-1"
        return $true
    }
    Write-Step 'would change' ".wslconfig ($path)"
    return $false
}

function Set-SparseVhd {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    # sparseVhd in .wslconfig only affects new VHDs; convert the existing one.
    $key = Get-ChildItem 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Lxss' -ErrorAction SilentlyContinue |
        Where-Object { (Get-ItemProperty $_.PSPath).DistributionName -eq $Distro } | Select-Object -First 1
    if (-not $key) { Write-Step skipped "sparse VHD (distro $Distro not registered yet)"; return }
    $vhd = Join-Path (Get-ItemProperty $key.PSPath).BasePath 'ext4.vhdx'
    $vhd = $vhd -replace '^\\\\\?\\', ''
    if (-not (Test-Path -LiteralPath $vhd)) { Write-Step skipped "sparse VHD ($vhd not found)"; return }
    $q = & fsutil.exe sparse queryflag $vhd 2>&1 | Out-String
    if ($q -notmatch 'NOT') { Write-Step unchanged "sparse VHD ($vhd)"; return }
    if ($PSCmdlet.ShouldProcess($vhd, "wsl --terminate $Distro; wsl --manage $Distro --set-sparse true")) {
        & wsl.exe --terminate $Distro | Out-Null
        $o = & wsl.exe --manage $Distro --set-sparse true 2>&1 | Out-String
        if ($LASTEXITCODE -ne 0) { Write-Step warn "could not make the VHD sparse (needs WSL 2.5+): $($o.Trim())" }
        else { Write-Step changed "sparse VHD ($vhd): freed disk space now returns to Windows" }
    }
}

function Set-FirewallRule {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    if ($script:NetMode -eq 'nat') {
        Write-Step skipped "Hyper-V firewall rule (NAT networking: the Mac comes in through Windows' OpenSSH, see the jump host step)"
        return
    }
    if (-not (Get-Command New-NetFirewallHyperVRule -ErrorAction SilentlyContinue)) {
        Write-Step warn 'New-NetFirewallHyperVRule is not available (needs Windows 11 22H2+); open TCP 2222 for WSL manually'
        return
    }
    $profiles = ($FirewallProfiles | Sort-Object -Unique) -join ', '
    $rule = Get-NetFirewallHyperVRule -Name $script:FirewallRuleName -ErrorAction SilentlyContinue
    if ($rule) {
        $haveProfiles = (("$($rule.Profiles)" -split ',\s*' | Sort-Object -Unique) -join ', ')
        $ok = ($rule.Direction -eq 'Inbound') -and ($rule.Action -eq 'Allow') -and ("$($rule.Protocol)" -match '^(TCP|6)$') -and
            ("$($rule.LocalPorts)" -eq "$SshPort") -and ($rule.VMCreatorId -eq $script:WslVmCreatorId) -and
            ("$($rule.Enabled)" -eq 'True') -and ($haveProfiles -eq $profiles)
        if ($ok) { Write-Step unchanged "Hyper-V firewall rule $($script:FirewallRuleName) (inbound TCP $SshPort to WSL, profiles $profiles)"; return }
        if ($PSCmdlet.ShouldProcess($script:FirewallRuleName, 'replace Hyper-V firewall rule')) {
            Remove-NetFirewallHyperVRule -Name $script:FirewallRuleName
        } else { Write-Step 'would change' "Hyper-V firewall rule $($script:FirewallRuleName)"; return }
    }
    if ($PSCmdlet.ShouldProcess($script:FirewallRuleName, "allow inbound TCP $SshPort to the WSL VM")) {
        New-NetFirewallHyperVRule -Name $script:FirewallRuleName -DisplayName "docker-offload-lan SSH ($SshPort)" `
            -Direction Inbound -Action Allow -VMCreatorId $script:WslVmCreatorId -Protocol TCP -LocalPorts $SshPort `
            -Profiles $FirewallProfiles -Enabled True | Out-Null
        Write-Step changed "Hyper-V firewall rule $($script:FirewallRuleName): inbound TCP $SshPort to WSL only (profiles $profiles)"
    } else { Write-Step 'would change' "Hyper-V firewall rule $($script:FirewallRuleName)" }

    if ($FirewallProfiles -notcontains 'Any' -and $FirewallProfiles -notcontains 'Public') {
        $public = @(Get-NetConnectionProfile -ErrorAction SilentlyContinue | Where-Object NetworkCategory -eq 'Public')
        foreach ($p in $public) {
            Write-Step warn ("network '{0}' is Public, so port {1} stays closed on it. If this is your home LAN: Set-NetConnectionProfile -InterfaceIndex {2} -NetworkCategory Private" -f $p.Name, $SshPort, $p.InterfaceIndex)
        }
    }
}

function Test-PortFree {
    $l = @(Get-NetTCPConnection -LocalPort $SshPort -State Listen -ErrorAction SilentlyContinue)
    if ($l.Count -eq 0) { Write-Step unchanged "nothing on Windows listens on TCP $SshPort"; return }
    foreach ($c in $l) {
        $p = Get-Process -Id $c.OwningProcess -ErrorAction SilentlyContinue
        $name = if ($p) { $p.ProcessName } else { "pid $($c.OwningProcess)" }
        # In mirrored mode, listeners inside WSL can show up as WSL/VM processes.
        if ($name -match '^(wslrelay|wslservice|vmmem|vmmemWSL|vmcompute)$') {
            Write-Step unchanged "TCP $SshPort is held by $name (sshd inside WSL)"
        } else {
            Write-Step warn "TCP $SshPort is already used on Windows by $name; sshd inside WSL cannot bind it in mirrored mode. Stop that program or use -SshPort and install.sh with a matching port."
        }
    }
}

function Set-JumpHost {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    if ($script:NetMode -ne 'nat') { return }
    # 1. Windows OpenSSH server
    $cap = Get-WindowsCapability -Online -Name 'OpenSSH.Server*' | Select-Object -First 1
    if ($cap.State -ne 'Installed') {
        if ($PSCmdlet.ShouldProcess('OpenSSH.Server', 'Add-WindowsCapability')) {
            Add-WindowsCapability -Online -Name $cap.Name | Out-Null
            Write-Step changed "Windows OpenSSH server installed ($($cap.Name))"
        } else { Write-Step 'would change' 'install the Windows OpenSSH server'; return }
    } else { Write-Step unchanged 'Windows OpenSSH server installed' }
    $svc = Get-Service sshd -ErrorAction SilentlyContinue
    if ($svc -and ($svc.StartType -ne 'Automatic' -or $svc.Status -ne 'Running')) {
        if ($PSCmdlet.ShouldProcess('sshd', 'set Automatic and start')) {
            Set-Service sshd -StartupType Automatic; Start-Service sshd
            Write-Step changed 'Windows sshd: automatic start, running'
        } else { Write-Step 'would change' 'Windows sshd: automatic start, running' }
    } elseif ($svc) { Write-Step unchanged 'Windows sshd: automatic start, running' }
    # 2. Firewall: optionally only the Mac(s)
    if ($AllowFrom) {
        $rule = Get-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -ErrorAction SilentlyContinue
        $have = if ($rule) { @(($rule | Get-NetFirewallAddressFilter).RemoteAddress) } else { @() }
        if ($rule -and ((@($have | Sort-Object) -join ',') -ne (@($AllowFrom | Sort-Object) -join ','))) {
            if ($PSCmdlet.ShouldProcess('OpenSSH-Server-In-TCP', "allow only $($AllowFrom -join ', ')")) {
                Set-NetFirewallRule -Name 'OpenSSH-Server-In-TCP' -RemoteAddress $AllowFrom
                Write-Step changed "Windows sshd firewall rule: only $($AllowFrom -join ', ')"
            } else { Write-Step 'would change' "Windows sshd firewall rule: only $($AllowFrom -join ', ')" }
        } elseif ($rule) { Write-Step unchanged "Windows sshd firewall rule: only $($AllowFrom -join ', ')" }
    }
    if (-not $JumpPublicKey) {
        Write-Step skipped "jump user $JumpUser (pass -JumpPublicKey with the Mac's public key to create it)"
        return
    }
    $pub = if (Test-Path -LiteralPath $JumpPublicKey) { (Get-Content -LiteralPath $JumpPublicKey -Raw) } else { $JumpPublicKey }
    $line = Get-JumpAuthorizedKeyLine -PublicKey $pub -Port $SshPort
    # 3. Local user without a usable password (random, never shown): key-only.
    if (-not (Get-LocalUser -Name $JumpUser -ErrorAction SilentlyContinue)) {
        if ($PSCmdlet.ShouldProcess($JumpUser, 'create local user (not an administrator)')) {
            $bytes = New-Object byte[] 32; [Security.Cryptography.RandomNumberGenerator]::Create().GetBytes($bytes)
            $pw = ConvertTo-SecureString ([Convert]::ToBase64String($bytes) + 'aA1!') -AsPlainText -Force
            New-LocalUser -Name $JumpUser -Password $pw -PasswordNeverExpires -UserMayNotChangePassword `
                -Description 'docker-offload-lan: ssh jump to WSL only (key-only, no shell)' | Out-Null
            Write-Step changed "local user $JumpUser (standard user, random password never shown, key-only ssh)"
        } else { Write-Step 'would change' "create local user $JumpUser" }
    } else { Write-Step unchanged "local user $JumpUser" }
    # 4. Its authorized key, forwarding to localhost:$SshPort only.
    $keys = Join-Path $env:ProgramData 'ssh\dol_jump_authorized_keys'
    $cur = if (Test-Path -LiteralPath $keys) { @(Get-Content -LiteralPath $keys) } else { @() }
    if ($cur -notcontains $line) {
        if ($PSCmdlet.ShouldProcess($keys, 'add the jump key')) {
            [IO.File]::WriteAllLines($keys, [string[]]@($cur + $line), [Text.UTF8Encoding]::new($false))
            & icacls.exe $keys /inheritance:r /grant 'Administrators:F' /grant 'SYSTEM:F' | Out-Null
            Write-Step changed "jump key ($keys): may only forward to localhost:$SshPort"
        } else { Write-Step 'would change' "jump key ($keys)" }
    } else { Write-Step unchanged "jump key ($keys)" }
    # 5. sshd_config Match block.
    $cfg = Join-Path $env:ProgramData 'ssh\sshd_config'
    $text = Get-Content -LiteralPath $cfg -Raw
    $block = (Get-JumpSshdBlock -User $JumpUser -Port $SshPort) -join "`r`n"
    if (-not $text.Contains($block)) {
        if ($PSCmdlet.ShouldProcess($cfg, "add Match User $JumpUser")) {
            Copy-Item -LiteralPath $cfg -Destination "$cfg.bak-dol-$(Get-Date -Format yyyyMMdd-HHmmss)"
            $text = [regex]::Replace($text, '(?s)\r?\n?# docker-offload-lan jump user \(begin\).*?# docker-offload-lan jump user \(end\)\r?\n?', "`r`n")
            [IO.File]::WriteAllText($cfg, $text.TrimEnd() + "`r`n`r`n" + $block + "`r`n", [Text.UTF8Encoding]::new($false))
            $t = & "$env:WINDIR\System32\OpenSSH\sshd.exe" -t 2>&1 | Out-String
            if ($LASTEXITCODE -ne 0) { throw "sshd_config is invalid after the change (a backup is next to it): $t" }
            Restart-Service sshd
            Write-Step changed "sshd_config: Match User $JumpUser (forwarding to localhost:$SshPort only, no shell)"
        } else { Write-Step 'would change' "sshd_config: Match User $JumpUser" }
    } else { Write-Step unchanged "sshd_config: Match User $JumpUser" }
}

function Register-KeepAliveTask {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    if ($NoKeepAliveTask) { Write-Step skipped 'keep-alive scheduled task (-NoKeepAliveTask)'; return }
    # Not "$env:USERDOMAIN\$env:USERNAME": in an OpenSSH session USERDOMAIN is
    # WORKGROUP and the task principal cannot be resolved (found on a real tower).
    $user = [System.Security.Principal.WindowsIdentity]::GetCurrent().Name
    $exe = Join-Path $env:WINDIR 'System32\wsl.exe'
    # A process that never exits keeps the distro (and with it systemd, sshd,
    # docker and dol-node) running; instanceIdleTimeout=-1 is the second guard.
    $taskArgs = "-d $Distro -u root --exec /bin/sleep infinity"
    $existing = Get-ScheduledTask -TaskName $script:TaskName -ErrorAction SilentlyContinue
    if ($existing) {
        $a = $existing.Actions | Select-Object -First 1
        $hasBoot = @($existing.Triggers | Where-Object { $_.CimClass.CimClassName -eq 'MSFT_TaskBootTrigger' }).Count -gt 0
        if ($a.Execute -eq $exe -and $a.Arguments -eq $taskArgs -and $existing.Principal.UserId -match [regex]::Escape($env:USERNAME) -and $hasBoot) {
            Write-Step unchanged "scheduled task '$($script:TaskName)'"
            return
        }
    }
    if (-not $PSCmdlet.ShouldProcess($script:TaskName, 'register scheduled task (at startup and logon, keeps WSL running)')) {
        Write-Step 'would change' "scheduled task '$($script:TaskName)'"; return
    }
    $action = New-ScheduledTaskAction -Execute $exe -Argument $taskArgs
    $triggers = @((New-ScheduledTaskTrigger -AtStartup), (New-ScheduledTaskTrigger -AtLogOn -User $user))
    # S4U: runs at boot without a stored password and without a logged-on user.
    $principal = New-ScheduledTaskPrincipal -UserId $user -LogonType S4U -RunLevel Limited
    $settings = New-ScheduledTaskSettingsSet -AllowStartIfOnBatteries -DontStopIfGoingOnBatteries -StartWhenAvailable `
        -ExecutionTimeLimit ([TimeSpan]::Zero) -MultipleInstances IgnoreNew -RestartCount 3 -RestartInterval (New-TimeSpan -Minutes 1)
    Register-ScheduledTask -TaskName $script:TaskName -Action $action -Trigger $triggers -Principal $principal -Settings $settings `
        -Description 'Starts the docker-offload-lan WSL distro at boot and keeps it running.' -Force | Out-Null
    Write-Step changed "scheduled task '$($script:TaskName)' (at startup + logon, as $user)"
    if (-not $script:NeedsFirstLaunch) { Start-ScheduledTask -TaskName $script:TaskName }
}

function Set-NoSleep {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    if (-not $NoSleep) { Write-Step skipped 'power settings (pass -NoSleep to never sleep on AC power)'; return }
    $changed = $false
    foreach ($setting in @(@{ Sub = 'SUB_SLEEP'; Name = 'STANDBYIDLE'; Flag = 'standby-timeout-ac' }, @{ Sub = 'SUB_SLEEP'; Name = 'HIBERNATEIDLE'; Flag = 'hibernate-timeout-ac' })) {
        $q = & powercfg.exe /query SCHEME_CURRENT $setting.Sub $setting.Name | Out-String
        if ($q -match 'Current AC Power Setting Index:\s*0x([0-9a-fA-F]+)' -and [Convert]::ToInt32($Matches[1], 16) -eq 0) { continue }
        if ($PSCmdlet.ShouldProcess($setting.Flag, 'powercfg /change ... 0')) {
            & powercfg.exe /change $setting.Flag 0
            $changed = $true
        }
    }
    if ($changed) { Write-Step changed 'power: never sleep or hibernate on AC power' } else { Write-Step unchanged 'power: never sleep or hibernate on AC power' }
}

function Get-LanIPv4 {
    $c = Get-NetIPConfiguration | Where-Object { $_.IPv4DefaultGateway -and $_.NetAdapter.Status -eq 'Up' } | Select-Object -First 1
    if ($c) { return ($c.IPv4Address | Select-Object -First 1).IPAddress }
    return $null
}

function Show-NextStep {
    $ip = Get-LanIPv4
    # setup.ps1 may be run from a repo checkout (deploy\windows) or copied
    # next to install.sh; fall back to a placeholder if neither is found.
    $installWsl = '<path to install.sh inside WSL>'
    $candidates = @((Join-Path $PSScriptRoot 'install.sh'))
    $parent = Split-Path -Parent $PSScriptRoot
    if ($parent) { $candidates += (Join-Path (Join-Path $parent 'linux') 'install.sh') }
    foreach ($installWin in $candidates) {
        if (Test-Path -LiteralPath $installWin) {
            $installWsl = '/mnt/' + $installWin.Substring(0, 1).ToLowerInvariant() + ($installWin.Substring(2) -replace '\\', '/')
            break
        }
    }
    Write-Host ''
    Write-Host "LAN IP of this machine: $(if ($ip) { $ip } else { '(not found: check Get-NetIPConfiguration)' })" -ForegroundColor Green
    Write-Host ''
    Write-Host 'Next steps:'
    if ($script:WslConfigChanged -and -not $Restart) {
        Write-Host '  0. Apply .wslconfig:  wsl --shutdown   (or re-run with -Restart)'
    }
    if ($script:NeedsFirstLaunch) {
        Write-Host "  1. Open the distro once to create your Linux user:  wsl -d $Distro"
    }
    Write-Host "  2. Install the node inside WSL (copy your Mac's ~/.ssh/id_ed25519.pub here first):"
    Write-Host "       wsl -d $Distro -u root -- bash $installWsl --pubkey `"<contents of id_ed25519.pub>`""
    Write-Host '     If it enables systemd it asks you to run `wsl --shutdown` and re-run it once.'
    $hostArg = if ($ip) { $ip } else { '<LAN IP>' }
    if ($script:NetMode -eq 'nat') {
        Write-Host "  3. On the Mac:  dol init --name tower --host localhost --jump $JumpUser@$hostArg"
        Write-Host "     (ssh goes Mac -> Windows OpenSSH (${hostArg}:22, user $JumpUser) -> WSL sshd on localhost:$SshPort)"
    } else {
        Write-Host "  3. On the Mac:  dol init --name tower --host $hostArg"
    }
}

function Invoke-Setup {
    [CmdletBinding(SupportsShouldProcess = $true)] param()
    Assert-Admin
    Write-Host "docker-offload-lan Windows setup (distro $Distro, memory $Memory, processors $Processors, swap $Swap)" -ForegroundColor Cyan
    $script:NeedsFirstLaunch = $false
    Test-Prerequisite
    Install-WslDistro
    $script:WslConfigChanged = Update-WslConfig
    Set-SparseVhd
    Set-FirewallRule
    Set-JumpHost
    if ($script:NetMode -eq 'mirrored') { Test-PortFree }
    Register-KeepAliveTask
    Set-NoSleep
    if ($script:WslConfigChanged -and $Restart -and $PSCmdlet.ShouldProcess('WSL', 'wsl --shutdown (apply .wslconfig)')) {
        & wsl.exe --shutdown
        Write-Step changed 'wsl --shutdown (the keep-alive task restarts the distro)'
        if (-not $NoKeepAliveTask -and -not $script:NeedsFirstLaunch) { Start-ScheduledTask -TaskName $script:TaskName }
    }
    Show-NextStep
}

# Run unless dot-sourced (tests dot-source this file to get the functions).
if ($MyInvocation.InvocationName -ne '.') {
    Invoke-Setup
}
