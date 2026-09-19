# ADR-029: TLS is on by default, and a missing certificate is a legible state

**Status:** Partially accepted (the gate, v0.2.1) / Proposed (the default flip)
**Date:** 2026-09-19

## Context

Two defects meet in the same field, and they were found together while making the
Helm charts stop restating the operator's defaults (0.2.0).

**The operator never looks at the TLS Secret.** `spec.ldap.tls.secretName` is
passed straight into the pod's volume definition (`slapdcluster_controller.go`,
the single reference to the field) and is never read, never validated, never
reported on. Name a Secret that does not exist and every pod sits in
`ContainerCreating` indefinitely while the `SlapdCluster` says nothing at all:
no condition, no phase, no event. The diagnosis lives in `kubectl describe pod`,
which is precisely where a user who deployed a CR is not looking.

**TLS is off by default in the API and on by default in the chart.** The CRD
defaults `enabled: false`; `charts/slapd` sets `enabled: true` with
`secretName: slapd-tls`. So the product's own answer for a directory server is
plaintext, and the secure posture exists only for people who install through that
one chart. 0.2.0 established the opposite rule for every other field — if a chart
must carry a value for the product to be reasonable, the product's default is
wrong, not the chart — and TLS is the last place the rule is not applied.

The second defect also makes the first one worse: the chart's default names a
Secret (`slapd-tls`) that a fresh user has no reason to have yet, so the most
likely first experience of the most likely install path is a cluster that hangs
with no explanation.

## Options considered

1. **Status quo — the chart holds the opinion, the operator validates nothing.**
   Rejected. It leaves the "supported, documented, silent" state that this
   project refuses elsewhere, and it contradicts the rule the rest of 0.2.0
   follows.

2. **Validate at admission (CEL / a webhook).** Rejected as insufficient rather
   than wrong. CEL cannot see other objects, so it cannot check that a Secret
   exists; a validating webhook could, but only at write time — a Secret deleted
   or created afterwards would never be noticed, and the interesting case is
   exactly the one where the Secret arrives later (cert-manager issuing, an
   operator installed before its prerequisites). The check has to be part of
   reconciliation, where it re-runs.

3. **Gate only: validate the Secret, leave the default off.** The gate alone is
   the whole safety improvement, and it needs no API change. Rejected as
   incomplete: it fixes the symptom of a chart-held default without moving the
   default, so `charts/slapd` keeps holding the one opinion that 0.2.0's own rule
   says belongs in the operator.

4. **Gate + TLS on by default.** Chosen.

5. **Flip `+kubebuilder:default` on `enabled` to `true`.** Rejected as not
   actually working. `Enabled` is a plain `bool`, so unset and explicit `false`
   are the same value, and a CRD default is only materialised when its PARENT
   object is present: a CR that omits `ldap:` entirely would still get `false`.
   Defaulting that fires for some CRs and not others is worse than either
   default, because it is invisible.

## Decision

**TLS is on unless the user turns it off, and a certificate that is not there
yet is a named, self-healing state rather than a hung pod.**

1. `SlapdTLSConfig.Enabled` becomes `*bool`. Unset means enabled; an explicit
   `false` disables. The pointer is what makes "the user chose plaintext"
   distinguishable from "the user said nothing", the same reason
   `SlapdMeshSite.ServerIDIndex` is a pointer (ADR-028) and `logLevel` is.
   Resolution happens in the operator, not the CRD — one source, at reconcile
   time, so a later release's answer reaches existing clusters (ADR-024's rule
   for slapd tunables, applied to slaptain's own).

2. `secretName` unset defaults to `<cluster name>-tls`, derived by the operator
   the way `spec.images` is derived from the operator's own image. No literal
   default baked into the CRD.

