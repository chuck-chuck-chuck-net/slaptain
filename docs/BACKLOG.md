# Backlog (non-migration tech debt)

Cross-cutting cleanup items that aren't tied to the migration plan
(`docs/MIGRATION-PLAN.md`) or a single ADR. Keep entries small and actionable.

## ~~Deprecation cleanup: `client.Apply` → `client.Client.Apply()` / `SubResource().Apply()`~~ — DONE (2026-09-14)

All twelve package-level `client.Apply` call sites are migrated to
`client.Client.Apply()` (six object patches in `slapdcluster_controller.go`) and
`r.Status().Apply()` — i.e. `SubResource("status").Apply()` — (six status
patches across the cluster/restore, database, schema, backup and scheduled-backup
controllers). `SA1019` is 0; the two `ldap.Dial` → `ldap.DialURL` sites in
`slctl` went with it, being a literal substitution on a plaintext
`localhost:<port>` port-forward.

The new API takes a `runtime.ApplyConfiguration` rather than a typed object.
Generated apply configurations exist for the core kinds but not for our CRDs, so
the objects are adapted through `client.ApplyConfigurationFromUnstructured` by
the one `applyConfiguration()` helper in `internal/controller/apply.go`. Both
paths send `json.Marshal` of the payload as an `application/apply-patch+yaml`
body, and `apply_test.go` pins that the unstructured form marshals to the same
JSON as the typed object — so the field set recorded for each field manager is
unchanged, as are `FieldOwner` and `ForceOwnership` (both option types implement
`ApplyToApply` / `ApplyToSubResourceApply` identically).

The broader pre-existing lint debt this entry always excluded is paid down too
(2026-10-17, #17): the operator module is at zero golangci-lint findings under an
unchanged `.golangci.yml`, so the CI gate's whole-module fallback (taken when a PR
diff is too large for the API) no longer fails on debt a PR did not add.

## ~~Three paths hand `Replace("olcSyncRepl")` a short set with no error~~ — DONE (2026-10-05, ADR-031 M4b)

`olcSyncRepl` is installed with one atomic
`modReq.Replace("olcSyncRepl", desired)`, so a set shorter than the pod's
current one is a **deletion**, not a partial update. Three paths skip a peer
and carry on, handing `Replace` a set missing that peer's stanzas:

- a peer whose bind-password Secret cannot be read;
- a discovery peer that has not resolved any addresses yet;
- `len(ep.URIs) == 0` inside the builder.

Each is "withhold means delete" in the shape RID-PLAN hazard 2 names: a
transiently unreadable Secret or a peer mid-discovery tears that peer's
cross-site stanzas off every pod, and replication for that link stops rather
than pausing. Nothing reports it — the pass is not marked skipped and the
database stays `Running`.

**Found 2026-10-03** by the ADR-031 Phase 5 conformance review on the withdrawn
branch, which verified all three exist unchanged on `main`.

**Two new triggers, 2026-10-05**, from ADR-031 M1's correctness review. Both
feed the discovery bullet above:

- A remote pod name that will not parse, or two remote pods claiming one
  ordinal, now refuse the reduction — and a refusal is published as an *empty
  provider list*, which is exactly the state the consumer already treats as
  benign.
- `discoveredAddresses` was removed with no compatibility read, so the first
  reconcile after an operator upgrade sees an unpopulated `discoveredProviders`
  and takes the same path. It self-heals on the next pass (the database watches
  the cluster), at the cost of one syncrepl session restart per cross-site link
  — which is more than ADR-031's "one `cn=config` write per pod per database"
  promises, and is an unwelcome shape while ITS#9398 is open.

Both are latent rather than live: the remote selector matches only RW
StatefulSet pods, whose names always parse and are unique. **Neither may ship:**
M1 and M4 land in the same release, and M4's hazard-2 withhold closes the window
before any of it is published. That makes the fix a release gate, not a
nice-to-have.

**Fix:** treat an unresolvable peer the way the RID gate treats an unassigned
column — withhold that pod's stanza convergence and report, rather than
converging to a set that omits it. Red-first: a cluster with two external peers,
one with an unreadable bind-password Secret, must leave both peers' stanzas on
the pod.

**Related:** the same review found `syncreplMatch` and `applySyncreplToPod` have
no test coverage at all, so the atomic `Replace` that makes this dangerous is
itself guarded only by prose.

**CLOSED by ADR-031 M4b.** All three paths now withhold the database's whole
stanza convergence and report, instead of skipping the peer: an unreadable
bind-password Secret, a discovery peer with no providers, and a peer that
resolved to no address, each with its own reason so a human is told which to
fix. The builder's `len(URIs) == 0` guard became unreachable and was converted
to an error rather than deleted, because being wrong about unreachability is
silent in one direction and loud in the other.

Two things the fix carried with it, both worth knowing. The refusal now blocks
**in-cluster** stanzas too, so a permanently broken peer stops more than its own
links — recorded in ADR-031's Consequences, with the "preserve the unresolved
peer's stanzas" refinement in this file as the follow-up. And the related gap
above is unchanged: `syncreplMatch` and `applySyncreplToPod` still have no
tests, so the atomic `Replace` remains guarded by prose.

---

## The operator writes and checks `{SSHA}` verifiers — move them to a KDF

Three places hash or verify a password as `{SSHA}` (salted SHA-1), and CodeQL
reports each as `go/weak-sensitive-data-hashing` (high):

- `generateSSHAHash` (`internal/controller/slapddatabase_controller.go`) — every
  verifier the operator writes: the data database's `olcRootPW`, the legacy
  `cn=replication,<suffix>` entry and the node-local identity (ADR-027);
- `sshaMatches` (`internal/controller/authdb.go`) — checks the node-local
  identity's stored verifier against the Secret to decide whether to rewrite it;
- `internal/backup/s3.go` — the restore preflight checks the replication password
  against the `replication-pw-hash` a backup stored in its object metadata.

`{SSHA}` is not wrong today: it is a scheme slapd verifies natively and the
documented default of `spec.ldap.passwordHash` (ADR-024), and the secrets the
operator generates are 24 to 32 random bytes, far beyond any brute force. It is
weak for a password a person chose, which ADR-027 rotation now allows.

The forward fix is a key-derivation scheme slapd can verify. `{ARGON2}` is the
obvious candidate: the 2.7.1 images already ship slapd's `argon2` module (core
since 2.7), and `golang.org/x/crypto/argon2` produces the verifier. To decide:

- module loading: `argon2` must be loaded on every pod before any verifier in
  that scheme is written, including RO pods and the `-ol26` pair (check which
  argon2 module, if any, its Debian 2.6 build carries);
- backups: artifacts taken before the change carry an `{SSHA}` hash, so the
  preflight must keep verifying both schemes;
- the default: whether `spec.ldap.passwordHash` follows, an ADR-024 amendment.
  `docs/TUNING.md` still says slaptain does not ship an argon2 module; that is
  out of date for the 2.7.1 images.

The CodeQL alerts stay open until the scheme changes; they are not dismissed.

## `ensureServerIDs` will renumber a live pod if the derivation ever moves again

`ensureServerIDs` converges `olcServerID` per pod: it compares the derived value
against the live one and `Replace`s when they differ (ADR-017). That was written
when the derivation could not move, so the Replace was a no-op for every
correctly-booted pod and only ever repaired a sid-0 pod or a pre-ADR-017 URL
list.

The serverID stride change of 2026-10-05 (ADR-028 amendment) moved it. On a mesh
upgraded across that release the operator renumbers every pod at index ≥ 1,
once, silently — and a renumbered pod's history stays filed under its old sid in
every peer's `contextCSN` vector, forever, without advancing. `docs/UPGRADING.md`
says do not do it; nothing stops someone doing it.

The gap is that the operator cannot tell "this pod booted wrong and needs
repair" from "this pod is correct and my derivation changed underneath it". Both
look like `current != desired`.

Worth considering, in rough order of cost:

- refuse to Replace a non-zero, non-URL-list `olcServerID` with a *different*
  non-zero bare integer, and report it as a condition instead — that is exactly
  the renumber case, and it leaves both legitimate repairs (sid 0, URL list)
  untouched;
- or record the derivation's generation somewhere node-local so the operator can
  distinguish the two.

Not urgent while there is one stride and no intent to change it again. It
becomes urgent the moment a second stride is contemplated — which is also when
it is too late to add cheaply.

## A scaled-away pod's mkdir Job is never reaped, only TTL'd

`reapSupersededDirJobs` is called from exactly one place — inside
`ensureDirJob`, for a pod the operator is currently ensuring a Job *for*
(`dirjob.go:109`). So the reap only ever visits pods that still exist.

Scale a cluster up and back down and the departed pod's completed mkdir Job
survives with its pod, which under ADR-018 is a **deletion lease on that pod's
PVCs**: `slapd-3-mkdir-… (succeeded) holds data-slapd-3, accesslog-slapd-3`.
Nothing reaps it, because nothing ever calls `ensureDirJob("slapd-3")` again.

**Bounded, not permanent.** `dirJobTTL` is 300 s and the file calls it "the
ADR-018 R4 crash backstop", so the lease clears after ~5 minutes. But ADR-018's
contract is that the operator reaps every Job it creates and TTL is a backstop
for a crash, not the mechanism — and for five minutes the PVCs of a scaled-away
pod cannot be deleted, which is exactly the window ADR-012 case-2 recovery
needs.

**Fix:** reap by cluster rather than by pod. When reconciling, list Jobs
carrying `dirJobLabel` for this cluster and reap any whose pod no longer exists
— the same list the per-pod reap already does, widened by one selector. Note
`reapSupersededDirJobs` is deliberately pod-label-scoped so it can never touch a
backup or restore Job; preserve that.

**Found 2026-10-05** by a manual full-gate e2e run (`E2E_BACKUP=1
E2E_SCALEUP=1 E2E_SCALE=1 E2E_RESILIENCE=1`, seed `1791221440`). It is
**pre-existing and unrelated to ADR-031** — `dirjob.go` is untouched by that
work, and both specs involved predate it. It needs two things to surface in one
suite: the ungated `scale-topology` spec scaling 3→4→3 to create and orphan the
Job, and `E2E_BACKUP=1` to enable `job_lease_test`, which the ADR-031 runs never
set. Spec order decides whether it fires at all, so it is **flaky by
construction** until fixed — a green run proves nothing here.

---

## `spec.replicas` above the RID layout's `P` has no cluster-level diagnosis

ADR-031 decision 5 says exceeding an axis is "a refusal naming which one and
what the ceiling is". The pod axis is only ever checked per *coordinate*, deep
in a stanza derivation — so a cluster scaled to 9 pods under `podsPerSite: 8`
surfaces as **every database withholding every stanza**, with an
`ErrOrdinalOutOfRange` buried in a log line, and nothing on the `SlapdCluster`
naming `podsPerSite` as the thing to change.

The refusal itself is correct and tested; it is the diagnosis that is missing,
and the symptom points at the databases rather than at the cluster that caused
it.

**Fix:** a `SlapdCluster` condition checking `spec.replicas` and the mesh's
`readReplicas` against the layout's `P`, named so the message says which to
change — the replica count or the layout. Red-first: a cluster at `replicas: 9`
under `P = 8` reports it on the cluster, not only as a per-database withhold.

Found 2026-10-05 by the M4a test author, turning ADR-031 decision 5 into cases.
It belongs to no milestone in `docs/RID-PLAN.md`.

---

## `ExternalPeer`'s "exactly one of uri, podAddresses, or discovery" is unenforced

`slapdcluster_types.go:364` documents the invariant in a comment and no CEL
rule implements it, so a peer with none of the three is admitted and fails at
reconcile instead. ADR-031 M4b makes that failure legible (`PeerAddressesEmpty`
withholds and names the peer), which is the important half — but admission is
where it belongs, and the comment currently claims a rule that does not exist.

**Fix:** an `XValidation` on the `ExternalPeer` type — per item, so it costs
nothing against the CEL budget that an unbounded list rule blew earlier.
Red-first: a peer with no mode is refused at `Create`.

Found 2026-10-05 by the M4b test author, checking whether its third refusal
reason was reachable from an admitted CR. It is.

---

## Preserve an unresolved peer's stanzas instead of withholding the whole pass

ADR-031 M4b withholds a database's entire stanza convergence when any external
peer fails to resolve, in-cluster stanzas included, because a set omitting that
peer is a deletion under the atomic `Replace`.

The coordinate makes something better possible: `ridcoord.Decode` recovers the
peer site from any stanza's rid, so the operator could read the pod's current
`olcSyncRepl`, keep the stanzas whose rid decodes to the unresolved peer's
remote slot, and `Replace` with the union — converging everything else while
deleting nothing. A permanently broken peer would then stop costing in-cluster
replication.

**Not taken in M4b** because it introduces a dependency on decoding live
`cn=config` state, and M4b lands immediately before the design's first contact
with a real slapd. Revisit after M5.

---

## A discovery refusal is invisible outside the operator log

`reconcileExternalPeerStatuses` writes a discovery error into
`status.lastError`, and `checkPeerCSNConvergence` then runs for the same peer
and unconditionally overwrites it with its own verdict — which, for a peer with
no addresses, is the generic `no remote addresses available`. The message naming
the offending pod survives only in the operator log.

Pre-existing mechanism; ADR-031 M1 is what gives it a message worth preserving,
since its done-criterion is "a malformed pod name refuses that peer **with the
pod named**" and `kubectl get sc -o yaml` does not name it.

