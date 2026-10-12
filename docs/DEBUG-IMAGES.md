# Debug images: naming a heap corruption in slapd

Two diagnostic variants of the slapd runtime image, built on demand and never
shipped. They exist for one class of problem: slapd corrupts its own heap, and
glibc only notices long afterwards, in a stripped binary, in an image with no
shell.

|  | `slapd:<tag>-debug` | `slapd:<tag>-asan` |
|---|---|---|
| binaries | **bit-identical to the release image** | AddressSanitizer, unoptimised |
| adds | detached debug info (`-dbgsym`) | debug info + `libasan` + ASan runtime options |
| glibc heap checking | on (`MALLOC_CHECK_`, `MALLOC_PERTURB_`, no tcache) | replaced by ASan's own |
| speed / memory | release speed, release memory | ~2x slower, 2-3x RSS |
| answers | *where* did it die | *who* broke the heap, and when |
| build | `make build-slapd-debug` | `make build-slapd-asan` |
| deliver | `make import-slapd-debug` / `push-slapd-debug` | `make import-slapd-asan` / `push-slapd-asan` |

Neither is in `make all` or `make push`, and no chart points at either.

## Which one to reach for

**`-debug` first.** Its binaries are the release binaries — same SHA-256, same
build ID — so a crash reproduces at the same rate, through the same timing, as
the one you are chasing. What it adds is a backtrace that resolves and an
allocator that complains earlier.

Because the package build is reproducible, this cuts both ways: the debug info
from `slapd:<tag>-debug` resolves offsets recorded against `slapd:<tag>`. An
`object+offset` already in a kernel ring buffer becomes a function and a line
without redeploying anything — see "Resolving an offset" below. Confirm the
build IDs match first; that is the whole claim.

**`-asan` when `-debug` has told you where it dies and that is not where it
broke.** Three different glibc allocator messages from the same fault are the
signature of this: `unaligned tcache chunk`, `double free or corruption`,
`free(): invalid pointer` are one root cause surfacing through whoever touches
the damaged heap first. The last code to run is where the busy code is, not
where the bug is. ASan closes exactly that gap — it intercepts every
allocation and reports an invalid free or a use-after-free *at the moment it
happens*, with three stacks: where the memory was allocated, where it was
freed, and where it was then used.

A report in `kubectl logs <pod> -c slapd --previous` is the deliverable:

```
==1==ERROR: AddressSanitizer: heap-use-after-free on address 0x...
    #0 0x... in syncprov_matchops ../../../../servers/slapd/overlays/syncprov.c:1234
  freed by thread T7 here: ...
  previously allocated by thread T3 here: ...
```

Those stacks are what turns "slapd crashes under delta-syncrepl" into an
OpenLDAP ITS report someone can act on.

## Deploying one

Pin **only** `spec.images.slapd.tag`. The init container stays on the plain
tag:

```yaml
spec:
  images:
    slapd:
      tag: <tag>-asan
  resources:
    requests: { memory: 4Gi }
    limits:   { memory: 8Gi }
```

This is the opposite of the `-ol26` rule, where the pair moves together or not
at all (ADR-021). There the two images disagree about the on-disk format; here
they are the same OpenLDAP version built from the same sources, and only
slapd's own compiler flags differ.

Under ASan, raise the memory limits before you start. The shadow map and the
redzones cost 2-3x RSS on top of whatever `back-mdb` maps, and an OOMKill at
the moment of interest looks like progress and is not.

Run the load **lower**, not higher. ASan roughly halves throughput, and the
goal is to hit the bug once with a readable report, not to measure anything.

## What each variant changes about behaviour

Be honest with yourself about which of these you are running.

`-debug` ships release binaries but sets three environment variables that are
not behaviour-neutral:

- `MALLOC_PERTURB_=165` fills freed memory with a byte pattern, so a
  use-after-free reads garbage instead of plausible data and fails nearer its
  cause.
- `MALLOC_CHECK_=3` makes the allocator abort on the first inconsistency it
  notices rather than carrying on.
- `GLIBC_TUNABLES=glibc.malloc.tcache_count=0` removes the per-thread free
  list. If the failure moves when you turn the tcache off, that path is
  involved; if it stays put, it is not.