3. **The certificate gate.** Before rolling the StatefulSet, when TLS resolves to
   enabled, the cluster controller reads the named Secret and requires `tls.crt`
   and `tls.key` (`ca.crt` stays optional — ADR-007's public-CA case). On failure
   it sets `TLSReady=False` with a reason naming the cause
   (`SecretMissing` / `SecretIncomplete`), sets the phase to `Pending`, emits an
   event, and requeues. It does not create or modify the StatefulSet.

4. **Never destructive.** For a cluster that is already running, a Secret that
   disappears is reported and nothing else: the pods have it mounted and are
   serving, and tearing down a working directory because a prerequisite object
   was deleted would turn a cosmetic problem into an outage. The gate withholds
   work; it never undoes it. This is the same shape as ADR-027's identity gate,
   which withholds syncrepl stanzas rather than removing them.

5. The charts then render **nothing** for TLS by default, and `charts/slapd`
   holds no opinions at all — every field it renders is one the user set.

## Consequences

- **A bare `SlapdCluster` no longer runs without a certificate.** That is the
  point, and with the gate it fails legibly: `kubectl get slapdcluster` shows
  `Pending` and the condition names the missing Secret. Users who want plaintext
  write `enabled: false`, which is one line and now an explicit choice.
- The quick start must provide a cert before the CR — it already documents
  `make gencert`, cert-manager and a private PKI, but the ordering becomes load
  bearing rather than advisory.
- `operator/config/samples/` and any doc showing a minimal CR need a cert step.
- The two e2e specs that deliberately run TLS-less (`restore_inplace_test.go`,
  `scaleup_test.go`) set `Enabled: false` explicitly and are unaffected — but
  they will need `*bool` at the call site.
- `*bool` is an API shape change. At v1alpha1 that is acceptable; existing stored
  objects carry an explicit `enabled` value already (the CRD default materialised
  it), so they keep the posture they have.
- New coverage required before this lands, red first: a unit table for the
  resolution (unset → on, explicit false → off, secretName derivation) and an
  e2e that creates a TLS cluster with no Secret, asserts `TLSReady=False` and no
  StatefulSet roll, then creates the Secret and asserts the cluster proceeds.
  The self-heal is the half that a unit test cannot show.

## Related

- ADR-024 — unset means slaptain's default, and a field that cannot be honoured
  is reported rather than ignored. This is that rule applied to TLS.
- ADR-027 — the identity gate: withhold work and report, never tear down.
- ADR-025 — the withhold belt, same shape.
- ADR-007 — the TLS posture these certificates serve, including the public-CA
  case that makes `ca.crt` optional.
- ADR-028 — `*bool`/pointer-for-unset precedent (`serverIDIndex`).

## Amendment, 2026-09-19: the gate ships without the default flip

The two halves of this ADR were separated on the way to v0.2.1, because they
have opposite risk profiles.

**The gate is a pure win and shipped in v0.2.1.** It changes no default and no
API shape: a cluster that asks for TLS and has no certificate reports
`TLSReady=False` with a reason that names the Secret, and withholds only the
StatefulSet. Nothing that worked before behaves differently.

**The default flip did not ship, and should not until the operator can issue
certificates itself.** Turning TLS on by default without an issuance story means
every minimal `SlapdCluster` stops at `TLSReady=False` until a human produces a
certificate — deliberately making the first run worse, to fix it a release
later. The flip costs nobody anything the moment slaptain can issue its own
material (the CloudNativePG/Strimzi pattern: an operator-managed CA, server
certs derived from names the operator already knows, and rotation). The two
belong in the same release.

Three findings from the certificate survey that motivated the split, recorded
because each one closes off an option that looks open:

1. **The cluster root CA cannot be the default path.** Issuing from it means the
   `kubernetes.io/kubelet-serving` signer, which is unavailable on several
   managed providers, requires manual approval (a privilege), and never renews.
   Decisively: the ecosystem's approver for that signer,
   kubelet-csr-approver, requires the CSR CommonName to equal the *requester's*
   username and permits at most one DNS SAN — our CSRs satisfy neither, and it
   **denies** by default rather than ignoring. A denied CSR cannot be approved
   afterwards. So in a cluster running the standard approver for that signer,
   the technique stops working. It stays documented as what it is: a
   convenience with free in-cluster trust, used by the test suite, not the
   recommended path (docs/TLS.md §3).

2. **A CLI subcommand was considered and rejected.** `slctl tls issue` would
   have re-implemented `tests/gencert.sh` in Go. It buys nothing over the shell
   for the crypto, and inherits the same defect — a one-shot with no renewal.
   If slaptain should work out of the box with TLS, the operator must issue and
   *rotate*, which a CLI cannot do. Reimplementing a lab tool as a first-class
   CLI surface would also make it look like a production path.

3. **No Kubernetes Event.** The decision above said the gate would emit one. It
   does not: no controller in this operator has an EventRecorder, and the
   condition plus the withholding log line is how every other gate here reports
   (mesh resolution, the ADR-027 identity gate). Adding the wiring and RBAC to
   say a third time what the condition already says was not worth it.

The phase behaviour was also made more precise than "Pending" (a phase this API
does not have). A cluster whose StatefulSet does not exist yet reports
`Bootstrapping` — it is waiting on a prerequisite and has never served. A
cluster that already has one keeps the phase its pods justify, `Running` or
`Degraded`, with `TLSReady` carrying the news: calling a healthy directory
`Error` because a Secret was deleted would be a false alarm.
