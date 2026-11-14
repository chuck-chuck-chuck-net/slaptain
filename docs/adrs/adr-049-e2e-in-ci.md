# ADR-049: The e2e suite runs in CI on kind, in a smoke and a slow tier

**Status:** Proposed. The smoke tier is implemented, the slow tier is planned.
**Date:** 2026-11-14

ADR-047 decision 8 left the e2e suite out of CI until images reached a registry
any cluster can pull from.
They do now (ADR-046), and this ADR decides where and when the suite runs in CI,
and which specs run there.

## Context

The suite ran only in one maintainer's lab,
so a pull request reached review without a single e2e run.

Most specs need one Kubernetes cluster.
Multi-site and external replication need two, with routed pod networks (ADR-016).

## Decisions

1. **Where does the suite run?**
   - On a kind cluster on a GitHub-hosted runner.
     It needs no account beyond GitHub, as ADR-047 asks of CI,
     and one cluster holds every spec CI runs.
   - A Namespace cluster stays an option (ADR-047 decision 6).
2. **Which images does it test?**
   - The pull request's dev images (ADR-046),
     so the suite tests what the pull request would ship, and the job builds nothing.
   - The job waits until `publish-dev` has pushed its own commit's images,
     because the branch tag still points at the previous push until then.
     When a newer push cancelled those builds, the tag will never show this commit,
     and the job cancels itself rather than test a newer commit under this one's name.
3. **When does it run?**
   - On every push to a pull request labelled `dev images`, and when the label is added.
     Dev images build only under that label, and a run occupies a runner for minutes,
     so the author opts in.
   - `e2e.yml` is a workflow of its own, so it can wait for the images
     and be rerun without rebuilding them.
   - Runs of one commit and tier cancel each other.
     Opening a pull request together with its labels starts one run per event,
     and only one per tier needs to finish.
   - A new push never cancels an older commit's run.
     On a branch pushed in quick succession, no run would finish otherwise.
   - Pull requests from forks get no dev images, so they get no e2e run.
   - Deferred: a run on `main`.
4. **What does the smoke tier run?**
   - The ungated specs, minus those that roll, restart or change the cluster for minutes.
     They take most of the suite's time,
     and smoke is meant to answer within minutes of every push.
   - The Secret rotation spec is one of them.
     The operator does not watch the Secret,
     so the spec waits twice for the five-minute resync.
   - Flaky specs stay out until they are fixed,
     so a red smoke run means the change broke something.
5. **Does a run stop at the first failure?**
   - No. Building the images and starting the cluster take longer than the specs,
     so stopping early saves little and reports far less.
   - The job summary records the Ginkgo seed,
     and a failed run uploads the cluster state,
     so a failure can be read without rerunning the job and replayed in the same order.
6. **How does the job save time?**
   - The job waits for the images anyway,
     so it starts kind and compiles the suite in the meantime.
7. **What does kind need from the images?**
   - The slapd launcher of ADR-048.
     kind on GitHub-hosted runners hands slapd an open-file limit of 2^31,
     and without the launcher slapd opened no listener before the startup probe gave up.
8. **What does the slow tier run?**
   - The specs smoke leaves out, started by the `e2e slow` label.
   - Planned. A later revision of this ADR decides its contents and layout.

## Consequences

- A pull request labelled `dev images` gets a smoke result
  a few minutes after its images are pushed.
- A spec on the slow list gets no regular run until the slow tier lands.
- A regression that only appears once two pull requests merge
  shows up on the next labelled pull request, not on `main`.
- kind on the runner is the regression test for ADR-048.

## Not covered

- A run on `main`, deferred above.
- Multi-site and external replication, which need a second cluster.
- Pull requests from forks.

## Related

- ADR-016: pod-routed cross-cluster replication, which the uncovered specs need.
- ADR-046: the branch images the suite tests.
- ADR-047: the CI architecture, whose decision 8 this ADR replaces.
- ADR-048: the open-file limit kind needs.
