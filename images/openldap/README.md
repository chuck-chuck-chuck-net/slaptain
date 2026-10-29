# OpenLDAP 2.7.1 Debian packages (vendored packaging fork)

`debian/` here is a fork of the **Debian openldap source packaging**, rebased
from Debian's `2.6.14+dfsg-2` onto pristine upstream **OpenLDAP 2.7.1**. The
`Containerfile` builds it inside a `debian:trixie-slim` container and hands the
resulting `.deb` files to both `images/slapd` and `images/slapd-init`.

Why a fork at all: no Debian suite — trixie, sid or experimental — packages
OpenLDAP 2.7, so shipping 2.7 at all means forward-porting the packaging
ourselves. And 2.7 is the default because the LMDB format break makes any
later default move a forced migration of every cluster that said nothing;
defaulting to 2.7 now means whoever takes the default never migrates
(ADR-040). An earlier version of this file said the fork existed because "the
ITS#9580 delta-syncrepl fix ships in no 2.6.x release" — that reasoning is
retracted, and ADR-021's own amendment says so. See [ADR-021](../../docs/adrs/adr-021-openldap-2.7-dual-images.md)
for the decision and [`docs/OPENLDAP-VERSIONS.md`](../../docs/OPENLDAP-VERSIONS.md)
for the user-facing version/tag contract.

## Provenance

| | |
|---|---|
| Packaging base | Debian `openldap` source package, salsa `master` at `2.6.14+dfsg-2` |
| Forked on | 2026-09-11 |
| Upstream source | `openldap-2.7.1.tgz` from <https://www.openldap.org/software/download/OpenLDAP/openldap-release/> |
| Upstream sha256 | `253db80f301258ea69cda1184766d57395b836aaabf41157eb0316eb0fac1341` (pinned in `Containerfile`, verified at build time) |
| Package version | `2.7.1-0+slaptain3` patched, `2.7.1-0+pure3` pure (see below) |

Upstream packaging: <https://salsa.debian.org/openldap-team/openldap>

## The delta vs. Debian's packaging

1. **`debian/changelog`** — our own entries on top of Debian's, recording
   everything below: `2.7.1-0+slaptain1` for the rebase onto 2.7.1,
   `2.7.1-0+slaptain2` for the local patch in item 8, `2.7.1-0+slaptain3` for
   taking `slapd_rq.rq_mutex` around that patch's live update of a running
   purge task.
2. **`debian/configure.options`** — dropped `--enable-perl=mod`,
   `--enable-sql=mod`, `--with-odbc=unixodbc`. `back-perl` and `back-sql` were
   removed upstream in 2.7; configure rejects the options.
3. **`debian/control`** — dropped the now-unused `libperl-dev` and
   `unixodbc-dev` build dependencies. `Maintainer` is slaptain, Debian's moves
   to `XSBC-Original-Maintainer` and the Uploaders list is dropped: that field
   is what `slapd -VV` prints, and these packages are not Debian's.
4. **`debian/slapd.manpages`** — dropped `slapd-perl.5` and `slapd-sql.5`
   (gone with the backends; `dh_installman` fails on a missing page).
5. **`debian/patches/series`** — dropped `64-bit-time-t-compat.patch` and
   `ITS-10508-syncrepl-set-no_opattrs-on-modrdn.patch`; both are incorporated
   in upstream 2.7.1. The patch files are deleted, not just unlisted.
6. **`debian/patches/getaddrinfo-is-threadsafe`** — refreshed against 2.7.1
   (context and line offsets only; the change it makes is unaltered).
7. **`debian/libldap2.symbols`** — added the 2.6.14 → 2.7.1 libldap ABI delta:
   the `OPENLDAP_2.201` version node, `ldap_domain2hostlist_proto@OPENLDAP_2.201`
   and `ldap_pvt_thread_detach@OPENLDAP_2.200`. The build does **not** relax
   `DPKG_GENSYMBOLS_CHECK_LEVEL`, so any further ABI change fails it.

