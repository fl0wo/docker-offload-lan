# ADR-0010: Lease IDs, local leases and client state layout

- Status: accepted
- Date: 2026-11-13

## Decision
- Lease IDs are `l-` + 10 chars of lowercase base32 (`a-z2-7`), generated from crypto/rand, for both remote and local leases. Session IDs are `s-` + 10 chars.
- Client state: `$XDG_STATE_HOME/docker-offload-lan/` with `leases/<id>.json` (one file per lease, atomic rename writes), `kube/<id>-<cluster>.yaml`, `compose/<id>-<project>.yaml`, `sessions/<session>.json` (PID + process start time) and a `state.lock` flock for cross-process operations.
- Client-only lease state `lost` marks leases whose heartbeat got 404/410.

## Consequences
Concurrent `dol mcp` processes share one state dir safely; dead sessions are detected by PID + start time.
