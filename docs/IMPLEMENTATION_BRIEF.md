# docker-offload-lan — Implementation Brief for Coding Agents

> Read this whole document before writing any code. It is the source of truth for scope, architecture, contracts and milestones. Commit it to the repo as `docs/IMPLEMENTATION_BRIEF.md` and keep it updated when decisions change (via ADRs, see §14).

---

## 0. How to work with this brief

You are one of several coding agents implementing an open-source project from scratch. The owner is a software engineer who will review your work but may not be watching in real time.

Working rules:

1. **Contracts first.** Shared wire types live in `pkg/api` (versioned `v1`). Do not change a public contract (HTTP API, MCP tool schema, config schema, label names) without writing an ADR in `docs/adr/` and updating this brief.
2. **When something is ambiguous**, pick the simplest option consistent with the goals in §2, record it as an ADR, and keep going. Stop and ask only for decisions that are irreversible or touch the owner's real machines destructively.
3. **Verify external facts against current documentation, not memory.** Library module paths, SDK APIs, CLI flags, WSL config keys and tool versions change. §16 lists the ones that must be verified before relying on them.
4. **Never touch the owner's real Docker hosts destructively.** Tests must not hit a real node unless `DOL_E2E=1` is set. Code may only delete resources it can prove it owns (see §7.6).
5. **Small, reviewable PRs.** Conventional Commits (`feat:`, `fix:`, `docs:` …). Every PR: tests, updated docs, green CI.
6. **Error messages are product.** Every error a human or agent sees must say what happened, why, and what to do next.

---

## 1. The problem

The owner works on a **MacBook Pro with 24 GB RAM** and uses Claude Code agents heavily. Those agents routinely spin up Docker workloads for end-to-end testing: **kind/Kubernetes clusters running many microservices**, plus compose stacks and one-off containers. This regularly drives the laptop to very high memory pressure, slowing everything down (including the agents themselves).

On the same LAN sits a **Windows 11 tower** that is mostly idle for this purpose:

- AMD Ryzen 5 3600X (6 cores / 12 threads), **48 GB RAM**, 8 GB GPU
- **238 GB disk** (tight — disk management is a first-class concern)

The goal: **let agents on the Mac transparently run their containers on the tower** over the LAN, using SSH, so the Mac's RAM stays free — while keeping the experience as close as possible to running Docker locally (start, stop, delete, logs, exec, ports, kind clusters).

If the tower is unreachable, out of capacity, or refuses for any reason, **work must fall back to the Mac's local Docker automatically** — the agent should never be blocked; it should simply be told where its workload landed and why.

### Why existing tools don't solve it

Research was done before starting (Nov 2026):

| Project | What it does | Why it's not enough |
|---|---|---|
| Docker Offload (Docker Inc.) | Moves the Docker engine to Docker's cloud; GA April 2026; Docker Business add-on | Cloud-only, paid, not your own hardware, small hosts |
| Knuckles-Team/container-manager-mcp | MCP that routes Docker/Podman/Swarm/k8s operations to many hosts over SSH, with health checks and a doctor tool | A remote control, not a scheduler: no capacity-aware admission, no fallback, no lease cleanup. Python |
| thebadking/dockforward | Docker CLI replacement that runs on a remote host and forwards ports back | No MCP, no fallback, no capacity logic, dormant since Feb 2025 |
| Swartdraak/Docker-MCP | Docker MCP with remote host support | Single host, no placement |
| dagger/container-use | MCP giving each agent an isolated container + git branch | Solves isolation, not offloading RAM to another machine |

**Nobody combines:** node-side admission control + automatic local fallback + lease/heartbeat cleanup of agent-created resources + kind-aware networking, on the user's own LAN hardware. That combination is this project.

### What to borrow from prior art

- **container-use**: packaging and agent integration — stdio MCP added with one `claude mcp add` command, Homebrew tap + install script, an agent rules file users append to `CLAUDE.md`, Apache-2.0, Go.
- **container-manager-mcp**: host registry with health checks and lazy reconnect; a doctor command.
- **dockforward**: detecting published ports and handling local port conflicts when forwarding.
- **Docker Offload's public docs**: session lifecycle — activity-based idle detection, ephemeral environments deleted after inactivity, lease concept.
- **docker/cli `connhelper`**: how `ssh://` Docker hosts are dialed (spawn `ssh … docker system dial-stdio`). We copy this pattern for our own node API (§6, ADR-0002).

---

## 2. Goals and non-goals

### v1 goals (must work for the owner's setup)

- G1. One MacBook (client) + one Windows 11 tower (node) on the same LAN.
- G2. A local MCP server on the Mac that Claude Code uses to acquire capacity, with automatic fallback to local Docker.
- G3. Node-side admission control based on real memory and disk; the node can refuse.
- G4. Agents keep using native `docker`, `docker compose`, `kubectl` against the target they were given.
- G5. Stop/remove/list scoped to resources the client owns, same as local.
- G6. Automatic cleanup of abandoned resources (leases + heartbeats + reaper).
- G7. kind clusters on the node, usable from the Mac (kubeconfig + tunnels).
- G8. Ports of remote services reachable as `localhost:<port>` on the Mac via SSH tunnels.
- G9. Reproducible, idempotent setup scripts for the Windows tower (WSL2) and the Mac.
- G10. A `doctor` command that diagnoses every link in the chain.

### v2 goals (design for them now, don't build them yet)

- Multiple MacBooks sharing one or more nodes (multi-tenant leases; owner-scoped operations).
- Node can be Linux or macOS (e.g. Mac Studio running OrbStack/Colima/Docker Desktop).
- Multiple nodes with ranking; mDNS discovery; Wake-on-LAN; optional mTLS TCP API; dashboard; GPU-aware scheduling.

