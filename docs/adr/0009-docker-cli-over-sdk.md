# ADR-0009: Shell out to the docker CLI instead of the Docker Go SDK (v1)

- Status: accepted
- Date: 2026-11-13

## Context
The brief prefers the Docker Go SDK for events/inspect/list and the CLI for contexts and compose. The SDK module moved (`github.com/docker/docker/client` → `github.com/moby/moby/client`, v0.6.x as of 2026-11), it does not resolve docker contexts or `ssh://` hosts by itself, and it pulls a large dependency tree.

## Decision
v1 uses the `docker` CLI everywhere (`--format '{{json .}}'`, `inspect`, `events --format`), behind a small interface in `internal/dockerx` so the implementation can switch to the SDK later without touching callers. The node runs the CLI against its local daemon; the client runs it with `--context`.

## Consequences
- Identical behavior to what agents run; contexts and ssh:// work for free.
- Output parsing is covered by tests with recorded CLI output.
- Slightly more process spawning; negligible at our call rates.

## Alternatives
- `github.com/moby/moby/client`: revisit if event streaming or performance needs it.
