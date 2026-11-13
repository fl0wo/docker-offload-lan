# Security model

docker-offload-lan moves container workloads from your laptop to another machine on your LAN. This page covers what that exposes and which defaults limit it. Report vulnerabilities as described in [SECURITY.md](../SECURITY.md).

## Trust boundaries

```
 Mac (client)                          Node (WSL2 / Linux)
 ┌──────────────┐   ssh :2222 (keys)   ┌───────────────────────────────────┐
 │ dol mcp      │ ───────────────────► │ sshd ─► user dol (groups dol, docker)
 │ docker CLI   │                      │          ├─ docker system dial-stdio ─► dockerd (root)
 └──────────────┘                      │          └─ dol node dial-stdio ─► node.sock ─► dol node (user dol)
                                       └───────────────────────────────────┘
```

- **The only network entry point is sshd on TCP 2222.** Neither the Docker API nor dol's node API listens on TCP, now or ever ([ADR-0002](adr/0002-all-traffic-over-ssh.md)). Do not "fix" connectivity by enabling `tcp://…:2375`: an unauthenticated Docker API on a LAN gives root to anyone on that LAN.
- **Authentication is your SSH key.** Being able to log in as `dol` is the authorization for everything else.

## The `docker` group is root on the node

User `dol` is in the `docker` group. Anyone who can use the Docker socket can start a privileged container that mounts `/`, so **access as `dol` is equivalent to root on the node**:

- in **WSL2**: root inside the WSL VM, which has `/mnt/c` (your Windows C: drive) mounted and Windows interop enabled. In practice this reaches the Windows user's files and can launch Windows programs as that user;
- on a **Linux** node: root on that machine.

Treat the `dol` key like a root key. Rootless Docker would reduce this risk and is on the v2 list.

The node agent itself (`dol-node.service`) runs as the unprivileged `dol` user with systemd hardening: no capabilities, read-only system, private /tmp, and only `/srv/dol` and its own state and runtime directories writable. It still talks to the Docker socket, which is root-equivalent.

## SSH configuration (installed by `install.sh`)

All of the following applies to connections on port 2222 (`Match LocalPort 2222`, [ADR-0030](adr/0030-sshd-on-2222-match-localport.md)):

- `AllowUsers dol`: only the `dol` account can log in on this port;
- `AuthenticationMethods publickey`, `PasswordAuthentication no`, `KbdInteractiveAuthentication no`: keys only. The `dol` account has no password;
- `PermitRootLogin no`, `X11Forwarding no`, `AllowAgentForwarding no`;
- `AllowTcpForwarding local`: `ssh -L` tunnels (used by `dol_forward` and kind access) work, remote forwarding does not;
- `MaxSessions 64`, because docker and dol multiplex many sessions over one ControlMaster connection.

Keys:

- Use a dedicated or passphrase-protected key, loaded into your agent or the macOS keychain. dol always runs ssh with `BatchMode=yes`, so it never prompts.
- `~dol/.ssh/authorized_keys` contains exactly the keys you passed with `--pubkey`. Review it from time to time.
- `dol init` accepts the node's host key on first contact (`StrictHostKeyChecking=accept-new`) and refuses changed keys afterwards. If a host key changes unexpectedly, investigate before you run `ssh-keygen -R`.
- On the Mac, ControlMaster sockets live in `~/.ssh/cm-%C` (mode 0600, inside your 0700 `~/.ssh`).

## Network exposure

- **Windows firewall.** `setup.ps1` opens only inbound TCP 2222 to the WSL VM (Hyper-V firewall rule `dol-ssh-2222`), only on **Private** and **Domain** networks by default. On a network marked Public the port stays closed.
- **Published container ports reach the whole LAN.** `docker run -p 8080:80` on the node binds `0.0.0.0:8080`. With WSL mirrored networking, that is the Windows machine's LAN address, so anyone on your network can reach the service. Docker's port publishing also bypasses ufw/firewalld rules on Linux. The agent rules therefore tell agents to publish as `-p 127.0.0.1:8080:80` and to reach the service through `dol_forward`, an ssh tunnel.
- **kind API servers** bind `127.0.0.1` on the node and are reached through ssh tunnels (`access_mode: tunnel`, the default). `lan` mode binds `0.0.0.0` and adds the node's LAN IP to the certificate; use it only on trusted networks. Kubernetes authentication still applies, but the API server is then reachable from the LAN.

## Labels are ownership, not security

dol tags every resource it creates with `io.github.fl0wo.dol.*` labels and only stops or deletes resources that carry them ([ADR-0006](adr/0006-ownership-via-labels.md)). This protects your other containers from cleanup bugs. **It is not an access control.** Anyone with Docker access on the node, including any agent holding a lease, can see and change every container, labelled or not. Unlabeled resources are never deleted by default; they are only reported (`reap_unlabeled_after_s: 0`).

## Data on the node

- `/srv/dol/<lease>/…` holds synced workspaces (copies of your project files, minus `.gitignore`d paths and `.git`). They are deleted when the lease is released or reaped. Secrets in your working tree (such as `.env` files) are copied unless they are gitignored.
- `/var/lib/docker-offload-lan` holds the lease database and has no secrets. kubeconfigs of kind clusters are created on the node and handed to the client. On the Mac, they are stored under `~/.local/state/docker-offload-lan/` with mode 0600.
- dol never logs keys, kubeconfigs or tokens and sends no telemetry.

## Checklist

- [ ] The node is on a network you trust, marked **Private** in Windows.
- [ ] Only your keys are in `~dol/.ssh/authorized_keys`.
- [ ] `dol doctor` is green; no TCP Docker API is enabled anywhere (`ss -ltnp | grep -E '2375|2376'` on the node prints nothing).
- [ ] Containers that don't need to be reachable from the LAN publish on `127.0.0.1`.
- [ ] You accept that `dol` is root-equivalent on the node (and, under WSL, can reach your Windows user's files).
