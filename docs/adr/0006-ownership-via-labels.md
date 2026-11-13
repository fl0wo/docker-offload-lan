# ADR-0006: Ownership via labels

- Status: accepted
- Date: 2026-11-13

## Context
We must never delete what we do not own, on the owner's real machines.

## Decision
Every resource we create, or agents create per our rules, carries `io.github.fl0wo.dol.{lease,owner,session,project,managed}`. The prefix is the constant `v1.LabelPrefix`. Every destructive operation filters by `label=io.github.fl0wo.dol.lease=<id>` and re-checks `managed=true` and the lease label on the inspected object before acting. Kind clusters and compose projects are additionally tracked explicitly on the lease.

## Consequences
- Resources created without labels are never touched (only reported).
- Labels cannot be changed after creation in Docker; ownership is immutable.

## Alternatives
- Tracking IDs in the lease store only: misses resources created by agents directly; rejected.
