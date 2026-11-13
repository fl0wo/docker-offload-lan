# ADR-0021: `dol mcp` session lifecycle, exit behavior and lost leases

- Status: accepted
- Date: 2026-11-13

## Context
Brief §5/§7.6 define sessions, heartbeats and dead-session cleanup but leave open what happens on a clean exit, which leases a process heartbeats, and how stale client records are cleaned.

## Decision
- **Start:** generate `v1.NewSessionID()`, write `sessions/<id>.json` (PID + process start time from sysctl `kern.proc.pid` on macOS, `/proc/<pid>/stat` field 22 on Linux), then in the background run dead-session cleanup and start one heartbeat goroutine.
- **Dead session** = no session record, PID gone, or PID alive with a different start time (PID reuse). If the start time cannot be read for a live PID, the session counts as alive (never tear down a live session's work by mistake).
- **Dead-session cleanup:** local leases of dead sessions are torn down (tracked compose projects, then label-scoped containers → networks → volumes) and their records, kubeconfigs and rendered compose files deleted; pinned local leases are kept until their `expires_at`. Remote lease records of dead sessions are only *forgotten* when the node answers 404/410 or a terminal state; the node's reaper owns remote cleanup.
- **Heartbeats:** each process heartbeats only its own session's `active` remote leases, in parallel, every `heartbeat_interval_s`, each bounded by `lease_request_timeout_ms`. 404/410 → client state `lost` with the node's answer in `reason`; transport errors are logged and retried (never mark lost).
- **Lost leases:** every tool call naming a lost lease returns `lease_lost` with the lease, node, why and "call dol_acquire". `dol_release` on a lost lease succeeds with a warning and only clears client state.
- **Release when the node is unreachable:** returns an error, sets the record to `releasing` (no more heartbeats, so the reaper takes it after the grace period) and can be retried.
- **Exit** (stdin closed or SIGTERM/SIGINT): stop heartbeats, close child processes (the M4 tunnel hook), unregister the session. **Remote leases are not released**: the node reaper removes them after `heartbeat_grace_s` unless pinned. Local leases are torn down by the next `dol mcp` that starts (the session is dead by then). There is no config switch for release-on-exit in v1; adding `policy.release_on_exit` later is backwards compatible.

## Consequences
- Restarting Claude Code within the grace period does not resurrect the old session's remote leases (new session id); they expire. `dol_pin` is the tool for keeping work.
- A machine that never starts `dol mcp` again keeps stale local containers; `dol release <id>` cleans them by hand.

## Alternatives
- Releasing remote leases on exit: kills work on every Claude Code restart or crash-restart; the reaper already covers abandonment. Rejected for v1.
