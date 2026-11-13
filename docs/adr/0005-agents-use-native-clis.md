# ADR-0005: Agents use native CLIs

- Status: accepted
- Date: 2026-11-13

## Context
Wrapping the Docker API as dozens of MCP tools is large, always incomplete, and worse than the CLIs agents already know.

## Decision
`dol` places, labels, plumbs and cleans up. Agents run `docker`, `docker compose` and `kubectl` themselves using the exact command prefixes returned by `dol_acquire` (`docker --context dol-<node>`, `kubectl --kubeconfig <path>`). Helper tools exist only where plumbing is non-trivial: compose rewriting, kind, tunnels, sync. Prefixes are explicit per command because environment variables do not persist between agent bash calls.

## Consequences
- Labeling discipline depends on agents following rules (`rules/agent.md`); the reaper still cleans what carries labels, M6 evaluated a label-injecting proxy to remove the discipline requirement; it is available as an opt-in (ADR-0040).

## Alternatives
- Full Docker API as MCP tools: rejected (scope, fidelity).