They are baked into the image because the operator sets no environment on the
slapd container and distroless has no shell to set any from. That is also why
these are separate images rather than one image plus a runtime flag.

`-asan` is a different program: unoptimised, every allocation routed through
the sanitizer. Note that glibc's own tcache diagnostics stop firing under it —
ASan reports instead. Their absence is not the bug going away.

Its baked `ASAN_OPTIONS`:

| option | why |
|---|---|
| `detect_leaks=0` | slapd holds allocations to exit by design; a shutdown leak report buries the one we want |
| `abort_on_error=1` | die on the report, so kubelet records the restart and the stacks land in `--previous` |
| `print_stacktrace=1` | the stacks are the deliverable |
| `malloc_context_size=30` | deep enough to name the path that allocated the chunk, not just the wrapper that called `malloc` |
| `allocator_may_return_null=1` | without it, an allocation ASan cannot satisfy is a hard abort — which reads like a finding and is a memory limit |

## Resolving an offset

A kernel ring buffer reports faults as `object+offset`. Export the debug tree
from the `-debug` image and resolve against it:

```bash
podman create --name dbg <registry>/slaptain/slapd:<tag>-debug
podman export dbg | tar -x usr/lib/debug usr/lib/ldap usr/sbin
podman rm dbg

id=$(readelf -n usr/lib/ldap/back_mdb.so | awk '/Build ID/{print $3}')
addr2line -f -C -e usr/lib/debug/.build-id/${id:0:2}/${id:2}.debug 0x2d26a
```

Check that `$id` matches the build ID of the same object in the image that
actually crashed. If it does not, the offset belongs to a different build and
the answer will be confidently wrong.

## How the images are built

`BUILD_VARIANT` (`release` | `debug` | `asan`) is a build argument on both
`images/openldap-deb/Containerfile` (the package build) and
`images/slapd/Containerfile` (the runtime image). It has to match on both: the
compiler flags live in the packages, and the runtime stage only picks up the
debug symbols and the environment that go with them. The Makefile targets keep
the two in step.

Each variant needs its own package build — a full cycle, several minutes — so
the carrier image is tagged `openldap-deb:<tag>-debug` / `-asan` and nothing in
the normal loop depends on those stamps.

Three decisions in there are worth knowing before changing any of it.

**Debug info travels as `-dbgsym` packages, not `DEB_BUILD_OPTIONS=nostrip`.**
`nostrip` looks simpler and is a trap: `dh` drops `dh_strip` from the sequence
entirely under it, and this packaging creates the `slapadd`/`slappasswd`/…
hardlinks *inside* `override_dh_strip`. The packages then build and install
cleanly and contain no `slapadd`, which surfaces as a postinst failure three
images downstream.

**The sanitizer flags go in `DEB_CFLAGS_APPEND` and friends, not
`DEB_BUILD_MAINT_OPTIONS`.** `debian/rules` sets `DEB_BUILD_MAINT_OPTIONS`
unconditionally at line 4, so a `sanitize=+address` passed through the
environment there is silently discarded. The `*_APPEND` variables are the
user-side counterparts that `dpkg-buildflags` reads from the environment and
`debian/rules` never touches. `_FORTIFY_SOURCE` and ASan interpose the same
functions, so `DEB_CPPFLAGS_APPEND=-U_FORTIFY_SOURCE` undefines it last.

**There is no symbolizer in the image, and none is needed.** libasan carries
its own, in-process, and it follows the build-ID link into
`/usr/lib/debug/.build-id` the way gdb does. That is what makes a symbolised
report possible at all in a distroless image with no shell and nowhere to
install `llvm-symbolizer` from. Staging that directory is the only reason the
`-dbgsym` packages are unpacked into the runtime tree.

The build asserts the things that would otherwise fail silently later: that an
`asan` build's `slapd` really does depend on `libasan`, that the sanitizer
runtime reached the staging tree, and that `/usr/lib/debug/.build-id` exists. A
missing sanitizer runtime does not degrade the image — it stops the container
from starting, and there is no shell in there to find that out from.
