# ADR-0030: sshd for dol listens on 2222, scoped with `Match LocalPort`

- Status: accepted
- Date: 2026-11-13

## Context
In WSL mirrored networking Windows and Linux share one port space, so Windows OpenSSH on 22 blocks a Linux sshd on 22 (docs/compat.md §3). On plain Linux nodes, sshd on 22 is often how the owner administers the machine. The brief asks for `AllowUsers dol`, key-only auth and a raised `MaxSessions`, which would lock other users out if applied globally. Ubuntu ≥ 22.10 also uses socket activation (`ssh.socket`), whose listen ports come from a generator rather than from `sshd_config`.

## Decision
- `install.sh` writes `/etc/ssh/sshd_config.d/10-docker-offload-lan.conf` with `Port 2222` (plus `Port 22` in `--sshd-mode alongside`) and a `Match LocalPort 2222` block holding `AllowUsers dol`, `AuthenticationMethods publickey`, `PasswordAuthentication no`, `KbdInteractiveAuthentication no`, `PermitRootLogin no`, `MaxSessions 64`, `AllowTcpForwarding local`, `X11Forwarding no` and `AllowAgentForwarding no`. It ends with `Match all`.
- `--sshd-mode auto` resolves to `only-2222` on WSL and in containers, and to `alongside` on plain Linux. `alongside` is refused on WSL.
- If `ssh.socket` is enabled, the installer switches to `ssh.service`, so `Port` is honoured without generator drop-ins. `sshd -t` runs before every restart.
- The `dol` user's password field is `*`: no password, but not "locked" the way `!` is, so pubkey login works under every PAM setup.

## Consequences
- On plain Linux, port 22 keeps working for other users. Only `dol` with a key can log in on 2222.
- `AllowTcpForwarding local` is what `dol_forward` (`ssh -L`) needs. Remote forwarding stays off.
- The integration test checks the effective config with `sshd -T -C lport=2222`.

## Alternatives
- Global `AllowUsers dol`: rejected because it locks admins out of port 22.
- Keep socket activation and write `ssh.socket.d` overrides: rejected because it depends on the Ubuntu release and is more fragile.
