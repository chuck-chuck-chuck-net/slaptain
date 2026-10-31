# ADR-044: The build graph is a build-graph file, and `make` is not in the pipeline

**Status:** Proposed
**Date:** 2026-10-31

## Context

Until now the build was `podman build` invocations in the root Makefile, with
stamp files under `.stamps/` standing in for incrementality, and one artifact
that was not a deployable image: `openldap-deb`, a `FROM scratch` carrier
holding nothing but `/debs`, built once per flavour and consumed by both
images of a pair via `--build-arg OPENLDAP_DEB_IMAGE`. It was named
`localhost/<project>/openldap-deb:<tag>` and deliberately never pushed.

Adding a second CI substrate — an internal forge whose runners drive a shared
remote BuildKit worker in host mode, with no Docker daemon and no local image
store — made two load-bearing assumptions visible at once:

1. **The carrier lived in a local image store.** `localhost/...` cannot be
   resolved by a remote worker, which reads `FROM` from a registry. The thing
   that made "compile OpenLDAP once, feed both images" cheap was the thing
   that was not portable.
2. **`make` was in the pipeline**, and on inspection it was not doing the job
   make exists for. A CI checkout is fresh, so `.stamps/` does not exist, so
   every stamp is stale, so everything rebuilds regardless. What is actually
   incremental in CI is BuildKit's content-addressed cache, one layer below
   and indifferent to make. `make` was in the pipeline carrying *configuration*
   — image names, tag derivation, the flavour matrix — and the orchestration
   came along with it.

The second point generalises past the new substrate: it had been equally true
of `.github/workflows/publish.yml`, which has called `make push` since the
beginning.

## Options considered

**A. Keep podman; translate in a shim.** A wrapper accepting podman-build
grammar and emitting `buildctl`. Rejected: it keeps two tools in the picture
and makes the translation layer a thing to maintain and misread, for no gain
over speaking BuildKit directly.

**B. Hand-write the build steps in each workflow.** Rejected, and this is the
one that keeps looking attractive: it would give the layering people expect
(make as a human frontend, the pipeline invoking the tool). It costs **two
descriptions of the build graph** — Makefile and YAML — with nothing keeping
them in step. The graph is six carriers and eleven images with flavour-
dependent build args that decide whether a binary reports `+pure` or
`+slaptain`. Drift there does not produce a build failure; it produces a pure
image labelled patched. This repository already carries a comment about
exactly that failure, from when `local-patches/` was duplicated into
`debian/patches/` and the two copies had nothing keeping them in step.

**C. Address the carrier in the registry.** Implemented first, and it works:
one variable, no Dockerfile changes, one build graph everywhere. Superseded
rather than rejected — see decision 4. Its costs were real: every build had to
*publish* (a remote worker has no local store to leave a result in), so
`make all` stopped being a build and became a release; and a
"publish nothing" mode still had to push the carrier, because `FROM` reads
from a registry.

**D. Merge the carrier into the consumer Dockerfiles as a shared stage.**
Removes the artifact entirely. Rejected: "compile once, feed both" then
becomes a property of *cache state* rather than of the graph. A cache miss
costs a second ten-minute OpenLDAP compile and looks exactly like a slow
build — the reassuring reading and the empty one are indistinguishable, which
is the failure mode this project has a standing rule about.

**E. `buildx bake` with `target:` contexts.** Chosen. See the decisions.

## Decisions

**1. BuildKit is the engine, `buildx` the client.** podman is no longer a
build tool here. It remains a container *runtime* for `docs/its9398/` and the
exporter's testdata recorder, which run containers rather than build images.
`buildx` was chosen over `buildctl` because it speaks `docker build` grammar
and understands bake files, and because it runs standalone — no Docker daemon
— which is what makes one client work on a laptop, on a GitHub runner and on
an Alpine CI runner alike.

**2. The build graph is `docker-bake.hcl`.** `scripts/bake.sh` is the single
entry point: it derives what HCL cannot read for itself (the tag addressing,
the OpenLDAP version from the packaging fork's changelog, the patch list from
the directory) and invokes bake. One description, consumed identically by a
laptop and by every pipeline.

**3. `make` appears in no pipeline.** Its image targets are aliases onto
`scripts/bake.sh`, and the stamp machinery is deleted. make keeps the job it
is good at — being the thing a human types at — and the pipelines need nothing
but `buildx` and a shell. A consequence worth stating in its own right: the CI
runner image needs no `make` installed on this project's behalf.

