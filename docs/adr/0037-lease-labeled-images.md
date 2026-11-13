# ADR-0037: Images built for a lease are labeled and removed on release

- Status: accepted
- Date: 2026-11-13

## Context
On the real tower, images built by the golden path (`svc-a:e2e`, `svc-b:e2e`) stayed on the node after `dol_release`; only containers, networks and volumes were torn down. The node has a 238 GB disk shared with Windows, and agents build images constantly ("build where you run").

## Decision
- Agents add `label_flags` to `docker build` as well (`docker build` accepts `--label`); rules, server instructions and next steps say so. The M6 proxy (opt-in) labels legacy builds automatically.
- Teardown removes images labeled with the lease, after containers/networks/volumes, re-checking the labels on the inspected image and never forcing (an image still used by another lease's container is kept and reported).
- `Inventory.images` is added (optional field; old nodes omit it).

## Consequences
Unlabeled images (built without the flags) are still left to the disk-pressure prune of dangling images and build cache (brief §7.6 step 5). Images loaded into kind clusters live inside the cluster and go with it.
