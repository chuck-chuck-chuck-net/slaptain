# Development Guide

How to build, run, test, debug and change slaptain: which command does what, in
which order, and where the real reference lives. This guide does not re-explain
LDAP or the operator model — [`ONBOARDING.md`](ONBOARDING.md) does that — and it
does not restate the project's conventions and discipline in full, which live in
[`CLAUDE.md`](../CLAUDE.md) (written for coding agents, binding for humans too).

---

## Prerequisites

| Tool | Why |
|---|---|
| `podman` | Image builds. `CONTAINER_ENGINE=docker` switches engines; nothing in the Containerfiles is podman-specific |
| Go 1.25.3+ | Two modules: the operator and the e2e suite. `operator/go.mod` carries the authoritative version |
| Helm 3 | Every deployment path is Helm. There is no kustomize in this repo |
| `kubectl` | Contexts for each cluster you target |
| `yq` v4 | Only for `lab.yaml` — `tests/e2e.sh` refuses to read a config file without it |
| `ldap-utils` | `slctl ldapsearch` wraps the system binary; `tests/e2e-storm-repro.sh` needs it too |

Kubebuilder, controller-gen, setup-envtest and golangci-lint are **not**
prerequisites: `operator/Makefile` downloads pinned versions into
`operator/bin/` on first use.

Every target that touches a cluster takes `CONTEXT=<name>`, which threads
`--context` / `--kube-context` through every `kubectl` and `helm` call. Without
it they use the current kubeconfig context.

On the cluster side you need a **default StorageClass** — persistence is
mandatory since [ADR-013](adrs/adr-013-defer-hot-database-management.md), the
`persistence.enabled` field is gone — and a registry the cluster can pull from.
If it cannot pull from anywhere, `DELIVERY=import` ships images to the nodes
over SSH instead (see below).

---

## Repository map

Two Go modules, deliberately separate: `operator/`
(`github.com/chuck-chuck-chuck-net/slaptain/operator`) holds the kubebuilder v4
operator plus the `slctl` CLI under `cmd/slctl/`, and `tests/e2e/` is its own
module so the test suite's dependencies never leak into the shipped binary.

`images/` holds five build contexts: `slapd` and `slapd-init` (each with a
`Containerfile` for OpenLDAP 2.7.1 and a `Containerfile.ol26` for the legacy
2.6 pair), `slapd-toolkit`, `operator`, and `openldap-deb` — the vendored
Debian packaging fork that produces the 2.7.1 `.deb` files
([README](../images/openldap-deb/README.md),
[ADR-021](adrs/adr-021-openldap-2.7-dual-images.md)).

`charts/` holds four: `operator` (the operator itself, with the generated CRDs
under `crds/`), `slapd-cluster` (a `SlapdCluster` CR — this is what the tests
deploy), `slapd-toolkit` (debug pod), and `slapd` (the pre-operator standalone
chart, kept for reference only, not exercised by the suite).

`tests/` holds the Ginkgo suite (`tests/e2e/`), its fixtures
(`tests/resources/{example,lab,storm-repro}/`) and the orchestration scripts —
`e2e.sh` for the normal cycle, `e2e-migration.sh` and `e2e-storm-repro.sh` for
the two scenarios that need their own topology.

---

## Building images

```bash
make all          # six images + slctl
make push         # build, then push all six
make show-tag     # print the tag everything will be built and deployed with
```

The six pushable images are the 2.7.1 `slapd` / `slapd-init` pair, the `-ol26`
2.6 pair, `slapd-toolkit` and `operator`. A seventh, `openldap-deb`, is
local-only (`localhost/` prefix, `FROM scratch`, nothing but `/debs`): both 2.7
Containerfiles consume it via `--build-arg OPENLDAP_DEB_IMAGE`, so the package
build runs **once per tag** and feeds both. It is stamp-cached like everything
else under `.stamps/`, which is why a second image build after the first is
fast. `RUN_UPSTREAM_TESTS=1` adds OpenLDAP's own test suite to that build; it
costs tens of minutes and is off by default.

Individual targets: `build-init`, `build-slapd`, `build-ol26` (both legacy
images), `build-toolkit`, `build-operator`, `build-openldap-deb`, `build-slctl`
(→ `bin/slctl`, plus `install-slctl` for `/usr/local/bin`).

### The tag

