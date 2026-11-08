# ADR-047: CI architecture: images build with BuildKit in CI, so development needs no local hardware

**Status:** Proposed
**Date:** 2026-11-08

The project should be open to more collaborators.
Three requirements follow:

- Working on it needs no specific local hardware.
- A change can be deployed to an arbitrary remote cluster without manual steps,
  ideally with one command in the future.
- Iteration is fast, container builds above all.

The goal is CI/CD that a contributor can work with remotely,
with no local setup at all.
This ADR decides where and how images are built and on which runners CI runs,
and makes it possible to run the e2e suite in CI in the future.

## Context

Images were built with podman through the Makefile,
on a laptop or in CI for `main` and tags,
where building all eleven took 699 s.
Pushing them from a laptop over a home DSL line is not workable.

The e2e suite needs Kubernetes clusters with routed pod networks between them,
so it runs only in one maintainer's lab.

The upstream organisation has no Namespace runners activated.

## Decisions

1. **Where do images build?**
   - In CI: for `main`, for release tags,
     and for pull requests labelled `dev images` (ADR-046).
   - A local build on a laptop stays an optional alternative, from the same build graph.
   - Discarded:
     - The laptop as the route to a cluster: it needs the hardware and an upload link.
2. **Which build engine?**
   - BuildKit, driven by `buildx`, because it builds faster.
     It runs independent stages in parallel,
     reuses a layer cache across runs and machines,
     and keeps compiler and apt caches on cache mounts.
     On GitHub-hosted runners a cold build of all images went from 699 s with podman
     to 219 s with decision 7, and a warm one takes 28 s.
   - The same client runs on a laptop, on a GitHub-hosted runner and against a remote builder.
   - podman stays a container runtime for `docs/its9398/`, not a build tool.
   - Discarded:
     - podman: it has no remote worker,
       and on Namespace it needed four workarounds
       (fuse-overlayfs, sysctls, chroot isolation, short-name resolution)
       and was still slower than on GitHub-hosted runners.
     - A shim translating podman's build grammar to `buildctl`:
       two tools, and a translation layer to maintain.
3. **Where does the build graph live?**
   - In `docker-bake.hcl`.
     `scripts/bake-vars.sh` derives what HCL cannot read for itself:
     the image tags, the OpenLDAP version from the packaging fork's changelog,
     and the `-ol26src` patch list.
   - CI leads: the workflows run bake directly.
     The Makefile's image targets are a compatibility and convenience layer of aliases onto bake.
   - Discarded:
     - Build steps written into each workflow:
       two descriptions of one graph with nothing keeping them in step.
       Drift builds a pure image labelled patched, and nothing fails.
     - `make` in the pipeline: it carried only configuration,
       and its stamps never apply on a fresh checkout.
4. **How do the two images of a pair share one package build?**
   - A pair is the `slapd` runtime image and its `slapd-init` bootstrap image
     for one OpenLDAP flavour (pure, slaptain, ol26src).
     Both must carry the same OpenLDAP build,
     because `slapd-init` runs `slaptest`, `slapcat` and `slapmodify` on the volumes `slapd` opens.
   - The package build is a stage of `images/openldap/Containerfile`,
     and `slapd` and `slapd-init` are two targets of that file.
     A pair always builds in one bake run, so OpenLDAP compiles once per flavour.
   - Discarded:
     - A carrier image in the local store: a remote worker cannot read it.
     - A carrier image in a registry: every build has to publish,
       and a build that publishes nothing still has to push the carrier.
5. **For which platform?**
   - `linux/amd64`, pinned rather than detected.
     BuildKit builds for the worker's architecture,
     so an arm64 laptop would build images an amd64 cluster cannot run.
     On arm64 the pin means emulation, which is slow for the OpenLDAP compiles.
6. **Which runners?**
   - GitHub-hosted, for now.
     With decision 7 they are fast enough, and Namespace is not activated upstream.
   - Both are acceptable, and the future is probably a hybrid, especially for e2e.
   - A fork's CI runs on GitHub-hosted runners with no extra account,
     which makes contributing a little easier.
   - Deferred:
     - Namespace: faster on every Go job and on package rebuilds,
       and it creates Kubernetes clusters on demand.
       The setup is in the history.
     - Self-hosted runners: they cost effort to build and run.
       They are the fallback if both hosted options become too slow or too expensive.
7. **How do GitHub-hosted runners build fast enough?**
   - One job per OpenLDAP flavour, so each package build gets a runner's 4 vCPU to itself.
   - Each image's layers go to the GitHub Actions cache, in a scope of their own.
   - ccache is carried from one run to the next.
8. **Does the e2e suite run in CI?**
   - Not yet. CI now publishes images to a registry any cluster can pull from,
     which is the prerequisite.
   - Where the suite runs is still being tried out:
     Namespace clusters, kind on the runner, or a remote cluster.

## Consequences

**Build times.**
All eleven images, measured on 2026-10-29 (Namespace) and 2026-11-08 (GitHub-hosted).

| Build                                  | Runner                   |  Time |
|----------------------------------------|--------------------------|------:|
| podman, `make push`                    | GitHub-hosted            | 699 s |
| podman, `make push`                    | Namespace                | 870 s |
| bake, one job, cold                    | GitHub-hosted            | 414 s |
| bake, four jobs, cold                  | GitHub-hosted            | 219 s |
| bake, four jobs, warm                  | GitHub-hosted            |  28 s |
| bake, four jobs, `debian/rules` edited | GitHub-hosted            | 149 s |
| bake, cold                             | Namespace remote builder | 215 s |
| bake, `debian/rules` edited            | Namespace remote builder |  80 s |
| bake, Go-only change                   | Namespace remote builder |  11 s |

The one-job rows time the build-and-push step;
the four-job rows time the whole run.
A `debian/` edit costs more on GitHub-hosted runners
because each package build there recompiled with an empty ccache (3 hits in 642 calls).
How much carrying ccache over saves is not measured yet.

**Contributors.**
Building and publishing needs no local setup.
A local build needs `buildx` and a BuildKit worker; Docker Desktop's embedded one is enough.
Unlike podman, BuildKit always needs a worker process.

**Cost of the cache.**
Exporting the layer cache adds 20-45 s to each job.

**Deploying a branch with one command.**
The next step, not done here.
It needs the Helm charts to pin their images by tag and digest.
Then a branch's chart can be published next to its images,
and the documented install is `helm install` with the branch name as the chart version
(ADR-046 decision 9).

**e2e in CI, open.**
Whether clusters in CI can route pod networks to each other (ADR-016),
whether a runner reaches a node by NodePort,
and what a run costs.

**Not measured.**
Any arm64 build;
a Go-only change on GitHub-hosted runners.

## Related

- ADR-016: pod-routed cross-cluster replication, which an e2e run in CI needs.
- ADR-021: the vendored packaging fork the package stage builds.
- ADR-040: the image flavours the bake file builds.
- ADR-046: branch images, built by this pipeline.
