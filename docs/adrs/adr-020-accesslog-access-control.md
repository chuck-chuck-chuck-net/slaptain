# ADR-020: An accesslog database is at least as restrictive as the database it journals

**Status:** Accepted
**Date:** 2026-08-25

## Context

The operator creates the accesslog database with no `olcAccess` at all
(`ensureAccesslogDB`, `slapddatabase_controller.go:1099`). Neither the init
container nor any chart sets a frontend or global ACL. A slapd database with no
`olcAccess` inherits the frontend's default access, and that default is **read**:

```c
	/* servers/slapd/frontend.c */
	frontendDB->be_dfltaccess = ACL_READ;
```

```c
	/* servers/slapd/backend.c */
	be->be_dfltaccess = frontendDB->be_dfltaccess;
```

So the change journal is readable by anyone who can reach the port, including
anonymously.

That would be unremarkable if the data tree were equally open — and on a
`SlapdDatabase` with no `acls:` it is, since the same default applies. The gap is
the configured case, which is the normal one. Our own example fixture
(`tests/resources/example/database.yaml`) declares:

```yaml
    # userPassword hidden everywhere else (self-write + anon-auth only).
    - >-
      to attrs=userPassword
      by self write
      by anonymous auth
      by * none
```

Meanwhile the accesslog overlay logs every write, and a password change is a
write: the resulting `auditWriteObject` entry carries the new value in `reqMod`.
The journal therefore hands out, to anonymous readers, precisely the attribute
the database is configured to deny them. Every ACL a user writes on a data DB is
bypassable through that database's log, for any value that has been written since
the last purge.

This is not specific to `userPassword` — it applies to any attribute an ACL
restricts. `userPassword` is simply the case where the fixture makes the intent
unambiguous.

The gap was surfaced while working through ADR-019 and is independent of it:
a shared log has it, per-database logs have it. ADR-019 is nonetheless the right
moment to close it, because it rewrites `ensureAccesslogDB` anyway and gives each
log exactly one database whose ACL intent it must respect — which is what makes
the rule below expressible at all.

## Options considered

**Leave it; document that the accesslog is sensitive.** Rejected. The whole
point of `SlapdDatabase.spec.acls` is that the user states an access policy and
the operator enforces it. Enforcing it on one of two doors is not enforcement,
and the open door is the one nobody thinks to look at.

**Mirror the data DB's `spec.acls` onto the log.** Rejected. The rules are
written against the data DIT — they name `dn.subtree="ou=Mail,dc=example,dc=org"`,
`attrs=userPassword`, `by self` — and none of that has any meaning against
`auditWriteObject` entries under `cn=accesslog-*`. `by self` in particular would
silently match nothing. Translating data-tree ACLs into journal ACLs is a
transformation with no correct definition.

**Expose a `spec.replication.accesslogACLs` passthrough.** Rejected for now.
It is a real escape hatch, but there is no demonstrated need: exactly one
identity has a legitimate reason to read the journal, and the operator already
knows which. Adding user-facing ACL surface for a database the user never
declared is API weight ahead of demand. Revisit if a concrete case appears.

**Grant the replication identity, deny everyone else.** Chosen.

## Decision

**Each accesslog database carries an explicit `olcAccess` granting read to the
replication bind DN of the database it journals, and nothing to anyone else.**

- **R1 — The rule is fixed and derived, not user-supplied.** The operator writes
  exactly:

  ```
  to * by dn.exact="cn=replication,<suffix>" read by * none
  ```

  where `<suffix>` is the journalled database's suffix. It is the same identity
  and the same `dn.exact` form the data DB's own replication grant already uses
  (`applyACLs`, `slapddatabase_controller.go:676-682`), so the two stay in step by
  construction.

- **R2 — This adds no constraint that `applyACLs` does not already impose.**
  Delta-syncrepl consumers must read *both* the data DB and its log. On any
  database with `acls:` configured, a consumer that does not bind as
  `cn=replication,<suffix>` already fails against the data DB. Restricting the
  log to the same DN therefore changes nothing for a working topology and closes
  the bypass for everything else. This is the property that makes the change
  safe to fold into ADR-019 rather than stage separately.

