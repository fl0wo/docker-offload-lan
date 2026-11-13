# Setting up a Windows 11 node

This turns a Windows 11 PC into a docker-offload-lan node. Docker Engine runs natively inside a WSL2 Ubuntu 24.04 distro (no Docker Desktop), sshd listens on port 2222 inside WSL, and the dol node agent runs as a systemd service. For a plain Linux node, skip to [step 3](#3-install-the-node-inside-wsl) and run `install.sh` directly.

Requirements:

- Windows 11 22H2 or newer (build 22621+), which mirrored networking needs.
- Hardware virtualization enabled in the BIOS/UEFI (SVM on AMD, VT-x on Intel).
- An administrator account.
- About 30 GB free on C:. More is better: images and kind clusters live in the WSL virtual disk on C:.

Both scripts are idempotent. Re-running them is safe: every step prints `[changed]`, `[unchanged]` or `[skipped]`.

## 1. Get the scripts

Clone the repository or unpack a release archive (it contains `deploy/`), for example to `C:\Users\<you>\docker-offload-lan`.

## 2. Run `setup.ps1` (Windows side)

In an **elevated** PowerShell (Run as administrator):

```powershell
cd C:\Users\<you>\docker-offload-lan\deploy\windows
Set-ExecutionPolicy -Scope Process Bypass
.\setup.ps1 -WhatIf          # show what would change
.\setup.ps1 -NoSleep          # do it; -NoSleep: never sleep on AC power
```

| Parameter | Default | Meaning |
|---|---|---|
| `-Distro` | `Ubuntu-24.04` | WSL distro to install and keep alive |
| `-Memory` | `40GB` | WSL VM memory limit (`[wsl2] memory`) |
| `-Processors` | `12` | `[wsl2] processors` |
| `-Swap` | `8GB` | `[wsl2] swap` |
| `-SshPort` | `2222` | port opened in the Hyper-V firewall |
| `-FirewallProfiles` | `Private,Domain` | network profiles the firewall rule applies to |
| `-NoSleep` | off | AC power: never sleep or hibernate |
| `-NoKeepAliveTask` | off | do not register the keep-alive scheduled task |
| `-Restart` | off | run `wsl --shutdown` at the end if `.wslconfig` changed |

What it does:

1. **Checks** for Windows 11 22H2+ and virtualization.
2. **Installs WSL and Ubuntu 24.04** if they are missing (`wsl --install -d Ubuntu-24.04 --no-launch`), then runs `wsl --update`. After a fresh install, open the distro once (`wsl -d Ubuntu-24.04`) to create your Linux user.
3. **Merges `%USERPROFILE%\.wslconfig`.** It never overwrites the file, and it saves a `.bak-dol-<timestamp>` copy first. Keys go into the sections WSL actually reads (verified in [compat.md](compat.md) §3):

   ```ini
   [wsl2]
   memory=40GB
   processors=12
   swap=8GB
   networkingMode=mirrored

   [experimental]
   sparseVhd=true
   autoMemoryReclaim=gradual

   [general]
   instanceIdleTimeout=-1
   ```

   Older guides put `sparseVhd` and `autoMemoryReclaim` under `[wsl2]`, where WSL ignores them. If the script finds them there, it comments them out. `sparseVhd` only affects newly created disks, so the script also converts the existing distro's disk with `wsl --manage Ubuntu-24.04 --set-sparse true` (needs WSL 2.5+).
4. **Adds a Hyper-V firewall rule** `dol-ssh-2222`: inbound TCP 2222 to the WSL VM only (VMCreatorId `{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}`), on Private and Domain networks. If your LAN shows up as **Public**, the script tells you how to switch it to Private.
5. **Checks that port 2222 is free on Windows.** In mirrored mode Windows and WSL share ports, so a Windows program on 2222 would block sshd in WSL.
6. **Registers the scheduled task** "docker-offload-lan keep WSL alive". At startup and at logon it runs `wsl -d Ubuntu-24.04 -u root --exec /bin/sleep infinity`. This boots the distro (systemd then starts docker, ssh and dol-node) and keeps it running. `instanceIdleTimeout=-1` is a second guard against idle shutdown. See [ADR-0031](adr/0031-wsl-keepalive.md).
7. **Prints the LAN IP** and the next commands.

Apply `.wslconfig` changes with `wsl --shutdown` (or pass `-Restart`).

> If the task does not start WSL at boot on your Windows build (WSL under an S4U logon is not supported everywhere), open Task Scheduler, edit the task, and choose "Run only when user is logged on". With automatic sign-in the effect is the same.

## 3. Install the node inside WSL

Copy your Mac's public key (`cat ~/.ssh/id_ed25519.pub` on the Mac; create one with `ssh-keygen -t ed25519` if you have none). Then, in PowerShell:

```powershell
wsl -d Ubuntu-24.04 -u root -- bash /mnt/c/Users/<you>/docker-offload-lan/deploy/linux/install.sh --pubkey "ssh-ed25519 AAAA... you@mac"
```

On the first run, if systemd is not enabled yet, the script sets `[boot] systemd=true` in `/etc/wsl.conf` and stops. In that case run `wsl --shutdown` and run the same command again.

`install.sh` then:

- installs Docker Engine from Docker's official apt repository (deb822 `docker.sources`, key in `/etc/apt/keyrings/docker.asc`);
- merges `/etc/docker/daemon.json`: `local` log driver capped at 20 MB × 5 files, and BuildKit GC with `defaultMaxUsedSpace` 40GB (`--buildkit-max` changes it). Existing keys are kept and a backup is written;
- creates user `dol` (groups `dol` and `docker`) with key-only login and your key in `authorized_keys`;
- adds the sshd drop-in on **port 2222** (key-only, `AllowUsers dol`, `MaxSessions 64`, all scoped to port 2222). On WSL, sshd does **not** listen on 22, which belongs to Windows OpenSSH. See [ADR-0030](adr/0030-sshd-on-2222-match-localport.md);
- installs pinned `kind` v0.33.0 and `kubectl` v1.37.2, checking their sha256, plus `rsync` and `jq`;
- installs `dol`, either from `--dol-binary <path>` or from the latest GitHub release with a checksum check;
- creates `/srv/dol` (the workspace sync root) and `/etc/docker-offload-lan/node.yaml`. An existing `node.yaml` is never overwritten;
- enables `dol-node.service`, which runs as user `dol` with systemd hardening;
- runs `dol doctor --node`.

Useful flags: `--dry-run`, `--node-name tower`, `--dol-binary ./dol`, `--dol-version v0.1.0`, `--skip-doctor`. Run `install.sh --help` for the full list.

## 4. Check the node

Inside WSL:

```sh
sudo dol doctor --node
```

Every line should show ✓ (or · for checks that don't apply). [troubleshooting.md](troubleshooting.md) has one section per check id.

## 5. Continue on the Mac

See [setup-mac.md](setup-mac.md): `dol init --name tower --host <LAN IP printed by setup.ps1>`.

## Undo

- Windows: `Unregister-ScheduledTask -TaskName 'docker-offload-lan keep WSL alive'`, `Remove-NetFirewallHyperVRule -Name dol-ssh-2222`, then restore `.wslconfig` from its `.bak-dol-*` copy.
- WSL: `sudo systemctl disable --now dol-node`, then remove `/etc/systemd/system/dol-node.service`, `/etc/ssh/sshd_config.d/10-docker-offload-lan.conf`, `/etc/docker-offload-lan`, `/var/lib/docker-offload-lan` and `/srv/dol`, and run `sudo userdel -r dol`. Or simply `wsl --unregister Ubuntu-24.04`, which deletes the whole distro and its data.
