## Containers: use docker-offload-lan

- Before starting any container, compose stack, kind cluster or heavy build, call `dol_acquire`
  with a realistic `est_memory_gb`. Use exactly the command prefixes it returns for every
  docker/compose/kubectl command, in every command (env vars do not persist between commands).
- Add the returned `label_flags` to every `docker run`, `docker network create` and
  `docker volume create`. Prefer `dol_compose_up` over raw `docker compose up`.
- For kind, use `dol_kind_create` and `dol_kind_load`. Build images with the returned docker
  prefix so they are built on the machine that runs them. Never `docker save | docker load`
  images across machines.
- Bind mounts: call `dol_sync` and use the returned remote path, or use `dol_compose_up`.
- Publish ports on 127.0.0.1 and use `dol_forward` to reach them at localhost.
- If the result says `placement: local`, you are on the laptop: keep the footprint small.
- When finished, call `dol_release`. Use `dol_pin` only if the user asks to keep things running.
