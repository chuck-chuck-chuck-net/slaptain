# ADR-027: The replication identity is node-local, not an entry in the replicated tree

**Status:** Proposed
**Date:** 2026-09-14

## Context

A syncrepl consumer authenticates to its provider with a simple bind as
`cn=replication,<suffix>`. That one secret is therefore stored twice, in two
different forms, because the protocol requires both:

- the **consumer** must *present* it, so the syncrepl stanza carries it in
  plaintext (`credentials=…` in `cn=config`), sourced from the per-site
  Kubernetes Secret `<dbname>-credentials`;
- the **provider** must *verify* it, so it exists as an SSHA hash in the
  `userPassword` of an LDAP entry.

Today that entry lives **inside the replicated data tree** (`slapddatabase_controller.go:1900`,
`replDN := "cn=replication," + sd.Spec.Suffix`). That single placement decision
is the source of a defect class:

- **The two stores propagate in opposite ways.** The entry replicates mesh-wide
  and the mesh converges on one winner; the Secrets do not replicate at all and
  each site generates its own. Nothing enforces that they agree.
- **Measured, 2026-09-13:** three sites, three independently generated Secrets,
  one converged entry — two sites held credentials matching nothing and their
  replication had been dead for nineteen days without a single failing health
  signal.
- **`ensureReplicationUser` is create-if-missing.** It checks existence and
  returns; it never compares the password. So rotation is silently unsupported,
  and a **passwordless** entry — measured live, a copy that arrived with
  `userPassword` stripped by an ACL — satisfies the existence check forever
  while locking every consumer out.
- **It cannot self-heal.** A corrective write into the replicated tree reaches
  the other sites *via syncrepl*, which is precisely what is broken. And under
  multi-master, every site converging the attribute to its own Secret is a
  permanent write storm.
- **ADR-026 R2 forbids the obvious repair.** The entry is shared state whose
  competing writers include every other site and any human with `ldapmodify`
  (ADR-002 sanctions hand edits). A corrective write therefore needs an explicit
  single-writer rule — which site, which pod, on what authority — before it can
  be safe. That is a design pass, deferred repeatedly.

**The reframe this ADR rests on:** the problem was never "two stores". It is
that *one of the two stores is shared, replicated, multi-writer state*. The
duplication is unavoidable; its placement is not.

The industry analogy holds and then breaks in an instructive place. MySQL and
Galera have the identical split — `mysql.user` replicates, each replica's
connection config is local — so the shape is conventional. But `mysql.user` is a
system table only an administrator writes, whereas ours sits in the *customer's*
data tree under *customer-defined* ACLs. That disanalogy is exactly how we
produced a password-stripped copy, and it is an argument for getting the
credential out of the data tree independent of everything else.

## Options considered

### A. A node-local authentication database (chosen)

