# docker-offload-lan (`dol`)

Run your coding agents' Docker workloads, including compose stacks and kind clusters, on **another machine on your LAN** over SSH. Your laptop's RAM stays free, and if that machine is off or full, work falls back to local Docker automatically.

> **Not affiliated with Docker, Inc.** docker-offload-lan is an independent community project. It is not affiliated with, endorsed by, or sponsored by Docker, Inc., and it is unrelated to Docker's official "Docker Offload" product. Docker and the Docker logo are trademarks or registered trademarks of Docker, Inc. in the United States and/or other countries. Docker, Inc. and other parties may also have trademark rights in other terms used herein. This project works with Docker Engine; it does not ship or modify it.

## What it does

```
 MacBook (client)                         LAN node (Windows 11 + WSL2, or Linux)
 Claude Code ──stdio──► dol mcp ──ssh──►  dol node  (admission · leases · reaper)
                           │      ──ssh──► Docker Engine · kind clusters
                           └─► local Docker (automatic fallback)
```

- **Placement with admission control.** An agent calls `dol_acquire` with a memory estimate. The node reserves the capacity or refuses with numbers, based on real memory and disk. On WSL2 that includes the Windows host's free RAM and C: drive.
- **Automatic fallback.** If the node is unreachable, frozen, full or failing, the work lands on local Docker. The result says `placement: "local"` and why. A frozen node falls back within `health_timeout_ms` (measured: 2.01 s with a 2 s timeout).
- **Native CLIs.** Agents keep running `docker`, `docker compose` and `kubectl`, using the exact prefixes `dol` returns (`docker --context dol-tower …`, `kubectl --kubeconfig …`).
- **Nothing leaks.** Leases, heartbeats and a reaper remove everything an abandoned session left behind. Only resources carrying dol's labels, or tracked on a lease, are ever touched.
- **kind on the node.** Clusters run where the RAM is. `kubectl` on your Mac reaches them through an SSH tunnel; kubeconfigs are written for you.
- **Ports and files.** `dol_forward` makes node services reachable at `localhost:<port>`. `dol_sync` and `dol_compose_up` sync your project so relative bind mounts work unchanged.

Everything runs over SSH. Neither the Docker API nor the node API is ever exposed on TCP.

## MCP tools

| Tool | Use it to |
|---|---|
| `dol_status` | See node health, capacity and this session's leases |
| `dol_acquire` | Reserve capacity before running anything; returns placement and command prefixes |
| `dol_release` | Tear down everything of a lease, close its tunnels and delete its kubeconfigs |
| `dol_list` | List leases and the resources they own |
| `dol_stop` / `dol_remove` | Stop or remove an owned container, network, volume or compose project |
| `dol_resize` / `dol_pin` | Change a lease's memory estimate / keep it alive after the session ends |
| `dol_forward` / `dol_unforward` | Open or close an SSH tunnel to a node port |
| `dol_sync` | Sync a directory to the node for bind mounts |
| `dol_compose_up` / `dol_compose_down` | Run a compose project with synced, rewritten bind mounts and labels |
| `dol_kind_create` / `dol_kind_load` / `dol_kind_delete` | Manage kind clusters on the lease's host |

## Quick start

1. **Node (Windows 11):** run `deploy/windows/setup.ps1` as admin, then `deploy/linux/install.sh --pubkey ~/.ssh/id_ed25519.pub` inside the WSL distro. For a Linux node, run only `install.sh`. See [setup-windows.md](docs/setup-windows.md).
2. **Mac:** install `dol`, then run `dol init` ([setup-mac.md](docs/setup-mac.md)). It adds the SSH host, creates the docker context, runs `dol doctor` and prints:
   ```sh
   claude mcp add -s user docker-offload-lan -- dol mcp
   ```
3. Append [`rules/agent.md`](rules/agent.md) to your `CLAUDE.md`.
4. When something is off, run `dol doctor`. Every failed check prints its fix ([troubleshooting.md](docs/troubleshooting.md)).

## Documentation

- [Architecture](docs/architecture.md): components, lease lifecycle, admission, SSH details
- [Security](docs/security.md): threat model. Note that the node's `docker` group is root-equivalent
- [ADRs](docs/adr): design decisions
- [Compatibility](docs/compat.md): verified versions and external facts
- [Implementation brief](docs/IMPLEMENTATION_BRIEF.md): scope and milestones

## Development

```sh
go test -race ./...                                       # unit + MCP tests
DOL_INTEGRATION=1 go test ./test/integration/...          # containerized node over real ssh
DOL_INTEGRATION=1 DOL_INTEGRATION_KIND=1 go test -run Kind ./test/integration/...
```

See [CONTRIBUTING.md](CONTRIBUTING.md).

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
