# Troubleshooting

Start with `dol doctor` on the Mac and, if needed, `sudo dol doctor --node` on the node. Each check has an id, and each section below is named after one, so `✗ docker-context` means: read [docker-context](#docker-context). Every failing check already prints a `fix:` line; this page explains the background. Use `dol doctor --json` to get the full report for a bug report.

Status symbols: ✓ ok · ⚠ warning (works, but degraded) · ✗ failure (remote placement will not work; agents fall back to local Docker) · i info · · skipped (a prerequisite failed or the check does not apply).

---

## Client checks (`dol doctor`)

### config
The client config `~/.config/docker-offload-lan/config.yaml` (or `$XDG_CONFIG_HOME/...`) could not be read or has no nodes.
- *unknown key*: a typo or wrong indentation. The message names the line. Compare with `schemas/client-config.schema.json`.
- *no nodes configured*: run `dol init --name tower --host <ip>`.
- `--node-name x` with an unknown node: list the nodes with `grep name: ~/.config/docker-offload-lan/config.yaml`.

### local-docker
dol could not reach your local Docker, which it uses for **fallback** placements. With `local.docker_context: auto`, dol uses the current context unless it is a `dol-*` context, then tries `orbstack`, `desktop-linux` and `colima`, then `default`. Start Docker Desktop, OrbStack or Colima, or set `local.docker_context` explicitly.

### local-rsync
Workspace sync (`dol_sync`, `dol_compose_up` with bind mounts) needs rsync. macOS `openrsync` works with dol's flags; Homebrew rsync (`brew install rsync`) is preferred when present.

### ssh-config
Output of `ssh -G <alias>`. Expected: port 2222, user `dol`, `ControlMaster auto`.
- *port 22 / user is your Mac user*: ssh does not see the Host block. Check that `~/.ssh/config` starts with `Include config.d/*`. An Include placed after a `Host` line only applies inside that host. Re-run `dol init`.
- *ControlMaster off*: every docker command then pays a full ssh handshake (slow, and it can hit sshd's `MaxStartups`). Re-run `dol init`.

### ssh
`ssh -o BatchMode=yes <alias>` failed. BatchMode means dol never prompts, because agents cannot type passwords.
- **Permission denied (publickey)**: your key is not in `~dol/.ssh/authorized_keys`. On the node: `sudo /path/to/install.sh --pubkey "<contents of ~/.ssh/id_ed25519.pub>"`. If your key has a passphrase, load it into the agent (`ssh-add --apple-use-keychain ~/.ssh/id_ed25519`).
- **Connection refused**: nothing listens on 2222. See [sshd-port](#sshd-port).
- **Connection timed out / No route to host**: the node is off, asleep or not on the LAN, or the firewall blocks it. On Windows: `Get-NetFirewallHyperVRule -Name dol-ssh-2222`, and check that the network profile is Private (`Get-NetConnectionProfile`). Re-run `setup.ps1` to fix both. Is WSL running? `wsl -l -v` should show the distro as Running (see the keep-alive task in [setup-windows.md](setup-windows.md)).
- **Host key verification failed**: on first contact run `ssh tower` once, or re-run `dol init`, which uses `accept-new`. If the key *changed* (for example after reinstalling the distro), verify that is expected, then `ssh-keygen -R "[<ip>]:2222"`.
- **Could not resolve hostname**: the alias is not defined. See [ssh-config](#ssh-config).

### ssh-control-master
`ssh -O check <alias>` found no master connection. It works without one, just more slowly. Make sure the Host block has `ControlMaster auto`, `ControlPath ~/.ssh/cm-%C` and `ControlPersist 10m`. A stale socket can be cleared with `ssh -O exit tower`.

### remote-docker-group
The ssh user on the node is not in group `docker` (needed for the Docker socket) or `dol` (needed for the node agent socket). On the node: `sudo usermod -aG docker,dol dol`. Then drop the old multiplexed connection, which still has the old groups: `ssh -O exit tower`.

### docker-context
`docker context inspect dol-<name>` / `docker --context dol-<name> version` failed.
- *does not exist*: `dol init` creates it.
- *points to … expected ssh://<alias>*: `docker context update dol-tower --docker host=ssh://tower`, or re-run `dol init`.
- *version failed*: Docker over ssh runs `docker system dial-stdio` **on the node**, so the docker CLI must be installed there (install.sh does this) and dockerd must be running (`sudo systemctl status docker`).

### node-agent
`ssh <alias> dol node dial-stdio` → `GET /v1/healthz` failed.
- *dol: command not found*: dol is not installed on the node or not on PATH for non-interactive ssh. Re-run `install.sh --dol-binary …`.
- *connect to node agent / node.sock*: the agent is not running. On the node: `sudo systemctl status dol-node` and `journalctl -u dol-node -n 50`. See also [node-socket](#node-socket).
- *not implemented*: the node runs a dol build without the node agent. Install a release.
- *degraded*: the agent runs but reports problems (for example, Docker is down). The message lists them.

### version
The client and node must speak the same API (`v1`). A different minor version gives a warning: upgrade the older side (`brew upgrade dol`, `install.sh --dol-version vX.Y.Z`).

### arch
Informational. The Mac is arm64 and the tower is amd64: **build where you run**. `docker --context dol-tower build …` builds natively on the node. Never `docker save | docker load` images across machines.

### clock-skew
The node's clock (from `/v1/healthz`) compared with the Mac's. Lease expiry is computed on the node, so a large skew makes leases look expired early or late. WSL clocks drift after Windows sleeps: `sudo hwclock -s` inside WSL, or `wsl --shutdown`. On Linux: `timedatectl set-ntp true`. On macOS: Settings → General → Date & Time → automatic.

### capacity
The node is not accepting leases. The message says why: draining, memory, or the maximum number of leases. Free memory or release leases (`dol leases`, `dol release <id>`). In the meantime, new work falls back to local Docker automatically.

### disk
Free disk on the node is below (✗) or close to (⚠) `min_free_disk_gb`. On WSL, the real limit is **Windows C:** (`host drive` in the message), not the virtual disk. Free space with `docker --context dol-tower system df`, then `docker --context dol-tower builder prune` and `image prune`. Because of `sparseVhd`, the freed space goes back to Windows.

### unlabeled
Informational. These are containers, volumes and networks on the node that do not carry dol's labels (`io.github.fl0wo.dol.managed=true`). dol never deletes them; they are yours. List containers with their dol label using `docker --context dol-tower ps -a --format '{{.Names}} {{.Label "io.github.fl0wo.dol.managed"}}'`. Unlabeled containers show no `true`.

### remote-kind
`kind` or `kubectl` is missing on the node, or not at the pinned version (kind v0.33.0, kubectl v1.37.2). Re-run `install.sh`.

### remote-rsync
rsync is missing on the node: `sudo apt-get install rsync`, or re-run `install.sh`.

---

## Node checks (`sudo dol doctor --node`)

### node-config
`/etc/docker-offload-lan/node.yaml` is missing or invalid. `install.sh` writes a default and never overwrites an existing file. Compare yours with `schemas/node-config.schema.json`.

### docker-daemon
`docker info` failed: `sudo systemctl enable --now docker` and `journalctl -u docker -n 50`. A warning about the log driver means `/etc/docker/daemon.json` lacks `"log-driver": "local"`, so container logs grow without a cap. Re-run `install.sh`, which merges the setting in, then `sudo systemctl restart docker`.

### dol-user
User `dol` must be in groups `dol` and `docker`: `sudo usermod -aG dol,docker dol`.

### systemd-unit
`dol-node.service` is not active or not enabled: `sudo systemctl enable --now dol-node` and `journalctl -u dol-node -n 50`. In WSL without systemd, this check is skipped and install.sh enables systemd for you (then run `wsl --shutdown`).

### node-socket
The agent socket `/run/docker-offload-lan/node.sock` must exist with mode `0660` and group `dol`. It is missing when the agent is not running. A wrong mode or group means the agent was started outside `dol-node.service`, which runs it as `dol:dol` with `RuntimeDirectory=docker-offload-lan`.

### node-agent-local
The node doctor calls the agent's health endpoint directly over the socket. A failure here, with the socket present, points at the agent itself: `journalctl -u dol-node -n 50`.

### sshd-port
Nothing listens on TCP 2222. `sudo sshd -t` shows config errors. Then `sudo systemctl restart ssh`. The installer writes `/etc/ssh/sshd_config.d/10-docker-offload-lan.conf`, and on Ubuntu it switches from `ssh.socket` to `ssh.service` (see [ADR-0030](adr/0030-sshd-on-2222-match-localport.md)). On WSL, a warning appears if sshd also listens on 22. Remove `Port 22` from `/etc/ssh/sshd_config`: in mirrored mode, port 22 belongs to Windows OpenSSH. On the Windows side, `setup.ps1` warns if a Windows program holds 2222.

### kind
### kubectl
Missing or not at the pinned version. Re-run `install.sh`, which downloads the pinned version and verifies its sha256.

### rsync
`sudo apt-get install rsync`.

### sync-root
`/srv/dol` (`sync_root` in node.yaml) must exist and be owned by `dol`: `sudo install -d -m 0750 -o dol -g dol /srv/dol`.

### disk-docker
Free space on Docker's data root. **On WSL this is the virtual disk's maximum size, not real free space.** See [disk-wsl-host](#disk-wsl-host).

### disk-wsl-host
Free space on Windows C: (`/mnt/c`), where the WSL virtual disk grows. Below `min_free_disk_gb`, the node refuses new leases. Free space on C: or prune Docker data.

### wsl-interop
WSL interop lets the node read Windows' free memory (`windows_memory_check`). If it is disabled, the capacity provider uses only the WSL VM's view. Enable it with `[interop] enabled=true` in `/etc/wsl.conf`, then `wsl --shutdown`.

---

## Other problems

- **`docker --context dol-tower …` hangs or is slow:** check [ssh-control-master](#ssh-control-master). Many parallel docker commands can exceed sshd's per-connection session limit; install.sh raises `MaxSessions` to 64.
- **A container port is not reachable from the Mac:** publish it on the node as `-p 127.0.0.1:8080:80` and use `dol_forward`. A plain `-p 8080:80` binds 0.0.0.0 on the node, which makes it reachable from the whole LAN (see [security.md](security.md)).
- **WSL stops when idle:** check `[general] instanceIdleTimeout=-1` in `%USERPROFILE%\.wslconfig` and the scheduled task "docker-offload-lan keep WSL alive" (Task Scheduler → History).
- **`install.sh` stops after enabling systemd:** this is expected. Run `wsl --shutdown` in Windows, then run the script again.

## WSL downloads are much slower than on Windows

Symptom: `curl` inside the WSL distro is many times slower than `curl.exe` on Windows; the first `dol_kind_create` times out pulling `kindest/node`.

On the first real tower (Windows 10, TP-Link USB Wi-Fi) WSL got 68 KB/s while Windows got 2 MB/s. Two fixes, both measured there:

1. Disable Large Send Offload on the WSL adapter (68 KB/s → ~1 MB/s): `Disable-NetAdapterLso -Name "vEthernet (WSL)"` in an admin PowerShell. The adapter is recreated with the WSL VM, so run `setup.ps1 -DisableWslLso` to reapply it at every boot.
2. If name resolution inside WSL times out (`getent hosts example.com` hangs), WSL's NAT DNS proxy is not answering. Use static resolvers in the distro (→ 2.7 MB/s):
   ```sh
   sudo sh -c 'printf "[network]\ngenerateResolvConf = false\n" >> /etc/wsl.conf; rm -f /etc/resolv.conf; printf "nameserver 192.168.1.1\nnameserver 1.1.1.1\n" > /etc/resolv.conf'
   ```
   (use your router's IP). Do **not** disable RSC on `vEthernet (WSL)`: on that tower it cut WSL's DNS off entirely.

## scp to the node fails with "Connection closed"

The dol sshd configuration on port 2222 serves dol's own traffic (ssh exec, port forwards, rsync). Modern `scp` uses the SFTP subsystem; use `ssh <node> 'cat > /path' < file` instead, or `scp -O` (legacy protocol).
