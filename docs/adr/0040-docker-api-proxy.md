# ADR-0040: Label-injecting Docker API proxy (M6 spike)

- Status: accepted as opt-in (`proxy.enabled: false` by default)
- Date: 2026-11-13

## Context
ADR-0005 has agents run the plain `docker` CLI with a `--context` prefix, and ADR-0006 makes labels the only proof of ownership. So every `docker run`, `network create` and `volume create` has to carry `label_flags`, and bind mounts have to name node paths after `dol_sync`. Agents forget to do this. Unlabeled resources are never reaped (ADR-0012), and Mac paths do not exist on the node.

Brief §13 M6 asks whether `dol mcp` can remove that discipline. The idea is a per-lease unix socket on the Mac that proxies the Docker Engine API to the lease's engine. The engine is either the node, through `ssh <host> docker system dial-stdio`, or the local engine. On the way through, the proxy:

- injects the lease labels;
- rewrites project bind mounts;
- records published ports.

## Decision
**Accept it as opt-in.** `internal/dockerproxy` stays in the product. Off by default, `dol_acquire` behaves exactly as before. When the client config has `proxy.enabled: true`, `dol mcp` also serves the proxy for each lease.

### Proxy behavior (`internal/dockerproxy`)
- **Engine API passthrough.** It is an `http.Handler` built on `httputil.ReverseProxy`:
  - `FlushInterval: -1`, so streams such as `logs -f`, `events` and pull/build progress are flushed as they arrive;
  - request bodies are streamed;
  - API version prefixes (`/v1.NN/…`) are stripped before routing.
- **Rewritten calls.** Only these calls are rewritten:
  - `POST /containers/create`:
    - merges the lease labels into `Labels`;
    - rewrites the sources of `HostConfig.Binds` (`src:dst[:opts]`) and of `HostConfig.Mounts[type=bind]` that lie under the project directory to `<remote_root>/<lease>/<project>` (remote leases only; this reuses `compose.MapPath`, symlink-aware);
    - adds the lease labels to `Mounts[type=volume].VolumeOptions.Labels`, so implicitly created named volumes are labeled (the engine ignores this for existing volumes; verified);
    - records `PortBindings` together with the container ID taken from the response.
  - `POST /networks/create`, `POST /volumes/create`: merge `Labels`.
  - `POST /build`: merges into the `labels` query parameter.
- **Body limits.** Create bodies are decoded with `UseNumber`, so 64-bit integers survive. They are bounded at 8 MiB; a larger body gets a 413.
- **Label merge.** User labels are never changed. Keys under `io.github.fl0wo.dol.` always get the lease's value: a resource created through a lease socket belongs to that lease, so an agent cannot spoof another lease.
- **Bind sources outside the project.** On remote leases, absolute bind sources outside the project directory are refused with a Docker-style 400 (`Error response from daemon: docker-offload-lan proxy: …`), as `dol_compose_up` does. `proxy.allow_external_binds: true` passes them through as node paths instead.
- **Upgrade requests** (`Connection: Upgrade`: attach, exec start, BuildKit `/session` and `/grpc`) are handled by our own code, not by `ReverseProxy`. `ReverseProxy` closes both sides as soon as one direction ends, which truncates `echo x | docker exec -i c cat`. Our code:
  1. sends the request on a dedicated upstream connection;
  2. on `101`, relays the engine's response verbatim, then splices the two connections byte for byte;
  3. on client EOF, propagates it with `CloseWrite` upstream; `sshx.StdioConn` gained `CloseWrite`, which closes ssh's stdin;
  4. when the upstream side ends, half-closes the client and then closes both.

  If the engine answers with anything other than `101` (for example 404 or 409), that response is relayed as a normal response.
- **Socket.** `<state>/proxy/<lease>.sock`, in a directory with mode 0700; the socket itself is 0600.
  - If that path would be longer than 103 bytes (the macOS limit), the socket goes to `/tmp/dol-<uid>/`.
  - A stale socket is replaced. A live socket, or a file that is not a socket, is refused.
  - `Server.Close` closes the hijacked streams too, because `http.Server.Close` does not.

### Wiring (opt-in)
- **Config.** `ClientConfig.Proxy{Enabled, AllowExternalBinds}`; the schema is regenerated.
- **Acquire.** `dol_acquire` starts the proxy after the lease is stored. A failure only adds a warning; the context prefix still works.
- **Additive result fields.**
  - `AcquireResult.docker_host` (`unix://…`);
  - `commands.docker_proxied` (`docker -H unix://…`) and `commands.compose_proxied`;
  - one extra next step and one extra clause in the summary.

  These appear in every `placementInfo` while the proxy runs. Existing fields are unchanged.
- **Upstream.**
  - Remote leases: `ssh <ssh_host> docker system dial-stdio`, with the user's ssh config. Idle connections are kept alive (at most 8).
  - Local leases: the endpoint of the local docker context (`unix://` only).
