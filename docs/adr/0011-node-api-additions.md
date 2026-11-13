# ADR-0011: Node API additions for compose/sync tracking and release options

- Status: accepted
- Date: 2026-11-13

## Context
Brief §7.10 requires registering compose projects and sync directories on a lease so that teardown removes them, but the §7.2 API table has no route for it. Sync directories live on the node filesystem; the node must only ever delete directories it can prove belong to the lease.

## Decision
Additive, backwards-compatible routes:
- `POST /v1/leases/{id}/compose/{project}` → 200: track a compose project on the lease.
- `POST /v1/leases/{id}/sync` body `{"path": "..."}` → 200: track a sync dir. The node accepts only paths equal to or below `<sync_root>/<lease id>/` (node config `sync_root`, default `/srv/dol`, must match the client's `sync.remote_root`), after `filepath.Clean`, rejecting `..` escapes and symlinks.
- `DELETE /v1/leases/{id}?keep_sync_dir=true` keeps sync dirs on release.
- `Health` gains `time` (clock-skew detection), `accepting` and `problems`; `Capacity` gains `unlabeled` and `warnings`; `Lease` gains `last_error`.

## Consequences
New node config key `sync_root`. Old clients are unaffected.
