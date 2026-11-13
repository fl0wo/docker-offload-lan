# ADR-0033: Doctor check model

- Status: accepted
- Date: 2026-11-13

## Context
`dol doctor` must diagnose every link (brief §12) with actionable fixes, run on both client and node, and be testable without real machines.

## Decision
- `internal/doctor` defines checks as tables of `{ID, Title, Timeout, Run(ctx, state) Outcome}`. Tables run in order. A per-run state lets later checks skip when a prerequisite failed; for example, everything that goes over ssh is skipped when `ssh` fails. A check that panics is reported as `fail`.
- Every result has a stable `id`, a `status` (`ok`, `warn`, `fail`, `info` or `skip`), a measured duration, a message and, for warn and fail, a specific `fix`. The ids are the section names in docs/troubleshooting.md.
- All side effects are injected: a process `Runner` (ssh, docker, rsync, systemctl …), a `NodeAPI` (nodeclient over ssh or a unix socket), and readers for stat, statfs and listening ports (/proc/net/tcp). Unit tests use scripted fakes. Integration tests run the client checks against the containerized node.
- The exit status is 1 if any check is `fail`. `warn` and `info` never fail the run. `--json` prints the full report.
- Severity:
  - `fail` for anything that breaks remote placement: ssh, docker group, docker context, node agent, API mismatch, disk below the minimum, clock skew over 30 s.
  - `warn` for degradations: no ControlMaster, minor version drift, skew of 2–30 s, disk below twice the minimum, tool version drift.
  - `info` for the architecture difference.

## Consequences
A new check is one table row plus a test case. Check ids are a public interface for the docs and for `--json` consumers, so renaming one requires a docs update.

## Alternatives
Free-form check functions that print directly: rejected because they are hard to test and to emit as JSON.
