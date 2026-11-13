# ADR-0002: All client↔node traffic goes over SSH

- Status: accepted
- Date: 2026-11-13

## Context
The client needs both the Docker API and our node API on the node. Exposing either over TCP creates a second trust boundary, tokens on the wire and firewall work.

## Decision
- Docker API: `ssh://` docker contexts (Docker's own connhelper spawns `ssh … docker system dial-stdio`).
- Node API: HTTP/1.1 over a unix socket on the node, reached with `ssh <host> dol node dial-stdio`, which bridges stdio to the socket. The Go client uses a custom `DialContext` that spawns this process and uses its stdio as the connection.
- We shell out to the system `ssh` binary; we do not embed an SSH client. This respects `~/.ssh/config`, agents, ProxyJump and ControlMaster.

## Consequences
- One trust boundary: SSH keys. Authorization = being able to SSH as a member of the `dol` group.
- No extra open ports besides sshd (2222 on WSL nodes).
- With ControlMaster, each API call costs a channel open, not a TCP+key exchange.
- Every request spawns an ssh process; the client reuses HTTP connections (keep-alive) over a single dial-stdio session where possible.

## Alternatives
- TCP + mTLS listener: deferred to v2 (optional).
- Embedded `golang.org/x/crypto/ssh`: ignores user ssh config and agent setups; rejected.
