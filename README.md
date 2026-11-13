# docker-offload-lan (`dol`)

Run your coding agents' Docker workloads, including compose stacks and kind clusters, on **another machine on your LAN** over SSH. Your laptop's RAM stays free, and if that machine is off or full, work falls back to local Docker automatically.

> **Not affiliated with Docker, Inc.** docker-offload-lan is an independent community project. It is not affiliated with, endorsed by, or sponsored by Docker, Inc., and it is unrelated to Docker's official "Docker Offload" product. Docker and the Docker logo are trademarks or registered trademarks of Docker, Inc. in the United States and/or other countries. Docker, Inc. and other parties may also have trademark rights in other terms used herein. This project works with Docker Engine; it does not ship or modify it.

## What it does

```
 MacBook (client)                         LAN node (e.g. Windows 11 + WSL2, or Linux)
 Claude Code ──stdio──► dol mcp ──ssh──►  dol node  (admission · leases · reaper)
                           │      ──ssh──► Docker Engine · kind clusters
                           └─► local Docker (automatic fallback)
```

- **Placement with admission control.** An agent calls `dol_acquire` with a memory estimate. The node reserves the capacity or refuses (HTTP 409 with numbers), based on real memory and disk, including the Windows host when the node runs in WSL2.
- **Automatic fallback.** If the node is unreachable, refuses or errors, the work lands on local Docker, and the result says `placement: "local"` and why.
- **Native CLIs.** Agents keep running `docker`, `docker compose` and `kubectl`, using the exact prefixes `dol` returns (`docker --context dol-tower …`).
- **Nothing leaks.** Leases, heartbeats and a reaper remove everything an abandoned session left behind. Only resources carrying dol's labels are ever touched.
- **kind on the node.** Cluster API servers are reached through SSH tunnels; kubeconfigs are written for you.
- **Ports.** Use `dol_forward` to reach remote services at `localhost:<port>`.

Everything runs over SSH. No Docker API or node API is ever exposed on TCP.

## Status

Under active development; see the [implementation brief](docs/IMPLEMENTATION_BRIEF.md) for scope and milestones and [docs/adr](docs/adr) for design decisions.

## Quick start

1. **Node (Windows 11):** run `deploy/windows/setup.ps1` as admin, then `deploy/linux/install.sh` inside the WSL distro. For a Linux node, run only `install.sh`. See [docs/setup-windows.md](docs/setup-windows.md).
2. **Mac:** install `dol`, then run `dol init` and follow the prompts. It adds the SSH host, creates the docker context, runs `dol doctor` and registers the MCP server:
   ```sh
   claude mcp add -s user docker-offload-lan -- dol mcp
   ```
3. Append [`rules/agent.md`](rules/agent.md) to your `CLAUDE.md`.

## License

Apache-2.0. See [LICENSE](LICENSE) and [NOTICE](NOTICE).
