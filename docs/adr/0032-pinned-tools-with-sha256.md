# ADR-0032: Pin kind/kubectl versions and their sha256 in install.sh

- Status: accepted
- Date: 2026-11-13

## Context
The node agent shells out to `kind` and `kubectl` (ADR-0007), so its behaviour depends on their versions. Downloads must be verified.

## Decision
- `install.sh` pins `KIND_VERSION=v0.33.0` and `KUBECTL_VERSION=v1.37.2` and embeds their sha256 for linux/amd64 and linux/arm64. The hashes were taken on 2026-11-13 from the official `kind-linux-<arch>.sha256sum` files and `dl.k8s.io/.../kubectl.sha256`. A mismatch aborts the install.
- Idempotency compares the installed version (`kind version`, `kubectl version --client -o json`), so re-runs work offline.
- `internal/doctor/pins.go` holds the same versions for `dol doctor` (client and `--node`). `TestPinsMatchInstallScript` fails if the two drift apart.
- A `dol` binary downloaded from GitHub releases is verified against the release's `checksums.txt` (from GoReleaser).

## Consequences
Bumping a pin is one commit that changes the version, both hashes, `pins.go` and docs/compat.md.

## Alternatives
- Fetch the checksum file next to the binary at install time: rejected, because it only protects against transport corruption, not against a swapped release.
- Install kind/kubectl from apt: rejected, because Ubuntu does not package them at pinned versions.
