# ADR-0034: What `dol init` may change on the client, and how

- Status: accepted
- Date: 2026-11-13

## Context
`dol init` edits files the user owns (`~/.ssh/config`, the client config) and creates a docker context. It must be idempotent and must never damage the user's setup.

## Decision
- **ssh.** dol owns `~/.ssh/config.d/docker-offload-lan` (mode 0600).
  - Each node's `Host` block sits between `# BEGIN/END docker-offload-lan node <alias>` markers and is rewritten in place.
  - If a `Host <alias>` exists outside the markers, init refuses.
  - The only change to `~/.ssh/config` is `Include config.d/*` at the very top, because an Include after a `Host` line would be scoped to that host. It is added only when no global Include already covers the fragment, and only after a timestamped backup.
  - Host values are validated against a strict character set, so they cannot inject ssh directives.
- **Client config.** It is edited as a YAML node tree, so comments survive, as do node keys that dol does not manage (e.g. `wake_on_lan`). The result is validated with the real loader before an atomic write, and a `.bak-dol` copy is kept.
- **Docker contexts.** `dol-<name>` is created with the description "managed by docker-offload-lan".
  - If a context with that description points to a different host, it is updated.
  - A context without that description is never touched: init fails and tells the user what to do.
  - The current context is never changed.
  - This step runs first, so a refusal writes nothing.
- **Host keys.** On first contact the host key is accepted with `StrictHostKeyChecking=accept-new`. A changed key is still refused.

## Consequences
Re-running init is the documented fix for most `ssh-config` and `docker-context` doctor findings.

## Alternatives
- Append to `~/.ssh/config` directly: rejected, because it is harder to update and to remove.
- Re-marshal the config struct: rejected, because it loses the user's comments.
