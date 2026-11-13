# Compatibility reference (external facts)

Verified 2026-11-13 against current upstream docs and source, per brief §16. Re-check these when bumping pins.
"src:" means the fact was read from the upstream source tree at the stated tag.

## 1. kind

| Fact | Value | Source |
|---|---|---|
| Latest release | **v0.33.0** (published 2026-08-26) | https://api.github.com/repos/kubernetes-sigs/kind/releases/latest ; https://proxy.golang.org/sigs.k8s.io/kind/@latest |
| Default node image in v0.33.0 | `kindest/node:v1.37.0@sha256:a1ed56cf…` | src: `pkg/apis/config/defaults/image.go` @ v0.33.0 |
| Download URL pattern | `https://kind.sigs.k8s.io/dl/<ver>/kind-<os>-<arch>` (redirects to `https://github.com/kubernetes-sigs/kind/releases/download/<ver>/kind-<os>-<arch>`). linux/amd64: `…/v0.33.0/kind-linux-amd64`; darwin/arm64: `…/v0.33.0/kind-darwin-arm64`. Each has a `.sha256sum` sibling on GitHub releases | src: `site/content/docs/user/quick-start.md`; GitHub release assets (HTTP 200 checked) |
| `networking.apiServerAddress` / `apiServerPort` | Both exist in v1alpha4 `Networking`. Address defaults to `127.0.0.1`; port defaults to random (`-1` = let the container runtime pick). Docs warn strongly against non-127.0.0.1 | src: `pkg/apis/config/v1alpha4/types.go` L170-182; https://kind.sigs.k8s.io/docs/user/configuration/#api-server |
| kubeadm API used for generated config | **v1beta4 for k8s ≥ v1.36.0** (so default v1.37 images), v1beta3 for v1.23–v1.35, v1beta2 below | src: `pkg/cluster/internal/kubeadm/config.go` L673-679 |
| Generated `certSANs` | `[localhost, "<apiServerAddress>"]` | src: `pkg/cluster/internal/kubeadm/config.go` L500 |
| Patch merging of `certSANs` | kind **appends** patch `certSANs` to the generated list (custom merge, not a JSON merge-patch list replace). A patch that omits `apiVersion` matches any kubeadm version | src: `pkg/internal/patch/kubeadm_merge.go` (`walkAndMerge`/`mergeCertSANs`), `pkg/internal/patch/kubeyaml.go` L33-36 |
| Cluster name rule | regex **`^[a-z0-9.-]+$`** (hard error). Length **> 50 chars only triggers a warning** ("probably too long"), not an error (`clusterNameMax = 50`) | src: `pkg/internal/apis/config/validate.go` L33; `pkg/cluster/internal/create/create.go` L49, L86-88 |
| `kind load docker-image` | The image **must be present in the Docker daemon kind talks to** (error `image: "<x>" not present locally`). Then kind runs `docker save` to a tar and imports it into each node, or re-tags it if the ID is already on the node. Flags: `--name/-n`, `--nodes` | src: `pkg/cmd/kind/load/docker-image/docker-image.go` L66-98, L181-212 |
| `kind create cluster` flags | `--name/-n`, `--config`, `--image`, `--retain`, `--wait <duration>` (default 0s; e.g. `180s`), `--kubeconfig <path>` | src: `pkg/cmd/kind/create/cluster/createcluster.go` L57-93 |
| Kubeconfig `server:` written by kind | `https://<HostIp>:<HostPort>` read from the API server container's port mapping. With `apiServerAddress: 0.0.0.0` it becomes `https://0.0.0.0:<port>`, so the client has to rewrite it | src: `pkg/cluster/internal/providers/docker/provider.go` `GetAPIServerEndpoint` |

LAN-mode patch. Leaving out `apiVersion` makes the patch work against both v1beta3 and v1beta4:

```yaml
kind: Cluster
apiVersion: kind.x-k8s.io/v1alpha4
networking:
  apiServerAddress: "0.0.0.0"
  apiServerPort: 30443
kubeadmConfigPatches:
- |
  kind: ClusterConfiguration
  apiServer:
    certSANs:
    - "192.168.1.50"
    - "tower"
    - "tower.local"
```

## 2. kubectl

| Fact | Value | Source |
|---|---|---|
| Latest stable | **v1.37.2** | https://dl.k8s.io/release/stable.txt |
| Download URL | `https://dl.k8s.io/release/<ver>/bin/<os>/<arch>/kubectl` (+ `.sha256`). linux/amd64 and darwin/arm64 both return HTTP 200 for v1.37.2 | https://kubernetes.io/docs/tasks/tools/install-kubectl-linux/ |

## 3. WSL

