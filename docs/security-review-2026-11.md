# Security review, November 2026

Internal review of docker-offload-lan at commit `1bfb622` (feat/m0-bootstrap, "style: gofmt golden test"). Fixes are on the review branch and listed below.

## Scope

- `dol mcp` (internal/mcpserver): every input field of every MCP tool, treated as attacker-controlled (prompt injection through a repository the agent reads).
- `dol node serve` (internal/node): every HTTP route on the unix socket, the reaper and teardown.
- The code those reach: internal/dockerx, internal/compose, internal/kindx, internal/sync, internal/sshx, internal/tunnel, internal/nodeclient, internal/state.
- Config (internal/config), `dol init` (internal/clientsetup), `dol node init` (cmd/dol), deploy/linux/install.sh, deploy/linux/dol-node.service, deploy/windows/setup.ps1.

Invariants checked:

1. dol never stops, removes or deletes a Docker resource, kind cluster, compose project or path it cannot prove belongs to the lease.
2. No argument or command injection into ssh, rsync (`-e`), docker, docker compose, kind, kubectl or a shell.
3. A client holding lease A cannot affect lease B (another owner) through the node API.
4. Nothing listens on TCP; socket 0660 group dol; kubeconfigs and rendered files 0600; no secrets in logs.
5. Setup: no world-writable files, safe sshd config, user files merged without clobbering, no unpinned downloads piped to a shell.

Out of scope: real remote machines (nothing was run against one), Docker and kind themselves.

## Method

1. Listed the entry points: 15 MCP tools and their input structs, 15 node routes, client and node config fields, CLI flags.
2. Traced each one to the `exec.Command`, `os.RemoveAll` or docker call it reaches, noting which values are validated, quoted or label-checked on the way.
3. For each suspected issue, wrote a Go test that sends hostile input to the real function through the existing fakes: the node's fake docker and kind runners, the MCP harness with `nodefake`, and `fakessh`. `fakessh` runs the "remote" command with `sh -c`, as sshd does, so it can prove or rule out shell injection. A finding counts as confirmed only when a test fails on the original code.
4. Fixed each confirmed finding with the smallest change that makes its test pass. Guards that held are kept as regression tests.
5. One behaviour depends on Docker Compose itself: what `down --volumes` deletes. It was checked against local Docker Desktop (Compose v2.39.4) using a throwaway volume. No remote machine was involved.

## Findings

| ID | Severity | Invariant | Status | Test |
|---|---|---|---|---|
| SR-01 | **High**: data loss through a common, non-malicious path. A project dir named like an existing compose project (local Docker or another lease) is enough; `down --volumes` then deletes that project's volumes. | 1, 3 | fixed | `TestSecurityComposeUpRefusesForeignProject`, `TestSecurityComposeDownSkipsForeignResources` (internal/mcpserver), `TestSecurityTeardownDoesNotDownForeignComposeProjects` (internal/node) |
| SR-02 | **Medium**: the extra_args deny-list can be bypassed, switching `compose up` to an unchecked project name or file after dol has labeled and registered the real one, which re-opens SR-01. | 1, 2 | fixed | `TestSecurityComposeExtraArgsCannotOverrideProject` |
| SR-03 | **Low**: code execution on the Mac, but only from a node already controlled by the attacker. In v2, any other holder of a dol key qualifies. | 2 (node→client) | fixed | `TestSecurityKubeconfigFromNodeMustNotExecute` |
| SR-04 | **Low**: other local users of the node can read synced workspaces, including `.env` files. Only affects nodes set up with `dol node init` alone (install.sh uses 0750). | 4, 5 | fixed | `TestSecurityNodeInitSyncRootNotWorldReadable` (cmd/dol) |

### SR-01: compose project names are not ownership

`docker compose -p <p> up|stop|down --volumes` acts on every container, network and volume labeled `com.docker.compose.project=<p>`, no matter who created it. dol took `<p>` from `project_name`, the compose file's `name:`, or the directory name. It then:

- ran `compose up` over an existing project of that name. Compose recreates the foreign containers from dol's file, with this lease's labels, so the lease now "owns" them;
- registered `<p>` on the lease. The node accepts any valid name on any lease;
- ran `compose -p <p> down --volumes --remove-orphans` on release, on `dol_compose_down`/`dol_remove`, and in node teardown and reaper passes.

