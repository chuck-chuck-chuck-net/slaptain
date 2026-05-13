# Legacy OpenLDAP source preparation

This document is the **pre-flight checklist for the source side** of a
slaptain migration. Before slaptain consumes from a legacy OpenLDAP server (or
cluster) the source must be set up to serve syncrepl. Most prod-grade OpenLDAP
deployments already are; this doc tells you how to verify, what to add if it
isn't, and what to avoid changing during the migration window.

Companion docs:
- `MIGRATION-PLAN.md §2` — phased plan, slaptain-side stages.
- `ADR-010` — slaptain replication modes (`peer` / `consumer-only`).
- `ADR-011` — hot-migration topology contract (ServerID coexistence,
  plain-syncrepl interop).
- `ONBOARDING.md §Replication model` — syncrepl protocol background.

## TL;DR — what slaptain needs from the source

| # | Item | Hard requirement? | Where verified |
|---|---|---|---|
| 1 | `syncprov` overlay on each replicated data DB | Yes | §1 |
| 2 | Dedicated bind DN with read access to the data tree, including `userPassword` and operational attributes | Yes | §2, §3 |
| 3 | Network reachability from slaptain's pod IP to source `ldap://`/`ldaps://` port | Yes | §4 |
| 4 | Plaintext bind password known and provisioned on slaptain | Yes | §2 |
| 5 | TLS CA cert for the source server (when using `ldaps://`) | Yes if TLS | §4 |
| 6 | Source ServerIDs known and recorded for slaptain's `serverIDBase` decision | Yes for multi-master sources | §5 |
| 7 | Schema parity between source and slaptain | Yes | §6 |
| 8 | `accesslog` overlay (only required if you want delta-syncrepl, not plain) | No — `syncMode: plain` skips this | §1 |

## 1. `syncprov` overlay on the data DB

`syncprov` is what makes a slapd database a syncrepl **provider** — it stamps
search results with the LDAP Sync State control and tracks the contextCSN.
Without it, slapd serves ordinary LDAP but consumers fail at the protocol
level (`do_syncrep2: rid=N got search entry without Sync State control`).

### Verify (commands run against the source)

```bash
ROOT_PW='<cn=admin,cn=config password>'
ldapsearch -x -H ldaps://source.example.com -D "cn=admin,cn=config" -w "$ROOT_PW" \
  -b "cn=config" "(&(objectClass=olcOverlayConfig)(olcOverlay=syncprov))" \
  dn olcOverlay
```

You should see one entry per replicated DB, e.g.:

```
dn: olcOverlay={0}syncprov,olcDatabase={1}mdb,cn=config
olcOverlay: {0}syncprov
```

If no entries are returned, `syncprov` is not configured. Add it (cn=config
edit, no restart needed):

```ldif
# add-syncprov.ldif
dn: cn=module{0},cn=config
changetype: modify
add: olcModuleLoad
olcModuleLoad: syncprov

dn: olcOverlay=syncprov,olcDatabase={1}mdb,cn=config
changetype: add
objectClass: olcOverlayConfig
objectClass: olcSyncProvConfig
olcOverlay: syncprov
```

```bash
ldapmodify -x -H ldaps://source.example.com -D "cn=admin,cn=config" -w "$ROOT_PW" \
  -f add-syncprov.ldif
```

Repeat for each replicated database. If `olcModuleLoad: syncprov` already
exists for the cluster, skip the first stanza. If `slapd` was compiled with
`syncprov` statically linked (some distros do this), the moduleload is a no-op.

### `olcSpNoPresent: TRUE` and `olcSpReloadHint: TRUE`

These are tuning parameters for the syncprov overlay. They're not strictly
required but are conventional for production. Add them if missing:

```ldif
dn: olcOverlay={N}syncprov,olcDatabase={...}mdb,cn=config
changetype: modify
add: olcSpNoPresent
olcSpNoPresent: TRUE
-
add: olcSpReloadHint
olcSpReloadHint: TRUE
```

(Replace `{N}` and `{...}` with the actual ordering prefixes from your tree.)

## 2. Replication bind user

slaptain authenticates to the source as a dedicated user when pulling
syncrepl. Convention: `cn=replication,<suffix>` or `cn=syncuser,<suffix>`,
but **any DN works** — slaptain's `ExternalPeer.bindDN` is a free string
(see ADR-011 §"Assumed source"; a common source convention is
`cn=syncuser,ou=config,...`).

### Verify

```bash
ldapsearch -x -H ldaps://source.example.com \
  -D "cn=admin,<suffix>" -w "$ADMIN_PW" \
  -b "<suffix>" "(uid=replication)" dn   # or whatever your DN convention is
```

If absent, add:

```ldif
# add-replication-user.ldif
dn: cn=replication,<suffix>
objectClass: simpleSecurityObject
objectClass: organizationalRole
cn: replication
description: Syncrepl bind account for slaptain migration
userPassword: <hashed via slappasswd -h {SSHA}>
```

### Password handling

slaptain needs the **plaintext** of this password — it's stored in a
Kubernetes Secret on slaptain's side and used at runtime to bind to the
source. Generate it locally, hash with `slappasswd` for the source-side
`userPassword`, keep the plaintext for slaptain's Secret:

```bash
PW=$(openssl rand -base64 24 | tr -d '=+/' | head -c 32)
echo "Plaintext for slaptain Secret: $PW"
echo -n "Hashed for source userPassword: "
slappasswd -s "$PW" -h '{SSHA}'
```

On slaptain's side (see `docs/ONBOARDING.md §Bringing your own credentials`):

```bash
kubectl create secret generic legacy-source-bind-pw \
  -n <slaptain-namespace> \
  --from-literal=password="$PW"
```

Then in the SlapdCluster CR:

```yaml
spec:
  replication:
    mode: consumer-only
    externalPeers:
      - name: legacy-source
        uri: ldaps://source.example.com:636
        syncMode: plain
        bindDN: cn=replication,<suffix>
        bindPasswordSecretName: legacy-source-bind-pw
        tlsSecretName: legacy-source-ca
```

## 3. ACLs allowing the bind user to read everything

Syncrepl pulls entries with **all attributes** including
operational (`entryUUID`, `entryCSN`, `creatorsName`, `modifiersName`,
`createTimestamp`, `modifyTimestamp`, `structuralObjectClass`) and the
sensitive `userPassword`. The bind user must be authorized to read all of
them; ACL gaps cause silent data loss (entries replicated without
`userPassword` mean users can't authenticate against slaptain).

### Verify

Bind as the replication user and try to read a user entry:

```bash
ldapsearch -x -H ldaps://source.example.com \
  -D "cn=replication,<suffix>" -w "$PW" \
  -b "<some-user-dn>" -s base "(objectClass=*)" '+' '*'
```

Expected: full attribute set including `userPassword` and the
`entryUUID`/`entryCSN`/`creators*`/`modifiers*` operational attributes. If
`userPassword` is missing or `(none)` is shown for the entry, ACLs are
blocking.

### Fix (cn=config)

Prepend an ACL granting the bind user full read on the data DB:

```ldif
dn: olcDatabase={1}mdb,cn=config
changetype: modify
add: olcAccess
olcAccess: {0}to *
  by dn.exact="cn=replication,<suffix>" read
  by * break
```

`by * break` means the ACL doesn't terminate evaluation — subsequent ACLs
still apply for other binds. Adjust the ordering prefix `{0}` to be the
lowest among existing ACLs.

## 4. Network reachability and TLS

slaptain's pods must reach the source on its LDAP port. Two flavours:

### Plain `ldap://` (port 389)

Simplest. Use over a private network or wireguard / IPsec tunnel.
`ExternalPeer.uri: ldap://10.x.y.z:389`. No `tlsSecretName` needed.

### `ldaps://` (port 636) — recommended for any non-private network

Slaptain validates the source's certificate. Provide the source's CA bundle
as a Kubernetes Secret:

```bash
kubectl create secret generic legacy-source-ca \
  -n <slaptain-namespace> \
  --from-file=ca.crt=/path/to/source-ca.crt
```

Reference it in the ExternalPeer:

```yaml
externalPeers:
  - name: legacy-source
    uri: ldaps://source.example.com:636
    tlsSecretName: legacy-source-ca
```

If the source uses an internal CA, that CA's cert is what slaptain needs —
not the server's cert itself. For public-CA certs (Let's Encrypt etc.),
slaptain skips the `TLSCACertificateFile` directive and falls back to the
OpenSSL system trust store (see `docs/ONBOARDING.md §TLS`).

### IP-based provider URIs

If the source has no DNS name or the cert SAN doesn't match the IP, slaptain
automatically adds `tls_reqcert=allow` to the syncrepl stanza for IP-based
URIs — strict CA validation continues but hostname verification is relaxed.

## 5. ServerID hygiene

OpenLDAP uses `olcServerID` to attribute CSNs to their originating server.
If slaptain and the source use overlapping ServerIDs, contextCSN tracking
collides and replication silently stops propagating — both sides think
they've already seen each other's writes. This bit us in e2e testing; see
the inline comment on `SlapdReplicationConfig.ServerIDBase` for the failure
mode.

### Verify the source's ServerIDs

```bash
ldapsearch -x -H ldaps://source.example.com \
  -D "cn=admin,cn=config" -w "$ROOT_PW" \
  -b "cn=config" -s base "(objectClass=*)" olcServerID
```

Sample output for a 4-node source cluster:

```
olcServerID: 101 ldap://node01.prod.example.com
olcServerID: 102 ldap://node02.prod.example.com
olcServerID: 103 ldap://node03.prod.example.com
olcServerID: 104 ldap://node04.prod.example.com
```

For a single-master source with no explicit `olcServerID`, slapd's default
internal value is `0`.

### Pick slaptain's `serverIDBase` to avoid collision

Set `spec.replication.serverIDBase` on the SlapdCluster CR so slaptain's
IDs (`serverIDBase + ordinal + 1`) don't overlap with the source's. Examples:

| Source IDs | Pick for slaptain | Resulting slaptain IDs (replicas=3) |
|---|---|---|
| none (default 0) | 100 | 101, 102, 103 |
| 101-104 (single site) | 500 | 501, 502, 503 |
| 101-104 + 201-204 (two sites, per ADR-011) | 500 | 501, 502, 503 |
| 1-3 (small homelab) | 100 | 101, 102, 103 |

Record the source's IDs somewhere stable (your cutover runbook, or a tagged
comment on the SlapdCluster manifest).

## 6. Schema parity

Syncrepl transmits entries verbatim; slaptain stores them in its own slapd's
data DB. If the source uses a schema that slaptain doesn't have loaded,
slaptain rejects the entries on initial refresh with
`SearchResultReference` errors or schema-validation failures.

### Verify what schemas the source uses

```bash
ldapsearch -x -H ldaps://source.example.com \
  -D "cn=admin,cn=config" -w "$ROOT_PW" \
  -b "cn=schema,cn=config" -s one "(objectClass=*)" cn
```

This lists every schema loaded into the source's slapd. The core/cosine/nis/
inetorgperson set is loaded by slaptain's init container; anything beyond
that needs a `SlapdSchema` CR on the slaptain side.

### Adding schemas to slaptain

Each non-core schema present on the source must have a corresponding
`SlapdSchema` CR applied to slaptain **before** the initial syncrepl
refresh — otherwise the refresh fails and you'll have to wipe slaptain's
data PVCs and start over.

See `operator/config/samples/ldap_v1alpha1_slapdschema.yaml` for the CR
shape, and `docs/MIGRATION-PLAN.md §1.1` for the schema-migration tooling.

## 7. Multi-master source clusters

When the source is a multi-master cluster (see ADR-011), slaptain consumes
from one or more nodes via separate
`externalPeers` entries — **not** by joining the mesh as a peer. Joining
the mesh is the *promotion* step, which happens later (`mode: peer`); during
consumer-only mode, slaptain is read-only and doesn't advertise as a
provider.

### Pick which source nodes slaptain pulls from

In principle slaptain can pull from any one node and let the source's own
internal mesh propagate writes between nodes. In practice you'll want
slaptain pulling from at least two source nodes for redundancy, so a single
source-node restart doesn't pause slaptain's sync.

Configure multiple `externalPeers` with `replicasPerPeer: 1` (default) so
each slaptain pod connects to a different source pod (diagonal-first
assignment — see CRD godoc on `ExternalPeer.replicasPerPeer`).

### Source-side reciprocal stanzas (only at promotion time)

While slaptain is in `mode: consumer-only`, the source needs **no** changes
to consume from slaptain — slaptain isn't a provider. Once slaptain is
promoted (`mode: peer`), the source needs reciprocal `olcSyncRepl` entries
pointing at slaptain pods + adding slaptain's ServerIDs to the source's
`olcServerID` list.

Slaptain surfaces a condition `ReplicationModePeerWiringRequired=True` when
this human handoff is needed; the message includes the required source-side
configuration.

## 8. What to NOT do during the migration window

These changes on the source side can break replication mid-sync. Avoid
during the consumer-only window:

- **Schema additions** — adding a new attribute or objectClass while
  slaptain is sync'd causes new entries using that schema to be rejected on
  slaptain. Add the schema on slaptain first, then on the source.
- **ACL tightening on the replication bind user** — if the bind user loses
  read on some attribute or DN, slaptain silently stops receiving those
  entries. Verify ACLs are stable.
- **Restart of `syncprov` configuration** — adding/modifying the syncprov
  overlay may cause connected consumers to do a full refresh. Slaptain
  handles this fine but it's traffic-heavy.
- **CSN-history-affecting operations** — running `slapcat` and re-feeding
  the output via `slapadd` rewrites CSNs and confuses consumers. Don't do
  this on the source while slaptain is consuming.
- **Changing source ServerIDs** — would invalidate slaptain's contextCSN
  tracking and force a full refresh. If you must, do it before slaptain is
  deployed.

Safe operations during the window:
- Adding, modifying, or deleting **user entries** in the data tree.
- Adding new schemas, **if** they're added to slaptain first.
- Adding new database overlays that don't affect syncrepl (e.g., `memberof`,
  password policy).