**Fix:** do not clobber a non-empty `status.lastError` with the generic
no-addresses verdict, or give discovery its own field the CSN pass does not own.
Found 2026-10-05 by the M1 correctness review.

**This is also what blocks a finer diagnosis downstream.** ADR-031 M4b reports
every discovery failure as one reason, `PeerProvidersUndiscovered`, because
discovery publishes all five causes — first bringup, a bad kubeconfig, remote
RBAC, an unparseable remote pod name, a duplicate remote ordinal — identically,
as an empty provider list. Separating them cannot be done downstream; it has to
be carried out of discovery, which is this entry.

---

## A reinstall over surviving PVCs produces a healthy-looking, unconfigurable cluster

Deleting a `SlapdCluster` garbage-collects `<name>-config-password` (the
operator sets `SetControllerReference` on it); the PVCs survive on purpose. A
redeploy into the same namespace then mints a new `cn=config` password that
every pod rejects, because `/config` still hashes the old one. Full mechanism
and evidence in the ADR-012 amendment of 2026-09-21.

What makes it backlog-worthy rather than a documentation note: **nothing
reports it**. The operator logs `Invalid Credentials` on every pass — 358 times
in the observed case — and sets no condition, no event, no phase change. The
`SlapdCluster` says `Running`, pods are Ready, data replicates, and
`slctl inspect` indicts three downstream symptoms (zero stanzas,
`olcMultiProvider` unset, missing external stanzas) without naming the cause.
A user reaches the right answer only by reading operator logs.

Two candidate fixes, not mutually exclusive:

1. **Align the lifetimes: stop owning the Secret.** `/config` outlives the CR
   by design, and the password baked into it should too. Dropping the
   ownerReference makes a reinstall ADOPT the existing Secret — the create-only
   path already handles that — and the mismatch becomes unreachable. Cost: a
   `helm uninstall` leaves a Secret behind, so "delete the namespace" becomes
   the only complete wipe. That is already what ADR-012 recommends, and the
   Secret goes with the namespace either way.

2. **Detect and say so.** A bind that fails with err=49 against `cn=config` is
   not a transient error and never recovers on its own: report it as a
   condition (`ConfigCredentialMismatch`) with the repair, rather than looping
   silently. Cheap, and it also covers a hand-edited `cnConfigCredentials`
   Secret, which produces the same dead end by a different route.

3. **Converge it, and stop calling it create-only.** `olcRootPW` on
   `olcDatabase={0}config` is node-local and runtime-modifiable — a textbook
   ADR-024 R1 candidate — yet nothing writes it after `slaptest` renders it at
   first bootstrap (`grep` finds exactly one `olcRootPW` write in the operator,
   and it creates a *data* database). So the Secret is not merely create-only,
   it is **unreconciled**: editing it locks the operator out of every pod at
   once, with no repair path and no signal. Converging it per pod would make
   (1)'s reinstall self-heal and make rotation an ordinary operation — exactly
   what ADR-027 decision 6 did for the replication identity two metres away, and
   the same defect class it retired. One ordering trap makes this more than a
   one-liner: the operator must bind with the OLD password to write the new
   hash, so during the change some pods hold the new one and some the old.
   Try-new-then-old, the shape ADR-008's monitoring bind already uses.

(1) removes the common cause; (2) catches every other route to the same state;
(3) removes the class. Worth doing (2) regardless, because "the operator logged
it 358 times and told nobody" is the part that cost the afternoon.

---

## `lab.yaml`'s `vms:` is libvirt-specific, unread, and inside `sites[]`

`lab.yaml` began as a description of one lab for `tests/e2e.sh` and is now the
input to four user-facing bootstrap scripts. `sites[].vms` did not make that
transition well: it names libvirt domains and `qemu+ssh://` hypervisor URIs, it
is **read by nothing today** (`grep` finds no consumer — it is documented as
"planned: ITS#9580 dormancy repro"), and it sits inline in `sites[]` next to the
fields every user must fill in. A stranger reading the sample cannot tell that
four fields are required and the fifth is an unimplemented convenience for one
hypervisor.

Verified, so the awkwardness is cosmetic rather than functional: a `lab.yaml`
carrying only `sites[].{name,serverIDIndex,context,endpoint}` — no `vms`, no
`nodeAccessIP`, no `slaptain:` section — works with all four `scripts/mesh-*.sh`
and with `e2e.sh config`. Nothing requires the field.

Options, in the order they appeal:

1. **Drop it until the repro needs it.** It describes a test that has not been
   written. Re-adding it costs one commit, and the format stops promising
   something nothing reads.
2. **Nest it under a clearly-optional, clearly-scoped key** — `disruption:` or
   `platform: {libvirt: …}` — so the file separates "facts the bootstrap needs"
   from "facts only outage testing needs, on one hypervisor".
3. Leave it and document the split in the sample's comments. Cheapest, and the
   one that leaves a reader guessing which fields matter.

Whatever is chosen, the underlying rule is worth stating in the sample: this
file has two kinds of content, and only one of them is required to stand a mesh
up.

---

## The e2e fixture's founder is hardcoded `site-1`, and `e2e.sh config` claims it regardless

`tests/resources/example/database{,2}.yaml` declare `seed.site: site-1`, so the
suite requires a site literally named `site-1`. Since `lab.yaml`'s
`sites[].name` became authoritative, a lab whose sites are named anything else —
`site-a`, `ams`, `fra`, the names the documentation itself suggests — renders a
mesh in which no site matches the declared founder.

The chart catches it, which is the good half: `slapd-mesh` refuses at render with
*"database … names seed.site … which is not a site in mesh.sites"*. Verified
against a two-site `site-a`/`site-b` lab file.

The bad half is that `e2e.sh config` prints the founder as a constant:

    site identities:      kube-a=site-a kube-b=site-b (founder: site-1, per spec.seed.site)

which names a site that does not exist in that lab, in the one command whose job
is to show you the resolved configuration before you run anything.

Two candidate fixes, and the choice is a real one:

- **Validate and refuse early.** `e2e.sh` checks that every founder named by the
  fixtures exists among the site names, and dies naming both. Honest, no hidden
  rewriting, and it makes the constraint explicit — but it means the suite only
  runs on labs whose sites are named `site-1..N`.
- **Substitute when building the directory values.** The suite already
  transforms the fixture CRs into chart values; it could map the declared
  founder onto the first site's actual name. More flexible, at the cost of the
  generated values differing from the committed fixture — a transformation of
  the kind ADR-028 Phase 2b deliberately removed when `strip_seed_block` died.

Either way `config` must stop printing a founder it has not verified.

---

## `NeedsAccesslog` and `NeedsAccesslogVolume` have identical bodies

`operator/api/v1alpha1/slapdcluster_types.go:951` and `:978` are byte-identical:
`return sc.Spec.Replicas > 1 || len(sc.Spec.Replication.ExternalPeers) > 0`. Two
names for one predicate, so a change to the rule has to be made twice and a
reader cannot tell which distinction was intended.

Cost observed 2026-09-18: a reviewer mutation-testing the accesslog-mount guard
patched the first occurrence, saw the test still pass, and nearly recorded a
coverage gap that did not exist — the guard depends on the *second*. A
duplicated predicate wastes verification, which is exactly when you least want
to be misled.

Decide whether the two concepts are genuinely distinct (then the bodies should
diverge, or one should call the other) or the same (then one should go).

---

## Two cross-site transports have no e2e coverage any more

ADR-028 Phase 7 deleted the hand-wired e2e path, and with it the only coverage of
two transports the operator still supports:

- cross-site **NodePort `uri` peers** (`externalPeers[].uri`)
- **static Multus `podAddresses`** (ADR-007, no discovery)

Neither is expressible through a `SlapdMesh` — the mesh names sites and lets the
operator discover addresses, which is the whole point of the layer — so the
coverage cannot be recovered by porting the specs. A multi-site run now refuses
to start without `pod-routed` or `multus` discovery.

The product still offers both on a mesh-less `SlapdCluster`, so the options are:
drop them from the supported set (they are then dead code to delete, not debt),
or give them a small dedicated fixture outside the mesh path. Do not leave the
third state — supported, documented, untested — standing indefinitely.

Note the related gap while deciding: the `multus` branch of the mesh path itself
is written but untested; every run so far has been `pod-routed`.

---

## ~~`make -C operator lint` cannot run under Go 1.27~~ — DONE (2026-10-10)

