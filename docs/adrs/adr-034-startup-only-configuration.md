# ADR-034: Startup-only configuration has a change path — the init container converges `cn=config` offline

**Status:** Accepted
**Date:** 2026-10-14

## Context

Most of `cn=config` can be modified on a running `slapd`, and ADR-024 R1 makes
that the default: a tunable converges per pod on every reconcile. A minority
cannot, and this repository has collected that minority the hard way — each
entry below was found by killing or hanging a live server:

- `olcDbMaxSize` and `olcDbEnvFlags` — segfault on live modify (ADR-024
  amendment).
- `olcIdleTimeout`, `olcWriteTimeout` and the monitor ACL MOD — hang slapd
  (same amendment).
- `idlExponent` — governs the on-disk index layout, so it is written at
  bootstrap and never again (ADR-024 R2).

For that class the change path on record is "recreate the database" — a dump
and reload, with the outage that implies — or nothing at all. Yet nearly every
member of the class needs no more than a new configuration file and a restart:
LMDB fixes `mapsize` at environment open, so growing `olcDbMaxSize` is a
restart-scale operation that we document as a recreate because we have no way
to get a new value into `slapd.d` without a running server to write it through.

`olcSpSessionlogSource` is the setting that forces the question. Pointing a data
database's syncprov session log at its accesslog keeps one copy of the history,
on disk, across restarts, instead of a second copy in RAM that does not survive
one. Setting it on a running server is a double free:

```c
case SP_LOGDB:
    if ( CONFIG_ONLINE_ADD( c ) ) {
        if ( !select_backend( &c->value_ndn, 0 ) ) { ...; break; }
        ch_free( c->value_ndn.bv_val );      /* syncprov.c:3905 */
    }
    si->si_logbase = c->value_ndn;           /* 3907 — stores the freed pointer */
```

`si_logbase.bv_val` is dangling from assignment, and the delete path frees it
again at `syncprov.c:3814`. Reproduced 60/60 under AddressSanitizer with both
frees in `sp_cf_gen`; the startup path is clean over 200 writes plus a runtime
delete. It is not a known ITS and the code is byte-identical in 2.5.20, 2.6.0,
2.6.10, 2.6.15, 2.7.1 and master — there is no release to upgrade to. It has
already cost two live providers.

Two facts from the sources bound every option below:

1. `CONFIG_ONLINE_ADD(ca)` is `(!(ca)->lineno)`, and `config_add_internal`
   sets `lineno = 0` for **any** LDAP operation against `cn=config` and `1`
   for the configuration-directory read at startup (`bconfig.c:5497-5505`).
   An LDAP **Add** of the syncprov overlay entry carrying the attribute is
   therefore as fatal as a Modify. Because ADR-004 has the SlapdDatabase
   controller create the data database and its overlays online, there is no
   "create it with the attribute" shortcut: the attribute can only ever enter
   through `slapd.d` as read at startup.
2. `syncprov_db_open` resolves the configured DN with `select_backend` and
   returns `-1` when it is absent (`syncprov.c:4160-4168`). A value that was
   correct when written and whose target has since gone makes the pod
   **unbootable** — the failure shape of the withdrawn ADR-019 R8, which is
   why ADR-026 R2 exists.

So the bug removes our discretion about *how strictly* to treat this attribute.
It does not create the class. The class is slapd's, it predates the bug, and
it has no change path today.

## Options considered

### Option 1: Do not support it; make the field immutable

Reject a change to any startup-only setting after creation.

− Immutable means "recreate the database" for a tunable — strictly more
  expensive than the restart we are trying to avoid, and the status quo we
  want to retire for `olcDbMaxSize`.
− It also hides the real constraint behind an API rule: the setting is not
  immutable, it is unwritable *while slapd runs*.

### Option 2: Write it online, then restart

− The write itself is the crash. There is no window in which the value is set
  and the server is still alive to be restarted.

### Option 3: A co-located Job edits `slapd.d` on a running pod (ADR-018 shape)

Mount the config PVC from a Job pinned to the pod's node, as `backup_job.go`
already does, and write the file under the running server.

− A `cn=config` write by the operator in the meantime rewrites the same entry
  from slapd's in-memory copy and silently discards the edit.
− The edit has no effect until a restart anyway, so the Job buys nothing over
  doing it at the next pod start, when nothing else holds the configuration.

### Option 4: Rewrite the LDIF files directly

− back-ldif verifies the `# CRC32` header when it reads an entry file
  (`ldif.c:460-477`), so a hand-written file must carry a correct checksum.
− `slapmodify -F <dir> -n 0` already does this correctly, and reaches
  `config_tool_entry_modify` (`bconfig.c:8229`), which delegates straight to
  back-ldif's tool modify. The configuration parser never runs, so `sp_cf_gen`
  is never reached and no online path is taken.

### Option 5: The init container converges a declared table offline at every pod start

+ The only moment in a pod's life when the configuration is on disk and
  nothing holds it open.
+ `slapcat`/`slapmodify` are in the init image already, from the same package
  build as the server (ADR-021), so the tools can never drift from the
  configuration they rewrite.
+ One mechanism serves the whole class; a further attribute is a table entry.
− The init container currently never touches an existing configuration. That
  invariant goes, and with it the guarantee that a pod restart cannot change
  anything.
