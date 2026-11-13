# ADR-0023: MCP tool result conventions (M3)

- Status: accepted
- Date: 2026-11-13

## Context
Brief §8 asks for structured content plus a 2–4 line summary, and actionable errors. The MCP Go SDK (v1.8.0) infers JSON Schemas from Go types, validates inputs and outputs, and turns handler errors into `isError` results.

## Decision
- Tools are registered with `mcp.AddTool[In, Out]`; `Out` is the matching `pkg/api/v1` type (`StatusResult`, `AcquireResult`, `ReleaseResult`, `ListResult`, `ActionResult` for stop/remove/resize/pin). Output schemas are inferred from them, so map fields in results are always non-nil (a JSON `null` map fails validation).
- Success: `Content` is set explicitly to one text block with a 2–4 line summary (placement/where, what to use next, reason, warnings); `StructuredContent` carries the typed result.
- Failure: the handler returns a `ToolError`; the result is `isError: true` with text `"<tool> failed [<code>]: <message>\nNext: <hint>"`, built from `v1.Error`. Errors from argument schema validation (e.g. an unknown `workload`) are produced by the SDK before our handler runs and list the allowed values.
- `dol_acquire`'s input schema adds `enum` for `workload` and `prefer` and bounds for `est_memory_gb`/`ttl_minutes`; required inputs are exactly `workload` and `est_memory_gb` (brief §8).
- `dol_list` filters by labels: `lease=<id>` for one lease, else `managed=true` + `owner=<owner>` (+ `session=<session>` unless `all_sessions`), on every docker context used by the selected leases, in parallel with a 20 s bound per context.
- `dol_stop`/`dol_remove` accept a compose project only when it is registered on the lease (`docker compose -p <p> stop|down`); otherwise the target is a container (remove also tries network, then volume) and ownership is re-verified via `inspect` labels (`dockerx.RequireOwned`).

## Consequences
- Agents get the same shape from every tool; the CLI (`dol status --json`) prints the same `StatusResult`.

## Alternatives
- Hand-written schemas via `Server.AddTool`: more control, but loses SDK validation and drifts from the Go types. Rejected.
