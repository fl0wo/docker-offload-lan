# Setting up the Mac (client)

You need:

- a node prepared with [setup-windows.md](setup-windows.md), or a Linux node with `install.sh`;
- an ssh key whose public half you passed to `install.sh --pubkey`;
- a local Docker (Docker Desktop, OrbStack or Colima) for the automatic fallback.

## 1. Install dol

```sh
brew install fl0wo/tap/dol      # once releases are published
# or from source:
go install github.com/fl0wo/docker-offload-lan/cmd/dol@latest
```

Optional but recommended: install `rsync` from Homebrew (`brew install rsync`). macOS ships `openrsync`, which also works with dol's flags (see [compat.md](compat.md) §5).

## 2. Run `dol init`

```sh
dol init                                              # interactive
dol init --name tower --host 192.168.1.50 --yes       # non-interactive
```

Flags: `--name` (default `tower`; used for the ssh alias, the node name and docker context `dol-<name>`), `--host` (LAN IP or DNS name), `--user dol`, `--port 2222`, `--identity-file ~/.ssh/id_ed25519`, `--yes`, `--run-claude-add`, `--skip-doctor`, `--skip-ssh-check`.

`dol init` is idempotent: every step prints `[changed]` or `[unchanged]`. It does the following:

1. Creates docker context `dol-tower` → `ssh://tower` with the description "managed by docker-offload-lan". **Your current context is not changed.** If a `dol-tower` context exists that dol did not create, init stops before writing anything.
2. Writes a Host block to `~/.ssh/config.d/docker-offload-lan`:

   ```
   Host tower
       HostName 192.168.1.50
       Port 2222
       User dol
       ControlMaster auto
       ControlPath ~/.ssh/cm-%C
       ControlPersist 10m
       ServerAliveInterval 15
       ServerAliveCountMax 3
   ```

   ControlMaster matters: every `docker --context dol-tower …` call and every dol API call reuses one ssh connection instead of doing a new handshake.
3. Adds `Include config.d/*` at the **top** of `~/.ssh/config` if it is missing. The original is backed up to `~/.ssh/config.bak-dol-<timestamp>` first.
4. Adds the node to `~/.config/docker-offload-lan/config.yaml` (`$XDG_CONFIG_HOME` is respected). Comments and your other settings are kept, and a `.bak-dol` copy is written.
5. Connects once with `StrictHostKeyChecking=accept-new`, which records the node's host key on first contact. A *changed* key is still refused.
6. Runs `dol doctor` for the node.
7. Prints the Claude Code registration command and, with `--run-claude-add`, runs it:

   ```sh
   claude mcp add -s user docker-offload-lan -- dol mcp
   ```

8. Suggests lowering your local Docker VM's memory once remote placement works. Local fallback keeps working with a smaller VM. Docker Desktop: Settings → Resources. OrbStack: Settings → System. Colima: `colima start --memory 4`.

## 3. Verify

```sh
dol doctor
docker --context dol-tower run --rm busybox echo hello from the node
```

`dol doctor` shows one line per check: ✓ ok, ⚠ warning, ✗ failure (with a `fix:` line), i info, · skipped. It exits with status 1 if any check failed. Use `--json` for machine-readable output and `--node-name tower` to check one node. Failures are explained in [troubleshooting.md](troubleshooting.md) under the check id.

## 4. Tell your agents

Append [`rules/agent.md`](../rules/agent.md) to your `CLAUDE.md`.

## Undo

```sh
docker context rm dol-tower
rm ~/.ssh/config.d/docker-offload-lan      # then delete the Include line in ~/.ssh/config if you like
rm -r ~/.config/docker-offload-lan
claude mcp remove -s user docker-offload-lan
```
