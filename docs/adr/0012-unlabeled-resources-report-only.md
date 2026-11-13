# ADR-0012: Unlabeled resources are reported, never deleted (v1)

- Status: accepted
- Date: 2026-11-13

## Context
Brief §7.6 step 4 says unlabeled Docker resources are never deleted by default, and that deleting them "requires `reap_unlabeled_after_s > 0` and is logged loudly". Deleting something only after it has been unlabeled for N seconds needs a first-seen timestamp per resource, kept across agent restarts, and a definition of "unlabeled" that never matches the node owner's own containers (databases, the WSL keep-alive, other tools). Getting this wrong deletes data on the owner's real machine (brief §0 rule 4, §17).

## Decision
In v1 the node agent never deletes unlabeled resources, whatever the config says. The reaper counts unlabeled containers, volumes and networks (`dockerx.CountUnlabeled`; kind node containers of dol clusters and Docker's built-in networks are excluded) and reports them in `GET /v1/capacity` → `unlabeled`. When `reaper.reap_unlabeled_after_s > 0` and unlabeled resources exist, every reaper pass logs a warning that the setting is not implemented and the resources are only reported.

The config key stays (schema unchanged) so a later version can implement it without a config migration.

## Consequences
- Agents that forget `label_flags` leak resources until a human removes them; `dol doctor` and `/v1/capacity` make the count visible.
- No code path can delete a resource that lacks `<P>.managed=true` plus a matching `<P>.lease`.

## Alternatives
- Implement it now with a `first_seen` table keyed by Docker object ID: rejected for M2 because the safety argument (what counts as abandoned) needs owner review first.