| Fact | Value | Source |
|---|---|---|
| `networkingMode=mirrored` | **`[wsl2]`**. Values: `none`, `nat` (default), `bridged` (deprecated), `mirrored`, `consomme`. Requires Win 11 22H2+ | https://learn.microsoft.com/en-us/windows/wsl/wsl-config (ms.date 2026-10-16) |
| `sparseVhd` | **Still `[experimental]`**, bool, default false. Applies only to *newly created* VHDs. For an existing distro run `wsl --manage <distro> --set-sparse true` (`wsl --manage` needs WSL 2.5+) | same; `--set-sparse` from src: https://github.com/microsoft/WSL/blob/master/localization/strings/en-US/Resources.resw ; https://learn.microsoft.com/en-us/windows/wsl/disk-space |
| `autoMemoryReclaim` | **Still `[experimental]`**. Values `disabled`, `gradual`, `dropCache`. **Default is now `dropCache`**; an unknown value also means dropCache | same |
| `memory`, `processors`, `swap` | `[wsl2]`. Sizes use `GB`/`MB` suffixes | same |
| `vmIdleTimeout` | `[wsl2]`, ms, default `60000`: "The number of milliseconds that a VM is idle, before it is shut down." (Win 11 only) | same |
| `instanceIdleTimeout` | **`[general]`**, ms, default `15000`: "The number of milliseconds that a distro is idle, before it is shut down. **Set to -1 to disable auto shutdown**" | same |
| `firewall` | `[wsl2]`, default true: Windows Firewall and Hyper-V firewall rules filter WSL traffic | same |
| `ignoredPorts`, `hostAddressLoopback` | `[experimental]`, mirrored mode only | same |
| systemd | `/etc/wsl.conf` → `[boot]` `systemd=true`, then `wsl.exe --shutdown`. `[boot] initTimeout` default 10000 ms | same |
| Mirrored mode LAN inbound | Listed benefit: "Connect to WSL directly from your local area network (LAN)". Inbound is gated by the Hyper-V firewall, so a rule is needed | https://learn.microsoft.com/en-us/windows/wsl/networking |
| WSL VM creator ID | **`{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}`** | same |
| Hyper-V firewall rule (MS example) | `New-NetFirewallHyperVRule -Name "MyWebServer" -DisplayName "My Web Server" -Direction Inbound -VMCreatorId '{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}' -Protocol TCP -LocalPorts 80` | same |
| `New-NetFirewallHyperVRule` params | `-DisplayName` (required), `-Name` (unique ID, use it for idempotency), `-Direction Inbound\|Outbound` (default Inbound), `-Action Allow\|Block` (default Allow), `-VMCreatorId '{GUID}'`, `-Protocol`, `-LocalPorts`, `-RemotePorts`, `-LocalAddresses`, `-RemoteAddresses`, `-Profiles Any\|Domain\|Private\|Public`, `-Enabled True\|False` (strings), `-RulePriority`, `-WhatIf`. Related cmdlets: `Get-`/`Set-`/`Remove-NetFirewallHyperVRule`, `Get-NetFirewallHyperVVMCreator` | https://learn.microsoft.com/en-us/powershell/module/netsecurity/new-netfirewallhypervrule |
| Port sharing in mirrored mode | Windows and Linux share one port space. By default Linux **cannot bind a port Windows already uses**; `ignoredPorts` lets Linux bind it, but only for traffic inside Linux. So Windows OpenSSH on 22 blocks Linux sshd on 22, and **2222 is fine** as long as nothing on Windows holds it | wsl-config `ignoredPorts` row; https://github.com/microsoft/WSL/issues/10632 |

## 4. Docker over SSH (`ssh://` contexts)

| Fact | Value | Source |
|---|---|---|
| What the CLI runs remotely | `ssh -T -o ConnectTimeout=30 … <host> docker system dial-stdio` (or `docker --host=unix://<path> system dial-stdio` if the URL has a socket path). **The `docker` CLI must be installed on the remote** ("requires Docker 18.09 or later on the remote host"). It uses the system `ssh` binary, so `~/.ssh/config` (ControlMaster, Port) applies | src: https://github.com/docker/cli/blob/master/cli/connhelper/connhelper.go L28, L47-66, L86-99 |
| Docker's SSH guidance | Recommends `ControlMaster auto`, `ControlPath ~/.ssh/control-%C`, `ControlPersist yes`. **No mention of MaxSessions** | https://docs.docker.com/engine/security/protect-access/ |
| sshd `MaxSessions` | Default **10** sessions per multiplexed connection. Each concurrent docker call (and each `dial-stdio`) over a ControlMaster uses one session, so raising it (e.g. 64) is our own recommendation, not a Docker one. `MaxStartups` default `10:30:100` | https://man.openbsd.org/sshd_config |
| Current Docker Engine | docker-v29.9.3 (2026-11-03) | https://api.github.com/repos/moby/moby/releases/latest |

