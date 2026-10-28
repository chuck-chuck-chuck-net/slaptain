# ADR-046: Branch images go to a public dev/ path, tagged by branch, and expire

**Status:** Proposed
**Date:** 2026-10-29

Container images are built in CI for every Pull Request labelled `dev images`,
so its author, QA and people outside the project can test it
without manually building the images on their laptop
and uploading them to a registry their cluster can access.

## Context

CI publishes images from `main` (`sha-<hash>`) and from `v*` tags (the version, release candidates included).
A branch reaches a cluster only through a hand-run `make push`.

## Decisions

1. **Build images from branches?**
   - Yes.
   - Discarded:
     - No, only `main` and releases: every tester builds the images themselves.
2. **When?**
   - On every push to a pull request labelled `dev images`,
     so the maintainers choose which branches get images and from when.
   - Removing the label stops new builds and keeps the existing images.
   - Discarded:
     - Every push to every pull request: no say over which branches get images.
     - Every push to every branch: runner time for branches nobody tests.
     - On demand: someone has to start every build by hand.
     - Deleting the images when the label is removed:
       stopping new builds is not the same as being done testing.
3. **Where?**
   - A `dev/` namespace under the project's own registry owner.
   - Discarded:
     - The release packages: tag pollution, and hard to clean up.
     - A separate registry owner: another credential to manage.
     - An external registry: an added dependency.
4. **Visible to whom?**
   - Public.
   - Discarded:
     - Private: needs a pull Secret and shuts out outsiders.
5. **Which tag?**
   - The branch name as a slug, moving with every build. Pin by digest when needed.
   - Discarded:
     - The commit: the tester has to look up a hash to get the latest build.
     - Both: two tags per image for a need the digest already covers.
6. **Where do release candidates go?**
   - With the releases, as today.
   - Discarded:
     - `dev/`: an RC is a release to publish, not a branch to test.
7. **Clean up?**
   - Yes, with 7 days' retention to start. Adjust when needed.
   - Also when the pull request closes.
   - Discarded:
     - No cleanup: dev images pile up.
     - Age only: a merged branch keeps its images for up to a week.
8. **What may cleanup delete?**
   - Only `dev/`. Never a mainline or release candidate image.
   - Discarded:
     - Anything matching a tag pattern: one wrong pattern deletes releases.
9. **Charts for branches?**
   - Later, if and when necessary.
     A branch chart only helps once the chart carries the image tags and digests.
   - Discarded for now:
     - Dev charts in `dev/`.

## Consequences

- A pull request from a fork gets no images,
  because GitHub gives its workflows a read-only token.
- A branch tag moves, so a running pod picks up a new build only on a pull or a restart.
- Branch names and builds are public for up to 7 days.
- Only people with triage or write access can label a pull request,
  so a contributor cannot start builds for their own.
- Testing a branch needs a slaptain checkout for the charts (decision 9).

## Related

- `docs/VERSIONING.md`: the tag rules for `main` and releases, unchanged here.
- ADR-040: how a data-plane image is named and selected.