- **R3 — rootDN bypass is the operator's access path, and stays implicit.** The
  log's `olcRootDN` is `cn=admin,cn=config`, and a rootDN bypasses ACLs, so the
  operator's own cn=config work is unaffected. Do not add an explicit grant for
  it; an ACL line that restates a bypass is noise that later readers will mistake
  for a requirement.

- **R4 — Offline paths are unaffected and must stay that way.** `slapcat` and
  `slapadd` in backup and restore Jobs (ADR-014) read the LMDB files directly and
  never evaluate ACLs. Nothing in this ADR may be implemented in a way that makes
  a Job depend on a bind identity.

- **R5 — The ACL is part of creating the log, not a separate reconcile step.**
  It is written in `ensureAccesslogDB` alongside the DB entry and its syncprov
  overlay, and converged on every reconcile like the data DB's ACLs are. A log
  that exists without its ACL is the exact state this ADR exists to prevent, so
  the window in which one exists must be as close to zero as the other two
  attributes' is.

## Consequences

- Anonymous and ordinary authenticated clients can no longer read any accesslog.
  For a cluster whose data DB has no `acls:` this is a tightening relative to
  today, deliberately: the journal is operator-managed infrastructure, not part
  of the user's data surface, and there is no reading of "the user did not
  configure ACLs" that implies "the change journal should be public".
- Diagnostics that need journal contents must bind as the config rootDN.
  `slctl ldapsearch --as config` already does; `--as admin` (the *data* rootDN)
  will now be denied on the log, which is correct — it is a different database
  with a different rootDN. Worth a line in the `slctl` docs, since the failure
  would otherwise look like a bug.
- `slctl inspect`'s naming-context checks read `namingContexts` from the rootDSE,
  which is frontend-governed and unaffected by per-database ACLs. The e2e should
  assert that explicitly rather than assume it, since the check is load-bearing
  for ADR-019's per-database log assertions.
- External peers must bind as `cn=replication,<suffix>` to consume delta from
  us. Per R2 this is already true wherever `acls:` is set; on an ACL-less
  database it becomes newly true. Called out here because `ExternalPeer.bindDN`
  is user-supplied and a mesh that relied on the permissive default would break
  — loudly, at the consumer, which is the right place.
- No CRD change, no RBAC change, no new Secret. The rule is derived from the
  suffix the operator already has in hand.

## Related

- ADR-004 — ACLs are declared on `SlapdDatabase`; this keeps that declaration
  meaningful by closing the path around it.
- ADR-008 — replication credentials; `cn=replication,<suffix>` is the identity
  this ADR grants, and the uniform-password assumption is why one DN suffices.
- ADR-014 — backup and restore run offline `slapcat`/`slapadd` (R4).
- ADR-019 — one accesslog per data database. Independent of this gap, but the
  change that makes "the log of *this* database" a well-defined thing to write an
  ACL for, and the pass in which this is implemented.

## References

- `slapd.access(5)` — ACL syntax and evaluation order:
  <https://www.openldap.org/software/man.cgi?query=slapd.access>
- `slapo-accesslog(5)` — the audit schema; `reqMod` carries the modified values:
  <https://www.openldap.org/software/man.cgi?query=slapo-accesslog>
- OpenLDAP Administrator's Guide 2.6, *Access Control*:
  <https://www.openldap.org/doc/admin26/access-control.html>
- slapd sources, `git.openldap.org/openldap/openldap`:
  - `servers/slapd/frontend.c` — `frontendDB->be_dfltaccess = ACL_READ` (`:99`)
  - `servers/slapd/backend.c` — per-backend inheritance of the default (`:616`)
  - `servers/slapd/acl.c` — `be_dfltaccess` consulted when no ACL matches
    (`:206-211`)