8. **`debian/patches/accesslog-purge-phase.patch`** — ours, not Debian's, and
   the only patch here that adds a feature rather than adapting one. `logpurge`
   takes an optional third value, the phase, and the purge task then fires at
   `phase + k*interval` counted from the epoch instead of at `now + interval`
   counted from each dispatch, so a purge can be *placed* — see
   [ADR-037](../../docs/adrs/adr-037-purge-off-the-write-path.md). Appended last
   in `series`; it touches `include/ldap_rq.h`, `libraries/libldap/rq.c` and
   `servers/slapd/overlays/accesslog.c`, which no Debian patch touches, so the
   position is a convention rather than a constraint.

   The same patch is also applied to the 2.6 source build by
   `Containerfile.ol26src`, which copies everything in `local-patches/` onto
   Debian's own series. **`local-patches/` is the source of truth**: the copy
   under `debian/patches/` exists because the 2.7 build only ever sees
   `debian/`, and the two must stay byte-identical.

9. **`local-patches/accesslog-purge-logging.patch`** — 2.6 ONLY, and
   deliberately not in `debian/patches/series`. It backports 2.7.1's
   `accesslog_purge:  dn="…" etime=… nentries=…` summary line, which 2.6.x does
   not have at any log level, so a 2.6 server gives no way to read how long a
   purge took or how many entries it deleted — both of which a purge schedule
   has to be sized against. 2.7 already emits it; applying this there would
   duplicate the block.

   Kept as its own file rather than folded into item 8 because it is a
   different change with a different subject: one places the purge, the other
   measures it, and only one of them is wanted on 2.7. `local-patches/*.patch`
   is copied in glob order, so the logging patch lands before the phase patch
   and the phase patch's later hunks apply at an offset.

That is 14 of Debian's 17 patches applying unchanged, one refreshed and two
retired — 15 entries in `series`, plus one of ours at the end, which is the
reason a packaging fork beat a raw source build (ADR-021). (The count read "15
unchanged" before; 15 was the surviving series length, not the unchanged
count.)

## Version scheme — do not use a `~` revision

The package version must **not** sort below a plain `2.7.1`. `slapd` carries a
generated self-dependency `slapd (>= 2.7.1)` via `libslapi`'s shlibs, so a
`2.7.1-0~slaptain1`-style revision makes the package uninstallable against
itself. `2.7.1-0+slaptainN` sorts *above* `2.7.1` and *below* Debian's eventual
`2.7.1-1`, so a future distro package supersedes ours cleanly.

Bump `N` whenever the built packages change in a way nothing else records. The
default 2.7 images carry no label saying which patches went in — unlike the
`-ol26src` pair, which is labelled — so the package version `slapd -VV` prints
is the only discriminator between two builds of the same upstream release.

### The revision token names the FLAVOUR; `N` is yours

`SLAPTAIN_PATCHES` decides two things from one argument: which patches go in,
and which token the revision wears.

| build | version | |
|---|---|---|
| `SLAPTAIN_PATCHES=1` (default) | `2.7.1-0+slaptain3` | carries our behaviour patches |
| `SLAPTAIN_PATCHES=0` | `2.7.1-0+pure3` | same source, same packaging fork, none of our patches |

`N` is **not derived and must not be**. It is a hand-maintained packaging
revision with a hand-written changelog entry saying what moved, and it covers
packaging work as well as patches — `+slaptain1` carried no behaviour patches
at all. Both flavours share it, because both are that revision of this
packaging fork. What the build derives is only which token it wears, by
rewriting the top changelog entry before `dpkg-buildpackage`.

`pure` and `slaptain` are the two values of ADR-040's `select.flavour`,
deliberately: the version string names the axis with the same words the API
does.

**Why this exists.** Until 2026-10-23 a pure build reported
`2.7.1-0+slaptain3`, byte-identical to the patched one. That broke two things.
ADR-040 decision 3 names `slapd -VV` as the second, independent route to an
image's provenance — and it is the only route available from inside a running
pod, where the OCI labels cannot be read — but on the axis that matters today
there was no second route, because both answers were the same string. And the
operator's `AccesslogPurgeConverged=False/PurgePhaseUnsupported` tells the
reader to pin a patched image and verify it with `slapd -VV`; a pure build
passed that test while carrying no patch, so an operator following the
instruction would verify the version, conclude the image was right, and have no
next step. Latent while patched was the default; ADR-040 decision 2 inverts
that, and this had to land first.

