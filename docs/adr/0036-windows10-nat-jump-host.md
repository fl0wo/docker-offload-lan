# ADR-0036: Windows 10 nodes — NAT WSL networking with Windows OpenSSH as jump host

- Status: accepted
- Date: 2026-11-13

## Context
The brief assumes Windows 11: mirrored WSL networking (the WSL sshd on :2222 is reachable on the LAN IP) and a Hyper-V firewall rule. The first real tower is Windows 10 22H2 (build 19045). Mirrored networking and `New-NetFirewallHyperVRule` do not exist there; WSL uses NAT, its IP changes on every restart, and only Windows' localhost is forwarded into the distro.

## Decision
- `setup.ps1 -Networking auto|mirrored|nat`; `auto` picks mirrored on build >= 22621, else nat. Builds < 19041 are refused (no WSL 2).
- In nat mode the Mac reaches the WSL sshd through **Windows' OpenSSH server as an ssh jump host**: `Mac → Windows sshd :22 → localhost:2222 (WSL localhost forwarding) → sshd in the dol distro`. No portproxy, no firewall rule for 2222, nothing to update when the WSL IP changes.
- The jump account is a dedicated standard Windows user (`dol-jump`, random password never shown, key-only). Its key line is `restrict,port-forwarding,permitopen="localhost:2222",permitopen="127.0.0.1:2222"` and a `Match User dol-jump` block sets `AllowTcpForwarding local`, `PermitOpen localhost:2222 127.0.0.1:2222`, `PermitTTY no`, `ForceCommand cmd.exe /c exit 1`. The Windows sshd firewall rule can be limited to the Mac's IP (`-AllowFrom`).
- `dol init --host localhost --jump dol-jump@<lan-ip>` writes a `Host <name>-jump` block and `ProxyJump` + `HostKeyAlias dol-<name>` on the node block.
- kind `lan` access mode is unavailable in nat mode; the default `tunnel` mode works unchanged.

## Consequences
- One extra hop (measured on the tower: 1.06 s first connection, 68 ms with ControlMaster afterwards — the hop is paid once per master).
- Windows' sshd must stay installed on Windows 10 nodes. The jump user can open a single forward to the WSL sshd and nothing else; the dol user inside WSL is still the trust boundary for Docker (root-equivalent, docs/security.md).

## Alternatives
- `netsh interface portproxy` 0.0.0.0:2222 → WSL IP + firewall rule + a boot task to refresh the IP: more moving parts, exposes 2222 on the LAN, breaks silently when the IP changes.
- Upgrading to Windows 11: best, but not our call.
