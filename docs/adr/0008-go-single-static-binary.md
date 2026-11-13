# ADR-0008: Go, single static binary, no CGO

- Status: accepted
- Date: 2026-11-13

## Decision
One Go binary `dol` (never `docker-*`) with subcommands for client and node. `CGO_ENABLED=0`. SQLite via the pure-Go `modernc.org/sqlite`. Official MCP Go SDK (`github.com/modelcontextprotocol/go-sdk`).

## Consequences
Easy cross-compilation (darwin/arm64, darwin/amd64, linux/amd64, linux/arm64) and distribution via GoReleaser and Homebrew.
