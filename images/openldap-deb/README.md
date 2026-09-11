# OpenLDAP 2.7.1 Debian packages (vendored packaging fork)

`debian/` here is a fork of the **Debian openldap source packaging**, rebased
from Debian's `2.6.14+dfsg-2` onto pristine upstream **OpenLDAP 2.7.1**. The
`Containerfile` builds it inside a `debian:trixie-slim` container and hands the
resulting `.deb` files to both `images/slapd` and `images/slapd-init`.

Why a fork at all: the ITS#9580 delta-syncrepl fix ships in no OpenLDAP 2.6.x
release, and as of 2026-09-11 no Debian suite — trixie, sid or experimental —
packages 2.7. See [ADR-021](../../docs/adrs/adr-021-openldap-2.7-dual-images.md)
for the decision and [`docs/OPENLDAP-VERSIONS.md`](../../docs/OPENLDAP-VERSIONS.md)
for the user-facing version/tag contract.

## Provenance

| | |
|---|---|
| Packaging base | Debian `openldap` source package, salsa `master` at `2.6.14+dfsg-2` |
| Forked on | 2026-09-11 |
| Upstream source | `openldap-2.7.1.tgz` from <https://www.openldap.org/software/download/OpenLDAP/openldap-release/> |
| Upstream sha256 | `253db80f301258ea69cda1184766d57395b836aaabf41157eb0316eb0fac1341` (pinned in `Containerfile`, verified at build time) |
| Package version | `2.7.1-0+slaptain1` |

Upstream packaging: <https://salsa.debian.org/openldap-team/openldap>

## The delta vs. Debian's packaging

1. **`debian/changelog`** — one `2.7.1-0+slaptain1` entry on top recording
   everything below.
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

That is 15 of Debian's 17 patches applying unchanged, one refreshed, two
retired — the reason a packaging fork beat a raw source build (ADR-021).

## Version scheme — do not use a `~` revision

The package version must **not** sort below a plain `2.7.1`. `slapd` carries a
generated self-dependency `slapd (>= 2.7.1)` via `libslapi`'s shlibs, so a
`2.7.1-0~slaptain1`-style revision makes the package uninstallable against
itself. `2.7.1-0+slaptain1` sorts *above* `2.7.1` and *below* Debian's eventual
`2.7.1-1`, so a future distro package supersedes ours cleanly.

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

Nothing outside `images/` and the Makefile depends on the fork — no operator
code, no chart, no CRD field.