Re-pinned to v2.12.2, the version `.github/workflows/lint.yml` runs too, so
local and CI lint by one ruleset; verified against the go 1.26 toolchain in
`operator/go.mod`. The debt itself is untouched: an uncapped run at v2.12.2
on the Go 1.26 tree reports 581 findings (goconst 407, modernize 67, errcheck
47, lll 36, gocyclo 8, unparam 7, unused 5, prealloc 3, revive 1 — the
modernize count was 28 on Go 1.25; the 1.26 rewrites added the rest). Until
the sweep, CI's gate is `only-new-issues` — it holds the line rather than
drawing it. A golangci-lint built with an older Go than the module targets
refuses to run at all ("the Go language version used to build golangci-lint
is lower than the targeted Go version"); `make -C operator lint` installs it
under the module's toolchain, so delete a stale `operator/bin/golangci-lint-*`
after a toolchain bump.


**What:** the pinned `golangci-lint` v2.7.2 cannot read Go 1.27's export data
(`export data version 4 is greater than maximum supported version 2`). It fails
with 5 bogus `typecheck` errors and never reaches the real linters, so the
target is unusable on a machine whose default toolchain is 1.27.

**Workaround in use:** run `operator/bin/golangci-lint` with `GOROOT`/`PATH`
pointed at a go1.26.x toolchain (one is already in the module cache).

**Why it matters:** the lint gate silently reports nothing useful rather than
failing loudly as a version problem, so a contributor can believe the tree is
lint-clean when the linters never ran. Found 2026-09-14 while closing the
`client.Apply` deprecation, where the before/after counts had to be produced
through the workaround.

**How:** bump the pinned golangci-lint to a release that supports the current
Go export format, and re-pin deliberately rather than floating. While there,
note that golangci caps identical messages at 3 by default — the uncapped
counts (`--max-same-issues=0 --max-issues-per-linter=0`) are the honest ones,
and the remaining debt is 128 findings (errcheck 40, lll 31, modernize 15,
goconst 13, prealloc 13, gocyclo 9, unused 4, unparam 2, revive 1).

---

## e2e framework: specs cannot provision their own topology

**What:** the Go e2e suite cannot stand up the environment it runs against. The
primary fixture — namespace, operator install, TLS, NodePort services, node
access IPs, cross-site kubeconfigs, versitygw — is provisioned by
`tests/e2e.sh` before `go test` starts, and `BeforeSuite` simply assumes it is
there. Specs *can* create `SlapdCluster` CRs (`restore_test.go`,
`restore_inplace_test.go`, `restore_replication_test.go` each do), but there is
no framework for "give me a cluster with topology X, reachable, with
credentials": every such spec hand-rolls the same boilerplate — clone images and
pull secrets from the source cluster, mirror the suffix, expose a NodePort,
read the generated password, wait for readiness — and depends on `E2E_NODE_IP`
being exported by the shell.

**Why it matters:** any behaviour that only appears in a *particular topology*
is effectively untestable without either bending the shared fixture or
duplicating that boilerplate again. It is why the whole suite leans on one
shared, mutable `slapd` cluster that ~15 specs read from and three actively
damage, and why cross-spec interference is a recurring failure mode (see the
`FAIL_FAST` / `E2E_SEED` knobs added for exactly this reason).

**Why deferred:** it is a refactor of the test framework, not a change to the
product, and doing it inline inside feature work would bury it. It wants a
deliberate design pass: what a "cluster fixture" helper looks like, whether
`e2e.sh` shrinks to bootstrapping only the operator and the S3 target, and how
per-topology fixtures are torn down without leaking PVCs.

**How:** introduce a fixture helper that provisions a `SlapdCluster` +
`SlapdDatabase` of a requested shape (replica count, replication on/off,
external peers or none, TLS or not), exposes it, returns a connected client, and
registers its own cleanup. Port the three existing hand-rolled cases onto it
first — they are the specification for what the helper must do.

### 2026-09-14: the `cleanupPolicy` spec is the newest instance, and it cost a run

`cleanup_policy_test.go` (ADR-005 coverage, landed today) needed three
throwaway `SlapdDatabase`s and had nowhere to put them, so it creates them on
the **shared** `slapd` fixture. Two concrete taxes followed, both of which a
fixture helper would have removed:

- **Cost:** every create and delete changed the StatefulSet's `DATABASE_DIRS`
  and rolled the cluster (ADR-013's accepted wart). The container paid that four
  times — ~8-10 minutes of a suite that otherwise runs in ~13. *Removed
  2026-09-28: the list moved off the pod template (ADR-013's second amendment
  that day), so the container now rolls nothing. The argument for the fixture
  helper stands on its other leg — the shared cluster is still shared.*
- **A wrong-tree failure that only a live run could catch:** the fixtures were
  first written with suffixes *under* the shared fixture's own suffix
  (`dc=cleanupdel,dc=example,dc=org` under `dc=example,dc=org`). slapd refuses a
  database whose naming context is already served by a preceding one
  (LDAP 80, "already served by a preceding mdb database"), so the databases
  could never be created and `BeforeAll` timed out after 8 minutes. The spec was
  red — for the wrong reason. A helper that owns its own cluster would have
  given the spec its own naming-context namespace and made the collision
  impossible rather than merely detectable.

Reading for whoever picks this up: the collision is *not* a slaptain rule, it is
slapd's — any fixture that invents a suffix on a shared cluster has to pick a
tree no existing database serves.

### 2026-09-15: and the restore specs pay it as a timing coupling

The restore specs read the shared fixture's DIT (their artifact is a slapcat of
it) and time the restore machine against it, so `E2E_SCALE=1` — which inflates
that fixture to 1200+ entries and, more importantly, the operator's per-tick
workload — changed how long their waits needed to be. Their fixed 8-minute
budgets failed a restore that was still progressing. Fixed spec-side by waiting
on progress instead (see "e2e needs a liveness assertion class"), but the
coupling itself is this entry's: a spec that owned its own small cluster would
not have had its budget moved by an unrelated spec's write volume.

### 2026-09-15: and teardown could not see what the specs had created

A three-site run deadlocked in `./tests/e2e.sh teardown`. An earlier suite's
in-place restore spec had failed and kept its evidence by design
(`E2E_KEEP_ON_FAILURE`), leaving a spec-created `SlapdCluster` + `SlapdDatabase`
standing — and therefore a running pod. Teardown deleted the resources *it* had
created, then ran `kubectl delete pvc --all`; a pod object naming a PVC is a
deletion lease on it (ADR-018), so those PVCs went `Terminating` and stayed
there, the namespace never drained, and — the context loop being serial — the
other two sites were never torn down at all.

The ordering fix landed in `tests/e2e.sh` (owners before storage, by `--all`
rather than by fixture name; bounded waits; a loud line naming any leftover it
destroys). But the *cause* is this entry's: teardown has no inventory of what
the specs provisioned, because provisioning is hand-rolled per spec. A fixture
helper that registered its own cleanup would have removed both the debris and
teardown's need to guess at it — the `--all` sweep is a safety net over an
unknown set, not knowledge of it. Note the second-order cost too: the sweep now
deletes kept-on-failure evidence that somebody may have wanted, which is the
right default for a teardown but a worse trade than never needing the sweep.

## Permanent e2e coverage for in-place restore topologies

**What:** `SlapdRestore` has three topologies with different meanings (ADR-014,
amendment 2026-08-24), and automated coverage exists for only one:

| Topology | Meaning | Coverage |
|---|---|---|
| `replicas: 1`, replication off | true rollback | `restore_inplace_test.go` — green 2026-08-24 |
| N≥2, no external peers | true rollback; the accesslog wipe matters here | **verified manually only** |
| mesh member (external peers) | local re-seed, mesh repairs | none — and none is wanted as a *rollback* assertion |

`restore_replay_test.go` is the spec for row 2 — it guards the accesslog-wipe fix
(`fix(restore): wipe the accesslog LMDB on restore under replication`). It
currently runs against the **shared** primary cluster, so on a multi-site
deployment it asserts rollback semantics in the one topology where they are not
promised, and fails deterministically. It needs its own N≥2 cluster with no
external peers.

**Why deferred:** blocked on the e2e framework item above — relocating it means
hand-rolling a fourth bespoke cluster, which is the duplication that item exists
to remove. Until then the spec's red on multi-site runs is *expected* and should
not be read as a product regression.

**How:** once the fixture helper exists, move the spec onto a dedicated N≥2,
no-external-peers cluster. That restores its validity, closes the row-2 gap, and
removes the most destructive spec from the shared fixture as a side effect.

## Flake: bootstrapFrom restore intermittently never sets restoreApplied

`restore [It] restores a backup into a fresh cluster via bootstrapFrom` has failed
**2 of 5 observed runs** (2026-08-25/26), always the same way: timeout after 480 s
waiting for `restoreApplied=true`.

What is established:

- `slapd-restore-0` refuses connections on 1024 for the entire window
  (`dial ...:1024: connect: connection refused`, logged by the SlapdDatabase
  controller every 10 s).
- Because the cluster therefore never reaches `Running`, the restore state machine
  **never enters preflight** — the SlapdCluster controller logs a single line for
  that cluster for the whole run, where a healthy run shows
  `entering restore (preflight)` → `preflight passed; scaling down`.
- So `restoreApplied` was never going to flip. The timeout is a symptom; the pod
  not starting is the fault.

What is **not** established: why slapd does not start. An initial theory —
orphaned PVCs from a previous run being rebound with populated `/config` and
`/data` — is **refuted**: a later session cleared the orphans before every run and
the failure still occurred once. The `slapd-ip` and `slapd-rr` restore clusters in
the same runs start fine.

Why it had resisted diagnosis: the spec's cleanup deletes the SlapdCluster and
SlapdDatabase, so by the time anyone looked there was no pod, no CR, and no pod log
left to inspect.

### 2026-09-12: the failure is now inspectable; the flake did not reproduce

Both halves of "make it inspectable" have landed in the restore specs
(`restore_test.go` hosts the helpers; `restore_inplace_test.go` and
`restore_replay_test.go` use them):

- **Autopsy at failure time** (`dumpRestoreAutopsy`, an `AfterEach` that fires only
  on a failed spec, before any teardown): SlapdCluster + SlapdDatabase status YAML,
  every pod's phase/conditions/per-container state, the `init` and `slapd` container
  logs (plus the *previous* container's log when it has restarted), and the
  namespace events for `<cluster>*`. It runs regardless of the keep setting, so even
  a run that must leave nothing behind prints its own post-mortem.
- **Keep-on-failure** (`keepOnFailure`, default **on**; `E2E_KEEP_ON_FAILURE=0`
  restores unconditional teardown): a failed restore spec leaves its cluster,
  database, backup, service and PVCs standing and prints what was kept, the
  `slctl debug-dump -n <ns> <cluster>` one-liner, and the cleanup commands.
- **PVC leak fixed spec-side** (`deleteRestorePVCs`): green runs now delete the
  `volumeClaimTemplates` PVCs by `app.kubernetes.io/instance`. Operator behaviour is
  untouched — ADR-005's Retain default stands.

Verified by mutation (the tooling's red): the `restoreApplied` timeout was
temporarily cut to 6 s so the spec failed exactly the way the flake does — with
`slapd-restore-0` not yet serving. The autopsy printed the full init-container log,
the pod's container states, 32 events, and the keep banner; the cluster was still
standing afterwards. Mutation reverted.

**Hunt: 16 consecutive green runs, no reproduction** (scm 3-site lab, build
`fd9b420`, OpenLDAP 2.7.1): 10 runs of the bootstrapFrom spec alone
(`E2E_LABEL_FILTER=restore-bootstrap`, ~40 s each) and 6 runs of the whole
`restore` label group (~172 s each, 4 specs, replay skipped as expected on a mesh).
Every run left zero `slapd-restore*` / `slapd-ip*` / `slapd-rr*` objects behind.
So: unreproduced on *this* substrate, which is fast enough that the whole spec
finishes in 37 s against a 480 s timeout — i.e. the failing runs were roughly an
order of magnitude off normal, not marginally slow.

Still **not established**: why slapd does not start. Candidate mechanisms to check
the next time it fires, now that the evidence survives — in the order the autopsy
answers them:

1. Pod never scheduled or stuck mounting (events: `FailedScheduling`,
   `FailedAttachVolume`, `FailedMount`). The lab's storage is node-local RWO
   (`openebs-lvm`), and ADR-018's deletion-lease rule means any pod object still
   naming one of the three PVCs pins the volume to a node.
2. Init container looping or exiting non-zero (init log + `state=terminated(exit=…)`).
3. slapd crash-looping (`restarts>0` plus the *previous* container log — the reason
   a slapd that dies at startup leaves an empty current log).
4. Pod healthy but unreachable, i.e. a service/DNS/routing fault rather than a slapd
   fault (pod `Ready=True` while the SlapdDatabase reports `NoPodsReachable`).

Note that (4) is the normal *transient* state for the first ~10 s of every run — the
SlapdDatabase legitimately passes through `phase=Error`/`NoPodsReachable` before the
pod serves. The flake is that state persisting, not its appearance.

---

## Cross-site recovery after a pod-IP change: the bottleneck is downstream of discovery

### 2026-09-28: it is also the dominant term in e2e wall-clock, and it varies 50x

Two full three-site runs an hour apart, same code, same gates, same 98/99:
**953 s and 1352 s**. The 42 % swing is almost entirely this entry. Per-spec:

| spec | run A | run B |
|---|---|---|
| resilience, simultaneous restart of all pods | 5 s | **4m20s** |
| resilience, one pod restarted | 5 s | **45 s** |

4m20s is inside the 130-268 s band this entry already records. The *5 s* readings
are the anomaly: they mean Kubernetes happened to give the pods back their
previous IPs, so no peer had anything to rediscover. Nothing in the fixture
controls that, so any single measurement of "how long does a restart cost" is a
coin flip between ~5 s and ~5 min.

Two consequences worth keeping:

- **A runtime regression hunt should start here.** The obvious suspects for a
  suite that got 400 s slower — a new probe, a new condition, an added wait —
  were all wrong; the answer was IPAM luck in one spec. Comparing per-spec
  `• [N seconds]` lines finds it in a minute, where comparing totals does not.
- **A live demo should not restart a pod and expect cross-site convergence.**
  On the unlucky side of that coin it is five minutes of nothing happening.

A third, smaller confound from the same comparison, unrelated to this entry:
Ginkgo's seed changes the spec order, and `job-lease`'s "no terminated pod holds
a slapd PVC" check landed straight after the `replicated restore` specs in one
run (90 s waiting for their Job pods to be reaped) and after a quiet spec in the
other (0.02 s). Both pass; the budget is doing its job. It is a reminder that
this suite's specs share one cluster and its timings are not independent.


Supersedes the earlier item "Peer address discovery has no vote in the requeue
decision", whose premise **measurement refuted**.

Under ADR-016 pod-routed transport, replacing a pod invalidates every peer's syncrepl
addresses and recovery takes minutes. The theory was that peer address discovery rides
on the 60 s `csnCheckInterval` and cannot ask for anything faster. That was
implemented — a 15 s discovery cadence decoupled from CSN monitoring, plus a 3 s
settle-tightening while the observed address set is still changing — and measured on
three pod-routed sites:

| build | full site restart | single pod |
|---|---|---|
| baseline | 146 s, 353 s, 308 s | 31 s, 6 s |
| decoupled cadence | 189 s, 360 s, 145 s | 75 s, 34 s |

Indistinguishable, with the mechanism verifiably firing (15 s reconcile spacing,
discovery-only passes logged). Reverted (`e692190`); implementation preserved in
`11dc2c4` if it is ever wanted as a component of a real fix.

A decomposition trace of one full-restart recovery:

    t+7s    siteA Ready with new pod IPs
    t+148s  siteB's status carries all the new IPs
    t+289s  a siteA write is visible on siteB
    t+432s  siteB's syncrepl stanzas name the new IPs

**The dominant term is downstream of discovery and is still unidentified** —
stanza-rewrite scheduling, or slapd's own consumer reconnect. The trace is also
internally inconsistent (stanzas appearing to be rewritten *after* the write was
already replicating), and the probe that produced the stanza timing was unreliable:
siteB names one provider per peer, so an "all IPs present" check can never pass.

**Next step for whoever takes this: instrument the stanza-rewrite path, do not tune
another interval.** Specifically, timestamp (a) the `SlapdCluster` status write
carrying new addresses, (b) the `SlapdDatabase` reconcile that consumes it, (c) the
`olcSyncRepl` modify landing on each pod, and (d) the consumer's first successful
connection. Note the baseline spans 146-353 s, so any claim of improvement needs
distributions rather than single samples.

Worth checking early, because it would explain the gap: the `SlapdDatabase`
controller's requeue is 10 s only on *unmet preconditions*, and a pod whose database
reconcile fails is skipped for stanza writes entirely (the ADR-019 R8 stanza-deferral
fix). During a mass pod replacement, pods legitimately fail that reconcile for a
while — so stanza rewrites may be deferred for reasons unrelated to discovery.

---

## SlapdDatabase and SlapdSchema need an explicit periodic resync

Prerequisite for any watch filtering, and a gap in its own right. See the ADR-002
amendment and `docs/reconcile-loop-fixes.md`, both 2026-08-26.

Neither controller requeues on its success path, so neither re-examines per-pod
`cn=config` on any schedule. The resync that exists today is **incidental**:
`checkPeerCSNConvergence` stamps `lastChecked` every 60 s, that changes the
`SlapdCluster` status, and the unfiltered watch re-reconciles every database in the
namespace. Everything that converges without a CR change rides on it — drift, hand
edits, re-applied ACLs.

*(Amended 2026-09-14: the ADR-019 R8 migration used to be the fourth item on
that list and is gone with R8. This does **not** unblock the watch filter — the
reverted `5dd5e3f` failed four specs, three of which had nothing to do with R8,
and the remaining three reasons are unchanged.)*

Two things follow:

- **Filtering that watch is unsafe until the resync is explicit.** Already attempted
  and reverted: a predicate faithful to the four fields the controller reads failed
  four e2e specs.
- **A cluster with `replication.enabled: false` has no periodic resync at all**, since
  the `SlapdCluster` controller only requeues periodically when replication is
  enabled. Standalone-cluster drift is corrected only if some unrelated event fires.
  Nobody intended this and no e2e covers it.

The work: give both controllers their own `RequeueAfter` sized for drift correction
(independent of `csnCheckInterval`, which exists for CSN monitoring), add an e2e that
proves drift is corrected without any CR change — hand-edit an ACL on one pod, wait,
assert it is restored — and only then revisit the watch predicate. That e2e is the
piece with real value: it would have caught the reverted change directly rather than
via four unrelated failures.

---

## Cross-site replication health is not readable from one site

Not a defect — a documentation and tooling gap, recorded so nobody builds
monitoring on a false assumption. See the ADR-008 amendment of 2026-08-26.

`status.externalPeerStatuses` is **consumer-side**: it describes this site's
*inbound* links. Because delta-syncrepl is pull-based and a provider keeps no
consumer registry, a provider cannot observe that a consumer stopped consuming from
it. So if B stops consuming from A, that is visible on **B**, and A correctly reports
`Synced` at the same time. Reading mesh health means reading every site's CR.

Two follow-ups worth considering:

- **`slctl` has no cross-site view.** `slctl inspect` is single-cluster. Something
  that takes several contexts and joins each site's peer statuses into one mesh
  verdict would turn "read four CRs and correlate by hand" into one command. This is
  the natural home for the join, and it needs no operator change.
- **Lag is only observable under traffic.** `csnSyncThreshold` is 5 s, so a broken
  link shows as `Lagging` promptly *while writes flow*; on an idle database both
  sides sit at the same CSN and the state reads `Synced` across an arbitrarily broken
  link. Heartbeat writes would close that, and per the ADR-008 amendment of
  2026-09-11 would also close a real gap: idleness lets a serverID go dormant,
  which is what makes `syncprov`'s minCSN fallback pick a stale lookup key and
  trigger the ITS#9580 "sync cookie is stale" full-refresh storm on reconnect
  (see `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`).
  They stay deferred anyway: the OpenLDAP 2.7 upgrade (ADR-021) and the syncprov
  sessionlog's per-SID viability check (ADR-022) act on that defect directly, and
  the objection that the operator would be writing into the user's data tree
  still applies to a heartbeat, which would only mask it. This is the second
  time the system has turned out to depend on periodic activity nobody
  deliberately generates — the first was the incidental 60 s-tick resync
  found in the ADR-002 amendment (2026-08-26).

---

## e2e cannot pin an old `slapd-init` image, so one migration failure mode has no guard

The 2026-08-25 "syncrepl stanzas written to a pod whose accesslog DB does not
exist" fix is proven only by hand-run live scenarios. A permanent regression test
needs pods whose init container predates the per-database accesslog directory
creation, because the missing thing is a *directory on a PVC the operator cannot
touch* (ADR-018) — unlike a `cn=config` shape, which a spec can manufacture by
hand. (The gated ADR-019 R8 migration spec did exactly that trick; it was
removed with R8 on 2026-09-14, and manufacturing a `cn=config` pre-state against
a live operator is part of what went wrong there — see ADR-026 C1.)

What is missing is a fixture capability: deploying a cluster with `spec.images`
pinned to a chosen older init tag, then upgrading the operator underneath it.
That overlaps with "e2e framework: specs cannot provision their own topology"
above and should probably be solved with it rather than separately.

---

## Cross-site orchestration (hub-and-spoke) — explicitly undecided

**What:** automating the mesh-wide rollback runbook (`docs/BACKUP.md`, "Rolling
back a replicated deployment") so it becomes one declarative action instead of a
careful manual sequence across N clusters.

**Why this is not just a feature:** it requires one actor to drive *other*
clusters — quiesce them, run restores there, and sequence the result. Today the
operator reaches across clusters only to **read**: it discovers peer pod
addresses through a remote kubeconfig (ADR-007 amendment, ADR-016). Driving a
remote site is categorically different. It introduces a control plane, and with
it hub failure, partition behaviour, and arbitration — and it stands in direct
tension with this project's second architectural requirement, that each site be
autonomous.

**Why deferred, and deliberately undecided:** this is a fundamentals question,
not a backlog chore. It is recorded here so the option is not lost, *not* as an
agreed direction. The current answer — a human-operated distributed runbook — is
legitimate and is how comparable systems document the same operation. Nothing
about it is broken; it is manual.

**If pursued:** it gets its own ADR, deciding the topology (a control-plane
cluster? a peer-elected coordinator? a CLI-driven sequence with no new
controller?) and the failure semantics before any code. See the ADR-014
amendment (2026-08-24), section "Not decided: hub-and-spoke orchestration".

---

## ADR-022 follow-ups: the persistent sessionlog source

**The checkpoint half is DONE (2026-09-13).** `olcSpCheckpoint` now converges on
every reconcile alongside `olcSpSessionlog`, off a single search of the syncprov
overlay; the `planSessionlog` seam did generalize, as `planOverlayAttr`. See the
R4-debt entry above.

Still open: `syncprov-sessionlog-source` (the persistent, accesslog-backed
sessionlog) was rejected in ADR-022 because it reads the change journal — the
artifact that is contaminated or purged in exactly the scenarios where a
persistent log would pay off. Revisit once the journal's trustworthiness across
refresh/purge transitions is settled (upstream ITS#9580 work, ADR-021).

---

## ridBase cross-CR uniqueness is documented but not enforced

`spec.replication.ridBase` must be unique across the SlapdDatabase CRs of one
cluster (colliding values produce colliding RIDs, which corrupts per-consumer
cookie state). The field's doc comment used to claim "the operator validates
this" — it never did; the only admission rule is the CEL presence check. Found
2026-09-12 while retiring the foreignRIDs entry.

The fix is a reconcile-time check in the SlapdDatabase controller (list sibling
SlapdDatabases of the same cluster, refuse with phase=Error on a ridBase whose
stanza range overlaps another's), plus a red-first e2e: two databases with the
same ridBase, assert the second reports the collision instead of writing
colliding stanzas. A webhook would also work but the project has none — do not
grow one just for this.

---

## A version-crossing image change on a populated cluster fails without explanation

Editing a populated 2.6 cluster's `spec.images` to the 2.7 pair (or back)
crashloops every pod on volumes the new slapd cannot open (LMDB 1.0 format
break, ADR-021). The operator neither refuses the change nor explains the
failure — the user gets CrashLoopBackOff and has to find the runbook.

Honest constraints: image tags are free-form (nothing reliable marks a tag as
"2.6" or "2.7"), and ADR-018 means the operator cannot inspect the volumes. So
a hard admission guard is likely impossible without new API surface (e.g. an
explicit `spec.images.generation` the deployer asserts). The realistic minimum
is observability: when pods crashloop after an image change, surface the slapd
startup error (the LMDB version complaint is in the container log) into a
SlapdCluster condition with a pointer to docs/OPENLDAP-VERSIONS.md. Decide the
shape before building; do not grow a webhook for this.

---

## `kubectl rollout status` returns before readiness on slaptain StatefulSets

Observed during the v0.1.0 demo rehearsal: `kubectl rollout status
statefulset/slapd` completes in ~0.1 s with "partitioned roll out complete"
while the pod is still ContainerCreating — the documented wait gates were
decorative. The controller sets no `updateStrategy` explicitly, so the
partition involved is API-server defaulting; whether kubectl's partitioned
branch (which checks `updatedReplicas`, never readiness) should trigger on a
defaulted partition of 0 needs root-causing before blaming anyone. Fix
Discipline applies: reproduce, read the kubectl branch condition, then decide
whether the operator should set an explicit strategy or the docs should simply
never use `rollout status` as a readiness gate (the demo now waits on
`status.readyReplicas`, which cannot lie).

### 2026-09-16: `readyReplicas` *can* lie, and did

The parenthesis above is wrong: the slapd container has no probes at all, so
`status.readyReplicas` is no more trustworthy than `rollout status` was.
Promoted to its own entry — see "The slapd container has no `readinessProbe`"
below.

---

## ~~The slapd container has no `readinessProbe` — `readyReplicas` can lie~~ — DONE (2026-09-28, ADR-030)

*Promoted 2026-09-27 out of a subsection of "`kubectl rollout status` returns
before readiness", where it was easy to miss. The finding itself is 2026-09-16.*

The slapd container defines **no `readinessProbe` and no `livenessProbe`**, so
"Ready" means only that the process exists. A slapd frozen for 874 s — accepting
TCP, answering nothing, logging nothing — held `readyReplicas: 3`,
`phase=Running` and its place in the Service endpoints for the entire freeze
(see "The `cn=config` pause freeze" below). Every wait built on `readyReplicas`,
in the demo and in e2e, inherits that blindness.

**Why it was never urgent, which is the reusable part.** ADR-013 (2026-05-14)
asked the *hard* question — should readiness mean "slapd has converged data"? —
and deferred it for reasons that still hold (what "converged" means; the
implementation surface). The easy question — "does slapd answer LDAP at all?" —
has none of those problems and was never asked separately. It rode along in the
deferral of the ambitious one. That is how a trivial guard goes missing for four
months: not by being rejected, but by being bundled.

**Wanted:** a `readinessProbe` doing a real rootDSE query, with a
`failureThreshold` sized well above a normal sub-second `cn=config` pause so
legitimate config writes never flap pods out of the Service, and **no**
liveness probe — restarting a pod mid-freeze would invalidate every peer's
syncrepl addresses again under ADR-016, turning a recoverable stall into a
topology event. Sizing that threshold is the whole design question and wants
measured pause durations, not a guess.

**Four constraints to design against:**

1. **`publishNotReadyAddresses` is not set on the headless Service** (verified in
   `reconcileHeadlessService`; it defaults false). Every per-pod operation the
   operator performs resolves `<name>-<ordinal>.<name>-headless…`. A NotReady pod
   leaves that DNS, so the operator cannot dial it, cannot converge it, and
   ADR-027's gate then withholds syncrepl site-wide. ADR-013 already named the
   fix and it never shipped, because the probe it was written for never shipped.
   Land it **on its own, before the probe**: today nothing is ever NotReady, so
   it is a literal no-op, and shipping it in the same commit as the probe is how
   the deadlock gets discovered the hard way.
2. **`OrderedReady` turns a wrong probe into "no cluster at all."** Ready is
   trivially true today, so the StatefulSet always proceeds. With a probe, a
   pod-0 that never goes Ready means pod-1 is never *created* — and the operator
   cannot help, because there is no pod to converge.
3. **Distroless has no exec probe.** kubelet offers HTTPGet, TCPSocket, Exec.
   slapd speaks no HTTP; TCPSocket is what we have and is *proven* insufficient
   (the frozen pod accepted TCP throughout); Exec needs a binary in the image —
   the cost of ADR-013's option C', resurfacing, and the second item in the class
   `BUG-ANALYSIS-database-dirs-rolling-restart.md` opened. `spec.readinessGates`
   driven by the operator avoids touching the image and reuses an observation it
   already makes every 60 s, but an unset gate defaults False, which is
   constraint 2 wearing a different hat. Rule it out on the record, not by
   omission.
4. **Any LDAP answer is success**, `err=50` / `err=32` included: anonymous access
   to the rootDSE may legitimately be denied by ACLs (ADR-025 treats uniform
   invisibility as normal, not as divergence). Only timeout and
   connection-refused are failures. What is measured is "the server answers",
   never "the server answers *me*".

**Free first step, independent of all four.** The operator already makes this
observation and throws it away: 106 consecutive bind timeouts against the frozen
pod, logged at INFO, reaching nobody. Promoting that to a status condition does
not fix Service endpoints, but it makes the failure *visible* — most of what was
missing — at zero risk to bootstrap.

**Treat the probe as a replication-path change** and run one multi-site cycle
before merging. Specs waiting on `readyReplicas` will start genuinely waiting
instead of passing spuriously; that is the point, and it will surface latent
timing assumptions.

### 2026-09-28: shipped as ADR-030, and the hard constraint dissolved

All four constraints survived contact, and constraint 3 — "distroless has no
exec probe" — turned out to be cheaper than written: the slapd image's build
stage **already installs `ldap-utils`** from the vendored fork (ADR-021), so a
version-matched LDAP client was one `cp` away. No Go stage, no repo-root build
context, no resurrection of ADR-013's option C'.

`ldapwhoami`, not `ldapsearch`, and that turns constraint 4 from a requirement
into a non-problem: WhoAmI is an extended operation that reads no entry, so the
probe cannot be broken by a user's ACLs at all. There is nothing to special-case
about `err=50` / `err=32` because they cannot arise. (`ldapsearch` would also
have needed exit-code post-processing that a shell-free image cannot do.)

Constraint 1 landed **alone and first**, as this entry insisted, while it was
still a literal no-op. Constraint 2 is answered by a 300 s `startupProbe`, so a
slow LMDB open is waited out rather than turned into a cluster that never forms.
`readinessGates` are ruled out on the record in ADR-030 rather than by omission.

Thresholds: startup 300 s, readiness 60 s. The 60 s is reasoned from both ends
(well above a routine sub-second `cn=config` pause, far below the 874 s freeze).
The 300 s is a **guess** — see the big-DIT entry below, whose fourth consumer
this now is.

**The "free first step" shipped too**, as the `PodsResponsive` condition on
SlapdCluster. Same readings the 60 s CSN tick already takes, so no extra
queries; the same answer/silence rule as the probe, so a bind rejection is not
mistaken for a dead pod; and a pod unreadable on one database but answering on
another stays responsive, because that case already has a reporter
(`CSNQueriesIncomplete`). The condition's `lastTransitionTime` is the duration
the 106 log lines never gave anyone.

---

## The `cn=config` pause freeze: mechanism known, trigger unreproduced

**What:** any write to `cn=config` pauses the whole server until every other
threadpool task is idle (`slap_pause_server` → `ldap_pvt_thread_pool_pause`,
`libraries/libldap/tpool.c:1304-1307`). A task blocked on a silent peer
therefore freezes the entire slapd for its own duration, and the operator
issues those writes *precisely* when a peer has become unreachable, because
ADR-016 stanzas name pod IPs. Measured once, on two sites in the same window:
**874.016 s** and **864.310 s**, each ending with the completion of the
operator's own `MOD olcSyncRepl`.

**What is NOT known:** what held a task that long. Four targeted reproductions
on a three-site lab all completed their config write in milliseconds — graceful
mass pod delete, permanent peer removal, a no-RST cross-site blackhole, and the
same blackhole applied mid-refresh with a 6000-entry transfer in flight. They
share one property: the consumers were idle, and an idle persist-phase consumer
polls with `tout={0,0}` and yields, holding no task. So routine pod churn does
not arm this. The state that does is a consumer blocked *mid-operation* when its
peer goes silent; producing that deliberately is the open problem.

**Why it matters anyway:** while it lasts, the pod serves nothing, Kubernetes
cannot see it (no probes), and the ADR-027 gate turns it into a site-wide
replication stall (next entry).

**How to pick it up — instrument before theorising.** Land the liveness
assertion class first ("e2e needs a liveness assertion class" above): parse
`etime` from slapd stats logs and assert no all-pods-silent gap during
convergence. The next occurrence then arrives with evidence instead of
inference. Two instrument traps cost three wrong readings during the original
investigation and will cost them again: `kubectl logs --since` reads **only the
current file**, and these pods rotate every ~90 s under `logLevel: 16640`
(use the node's `/var/log/pods/.../0.log*`); and an `ipBlock`-only NetworkPolicy
denies **in-cluster** pod traffic too, because Cilium matches pod sources by
identity rather than CIDR (add a `namespaceSelector: {}` allow rule).

**Explicitly rejected until it is reproducible:** shortening the shipped
`keepalive` default from `240:3:30`. The 874 s observation sits just under the
e2e fixture's non-default `300:10:60` bound (900 s), which is suggestive
arithmetic and nothing more. Full reasoning in the ledger entry.

---

## ADR-027's identity gate is all-or-nothing per site

**What:** `slapddatabase_controller.go:302-320` withholds syncrepl configuration
while *any* RW pod is missing its node-local identity entry. The invariant is
sound — point a consumer at a provider lacking the entry and the bind DN does
not resolve there, so it fails with `err=49` forever — but the scope is wider
than the invariant needs.

**What it cost, measured:** one wedged pod (unable to answer the identity probe)
withheld **every stanza for every database on every pod at that site**, for the
whole 874 s freeze. The site therefore never repointed its external stanzas at a
peer's new pod IPs, and the e2e cross-site recovery spec failed at its 480 s
budget. The freeze was the cause; the gate's scope is what turned one sick pod
into a site-wide replication stall.

**Wanted:** withhold only the stanzas that *name* an unconverged pod. A stanza
pointing at a healthy provider is not made unsafe by a third pod's missing
entry. That preserves the ordering guarantee the cutover rests on while
removing the amplification.

**Note it is a guard narrowing, not a bug fix:** the gate does what ADR-027 says
it should. Changing it needs an ADR-027 amendment recording why the narrower
scope still satisfies the cutover's ordering property, and what an operator
should see instead (today the withholding is an INFO line and nothing surfaces
on the CR — arguably the worse half of this entry).

---

## Rotating `replication-password` does not revoke the leaked credential

**What:** `ensureReplicationUser` is create-only — it searches for
`cn=replication,<suffix>`, finds it, and returns without comparing anything
(`slapddatabase_controller.go:2013`). ADR-027 made the *node-local* identity
converged, so a rotation of the Secret's `replication-password` does move
`cn=repl-<db>,cn=slaptain-auth` and every stanza's `credentials=` (measured 52 s
across four pods). The legacy entry is not touched: it keeps the **old**
password, and ADR-027 steps 2+3 deliberately keep its `olcAccess` read-all and
its `olcLimits` exemption on both the data DB and the journal.

**Consequence:** rotation today is hygiene, not containment. If the password
leaked, the leaked value still binds as `cn=replication,<suffix>` afterwards,
with read over the whole data tree *and* over the accesslog — which is the
ADR-020 bypass surface (an anonymous read of a shared journal once returned
`reqMod: userPassword:+ {SSHA}…` for a user the data DB denies; the legacy
identity is explicitly granted there). There is no revocation path that the
operator performs.

**Why it is not the DONE item above:** that entry closed the two-store
*drift* problem and records this window's caveats as an availability concern —
"rotating the Secret mid-window strands the create-only legacy entry", i.e. a
not-yet-upgraded peer locks out. The security reading is sharper and is written
down nowhere: the stale copy is not merely useless, it is a live credential the
rotation was supposed to kill.

**The real fix is already scheduled:** ADR-027 migration step 4 drops the legacy
grants, which makes the stale entry inert (deleting the entry itself stays a
manual cleanup — ADR-026 R2, it is replicated state the operator does not own).
Until that release lands, an incident cannot wait, so what is missing is the
interim answer.

**Options considered:**

1. **Converge the legacy entry too** — a small change to `ensureReplicationUser`,
   and rejected: it writes into the replicated customer tree on every pass,
   which is exactly the shared multi-writer state ADR-026 R2 forbids and half of
   why ADR-027 moved the identity out.
2. **Runbook** (preferred): rotate the Secret, then delete or repair
   `cn=replication,<suffix>` by hand, accepting that un-upgraded peers break
   until they upgrade. Matches the ADR-019 R8 precedent — a manual runbook in
   preference to an unsafe automated write.
3. **Make step 4 conditional** on observing that every peer is upgraded. The
   operator cannot see a remote site's auth database, so cross-site this
   collapses back into (2).

**Wanted:** an ADR-027 amendment stating the leaked-credential case explicitly
(rotation ≠ revocation while the legacy grants stand), plus the (2) runbook
written down where an operator under time pressure will find it. Step 4 then
closes it for good.

---

## Designed, deferred: opt-in `SlapdBackup.spec.requireConverged` with a bounded wait

A backup always `slapcat`s pod-0, so on a replicating cluster that is briefly
behind, a write ACKed elsewhere can legitimately be missing from the artifact
(demonstrated 2026-09-12; ADR-014 amendment of that date). That amendment fixed
the *silence*: every backup now records `status.sourcePod`,
`status.sourceContextCSN` and a `SourceConverged` condition. It deliberately did
**not** add a gate.

The opt-in gate is designed and parked here until someone asks for it:

- `spec.requireConverged: true` (never default) plus `spec.convergenceTimeout`;
- while the target `SlapdCluster` reports `ReplicationConverged != True`, the
  backup stays `Pending` with a reason saying what it is waiting for;
- on timeout it **takes the backup anyway** and records that it waited and gave
  up. A backup that declines to run is a backup you do not have; "requireConverged"
  must never become "no artifact exists".

Rejected outright, and not to be revisited without new information: a strict
CSN-dominance gate (refuse unless the source's CSN vector dominates every
peer's). A replica of a heavily-written provider never dominates, and under
multi-master two vectors can be mutually incomparable — the predicate is both
unusable and ill-defined.

Also unbuilt, cheap, and independent: stamping `sourcePod` + the converged flag
into the S3 **object metadata** at upload, so a restore can surface them without
the CR. `backup.Upload` already accepts a metadata map; what is missing is a way
for the Job's uploader to carry arbitrary pairs (`manager backup-upload
--meta k=v`).

---

## Scale and operations tunables — from the production-config review, 2026-09-12

A review of slaptain's generated `cn=config` against a large production OpenLDAP
platform produced 18 findings. The five that break a large cluster outright are
have **landed** (2026-09-12); **ADR-024 fixes where each of these lives**
(converged per pod / bootstrap-time / create-only-by-nature, and which CR owns
it), so the entries below record the gap and the severity, not the design. Pick
any of them up by reading ADR-024's placement table first — the class is already
decided.

Already done, listed so nobody re-raises them: the replication identity's
`olcLimits` exemption on the data and accesslog databases (ADR-020 amendment —
this was a silent 500-entry replication cap), `olcDbMaxSize` on both with
operator defaults and a converge-on-change path that refuses a shrink out loud,
per-database `sizeLimit`/`timeLimit`/`limits`, the `entryCSN`/`entryUUID`
baseline indices on data databases, and `spec.backend.idlExponent`. The
many-entries fixture class ADR-024 asks for is `tests/e2e/scale_test.go`, gated
`E2E_SCALE=1` — every one of the five is structurally invisible without it.

**Landed 2026-09-13** (the degrades-and-hygiene half): `olcDbCheckpoint`,
`olcDbNoSync` (now converged and inheriting a cluster-wide
`spec.tuning.noSync`), `olcDbRtxnSize`, `olcDbEnvFlags` (create-only and
reported), the syncrepl stanza timeouts and a default keepalive,
`slapadd -q` + `spec.tuning.toolThreads` for restores,
`spec.ldap.tls.{protocolMin,cipherSuite}`, `spec.ldap.passwordHash`, and a
`logLevel` default of 16640. Guarded by `tests/e2e/tunables_test.go` (ungated).
Three of the review's items did NOT survive contact with a live cluster and are
re-entered below with their evidence.

- **Connection lifetimes cannot be converged — `olcIdleTimeout` and
  `olcWriteTimeout`** (degrades; was the urgent half of the review's finding
  10). slapd's default for both is *never close*, so a pod behind a stateful
  firewall accumulates connections whose peer is long gone until it runs out of
  file descriptors. The gap is real. The **placement was wrong**: an
  `ldapmodify` of either attribute against a running OpenLDAP 2.7.1 does not
  fail and does not crash — **it hangs the server**. The modify's CSN is queued
  and never graduates, the pod then answers nothing at all (not even an
  anonymous rootDSE), and `SIGTERM` sticks in `slapd shutdown: waiting for 2
  operations/tasks to finish`:

  ```
  conn=1050 op=2 MOD dn="cn=config"
  conn=1050 op=2 MOD attr=olcIdleTimeout
  slap_get_csn: conn=1050 op=2 generated new csn=…
  slap_queue_csn: queueing …
  <nothing, ever>
  ```

  Reproduced twice on a healthy three-pod cluster (three of four pods the first
  time, all three RW pods the second); recovery was restarting every pod. The
  mechanism is the daemon thread: a non-zero `global_idletimeout` arms
  `connections_timeout_idle`, which walks the connection table from the daemon
  loop while the modify that armed it is still executing inside one of those
  connections. Both values are **fine when they come from the boot config** — a
  pod that *started* with `olcIdleTimeout 3600` / `olcWriteTimeout 300` served
  normally for twenty minutes. *Surface:* ADR-024 **R2**, bootstrap-time —
  `idletimeout`/`writetimeout` directives in the init container's generated
  `slapd.conf`, with R2's documented recreate path. No CRD field until then: a
  field the operator cannot honour is what R4 forbids.
- **Thread and buffer counts** (degrades, unmeasured). `olcThreads`,
  `olcListenerThreads`, `olcConcurrency`, `olcSockbufMaxIncoming`,
  `olcSockbufMaxIncomingAuth`, `olcConnMaxPending` are neither set nor exposed.
  Upstream's defaults are defensible and the reference production platform
  leaves them alone too, so these wait for a measurement that asks for one.
  `spec.tuning` is the home when one does.
- **No `cn=monitor`, no native metrics** (hygiene; *attempted 2026-09-13 and
  withdrawn*). The monitor backend module is not loaded, no monitor database
  exists, `olcMonitoring` is never set — so there are no per-database operation
  counters and nothing an exporter can read. Monitoring today is the operator's
  CSN polling, which by ADR-008's own amendment cannot see an idle-but-broken
  link.

  An implementation landed and was reverted the same day. Individually every
  piece works on a running 2.7.1 pod: `olcModuleLoad: back_monitor` is accepted,
  `olcDatabase=monitor` can be added live, `cn=Monitor` answers immediately, and
  an ACL granting read to the existing `cn=replication,<suffix>` identity
  (ADR-008 reuse) enforces correctly — anonymous gets `No such object`. What
  does **not** work is converging it: with a monitor database present, an
  `ldapmodify` of its `olcAccess` hangs slapd with the same signature as the
  connection timeouts above (CSN queued, never graduates, pod dead to all
  clients), observed on three of four pods. The operator rewrote that ACL every
  reconcile because its comparison of what it wrote against what slapd stores
  never matched — so steady state was one modify away from bricking the cluster.

  *Surface when picked up again:* fix the ACL comparison FIRST and prove the
  operator reaches a no-write steady state, then establish whether an `olcAccess`
  modify on a monitor database is safe at all. Until both are answered this is
  not a hygiene feature, it is an outage generator. The e2e assertions (monitor
  database present per pod; readable by the replication identity; denied
  anonymously) are worth restoring from commit `07b89af`'s parent.
- **TLS: no CRL policy, no `olcLocalSSF`/`olcSecurity`** (hygiene; the protocol
  floor and cipher policy landed 2026-09-13 as `spec.ldap.tls.protocolMin` /
  `.cipherSuite`). `olcTLSCRLCheck` and a minimum-SSF statement are still
  unstated, so a revoked peer certificate is accepted and there is nothing
  forcing a client onto TLS. *Surface:* the same converged block.
- **A stronger password-hashing scheme is not available** (hygiene; the policy
  field landed 2026-09-13 as `spec.ldap.passwordHash`, pinned to `{SSHA}` and
  converged). The module-availability question the review asked is answered:
  slaptain's runtime image does **not** ship `pw-argon2`, so `{ARGON2}` cannot be
  selected today — asking for it would produce a slapd that rejects every
  password write. Nor is `olcPasswordCryptSaltFormat` exposed. *Surface:* build
  the module into `images/slapd/Containerfile` first; the CRD field is already
  there to point at it.
- **Overlay surface beyond replication** (hygiene, document-only). We manage
  `syncprov` and `accesslog` and nothing else; the production platform also runs
  password-policy, uniqueness, dynamic-list and member-of overlays plus
  last-bind tracking. Not a scale defect — it is the difference between a
  replicating store and a directory an application stack can sit on. Worth a
  stated position (explicitly out of scope, or an ADR for a generic overlay
  surface on `SlapdDatabase`) rather than silence.

**Stale prior comparison.** An older written-down comparison lists
`olcSpCheckpoint`, `olcSpSessionlog`, `olcAccessLogPurge`,
`olcAccessLogOps`/`Success`, syncrepl `retry`, the accesslog indices and
per-database accesslog separation as missing. They are not: ADR-019, ADR-022 and
the syncprov overlay code cover all of them. Do not re-raise them from that
document.

---

## ~~ReplicationConverged compares CSN sets ACROSS databases — structurally ~always False on a multi-DB cluster~~ — DONE (2026-09-14)

Fixed per database, together with the two sibling instances in the same
functions (query errors that only decorated the message; the peer verdict
computed from whichever database answered). New `replicationState:
PartiallyVerified` for a peer whose evidence is incomplete. Unit red-first on
both seams plus a live e2e red; see `docs/reconcile-loop-fixes.md` (2026-09-14)
and the ADR-008 amendment of the same date. The original entry follows for its
evidence.

**What:** `checkLocalCSNConvergence` (`slapdcluster_controller.go`) appends every
(pod × database) contextCSN set into one flat list and `csnConverged` compares
them all pairwise. Two databases have different CSN sets *by construction*
(independent write histories), so on any cluster with two replicated
SlapdDatabases the condition compares db1's CSNs against db2's and reads
`CSNsDiverged` with a meaningless "lag" — the age difference between two
different databases' last writes. Single-site included; the two-database
`example` fixture triggers it everywhere.

**Evidence (2026-09-13, while root-causing the db2 cross-site breakage):** a
run's backup spec recorded `SourceConverged=False (ClusterDiverged: …
CSNsDiverged … local CSN divergence: 372.8s lag across 3 pods (2 query
errors))`; the live lab showed `8.8s lag across 3 pods (1 query errors)` while
db1 was genuinely converged on all pods. 372.8 s and 8.8 s are db1-newest minus
db2-newest, not replication lag. The same flattening feeds
`checkPeerCSNConvergence`'s `localNewestTime`, so peer lag can also mix
databases; and a failing database's queries are silently `continue`d, so the
peer verdict is computed from whichever database answers.

**Impact:** every backup on a multi-database cluster records
`SourceConverged=False` regardless of health (ADR-014 amendment 2026-09-12
consumes this condition), and the designed-but-deferred `requireConverged` gate
would wait forever. The condition is a shipped status field reporting a falsehood.

**How:** converge per database — group CSN sets by database, a database is
converged when its own sets match across pods, the condition is the AND with a
per-database message. Red-first unit on the grouping (two healthy DBs' distinct
sets must read converged; one DB diverged across pods must not). Class: one
structure answering two questions — a per-database verdict flattened into a
per-cluster list. Separate fix by decision (2026-09-13): it visibly changes
backup `SourceConverged` semantics and deserves its own red-first pass.

---

## ~~`slctl inspect`'s CSN check only ever looks at the FIRST data suffix~~ — DONE (2026-09-14)

Fixed per database, mirroring the operator-side fix above: `inspect` probes
`contextCSN` for every non-`cn=` naming context, the verdict moved into a pure
seam (`cmd/slctl/cmd/csncheck.go`) keyed by suffix, and the cluster verdict is
the worst per-database one, naming the database it indicts. `--json` carries
`pods[].contextCSNBySuffix` (replacing the flat `contextCSN` array, which only
ever held the first suffix's vector) and the per-pod display gets one section
per database. The `dataSuffixFromNamingContexts` helper — the trap itself — is
deleted, and `debug-dump`'s identical first-suffix-only dump is fixed with it.
Also reworded the misleading "never synced?" on a pod with no contextCSN: a
hidden glue suffix entry takes `contextCSN` with it (ADR-025) and this probe is
anonymous, so the check no longer asserts a cause it cannot know. Unit
red-first, live-verified on a two-database cluster. The original entry follows
for its evidence.


**What:** `inspect.go` picks one suffix via `dataSuffixFromNamingContexts`
(first non-`cn=` naming context) and reads `contextCSN` for that one only. So
the `csn-convergence` check — and the per-pod `contextCSN` shown in the
detailed output — covers db1 and is blind to every other database on the
cluster. The two-database fixture has had no slctl CSN coverage for db2 since
it landed (2026-08-25).

**Why it matters:** same class as the operator-side defect fixed 2026-09-14
(a per-database quantity handled on a single-value axis), and slctl is the tool
a human reaches for when the operator's condition says something is wrong —
it currently cannot confirm or deny it for any database but the first.

**Why deferred:** found while fixing the operator half; folding it in would
blur a controller fix with a CLI change in a different binary, and the CLI
wants a decision of its own (per-suffix sections in the human output, a
`contextCSN` map in `--json`, and what `--short` prints).

**How:** probe every non-`cn=` naming context, key the check by suffix, and
report per-database (pass/fail naming the suffix). The ADR-025 suffix-health
probes in the same function already iterate all naming contexts — follow that
shape.

---

## ~~The replication credential lives in two stores with no reconciliation between them~~ — DONE (2026-09-15, ADR-027)

**What:** `cn=replication,<suffix>`'s password exists as (a) a per-site k8s
Secret (`<dbname>-credentials`, create-only, ADR-008) and (b) a `userPassword`
on an entry INSIDE the replicated DIT. The DIT converges mesh-wide on its own
(CSN conflict resolution picks winners); the Secrets don't, and nothing detects
or repairs the skew: `ensureReplicationUser` is create-if-missing on the entry's
existence — it never verifies or converges the password, so a stale, foreign, or
even **passwordless** entry (observed live 2026-09-13: an ACL-stripped copy with
no `userPassword` at all — a state that locks every consumer out and satisfies
the existence check forever) is permanent until the DIT is wiped.

**Consequences, all currently by-design-unsupported rather than handled:**
rotation of `replication-password` breaks the mesh silently; re-running
`tests/e2e.sh setup` over retained DITs rotates the Secrets but not the entries
(same class — the supported reset is a full teardown including PVCs); and a
mesh that ever ran with divergent Secrets (the 2026-09-13 db2 breakage) cannot
self-heal because the repair channel is the broken channel.

**Why deferred:** a converge-the-password path is its own design pass —
multi-master needs a single-writer rule (every site rewriting the entry to its
own Secret is a churn storm; the passwords must already be uniform per
ADR-008), a bind-probe as the drift detector, and decided rotation semantics.
ADR-008 explicitly says create-only/never-rotates today; changing that is an
ADR amendment, not a bug fix.

**How, when picked up:** detect first (a per-database self-bind as
`cn=replication,<suffix>` from the operator; surface a condition on
SlapdDatabase when it fails — cheap, no writes, catches all of the above
loudly), and only then decide whether/where a repair write belongs.

### 2026-09-14: the detector now has a pattern to copy, and a rule to obey

Two things landed today that make the "detect first" half cheaper and better
defined than when this was written:

- **The pattern exists.** `internal/suffixprobe` is a shared per-pod LDAP probe
  consumed by the operator (`DataPresent`), the backup controller
  (`SourceSuffixHealthy`) and `slctl` — one classifier, three call sites, no
  duplicated predicate. A replication-credential probe is the same shape: bind
  as `cn=replication,<suffix>` per database per pod, surface a condition, write
  nothing. Copy that structure rather than inventing a fourth.
- **ADR-026 constrains the repair half.** R2: a destructive or corrective action
  on state the operator does not exclusively own may not be authorised by
  evidence it does not own. The `cn=replication` entry is *mesh-shared* state
  whose competing writers include every other site and any human with
  `ldapmodify`, so a repair write needs an explicit single-writer rule — which
  site, which pod, on what authority — before it can be safe. That is precisely
  the design pass this item defers, and R2 is now the frame for it: report and
  stall is the cheap correct default; repairing needs ownership.

---

### 2026-09-15: closed by removing the store, not by reconciling the two

ADR-027 moved the replication identity to `cn=repl-<db>,cn=slaptain-auth` in a
per-pod, never-replicated auth database on the data PVC, converged from the
Secret on every reconcile. That kills this item at its cause rather than
answering it: the duplication is inherent to simple bind, but the verifier copy
is no longer in the *replicated customer tree*, so it is no longer shared
multi-writer state — no single-writer rule is needed, no drift detector, and
rotation is supported (measured 52 s across four pods; worst case is the
SlapdDatabase resync floor of 5 minutes).

The "detect first" plan was therefore overtaken, not executed. The two live
findings that motivated this entry — the 19-day dead db2 link and the
password-stripped copy — are both impossible in the new store: it is node-local,
so nothing replicates a foreign or ACL-mangled copy into it, and the entry is
converged rather than create-if-missing.

**What remains, and it is not this item:** the legacy `cn=replication,<suffix>`
entry and its grants are deliberately retained for the staged-upgrade window
(ADR-027 migration steps 2+3). Dropping the grants is step 4, a separate
release; deleting the entry itself is a documented manual cleanup, because it is
a write into replicated state the operator does not own (ADR-026 R2). Two
window caveats are recorded in CLAUDE.md and ADR-027: the migration window is
one-directional (a NEW consumer against an OLD provider fails — pin
`externalPeers[].bindDN` meanwhile), and rotating the Secret mid-window strands
the create-only legacy entry.
That second caveat has a security half that is its own open item — see
"Rotating `replication-password` does not revoke the leaked credential".

---

## ~~R4 debt: syncprovCheckpoint and accesslogPurge are silently write-once; purge has no default~~ — DONE (2026-09-13)

Both now converge on every reconcile through a shared pure planner
(`planOverlayAttr`, generalising `planSessionlog`) plus a thin LDAP executor, on
both the syncprov and the accesslog overlay. `accesslogPurge` gained the R5
default `"7+00:00 1+00:00"` (7-day window, daily sweep) with `"none"` as the
explicit opt-out sentinel, consistent with `keepalive`.

Both attributes were put through the live-modifiability probe first — the
precondition ADR-024's amendments made mandatory for this class — and both
**passed**: replace and delete, each instantaneous, the pod still serving and
still accepting data writes afterwards, zero restarts. So no R2/R3 downgrade was
needed and ADR-024 needed no further amendment; the probe confirmed the class
rather than refuting it.

## ~~`DataPresent` does not cover read-only replica pods~~ — DONE (2026-09-14)

The probe now visits the `<name>-readonly-N` pods too, so an RO-only glue
(ADR-025 evidence item 5: a consumer initial-syncing from a glued provider
replicates the glue with the same entryUUID) is visible to the standing signal
and not only to `slctl inspect`. The deferred call — that including RO pods
widens the transient False window during a legitimate initial sync — was
answered by a distinct reason rather than by suppression:
`False/DataMissingOnReadOnlyPods` when every writable pod has the entry and a
read-only one does not, so an alert rule can hold it to a longer fuse than
`DataMissingOnPods` / `GlueSuffix`. Phase-neutral (the `readOnlyReadyReplicas`
precedent), observability-only (ADR-012), and byte-identical behaviour at
`readReplicas: 0`. See the ADR-025 amendment of the same date.

---

## ADR-005 step 4 is unimplemented for `cleanupPolicy: Retain`

**What:** ADR-005's teardown sequence says the operator removes the database's
syncrepl stanzas **in both cases** — `Delete` and `Retain`. The `Delete` path
now does the right thing by construction: the stanzas are attributes of the data
DB entry and die with it (verified 2026-09-14 while fixing the non-leaf delete).
The `Retain` path does not. The CR is finalized, the operator stops managing the
database, and the stanzas it previously wrote stay on every pod.

**Why nothing dangles today:** the stanzas name *this* database, and under
`Retain` this database stays. The retained configuration is self-consistent and
slapd keeps replicating it — which is arguably what `Retain` should mean. The
gap is between the ADR's wording and the code, not (yet) between the code and a
broken cluster.

**Where it could bite:** a retained database is orphaned configuration — nothing
converges it any more. If the cluster's replica count later changes, those
stanzas still name the old peer set, because no controller owns them; the
database then replicates against a topology that no longer exists. Whether that
is "as intended" for a deliberately abandoned database or a leak is a decision
nobody has made. ADR-005's own text implies the latter.

**Decide first, then implement.** Does `Retain` mean *"stop managing, leave it
exactly as it is"* — then amend ADR-005 step 4 to say so and this closes as a
doc fix — or *"leave the data, unwire the replication"* — then implement stanza
removal for `Retain` and the ADR stands as written? The second is the larger
job and needs care: removing stanzas is a `cn=config` write against a database
the operator has just disowned, so ADR-026 R2 applies — it must be authorised by
the operator's own record of what it wrote, never by inspecting what happens to
be there at teardown time.

**Found:** 2026-09-14, while fixing `cleanupPolicy: Delete` on replicated
databases; deliberately not folded into that change.

---

## e2e needs a liveness assertion class (found via the timeout= freeze)

The suite asserts end states through Eventually with generous budgets, so a
self-resolving full-server freeze passes green: the timeout= regression's three
~299 s silence gaps per convergence pass — etime≈300 sitting right there in the
logs — tripped nothing, and was found by a human noticing silence in a manual
run. Wanted: assertions that bound liveness during convergence — operator LDAP
op etimes under a threshold, and/or a probe writer asserting no
all-pods-silent gap longer than N seconds during setup/convergence. Cheap
first cut: parse etime from slapd stats logs in the tunables spec.

### 2026-09-15: the first instance landed, from the opposite direction

The restore specs now wait on **progress**, not on a deadline
(`tests/e2e/restore_wait_test.go`): any change in the restore machine's
observable state — cluster phase, `status.restore` sub-phase and message,
StatefulSet replicas, restore Job and Job-pod state, `restoreApplied` — resets
the clock; 5 min with nothing changing fails, 15 min while a restore Job pod is
*Running* (slapadd is silent, so that step is opaque) fails, and a 30 min
ceiling is the loud backstop. Terminal negative evidence (a Job at
`BackoffLimitExceeded`, `status.restore.phase=Failed`) fails immediately instead
of waiting out any budget. Pure policy in `restoreBudget.verdict`, unit-tested
red-first off the measured incident trace.

Same signal as this entry wants, opposite failure mode: this entry is about a
stall that currently passes green, that one about a slow-but-advancing machine
that failed red (a `E2E_SCALE=1 E2E_BACKUP=1` run, 2026-09-15: the in-place
spec's bootstrapFrom wait timed out at 480 s on a restore that completed
afterwards). Whoever picks up the convergence half should reuse the shape —
sample a fingerprint, treat change as liveness, keep the trail for the failure
message — rather than invent a second mechanism.

Numbers worth keeping, because they say where restore time actually goes:
of the failing 480 s, ~347 s was spent before the first restore Job existed, in
coarse operator ticks (107 s CR → pod, 60 s → Ready, 60 s → preflight, 120 s in
preflight). The same specs against an idle single-site operator: 20-25 s
end-to-end. And `slapadd -q` — the term the artifact size actually drives — is
~19 µs/entry (1202 entries 25 ms → 250 002 entries 4.8 s, measured in the
slapd-init image), i.e. sub-second for the 1200-entry E2E_SCALE fixture. So an
inflated fixture lengthens restores through the *operator's* per-tick workload,
not through the bulk load, and a per-entry timeout coefficient would have been
fitted to the wrong variable.

## PRIORITY RAISED: the big-DIT initial-sync e2e

Deferred twice; the tax keeps arriving. A >500-entry (now: multi-second
refresh-window) initial full sync exercises exactly the long refresh phases
where the timeout= freeze had its collision window, on top of its original
purpose (the sizelimit class). E2E_SCALE seeds entries into a LIVE mesh (small
deltas); this lane must instead create a FRESH consumer against a populated
provider — pod-recreate or bootstrapFrom against a big artifact. It is also the
lane that would calibrate the restore specs' `workingStall` budget, which is
currently ~19 µs/entry extrapolated rather than measured against a DIT big
enough to matter (see the liveness entry above). Next e2e
investment, before further replication-path changes.

**Second consumer, 2026-09-14 — an unmeasured number is now user-visible.**
`DataPresent` was extended to the read-only fleet (ADR-025 D5 amendment), so an
RO pod doing a legitimate initial sync makes the condition read
`False/DataMissingOnReadOnlyPods` until that sync completes. On fixture-sized
data that is seconds; **on a production-sized DIT nobody has measured it**, and
the answer decides whether the reason needs a documented "expected during RO
bring-up" caveat, an alerting-delay recommendation, or nothing at all. This lane
is what produces the number: stand up a populated provider, add an RO replica,
and time the window from the condition's own transitions. The same run answers
the older question — how long a fresh consumer's refresh phase actually is —
against the same fixture.

**Third consumer, 2026-09-15 — the number is now a budget in `tests/e2e.sh`.**
The post-suite `check_data_present_every_site` gate got reason-aware budgets:
60 s for every reason that is never legitimately transient after a completed
suite (`GlueSuffix`, `DataMissingOnPods`, `DataMissing`, `NoDataYet`,
`NoReachablePod`, an absent or unrecognised one), 600 s for
`DataMissingOnReadOnlyPods`, overridable as `E2E_DATAPRESENT_TIMEOUT` /
`E2E_DATAPRESENT_RO_TIMEOUT`. **The 600 s is a guess and is labelled as one in
the script**, for exactly the reason this entry exists: fixture-sized numbers
are all anybody has. Measured on a single-site lab on 2026-09-15, forcing the
shape by wiping the RO pod's data PVC behind an egress NetworkPolicy that
denied its syncrepl: the condition read `False/DataMissingOnReadOnlyPods`
within 5 s of the pod coming up empty, stayed False for as long as the block
stood (78 s, bounded only by when it was lifted — a broken replica does not
self-heal, so the fuse does end in a failure), and returned to
`True/RootEntryVisible` **18 s** after the block was removed. 600 s is ~33x
that, on a DIT of a few dozen entries. This lane replaces the guess with a
number; until then, raise the env var rather than trusting the default on a
fixture that has grown.

**Fourth consumer, 2026-09-28 — a probe threshold now depends on it.** ADR-030
gives the slapd container a `startupProbe` with a 300 s budget, sized to "far
beyond any fixture" because that is all the evidence there is: every e2e pod
opens a few-dozen-entry LMDB, and `E2E_SCALE` seeds into a LIVE mesh rather than
opening a big one cold. How long slapd takes to answer its first LDAP request on
a production-sized LMDB is unmeasured, and the consequence of guessing low is
sharper than for the other three consumers: under `OrderedReady` a pod-0 that
never reaches Ready means pod-1 is never created, so the cluster does not form
at all. Fixture-sized measurement for scale: 3-5 s from container start to Ready
across all twelve pods of the three-site lab, i.e. ~60-100x headroom on a DIT of
a few dozen entries. This lane produces the number that matters.

A second finding from the same session, worth more than the budget: the gate
now **pokes** each pending `SlapdDatabase` with an annotation every 30 s,
because `DataPresent` is phase-neutral observability and a healthy database
reconciles on the 5-minute `databaseResyncInterval` floor. Without that, a
`False` that had already healed could sit in status for up to five minutes with
nothing wrong anywhere — so any budget shorter than five minutes would have
been measuring condition staleness, not cluster state. Anything else that
gates on a `SlapdDatabase` condition from outside the operator inherits the
same problem.

---

## `readReplicas: 1 → 0` leaves the read-only fleet running

**What:** the three read-only reconcilers *skip* on `spec.readReplicas == 0`
instead of removing what an earlier non-zero value created —
`reconcileReadOnlyStatefulSet` (`operator/internal/controller/slapdcluster_controller.go:766`),
`reconcileReadOnlyService` (`:712`) and `reconcileReadOnlyHeadlessService` (`:671`)
each `return nil` immediately. Skipping is the right behaviour for a cluster that
never had RO replicas; it is the wrong behaviour for one that is being scaled back
to zero. The `<name>-readonly` StatefulSet keeps its old replica count, its pods
keep consuming via delta-syncrepl, and both RO Services keep resolving — until the
whole `SlapdCluster` is deleted and the owner reference finally reaps them.

**Observed 2026-09-20** on a single-site lab: `slapd` at `spec.replicas: 1`,
`spec.readReplicas: 0`, yet `slapd-readonly` reads `1/1` with `slapd-readonly-0`
Running and `config-slapd-readonly-0` / `data-slapd-readonly-0` Bound and in use.
Nothing in the CR says that pod should exist.

**Why it matters:** the declarative contract is broken in the direction users
least expect — an orphan replica that still holds a syncrepl consumer slot on
every RW pod, still answers on `<name>-readonly`, and still pins its PVCs (so its
node, ADR-018). It is also invisible from the CR: `status.readOnlyReplicas`
reports the spec's 0 while a pod serves. Somebody reducing an RO fleet as a cost
or blast-radius measure gets no warning that nothing happened.

**Not this entry:** the stale `{config,data,accesslog}-slapd-{1,2}` PVCs left by a
`replicas: 3 → 1` scale-down. `volumeClaimTemplates` PVCs are never garbage
collected by the StatefulSet controller — that is upstream Kubernetes behaviour
and matches ADR-005's Retain default. Deleting them is the user's call.

**How:** on `readReplicas == 0`, delete the RO StatefulSet and both RO Services
rather than returning early — pods first so their PVC deletion leases are released
(ADR-018), and idempotently, since the usual case is that none of them exist. Decide
explicitly what happens to the RO PVCs: the consistent answer is Retain (ADR-005),
which means the scale-to-zero leaves storage behind and should say so. Red-first
per Test Discipline: an e2e that deploys with `readReplicas: 1`, flips it to 0, and
asserts the StatefulSet, both Services and the pod are gone — it is red against
current code for the right reason today.

---

## The operator's control channel has no stated TLS posture

Every connection the operator makes to configure a pod is **plain `ldap://` on
port 1024**. Verified across all nine `cn=config` call sites —
`slapdcluster_tunables.go:119`, `slapddatabase_controller.go:552/1993/2099/2243/2905/3116`,
`slapddatabase_authdb.go:164`, `slapdschema_controller.go:182` — each a bare
`ldap.DialURL("ldap://"+addr)`, no StartTLS. Only the replication path (1025) and
the schemed readers in `csn.go` / `slapdbackup_controller.go` ever use `ldaps://`.

So the `<name>-config-password` Secret **authenticates** the operator; it does not
**encrypt** anything. What crosses the pod network in the clear, on every pod, on
every convergence pass:

- the `cn=admin,cn=config` password itself, in a SIMPLE bind;
- every `olcSyncRepl` value written by `applySyncreplToPod` — and a stanza carries
  `credentials=<replication password>` in plaintext by construction (ADR-027
  Consequences: "inherent to simple bind");
- whatever a seed carries, on the one pod-0 pass that applies it.

Nothing restricts who may reach that port, either: the only NetworkPolicy in the
repo is `charts/operator/templates/networkpolicy.yaml`, which gates **metrics
ingress to the operator** and says nothing about slapd. Port 1024 on every pod is
reachable by anything the CNI's default allows.

**This is not a bug report.** In-cluster control traffic over a trusted pod network
is what most database operators do, and it may well be the right call here. The
gap is that it was never *decided* in writing. The posture appears exactly once in
the repo, as an annotation inside a log walkthrough
(`docs/BOOTSTRAP.md:350`, "← plaintext LDAP, port 1024"), and nowhere as a
statement anyone could agree or disagree with. ADR-029 argues TLS should be on by
default for client traffic and does not mention the control channel at all; ADR-027
option C counts "no plaintext secret anywhere in `cn=config`" as a benefit mTLS
would buy, without noting that the write which puts it there is itself plaintext.
A reader auditing slaptain's TLS story today has to read Go to find this out.

**Wanted:** a stated posture, in `docs/TLS.md` and as an ADR if the answer is
anything other than "yes, deliberately, forever". The three candidate answers, with
their real costs:

1. **Keep it, and say so.** Cheapest and possibly correct. Then say what it assumes
   (a trusted pod network) and what would make it false (shared/multi-tenant
   clusters, CNIs without encryption), and pair it with a shipped NetworkPolicy for
   1024 so the assumption has something enforcing it.
2. **`ldaps://` on 1025.** slapd already listens there. The operator would have to
   verify a certificate whose SAN covers the pod's headless FQDN — which
   `tests/gencert.sh` mints but a user-supplied Secret need not — and it can only
   apply when `spec.ldap.tls.enabled`, so the plaintext posture survives for
   precisely the clusters that opted out of TLS. That conditionality is the design
   question, not the dialling.
3. **StartTLS on 1024.** Same certificate question, no second port, and a graceful
   downgrade path. Probably the smallest step that changes the answer.

Whichever wins, note that it does not remove the plaintext `credentials=` from
`cn=config` on disk — that is ADR-027 option C's territory, deferred separately —
and it does not touch the diagnostics leak recorded there (`slctl debug-dump`
collects stanzas verbatim).

---

## An operator upgrade rolls every slapd pod in every cluster it manages

When `spec.images` is unset — the documented, recommended shape — both
data-plane images resolve from the operator's own reference:
`defaultDataPlaneRepo(r.OperatorImage, "slapd"|"slapd-init")` for the repository
and `r.DefaultImageTag` (the `OPERATOR_IMAGE_TAG` env, injected by the chart
from `Chart.AppVersion`) for the tag. So upgrading the operator rewrites the
`slapd` and `slapd-init` image strings in **every pod template of every
StatefulSet of every SlapdCluster it manages**, read-only fleets included, and
the StatefulSet controller rolls all of them.

The *defaulting* is deliberate and good (CloudNativePG's pattern, described in
CLAUDE.md): operand images track the operator's version instead of drifting. The
**consequence** is stated nowhere. `helm upgrade slaptain` is, today, a rolling
restart of every directory in the cluster, and nothing in the docs says so.

**Found by accident, 2026-09-28:** it masked the first verification of the
`LDAP_REPLICAS` fix — scaling right after an operator redeploy rolled everything
and looked like the fix had failed. The giveaway was `slapd-readonly-0` rolling
too, which `LDAP_REPLICAS` could not explain because it was never set on RO
pods. Anyone testing pod-template churn must let the operator settle first; that
is now recorded in `docs/reconcile-loop-fixes.md`.

**Why this is heavier here than for a single-site database operator.** The pods
keep their PVCs, so there is no refresh storm (ADR-021 is not in play). What
*is* in play is ADR-016: every replaced pod invalidates the syncrepl addresses
each remote site holds for it, measured at 130-268 s of cross-site outage per
event. An operator upgrade on a three-site mesh therefore produces one
rediscovery window per pod per site — with **none** of the gating ADR-023
specifies for precisely this operation (proceed only while `minAvailable`
*current* copies survive; wait for `ReplicationConverged`; wait for peer
rediscovery to settle). The StatefulSet controller's own gate is pod Readiness,
and Readiness here means "the process exists" — see "The slapd container has no
`readinessProbe`" above, which makes this roll able to outrun convergence.

**Wanted:** a decision, then documentation to match it. Three candidates:

1. **State it and leave it.** Cheapest. Document that an operator upgrade rolls
   the fleet, and recommend pinning `spec.images` on mesh deployments so
   data-plane upgrades become a separate, deliberate act. Cost: pinned images
   drift from the operator, which is the property the defaulting exists to
   provide, and ADR-021's dual-image rules make "always pin both or neither" a
   trap someone will step in.
2. **Default at creation only.** Resolve the tag when the StatefulSet is first
   written and leave whatever is there afterwards. Keeps first-run convenience,
   removes the surprise — and removes the upgrade-tracking property entirely,
   silently, which is worse than either explicit choice.
3. **Sequence it.** Reuse the ADR-023 gates for an operator-initiated roll:
   ordinal at a time, gated on convergence and peer rediscovery. Correct and the
   most work; note the machinery already has to exist for ADR-023 anyway, so the
   marginal cost is the trigger and the bookkeeping, not the state machine.

Whichever wins, the demo and `docs/DEVELOPMENT.md` should stop implying that
`make operator-helm-install` is a control-plane-only operation.

---

## Hot migration e2e never wires the source back, so Stage 2 is untested

**What:** `tests/e2e-migration.sh` stands up `fakeprod` (peer mode,
`deltaSync: false` — a stand-in legacy plain-syncrepl provider) and `slaptain`
(consumer-only, `externalPeers` → fakeprod), and `tests/e2e/migration_test.go`
asserts seven things about it: the seeded entry arrives by plain syncrepl,
`entryUUID` matches the source, `DataPresent` holds on a never-seeded database
(ADR-025 D5), consumer-only rejects writes, `mode: peer` promotes **in place**
(pod UID and slapd container `StartedAt` unchanged), writes are accepted after
promotion, and `entryUUID`s survive the promotion.

`apply_fake_prod()` gives fakeprod **no `externalPeers`**. Nothing points back at
slaptain, which is exactly ADR-011 Stage 2's "source-side configuration changes:
add reciprocal syncrepl stanzas pointing at slaptain pods". So the fixture never
reaches Stage 2 — it reaches the slaptain half of it.

**Why it matters:** the post-promotion spec asserts the write is *accepted*
(`migration_test.go:215`); it never asserts it *arrives* at fakeprod, and no spec
writes on the source after promotion. Stage 2's verification clause — "lag stable
< 5 s in **both** directions" — has nothing behind it, and bidirectional
dual-running is the property that makes the migration hot rather than a cutover.
ADR-011's status line reads "e2e-verified via `tests/e2e-migration.sh`", which is
true of the interop and the stage transitions and overstates the rest; it wants a
one-line amendment either way.

Bidirectionality is covered *elsewhere* — `external_replication_test.go`
(`E2E_EXTERNAL_REPL=1`) exercises two-way delta-syncrepl between two slaptain
clusters. The untested combination is specifically **bidirectional against a
plain-syncrepl, non-slaptain provider**, i.e. the only topology hot migration
exists for.

**Three further gaps in the same fixture**, smaller and separable:

- **Foreign ServerID coexistence.** Stage 2 puts slaptain's serverIDs on the
  source's `olcServerID` list. That is the CSN-collision hazard ADR-011's
  "Foreign ServerID declaration" decision exists to prevent, and nothing
  exercises it.
- **Demotion.** ADR-010's in-place peer → consumer-only demotion is the
  documented rollback for Stage 2 (`docs/MIGRATION-HOT.md:137`). Only the
  promotion direction has a spec.
- **Single pod on both sides** (`replicas: 1`), so no per-pod ServerID
  coordination under a migration and no mesh member migrating.

Stages 3–5 (client cutover, source drain, source retirement) are runbook-only by
design — human and ops steps, not operator behaviour. They are not part of this
entry.

**Wanted, cheapest first:** after the promotion spec, patch `fakeprod`'s
SlapdDatabase/SlapdCluster to add an `externalPeers` entry pointing at
`slaptain-0` with `deltaSync: false`, then assert the post-promotion write
appears on `srcConn` — the spec already holds a connection to fakeprod for the
`entryUUID` comparison, so the machinery is there. Red-first is free here: the
assertion is red against the current fixture for the right reason. That converts
the largest untested claim into a spec without needing a real legacy slapd.

Foreign-serverID and demotion are separate pieces; the multi-pod variant depends
on the same "specs cannot provision their own topology" framework gap as the
`slapd-init` pinning entry above.

---

## `cross-site-csn` Unreachable is a hard `fail`, so a bootstrap blip red-exits `slctl inspect`

Carried out of GitHub issue #1 when it was closed (2026-10-06). Everything else
that issue raised has landed; this one item it filed as "owner unsure — needs a
decision", and the decision was never taken.

`slctl inspect` grades the `cross-site-csn` check as `warn` when a peer is
**Lagging** (data is flowing, just behind) and `fail` when it is **Unreachable**
(`operator/cmd/slctl/cmd/inspect.go:873`). `fail` sets the exit code, so any
pass with an Unreachable peer is red in CI and e2e.

The problem is that Unreachable has two populations behind it, and the check
cannot tell them apart:

- **Transient.** Measured on a fresh three-site pod-routed bring-up: every
  remote pod CSN query timed out for ~1 minute while cross-site routing
  converged, and `cross-site-csn` hard-failed — while replication itself was
  never broken (the local `contextCSN` already carried the remote serverIDs).
  It self-healed to 13 passed / 0 failed with no code or config change. The
  probe was wrong about the thing it was probing.
- **Persistent.** A genuinely dead cross-site link, which is exactly what the
  check exists to catch and should stay a hard fail.

**The open question is not "warn or fail"** — it is what distinguishes the two
without a timer that lies. Candidates, none costed:

- A bootstrap/grace window: suppress to `warn` while the cluster is young or
  while `status.externalPeerStatuses[].lastChecked` shows the peer has never yet
  been reached. Cheap, but "young" is a guess, the same class of admitted guess
  as ADR-030's startup budget.
- Grade on **duration**, not state: the peer status already carries
  `lastChecked`, and a never-reached peer is distinguishable from one that was
  Synced five minutes ago. This is the honest version and needs a status field
  that records when the peer was last *reachable*, which does not exist today.
- Leave `fail` and fix the caller: have e2e tolerate the window explicitly
  rather than teaching the diagnostic to be vague. Keeps `inspect` honest at the
  cost of pushing the judgement into every consumer.

Note that `PartiallyVerified` (ADR-008 amendment, 2026-09-14) already solved the
*adjacent* case — the peer answered but some database yielded no readable
contextCSN — and deliberately did not touch the nothing-answered-anywhere case,
which stays Unreachable. Whatever lands here should keep that split intact.

---

## `replication.enabled: true → false` half-tears-down a live cluster, silently

Nothing rejects the edit — there is no admission webhook — and the operator then
does half a teardown, because the two halves of the replication config are gated
differently:

| | on `enabled: false` |
|---|---|
| syncprov overlay, accesslog overlay | removed (`wantsSyncProv` includes `Replication.Enabled`) |
| accesslog DB `cn=accesslog-<db>` | **dropped — the change journal is destroyed** |
| `olcSyncRepl` stanzas | left in place (`slapddatabase_controller.go` step 10 is `if enabled { … }`, no else) |
| `olcMultiProvider` | left `TRUE` (only ever written on the enabled path) |

Every pod is left consuming from peers that no longer serve syncrepl — slapd's
*"got search entry without Sync State control"*. Replication stops by breaking
rather than by being switched off, and the journals do not survive it.

Found 2026-10-07 while answering a plain question: why would anyone ever set
`replication.enabled: false` — to stop replication for maintenance? The honest
answer is "at creation, to mean standalone". The field is a creation-time
declaration and a one-way on-ramp; `false → true` is designed, live, covered by
`scaleup_test.go`, and (since the ADR-013 amendment of the same date) rolls no
pod. `true → false` is not its inverse, and nothing says so at the point of use.

Note the *designed* demotion already works and has none of this problem:
`mode: consumer-only` keeps `enabled: true`, so `reconcileReplication` still runs
and **converges** the stanza set (in-cluster providers emptied,
`olcMultiProvider: FALSE`, data DB `olcReadOnly: TRUE`) instead of abandoning it.

### Open — needs a proper look

1. **Surface it.** A condition — `ReplicationConfigStale`, or a reason on an
   existing one — when `enabled` is false and pods still carry `olcSyncRepl`.
   The operator reads every pod's `cn=config` every pass, so the evidence is
   already in hand; this is reporting, not new machinery. Cheapest real
   mitigation.
2. **Decide whether a disable path should exist at all.** Deliberately not
   written on speculation. It would have to perform exactly the destructive
   short-set `Replace` on `olcSyncRepl` that ADR-031 decision 6 exists to
   prevent, against node-local `cn=config` the operator cannot prove it is the
   only writer of (ADR-026 R2). ADR-005 is the standing lesson on what teardown
   of replicated state costs when it is added quietly — it was wrong for a year
   and nobody noticed. If this is built, it is an ADR, not a patch.
3. **Consider whether the overlay teardown should be gated the same way as the
   stanza write.** The split is the actual defect: one half honours "a withhold
   must leave the pod's config alone" and the other half does not. Making the
   overlay/accesslog teardown as conservative as step 10 would turn a destructive
   half-state into a benign no-op, and is a much smaller change than (2). Worth
   costing before (2) is even considered.

Prior art to read first: ADR-031 decision 6 (short set = deletion), ADR-026 R2
(do not destroy shared `cn=config` state), ADR-019 amendment 2026-09-14 (the
withdrawn R8 migration that stranded an `olcAccessLogDB` and made a pod
permanently unbootable — the closest thing to this hazard that has actually
bitten).

Background written up on branch `fix/pod-template-roll-on-replication-enable`:
ADR-010 amendment of 2026-10-07 and the `spec.replication.enabled` field
description. This item is parked here on purpose so it is not gated on that
branch merging.

---

## A fresh SlapdDatabase reads `Error/NoPodsReachable` while its mkdir Job runs

For the first few seconds after a `SlapdDatabase` is created, its phase is
`Error` with reason `NoPodsReachable` and message *"Could not apply database to
any pod"*. It turns `Running` on a later pass without anyone doing anything.
Seen while walking `docs/demo/` (2026-10-10): `Error` at 0 s, `Running` at 15 s.

The mechanism, from the code: on the first pass the database's directories do
not exist yet, so `ensureDatabaseDirs` (`dirjob.go`) creates the mkdir Job and
returns `errDatabaseDirsPending`. The per-pod apply returns that error, so
the pod lands in `failedPods`, and with no pod in `appliedPods`
`databasePhase` (`databasephase.go`) ranks the pass as precedence 1, "no pod
could be reached at all".

The pods *were* reached: the operator dialled and bound before it asked for the
directories. The status points a human at the network when the right action is
to wait. Waiting on directories is pending work, which `databasePhase` already
has a rank for, below the error case.

Why nothing caught it: the e2e suite and the demo both wait for `Running`, and
a transient `Error` on the way there is invisible to a wait.

### Shape of a fix (not written)

- Carry "directories pending" out of the per-pod apply as a distinct outcome
  instead of a generic failure, and let `databasePhase` rank it as pending
  (`Pending` or `Degraded`, reason e.g. `DirectoriesPending`) rather than as
  `NoPodsReachable`.
- Red first: a `databasePhase` unit case where every pod is directory-pending,
  observed to return `Error/NoPodsReachable` before the change.
- e2e: a fresh database never reports `Error` on its way to `Running` (poll the
  phase from creation, not just the end state).
- Once fixed, the explanatory paragraph in `docs/demo/README.md` step 3 can
  shrink to a line or go.