- **Lifetime.** The proxy is closed on `dol_release` and when `dol mcp` exits (`Core.Close`). It never outlives the process that created it.

## Measurements
Tools: `proxytest.Run`, the real `docker` 28.4.0 CLI (API 1.51), Compose 2.39.4 and buildx 0.28.0, on an M-series Mac with Docker Desktop.

- Local engine: `DOL_PROXY_REAL=1 go test ./internal/dockerproxy -run TestRealEngine`.
- Fake node: `DOL_INTEGRATION=1 go test ./test/integration -run TestDockerProxyOverSSH`. The fake node is the containerized Ubuntu node with sshd, Docker-in-Docker and ssh ControlMaster, reached over 127.0.0.1, so there is no LAN round-trip time.

Method:
- Direct and proxied runs are interleaved.
- Latency rows have N = 50 (CLI) and N = 200 (API).
- Each build uses a fresh 200 MiB random file, so there is no cache and no incremental context sync.

### Local engine (desktop-linux; the direct path uses `-H unix://…/docker.sock`)
| measurement | N | direct p50 | direct p95 | proxy p50 | proxy p95 | Δ p50 |
|---|---|---|---|---|---|---|
| API `GET /_ping` (keep-alive, no CLI) | 200 | 0.54 ms | 0.74 ms | 0.57 ms | 0.78 ms | +0.03 ms |
| API `GET /containers/json?all=1` | 200 | 18.55 ms | 21.54 ms | 18.92 ms | 22.51 ms | +0.37 ms |
| CLI `docker ps` | 50 | 13.35 ms | 15.65 ms | 13.53 ms | 15.75 ms | +0.18 ms |
| CLI `docker create` + `docker rm` | 50 | 95.58 ms | 105.42 ms | 96.77 ms | 105.02 ms | +1.19 ms |

| build, 200 MiB context (wall time) | runs | direct p50 | direct MB/s | proxy p50 | proxy MB/s |
|---|---|---|---|---|---|
| legacy builder (`DOCKER_BUILDKIT=0`, `POST /build`) | 5 | 5.07 s | 40 | 5.70 s | 35 |
| BuildKit (buildx docker driver, `/session` + `/grpc`) | 5 | 4.74 s | 42 | 4.62 s | 44 |

A first run used `--context desktop-linux` as the direct path. There the proxy looked faster: `docker ps` took 13.20 ms direct and 11.26 ms through the proxy, because resolving the context costs the CLI about 2 ms. The table above therefore compares like with like.

### Fake node over ssh (the direct path is the `ssh://` docker context `dol-it`)
| measurement | N | direct p50 | direct p95 | proxy p50 | proxy p95 | Δ p50 |
|---|---|---|---|---|---|---|
| API `GET /_ping` (keep-alive) | 200 | 0.34 ms | 0.46 ms | 0.37 ms | 0.53 ms | +0.03 ms |
| API `GET /containers/json?all=1` | 200 | 0.35 ms | 0.45 ms | 0.37 ms | 0.52 ms | +0.03 ms |
| CLI `docker ps` | 50 | 27.15 ms | 28.73 ms | 11.48 ms | 12.44 ms | **−15.67 ms** |
| CLI `docker create` + `docker rm` | 50 | 78.93 ms | 332.85 ms | 45.85 ms | 272.30 ms | **−33.08 ms** |

| build, 200 MiB context | runs | direct p50 | direct MB/s | proxy p50 | proxy MB/s |
|---|---|---|---|---|---|
| legacy builder | 3 | 2.52 s | 79 | 2.73 s | 73 |
| BuildKit | 3 | 4.28 s | 46 | 4.15 s | 48 |

Interpretation:
- **The proxy's own cost** is about 0.03 ms per request.
- **Over ssh the proxy is faster.** Each `docker --context ssh://…` invocation starts `ssh … docker system dial-stdio`, which costs about 15 ms even with ControlMaster. The proxy keeps warm dial-stdio connections instead.
- **BuildKit builds** show no difference.
- **The legacy `/build` upload** is 8–12% slower through the proxy (raw `curl -T` of the same tar: 4.5 s direct, 5.3 s proxied). The cause is not explained:
  - against an in-process fake engine, the proxy streams a 256 MiB chunked body at about 2 GB/s with a peak heap of 2–3 MiB (`TestStreamedBodyThroughput`);
  - forcing 1 MiB upstream writes did not change the result.

  It matters little, because the docker CLI uses BuildKit by default.

