# ADR-0003: Admission control is node-side

- Status: accepted
- Date: 2026-11-13

## Context
Several `dol mcp` processes (one per Claude Code session) and, in v2, several machines may request capacity on one node concurrently.

## Decision
The node is the only source of truth for its own capacity. `POST /v1/leases` performs check-and-reserve under a single process-wide mutex, using the algorithm in brief §7.3. Committed memory is the sum of estimates of active leases; observed usage is taken from the provider. The larger of the two counts.

## Consequences
- No central scheduler; v2 multi-client is safe by construction.
- Concurrency correctness is testable in isolation with the fake provider (no overcommit under N concurrent requests).
- Clients may still race for the last slot; losers get 409 and fall back.

## Alternatives
- Client-side scheduling from capacity snapshots: racy across clients; rejected.
