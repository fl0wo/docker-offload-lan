# ADR-0035: Integration tests use a fake node built by the real install.sh

- Status: accepted
- Date: 2026-11-13

## Context
Brief §14 asks for a containerized Linux fake node (sshd, Docker-in-Docker and `dol node serve`) so that CI can run the client↔node path without hardware. The install script also needs a real test.

## Decision
- **Image.** `test/integration/Dockerfile` (FROM ubuntu:24.04) runs `deploy/linux/install.sh --container-mode` twice:
  1. for the heavy, cached layer, with `--no-dol`;
  2. with the freshly cross-compiled `dol`.

  `--container-mode` skips systemd.
- **Entrypoint.** It does the following, in order:
  1. Starts dockerd, with the docker:dind cgroup v2 fix.
  2. Re-runs install.sh with the per-test public key.
  3. Starts sshd.
  4. Starts `dol node serve` as user `dol`, tolerating a build that has no agent yet.
  5. Touches `/run/dol-it-ready`.
- **Container.** It runs privileged with `--init`, and 2222 is published on a random `127.0.0.1` host port. `/var/lib/docker` is an anonymous volume, removed with `docker rm -v`.
- **Harness.** `test/integration/harness`:
  - generates an ed25519 key;
  - writes an ssh config with `StrictHostKeyChecking no`, `UserKnownHostsFile /dev/null` and `IdentityAgent none`;
  - puts `ControlPath` in a short `/tmp/dolit-*` dir, because macOS limits unix socket paths to about 104 bytes;
  - exposes `sshx.Options{ExtraArgs: [-F cfg]}`;
  - creates docker context `dol-it` in a temp `DOCKER_CONFIG`.
- **ssh wrapper.** docker's ssh connhelper runs `ssh` from PATH, and OpenSSH reads `~/.ssh/config` from the passwd home directory, ignoring `$HOME`. The harness therefore puts a wrapper `ssh` script first in PATH that execs the real ssh with `-F <test config>`.
- **Gating.** Tests run only with `DOL_INTEGRATION=1`. `DOL_IT_KEEP=1` keeps the containers for debugging, and `DOL_IT_RMI=1` removes the image at the end.

## Consequences
Every integration run also tests install.sh end to end: packages, the Docker repo, pinned downloads, sshd config and idempotency. The first build needs internet access; later builds reuse the cached layer.

## Alternatives
- docker:dind as the base image: rejected, because it would not exercise install.sh.
- testcontainers-go: rejected, because it adds a large dependency for little gain over the CLI.