**4. A carrier is not an image.** `contexts = { debs = "target:openldap-deb" }`
makes bake stitch carrier and consumers into one BuildKit graph and pass the
layers in-process. The carrier is never named, pushed or pulled, and it
carries **no tag at all**, so no output setting can let one escape to a
registry. This restores what option C had to give up: `make all` builds and
publishes nothing again, and `cacheonly` is genuinely registry-free.

The Containerfiles are unchanged. A named context *shadows a build stage of
the same name*, so each still declares `ARG OPENLDAP_DEB_IMAGE` and
`FROM ${OPENLDAP_DEB_IMAGE} AS debs`; supply no context and that stage does
its old job. A bare `buildx build --build-arg OPENLDAP_DEB_IMAGE=<ref>` still
builds any of them, which makes this decision reversible.

**5. Every build names its file with `-f`,** including the ones whose file is
the plain `Containerfile`. BuildKit's dockerfile frontend looks for
`Dockerfile` and nothing else; buildah also accepted `Containerfile`
implicitly. The failure is legible but misleading — *"failed to read
dockerfile: open Dockerfile: no such file or directory"* reads as a missing
file rather than a missing flag.

**6. `.dockerignore` is load-bearing, not hygiene.** Two images build from the
repository root, and BuildKit reads `.dockerignore` only, where podman also
honoured `.containerignore`. Without it the whole working tree travels to the
worker on every build — over a network, when the worker is remote. Note the
failure mode a `.containerignore` would now have: a *slow* build, not a broken
one, which is the kind nobody investigates.

**7. `PLATFORM` is pinned to `linux/amd64`, not detected.** BuildKit defaults
to the *worker's* native architecture, so this graph on an arm64 machine would
quietly produce arm64 images that an amd64 cluster cannot run, with no error
until a pod crashloops. Pinned rather than detected, and the trade is stated
rather than hidden: on arm64 the pin means QEMU, which for the OpenLDAP
compiles is slow enough to look like a hang. Slow and right is diagnosable;
fast and wrong is not. The escape hatch is an amd64 builder, which is both.

The fuller answer is ADR-041's shape — **detect to refuse, not to choose**,
with a detector that can *exonerate* as well as accuse — and is deferred to
`docs/BACKLOG.md` because nobody here is on arm64 today, which makes it
insurance rather than a fix.

## Consequences

**The build has three separable roles.** A *description* (this graph), a
*client* (`buildx` plus two shell scripts), and a *worker* that can be
anywhere: local, a container, Docker's embedded one, or remote. The same
`scripts/bake.sh images` produces identical artifacts in all of them —
measured, not assumed: the same graph run on a laptop and on a remote worker
produced byte-identical `slapd -VV` strings for all four flavours.

**A worker is now required, and that is a real loss.** podman is genuinely
daemonless: `podman build` forks, works, exits. BuildKit is a client/worker
split by design. The worker can be auto-managed (buildx's `docker-container`
driver, upstream's `buildctl-daemonless.sh`) so it can *feel* daemonless, but
the trade was made deliberately: what it buys is the persistent
content-addressed cache and the freedom to put the worker on other hardware.

**Prerequisites move.** A contributor needs `buildx` and a BuildKit worker
rather than podman or docker. Docker Desktop's embedded worker qualifies,
which makes macOS and Windows-with-WSL *better* served than before, where
podman meant a Linux VM either way.

**`sha256sum` and `mapfile` were GNU/bash-4 assumptions** in the wrapper
scripts, on the everyday dirty-tree path. Both fixed; both would have broken
macOS silently. The portable `sha256` fallback was checked to produce the
*same* digest by both routes — a fallback that disagreed would be worse than
the failure, deriving a different tag for an identical tree.

**arm64 is reachable only deliberately,** via `PLATFORM`, and is unmeasured.

## What is measured and what is not

Measured: the full `images` group builds end to end on a remote worker
(exit 0, eleven images, six carriers, every in-Containerfile gate green);
all four flavours verified from the *published* binaries' version strings by
a route independent of the build gates; the carrier's absence from the
registry confirmed by the registry itself (`NAME_UNKNOWN`); the cache export
confirmed by the three refs it creates; `target:` contexts proven with a
control whose `ARG` default pointed at a registry that cannot exist, which
failed — making the passes attributable rather than coincidental.

Not measured: a cold build in CI (the shared worker's cache was warm from
validation when the pipeline first ran), and therefore the cache *import*
path; `scripts/bake.sh --import`; any arm64 build.

## Related

- ADR-040 — data-plane image identity; the flavour axis this graph builds, and
  the source of decision 7's "detect to refuse" shape via ADR-041
- ADR-041 — the detector shape deferred in decision 7
- ADR-021 — the vendored packaging fork the carriers build
- ADR-035 — ADRs conflict; the clean design wins before 1.0
