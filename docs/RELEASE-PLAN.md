# Release plan: 0.4.0 and the three releases after it

Written 2026-10-22, at the close of the session that planned it. Nothing here
has been executed. It exists so the next session can confirm, re-litigate or
run it without re-deriving the reasoning — every decision below has an ADR or
a measurement behind it, and the ones that are still open say so.

## Where the tree stands

- `feat/accesslog-purge-phase` — 69 commits ahead of `origin/main`, clean,
  implementation complete and measured. This is 0.4.0.
- `docs/its9398-research` — **MERGED into local `main` 2026-10-22** (`c696fcc`),
  ahead of 0.4.0 rather than in the 0.5.0 slot this plan first gave it: the
  conflict surface against `main` turned out to be two append-at-EOF doc hunks,
  not the three-file tangle expected against the 0.4.0 branch. ADR-021's
  retraction is therefore on `main` now, so `main` no longer states a reason
  for its own default that we know to be wrong. Two notes from the merge: the
  branch's history was **rewritten** first (it carried lab cluster context
  names predating the blacklist entry — `backup/its9398-prerewrite` holds the
  pre-rewrite tip), and putting the documents side by side exposed a
  disagreement about the purge's role in the storm, recorded in `BACKLOG.md`
  rather than resolved.
- `feat/rid-allocation-ledger` — 28 commits, unmerged, 152 behind.
  **Rejected 2026-10-22, will not be merged** (see 0.5.0). Kept as a record.
- `backup/adr-034-prerebase`, `feat/adr-034-startup-only-config`,
  `fix/own-sid-watermark-gap` — all merged into `origin/main` by patch-id;
  safe to delete.
- 20 `worktree-agent-*` branches, 9 with uncommitted leftovers from the purge
  work. `git worktree prune` once 0.4.0 is out.

## 0.4.0 — the purge rotation

Breaking for everyone, including non-adopters: `accesslogPurge` becomes an
object, and a **stored** string fails the typed LIST for the whole result set,
stopping `SlapdDatabase` reconciliation cluster-wide in every namespace. Not a
patch release under any reading. `docs/UPGRADING.md` already carries the
migration under a heading that still says `## Unreleased`.

**Status 2026-10-22: items 1-7 are done; only the tag remains.** The 0.4.0
branch is merged into local `main` and nothing is pushed.

**The multi-site validation, in full.** Three cycles on a three-site mesh,
pod-routed, images at `sha-0582604`.

| cycle | result |
|---|---|
| rotation OFF | 107 passed / 2 failed / 7 skipped |
| rotation ON (`E2E_ROTATION=1`) | 101 passed / 3 failed / 12 skipped |
| `write-rotation` only, after the fixture fix | **6 passed / 0 failed** |

**No product regression in any of them.** The reds, each run to ground:

- *hot-add* and *exporter sidecar* (cycle 1) — collateral. `external_replication`
  removes site-3 from the `SlapdMesh` and restores it, and because peer CA
  volumes live in the pod template that rolls every pod twice; the hot-add spec
  snapshotted mid-roll. The spec predates this branch by a month.
- *dataloss recovery* (cycle 2) — the ITS#9398 storm, in the configuration we
  knowingly ship. See the known-issue entry in `CHANGELOG.md`.
- *exporter sidecar* (cycle 2) — collateral of that storm: ADR-030's probe
  correctly ejected a pod that was no longer answering LDAP. The probe working,
  not failing.
- *write-rotation BeforeAll* (cycle 2) — **the one genuine defect, and it was in
  the test.** Fixed in `129b774`; see cycle 3.

