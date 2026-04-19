# ADR-006: Schema Lifecycle and Update Semantics

**Status:** Proposed
**Date:** 2026-04-19

## Context

OpenLDAP schemas are stored in `cn=schema,cn=config` as LDAP entries. Each schema entry
contains lists of `olcAttributeTypes` and `olcObjectClasses`. Schemas are **global** — once
loaded, they are visible to all databases on the slapd process.

The `SlapdSchema` CR (ADR-004) declares the desired schema elements. The controller must
apply these to every pod's cn=config individually (cn=config is node-local, per ADR-002).
The key design question is: what does the controller do when the CR spec changes?

### OpenLDAP's Schema Modification Constraints

| Operation | Supported | Constraint |
|-----------|-----------|------------|
| Add new attributeType | Yes | OID must be unique |
| Add new objectClass | Yes | OID must be unique; referenced attributeTypes must exist |
| Delete attributeType | OpenLDAP 2.5+ only | Only if no objectClass references it and no entry uses it |
| Delete objectClass | OpenLDAP 2.5+ only | Only if no entry uses it |
| Modify attributeType (change syntax, etc.) | No | Must delete and re-add |
| Replace entire attribute list | Technically yes | Dangerous if entries depend on removed definitions |

In practice, schema evolution in OpenLDAP deployments is **additive only**: new attributeTypes
and objectClasses are added over time. Mature schemas accumulate 10+ incremental additions
over their lifetime, with zero modifications or deletions.

## Decision

The `SlapdSchema` controller implements a **desired-minimum** model:

### What the controller does

1. **Ensures all declared elements exist.** For each `attributeType` and `objectClass` in the
   CR spec, the controller checks whether it exists in the pod's `cn=schema,cn=config`. If
   missing, it adds it via `ldapmodify`.

2. **Reports status per pod.** `status.appliedToPods` lists pods where all declared elements
   are present. `status.failedPods` lists pods where application failed (with reason).

3. **Detects drift.** If an element exists in cn=config with a different definition than the
   CR spec (e.g. different syntax), the controller reports this in a `SchemaDrift` condition
   but **does not auto-modify**.

### What the controller does NOT do

1. **Does not delete elements.** Removing an attributeType from the CR spec does not delete
   it from cn=config. The controller manages the minimum set, not the exact set. Extras are
   tolerated.

2. **Does not modify existing elements.** If a declared attributeType already exists with a
   different definition, the controller reports drift but does not attempt to replace it.
   Modifying schema elements in a running directory risks breaking existing entries.

3. **Does not enforce ordering between attributeTypes.** OpenLDAP resolves internal
   dependencies at load time. The controller adds elements in the order declared in the CR
   spec, which must be dependency-ordered (attributes before objectClasses that reference
   them).

### Schema Evolution Workflow

To evolve a schema:

1. **Add new elements:** Update the `SlapdSchema` CR to include new attributeTypes or
   objectClasses. The controller adds them to all pods on the next reconcile.

2. **Modify existing elements:** Not supported by the operator. If an attribute definition
   must change (e.g. syntax change), this requires a manual migration:
   - Ensure no entries use the attribute
   - Delete the old definition from each pod's cn=config manually
   - Update the CR with the new definition
   - The controller adds the new definition on the next reconcile

3. **Remove unused elements:** Not managed by the operator. Remove manually from cn=config
   if desired. The controller will not re-add elements that are not in the CR spec.

This is consistent with standard OpenLDAP schema management practice: incremental additions
only. Destructive changes are manual operations with human oversight.

### Structured Format

Schema elements use the standard RFC 4512 syntax in string form:

```yaml
spec:
  attributeTypes:
    - >-
      ( 1.3.6.1.4.1.99999.1.1.1
        NAME 'myAppContextId'
        EQUALITY integerMatch
        SYNTAX 1.3.6.1.4.1.1466.115.121.1.27 )
  objectClasses:
    - >-
      ( 1.3.6.1.4.1.99999.1.2.1
        NAME 'myAppUser'
        SUP inetOrgPerson
        STRUCTURAL
        MAY ( myAppContextId $ myAppUserId ) )
```

This format:
- Matches how OpenLDAP stores schemas in cn=config (`olcAttributeTypes`, `olcObjectClasses`)
- Matches how schema RFCs define elements
- Is diffable in version control (unlike opaque JSON blobs)
- Can be validated syntactically before applying (OID format, referenced types exist)

## Consequences

- Schema evolution is additive — the controller only adds, never removes or modifies. This
  matches OpenLDAP's own constraints and standard practice.
- Drift detection provides visibility without risk — operators see when cn=config diverges
  from the CR spec without the controller making destructive changes.
- Manual migration is required for non-additive schema changes. This is intentional: changing
  a schema attribute's syntax in a directory with live data is a high-risk operation that
  requires human judgment.
- The structured format (RFC 4512 strings) replaces the opaque JSON blob format used in the
  current `spec.ldap.schemas`. This is a breaking change at v1alpha1.

## Related

- ADR-002: cn=config is node-local — schemas are applied to each pod individually.
- ADR-004: Multi-resource CRD architecture — `SlapdSchema` is one of the three CRDs.
