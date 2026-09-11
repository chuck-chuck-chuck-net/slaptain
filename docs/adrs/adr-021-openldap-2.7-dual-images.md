# ADR-021: OpenLDAP 2.7.1 by default from a vendored packaging fork; 2.6 kept as `-ol26`

**Status:** Accepted
**Date:** 2026-09-11

## Context

A slaptain pod that loses its volumes and rejoins the mesh drives every peer into
a stale-cookie refresh loop. The mechanism is upstream
[ITS#9580](https://bugs.openldap.org/show_bug.cgi?id=9580): the refreshing
consumer writes the entries it receives into its own accesslog in *receive*
order, so that accesslog stops being a valid delta-sync source. On every
reconnect `syncprov_findcsn(mincsn)` fails and the provider answers
`err=4096 text=sync cookie is stale`. Measured on Debian trixie's 2.6.10: ~14k
connections in 27 seconds, several hundred millicores per pod, thousands of those
log lines. The data converges; the CPU does not, for minutes. Full diagnostic
trail in `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md`.

Our previous posture was to tolerate it — extend the `dataloss_recovery_test`
timeout and tell operators to expect a CPU spike. That was the right call while
no fixed release existed. It no longer is.

Dated public facts, all as of 2026-09-11:

- The fix is [MR 472](https://git.openldap.org/openldap/openldap/-/merge_requests/472),
  commit `414866b8` (2022-01-11). It is contained in the `OPENLDAP_REL_ENG_2_7_0`
  and `OPENLDAP_REL_ENG_2_7_1` release tags, and in **no** 2.6.x release tag.
- OpenLDAP 2.7.1 is a released upstream tarball
  (<https://www.openldap.org/software/download/OpenLDAP/openldap-release/>).
- No Debian suite packages 2.7 — not trixie, not sid, not experimental. The
  packaging team's `master` is at `2.6.14+dfsg-2`
  (<https://salsa.debian.org/openldap-team/openldap>).
- Symas publishes no 2.7 package stream.

→ The fix is released, and there is nothing to `apt-get install`. At the same
time ADR-011 needs us to keep running the *same* code as a legacy migration
source, and that is 2.6 on every distro shipping today.

## Options considered

1. **Wait for a distribution package.** Rejected. The fix has been merged for
   four and a half years without reaching a 2.6 release, and no 2.7 Debian
   package is announced. Waiting means shipping a known multi-minute CPU
   pathology on the most ordinary recovery path we have — a pod losing its disk.
2. **Build slapd from raw upstream sources.** Rejected in favour of option 3. It
   means re-deriving everything Debian's packaging already solves: the
   `configure` flag set, the module and schema layout, `/etc/ldap` vs
   `/etc/openldap`, the `openldap` user, the fifteen still-relevant Debian
   patches. Every one of those is load-bearing here — `bootstrap.sh`, the
   operator's module paths and the distroless staging in
   `images/slapd/Containerfile` all encode Debian's layout. It also has no exit:
   nothing to hand back to when a distro package appears.
3. **Fork Debian's source packaging onto upstream 2.7.1 (chosen).** The rebase is
   small and mechanical: 15 of 17 patches apply unchanged, one needs a context
   refresh, two are already upstream in 2.7.1. Beyond that, drop the
   `back-perl`/`back-sql` build options, build dependencies and manpages (both
   backends were removed upstream in 2.7) and record the three-symbol libldap ABI
   delta. The output is `.deb` files with Debian's exact layout, so nothing
   downstream changes, and the exit is a `git rm` of one directory.

We only considered 2.7 as the target, and 2.7.1 over 2.7.0 as the newer of the
two releases carrying the fix.

## Decision

Ship two image pairs, with the plain tag being OpenLDAP 2.7.1.

| Image | OpenLDAP | Built from |
|---|---|---|
| `slapd:<tag>`, `slapd-init:<tag>` | 2.7.1 (`2.7.1-0+slaptain1`) | vendored fork, `images/openldap-deb/` |
| `slapd:<tag>-ol26`, `slapd-init:<tag>-ol26` | 2.6.x from Debian trixie | `images/*/Containerfile.ol26` |

- `images/openldap-deb/` holds the vendored `debian/` fork and a Containerfile
  that fetches the upstream 2.7.1 tarball, verifies a pinned sha256 (a mismatch
  fails the build) and runs `dpkg-buildpackage` with `DFSG_NONFREE=1`. Its final
  stage is `FROM scratch` carrying only `/debs` — a local-only artifact carrier
  (`localhost/…`), never pushed, consumed by *both* 2.7 images so the package
  build runs once per tag. Provenance, the full delta, the version-scheme rule
  and the exit strategy live in `images/openldap-deb/README.md`.
- The package version is `2.7.1-0+slaptain1`, never a `~` revision. `slapd`
  carries a generated self-dependency `slapd (>= 2.7.1)` via `libslapi`'s
  shlibs, so any version sorting *below* plain `2.7.1` makes the package
  uninstallable against itself. `+slaptain1` sorts below Debian's eventual
  `2.7.1-1`, so a distro package supersedes ours without intervention.
- No `DPKG_GENSYMBOLS_CHECK_LEVEL` relaxation. We record the 2.6 → 2.7 libldap
  ABI delta in `debian/libldap2.symbols`, so any further symbol change fails the
  build instead of passing unnoticed.
- The `-ol26` pair stays buildable and pushed (`make build-ol26`, `make push`).
  It serves hot-migration and legacy-interop clusters per ADR-011, where matching
  the source cluster's code is worth more than the ITS#9580 fix. Pin it with
  `spec.images.{slapd,init}.tag=<tag>-ol26` — both images or neither, because the
  init container writes the config and data that slapd then opens and the on-disk
  formats differ (see Consequences).
- The operator needs no change. Its image defaulting derives the repository from
  `OPERATOR_IMAGE` and the tag from `OPERATOR_IMAGE_TAG` — the plain tag — which
  now resolves to 2.7.1 with zero Go changes. The default is the fixed version;
  2.6 is an explicit opt-in.
- We defer version gating. No operator feature is 2.7-only today, so nothing has
  to branch on the slapd version. The mechanism stays an open question until the
  first such feature exists; candidates are reading the running version from the
  rootDSE / `cn=monitor`, and a declared `spec.ldap.version` floor. We are
  deliberately not building it speculatively.
- The guard against a regression is a behavioural e2e assertion, not a version
  check. `tests/e2e/dataloss_recovery_test.go` asserts that fewer than 50
  `sync cookie is stale` lines appear across the RW pods' slapd logs in the
  recovery window. The spec skips itself when the cluster's `logLevel` lacks the
  `stats` bit, since without it the count is trivially zero and the green would
  be false.

  **Measured negative result (2026-09-11):** a fresh single-site 3-pod cluster
  on 2.6.10 does **not** reproduce the storm — the red-first run counted 0
  occurrences with an 18 s recovery. Every local SID had a recent CSN, so the
  mincsn lookup succeeded; the storm needs a **dormant SID**, i.e. the
  multi-site mesh of the investigation doc with one site originating no writes.
  The assertion therefore stands as a tripwire, but its red has not yet been
  observed in an e2e run. A first three-site `-ol26` attempt (same date) also
  counted 0: the suite's own cross-site probe traffic keeps every SID warm, so
  dormancy never accumulates inside one run. That matches the original repro's
  intermittency (it hit on iteration 28 of a repro loop). Observing the red
  deliberately needs an iteration loop with an idle site, not a single cycle —
  until then the tripwire's value is guarding 2.7 against regression (~0
  measured single-site and three-site), not proving 2.6 broken (the
  investigation's live capture already does that).

## Consequences

- LMDB 1.0 breaks the on-disk format, so there is no in-place upgrade: 2.7 slapd
  cannot open a `/data` or `/accesslog` volume that 2.6 created. Pointing an
  existing cluster at the 2.7 images is not an upgrade, it is a crashloop. The
  migration is dump and reload.
- The supported 2.6 → 2.7 runbook rides ADR-014: take a `SlapdBackup` while the
  cluster still runs 2.6, pin the 2.7 image pair, then `SlapdRestore`.
  Step-by-step in `docs/OPENLDAP-VERSIONS.md`. The alternative — wipe one pod's
  volumes and let it resync — works, and the refreshing pod is the one running
  the *fixed* code, but the peers serving that refresh are still 2.6 and still
  pay the ITS#9580 storm. Prefer backup/restore.
- A mixed 2.6/2.7 mesh is wire-compatible: syncrepl is unchanged and the version
  skew is invisible on the wire. We defer cross-site validation of such a mesh —
  nothing here depends on it, and the runbook does not require it. So: argued
  from the wire format, unproven end-to-end.
- We now carry packaging. A Debian security update to `slapd` no longer reaches
  our 2.7 images, and a CVE means rebuilding the fork against a new upstream
  release. That is the real cost here, and the reason the exit strategy is
  written down. The `-ol26` pair keeps tracking Debian.
- `make push` now pushes six images (2.7 pair, 2.6 pair, toolkit, operator), and
  the first 2.7 build of a tag compiles OpenLDAP — minutes, not seconds. The
  build is stamp-cached per tag and shared between the two images, so we pay it
  once.
- We do not run OpenLDAP's own test suite in the image build (`nocheck` by
  default, `RUN_UPSTREAM_TESTS=1` to opt in). What validates these images is the
  slaptain e2e suite — nobody should read "packaged" as "upstream tests passed".
- `slapd-toolkit` stays on Debian's 2.6 client tools. `ldapsearch` and friends
  speak the protocol, not the storage format.
- Schema files come from the pristine upstream tarball rather than Debian's
  DFSG-stripped copies (`DFSG_NONFREE=1`). The definitions are functionally
  identical; the difference is the RFC text Debian may not ship.

## Related

- ADR-008 (amendment 2026-09-11) — what CSN comparison can and cannot observe;
  why the stale-cookie storm has to be caught in the logs rather than in peer
  status.
- ADR-011 — hot-migration topology contract; why the 2.6 pair stays buildable.
- ADR-014 — S3 backup/restore; the machinery the 2.6 → 2.7 runbook rides on.
- ADR-022 — companion decision recorded the same day.
- `docs/INVESTIGATION-replication-divergence-after-dataloss-and-restart.md` — the
  ITS#9580 diagnosis and the posture this ADR supersedes.
- `docs/OPENLDAP-VERSIONS.md` — the user-facing version/tag contract and runbook.
- `images/openldap-deb/README.md` — fork provenance, delta and exit strategy.
