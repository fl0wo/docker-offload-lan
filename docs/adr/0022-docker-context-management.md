# ADR-0022: Docker context management and local context detection

- Status: accepted
- Date: 2026-11-13

## Context
Brief §7.7: `dol mcp` ensures `dol-<node>` contexts exist, never modifies contexts it did not create, and never changes the current context. Local placement needs a local context name for its command prefix (`docker --context <local> …`).

## Decision
- dol-created contexts carry the description **`managed by docker-offload-lan`** (`dockerx.ContextDescription`); that is the ownership marker.
- `dockerx.EnsureContext(name, sshHost)` reads `docker context ls --format '{{json .}}'` (one call gives name, description, endpoint and errors; `docker context inspect` needs one call per context):
  - missing → `docker context create <name> --description "managed by docker-offload-lan" --docker host=ssh://<sshHost>`;
  - ours with another endpoint → `docker context update` with the same flags;
  - not ours → never modified; if its endpoint differs a warning is returned in `dol_acquire.warnings` and the context is used as is.
  It runs once per context per `dol mcp` process. `docker context use` is never called.
- **Local context** (`local.docker_context: auto`): the current context (`docker context show`) if it is usable, else the first usable of `orbstack`, `desktop-linux`, `colima`, `default`. *Usable* = exists, no error, endpoint is a local socket (`unix://` or `npipe://`), name does not start with `dol-` and it is not dol-managed. If none is found, `default` is used and a warning is attached to local placements. Detection runs once at start-up.

## Consequences
- A user who points their current context at a node (ssh://) still gets a local fallback on the laptop's own engine.
- Reachability of the local daemon is reported by `dol_status`, not checked during detection (keeps start-up fast).

## Alternatives
- Probing each candidate with `docker info`: slower start-up (each can take seconds when the VM is stopped). Rejected.
