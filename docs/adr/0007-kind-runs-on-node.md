# ADR-0007: kind runs on the node

- Status: accepted
- Date: 2026-11-13

## Context
kind clusters are the biggest memory consumers. kind talks to the local Docker daemon and binds the API server on the daemon host.

## Decision
The node agent runs the node's own `kind` binary (version pinned by `install.sh`) and returns the kubeconfig. Default access mode is `tunnel`: API server bound to 127.0.0.1 on the node, the client opens `ssh -L` and keeps `https://127.0.0.1:<port>` in the kubeconfig. `lan` mode binds 0.0.0.0 and adds the node LAN IP/hostname to `certSANs`.

## Consequences
- API server ports are allocated from `kind.port_range` and persisted so client tunnels can use the same local port.
- Images must exist on the node (build there; `kind load docker-image` runs on the node).

## Alternatives
- Running kind on the client against a remote DOCKER_HOST: kind's port bindings and kubeconfig assume a local daemon; rejected.
