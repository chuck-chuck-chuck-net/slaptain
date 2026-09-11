# OpenLDAP versions and image tags

slaptain ships two slapd image pairs. The plain tag is **OpenLDAP 2.7.1**; the
`-ol26` tag is the previous **OpenLDAP 2.6** build from Debian's package. The
decision and its reasoning are in
[ADR-021](adrs/adr-021-openldap-2.7-dual-images.md).

## Which image is which

| Image tag | OpenLDAP | Use it for |
|---|---|---|
| `<registry>/slaptain/slapd:<tag>` + `slapd-init:<tag>` | 2.7.1 (`2.7.1-0+slaptain1`) | everything — this is the default |
| `<registry>/slaptain/slapd:<tag>-ol26` + `slapd-init:<tag>-ol26` | 2.6.x (Debian trixie) | legacy interop and hot-migration clusters that must match a 2.6 source (ADR-011) |

`<tag>` is the same release tag for both pairs — `-ol26` is only a suffix, never
a separate version line. `slapd-toolkit` and `operator` have one tag each and
carry no suffix.

2.7 is the default because OpenLDAP 2.6 is affected by upstream
[ITS#9580](https://bugs.openldap.org/show_bug.cgi?id=9580): a pod that loses its
volumes and resyncs from peers drives the whole mesh into a
`sync cookie is stale` refresh loop for minutes. The fix is released only in
2.7.0/2.7.1, and no distribution packages 2.7 as of 2026-09-11 — hence the 2.7
images come from a vendored Debian packaging fork
([`images/openldap-deb/`](../images/openldap-deb/README.md)).

## Pinning the 2.6 pair

Set both image tags on the `SlapdCluster` — **both, or neither**. The init
container writes the config and data volumes that slapd then opens, and 2.6 and
2.7 do not share an on-disk format.

```yaml
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdCluster
metadata:
  name: slapd
spec:
  images:
    slapd:
      tag: <tag>-ol26
    init:
      tag: <tag>-ol26
```

With the `slapd-cluster` chart:

```bash
helm upgrade --install slapd ./charts/slapd-cluster \
  --set images.slapd.tag=<tag>-ol26 \
  --set images.init.tag=<tag>-ol26
```

Local build and delivery targets:

```bash
make build-ol26          # both -ol26 images
make import-ol26         # import them into the nodes' CRI
make push                # pushes all six images (both pairs, toolkit, operator)
```

To run the e2e suite against the 2.6 pair (the operator image stays on the plain
tag):

```bash
SLAPD_TAG_SUFFIX=-ol26 ./tests/e2e.sh all <context>
```

Expect `dataloss_recovery_test`'s ITS#9580 spec to **fail** there: that failure
is the behaviour the version bump fixes.

## Migrating an existing 2.6 cluster to 2.7

> **Read this first.** 2.7 uses LMDB 1.0, whose on-disk format 2.6 slapd cannot
> read, and vice versa. Swapping the image tags on a running cluster is **not an
> upgrade**: slapd fails to open `/data` and `/accesslog`, and the pods
> crashloop. The migration is a dump and reload — back up on 2.6, switch the
> images, wipe the volumes, restore.
>
> Plan a window. The cluster is unavailable from step 3 until step 6 completes.

The runbook uses the backup/restore machinery from
[ADR-014](adrs/adr-014-s3-backup-restore.md) — see [`BACKUP.md`](BACKUP.md) for
the S3 credentials Secret and the `SlapdBackup` fields.

### 1. Back up, still on 2.6

```bash
kubectl apply -n <namespace> -f - <<'EOF'
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdBackup
metadata:
  name: pre-ol27
spec:
  databaseRef: <slapddatabase-name>
  storage:
    bucket: slaptain-backups
    endpoint: ""                       # empty for AWS S3; FQDN for an in-cluster store
    region: <region>
    credentialsSecretName: <s3-credentials-secret>
EOF

kubectl wait -n <namespace> slapdbackup/pre-ol27 \
  --for=jsonpath='{.status.phase}'=Completed --timeout=30m
```

Do not proceed until the phase is `Completed`. Note the object key from
`status`:

```bash
kubectl get -n <namespace> slapdbackup/pre-ol27 -o yaml
```

Take one backup per `SlapdDatabase` in the cluster.

### 2. Record the current state

```bash
kubectl get -n <namespace> slapdcluster/<cluster> \
  -o jsonpath='{.spec.images}{"\n"}{.spec.replicas}{"\t"}{.spec.readReplicas}{"\n"}'
kubectl get pvc -n <namespace>
```

Keep this. It is your rollback target: re-pinning `<tag>-ol26` and restoring the
same backup puts you back where you started.

### 3. Pin the 2.7 images

```bash
kubectl patch -n <namespace> slapdcluster/<cluster> --type merge -p \
  '{"spec":{"images":{"slapd":{"tag":"<tag>"},"init":{"tag":"<tag>"}}}}'
```

> **The crashloop window starts here.** The StatefulSet rolls each pod onto 2.7
> slapd, which cannot open the 2.6-format `/data` and `/accesslog` it finds, so
> the pods enter `CrashLoopBackOff` and stay there until step 4 gives them empty
> volumes. This is expected and is why the images are pinned *before* the
> volumes are wiped: doing it the other way round would have fresh volumes
> written by 2.6 and the break would simply move.
>
> `spec.replicas` cannot be set to 0 (the CRD requires ≥ 1), so there is no
> "scale down, swap, scale up" variant — crashlooping pods are how the cluster
> waits.

### 4. Wipe the volumes, per pod

PVCs from `volumeClaimTemplates` carry no labels, so delete them by name. A PVC
in use only gets a `deletionTimestamp` (the `kubernetes.io/pvc-protection`
finalizer); it is garbage-collected once no pod object references it, which
deleting the pod triggers. Any *other* pod naming the PVC — a finished backup
Job's pod included — blocks it too (ADR-018).

For each RW ordinal `i` in `0 … <replicas>-1`:

```bash
kubectl delete pvc -n <namespace> \
  config-<cluster>-$i data-<cluster>-$i accesslog-<cluster>-$i --wait=false
kubectl delete pod -n <namespace> <cluster>-$i
```

And for each read-only ordinal `j` (if `spec.readReplicas > 0` — these have no
accesslog volume):

```bash
kubectl delete pvc -n <namespace> \
  config-<cluster>-readonly-$j data-<cluster>-readonly-$j --wait=false
kubectl delete pod -n <namespace> <cluster>-readonly-$j
```

Confirm nothing is stuck before moving on — a PVC still in `Terminating` will
block its pod's volume mount:

```bash
kubectl get pvc -n <namespace>
kubectl get pods -n <namespace> -l app.kubernetes.io/instance=<cluster>
```

Fresh volumes are provisioned from the StatefulSet's `volumeClaimTemplates`, the
init container bootstraps `cn=config` on 2.7, and the operator recreates the
databases. The DIT comes back **empty**: the seed is a one-shot latch and does
not re-run (ADR-012). Step 6 is what puts the data back.

### 5. Confirm the pods are healthy and running 2.7

```bash
kubectl get pods -n <namespace> -l app.kubernetes.io/instance=<cluster> \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.status.phase}{"\t"}{range .spec.containers[?(@.name=="slapd")]}{.image}{end}{"\n"}{end}'

slctl status -n <namespace> <cluster>
slctl inspect -n <namespace> <cluster>
```

Every pod must be `Running` on the plain-tag image, with no `-ol26` left.

### 6. Restore the data

```bash
kubectl apply -n <namespace> -f - <<'EOF'
apiVersion: ldap.chuck-chuck-chuck.net/v1alpha1
kind: SlapdRestore
metadata:
  name: from-ol26
spec:
  databaseRef: <slapddatabase-name>
  source:
    backupRef: pre-ol27
EOF

kubectl wait -n <namespace> slapdrestore/from-ol26 \
  --for=jsonpath='{.status.phase}'=Completed --timeout=60m
```

The restore scales the cluster to 0, runs an offline `slapadd` on every pod and
scales back up — expect the cluster to be unavailable for its duration. One
restore at a time per cluster; repeat per database.

### 7. Verify

```bash
slctl inspect -n <namespace> <cluster>      # CSN convergence, topology, stanza counts
slctl ldapsearch -n <namespace> -b '<baseDN>' -s sub '(objectClass=*)' dn
```

Compare the entry count against what you had before step 3.

### Multi-site meshes

Migrate one site at a time. A 2.6 site and a 2.7 site replicate with each other
normally — syncrepl is unchanged across the versions — but note that the sites
still on 2.6 remain exposed to ITS#9580 until they are migrated. (A mixed mesh
is wire-compatible by construction; we have not run a cross-site validation of
one — see ADR-021.)

### The alternative: wipe one pod and resync

You can also migrate a replicated cluster pod by pod: delete one pod's PVCs, let
it come back on the 2.7 images and refresh from its peers. It works, and the
refreshing pod runs the *fixed* code — but the peers serving that refresh are
still 2.6, so every such pod costs one ITS#9580 storm. Prefer backup/restore for
anything but a lab.

## Rollback to 2.6

Same procedure in reverse: back up on 2.7, delete the PVCs, pin `<tag>-ol26` on
both images, restore. There is no shortcut — the format break is symmetric.
