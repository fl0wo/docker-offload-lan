# ADR-0004: Leases, heartbeats and a reaper

- Status: accepted
- Date: 2026-11-13

## Context
Agent sessions crash, laptops sleep, users quit. Remote resources must not leak on a 238 GB disk.

## Decision
Every workload belongs to a lease. The client heartbeats each active remote lease every `heartbeat_interval_s`. The node reaper expires unpinned leases after `heartbeat_grace_s` without a heartbeat or after `max_ttl_s`, and tears down everything the lease owns. Pinned leases expire only at an explicit `expires_at`. Orphans (managed label, unknown/dead lease) are removed. Unlabeled resources are only reported unless explicitly configured.

## Consequences
- A laptop that sleeps longer than the grace period loses its remote workloads; documented, and `dol_pin` exists for long-lived work.
- Teardown is idempotent and retried on the next reaper pass.
- On boot the node reconciles stored leases against Docker.

## Alternatives
- Session-bound SSH connections whose closure triggers cleanup: fragile with ControlMaster and network blips; rejected.
