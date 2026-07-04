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

## Add a README to the operator Helm chart (`charts/operator/`)

**What:** `charts/operator/` has no `README.md`. The project docs
(`README.md`, `docs/BACKUP.md`, ADRs) live in the repo, so a consumer who
installs the chart from a registry (`helm install … oci://…/slaptain-operator`)
has no docs at the point of use — values, CRDs, and the backup/restore feature
set are undiscoverable from the chart alone.

**Why deferred + open question:** unclear what shape it should take. Options:
a copy of the top-level `README.md` (drifts — two sources of truth); a thin
chart-specific README (values reference + "see the project README/docs for
concepts" links); or generated from `values.yaml` via a tool like
`helm-docs`. Decide the approach before writing it — a verbatim copy of the
top-level README is explicitly *not* wanted.

**How:** pick the approach (lean: a short chart README documenting `values.yaml`
knobs + image/CRD notes, linking back to the repo docs rather than duplicating
them), then keep it from drifting (helm-docs in `make operator-manifests`, or a
CI check).

## Remove the `foreignRIDs` field + its overlap validation (RIDs are consumer-local)

**What:** `SlapdDatabase.spec.replication.foreignRIDs` (and the validation that
rejects a DB whose computed RID range overlaps it) lets a deployer declare RIDs
"in use on the other side" of an external-peer relationship, to avoid a
cross-cluster RID collision. Remove it — the collision it guards against cannot
happen. A `rid` is the *consumer's* local handle for a syncrepl directive: it
keys that consumer's own replication cookie state and is never exchanged on the
wire. slaptain and any peer/source each number their own stanzas independently,
in disjoint per-node namespaces, so cross-cluster RID coordination is meaningless.

**Why deferred / why it's inconsistent right now:** ADR-011 was rewritten to drop
the cross-cluster RID discussion (RIDs are invisible across clusters), but that
rewrite deliberately did **not** touch the operator code. So the `foreignRIDs`
field + webhook currently outlive the ADR that justified them — no ADR endorses
the field anymore. This entry is the reconciliation reminder. (Contrast:
`foreignServerIDs` **stays** — ServerIDs *are* global, embedded in the CSN and
tracked in `contextCSN`, so cross-cluster ServerID collision is real.)

**How:** drop `foreignRIDs` from `SlapdDatabase` types + deepcopy + CRD; delete
the RID-overlap validation; keep `ridBase` (that's slaptain's *own* intra-cluster
RID uniqueness — still valid and still needed). Regenerate manifests; `grep -r
foreignRIDs` → 0. Update any docs/tests that referenced it.
