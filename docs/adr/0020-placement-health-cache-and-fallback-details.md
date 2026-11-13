# ADR-0020: Placement details — health cache, timeouts, local capacity

- Status: accepted
- Date: 2026-11-13

## Context
Brief §7.5 says "cache result 15s" for node health checks and leaves several details open: whether failures are cached, what "local capacity" means for `fallback: if_capacity` and `prefer: local`, and how a node that is up but draining is treated. Agents must never wait much longer than `health_timeout_ms` when a node is down or hung (golden path step 8).

## Decision
- **Health cache: successes only, 15 s per node.** Failures are never cached, so a node that comes back is used on the very next `dol_acquire`. A cached-healthy node whose lease request then fails as unreachable/timeout is evicted. `dol_status` always checks fresh and refreshes the cache.
- **Timeouts:** the health check is bounded by `health_timeout_ms`, the lease request separately by `lease_request_timeout_ms`, both via context deadlines (the nodeclient cancels the request and closes the ssh stdio connection). A hung node therefore costs at most `health_timeout_ms` per node; measured 518 ms with `health_timeout_ms=500` through the full MCP stack.
- **Healthy but `accepting: false`** (draining): recorded as `refused` with code `draining`; no lease request is sent.
- **Attempt outcomes:** `unreachable` (codes `unreachable`, `timeout`), `refused` (409 admission codes), `error` (anything else, e.g. 5xx), `placed`, `skipped` (local, when `prefer: local` and local has no room).
- **Local capacity** (`if_capacity`, `prefer: local`): `docker info` MemTotal of the local context minus the sum of `est_memory` of every *active local lease in client state* (all sessions). If local Docker is unreachable under `if_capacity`, the error code is `local_docker_down`, otherwise `no_placement`.
- **`fallback: always` does not probe local Docker** (keeps the fallback fast; `dol_status` shows local health). It warns when the estimate exceeds `local.max_memory_gb`.
- **`prefer: local`**: try local capacity first; if it does not fit, try nodes; if no node places, apply `fallback` (`never` and `if_capacity` then return an error because local was already found too small).
- **Arch mismatch**: a remote placement on a node whose `healthz.arch` differs from the client's GOARCH adds a "build where you run" warning.

## Consequences
- Repeated acquires against a dead node each pay up to `health_timeout_ms`; acceptable (2 s default) and always correct.
- The local committed-memory figure is advisory: agents can create unlabeled work outside leases.

## Alternatives
- Caching failures for 15 s: faster repeated fallbacks, but a recovered node would be ignored for up to 15 s. Rejected.
- Probing local Docker under `always`: adds 100 ms–1 s to every fallback for little benefit. Rejected.


## Amendment (2026-11-13): placement always probes health

The containerized golden-path test froze the node (`docker pause`) right after a successful acquire. With a cached health success, the fallback cost `lease_request_timeout_ms` (5 s) instead of `health_timeout_ms` (2 s). With ControlMaster a probe costs milliseconds, so `Place` now always probes fresh. The 15 s cache remains for read-only views only. Measured after the change: see `test/integration/golden_test.go` (frozen node → local).