The result: a lease could destroy another lease's compose project, or the user's own `docker compose` project in a directory with the same name (for example `~/code/shop` on Docker Desktop during a local fallback), together with its volumes. The tests show the exact `compose -p shop down --volumes` invocations against foreign resources.

Fix: `dockerx.RequireComposeOwned` lists everything carrying the project label and inspects each object for the lease's labels. `dol_compose_up` refuses a project that has foreign resources and tells the agent to pick another `project_name`. `dol_stop`, `dol_remove` and `dol_compose_down` refuse too. Client and node teardown skip `compose down` for such a project and log it; the lease's own share is still removed by the label-scoped teardown.

### SR-02: `extra_args` deny-list bypass

`extra_args` are appended after `compose … up`, and compose accepts its global flags there too. The deny-list compared only the text before `=`, so `-pvictim`, `-fother.yml` and `-dpvictim` (combined short flags) got through. Fix: single-dash arguments are checked letter by letter against `-p`, `-f` and `-H`.

### SR-03: kubeconfig from the node is trusted

The client rewrote only `clusters[].cluster.server` in the kubeconfig returned by `POST /v1/leases/{id}/kind` and saved it. A `users[].user.exec` or `auth-provider` entry runs a command on the Mac the first time the agent runs `kubectl --kubeconfig …`. `tokenFile`, `client-certificate` and `client-key` read local files. kind writes only inline `*-data` fields, so the client now refuses all of these.

### SR-04: `dol node init` made sync_root world-readable

`dol node init` created `sync_root` with `MkdirAll(0o2775)` and `chmod 2775`. Fix: `0770` at creation and `2770` after the chown to group dol, consistent with install.sh's `0750 dol:dol`.

## Checked and not an issue

Each item below was traced in the code. Where marked (test), a regression test exercises it and passed on the original code.

- **Shell injection through sync directory names** (test: `TestSecuritySyncDirNamesCannotInjectRemoteCommands`). `;`, `$()`, backticks, quotes, a leading `-`, `--rsync-path=…` and newlines in a local directory name are all reduced to `[A-Za-z0-9._-]` by `SanitizeProject` before the name reaches `mkdir` or rsync's remote path. The `-e` string quotes every ssh argument, and `--` comes before the source and destination. The fake ssh ran the remote commands with `sh -c` and the canary file was never created.
- **Node API path parameters** (test: `TestSecurityNodeAPIGuards`). Encoded slashes, `..` and NUL in `{id}` fail `ValidLeaseID`. Compose project names that look like flags or paths fail the compose regex. `{name}` for kind only matches clusters tracked on that lease, so lease A cannot load into or delete lease B's cluster. Bodies over 1 MiB are rejected before parsing.
- **Sync dirs of another lease, `..`, symlinks** (tests: `TestSecurityNodeAPIGuards`, `TestSecuritySyncTeardownDoesNotFollowInnerSymlinks`, existing `TestSyncDirSymlinkEscape`). Registration and teardown both re-validate `<sync_root>/<lease>/…` and the absence of symlink components. Symlinks inside a synced tree, which rsync `-a` copies from the repository, are removed as links, and their targets survive.
- **Compose `down --volumes` and pre-existing volumes declared with `name:`**. Against Compose v2.39.4, `down --volumes` did not remove a volume declared in the project file when that volume was unlabeled or carried another project's label. It is listed under residual risks because older Compose versions were not tested.
- **Label spoofing.** A container that someone else creates with our labels and a known lease id is removed when that lease is torn down. That is the design (ADR-0006): it is the spoofer's own container. Spoofing cannot redirect a removal to an unlabeled object. Teardown lists objects by label and then re-inspects each one by its immutable ID (volumes by name) before `rm`. Lease ids that fail `ValidLeaseID` in labels are ignored by the orphan reaper.
- **kind `config_yaml`**: `extraMounts` of `/`, `extraPortMappings` on `0.0.0.0`, kubeadm patches. Also compose files that bind-mount `/` or the Docker socket, or use `driver_opts` bind volumes. All of these give an agent the same power as `docker run --privileged -v /:/host`, which any holder of the docker context already has. The docker group is root on the node by design (docs/security.md). `allow_external_mounts` is a correctness guard (the paths cannot be synced), not a security boundary. The cluster name always comes from `--name dol-<lease>-<name>`, which overrides a `name:` in the config.
- **`dol_sync` / `project_dir` accept any local directory.** This can copy, for example, `~/.ssh` to the node. The agent can already read those files on the Mac, and the node belongs to the same user. This is not a privilege boundary.
- **`remote_host` of `dol_forward`.** It becomes part of a single `-L` argument and is sent to sshd as a direct-tcpip target, never to a shell, so it cannot become an ssh option. `AllowTcpForwarding local` lets the agent reach the node's LAN, which it can also do from a container.
- **HTTP request smuggling over dial-stdio.** dial-stdio is a byte pipe, and Go's server rejects ambiguous `Content-Length`/`Transfer-Encoding`. Everyone who can reach the pipe is already the `dol` user with full API and Docker access, so there is nothing to smuggle past.
- **TCP exposure, sockets, files.** Nothing in the code listens on TCP: the only `net.Listen("tcp")` calls are short-lived port probes and the 127.0.0.1-bound tunnel listeners started by ssh. The node socket is chmod 0660 and given group dol, and `UMask=0007` in the unit closes the gap between `listen` and `chmod`. Kubeconfigs (existing test), rendered compose files and the client state are 0600 inside 0700 dirs. The node's kind work dir is 0700 and the kubeconfig temp file is deleted. Logs carry ids, names and errors, never kubeconfigs or env values.
- **Config and setup.** `ssh_host` is rejected if it starts with `-` or contains shell metacharacters, which matters because rsync appends it unquoted after `-e`. `dol init` validates alias, host, user and identity file before writing the managed `Host` block, writes 0600 with a backup, and never edits outside its markers. install.sh downloads kind and kubectl against pinned sha256 values, nothing is piped to a shell, the sshd drop-in is scoped with `Match LocalPort 2222` (ADR-0030) and checked with `sshd -t`, and authorized_keys and daemon.json are merged rather than overwritten (daemon.json gets a timestamped backup first). setup.ps1 opens only TCP 2222 for the WSL VM, on the Private and Domain profiles.

