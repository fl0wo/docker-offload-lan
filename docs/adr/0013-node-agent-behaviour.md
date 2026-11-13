# ADR-0013: Node agent behaviour not spelled out by the v1 contract

- Status: accepted
- Date: 2026-11-13

## Context
Implementing M2 (`dol node serve`) required decisions that brief §7.2–§7.9 and ADR-0011 leave open. None of them changes a type, field, route or error code in `pkg/api/v1`; they define behaviour clients can rely on.

## Decision

**ssh_user.** The agent reads the peer uid of each unix-socket connection (`SO_PEERCRED` on Linux, `LOCAL_PEERCRED` on macOS, via `http.Server.ConnContext`) and maps it to a user name with `os/user` (`uid:<n>` if unknown). The peer is `dol node dial-stdio`, which sshd starts as the SSH user, so this is the SSH user. This replaces the brief's "from `SSH_CONNECTION`/`USER` passed by dial-stdio": the kernel's credentials cannot be spoofed by the client, and environment variables can.

**Admission.**
- Refusal precedence when several limits are hit: `draining` > `max_leases` > `insufficient_memory` > `insufficient_disk` (Docker data-root) > `insufficient_disk` (host drive).
- The capacity snapshot is taken before the admission mutex is acquired (a WSL2 snapshot can take seconds). Committed memory, which every admission changes, is read inside the mutex, together with the check and the insert.
- The host checks (Windows free RAM minus 2 GiB, host drive free space) apply whenever the provider reports the host value. Only `wsl2` does in production; the `fake` provider does when `host_free_*` is configured, so WSL2 refusals can be tested anywhere.
- `PATCH` re-runs memory admission only when the estimate grows, excluding the lease's own current reservation. Shrinking always succeeds. Draining and `max_active` do not block a resize.
- A provider failure is `503 unavailable`, not a refusal, so clients report it as a node problem.

**Expiry.**
- Unpinned: `expires_at = min(last_heartbeat + heartbeat_grace_s, created + min(ttl_s or max_ttl_s, max_ttl_s))`.
- Pinned: `expires_at` is explicit (`now + ttl_s` at creation or at the `PATCH` that sets it) and is not capped by `max_ttl_s`. A pinned lease without a TTL never expires, and its `expires_at` is the zero time (`0001-01-01T00:00:00Z`).
- A successful `PATCH` counts as a heartbeat.
- A heartbeat or mutation on an active lease past its deadline marks it `expired` right away and answers `410 lease_gone`, without waiting for the reaper.

**Release and teardown.**
- `DELETE /v1/leases/{id}` is idempotent: it runs the (idempotent) teardown in any state and returns `200` with whatever it removed. Partial failures are reported in `removed.errors`, the lease becomes `failed` with `last_error`, and the reaper retries it. Teardown keeps running if the client disconnects.
- Teardown order: kind clusters → `docker compose -p <p> down --volumes --remove-orphans --timeout 10` for registered projects → labeled containers, networks and volumes → registered sync dirs (re-validated: under `<sync_root>/<lease>`, no symlink components) → `released`. The empty `<sync_root>/<lease>` directory is removed too.
- One per-lease operation lock serializes kind create/load/delete and teardown. The reaper skips leases whose lock is held and retries on its next pass.

**Reaper.**
- Leases left in `releasing` (the agent crashed during a release), `expired` or `failed` are torn down on every pass, including the first pass at startup (reconciliation).
- Orphans are labeled resources whose lease is unknown or `released`. Expired and failed leases are covered by the full teardown above.
- Kind clusters named `dol-l-<10 base32>-<name>` whose lease is unknown, or released without tracking them, are deleted too. This extends step 3 to kind, whose node containers cannot carry our labels.
- Disk pressure (Docker data-root or host drive below `min_free_disk_gb`) runs only `docker builder prune -f` and `docker image prune -f`. Volumes are never pruned.

**Kind.** `{name}` in `…/kind/{name}` accepts the short name (`e2e`) or the full cluster name. In lan mode the certSANs are `[kind.lan_address or the first private IPv4, hostname]`, and `api_server.host` is the first of them. The kubeconfig is returned as kind wrote it; rewriting `server:` stays the client's job (brief §7.9 step 5). A failed `kind create` is rolled back: `kind delete`, untrack, free the port.

**Response bodies the contract leaves open.** These are informative; clients must not depend on them: `POST /v1/admin/drain` → `{"draining": bool, "leases_active": n}` (an empty body means drain on). `DELETE …/kind/{name}` → `{"deleted": "<cluster>"}`. `POST …/compose/{project}` and `POST …/sync` → the updated lease. The drain flag persists across restarts.

**Transport.** Handlers run detached from client disconnects, with a 2-minute deadline (kind operations: 15 minutes), because net/http cancels a request when the client half-closes after sending, which is what `dial-stdio` does on stdin EOF. `dial-stdio` retries a refused connect for up to 3 s (full listen backlog, or the agent restarting); a missing socket fails immediately with "cannot connect to node agent at <socket>".

## Consequences
- Clients (M3) can treat `410` as "lease lost" both for leases the reaper already handled and for leases that are merely past their deadline.
- Tests cover each point: `internal/node/*_test.go`.

## Alternatives
- Adding typed drain and kind-delete responses to `pkg/api/v1`: deferred until a client needs them, to keep the public contract minimal.
