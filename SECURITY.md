# Security policy

## Reporting a vulnerability

Please report vulnerabilities privately via GitHub's "Report a vulnerability" (Security tab → Advisories) on this repository. Do not open a public issue. We aim to acknowledge reports within 7 days.

## Security model (summary)

- All client↔node traffic goes over SSH. The Docker API and the node API are never exposed on TCP in v1.
- The node user `dol` is in the `docker` group, which is **root-equivalent** on the node (or its WSL VM). Treat the SSH key that can log in as `dol` like a root key.
- The node API socket is mode `0660`, group `dol`.
- dol only stops or deletes resources carrying its labels for the lease in question. Unlabeled resources are never deleted unless you opt in via `reaper.reap_unlabeled_after_s`.
- `-p 8080:80` on the node publishes on `0.0.0.0` and is reachable from your LAN. Publish on `127.0.0.1` and use `dol_forward`.

See docs/security.md for the full threat model.