− The value only lands at a restart, so a spec edit and its effect are
  separated by an operation a human performs.

## Decision

Option 5, with the operator confined to observing it.

R1 — **Startup-only attributes are declared in one table**: the attribute, the
`cn=config` entry that carries it, and how its value is derived. The init
container converges exactly that table offline at every pod start, with
`slapcat -n 0` to read and `slapmodify -n 0` to write. Idempotent: an already
correct configuration produces no write.

R2 — **The operator never writes a startup-only attribute to a running slapd.**
A guard refuses such a request before it reaches the wire, so no later change
can reintroduce the path by accident. Observation stays read-only: a reconcile
that finds the live value wrong reports a condition and does not issue an
`ldapmodify`. Putting one of these attributes into a converging reconcile loop
makes the operator a crash loop that fires on the next pass after any edit.

R3 — **The change path is a restart, and the operator does not take it.** A
divergence sets `ConfigRestartRequired` on the owning resource, naming the pods.
Restarting a provider fleet is a topology event at every peer (ADR-016, 130-268 s
of address rediscovery each) and a tunable edit must not trigger one behind the
user's back. An opt-in, operator-driven roll belongs to ADR-023's
`SlapdRollingReplace`, not here.

R4 — **The converger refuses on doubt.** It writes only when the target entry
and every backend the value references are present in the same configuration
dump, and it verifies by reading the configuration back; a failed read-back
fails the init container rather than handing slapd a configuration it will
reject. The error it must never produce is an unbootable pod.

R5 — **One customer per change.** `olcSpSessionlogSource` is the first and, for
now, only table entry. Moving an existing recreate-path tunable onto the vehicle
is a separate decision per tunable, with its own evidence on a live cluster —
`olcDbMaxSize` and `olcDbEnvFlags` are segfaults rather than a double free, and
their "restart is enough" claim is reasoned, not measured.

The desired values reach the init container through a ConfigMap named for what
it is, `<cluster>-startup-config`, mounted by name. ADR-013 bans spec values
that reach the pod template and can change during a cluster's life; a ConfigMap
*volume* is structural, changes once, and rides the roll an operator upgrade
already causes when it re-defaults the data-plane image tags.

## Consequences

- A cluster's first generation of pods does not carry the attribute. The
  controller creates the data database and its overlays online, without it;
  one restart later the converger puts it in place. This follows from the
  context's fact 1 and cannot be designed away short of moving database
  creation into the init container.
- A pod restart can now change `cn=config`. The blast radius is exactly the
  declared table, which is the reason the table is declared rather than
  inferred.
- ADR-024 gains a fourth placement class: converged offline at pod start,
  change path a restart. It sits between R1 (converged per pod, live) and R2
  (bootstrap-time, change path a recreate), and it is where several R2
  tunables should eventually move.
- The guard in R2 is worth having on its own. Three separate attributes have
  killed or hung a live slapd in this project's history; the list is what
  stops the fourth.
- `slctl inspect` gains an honest question to answer: which pods run a
  configuration that differs from the declared one.

## Related

- ADR-004 — the SlapdDatabase controller creates data databases online, which
  is why the attribute cannot be set at creation.
- ADR-013 — no spec value that can change during a cluster's life reaches the
  pod template; the ConfigMap volume is structural, not a value.
- ADR-018 — the co-located Job pattern, rejected here for lack of a safe
  moment rather than for cost.
- ADR-019, ADR-026 — a configuration that validates while running and refuses
  to open at startup is this project's established way to lose a pod.
- ADR-022 — the session log the source replaces; the accesslog database still
  carries none of its own.
- ADR-023 — where an operator-driven rolling restart belongs.
- ADR-024 — the three placement classes this one extends.

---

## Amendment, 2026-10-14: the table's wire format

R1 says the table is declared; it did not say in what. The operator and the
converger are separate programs in separate images, so the format between them
is a contract and belongs in the record.

A ConfigMap `<cluster>-startup-config`, one key `startup-config`, mounted
read-only at `/etc/slaptain/startup-config` on the init container, whose path
also arrives as `STARTUP_CONFIG_PATH`. One record per line, three tab-separated
fields:

```
<attribute>\t<database-suffix>\t<value>
```

A tab separates them because a DN carries spaces freely and neither an
attribute name nor a DN can carry a tab. The database is named by its
`olcSuffix`, matched case-insensitively, never by a `cn=config` DN — an
`olcDatabase={N}` prefix is positional and assigned by slapd (ADR-026 R1).
**Which entry carries the attribute is a property of the attribute**, compiled
into the converger: `olcSpSessionlogSource` lives on the syncprov overlay child
of the named database. The file states what, where and with which value, and
nothing about the shape of `cn=config`.

Two states that look alike and are not, which is the whole reason this is
written down:

- **File absent** — unmanaged. The converger does nothing at all. This is what
  a pre-ADR-034 operator leaves behind, and what the `Optional: true` volume
  projects when the ConfigMap has not been written yet. Treating it as "nothing
  is wanted" would strip a working configuration on an operator upgrade.
- **File present and empty** — managed, nothing wanted. Every managed attribute
  is removed. This is how the last database to drop `Accesslog` gets the
  attribute off the disk.

So an operator that understands the table **always writes the key, even
empty**. Omitting it as a shortcut for "nothing to do" is the one rendering
that silently breaks removal.