`GIT_TAG` is derived, not configured — by `scripts/image-tag.sh`, the single
source shared by the Makefile and `tests/e2e.sh`: an exact git tag if `HEAD` is
on one, otherwise the short commit hash, and on a dirty working tree
`<hash>-dirty-<contenthash>`. The dirty suffix hashes the actual diff, so the
same dirty state always derives the same tag (builds and e2e runs agree),
while a different edit derives a different one — a stale image can never
masquerade as your current tree. Untracked files are invisible to the tag
(`git add -N` a new file if it must count before its first commit).

Deploying still requires the tag to have been built and delivered, so
`tests/e2e.sh setup` probes the registry first and fails immediately — with
the fix spelled out — when the derived tag was never pushed. The two working
loops:

```bash
make push && ./tests/e2e.sh all <context>   # same tree state → same tag, just works

make push                                   # …or note a tag once
GIT_TAG=<pushed-tag> ./tests/e2e.sh all <context>   # …and pin it while editing
```

Override `REGISTRY` (default `ghcr.io/chuck-chuck-chuck-net`) and `PROJECT` for
your own registry; images are always `$(REGISTRY)/$(PROJECT)/<image>:$(GIT_TAG)`.

### Delivery: push or import

`DELIVERY=push` (default) pushes to the registry. `DELIVERY=import` saves each
image and loads it into every node's CRI over SSH — for clusters with no
reachable registry:

```bash
make import CONTEXT=<context>        # init, slapd, toolkit, operator
make import-ol26 CONTEXT=<context>   # the legacy pair, separately
```

The import checks `kubectl get nodes` for the tag first and skips when it is
already there; with content-derived tags, a tag match is a version match. SSH
user and CRI flavour come from node annotations, so you set them once per
cluster rather than on every invocation:

```bash
kubectl annotate nodes --all slaptain.chuck-chuck-chuck.net/cri=crio
kubectl annotate nodes --all slaptain.chuck-chuck-chuck.net/node-user=<user>
```

Precedence is CLI/env > annotation > default (`containerd`, `debian`). Node
addresses come from the k8s `InternalIP` unless you pass `NODE_IPS="..."`.

### When you need the `-ol26` pair

Only for legacy interop: a hot migration whose source is an OpenLDAP 2.6
deployment ([ADR-011](adrs/adr-011-hot-migration-topology.md)), or a run that
deliberately exercises the ITS#9580 behaviour 2.7 fixes. Pin **both** tags or
neither — 2.6 and 2.7 do not share an on-disk format. Deployment-side, that is
`SLAPD_TAG_SUFFIX=-ol26`, which touches only the slapd pair and leaves operator
and toolkit on the plain tag. The full version and tag contract, including the
2.6 → 2.7 migration runbook, is [`OPENLDAP-VERSIONS.md`](OPENLDAP-VERSIONS.md).

---

## Running the operator

### Deployed (Helm)

```bash
make operator-helm-install CONTEXT=<context>
make deploy-operator CONTEXT=<context>   # build + deliver + upgrade + rollout restart
```

`operator-helm-install` depends on `operator-crd-apply` because **Helm installs
a chart's `crds/` only on the first `helm install`, never on `helm upgrade`**.
Upgrade with a raw `helm upgrade` and a new or changed CRD silently stays stale,
after which the operator ignores fields you know you set. `operator-crd-apply`
applies
`operator/config/crd/bases/` server-side (also sidestepping the client-side
last-applied-annotation size limit the large CRDs would hit).

### Local (no image build)

```bash
make operator-crd-apply          # CRDs must exist in the cluster
cd operator && make run          # manifests + generate + fmt + vet, then go run ./cmd/main.go
```

It runs against your current kubeconfig context with your own credentials.
Scale any deployed operator to zero first — two managers reconciling the same
CRs is a fight you do not want to debug.

### Changing the API types

```bash
make operator-generate           # zz_generated.deepcopy.go
make operator-manifests          # CRD YAML + RBAC role, then syncs CRDs into charts/operator/crds/
```

House rule: the generated CRD under `operator/config/crd/bases/` and its copy
under `charts/operator/crds/` are **committed together with the type change**.
A chart carrying a CRD older than the code it deploys is the same failure mode
as the upgrade gotcha above, just committed.

### Gates

```bash
cd operator && make build        # compile
cd operator && make test         # envtest (auto-downloads the binaries) + fmt + vet
cd operator && make lint         # golangci-lint, pinned version, auto-installed
```

---

## Deploying a cluster to hack on

