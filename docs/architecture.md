# Architecture

This page explains how `dol` works today. The decisions behind it are in [docs/adr](adr), and the original scope is in the [implementation brief](IMPLEMENTATION_BRIEF.md).

## Components

```
 MacBook (client)                                   LAN node (WSL2 Ubuntu or Linux)
 ┌──────────────────────────────┐                  ┌──────────────────────────────────────┐
 │ Claude Code                  │                  │  sshd :2222 (key-only, user dol)      │
 │   │ stdio (MCP)              │   ssh            │    │                                  │
 │   ▼                          │ ───────────────► │    ├─► dol node dial-stdio ─► unix     │
 │ dol mcp                      │  node API        │    │      socket ─► dol node serve     │
 │   placement · leases ·       │                  │    │      (admission · SQLite leases  │
 │   heartbeats · tunnels ·     │   ssh            │    │       · reaper · kind)            │
 │   kubeconfigs · compose      │ ───────────────► │    └─► docker system dial-stdio       │
 │   │                          │  Docker API      │           ─► Docker Engine · kind      │
 │   └─► local Docker (fallback)│   ssh -L         │                                      │
 │                              │ ───────────────► │  127.0.0.1:<port> (services, kind API)│
 └──────────────────────────────┘                  └──────────────────────────────────────┘
```

All traffic goes over the system `ssh` binary, so your `~/.ssh/config`, keys, agent, ProxyJump and ControlMaster all apply. Nothing listens on TCP except sshd (ADR-0002).

| Package | Role |
|---|---|
| `pkg/api/v1` | Public wire contract: lease, capacity, error codes, labels, MCP result types |
| `internal/mcpserver` | `dol mcp` and the CLI behind it. The `Core` type implements every tool |
| `internal/placement` | Ranks nodes, probes health, requests leases and applies the fallback policy |
| `internal/state` | Client state dir: one JSON file per lease, flock, session liveness (PID and start time) |
| `internal/nodeclient` | Node API client. HTTP/1.1 over a `net.Conn` that is an ssh process's stdio |
| `internal/sshx` | Spawns ssh: one-shot commands and stdio connections |
| `internal/tunnel` | Managed `ssh -L` forwards that die with `dol`, even on SIGKILL |
| `internal/sync` | rsync to `<sync_root>/<lease>/<dir>`, honouring `.gitignore` |
| `internal/compose` | Renders compose files, rewrites bind mounts, injects labels, audits ports |
| `internal/node` | Node HTTP handlers, admission, SQLite lease store, reaper, teardown, kind |
| `internal/capacity` | Capacity providers: `linux`, `wsl2` (also reads Windows C: and Windows free RAM), `fake` |
| `internal/kindx` | kind config generation (tunnel/lan, certSANs), cluster naming, port allocation |
| `internal/dockerx` | docker CLI wrapper. Every destructive call re-checks ownership labels first |
| `internal/doctor`, `internal/clientsetup` | `dol doctor` and `dol init` |

## Lease lifecycle

```
            POST /v1/leases (admission under one mutex)
                        │ 201
                        ▼
   heartbeat ──►  ┌──────────┐  DELETE / dol_release    ┌───────────┐  teardown ok  ┌──────────┐
   (every 30 s)   │  active  │ ───────────────────────► │ releasing │ ────────────► │ released │
                  └──────────┘                          └───────────┘               └──────────┘
                        │ no heartbeat for grace (600 s)      │ teardown error
                        │ or created + max_ttl                ▼
                        ▼                               ┌──────────┐  retried every reaper pass
                  ┌──────────┐ ─── teardown ──────────► │  failed  │ ──────────────────────────►
                  │ expired  │                          └──────────┘
                  └──────────┘
   client only: a heartbeat answered 404/410 → "lost"; later tool calls on the lease return lease_lost.
```

Teardown runs in this order: kind clusters, compose projects, containers (stop with a 10 s timeout, then remove), networks, volumes, sync dir. Containers, networks and volumes are found by label `io.github.fl0wo.dol.lease=<id>`. Each object's labels are checked again right before it is deleted. Kind clusters, compose projects and sync dirs are tracked on the lease explicitly. Resources without dol's labels are counted and reported, never deleted (ADR-0006, ADR-0012).

## Placement and fallback

1. Nodes are tried in priority order. Each gets a **fresh** health probe bounded by `health_timeout_ms`. A cached success is never trusted for placement (ADR-0020 amendment).
2. Then comes `POST /v1/leases`, bounded by `lease_request_timeout_ms`. A 409 refusal carries numbers, for example `insufficient_memory: 6.0 GiB schedulable, 500.0 GiB requested (node it: 7.7 GiB total, 0.5 GiB reserved, …)`.
3. If no node places the lease, `fallback` decides: `always` creates a local lease, `if_capacity` checks local Docker's MemTotal first, and `never` returns an error listing every node's reason.

Every tool result carries `placement`, `node`, `reason`, `lease_id`, `commands` (with `--context` prefixes, because environment variables don't persist between agent bash calls), `warnings` and `next_steps`.

## Admission (node)

```
schedulable    = mem.total − reserve_memory
effective_used = max(Σ est_memory(active leases), mem.total − mem.available)
free_for_new   = schedulable − effective_used          (wsl2: also ≤ windows_free − 2 GiB)
accept iff est_memory ≤ free_for_new ∧ disk.free − est_disk ≥ min_free_disk
          ∧ (wsl2: C: free ≥ min_free_disk) ∧ active < max_active ∧ ¬draining
```

Check and reserve happen under one process-wide mutex. Tested with 1,000 concurrent lease requests through the real handler: exactly the analytic maximum was accepted, and committed memory never exceeded schedulable memory.

## SSH details that matter

- **ControlMaster pipes.** The ssh that becomes a ControlMaster forks a background master. That master inherits the pipes of the process that spawned it and keeps them open for `ControlPersist`, which can be minutes. `dol` therefore never waits for EOF on ssh pipes: `StdioConn.Close` closes its read end itself, and every spawn sets `WaitDelay`. Without this, a frozen node delayed fallback by the full `ControlPersist` (measured at 59.6 s in tests, which would be 10 min with the recommended config). With it, fallback took 2.01 s.
- **Tunnels.** Each tunnel uses `-S none` and `ExitOnForwardFailure=yes`. The remote command is `echo dol-tunnel-ready; exec cat >/dev/null`. The marker gives readiness without probing connections, and the blocked `cat` ends the tunnel when `dol` dies.
- **Destination as data.** The ssh destination always comes after `--`, so a host string can never be read as an option.

## How it is verified

| Layer | Judge |
|---|---|
| Unit | `go test -race ./...`: admission math and concurrency, reaper with a fake clock, store restart, label-scoped teardown, kubeconfig rewrite, compose rewrite against the real `docker compose` CLI, rsync semantics with the real (open)rsync |
| MCP | The SDK client drives the real server over in-memory transports, behaving like an agent |
| Integration | `DOL_INTEGRATION=1`: an Ubuntu 24.04 container provisioned by the real `install.sh`, running Docker-in-Docker, sshd on port 2222 and `dol node serve`. The real `dol mcp` binary is driven over stdio and reaches the node over real ssh. It covers golden-path steps 2, 3, 5 and 7–10, plus kind steps 4–5 with `DOL_INTEGRATION_KIND=1` |
| Hardware | `DOL_E2E=1` on the real Mac and tower (brief §15), including step 6 (Mac memory pressure) |