Move the identity into a small database that is **never replicated**:
per-pod, operator-populated from the Secret, converged on every reconcile like
any other node-local state (ADR-002's model for `cn=config`, ACLs and schemas).

- The Secret becomes the single source of truth; the LDAP copy becomes a
  *projection* the operator solely owns.
- Reconciliation stops being a design problem: there is no competing writer, so
  ADR-026 R2 does not bite, and no single-writer rule is required.
- **Rotation becomes an ordinary operation** — change the Secret, the operator
  rewrites the entry and the stanzas on every pod, all node-local, all
  idempotent. This is the single largest gain and it needs no new machinery.

### B. Bind as the data database's rootDN — rejected

`olcRootPW` already lives node-local in `cn=config`, so this needs no new
database at all. Rejected on two counts: the rootDN **bypasses every ACL**, so a
leaked consumer credential is full read/write of that database; and it breaks
ADR-020, which deliberately relies on the accesslog database having a *different*
rootDN than the data database it journals.

### C. SASL EXTERNAL over mTLS — deferred, recorded with its real value

No password anywhere: per-pod client certificates, `bindmethod=sasl
saslmech=EXTERNAL`, an `olcAuthzRegexp` mapping the certificate subject to an
authorization identity, ACLs rewritten against it.

Deferred, and the honest accounting is *why* — because option A already removes
the class, C's marginal value is much smaller than it first appears:

- **What it still buys:** no plaintext secret anywhere in `cn=config` (see the
  diagnostics leak below); and per-pod identity, so a single compromised pod can
  be revoked without touching the others — where today every consumer in the
  mesh binds as the same identity.
- **What it costs:** certificate expiry becomes a *new* scheduled failure mode —
  passwords do not expire, certificates do, and a silently failed rotation stops
  replication at a moment nobody is watching. Plus the authz mapping, every ACL
  rewritten (ADR-020's grant included), `olcTLSVerifyClient` on every pod, and
  materially worse diagnosability (`err=49` is a gift next to a handshake that
  failed for one of six reasons).
- **Current state, verified 2026-09-14:** `olcTLSVerifyClient` is never set, so
  slapd **never requests a client certificate**. The operator does mount and
  present `tls_cert`/`tls_key`, but only on external-peer stanzas and to a server
  that does not verify them — so today's "mTLS peer auth" is server-side TLS with
  CA verification, relaxed further to `tls_reqcert=allow` for IP-addressed peers.
  The CLAUDE.md claim is inaccurate and is corrected separately.
- CLAUDE.md states security hardening is a **secondary** driver, pursued where it
  does not compromise functionality, performance and scale. C does not clear that
  bar today; A does.

### D. Keep the entry where it is and converge it — rejected

The status-quo repair. Requires the single-writer rule ADR-026 R2 demands, still
cannot rotate without a mesh-wide coordinated window, and leaves the credential
in the customer's data tree under customer ACLs — the placement that produced the
password-stripped copy. Rejected as solving the symptom while preserving the
cause.

## Decision

**1. A per-pod, non-replicated authentication database holds the replication
identities.** Suffix `cn=slaptain-auth`; no `syncprov`, no `syncrepl`, no
accesslog; `olcRootDN: cn=admin,cn=config` (the same pattern ADR-020 already uses
for accesslog databases, so the operator can write it with a credential it
already holds).

**2. Identities stay per-database.** `cn=repl-<dbname>,cn=slaptain-auth`, one per
`SlapdDatabase`, preserving the isolation ADR-020 depends on — one database's
replication identity must not be able to read another database's journal.

**3. It is fixed infrastructure, not a user database.** Its directory
(`/data/_slaptain-auth`) is created **unconditionally by the init container at
bootstrap**, and it never appears in `DATABASE_DIRS`. Therefore it adds nothing
to the ADR-013 rolling-restart wart: the roll exists because `DATABASE_DIRS` is a
pod-template env var that changes when user databases come and go, and this
database is not one of them.

The reserved name cannot collide with a user database: `DATABASE_DIRS` entries
are `SlapdDatabase` CR names, and a Kubernetes object name cannot begin with an
underscore (RFC 1123). The guarantee is structural, not a convention.

**4. It lives on the existing `data` PVC.** No new volume, no new mount, no new
PVC-deletion-lease surface (ADR-018). `/config` keeps meaning "slapd.d and
nothing else"; the data volume keeps meaning "LMDB environments".

**5. ACLs on the auth database are minimal:** `to attrs=userPassword by anonymous
auth by * none`, `to * by * none`. It exists to answer binds, nothing else.

**6. The Secret remains the single source of truth**, and the entry is converged
to it on every reconcile — replacing today's create-if-missing. This is what makes
rotation work, and it is safe precisely because the target is node-local.

**7. Ownership split: the cluster controller owns the container, the database
controller owns its entry.** The auth database is shared by every database on a
pod, so letting each `SlapdDatabase` reconcile create-if-missing would make it
exactly the shared-state-with-competing-writers shape ADR-026 exists to prevent
(and `olcDatabase={N}` renumbering, R1, would apply on every create). Therefore:

- the **SlapdCluster** controller creates and converges the auth *database* on
  every pod, alongside the per-pod infrastructure it already owns (modules, TLS,
  global tunables) — one creator, no contention;
- the **SlapdDatabase** controller writes and converges only *its own entry*
  (`cn=repl-<dbname>,cn=slaptain-auth`) inside it, and defers with a retry when
  the database is not there yet.

The entries are per-database and singly-owned; the container is per-pod and
singly-owned. No reconcile ever needs evidence about another database.

## Consequences

- **ADR-008 needs amendment.** Its "create-only, never rotates" model and its
  "copy the Secret between clusters before deploying the CR" instruction both
  change: the Secret must still be uniform mesh-wide (every consumer presents it
  to every provider), but the *entry* is no longer a mesh-converged object, and
  rotation becomes supported.
- **ADR-020's grant identity changes** from `cn=replication,<suffix>` to
  `cn=repl-<dbname>,cn=slaptain-auth` on both the data database and its journal.
  The rule is unchanged; the DN it names is not.
- **The operator's CSN monitoring bind** (ADR-008) switches to the new DN.
- **This is a breaking change to an existing mesh.** The graceful path is
  additive-then-subtractive across two releases: (1) create the auth database and
  entries on every pod; (2) add ACL grants for the new DN *alongside* the old;
  (3) switch stanzas; (4) remove the old grants only once every site is upgraded.
  Steps 1-3 leave the old identity working, so a partially upgraded mesh keeps
  replicating. At alpha a coordinated break or a wipe-and-recreate (ADR-012) is
  also acceptable and much simpler; the phased path is documented so the choice
  is deliberate.
- **The old `cn=replication,<suffix>` entry is left in place.** Deleting it is a
  write into the replicated tree — exactly the shared-state action ADR-026 R2
  says needs ownership we do not have. Removing its *ACL grants* is `cn=config`,
  node-local and ours, so the entry becomes inert; the entry itself is a
  documented manual cleanup.
- **It does not help the ADR-011 foreign-provider case.** Consuming from a legacy
  provider means authenticating however that provider supports, against an entry
  in *its* DIT, created and owned by its administrator. Out of scope by nature.
- **RO pods** get the database too, for uniformity; only RW pods actually verify
  incoming binds against it.
- **What does not change:** the stanza still carries a plaintext `credentials=`
  (that is inherent to simple bind), the Secret layout, the RID scheme, the
  accesslog design, `cn=config` being node-local, and every replication-path byte
  other than the `binddn`.

## Related

- ADR-002 — `cn=config` is node-local and operator-converged per pod; this ADR
  extends that model to the replication identity.
- ADR-008 — the credential model this amends; its uniform-password assumption is
  what the current placement fails to enforce.
- ADR-011 — hot migration; the foreign-provider case this does not address.
- ADR-013 — the `DATABASE_DIRS` rolling-restart wart, and why decision 3 does not
  add to it. See also `docs/BUG-ANALYSIS-database-dirs-rolling-restart.md`, whose
  open question 2 (sacrifice distroless for a controller-side `mkdir`) is answered
  "no" and remains so.
- ADR-018 — PVC deletion leases; decision 4 adds no new volume.
- ADR-019 / ADR-020 — the per-database journal and its access rule, whose granted
  identity this renames.
- ADR-026 — R2 is the rule that makes the status-quo repair unsafe and this
  placement safe.

## Follow-ups recorded separately

- **Diagnostics leak the credential in plaintext.** `slctl debug-dump` collects
  syncrepl stanzas and e2e prints them on failure, so the replication password
  appears in artifacts people share (verified: it is in a captured `fails.log`).
  Redact `credentials=` in diagnostic output. Independent of this ADR and worth
  doing regardless of which option is taken.
- **Correct the "mTLS peer auth" claim in CLAUDE.md** — client certificates are
  presented but never verified (`olcTLSVerifyClient` unset), so nobody builds a
  security argument on a property we do not have.