```bash
make gencert CONTEXT=<context>            # creates the namespace + the slapd-tls Secret
make cluster-helm-install CONTEXT=<context>
make testing-apply CONTEXT=<context>
```

`gencert` creates `slaptain-testing` if needed and is idempotent.
`cluster-helm-install` deploys `charts/slapd-cluster` with
`tests/values.slapd-persistent.yaml` (3 RW pods, 1 RO replica, replication on)
and pins the slapd image tags to `$(GIT_TAG)$(SLAPD_TAG_SUFFIX)`.
`testing-apply` applies `tests/resources/$(TEST_RESOURCES)/` — the `SlapdSchema`,
`SlapdDatabase` and readpw Secret fixtures. Note the two defaults differ:
the Makefile defaults `TEST_RESOURCES` to `lab`, `tests/e2e.sh` to `example`.
Pass `TEST_RESOURCES=example` for the open-source fixtures.

Minimal hand-rolled alternative: the sample CRs under
`operator/config/samples/` (`ldap_v1alpha1_slapdcluster.yaml` and siblings).

The operator generates the credentials rather than taking them from you: the
cluster controller creates `<name>-config-password` (the `cn=config` admin), the
database controller creates `<dbname>-credentials` (data admin + replication
bind). Both hold plaintext, which the operator reads back whenever it talks
LDAP. The layout and the bring-your-own-credentials path are in
[`ONBOARDING.md`](ONBOARDING.md#secret-and-credential-model) and
[`BOOTSTRAP.md`](BOOTSTRAP.md).

Tear down with `make testing-delete cluster-helm-uninstall`.

---

## The e2e cycle

[`tests/README.md`](../tests/README.md) is the authority — gates, env vars,
fixtures, topologies, troubleshooting. The short version:

```bash
./tests/e2e.sh config                    # resolved config, touches no cluster
GIT_TAG=<pushed-tag> ./tests/e2e.sh all <context> [more-contexts...]
```

One context (or none — the current one) is single-site; two or more is
multi-site with a full external-peer mesh and the cross-site specs enabled. The
script provisions NodePorts, exports the addresses the suite needs, runs
`go test ./tests/e2e`, and tears the NodePorts down. `setup` / `test` /
`teardown` split it when you want to iterate: set up once, re-run `test`.

Describe the lab once in `lab.yaml` at the repo root (gitignored;
`E2E_CONFIG=<path>` overrides) instead of carrying contexts and node-access IPs
on every invocation — schema and comments in
[`lab.yaml.sample`](../lab.yaml.sample). It supplies lab facts only; per-run
knobs (`GIT_TAG`, `SLAPD_TAG_SUFFIX`, the `E2E_*` gates) stay on the command
line, and environment variables always win over file values.

Against an already-deployed cluster, `make e2e-run` runs the suite directly.
Iterating on one area, three flags matter: `E2E_LABEL_FILTER` (a Ginkgo label
expression, e.g. `accesslog` or `backup && !restore`), `FAIL_FAST=1` (stop at
the first failure instead of letting the cascade bury its cause — nothing is
torn down, so the cluster is left ready for `slctl`), and `E2E_SEED` (the
suite shares one mutable cluster, so spec order matters; replaying the logged
seed reproduces a run exactly). Heavier scenarios are behind their own gates —
`E2E_BACKUP`, `E2E_EXTERNAL_REPL`, `E2E_RESILIENCE`, `E2E_SCALEUP` — all
documented in `tests/README.md`.

Two scenarios bring their own topology and have their own scripts:
`tests/e2e-migration.sh` (consumer-only → peer promotion, wrapped by
`make e2e-migration`) and `tests/e2e-storm-repro.sh`, which manufactures the
ITS#9580 conditions on an `-ol26` mesh and exits `0` when it finally gets the
no-storm assertion red, `3` on an honest negative.

---

## Debugging

**Read [`reconcile-loop-fixes.md`](reconcile-loop-fixes.md) first.** It is the
log of every reconcile and replication bug found so far, with the symptom, the
root cause and why it was hard to spot. A surprising fraction of new symptoms
are an old entry wearing a new hat, and at least one entry exists specifically
to stop the next person re-attempting an optimisation that breaks convergence.

`slctl` ([`slctl.md`](slctl.md), `make build-slctl`) is the first tool to reach
for:

| Command | Use |
|---|---|
| `slctl status` | Phase, replicas, conditions — the 5-second look |
| `slctl inspect` | Per-pod LDAP queries plus CSN-convergence, topology and stanza-count checks; non-zero exit on failure |
| `slctl debug-dump` | Everything (CRs, pod logs, LDAP state, events, operator logs) into a timestamped directory |
| `slctl ldapsearch` | `ldapsearch` with the endpoint and bind identity resolved for you; `--as config\|replication` to read an accesslog, `--verbose` to print the equivalent raw command |
| `slctl debug` | Attach a toolkit container to a running slapd pod, adapting to the namespace's PodSecurity level |

The slapd image is distroless and has no shell, so interactive work goes
through the toolkit image: `make toolkit-install` for a persistent pod with
credentials and the CA pre-wired, or `tests/pod-debug.sh` for an ephemeral
`kubectl debug` container with the low-level capabilities.

Operator logs: `kubectl logs -n slaptain-system deploy/slaptain-operator`. slapd's own
logs use hex epoch timestamps — pipe them through
`tests/decode-slapd-ts.sh` to get ISO 8601. Raising slapd's verbosity is
`spec.logLevel` on the `SlapdCluster` (the e2e fixture uses 16640: sync debug
plus stats, which several specs depend on).

---

## Discipline

Three rules carry more weight here than in most repos. [`CLAUDE.md`](../CLAUDE.md)
states them in full; the short form:

### ADRs are first-class

[`docs/adrs/`](adrs/) records the decisions that constrain what you may build.
Check for a relevant one before designing, and follow it if it exists. If your
change conflicts with one, discuss it before writing code rather than silently
diverging. A decision whose scope grows gets an amendment with its date, never a
rewrite — the history of the reasoning matters as much as the conclusion.

### Fix Discipline

A fix lands only on a demonstrated root cause — logs, live cluster state, a
reproduction — never a plausible theory. Prefer the explanation that also
accounts for why it used to work. Map the blast radius against the ADRs and
`reconcile-loop-fixes.md`, and confirm the default path stays unchanged. Every
reconcile or replication fix gets an entry in that log.

### Test Discipline: red first

Every test must have been observed to fail, for the right reason, at least once
before it counts as coverage. A test written alongside the code it checks
mirrors that code's assumptions, bugs included — it passes, and it was never
shown to catch anything. Write the assertion from the ADR, watch it go red, then
implement. Where a test genuinely cannot go red, say so in writing: ADR-021's
ITS#9580 tripwire is on record as never having been observed red, with the
measurements that explain why, instead of being presented as proven coverage.
Behaviour-preserving refactors under green tests need no new red.

---

## Which document answers what

| Document | Answers |
|---|---|
| [`ONBOARDING.md`](ONBOARDING.md) | LDAP and OpenLDAP concepts, the operator model, the credential model, common operations |
| [`BOOTSTRAP.md`](BOOTSTRAP.md) | How a cluster comes up: init container vs operator phases, credential flow, peer discovery |
| [`REPLICATION.md`](REPLICATION.md) | Delta-syncrepl, the RID scheme, in-cluster and cross-site topologies |
| [`BACKUP.md`](BACKUP.md) | `SlapdBackup`, scheduled backups, restore into a fresh DB, in-place rollback |
| [`OPENLDAP-VERSIONS.md`](OPENLDAP-VERSIONS.md) | Which image is 2.7 vs 2.6, how to pin, the 2.6 → 2.7 migration runbook |
| [`slctl.md`](slctl.md) | The CLI: every command, flag and check, and how to read contextCSN |
| [`reconcile-loop-fixes.md`](reconcile-loop-fixes.md) | Past reconcile/replication bugs — read before debugging a new one |
| [`MIGRATION-PLAN.md`](MIGRATION-PLAN.md), [`MIGRATION-HOT.md`](MIGRATION-HOT.md), [`MIGRATION-LEGACY-SOURCE.md`](MIGRATION-LEGACY-SOURCE.md) | Replacing a legacy OpenLDAP: phases, hot-migration mechanics, source-side prep |
| [`BACKLOG.md`](BACKLOG.md) | Cross-cutting tech debt not tied to a plan or an ADR |
| [`adrs/`](adrs/) | Why the design is what it is |
| [`../tests/README.md`](../tests/README.md) | The test suite: gates, env vars, fixtures, multi-site setup |
| [`../CLAUDE.md`](../CLAUDE.md) | Conventions, discipline, and the agent-facing project summary |
