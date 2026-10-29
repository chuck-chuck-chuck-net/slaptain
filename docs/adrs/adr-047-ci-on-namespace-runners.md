# ADR-047: CI runs on Namespace runners

**Status:** Proposed
**Date:** 2026-10-29

## Context

CI compiles the e2e suite and never runs it.
The suite needs real Kubernetes clusters with routed pod networks between them,
so it runs only in one maintainer's lab.

The image build compiles OpenLDAP three times in a row and takes 12 minutes on GitHub-hosted runners.
An e2e test cycle is blocked until the images are built in CI.
Pushing eleven images over a home DSL line is not workable, so the build has to run next to the registry.
But GitHub-hosted runners are slow, hard to observe and hard to debug.

## Options considered

- **GitHub-hosted runners.** Slow, poor observability, hard to debug, and no clusters on demand.
- **Self-hosted runners.** They cost effort to build and run, so they are deferred.
  They are the fallback if Namespace becomes too expensive.
- **Namespace runners.** GitHub Actions runners plus short-lived Kubernetes clusters on demand, with the same workflow syntax.

## Decision

Every job runs on a Namespace profile named after its shape,
`namespace-profile-4x8`, `2x8`, `2x4` or `1x2`, so each job takes the runner size it needs.
The images build with `docker buildx bake` on Namespace's remote builders.

## Consequences

Measured on the fork `Y0-L0/slaptain` on 2026-10-29, one run each.

**Reliability.**
Namespace runners have been more reliable than GitHub-hosted ones.

**Image builds.**

| Build of all 11 images | Time |
|---|---|
| podman on GitHub-hosted | 11.8 min |
| podman on Namespace | 14.5 min |
| bake, cold cache | 3.5 min |
| bake, Go-only change | 11 s |

A cold build is 4x faster than podman on Namespace, and a Go-only change is 79x faster.
With a warm cache the Go jobs also run 2.5x to 6.8x faster than on GitHub-hosted.

**E2E in CI, later.**
Namespace creates Kubernetes clusters on demand.
A single-site run is a guess of $0.16 to $0.24 (`docs/ci/CI-COSTS-E2E.md`).
Open: whether clusters can route pod networks to each other (ADR-016), and whether a runner reaches a node by NodePort.

**Costs and caveats.**
- CI costs about $0.04 per push, $3.4 to $5 a month at October's volume.
- The Go cache volume missed on three of five runs, so warm Go times vary.
- The Makefile image targets are broken until the bake layout is migrated into them.
