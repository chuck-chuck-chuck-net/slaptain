# Version numbering: where the `v` goes, and where it must not

**The rule, in one line:** the `v` belongs to the **git tag** and to nothing else.

Everything downstream of the tag — container image tags, chart versions, chart `appVersion`,
`helm --version`, CHANGELOG headings — carries the bare semver: `0.2.0`, not `v0.2.0`.

SemVer 2.0.0 says outright that `v1.2.3` is *not* a semantic version; `1.2.3` is, and the `v`
is a common prefix for the *tag that points at it*. Git tags conventionally carry it; the
artifacts named by that tag should not.

Be precise about how much of that is a rule. For `Chart.yaml`'s `version:` it is enforced —
Helm rejects a leading `v` outright. For **container image tags it is convention, not law**:
`:v1.2.3` is a perfectly legal Docker tag and respectable projects ship it (cert-manager,
among others). So the case for stripping it there is not "the other way is invalid"; it is
that one spelling everywhere makes `appVersion` and the image tag the *same string*, which
turns the charts' `image.tag | default .Chart.AppVersion` fallback from a coincidence that
has to be maintained into an identity that cannot drift. That is worth more than either
spelling on its own merits, and it is why picking the same one as the sibling project costs
nothing.

---

## 1. The surfaces, and which spelling each takes

| Surface | Spelling | Example |
|---|---|---|
| git tag | **with `v`** | `v0.2.0`, `v0.2.0-rc1` |
| GitHub release title | **with `v`** (mirrors the tag) | `v0.2.0` |
| container image tag | bare | `slapd:0.2.0` |
| Helm chart version (the OCI tag) | bare | `charts/slapd:0.2.0` |
| `Chart.yaml` `version:` | bare | `version: 0.2.0` |
| `Chart.yaml` `appVersion:` | bare | `appVersion: "0.2.0"` |
| `helm --version <x>` | bare | `--version 0.2.0` |
| CHANGELOG heading | bare | `## [0.2.0] - 2026-09-19` |

Two of these are not free choices:

- **`Chart.yaml` `version:` must be strict SemVer-2** — Helm rejects anything else, and a
  leading `v` is not SemVer. Off-tag builds therefore need a pseudo-version such as
  `0.0.0-<hash>`, which is a valid pre-release. (This is also why there is no `latest` *chart*
  tag anywhere: Helm reads `--version` as a semver constraint, and `latest` is not one.)
- **`helm --version` against an OCI registry is passed through as the registry tag.** It does
  not normalise, and it does not fall back. Measured on the sibling project:

      $ helm show chart oci://ghcr.io/.../charts/littlered --version v0.4.0-rc1
      Error: failed to perform "FetchReference" on source:
        ghcr.io/.../charts/littlered:v0.4.0-rc1: not found

      $ helm show chart oci://ghcr.io/.../charts/littlered --version 0.4.0-rc1
      version: 0.4.0-rc1

  So getting the spelling wrong is a hard 404, not a graceful miss.

---

## 2. Where slaptain stands: adopted at v0.2.1

**Adopted 2026-09-19.** The rule above is slaptain's, identical to the sibling project's, and
the Makefile and `tests/e2e.sh` implement it.

What changed was one variable becoming two. `GIT_TAG` (from `scripts/image-tag.sh`) answers
*what is HEAD* and is unchanged — a release tag, else a short hash, else a content-hashed
dirty form. `IMAGE_TAG := $(GIT_TAG:v%=%)` answers *how is it addressed*, and every image
name, every `--set image.tag=`, `--app-version` and `CHART_VERSION` now derives from it. The
off-tag hash forms carry no `v` and pass through untouched, so only release builds are
affected.

    $ make show-tag GIT_TAG=v0.2.1
    git:    v0.2.1
    image:  0.2.1
    chart:  0.2.1

One string for all three, which is the point: the charts default their image tag to
`.Chart.AppVersion`, so `helm install ./charts/operator` from a checkout and the image a
release build pushes now name the same thing *by construction*.

**What this fixed, concretely.** Before adoption, a git checkout rendered
`…/operator:0.2.1` (from the in-tree bare `appVersion`) while a release build published
`…/operator:v0.2.1`. `helm install ./charts/operator` therefore pulled a tag that would
never exist; the `make` targets masked it by passing `--set image.tag=$(GIT_TAG)` explicitly,
so it only bit a user doing the obvious thing. The published **v0.2.0 release is not affected**
and needs no correction: it was packaged with `--app-version $(GIT_TAG)`, so its chart carries
`appVersion: v0.2.0` and its image is `operator:v0.2.0` — both spellings wrong under this
rule, but wrong *together*, so it resolves. Verified against the registry rather than assumed:

    helm template slaptain oci://ghcr.io/chuck-chuck-chuck-net/charts/slaptain --version 0.2.0
      → image: "ghcr.io/chuck-chuck-chuck-net/slaptain/operator:v0.2.0"   (registry: 200)