**One delta between the validated tree and the tag.** Three dependabot patch
bumps landed on `origin/main` while the validation ran — aws-sdk-go-v2
1.47.2→1.47.3 (s3 1.114.2→1.114.3) and ginkgo 2.33.0→2.33.1 in both modules —
and were merged in before tagging. They were re-checked by build, vet, the unit
suites (`internal/backup` included, which is the SDK's only consumer),
`helm-lint` and the generated-files gate. They were **not** put through another
mesh cycle; patch releases of an S3 client and a test framework did not seem
worth three more cycles, and that judgement is recorded here rather than left
implicit.

**Two things the validation taught that are not about this release.** The suite
had never been run against a cluster *deployed* with the rotation on, which is
how the fixture gap survived. And a mesh membership change rolls every pod at
every other site — a pod-template roll trigger ADR-013 does not list.

Pre-tag checklist:

1. **Finalise `images/openldap-deb/debian/changelog`** — the top entry is
   `2.7.1-0+slaptain3 UNRELEASED`. Rebuild and confirm `slapd -VV`, because the
   revision is what `AccesslogPurgeConverged=False/PurgePhaseUnsupported` tells
   operators to check. Since 2026-10-23 the token names the flavour: the
   patched build reports `+slaptain3` and the pure one `+pure3`, derived from
   `SLAPTAIN_PATCHES` and asserted by the build's version/flavour gate. `N` is
   still yours to bump by hand, and both flavours share it.
2. `docs/UPGRADING.md`: `## Unreleased` → the release heading. (0.5.0's is already written as `## v0.5.0`.)
3. **Experimental framing** into the `SlapdRotationConfig` field documentation
   and the head of `docs/PURGE-ROTATION.md`, per ADR-039 tier 2. Regenerate
   CRDs (`make operator-manifests`) so it reaches `kubectl explain`.
4. **Release notes.** `publish.yml` builds a hardcoded body plus GitHub's
   `generate_release_notes`; there is no `CHANGELOG.md`. With ~69 narrative
   commits the generated half is unreadable, and nothing currently tells a
   reader that this release carries a breaking change and an experimental
   feature. Add a per-release section to the body: the break and its migration
   pointer, the experimental feature, the non-upstream patch disclosure, and a
   call for feedback naming the arms nobody has run.
5. **ADR-037 stays `Proposed`** — deliberately, and worth a line saying so, since
   four other ADRs are stale in the other direction (004, 005, 006, 007 are all
   implemented and still say Proposed).
6. ~~**Decide what to say about the purge and the storm.**~~ **SETTLED
   2026-10-23**, from the source read this item asked for. A sweep that matches
   nothing writes nothing (`accesslog.c:679-691`, `:752`), and
   `accesslog_response` advances `minCSN` on the first logged write carrying a
   new serverID with no sweep involved (`:2113-2159`). So the purge is
   sufficient, never necessary, and **ADR-037's slot length is not a
   storm-exposure knob** — its purge half is a write-stall mechanism only
   (ADR-037 eighteenth amendment). The corrected account is in
   `docs/ITS-9398-REPRODUCTION.md`; the release notes say what the checkpoint
   default does and what two clean cycles do and do not establish.
7. ~~`make helm-lint verify-generated`, then a **multi-site e2e cycle**~~ —
   done 2026-10-22, see the status block above. Run the rotation cycle with
   `E2E_ROTATION=1` or the feature's own specs never execute.
8. Chart `version`/`appVersion` → the release version in all four charts, bump
   commit, then tag. **Done for 0.5.0.**

   Worth knowing why this is not cosmetic, because the symptom is quiet. A
   packaged chart gets its version from the Makefile (`--version`/`--app-version`
   off the git tag), and `make operator-helm-install` pins `image.tag`
   explicitly — so both of those paths are correct whatever `Chart.yaml` says.
   What is NOT correct is a plain `helm install ./charts/operator` from a
   checkout: `image.tag` falls back to `.Chart.AppVersion`, and so does
   `OPERATOR_IMAGE_TAG`, which is the operator's `DefaultImageTag` for
   *unpinned data-plane images*. A stale `Chart.yaml` therefore runs the
   previous release's operator AND hands the previous release's slapd pair to
   every cluster that did not pin — from a tree that looks like the new one.
   Caught during 0.5.0 prep by a `helm upgrade --reuse-values` reporting 0.4.0.

Deliberately **not** in 0.4.0: any default change, the image scheme, the
rotation decomposition. The release already carries one forced migration.

### How a release lands on `main`

**By rebase, never by merge.** The `protect-main-branch` ruleset rejects a push
carrying a merge commit, and it is a *ruleset* — the classic
`branches/main/protection` API reports `404 Branch not protected`, which is
misleading. v0.4.0 was assembled with three local merges, all of which had to be
linearized at push time.

The order that works:

```bash
git fetch origin                      # the remote moves; dependabot lands often
git rebase origin/main                # linearize; expect conflicts where a
                                      #   long-lived branch meets a moved main
git diff <the commit you validated> HEAD   # MUST be empty — see below
```

**The trap, which cost an afternoon.** Rebase drops merge commits, and with them
any content that existed *only* in a merge resolution. Three edits were lost
that way on v0.4.0 — a narrowed claim in the storm spec, an amendment ordering,
a stray heading — and the rebased branch still built and still passed `go vet`.
The only thing that caught it was diffing the rebased tree against the commit
the mesh validation had actually run against. Make that diff a gate, not a
courtesy, and if it is non-empty restore the difference as its own commit rather
than hand-editing history again.

Related: sanitising a branch for the blacklist means **trees and commit
messages**. The first rewrite on v0.4.0 fixed file contents only, and `pre-push`
— which scans every outgoing commit's message too — caught the rest on the
final check.

### Publishing: the workflow works since 2026-10-25; the hand path stays written down

**Resolved for the images, 2026-10-25.** The ghcr image packages were entitled
by hand (the repository added under *Manage Actions access*), and the failed
`publish-main` run for `70dd083` was re-run: attempt 2 succeeded, and the
registry confirms it by a second route — `sha-70dd083` is listed on every image
package, with the `-ol26` and `-ol26src` variants on the slapd pair, so all nine
images landed and not just the first. That is the first publish ever made by
CI in this repository: all twelve `publish-main` runs before it had failed, and
so had the one `publish.yml` run (v0.4.0).

**The charts, entitled 2026-10-26.** `publish.yml` also runs `make
charts-push`, into four packages of their own — `charts/slaptain`,
`charts/slapd`, `charts/slapd-mesh`, `charts/slapd-toolkit` — and every version
on them, 0.4.0 included, was pushed by hand, so they were created by a personal
token exactly as the image packages were; `publish-main` never touches them, so
the successful run above said nothing about them. They are not under the
`slaptain/` prefix, which is why the repository's Packages page did not list
them. On 2026-10-26 the repository was added to all four (and connected to
them: the repository's Packages page now lists nine packages, charts
included). That entitlement is a settings-page fact and has no run behind it
yet — 0.5.0 is the first tag that can show `publish.yml` reaching the end: the
chart push, the GitHub Release, the `slctl` binaries and the CRD bundle have
never been produced by CI.

Nothing below this line should be deleted before the 0.5.0 tag has run
`publish.yml` to the end. The analysis is the record of why, and the four traps
bind any hand push just the same (a lab registry, a hotfix, a rebuild from a
tag).

`.github/workflows/publish.yml` triggers on `push: tags: v*` and looks complete.
Until 2026-10-25 it was not. It was added on 2026-10-11 (`Ci/GitHub actions (#3)`) and **v0.4.0 was
its first execution ever** — the repository has eight version tags and, before
v0.4.0, zero GitHub Releases. Every release so far was hand-pushed.

It failed at the first `podman push`:

```
denied: permission_denied: write_package
  → ghcr.io/chuck-chuck-chuck-net/slaptain/slapd-init
```

All nine images had **built** by then, so this is not a build problem. The ghcr
packages were created by a personal token, and a package created that way does
not grant the repository's Actions token write access: each package needs the
repo added under *Package settings → Manage Actions access* with the **Write**
role (or the equivalent "inherit access from repository"). `permissions:
packages: write` in the workflow is necessary and not sufficient — it scopes the
token, it does not entitle it to a package it does not own.

Until 2026-10-25 that meant publishing from a workstation. The four traps, in
the order they bite, hold for any push made from one:

1. **`REGISTRY` may already be set in your environment**, in which case the
   Makefile's `REGISTRY ?= ghcr.io/chuck-chuck-chuck-net` never applies — `?=`
   yields to it. A plain `make push` then publishes the *release* somewhere
   else, reports success at every step, and leaves ghcr empty. **Pass
   `REGISTRY=` explicitly on the command line**, where it beats both.
2. **Build from a detached checkout at the tag.** The version is derived from
   `git describe`; even one commit past the tag, `make show-tag` yields image
   `sha-<hash>` and chart `0.0.0-<hash>`, and those are what gets published.
3. ~~**Clear `.stamps` when switching registry.**~~ Obsolete since the BuildKit rework: there are no stamps. BuildKit's cache is content-addressed and a registry change is part of the key. The original note, for the record — the stamps were not keyed by
   `REGISTRY`, so a lab build makes `make push` skip the rebuild and then fail
   with "image not known" — or, worse, push something stale.
4. **The exporter has its own version line** (ADR-033). Off an `exporter/vX.Y.Z`
   tag it publishes as `sha-<hash>`, which is correct and will look wrong next
   to nine `0.4.0` images.

The sequence that produced v0.4.0:

```bash
git checkout v0.4.0                       # detached; 1 and 2 above
# (3) no longer needed: no stamps
make show-tag REGISTRY=ghcr.io/chuck-chuck-chuck-net   # expect v0.4.0 / 0.4.0 / 0.4.0
env -u REGISTRY make push          REGISTRY=ghcr.io/chuck-chuck-chuck-net
env -u REGISTRY make charts-package charts-push REGISTRY=ghcr.io/chuck-chuck-chuck-net

# the release the workflow would have made: notes are the CHANGELOG section,
# extracted exactly as publish.yml extracts them
awk -v t="## v0.4.0" '$0==t{f=1;next} f&&/^## /{exit} f{print}' CHANGELOG.md > notes.md
#   ... then append the images / charts / CRD-first / slctl boilerplate
cat charts/operator/crds/*.yaml > dist/slaptain-crds.yaml
cd operator && for os in linux darwin; do for arch in amd64 arm64; do
  CGO_ENABLED=0 GOOS=$os GOARCH=$arch go build -trimpath -ldflags='-s -w'     -o ../dist/slctl-$os-$arch ./cmd/slctl; done; done
CGO_ENABLED=0 GOOS=windows GOARCH=amd64 go build -trimpath -ldflags='-s -w'   -o ../dist/slctl-windows-amd64.exe ./cmd/slctl
gh release create v0.4.0 --title v0.4.0 --notes-file notes.md --verify-tag dist/*
```

Fixing the workflow was a packages-permission change plus one real run, and
the build was not touched — images 2026-10-25, charts 2026-10-26, see the top
of this section. 0.5.0 is the first tag that can show `publish.yml` running to
the end.

### Git history

Keep it. In this repository commit messages are the reasoning record and
`git log` is the only index for them; squashing narrative commits destroys
more than it tidies. Fold only mechanical corrections of an immediately
preceding commit's own text — `08d696d` (1 insertion, 1 deletion) and
`a2d7297` are the two visible candidates. Correction *chains* such as
`5743aba` → `3d0d2b2` stay separate: those are two distinct findings and the
second is the more valuable record. Realistic saving is 69 → ~65, which is not
worth much churn. The higher-leverage move is item 4.

## 0.5.0 — the data-plane release

**One release, not four.** An earlier draft of this plan split what follows
across 0.5.0, 0.6.0 and 0.7.0, on the theory that each default flip deserved
its own release. That was wrong on two counts and is recorded here so nobody
re-derives it.

It cited **ADR-024 R5, "one customer per change"** — which is a rule about not
moving several tunables into a new *placement class* at once, so that a
misbehaviour has one suspect. It is a debugging-isolation rule for `cn=config`
changes and says nothing about release cadence. Stretching it produced a
sequence of releases that would supersede one another within days, which reads
as churn to anyone watching the repository and helps only a user who upgrades
at every single step.

And it confused **ordering** with **packaging**. The constraints below are
real, but every one of them is satisfied *inside* a release: a release is a
snapshot, not a sequence. What has to happen in order is the code landing, not
the tags being cut.

There is also a cost the split ignored. `publish.yml` has never worked (see
above), so **every release is a manual hand-push** — nine images, four charts,
assembled notes, cross-built `slctl` binaries. Four releases is four of those,
for a migration the user experiences once either way.

### What it contains

| | state |
|---|---|
| ADR-039 (two tiers of support) | done |
| ADR-040 (data-plane image identity) | done |
| ADR-037 rotation decomposition — the `mode` enum | done |
| Debian version pin, all three 2.6 paths | done |
| slaptain patches as a build axis + the pure 2.7 pair | done; cycle B validated it (109/2, both explained) |
| ~~the alias inversion — unsuffixed means pure~~ | done — `<tag>` is the pure build, patched moves to `<tag>-ol2.7.1-slaptain`; both in `all`/`push`. Verified on pulled images: default reports `+pure3`, the slaptain cell `+slaptain3` |
| ~~the image **selector** (`spec.images.select.{storageFormat,flavour}`)~~ | done — drives BOTH images, explicit tag still wins, every data-plane call site resolves through one function. Validated live: clearing the pinned tags moved slapd *and* slapd-init together |
| the volume preflight (ADR-041) | **designed, not built** — the measurement moved it from "nice diagnostics" to "the native failure recommends a destructive action" |
| an unhealthy pod blocks its own replacement | **found 2026-10-23, not built** — correcting the spec is necessary and not sufficient; shape 1 (say so in a condition) is small. `docs/BACKLOG.md` |
| ~~selector e2e coverage~~ | done — `image_selector_test.go`: the pin beats the selector, and with the pins cleared one `flavour` edit moves BOTH images to the same tag. Teeth proven by deploying an operator that applies the selector to `slapd` only; the spec went red naming both images |
| ~~the version-string collision~~ | done — the revision token is derived from `SLAPTAIN_PATCHES` (`+slaptain<N>` / `+pure<N>`), asserted by a symmetric build gate; both arms built and measured. The `pure` default is no longer blocked |
| ~~**default OpenLDAP version → 2.6**~~ | **withdrawn** — ADR-040 decision 8 reversed the same day; 2.7 stays. What flips is the default **flavour** to `pure` |
| ~~accesslog `syncprov-checkpoint` on by default~~ | done — `olcSpCheckpoint: 1 1` converged onto every accesslog DB's syncprov overlay, `accesslogSyncprovCheckpoint: none` to opt out. Load-measured as its own arm: no throughput or latency cost, ~7% less replication headroom, on fast local storage. The e2e asserts it on every RW pod, so every run from here is a data point |
| ~~`syncprovSessionlogSource: Accesslog` by default~~ | done — unset now resolves to Accesslog; `InMemory` by name for the old behaviour, and `syncprovSessionlog: 0` still wins. Lands at each pod's next restart (startup-only, ADR-034), so every upgraded cluster reports `StartupConfigConverged=False/RestartRequired` until then — expected, and said so in `UPGRADING.md` |

One `UPGRADING.md` entry, one migration for anyone moving 0.4 → 0.5. The
`## v0.5.0` CHANGELOG section is written (verified against publish.yml's own
awk extraction, so the release will not ship noteless).

### Mesh cycle, 2026-10-23 — the first run that did not end in a storm

Three sites, `sha-02e48b2`: **99 passed, 1 failed, 19 skipped.**

The number this release exists for: **zero stale-cookie lines.** The
`dataloss_recovery` spec wipes a pod's PVCs and lets replication restore it —
the scenario that reproduced ITS#9398 six times, at 2473, 9237, 9358, 4563,
5991 and 6582 lines — and reported `total=0` across all three pods.
`slctl inspect accesslog-mincsn` then read all three sites clean: 6 accesslog
readings each, every `minCSN` sid tracked in `contextCSN`, at all 18
pod×database pairs.

**Data point one, not proof.** The value of shipping the default is that every
run from here adds to the series without anyone doing anything.

First mesh exposure for three other things in this release, all green: pure 2.7
as the DEFAULT build (cycle B validated the pair, never as the default), the
image selector, and the `+pure3` / `+slaptain3` version split on published
images.

### Mesh cycle 2, 2026-10-23 — the storm stays away

Three sites, `sha-8d9b96c`: **98 passed, 1 failed, 22 skipped**, and again
**zero stale-cookie lines** from the PVC-loss recovery, with all three sites
clean on `accesslog-mincsn`. Two consecutive clean cycles against six prior
reproductions. Still not proof; it is the series starting.

First mesh exposure of the session-log default, and it converged: the pods did
take `olcSpSessionlogSource` on a fresh three-site cluster.

The `PurgeOnly` failure from cycle 1 is gone, so that fix held on a mesh.

**Four spec defects have now come out of this one default change, all the same
family: a precondition inherited from the default rather than stated.**
`phaseRefused` read a condition off the wrong resource; `sessionlog` asserted
the in-memory log unconditionally; `startup-config` declared `Accesslog` to
create a divergence that had become a no-op, and then asserted an empty
cluster-wide table while a second database still contributed a line. Every one
passed for as long as the old default kept it on the happy path, and no unit
test could see any of them.

That is an argument for moving a default rather than against it: **the move is
what tests the tests.** It also says something about what a green suite means
on a project whose defaults are still moving — it means the defaults have not
moved, not that the specs would survive it.

### Cycle 1's failure

The single failure was a TEST defect, not a product one, and the new default is
what exposed it: `phaseRefused` read `AccesslogPurgeConverged` from the
SlapdCluster, where that condition never appears. On a patched build the spec's
other branch wins the race and hides it; this was the first run where refusal
was the only possible outcome. Fixed and verified red/green against the live
mesh. The operator's own reporting was correct throughout.

### The two constraints that are real

**1. ~~The born-with backfill must record the OLD default.~~ GONE, and with it
the release's only format hazard.** ADR-040's amendment deleted born-with
pinning and reversed the version flip, so there is no backfill to get wrong and
no cluster handed an image that cannot open its volume. The default stays 2.7;
what moves is the **flavour**, and pure and patched 2.7.1 open the same
volumes. The selector replaces the mechanism entirely — the user's declaration
is the record.

The constraint that survives in its place is smaller but real: **do not make
`pure` the default while a pure build still reports `2.7.1-0+slaptain3`.** The
shipped `PurgePhaseUnsupported` condition tells operators to verify the patch
with `slapd -VV`, and a pure build passes that test while carrying no patch —
so on the day pure becomes the default, the instruction misleads the majority.
Fix the version string first, in that order, within the release
(`docs/BACKLOG.md`).

**2. Sort the rest by REVERSIBILITY, which is what actually differs:**

- **`pure` as the default flavour** — cheap to undo: both flavours share a
  storage format, so reverting is re-pinning an image, not a migration. This
  row used to read "2.6 as the default", expensive to undo; the amendment
  removed that cost from the release entirely.
- **accesslog checkpoint** — a converged tunable; reverting is changing a value.
- **`syncprovSessionlogSource: Accesslog`** — startup-only, converges at each
  pod's next restart, reverted the same way. Note it lands *gradually* across a
  fleet rather than at upgrade, which is harder to reason about than a roll.

### The checkpoint default rests on one control run

Worth stating plainly, because the rest of this release is better evidenced.
The case for defaulting a `syncprov-checkpoint` on the accesslog database is
`docs/its9398/control-checkpoint-1-1.log`, where `1 1` held the on-disk
`contextCSN` current while `100 5` let it lag — the ITS#9398 precondition
forming in one arm and not the other.

**Read properly 2026-10-23, and the case got stronger and more expensive at
once** (`docs/ITS-9398-REPRODUCTION.md` §Controls):

- The arms are sound and the effect is real. `100 5` stormed at 1473/s with
  the server naming the serverID present in on-disk `minCSN` and absent from
  on-disk `contextCSN`. No purge ran in any arm — all three use a 7-day
  retention over a one-minute run — so the checkpoint is acting on the
  mechanism directly.
- **`1 1` wins by ordering, not by a narrower race.** The log entry is added
  before `minCSN` is modified, so `si_chkops = 1` checkpoints first
  (`syncprov.c:2607`, `:2615`) and `contextCSN` lands before `minCSN` gains
  the serverID. Read from source — **neither arm attempts a kill inside the
  window**, so the protective ordering is reasoned, not measured.
- **The cost is unmeasured and is now the gating unknown.** `1 1` is a
  `contextCSN` write per logged operation into the environment whose single
  LMDB writer already contends with every client write (ADR-037). At the rates
  that work measured, nobody has tried it.
- One arm's instrument did not run at all (`stale_on`, the SIGTERM row) —
  filed in `docs/BACKLOG.md`. None of the four storm numbers came from a
  checked instrument.

**So this default is not ready to ship on this evidence.** It needs a
kill-inside-the-window control and a cost measurement under load first.

It is defensible to ship anyway under this project's alpha stance — choose now,
let the runs accumulate, and this one is trivially reversible. What is not
defensible is implying it was measured properly. It was not.

### Remaining build order

1. Cycle B: the pure pair through a multi-site cycle (ADR-040 decision 2).
2. Invert the alias — unsuffixed becomes pure, patched takes the token.
   Breaking for anyone pinning `spec.images.*.tag`.
3. Born-with pinning + the volume preflight, backfill recording 2.7.
4. The three default flips.
5. Two mesh cycles, one per tier-1 cell (ADR-040 decision 7), then the release.

## Standing hazards this plan does not resolve

- **ITS#9398** is red on 2.7.1 in `dataloss_recovery_test.go` and reproduced
  twice more on a three-site mesh. The purge was established as irrelevant to
  it (2026-10-19). It is in the baseline recovery path and nothing here fixes
  it.
- **Mixed-version mesh behaviour** is reasoned, not measured (ADR-027). Any
  move that makes 2.6 and 2.7 both first-class makes this a question somebody
  will actually ask.
- **ADR-036** is `Proposed` with nothing built, and is a prerequisite for the
  backup-accelerated pod rebuild recorded in `docs/BACKLOG.md`.