### Non-goals

- Not a general cluster scheduler (no Swarm/Nomad/k8s replacement).
- No cloud/internet use; LAN only. No NAT traversal.
- No Docker Desktop requirement on the node.
- No live container migration between hosts.
- No exposing the Docker API over TCP. Ever.

---

## 3. Naming, licensing, trademark hygiene

- Repository: `docker-offload-lan`. License: **Apache-2.0**.
- Binary / CLI name: **`dol`**. Do **not** name any binary `docker-*` (that is the Docker CLI plugin naming convention and Docker Inc. ships an official `docker offload` plugin; we must never be mistaken for it or picked up as a plugin).
- README must state near the top, in our own words: this is an independent community project, not affiliated with, endorsed by, or sponsored by Docker, Inc., and unrelated to the official Docker Offload product; Docker is a trademark of Docker, Inc. Check Docker's published trademark guidelines for their preferred notice wording and follow it.
- Do not use the Docker logo or whale imagery. Refer to Docker only descriptively ("works with Docker Engine").
- Docker label prefix: a single constant `LabelPrefix`, reverse-DNS based on the repo owner's GitHub namespace, e.g. `io.github.<owner>.dol`. Below written as `<P>`.

---

## 4. Environment facts

### Client (v1): MacBook Pro

- macOS, Apple Silicon (**arm64**), 24 GB RAM.
- Local Docker via Docker Desktop, OrbStack or Colima (context name varies: `desktop-linux`, `orbstack`, `colima`). Auto-detect; allow override.
- Claude Code launches our MCP server as a stdio subprocess, typically with the project directory as cwd.
- **Important:** in Claude Code, environment variables do not persist between separate bash tool calls. Every instruction we give agents must work per-command (`docker --context X …`, `kubectl --kubeconfig Y …`), not rely on `export`.

### Node (v1): Windows 11 tower