## Accepted by design

- **Invariant 3 does not hold against a client that talks to the node API directly.** Every client logs in as the same Unix user `dol`, the `owner` field is asserted by the client, and the API has no per-lease credential. Any dol key holder can list, PATCH, heartbeat or DELETE any lease, or register compose projects and sync dirs on it. That key holder can also `docker rm` anything directly, so API checks would not be a boundary (ADR-0002, docs/security.md). Through `dol mcp`, tools only accept lease ids from the local client state, so an agent cannot name another owner's lease. SR-01 was the case where lease A could hurt lease B through `dol` itself. Per-client authorization (separate Unix users or tokens) is needed before v2 multi-client.
- **Orphan kind clusters are recognized by name.** Clusters named `dol-l-<10 base32>-*` whose lease is unknown or released are deleted (ADR-0013). Such a name is in dol's namespace, but nothing proves who created the cluster.

## Residual risks (plausible, not proven or not fixed)

- **TOCTOU in sync dir removal.** `CheckNoSymlinks` and `os.RemoveAll` resolve the path separately. A process that swaps `<sync_root>/<lease>` for a symlink between the two calls would redirect the delete. This was not reproduced (it needs a race), and it brings no privilege gain: the swap needs write access to sync_root, which only the `dol` user and root have, and the agent also runs as `dol`. MCP tools cannot create a symlink at that depth, because rsync writes only inside the registered directory. `os.Root` plus `O_NOFOLLOW` traversal would close it if sync_root ever becomes shared between Unix users.
- **Name-based TOCTOU in `dol_stop`/`dol_remove`.** Ownership is inspected by name and the action is then taken by name. A rename or recreate between the two calls could redirect it. Only something with Docker access can cause that, and Docker access already allows the action itself. Hardening: act on the ID returned by inspect.
- **RequireComposeOwned is check-then-act.** A foreign object added to the project between the check and `compose down` would be removed. Same precondition as above.
- **Compose versions.** The `down --volumes` behaviour for volumes declared with `name:` was confirmed on Compose v2.39.4 only.
- **Supply chain.** install.sh resolves the dol release `latest` through the GitHub API and verifies the archive against `checksums.txt` from the same release. That protects integrity in transit, not authenticity; pass `--dol-version` or `--dol-binary` to pin. The Docker apt key is fetched over HTTPS without a fingerprint pin, which is the upstream instruction.