The derivation **refuses rather than guesses**: if the changelog version does
not end in `+slaptainN`, the pure build fails with a message saying so. A
silent no-op there is exactly the defect the rule exists to prevent.

### The version/flavour agreement gate

Immediately after `dpkg-buildpackage`, the build asserts that the version and
the patch set agree, on the artifacts just produced — symmetric, so each
flavour checks its own token *and* the absence of the other's, and one of the
two always runs.

It exists because the two can come apart silently and in the damaging
direction: a change that drops a patch but leaves the version alone ships a
pure slapd announcing itself as patched, and every downstream check then agrees
on the wrong answer.

Two routes, because they fail differently — the `.deb` control metadata (what
apt and the label derivation see) and the binary's compiled-in string (what
`slapd -VV` prints in a pod). The binary route asserts **equality** with the
metadata version rather than searching for a token, and that is deliberate:
`slapd -VV` also prints the Maintainer line, which says `slaptain` on *both*
flavours because the packaging is ours either way. A check for the absence of
`slaptain` in that output would fail every legitimate pure build.

## Building

```bash
make build-openldap-deb          # localhost/slaptain/openldap-deb:<tag>, /debs only
make build-slapd build-init      # both consume it via --build-arg OPENLDAP_DEB_IMAGE
```

The artifact-carrier image is `FROM scratch` and **never pushed** — it exists
so the package build runs once per tag and feeds both runtime images.

`DFSG_NONFREE=1` is exported for the build: we package the pristine upstream
tarball, so `debian/rules` must skip both its DFSG-freeness assertion and the
schema substitution that expects the `+dfsg`-repacked layout. The shipped
schema files are therefore upstream's, not Debian's stripped copies.

### The logpurge phase gate

The build asserts, on the binaries it just produced, that `logpurge` takes three
values: a three-value value is accepted, a four-value one is still `extra
cruft`, and a phase that is not less than the interval is refused by the patch's
own check. A dropped patch has no other symptom — the two-value form keeps
working and slapd keeps starting, so the rotation would quietly stop rotating.
The gate is skipped for `BUILD_VARIANT=asan`, where a non-zero exit says nothing
about the parser.

**One thing the phase does not survive: offline tooling.** `slaptest`,
`slapcat -n 0`, `slapadd` and `slapmodify` parse and validate a three-value
`logpurge` but re-emit it with two — the patch records "this value carries a
phase" only under `SLAP_SERVER_MODE`. Verified on 2.7.1-0+slaptain2:
`logpurge 0+00:15 0+00:05 0+00:02` in a `slapd.conf` converted with
`slaptest -f … -F …` lands in `slapd.d` as `olcAccessLogPurge: 00:15 00:05`.
Nothing writes the attribute that way today — the operator sets it over LDAP on
a running slapd — but an offline *read* of the value will under-report, which
matters to anything in the shape of
[ADR-034](../../docs/adrs/adr-034-startup-only-configuration.md)'s
converger.

### Upstream test suite

Skipped by default (`DEB_BUILD_OPTIONS=nocheck`). Run it with:

```bash
make build-openldap-deb RUN_UPSTREAM_TESTS=1
```

It adds tens of minutes and needs a working loopback plus the Kerberos test
dependencies in the build container. Honest statement of what our images are
validated by: the slaptain e2e suite, not OpenLDAP's own `make test`.

## Exit strategy

Drop this directory the moment a 2.7 package is available from Debian (or a
Symas 2.7 build we are willing to depend on):

1. Point `images/slapd/Containerfile` and `images/slapd-init/Containerfile` back
   at `apt-get install slapd ldap-utils` — i.e. re-adopt the shape still
   preserved in the `.ol26` variants.
2. Delete `images/openldap-deb/`, its Makefile targets, and the
   `OPENLDAP_DEB_IMAGE` plumbing.
3. Supersede ADR-021 and note the version the distro package carries.

The packaging is confined to `images/` and the Makefile — no chart and no CRD
field names it. One behaviour does leave that boundary: the `logpurge` phase in
item 8 is a feature of these packages and of nothing a distribution ships, so
operator code that emits a three-value purge value (ADR-037) stops working the
moment slapd comes from somewhere else. Re-adopting a distro build therefore
means either carrying the patch forward, getting it upstream, or giving up the
rotation — decide that in step 3 rather than discovering it in production.