- TLS cert rotation, as long as the same CA continues to validate.

## 9. Pre-flight script

Putting the verification steps together:

```bash
#!/bin/bash
# Pre-flight check: legacy OpenLDAP → slaptain migration source readiness.
set -e

SOURCE_URI="${SOURCE_URI:-ldaps://source.example.com:636}"
ADMIN_DN="${ADMIN_DN:-cn=admin,cn=config}"
DATA_DN="${DATA_DN:-cn=admin,dc=example,dc=org}"
ROOT_PW="${ROOT_PW:?set ROOT_PW for cn=admin,cn=config}"
DATA_PW="${DATA_PW:?set DATA_PW for the data admin}"
REPL_DN="${REPL_DN:-cn=replication,dc=example,dc=org}"
REPL_PW="${REPL_PW:?set REPL_PW for the replication bind user}"
SAMPLE_USER_DN="${SAMPLE_USER_DN:?set SAMPLE_USER_DN to any real user DN}"

echo "==> 1. syncprov overlay present on at least one DB"
ldapsearch -x -H "$SOURCE_URI" -D "$ADMIN_DN" -w "$ROOT_PW" \
  -b "cn=config" "(&(objectClass=olcOverlayConfig)(olcOverlay=syncprov))" dn \
  | grep -q '^dn:' && echo "   OK" || { echo "   FAIL"; exit 1; }

echo "==> 2. replication bind user exists"
ldapsearch -x -H "$SOURCE_URI" -D "$DATA_DN" -w "$DATA_PW" \
  -b "$REPL_DN" -s base "(objectClass=*)" dn \
  | grep -q '^dn:' && echo "   OK" || { echo "   FAIL"; exit 1; }

echo "==> 3. replication bind user can bind"
ldapsearch -x -H "$SOURCE_URI" -D "$REPL_DN" -w "$REPL_PW" \
  -b "" -s base "(objectClass=*)" namingContexts > /dev/null \
  && echo "   OK" || { echo "   FAIL"; exit 1; }

echo "==> 4. replication bind user can read userPassword + operational attrs"
result=$(ldapsearch -x -H "$SOURCE_URI" -D "$REPL_DN" -w "$REPL_PW" \
  -b "$SAMPLE_USER_DN" -s base "(objectClass=*)" '+' '*')
echo "$result" | grep -q '^userPassword' && echo "   userPassword: OK" \
  || { echo "   userPassword: FAIL — ACL blocks read"; exit 1; }
echo "$result" | grep -q '^entryUUID' && echo "   entryUUID: OK" \
  || { echo "   entryUUID: FAIL — operational attrs not visible"; exit 1; }

echo "==> 5. source ServerIDs"
ldapsearch -x -H "$SOURCE_URI" -D "$ADMIN_DN" -w "$ROOT_PW" \
  -b "cn=config" -s base "(objectClass=*)" olcServerID

echo ""
echo "Source is ready. Note the olcServerID values above when picking"
echo "spec.replication.serverIDBase on the SlapdCluster CR."
```

Save as `scripts/check-source-ready.sh`, set the env vars per your source,
and run. Green checks across all items mean slaptain can be deployed
pointing at this source.

## 10. Common errors and what they mean

| Symptom on slaptain | Likely cause |
|---|---|
| `do_syncrep2: got search entry without Sync State control` | `syncprov` overlay missing on source — see §1 |
| `ldap_sasl_bind_s failed (49)` | Wrong bind password, or replication user doesn't exist on source — see §2 |
| `ldap_sasl_bind_s failed (8)` | TLS handshake failure — CA cert mismatch, expired cert, or hostname mismatch — see §4 |
| Replication runs but `userPassword` missing from synced entries | ACL on source blocks read of `userPassword` for the replication user — see §3 |
| Initial refresh completes but new writes on source don't propagate | ServerID collision between source and slaptain — see §5 |
| Refresh fails with schema validation errors | Schemas present on source not loaded on slaptain — see §6 |
| `Operations error (1)` on initial refresh | Often an ACL or schema issue; check source slapd logs (`-d 256 -d 32` for ACL + filter trace) |

## 11. Decommissioning the source after cutover

This doc focuses on **preparing** the source as a syncrepl provider. The
inverse — what to clean up on the source after cutover completes — is the
final stage of the hot-migration runbook: see
[`MIGRATION-HOT.md` §"Stage 5 — slaptain alone + source decommissioning"](MIGRATION-HOT.md#stage-5--slaptain-alone--source-decommissioning).

The short version: once slaptain is the authoritative source, remove the
replication-bind ACL grant and either remove the replication user, or keep
it inert. Drop the source's `olcServerID` list. Stop the source slapd. Keep
a final `slapcat` archive of each DB before stopping it.