- **amd64**, 48 GB RAM, 238 GB disk.
- Docker runs as **native Docker Engine inside a WSL2 Ubuntu 24.04 distro** (not Docker Desktop). Rationale: lighter, no licensing questions, and the node is "just Linux" so the same install script serves v2 Linux nodes.
- WSL2 config: memory limit ~40 GB, mirrored networking (WSL shares the host LAN IP), sparse VHD, gradual memory reclaim.
- sshd runs **inside WSL on port 2222** (avoids clashing with Windows' own OpenSSH on 22 in mirrored mode). Only that port is opened on the Hyper-V firewall.
- A Windows scheduled task keeps the WSL distro alive at boot.
- Dedicated Linux user `dol` in the `docker` group, key-only SSH.

### Architecture mismatch

Client is arm64, node is amd64. Rule: **build where you run.** Never build on the Mac and transfer images to the node. `docker build` against a remote context ships the build context and builds natively on the node, which is correct. `doctor` and `acquire` must surface the arch difference.

---

## 5. Architecture overview

```
 MacBook (client)                                   Windows tower (node)
 ┌──────────────────────────────┐                  ┌──────────────────────────────────────┐
 │ Claude Code                  │                  │ WSL2 Ubuntu                           │
 │   │ stdio (MCP)              │                  │                                      │
 │   ▼                          │   SSH :2222      │  sshd ──► `dol node dial-stdio` ──┐  │
 │ dol mcp ─ placement ─────────┼─────────────────►│                                   ▼  │
 │   │  leases, heartbeats      │  (node API)      │  dol node (unix socket API)          │
 │   │  tunnels, kubeconfigs    │                  │    admission · leases · reaper       │
 │   │                          │   SSH :2222      │    capacity provider (wsl2)          │
 │   ├─ docker ctx dol-tower ───┼─────────────────►│  sshd ──► docker system dial-stdio   │
 │   │                          │  (Docker API)    │           ▼                          │
 │   └─ local docker (fallback) │                  │  Docker Engine · kind clusters       │
 └──────────────────────────────┘                  └──────────────────────────────────────┘
```

### Components (one Go binary, `dol`)

| Subcommand | Runs on | Role |
|---|---|---|
| `dol mcp` | client | stdio MCP server: placement, fallback, leases, heartbeats, tunnels, kubeconfigs, local-lease bookkeeping |
| `dol node serve` | node | node agent daemon (systemd): capacity, admission control, lease store, reaper, kind operations |
| `dol node dial-stdio` | node | bridges stdin/stdout to the node agent's unix socket (invoked over SSH by clients) |
| `dol init` | client | interactive setup: add node, SSH config snippet, docker context, run doctor, print/run `claude mcp add` |
| `dol node init` | node | writes node config, creates data dirs, validates Docker/kind |
| `dol doctor` | both | end-to-end diagnostics with actionable fixes |
| `dol status` / `dol leases` / `dol release <id>` | client | human CLI equivalents of MCP tools |

### Request flow (happy path)

1. Agent calls MCP tool `dol_acquire` with workload type and memory estimate.
2. `dol mcp` ranks configured nodes, health-checks them (short timeout), and requests a lease from the best one over SSH.
3. Node agent atomically checks capacity and reserves it, returning a lease (201) or refusing with a reason (409).
4. On success, `dol mcp` ensures the docker context `dol-<node>` exists, starts heartbeating, and returns to the agent: placement, lease id, exact command prefixes, labels to apply, and next steps.
5. Agent works with native CLI using the returned context/flags, or uses MCP helper tools (compose, kind, forward, sync) which inject labels and handle plumbing.
6. Agent calls `dol_release` when done. If it never does (session crashes, user quits), heartbeats stop and the node reaper tears everything down after a grace period.

### Fallback flow

Any of: no nodes configured, health check timeout, SSH failure, 409 refusal, 5xx → `dol mcp` creates a **local lease** (bookkeeping only, in client state), returns the local context, and the tool result clearly states `placement: "local"` with the reason. Policy is configurable (§7.3).

---

## 6. Key design decisions (write these as ADR-0001…0008 in M0)

- **ADR-0001 MCP runs on the client, not the node.** If the MCP lived on the node, a dead node would remove the tool and make fallback impossible.
- **ADR-0002 All client↔node traffic goes over SSH.** Docker API via `ssh://` docker contexts; our node API via `ssh <node> dol node dial-stdio` bridging to a unix socket (same pattern as `docker system dial-stdio`). Benefits: one trust boundary (SSH keys), no extra open ports, no tokens on the wire, reuses the user's `~/.ssh/config` (ControlMaster makes it fast). An optional TCP+mTLS listener is a v2 item. Implementation: shell out to the system `ssh` binary (respects user config, agents, ProxyJump), don't embed an SSH client.
- **ADR-0003 Admission control is node-side.** The node is the only source of truth for its own capacity. This makes v2 multi-client safe without a central scheduler. Lease creation is serialized on the node (check+reserve is atomic).
- **ADR-0004 Leases + heartbeats + reaper.** Every workload belongs to a lease; leases expire when heartbeats stop; the reaper removes everything owned by expired leases.
- **ADR-0005 Agents use native CLIs.** We don't re-wrap the Docker API as dozens of MCP tools. We place, label, plumb and clean up; agents run `docker`/`kubectl` themselves with the context we return. Helper tools exist only where plumbing is non-trivial (compose rewriting, kind, tunnels, sync).
- **ADR-0006 Ownership via labels.** Every resource we create or that agents create per our rules carries `<P>.lease`, `<P>.owner`, `<P>.session`, `<P>.project`. Destructive operations are always label-scoped.
- **ADR-0007 kind runs on the node.** The node agent invokes the node's own `kind` binary (versions controlled by the node install script) and returns a kubeconfig. Default access mode is SSH-tunneled (API server not exposed on the LAN).
- **ADR-0008 Go, single static binary, no CGO.**

---

## 7. Detailed specifications

### 7.1 Configuration

Client: `~/.config/docker-offload-lan/config.yaml` (respect `XDG_CONFIG_HOME`).

```yaml
version: 1
identity:
  owner: ""                 # default: $USER@$(hostname -s)
nodes:
  - name: tower
    ssh_host: tower         # an alias from ~/.ssh/config
    docker_context: dol-tower   # created/managed by dol
    priority: 100
    wake_on_lan:            # optional, v1.1
      mac: ""
      broadcast: 192.168.1.255
local:
  docker_context: auto      # auto-detect desktop-linux / orbstack / colima / default
  max_memory_gb: 8          # advisory: warn if a local fallback exceeds this
policy:
  prefer: remote            # remote | local
  fallback: always          # always | if_capacity | never
  health_timeout_ms: 2000
  lease_request_timeout_ms: 5000
  heartbeat_interval_s: 30
forwarding:
  auto_forward: false       # M4 stretch: auto-tunnel published ports of leased containers
sync:
  remote_root: /srv/dol     # node-side workspace root
  respect_gitignore: true
```

Node: `/etc/docker-offload-lan/node.yaml`.

```yaml
version: 1
name: tower
socket: /run/docker-offload-lan/node.sock   # group-owned by `dol`
data_dir: /var/lib/docker-offload-lan
capacity:
  provider: auto            # auto | linux | wsl2 | macos-vm (v2)
  reserve_memory_gb: 4      # never schedulable
  min_free_disk_gb: 30      # refuse new leases below this
  windows_memory_check: true  # wsl2 only: also consider Windows host free RAM
leases:
  heartbeat_grace_s: 600    # expire after this long without heartbeat
  max_ttl_s: 86400          # hard cap for unpinned leases
  max_active: 32
reaper:
  interval_s: 60
  reap_unlabeled_after_s: 0 # 0 = never delete unlabeled resources; only report them
kind:
  binary: /usr/local/bin/kind
  port_range: [16443, 16999]
  access_mode: tunnel       # tunnel | lan
```

Validate configs strictly (unknown keys are errors with a helpful message). Ship JSON Schema files in `schemas/`.

### 7.2 Node API (over the unix socket, HTTP/1.1, JSON)

Clients reach it through `ssh <host> dol node dial-stdio`. The Go client uses a custom `DialContext` that spawns that process and treats its stdio as the connection. Authorization = being able to SSH as a user in the `dol` group. The server records the SSH user (from `SSH_CONNECTION`/`USER` passed by dial-stdio) as `ssh_user` on the lease for audit.

All error responses: `{"error": {"code": "...", "message": "...", "details": {...}, "hint": "..."}}`.

| Method & path | Purpose | Responses |
|---|---|---|
| `GET /v1/healthz` | liveness + version | 200 `{status, version, api_version, node, os, arch, provider}` |
| `GET /v1/capacity` | current capacity | 200, see below |
| `POST /v1/leases` | request capacity | 201 lease · 409 `insufficient_memory` / `insufficient_disk` / `max_leases` / `draining` · 400 |
| `GET /v1/leases?owner=&state=` | list | 200 |
| `GET /v1/leases/{id}` | get | 200 · 404 |
| `POST /v1/leases/{id}/heartbeat` | keep alive | 200 `{expires_at}` · 404/410 (lease gone: client must mark it lost) |
| `PATCH /v1/leases/{id}` | resize estimate, pin/unpin | 200 · 409 (resize re-runs admission) |
| `DELETE /v1/leases/{id}` | release + teardown | 200 `{removed: {...}}` |
| `GET /v1/leases/{id}/resources` | inventory of owned resources | 200 |
| `POST /v1/leases/{id}/kind` | create kind cluster | 201 `{cluster, kubeconfig, api_server: {host, port}, access_mode}` |
| `POST /v1/leases/{id}/kind/{name}/load` | `kind load docker-image` on node | 200 |
| `DELETE /v1/leases/{id}/kind/{name}` | delete cluster | 200 |
| `POST /v1/admin/drain` | stop accepting new leases | 200 |

Capacity response:

```json
{
  "node": "tower", "arch": "amd64", "provider": "wsl2",
  "memory": {
    "total_bytes": 0, "available_bytes": 0, "reserved_bytes": 0,
    "committed_bytes": 0, "schedulable_free_bytes": 0,
    "host_free_bytes": 0
  },
  "disk": { "path": "/var/lib/docker", "free_bytes": 0, "host_free_bytes": 0, "min_free_bytes": 0 },
  "cpu": { "logical_cores": 12, "load1": 0.0 },
  "leases_active": 0,
  "accepting": true,
  "reason": ""
}
```

Lease request:

```json
{
  "owner": "alice@macbook", "session": "s-7h2k9q", "project": "payments-api",
  "workload": "kind",                 // container | compose | kind | build
  "est_memory_bytes": 10737418240,
  "est_disk_bytes": 0,
  "ttl_s": 0,                         // 0 = node default
  "pinned": false,
  "client_version": "0.1.0"
}
```

Lease object: `id` (format `l-` + 10 lowercase base32 chars), all request fields, `state` (`active` | `releasing` | `expired` | `released` | `failed`), `created_at`, `last_heartbeat_at`, `expires_at`, `ssh_user`, `labels` (map the client must apply), `resources` (tracked kind clusters, compose projects, sync dirs).

Persistence: SQLite via a pure-Go driver (no CGO) in `data_dir`. Survive restarts: on boot, the reaper reconciles stored leases against actual Docker resources.

### 7.3 Admission algorithm (node)

Under a global mutex:

```
schedulable      = mem.total - reserve_memory
committed        = sum(est_memory of active leases)
observed_used    = mem.total - mem.available
effective_used   = max(committed, observed_used)
free_for_new     = schedulable - effective_used
if wsl2 && windows_memory_check:
    free_for_new = min(free_for_new, windows_free - 2 GiB)    // Windows itself is under pressure too
accept iff  request.est_memory <= free_for_new
        and disk.free - request.est_disk >= min_free_disk
        and (wsl2: host C: free >= min_free_disk)              // see disk note below
        and active_leases < max_active
        and !draining
```

Refusals must include numbers in `details` and a human `message`, e.g. `insufficient_memory: 6.1 GiB schedulable, 10 GiB requested`.

Disk note (WSL2): inside WSL, `df /` reports the virtual disk's maximum size, not the real free space on the Windows drive. The real limit is the Windows C: drive, visible as `df /mnt/c`. The wsl2 provider must check both.

Memory note (WSL2): `/proc/meminfo` shows the VM's view. Windows-side free memory can be read via interop (`powershell.exe` CIM query). It is slow to start, so cache for ~10 s and degrade gracefully if interop is disabled.

### 7.4 Capacity providers

```go
type Provider interface {
    Name() string
    Snapshot(ctx context.Context) (Capacity, error)   // memory, disk, cpu, host-level extras
}
```

- `linux`: `/proc/meminfo` (MemTotal, MemAvailable), `statfs` on Docker's data-root (from `docker info`), `/proc/loadavg`.
- `wsl2`: `linux` + `/mnt/c` disk + optional Windows free memory. Detect WSL via `/proc/sys/fs/binfmt_misc/WSLInterop` or `microsoft` in `/proc/version`.
- `macos-vm` (v2 stub): capacity is the Docker VM's limit (`docker info` MemTotal), not system RAM.
- `fake`: deterministic, for tests.

### 7.5 Placement and fallback (client)

```
candidates = nodes sorted by priority desc
for node in candidates (stop at first success):
    health = GET /v1/healthz with health_timeout_ms   (cache result 15s)
    if fail: record reason; continue
    lease = POST /v1/leases with lease_request_timeout_ms
    if 201: return remote placement
    record refusal reason; continue
// no remote placement
switch policy.fallback:
    never        -> error listing every node's reason
    if_capacity  -> check local docker info MemTotal and current usage; error if insufficient
    always       -> local placement; warn if est_memory > local.max_memory_gb
create local lease in client state; return local placement with reasons
```

`prefer: local` inverts the order (try local capacity first, then nodes). Wake-on-LAN (v1.1): if a node with `wake_on_lan` fails health, send a magic packet and retry health for up to 60 s before moving on — only if the agent passed `allow_wake: true` or config says so.

### 7.6 Leases, heartbeats, ownership, reaper

- Client generates a `session` id per `dol mcp` process. It heartbeats all of its active remote leases every `heartbeat_interval_s`. A 404/410 on heartbeat marks the lease `lost` and future tool calls on it return a clear error.
- Client state lives in `~/.local/state/docker-offload-lan/` (respect `XDG_STATE_HOME`): leases (remote and local), forwards, generated kubeconfigs, rendered compose files. Use atomic writes and a file lock (several `dol mcp` processes run concurrently, one per Claude Code session).
- On `dol mcp` start: clean up **local** leases whose owning session process is dead (store PID + process start time), tearing down their labeled local resources.
- Labels on every resource:

```
<P>.lease=<lease id>
<P>.owner=<owner>
<P>.session=<session>
<P>.project=<project>
<P>.managed=true
```

- Node reaper (every `interval_s`):
  1. Active, unpinned leases with `now > last_heartbeat + heartbeat_grace` or `now > created + max_ttl` → `expired` → teardown.
  2. Pinned leases past an explicit `expires_at` → teardown.
  3. Resources labeled `<P>.managed=true` whose lease is unknown/released/expired → teardown (orphans).
  4. Unlabeled resources: never deleted by default; reported in `GET /v1/capacity` details and `dol doctor`. Deleting them requires `reap_unlabeled_after_s > 0` and is logged loudly.
  5. If disk free < `min_free_disk`: prune build cache and dangling images only. Never prune volumes belonging to active leases.
- Teardown order per lease: kind clusters → compose projects (`down --volumes` for lease-labeled volumes) → containers (stop, 10 s timeout, then remove) → networks → volumes → sync dir → mark `released`. Idempotent; partial failures recorded and retried next reaper pass.

### 7.7 Docker contexts and command prefixes

- `dol mcp` ensures `docker context create dol-<node> --docker host=ssh://<ssh_host>` exists (idempotent; never modifies contexts it didn't create; never changes the user's *current* context).
- Tool results always include ready-to-paste prefixes, because env vars don't persist across agent bash calls:

```json
"commands": {
  "docker":  "docker --context dol-tower",
  "compose": "docker --context dol-tower compose",
  "kubectl": "kubectl --kubeconfig ~/.local/state/docker-offload-lan/kube/l-abc-e2e.yaml",
  "label_flags": "--label <P>.lease=l-abc --label <P>.owner=alice@macbook --label <P>.managed=true ..."
}
```

- For local placement the same shape is returned with the local context.

### 7.8 Ports and tunnels

- `dol_forward` opens `ssh -N -o ExitOnForwardFailure=yes -L <local_port>:<remote_host>:<remote_port> <ssh_host>` as a managed child process. If `local_port` is taken, pick a free one and report it. Track by lease; close on release or when `dol mcp` exits.
- Remote host defaults to `127.0.0.1` (works for containers published on the node, since mirrored WSL networking and Docker's port publishing bind on the node).
- Security note in docs: `-p 8080:80` on the node binds `0.0.0.0` and is reachable from the LAN. Rules file recommends `-p 127.0.0.1:8080:80` and tunnels.
- M4 stretch `auto_forward`: subscribe to Docker events on the leased context; for containers carrying our lease label with published ports, open matching local forwards automatically (dockforward-style conflict handling).

### 7.9 kind

`POST /v1/leases/{id}/kind` on the node:

1. Cluster name: `dol-<leaseid>-<name>` (kind name length limits apply; validate).
2. Allocate an API server port from `port_range` (persist allocation).
3. Generate kind config by merging the user-supplied config (optional) with:
   - `networking.apiServerAddress: "127.0.0.1"` and `apiServerPort: <allocated>` in **tunnel** mode;
   - in **lan** mode `apiServerAddress: "0.0.0.0"` **plus** a `kubeadmConfigPatches` entry adding the node's LAN IP and hostname to `apiServer.certSANs` (otherwise TLS verification fails when connecting via the LAN IP).
   - Labels: kind node containers can't take arbitrary labels easily, so the lease tracks the cluster name explicitly in `resources`.
4. Run `kind create cluster --name … --config … --kubeconfig <tmp> --wait 180s`.
5. Return the kubeconfig. The client rewrites `server:`:
   - tunnel mode: opens a forward `127.0.0.1:<port>` → node `127.0.0.1:<port>`, keeps `https://127.0.0.1:<port>`;
   - lan mode: `https://<node-lan-ip>:<port>`.
   Saved under client state; path returned in `commands.kubectl`.
6. `extraPortMappings` from the user config are honored; mapped host ports are offered as forwards in the result.
7. Image loading: `dol_kind_load` → node runs `kind load docker-image` (images must already exist on the node, i.e. built there).

Local fallback: `dol mcp` runs the local `kind` with the same naming and labels-in-state, no tunnels.

### 7.10 Workspace sync and compose

Bind mounts reference Mac paths that don't exist on the node. v1 approach:

- `dol_sync(lease, path=cwd)` → `rsync -az --delete` over SSH to `<remote_root>/<lease>/<project>/`, honoring `.gitignore` (filter rule) and always excluding `.git` unless asked. Returns the remote path. Note: recent macOS versions ship `openrsync` instead of GNU rsync; verify flag compatibility (§16) and detect a Homebrew rsync if needed.
- `dol_compose_up(lease, files[], project_name?, sync=true, detach=true, extra_args[])`:
  1. Render with `docker compose -f … config --format json` locally (normalizes paths).
  2. If remote: sync the project, rewrite every bind-mount source under the project dir from the local prefix to the remote prefix; reject (with a clear message) bind mounts outside the project dir unless `allow_external_mounts`.
  3. Inject our labels into every service, network and named volume.
  4. Write the rendered file to client state; run `docker --context <ctx> compose -p <proj> -f <rendered> up` (`-d` default).
  5. Register the compose project on the lease.
- `docker build` needs no sync (build context is streamed).

### 7.11 Security

- Never expose the Docker API or node API on TCP in v1.
- `dol` user: key-only SSH, `docker` group (document clearly that this is root-equivalent on the node/VM). Optional rootless Docker is v2.
- Node socket mode `0660`, group `dol`.
- No secrets in logs. No telemetry.
- Setup scripts open only TCP 2222 inbound to the WSL VM.

---

## 8. MCP server specification (`dol mcp`)

Use the **official MCP Go SDK** (verify module path and current API, §16). Transport: stdio. Provide server `instructions` summarizing the rules in §9 in 5–8 lines.

Every tool returns structured JSON content **and** a 2–4 line human summary. Every result that can affect where things run includes `placement` (`remote` | `local`), `node`, `reason`, `lease_id`, `commands`, `warnings[]`, `next_steps[]`.

| Tool | Input | Behavior |
|---|---|---|
| `dol_status` | `{}` | Nodes with health, arch, capacity summary; this session's leases; local Docker status |
| `dol_acquire` | `{workload: container\|compose\|kind\|build, est_memory_gb: number, est_disk_gb?: number, project?: string, prefer?: remote\|local, pinned?: bool, ttl_minutes?: int, allow_wake?: bool}` | Placement per §7.5; returns lease + commands + label flags |
| `dol_release` | `{lease_id, keep_sync_dir?: bool}` | Full teardown (remote or local), close tunnels, delete kubeconfigs; return removed inventory |
| `dol_list` | `{lease_id?, all_sessions?: bool}` | Resources owned by this owner (label-scoped), local and remote |
| `dol_stop` | `{lease_id, target: container name/id \| compose project}` | Stop, refusing anything not carrying our labels |
| `dol_remove` | `{lease_id, target, volumes?: bool}` | Remove, same ownership check |
| `dol_compose_up` | §7.10 | Render/rewrite/label/sync, then up |
| `dol_compose_down` | `{lease_id, project_name, volumes?: bool}` | Down |
| `dol_sync` | `{lease_id, path?: string}` | Sync workspace, return remote path |
| `dol_kind_create` | `{lease_id, name, config_yaml?: string, access_mode?: tunnel\|lan}` | §7.9; returns kubectl prefix and forwards |
| `dol_kind_load` | `{lease_id, cluster, images[]}` | Load images on the node |
| `dol_kind_delete` | `{lease_id, cluster}` | Delete cluster |
| `dol_forward` | `{lease_id, remote_port, local_port?: int, remote_host?: string}` | Open tunnel; return actual local port |
| `dol_unforward` | `{lease_id, local_port}` | Close tunnel |
| `dol_resize` | `{lease_id, est_memory_gb}` | PATCH lease; may be refused |
| `dol_pin` | `{lease_id, pinned: bool, ttl_minutes?: int}` | Keep a lease alive beyond the session |

Tool descriptions are prompts for LLMs: write them to say *when* to use the tool, what to pass, and what comes back. Keep each under ~80 words. Example for `dol_acquire`: "Call before starting containers, compose stacks, kind clusters or heavy builds. Estimate memory generously (kind cluster with N services ≈ 2 GB + 0.5 GB per service). Returns where the work was placed (remote tower or local fallback) and the exact command prefixes to use for every docker/kubectl command. Always use those prefixes."

Memory estimate guidance (also in rules file): single small container 0.5 GB; compose stack 1–4 GB; kind control plane ~2 GB; each microservice 0.3–1 GB; JVM services ≥1 GB each. Agents should `dol_resize` if they grow.

---

## 9. Agent rules file (`rules/agent.md`, appended to users' CLAUDE.md)

Draft (refine during M7, keep it short):

```markdown
## Containers: use docker-offload-lan

- Before starting any container, compose stack, kind cluster or heavy build, call `dol_acquire`
  with a realistic `est_memory_gb`. Use exactly the command prefixes it returns for every
  docker/compose/kubectl command, in every command (env vars do not persist between commands).
- Add the returned `label_flags` to every `docker run`, `docker network create` and
  `docker volume create`. Prefer `dol_compose_up` over raw `docker compose up`.
- For kind, use `dol_kind_create` and `dol_kind_load`. Build images with the returned docker
  prefix so they are built on the machine that runs them. Never `docker save | docker load`
  images across machines.
- Bind mounts: call `dol_sync` and use the returned remote path, or use `dol_compose_up`.
- Publish ports on 127.0.0.1 and use `dol_forward` to reach them at localhost.
- If the result says `placement: local`, you are on the laptop: keep the footprint small.
- When finished, call `dol_release`. Use `dol_pin` only if the user asks to keep things running.
```

---

## 10. Repository layout

```
docker-offload-lan/
├── cmd/dol/                 # main: cobra (or similar) subcommands
├── pkg/api/v1/              # shared wire types: lease, capacity, errors, labels (PUBLIC CONTRACT)
├── internal/
│   ├── config/              # load/validate client & node config
│   ├── mcpserver/           # tool definitions, results formatting
│   ├── placement/           # ranking, health, fallback policy
│   ├── nodeclient/          # node API client over ssh dial-stdio
│   ├── node/                # HTTP handlers, admission, lease store, reaper
│   ├── capacity/            # providers: linux, wsl2, macosvm (stub), fake
│   ├── dockerx/             # docker contexts, label-scoped ops, events
│   ├── compose/             # render, rewrite, label injection
│   ├── kindx/               # config generation, kubeconfig rewrite
│   ├── tunnel/              # managed ssh -L processes
│   ├── sync/                # rsync wrapper
│   ├── state/               # client state dir, file locking
│   ├── sshx/                # ssh process helpers, dial-stdio
│   └── doctor/              # checks
├── deploy/
│   ├── windows/setup.ps1    # WSL install, .wslconfig, firewall, keep-alive task, power plan
│   ├── linux/install.sh     # docker engine, dol user, sshd:2222, daemon.json, kind, kubectl, systemd unit
│   ├── linux/dol-node.service
│   └── macos/               # v2 node support
├── rules/agent.md
├── schemas/                 # JSON Schema for configs and API
├── docs/
│   ├── IMPLEMENTATION_BRIEF.md
│   ├── adr/
│   ├── setup-windows.md · setup-mac.md · troubleshooting.md · security.md · architecture.md
├── test/e2e/                # scripted golden-path scenarios
├── .github/workflows/       # ci.yml, release.yml
├── .goreleaser.yaml
├── LICENSE (Apache-2.0) · NOTICE · README.md · CONTRIBUTING.md · SECURITY.md
```

## 11. Tech stack

- Go (latest stable), modules, no CGO. `log/slog` (JSON on node, text on CLI).
- Official MCP Go SDK. Docker Go client (verify current module path) — or shell out to `docker` CLI where the CLI is the simpler, more compatible choice (contexts, compose). Prefer the SDK for events/inspect/list, CLI for compose and context management.
- SQLite (pure Go driver) on the node.
- kind and kubectl invoked as binaries on the node (pinned in `install.sh`).
- `golangci-lint`, `go test -race`, GoReleaser (darwin/arm64, darwin/amd64, linux/amd64, linux/arm64), Homebrew tap.

---

## 12. Setup scripts (M1)

### `deploy/windows/setup.ps1` (admin, idempotent, `-WhatIf` support)

1. Check Windows 11 + virtualization; `wsl --install -d Ubuntu-24.04` if missing; ensure WSL is updated.
2. Merge (don't overwrite) `%USERPROFILE%\.wslconfig`: `memory`, `processors`, `swap`, `networkingMode=mirrored`, `sparseVhd=true`, gradual `autoMemoryReclaim`. Back up the original. Sizes are parameters with defaults (40 GB / 12 / 8 GB).
3. Add a Hyper-V firewall rule allowing inbound TCP 2222 to the WSL VM only.
4. Register a scheduled task at startup that keeps the distro running.
5. Optional `-NoSleep`: set AC sleep to never.
6. Print the LAN IP and next step (run `install.sh` inside WSL).

### `deploy/linux/install.sh` (Ubuntu/Debian, idempotent; used in WSL and on v2 Linux nodes)

1. Ensure systemd (in WSL: `/etc/wsl.conf` `[boot] systemd=true`, then instruct `wsl --shutdown`).
2. Docker Engine from Docker's official apt repo; `daemon.json` with BuildKit GC (keep ~40 GB) and `local` log driver with size caps (merge if exists).
3. User `dol` in `docker` group; `~dol/.ssh/authorized_keys` from a provided public key.
4. sshd on 2222: drop-in config, key-only, `MaxSessions` raised (Docker-over-SSH opens many sessions), `AllowUsers dol`.
5. Install pinned `kind`, `kubectl`, `rsync`.
6. Install `dol`, write `/etc/docker-offload-lan/node.yaml`, enable `dol-node.service`.
7. Run `dol doctor --node`.

### Mac: `dol init`

Add node → append a `Host` block to `~/.ssh/config.d/docker-offload-lan` (and an `Include` line if missing) with `Port 2222`, `ControlMaster auto`, `ControlPersist 10m`, `ServerAliveInterval 15` → create docker context → run doctor → print (and optionally run) `claude mcp add docker-offload-lan -s user -- dol mcp`. Suggest lowering the local Docker VM's memory once remote works.

### `dol doctor` checks

SSH reachable (BatchMode, timing), ControlMaster active, remote user in `docker` group, docker context works (`docker --context … version`), node agent reachable via dial-stdio, versions compatible, node arch vs client arch, capacity and disk (including `/mnt/c` on WSL), unlabeled resources count, kind present and version, rsync present on both ends, firewall/port 2222, clock skew (affects lease expiry), local Docker context detected. Each failure prints a specific fix.

---

## 13. Milestones and acceptance criteria

Work in this order. Each milestone ends with a demo note in the PR.

**M0 — Bootstrap (single agent, do first).** Repo skeleton, LICENSE/NOTICE, README with the unofficial disclaimer, CI (lint, test on ubuntu + macos), GoReleaser skeleton, ADR template + ADR-0001…0008, `pkg/api/v1` types, config structs + JSON Schemas, label constants. *Accept:* `go build ./...` and CI green; API types reviewed.

**M1 — Setup and doctor.** `setup.ps1`, `install.sh`, systemd unit, `dol init`, `dol doctor`. *Accept:* fresh WSL distro → node reachable from Mac; `dol doctor` all green; scripts idempotent (second run changes nothing).

**M2 — Node agent.** dial-stdio, unix-socket HTTP server, providers (linux, wsl2, fake), SQLite lease store, admission (§7.3), heartbeats, reaper (§7.6), teardown, drain. *Accept:* unit tests for admission math with the fake provider (including concurrent requests never overcommitting); integration test against a Linux test node; killing heartbeats leads to teardown within `grace + interval`.

**M3 — MCP server (core).** `dol_status`, `dol_acquire`, `dol_release`, `dol_list`, `dol_stop`, `dol_remove`, `dol_resize`, `dol_pin`; placement + fallback; client state with locking; local leases and dead-session cleanup. *Accept:* with node up → remote placement; node stopped → local placement with reason; node full (fake provider) → 409 → local fallback; ownership checks prevent touching unlabeled resources.

**M4 — Tunnels and kind.** `dol_forward`/`dol_unforward`, `dol_kind_create`/`load`/`delete`, kubeconfig rewrite, tunnel and lan modes (certSANs patch). Stretch: `auto_forward`. *Accept:* golden path step 3–6 in §15 passes.

**M5 — Sync and compose.** `dol_sync`, `dol_compose_up`/`down` with path rewriting and label injection. *Accept:* a compose project with relative bind mounts runs remotely unchanged from the agent's perspective.

**M6 — Spike: label-injecting Docker API proxy (decide via ADR).** Idea: `dol mcp` exposes a per-lease unix socket on the Mac that proxies the Docker API to the node over SSH and transparently (a) injects lease labels on create calls (containers, networks, volumes), (b) rewrites bind-mount paths, (c) notes published ports for auto-forwarding. Agents would then use plain `DOCKER_HOST=unix://…` with zero discipline required. Must handle HTTP upgrade/hijacked streams (attach, exec, logs follow, build). Deliverable: prototype + ADR accept/reject with measured overhead. Do not block v1 on it.

**M7 — Packaging and docs.** Releases, Homebrew tap, install script, `rules/agent.md` final, setup/troubleshooting/security docs, architecture diagram, demo GIF. *Accept:* a new user follows README from zero to golden path in under 30 minutes.

**v1.1:** Wake-on-LAN, `auto_forward` if not done, `dol` TUI/status command polish.

**v2 backlog:** multi-client (already mostly enabled by owner-scoped labels and node-side admission — add per-owner quotas and fair-share), macOS node provider, Linux node docs, multiple nodes ranking by free capacity, mDNS discovery, optional TCP+mTLS API, GPU-aware leases, web dashboard, rootless Docker.

### Parallelization plan

- Phase A (one agent): M0. Everyone else waits for `pkg/api/v1` to merge.
- Phase B (parallel): Agent 1 → M2 (node). Agent 2 → M3 (MCP) against an in-process fake node implementing `pkg/api/v1`. Agent 3 → M1 (scripts + doctor).
- Phase C (parallel after B): M4 and M5.
- Phase D: M6 spike, M7.
- One agent owns integration: runs the golden path on real hardware (`DOL_E2E=1`) after each phase.

---

## 14. Engineering conventions

- ADRs: `docs/adr/NNNN-title.md` (Context, Decision, Consequences, Alternatives).
- Errors: typed with stable `code`s mirrored in `pkg/api/v1`; wrap with context; never panic on bad input.
- Timeouts on every network/process call. Context cancellation everywhere.
- Child processes (ssh tunnels, kind) are tracked and killed on exit; no zombies.
- All file writes in state dirs are atomic; concurrent `dol mcp` processes are safe.
- Logs never contain kubeconfigs, keys or tokens.
- Cross-platform shell-outs: never assume GNU flags on macOS.
- Tests: table-driven unit tests; integration tests use a containerized Linux "fake node" (sshd + Docker-in-Docker + `dol node serve`) so CI can run the whole client↔node path without real hardware.

---

## 15. Golden-path end-to-end scenario (definition of done for v1)

Run on the real MacBook + tower with `DOL_E2E=1`:

1. `dol doctor` → all green.
2. In Claude Code, agent calls `dol_status` → tower healthy, capacity shown.
3. `dol_acquire {workload: kind, est_memory_gb: 12, project: "demo"}` → `placement: remote`.
4. `dol_kind_create {name: e2e}` → kubeconfig path; `kubectl --kubeconfig … get nodes` works from the Mac.
5. Build two sample services with the returned docker prefix, `dol_kind_load` them, deploy, `kubectl port-forward` or `dol_forward` → `curl localhost:<port>` returns 200 on the Mac.
6. Mac memory pressure stays low throughout (record `memory_pressure` before/after).
7. `dol_release` → `GET /v1/leases/{id}/resources` empty; `docker --context dol-tower ps -a --filter label=<P>.managed=true` empty; tunnels closed.
8. Stop `dol-node.service` → `dol_acquire` returns `placement: local` with a clear reason within `health_timeout_ms` + small overhead.
9. Restart it with the fake provider reporting low memory (or request 100 GB) → 409 → local fallback with numbers in the reason.
10. Acquire remotely, then kill the Claude Code session → after `heartbeat_grace + reaper interval`, everything for that lease is gone.

---

## 16. Facts to verify before relying on them

Check current official docs / release notes for each and record versions in an ADR or `docs/compat.md`:

- Official MCP Go SDK: module path, server/tool registration API, structured tool output support.
- Docker Go client module path (it has moved between `github.com/docker/docker/client` and `github.com/moby/moby/client` variants) and API version negotiation.
- `docker system dial-stdio` behavior over `ssh://` contexts; `MaxSessions` needs.
- WSL: `.wslconfig` keys (`networkingMode`, `sparseVhd`, `autoMemoryReclaim` — whether still under `[experimental]`), idle shutdown behavior (`vmIdleTimeout` or equivalent), Hyper-V firewall cmdlets and the WSL VM creator ID, systemd support.
- kind: current version, `networking.apiServerAddress`/`apiServerPort`, `kubeadmConfigPatches` for certSANs, cluster name length limit, `kind load docker-image` behavior.
- macOS rsync vs openrsync flag compatibility (`--filter`, `--delete`).
- Claude Code: `claude mcp add` syntax and scopes; whether MCP servers receive the project cwd.
- Docker trademark guidelines: preferred notice wording for the README.

---

## 17. Things you must not do

- Expose any API on TCP in v1, or suggest `tcp://…:2375` anywhere in docs.
- Delete or stop resources without our labels (except via an explicit, opt-in, logged config).
- Change the user's current Docker context or kubeconfig default.
- Name binaries `docker-*`, use Docker logos, or imply affiliation with Docker, Inc.
- Build on the Mac and ship images to the node.
- Require Docker Desktop on the node.
- Let a failing node block the agent: every failure path ends in either a local placement or an actionable error.