# Backlog (non-migration tech debt)

Cross-cutting cleanup items that aren't tied to the migration plan
(`docs/MIGRATION-PLAN.md`) or a single ADR. Keep entries small and actionable.

## Deprecation cleanup: `client.Apply` → `client.Client.Apply()` / `SubResource().Apply()`

**What:** controller-runtime deprecated the package-level `client.Apply` patch
type (staticcheck `SA1019`). All controllers currently use it for server-side
apply, e.g.:

```go
r.Patch(ctx, obj, client.Apply, client.ForceOwnership, client.FieldOwner(...))
r.Status().Patch(ctx, p, client.Apply, client.ForceOwnership, client.FieldOwner(...))
```

`make -C operator lint` flags this in `slapdcluster_controller.go` (×6),
`slapddatabase_controller.go`, `slapdschema_controller.go`, and
`slapdbackup_controller.go` — a handful of `SA1019` hits.

**Why deferred:** it's a uniform, mechanical migration to the newer
`client.Client.Apply()` / `client.Client.SubResource("status").Apply()` API
across every controller. Doing it wholesale in one focused change is cleaner
(and easier to review) than touching it inline inside unrelated feature work.
We are not ignoring it — it's parked here deliberately.

**How:** migrate every `client.Apply` call site in one pass; re-run
`make -C operator lint` to confirm the `SA1019` count drops to zero. Verify the
field-manager ownership semantics are unchanged (same `FieldOwner`,
`ForceOwnership`).

**Note:** `make -C operator lint` currently reports broader pre-existing debt
too (errcheck, gofmt, modernize, unused, gocyclo, lll — ~70 findings as of
2026-06-08). Worth a separate sweep, but out of scope for this entry.
