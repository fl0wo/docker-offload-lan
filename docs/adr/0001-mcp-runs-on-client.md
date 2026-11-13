# ADR-0001: The MCP server runs on the client, not the node

- Status: accepted
- Date: 2026-11-13

## Context
Agents (Claude Code) need tools to acquire capacity. The tools could be served by the node (where the work runs) or by a process on the client.

## Decision
`dol mcp` runs on the client as a stdio subprocess of Claude Code. It talks to nodes over SSH.

## Consequences
- A dead, sleeping or unreachable node cannot remove the tools; the MCP server can always fall back to local Docker.
- Client-side state (leases, tunnels, kubeconfigs) lives on the client and must be safe across concurrent `dol mcp` processes (file locking).
- Heartbeats are sent by the client, so the lease lifetime follows the agent session lifetime.

## Alternatives
- MCP on the node over SSH/stdio: one less moving part on the client, but fallback is impossible when the node is down, which violates goal G2.