### Verified through the proxy, on both targets unless noted
- `docker run --rm alpine echo`: correct output.
- `docker run -i`: 10 MiB stdin into `sha256sum`, hash matches (attach + half-close).
- `docker run`: 10 MiB of stdout, byte-exact (multiplexed attach stream).
- `docker exec -i`:
  - 4 MiB stdin, hash matches;
  - `echo ping | docker exec -i c cat` returns `ping` (output survives the client's half-close).
- `docker exec -it` under a pty (`script`): it is a tty, and the output is correct.
- Exit codes are propagated (`exit 7`).
- `docker logs -f`: first line after 12–16 ms; the stream ends with the container.
- `docker events --since/--until --filter label=…lease=…`.
- Labels, checked with `docker inspect`:
  - containers, networks and volumes carry the lease labels;
  - user labels are kept;
  - `--filter label=…lease=<id>` finds the container, which is what `dol_list` and the reaper use;
  - an implicit named volume from `--mount type=volume` is labeled (local engine only; this check was added after the ssh run).
- Bind rewrite:
  - `-v <proj>/sub:/data:ro` and `--mount type=bind,source=<proj>` are mapped to the node path;
  - `-v /etc:/x` is refused with a clear message.
- `docker compose up -d --wait`:
  - the container, the network and the named volume all carry lease labels;
  - compose's own labels are kept;
  - the relative `./sub` bind is rewritten;
  - a second `up` is a no-op (no recreate, so no config-hash drift);
  - `down -v` cleans up.
- `docker build`:
  - legacy builder: image labels injected;
  - BuildKit: the session and grpc upgrades work.
- Unit tests with an httptest fake engine cover:
  - label injection and preservation of user labels;
  - a spoofed lease label being overridden;
  - 64-bit integer fidelity;
  - chunked create bodies;
  - the oversize-body 413;
  - bind and mount rewrite and the refusal of external binds;
  - `/build` query merge with a byte-exact streamed 24 MiB context;
  - the hijack path, byte-exact in both directions (5 MiB in, 3 MiB out) with half-close;
  - exec start body forwarding with and without Tty;
  - relaying a non-101 hijack error;
  - streaming flush;
  - socket mode, stale-socket replacement, refusal of a live socket, `Close` ending hijacked streams;
  - the bounded-heap throughput check.

### Not verified, or not covered
- **BuildKit image labels.** BuildKit builds go over `/grpc` (h2c). Labels travel inside the gRPC solve request, which the proxy does not parse, so images built with BuildKit are not labeled. This matches the default `docker build`, `docker compose build` and `buildx bake`. The reaper does not reap images (it prunes dangling images and build cache only), so ownership is unaffected; only `docker image ls --filter label=…` misses them.
- **Implicit volumes from `-v name:/path`** (`HostConfig.Binds`) and anonymous volumes are not labeled; only `--mount type=volume` is. The fix would be to pre-create named volumes; it is not done.
- **Not exercised:** `docker cp` (`/archive`), `docker load`/`save` through the proxy, `docker push`/`pull` with registry auth, `docker attach` detach keys, swarm `/services/create` (not labeled), plugins, and `docker system dial-stdio` through the proxy.

  All of these are plain streamed calls or upgrades on the same code paths, but they are not measured.
- **Published ports** are recorded (`Proxy.Ports`, logged) but not forwarded yet. Ephemeral host ports (`-p 80`) are only known after start.
- **Tools that read docker contexts** (`docker context`, IDE integrations, `kind` on the Mac, anything ignoring `DOCKER_HOST`/`-H`) do not see the proxy. It is an endpoint, not a context, and creating a context for it would contradict ADR-0022's minimalism.
- **Real LAN.** Only the localhost fake node was measured; on a real LAN both paths pay the same round-trip time.
- **Abandoned hijacked streams.** If a client half-closes (or dies) while an attached container produces no output, the proxy keeps the stream until the container ends or the lease is released.

## Consequences
- With the option on, agents can run `docker -H unix://…/l-xxx.sock …` (the `commands.docker_proxied` value) without `label_flags`. Bind mounts under the project then work on remote leases after `dol_sync`, and compose files run unchanged. The `--context` prefix and `label_flags` remain the documented default and keep working.
- The socket lives only as long as the `dol mcp` process. After a restart, a remembered `docker_host` fails closed ("Cannot connect to the Docker daemon"), never silently against the wrong engine; re-run `dol_acquire`.
- Labeling is best effort, not enforcement: an agent can still bypass the proxy. ADR-0006 and ADR-0012 are unchanged.
- The socket grants full Docker API access to the lease's engine, which is the same trust as the docker context. It is private to the user (0700 directory, 0600 socket) and is never TCP (§17).

## Alternatives
- **On by default:** rejected for now. The open gaps (BuildKit image labels, `-v name:` volumes, process-bound lifetime, no real-LAN numbers) argue for a period as opt-in before changing the contract agents rely on.
- **Reject:** the measured overhead is negligible, the proxy is faster than ssh contexts, and it removes the main failure mode of ADR-0005.
- **`httputil.ReverseProxy`'s built-in upgrade handling:** it drops output after a stdin half-close, so it was replaced with the custom splice described above.
- **Parsing BuildKit gRPC to inject labels:** large and fragile. Instead, recommend `--label` or labels in the Dockerfile, or accept unlabeled images.
- **A docker context pointing at the socket:** it would modify the user's context store for a process-scoped endpoint. Rejected; `-H` per command fits ADR-0005, because environment variables do not persist.