## 5. macOS rsync (openrsync)

| Fact | Value | Source |
|---|---|---|
| macOS 15.6 `/usr/bin/rsync` | `openrsync: protocol version 29` / `rsync version 2.6.9 compatible` | local `rsync --version` on this Mac |
| Flag support | Man page documents `-a`, `-z/--compress`, `--delete` (+ `-before/-during/-delay/-after`, `--delete-excluded`), `--exclude`, `-f/--filter` with `merge`/`dir-merge` rules, `-e/--rsh` ("will generally handle quotes, but makes no attempt to deal with escape sequences") | `man openrsync` (macOS 15.6); https://keith.github.io/xcode-man-pages/openrsync.1.gz.html |
| Empirical test | `/usr/bin/rsync -az --delete --filter=':- .gitignore' --exclude=.git ./ dst/` worked: honoured `.gitignore` patterns including `*.log` in subdirs and `build/`, excluded `.git`, deleted stale files, rc=0. `-e 'ssh -p 2222 -o BatchMode=yes'` was parsed and ssh invoked | local test, 2026-11-13 |
| Remote side (Ubuntu 24.04) | rsync 3.2.7 (protocol 31, negotiates down to 29) | https://launchpad.net/ubuntu/noble/+source/rsync |
| Known incompatibilities | Openrsync accepts only a subset of GNU options (e.g. no `--checksum-choice`, `--info=progress2`, and `--log-file` reported missing). There are reports of assertion failures and `--backup-dir` problems. A protocol-29 `--checksum` false-mismatch bug shows up against remote rsync 3.2.x. None of these affect `-az --delete --filter --exclude -e` | https://mjtsai.com/blog/2025/06/06/sequoias-new-rsync/ ; https://zenn.dev/ultimatile/articles/rsync-protocol-29-chechsum-bug |
| Homebrew rsync | formula `rsync` 3.5.2 at `/opt/homebrew/bin/rsync` (Apple Silicon) | https://formulae.brew.sh/api/formula/rsync.json |

## 6. Claude Code MCP

| Fact | Value | Source |
|---|---|---|
| Syntax | `claude mcp add [options] <name> -- <command> [args...]`. Options: `-s/--scope local\|user\|project` (default `local`), `-e/--env KEY=value`, `-t/--transport stdio\|sse\|http`. Canonical form for us: **`claude mcp add -s user docker-offload-lan -- dol mcp`** | https://code.claude.com/docs/en/mcp ; `claude mcp add --help` (Claude Code 2.1.323) |
| Scopes | local → `~/.claude.json` (this project only); project → `.mcp.json`; user → `~/.claude.json` (all projects) | same |
| cwd of stdio servers | **Not documented.** Instead, Claude Code sets **`CLAUDE_PROJECT_DIR`** in the server's env to the project root, and answers MCP `roots/list` with the launch dir plus added dirs | same |

## 7. Docker trademark

| Fact | Value | Source |
|---|---|---|
| Guidelines URL | https://www.docker.com/legal/trademark-guidelines/ | — |
| Notice legend | "Docker and the Docker logo are trademarks or registered trademarks of Docker, Inc. in the United States and/or other countries. Docker, Inc. and other parties may also have trademark rights in other terms used herein." The legend "can be modified to include only our Marks." | same |
| Other rules | No false implication of sponsorship/endorsement/affiliation. Use marks as adjectives. Lowercase "docker" only for CLI commands. Marks must not "appear to be part of any other marks" or be used "as part of your own trademark"; no domain names containing the marks; the whale/logo may not be used or imitated without authorization | same |

## 8. Docker Engine on Ubuntu 24.04, daemon.json