**The discontinuity is deliberate and stops here.** Images through `v0.2.0` carry the `v`
(`v0.0.18` … `v0.2.0`); `0.2.1` onwards do not. History is immutable and honest; a partial
re-tag would be worse than either extreme. Chart *versions* never carried it, so the chart
series is continuous — only `appVersion` changes spelling, from `v0.2.0` to `0.2.1`.

## 3. How it was adopted (kept for the reasoning, the work is done)

1. **Split "what this build IS" from "how it is ADDRESSED."** They are different questions and
   one variable cannot answer both — `GIT_TAG` feeds `CHART_VERSION`, where a `v` is illegal,
   *and* image names, where today it is mandatory. Introduce a second variable:

       IMAGE_TAG := $(if $(filter v%,$(GIT_TAG)),$(GIT_TAG:v%=%),$(GIT_TAG))

   and use `IMAGE_TAG` in every `*_IMAGE` definition. `GIT_TAG` keeps its current meaning and
   `scripts/image-tag.sh` does not change — the off-tag hash forms pass through untouched.

2. **Decide what happens to the already-published `v0.0.x` images.** They are immutable history.
   Either leave them (the convention starts at the next release, stated in the CHANGELOG) or
   re-tag copies without the `v`. Leaving them is simpler and honest; a partial re-tag is worse
   than either extreme.

3. **Only then let a release tag be cut.** Once images and `appVersion` agree, the chart's
   default-image fallback is correct by construction and needs no code change.

---

## 4. Four traps, all of them observed in practice

These are the ways the rule gets broken *after* someone has written it down. Each one was hit
for real on the sibling project; none was caught by a test.

1. **Using the git tag verbatim as an image tag.** Produces `:v0.4.0`, which no CI ever
   publishes, so every default pull 404s at the one moment it matters most — the release.
   This was slaptain's state until v0.2.1.

2. **Reusing an `IS_RELEASE_TAG`-style predicate to decide *addressing*.** A predicate written
   to gate `:latest` deliberately excludes pre-releases (an rc must not move `latest`). Key
   image addressing off it and an rc falls into the "untagged" branch and gets a nonsense tag
   like `sha-v0.4.0-rc1`. The question to key off is *"is HEAD tagged at all"*, which is a
   different question. Two predicates, not one.

3. **`--app-version $(GIT_TAG)` while `--version` strips the `v`.** The chart then carries
   `appVersion: v0.4.0`, and because the templates default the image tag to `appVersion`, the
   chart's default image points at a tag that does not exist. This shipped once on the sibling
   project: its published `0.2.1` chart carries `appVersion: v0.2.1` while `0.2.2` and `0.3.0`
   carry the bare form — the fingerprint of a hand-packaged release. slaptain was exposed to
   the same failure from the opposite direction — images kept the `v`, the in-tree
   `appVersion` did not — which is what §2 resolves.

4. **Telling users `--version <version>` without saying which spelling.** The releases page
   shows `v0.2.0`; the chart wants `0.2.0`. A README that links to the former while asking for
   the latter is a coin flip, and losing it is a 404 rather than a warning. Write the mapping
   out: *"add `--version <version>` — without the leading `v`, e.g. `--version 0.2.0` for the
   `v0.2.0` release."*

The common shape of all four: **one variable was asked two different questions.** The fix is
never a cleverer regex; it is a second variable with a name that says which question it
answers.

---

## 5. Checklist for a new release surface

Before adding anything that embeds a version — a new chart, a new image, a manifest, a
download URL, a `--version` flag in docs:

- [ ] Does it derive from the git tag? Then strip the `v` unless it *is* a git ref.
- [ ] Is it a Helm `version:` field? Then it must be strict SemVer-2, so the `v` is not
      merely discouraged but invalid.
- [ ] Does anything default to it (an `appVersion` fallback, an env var, a docs example)?
      Check that the thing it resolves to actually exists in the registry — not that it looks
      plausible.
- [ ] Does a published artifact's spelling now disagree with an older one? Say so in the
      CHANGELOG rather than quietly re-tagging history.

---

## 6. How to verify, rather than assume

The registry is the authority on what was actually published. Anonymous, no credentials, so it
answers the question a *user* would get:

    # every tag of one image
    repo=chuck-chuck-chuck-net/slaptain/operator
    tok=$(curl -s "https://ghcr.io/token?scope=repository:$repo:pull" | jq -r .token)
    curl -s -H "Authorization: Bearer $tok" "https://ghcr.io/v2/$repo/tags/list" | jq .tags

    # a published chart's real metadata (version AND appVersion)
    helm show chart oci://ghcr.io/chuck-chuck-chuck-net/charts/slapd --version 0.2.0

The second is the one worth running after every release: it reads back what the pipeline
actually stamped, which is the field the four traps above all corrupt.
