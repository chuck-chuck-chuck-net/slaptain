# ADR-035: ADRs conflict, and no ADR wins by default

**Status:** Accepted
**Date:** 2026-10-15

## Context

Two accepted decisions collided for the first time in a way neither anticipated.

ADR-013 retired every pod-template roll the operator caused as a side effect:
a database added, a replica count moved, a value the operator derived. Its
2026-10-08 amendment stated the rule for a new feature's own fields — unset or
off must be byte-identical to the template without the feature, so that
upgrading the operator onto an existing cluster rolls nothing.

ADR-034 then added a ConfigMap volume the init container reads at pod start.
It is mounted unconditionally, so the operator carrying it rolls every cluster
once, including clusters using none of it.

Read generically, ADR-013's rule decides this on its own, and keeps deciding:
every later feature that needs anything in the pod must either hide inside an
object named for something else, or be gated on a field that did not exist when
the earlier cluster was installed. The first produces names that lie — the
honest alternative here was a table of startup-only `cn=config` attributes
carried inside a ConfigMap called `database-dirs` — and the second produces
permanent compatibility scaffolding around a v1alpha1 API. Both are costs paid
forever to avoid one restart of a system built to survive restarts.

That is a general shape, not a detail of this feature: a rule written to stop
unwanted churn, applied as an absolute, buys stability in pod templates with
clarity in everything a reader sees.

## Decision

**R1 — No ADR outranks another by number, by recency, or by being cited
first.** A conflict between two accepted decisions is itself a decision, taken
on the merits of the case and recorded in both. An ADR constrains; it does not
adjudicate.

**R2 — Before 1.0, the clean design wins where the two pull apart.** The
project is v1alpha1 and says so in its CRDs. The standing preference is to name
things for what they are and build them where they belong, and to pay a
migration cost now rather than carry a disguise for the life of the project.
This inverts at 1.0, when the compatibility promise starts and the trade
genuinely reverses.

**R3 — ADR-013's subject is the SlapdCluster's life, not the operator's
version.** The rolls it retired all happen while a cluster runs and nobody
asked for them. **An operator upgrade is not one of those**: it is a deliberate
act, taken by a human who chose a version, at a moment of their choosing, and
it already re-images the data plane for every cluster that has not pinned its
images. A new operator version may therefore change the pod template once.
What it may still never do is roll a fleet because a database was added, a
replica count moved, or a value was derived — which is what ADR-013 was written
to stop, and that part stands unchanged.

**R4 — The case at hand.** ADR-034 keeps its own `<cluster>-startup-config`
ConfigMap, named for what it holds, and accepts the single roll at the upgrade
that introduces it. ADR-033's pod-template goldens are regenerated deliberately
when the two features meet, citing this ADR.

## Consequences

- ADR-013's 2026-10-08 amendment keeps its force for what it was written
  about: a **spec field the user flips**, which must default to the state that
  adds nothing to the pod so that the upgrade itself is not the trigger. Read
  under R3, it does not reach a structural change that arrives with a new
  operator version.
- The corpus needs a consistency pass. Several ADRs are phrased as absolutes
  because, when they were written, nothing had yet contradicted them. Each
  such phrasing is a future collision resolved by whoever reads it first rather
  than on the merits. This ADR does not fix them; it is what a reader should
  reach for when one of them bites.
- A conflict resolved under R2 is still a cost and gets stated as one. "We are
  pre-1.0" explains a choice; it does not make the migration free, and the ADR
  taking the exemption says what it costs whom.
- After 1.0 this ADR is the thing to revisit first.

## Related

- ADR-013 — the pod-template rule whose generic reading prompted this, and
  whose scope R3 states.
- ADR-033 — the amendment that phrased the byte-identical requirement, and the
  goldens R4 regenerates.
- ADR-034 — the feature that collided, and the case R4 decides.