| Fact | Value | Source |
|---|---|---|
| Supported | Ubuntu 26.10, 26.04 LTS, **24.04 LTS (noble)**, 22.04 LTS | https://docs.docker.com/engine/install/ubuntu/ |
| Remove first | `docker.io docker-compose docker-compose-v2 docker-doc podman-docker containerd runc` (`docker-buildx` is also listed) | same |
| Keyring | `/etc/apt/keyrings/docker.asc` from `https://download.docker.com/linux/ubuntu/gpg` | same |
| Repo file | **deb822** `/etc/apt/sources.list.d/docker.sources` (not `docker.list`) | same |
| Packages | `docker-ce docker-ce-cli containerd.io docker-buildx-plugin docker-compose-plugin` | same |
| Firewall | Published ports bypass ufw/firewalld. Docker is only compatible with iptables-nft / iptables-legacy | same |
| `builder.gc` keys (Engine ≥ 28.0) | `enabled`, `defaultReservedSpace`, `defaultMaxUsedSpace`, `defaultMinFreeSpace`, `policy[]` (`reservedSpace`, `maxUsedSpace`, `minFreeSpace`, `keepDuration`, `filter`, `all`). **`defaultKeepStorage` / `keepStorage` are deprecated aliases for `defaultReservedSpace` / `reservedSpace`** | src: moby `daemon/config/builder.go` @ docker-v29.9.3 (absent in v27.5.1, present in v28.0.0); https://docs.docker.com/reference/cli/dockerd/ |
| Semantics | reservedSpace = never prune below this; maxUsedSpace = prune anything above this; minFreeSpace = target free disk (never below reserved). Prune target = `max(min(maxUsed, total−freeDeficit), reserved)` | https://github.com/moby/buildkit/blob/master/docs/buildkitd.toml.md ; src: buildkit `cache/manager.go` `calculateKeepBytes` |
| Defaults when none set | reserved 10% of disk, maxUsed 80%, minFree 20% | src: moby `daemon/internal/builder-next/worker/gc.go` |
| `local` log driver | `"log-driver": "local"`, `"log-opts": {"max-size": "10m", "max-file": "3"}`. Defaults: max-size 20m, max-file 5, compress true | https://docs.docker.com/engine/logging/drivers/local/ |

Recommended `daemon.json` fragment (merge into any existing file):

```json
{
  "log-driver": "local",
  "log-opts": { "max-size": "20m", "max-file": "5" },
  "builder": {
    "gc": {
      "enabled": true,
      "defaultReservedSpace": "10GB",
      "defaultMaxUsedSpace": "40GB",
      "defaultMinFreeSpace": "20GB"
    }
  }
}
```

## Implications for implementation

1. **kind names**: validate `^[a-z0-9.-]+$` and keep names ≤ 50 chars ourselves (kind only warns above 50). Lease IDs used in `dol-<leaseid>-<name>` must be lowercase with no underscores. Pin kind v0.33.0 and kubectl v1.37.2 with sha256 checks.
2. **certSANs patch**: use a `ClusterConfiguration` patch **without `apiVersion`** (works on v1beta3 and v1beta4; kind appends SANs). In LAN mode, rewrite the kubeconfig `server:` from `https://0.0.0.0:<port>` to the node's LAN IP.
3. **`kind load docker-image`** reads from the node's Docker daemon, so "build on the node first" is a hard requirement. Surface the `not present locally` error clearly.
4. **.wslconfig**: write `networkingMode/memory/processors/swap` under `[wsl2]`, `sparseVhd` and `autoMemoryReclaim=gradual` under **`[experimental]`**. To keep the node alive, set **`[general] instanceIdleTimeout=-1`** (and optionally a large `[wsl2] vmIdleTimeout`) in addition to, or instead of, the scheduled keep-alive task. `sparseVhd` does not convert an existing distro; use `wsl --manage Ubuntu-24.04 --set-sparse true`.
5. **Firewall**: `New-NetFirewallHyperVRule -Name dol-ssh-2222 -DisplayName "docker-offload-lan SSH" -Direction Inbound -Action Allow -VMCreatorId '{40E0AC32-46A5-438A-A0B2-2B479E8F2E90}' -Protocol TCP -LocalPorts 2222`. Check idempotency with `Get-NetFirewallHyperVRule -Name dol-ssh-2222`. Port 2222 avoids the Windows OpenSSH 22 clash, and `doctor` should check that no Windows process holds 2222.
6. **SSH**: the `docker` CLI must be installed on the node (docker-ce-cli covers this). Raise `MaxSessions` (e.g. 64), noting this is our own rationale. Docker's docs recommend ControlMaster.
7. **rsync**: macOS openrsync handles our exact flag set. Still prefer `/opt/homebrew/bin/rsync` (or `/usr/local/bin/rsync`) when present, since it is more featureful and better tested against GNU 3.x. Never use `--checksum` with openrsync. Pass `-e` as a single argv element from Go (no shell), or rely on the ssh config `Host` block's `Port 2222` and use plain `-e ssh`.
8. **MCP**: document `claude mcp add -s user docker-offload-lan -- dol mcp`. Resolve the project dir from `CLAUDE_PROJECT_DIR`, then MCP roots, then `os.Getwd()`, rather than relying on cwd alone.
9. **Trademark**: use the exact legend above in the README plus our own non-affiliation disclaimer. The guidelines discourage marks used "as part of your own trademark", and "docker-offload-lan" contains both "Docker" and the "Docker Offload" product name. That is a naming risk for the owner to weigh (not legal advice).
10. **daemon.json**: use `defaultMaxUsedSpace` for a "keep about 40 GB" cap, not `defaultKeepStorage`, which is now a deprecated alias for the *reserved* floor. The new keys need Engine 28+, which the apt repo provides. Install via the deb822 `docker.sources` file.
