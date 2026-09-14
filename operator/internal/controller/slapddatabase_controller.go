/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package controller

import (
	"context"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kerrors "k8s.io/apimachinery/pkg/util/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
	"github.com/chuck-chuck-chuck-net/slaptain/operator/internal/suffixprobe"
)

const (
	databaseFieldManager = "slapddatabase-controller"
	databaseFinalizer    = "ldap.chuck-chuck-chuck.net/database-cleanup"
)

// SlapdDatabaseReconciler reconciles a SlapdDatabase object.
// It manages per-database lifecycle: creating the MDB backend in cn=config,
// applying ACLs, indices, replication stanzas, and seeding initial data.
// See ADR-004 and ADR-005.
type SlapdDatabaseReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// ClusterDomain is the Kubernetes DNS domain used to build pod FQDNs for
	// per-pod LDAP connections and syncrepl provider URIs. See ADR-015.
	ClusterDomain string
}

// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapddatabases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapddatabases/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapddatabases/finalizers,verbs=update
// +kubebuilder:rbac:groups=ldap.chuck-chuck-chuck.net,resources=slapdclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

func (r *SlapdDatabaseReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	// 1. Fetch the SlapdDatabase resource.
	sd := &ldapv1alpha1.SlapdDatabase{}
	if err := r.Get(ctx, req.NamespacedName, sd); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 2. Handle deletion (finalizer).
	if !sd.DeletionTimestamp.IsZero() {
		return r.handleDeletion(ctx, sd)
	}

	// Ensure finalizer is present.
	if !controllerutil.ContainsFinalizer(sd, databaseFinalizer) {
		controllerutil.AddFinalizer(sd, databaseFinalizer)
		if err := r.Update(ctx, sd); err != nil {
			return ctrl.Result{}, err
		}
	}

	// 3. Fetch the referenced SlapdCluster.
	sc := &ldapv1alpha1.SlapdCluster{}
	if err := r.Get(ctx, client.ObjectKey{
		Name:      sd.Spec.ClusterRef,
		Namespace: sd.Namespace,
	}, sc); err != nil {
		if errors.IsNotFound(err) {
			log.Info("referenced SlapdCluster not found", "clusterRef", sd.Spec.ClusterRef)
			r.setStatus(ctx, sd, ldapv1alpha1.DatabasePhasePending, nil, nil,
				"ClusterNotFound", fmt.Sprintf("SlapdCluster %q not found", sd.Spec.ClusterRef))
			return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
		}
		return ctrl.Result{}, err
	}

	// 3a. Honor spec.suspend on either CR. Parent SlapdCluster being suspended
	// implies its databases should also pause — manual interventions that
	// suspend the cluster will typically span cn=config edits this controller
	// would otherwise revert.
	if sd.Spec.Suspend || sc.Spec.Suspend {
		log.Info("reconciliation suspended", "sdSuspend", sd.Spec.Suspend, "scSuspend", sc.Spec.Suspend)
		return ctrl.Result{}, nil
	}

	// 4. Wait for cluster to be Running.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		log.Info("waiting for cluster to be Running", "phase", sc.Status.Phase)
		r.setStatus(ctx, sd, ldapv1alpha1.DatabasePhasePending, nil, nil,
			"ClusterNotReady", fmt.Sprintf("SlapdCluster %q is %s", sd.Spec.ClusterRef, sc.Status.Phase))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// 4a. Refuse a durability posture that loses writes without saying so:
	// noSync with the checkpoint explicitly disabled. ADR-024 R4 in its
	// "rejected" form — the alternative is writing it and letting the operator
	// discover the trade during an incident. Checked before anything touches a
	// pod, so the CR reports Error without a half-applied configuration.
	if err := validateDurability(sd, sc); err != nil {
		log.Info("rejecting durability configuration", "err", err)
		r.setStatus(ctx, sd, ldapv1alpha1.DatabasePhaseError, nil, nil,
			"UnsafeDurability", err.Error())
		return ctrl.Result{}, nil
	}

	// 5. Reconcile credentials secret.
	if err := r.reconcileCredentials(ctx, sd, sc); err != nil {
		r.setStatus(ctx, sd, ldapv1alpha1.DatabasePhaseError, nil, nil,
			"CredentialsInvalid", err.Error())
		return ctrl.Result{}, fmt.Errorf("reconcileCredentials: %w", err)
	}

	// 6. Read passwords.
	configPW, err := r.getClusterConfigPassword(ctx, sc)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getClusterConfigPassword: %w", err)
	}

	rootPW, err := r.getDatabaseRootPassword(ctx, sd)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("getDatabaseRootPassword: %w", err)
	}

	// 7. Apply database to every pod. cn=config is node-local (ADR-002).
	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"

	var appliedPods, failedPods []string

	// Pods whose per-pod database reconcile completed. reconcileReplication
	// writes syncrepl stanzas only to these — see the comment on its healthyPods
	// parameter.
	healthyPods := map[string]bool{}

	// Start each pass assuming every operator-managed tunable matches; the
	// per-pod work flips this to False when it finds one it cannot apply to a
	// live database (today: the map size). Set here rather than at the end so
	// a divergence that has been repaired — by recreating the database —
	// clears itself without a special case (ADR-001 idempotency).
	setCondition(&sd.Status.Conditions, metav1.Condition{
		Type:               tunablesConvergedCondition,
		Status:             metav1.ConditionTrue,
		Reason:             "Converged",
		Message:            "every operator-managed tunable matches cn=config on every pod",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	})

	// RW pods.
	for i := int32(0); i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", sc.Name, i)
		host := fmt.Sprintf("%s.%s.%s.svc.%s",
			podName, headlessSvc, sc.Namespace, r.ClusterDomain)
		if err := r.reconcilePodDatabase(ctx, host, configPW, rootPW, sd, sc, false, i); err != nil {
			log.Info("database reconcile skipped for pod (will retry)",
				"pod", podName, "err", err)
			failedPods = append(failedPods, podName)
		} else {
			appliedPods = append(appliedPods, podName)
			healthyPods[podName] = true
		}
	}

	// RO pods.
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			podName := fmt.Sprintf("%s-readonly-%d", sc.Name, i)
			host := fmt.Sprintf("%s.%s.%s.svc.%s",
				podName, roHeadless, sc.Namespace, r.ClusterDomain)
			// ordinal is unused for RO pods (serverID is RW-only), but the
			// signature requires it.
			if err := r.reconcilePodDatabase(ctx, host, configPW, rootPW, sd, sc, true, i); err != nil {
				log.Info("database reconcile skipped for read-only pod (will retry)",
					"pod", podName, "err", err)
				failedPods = append(failedPods, podName)
			} else {
				appliedPods = append(appliedPods, podName)
				healthyPods[podName] = true
			}
		}
	}

	// Track whether any step was incomplete — forces requeue even if database
	// creation succeeded on all pods. Without this, the operator would stop
	// reconciling before seed/replication are applied (see reconcile-loop-fixes.md:
	// "Operator stops reconciling before all pods are configured").
	pendingWork := len(failedPods) > 0

	// 8. Seed initial data. ONE-SHOT per cluster lifetime — once SeedApplied=true,
	//    we never re-evaluate. Seed data is the user's initial-conditions sketch,
	//    not operator-owned declarative state, so re-applying on a "missing data"
	//    signal would be wrong: it would mask real data loss (PVC reset, single-pod
	//    cluster) with a fake "recovery" to a tiny subset of what should be there.
	//    For multi-pod clusters that genuinely lose a pod's data, syncrepl handles
	//    recovery from peers — no operator action needed. For total data loss
	//    (single-pod or all peers lost), the right answer is restore-from-backup
	//    or explicit CR-and-PVC delete, not silent re-seed. See ADR-012.
	//    ADR-025 belt (withhold-create only): when pod-0's suffix entry was
	//    already created by ANOTHER cluster in the mesh (a foreign serverID —
	//    the founder site's seed arrived via replication first), the local seed
	//    is withheld wholesale and SeedApplied latches: seeding the same DNs
	//    with fresh entryUUIDs is the multi-site seed race that manufactures a
	//    permanent glue suffix. This is the OPPOSITE direction of the reverted
	//    verifySeedExists (which re-created on absence): we only ever DECLINE
	//    to create on positive evidence of a foreign creator, never re-apply.
	if seedNeeded(sd) {
		withheld, err := r.applySeedData(ctx, sc, sd, rootPW)
		switch {
		case err != nil:
			log.Info("seed data not yet applied (will retry)", "err", err)
			pendingWork = true
		case withheld:
			log.Info("seed withheld: suffix entry already created by a foreign serverID "+
				"(founder site's seed replicated in first — ADR-025); latching SeedApplied",
				"suffix", sd.Spec.Suffix)
			sd.Status.SeedApplied = true
		default:
			sd.Status.SeedApplied = true
		}
	}

	// 9. Create replication bind user if replication is enabled.
	//    The user cn=replication,<suffix> must exist in the data tree before
	//    syncrepl stanzas can authenticate. Created idempotently on pod-0.
	//
	//    Skipped in consumer-only mode (ADR-010): the cn=replication user is
	//    the bind identity for external consumers authenticating *to* this
	//    cluster, but consumer-only clusters don't advertise as a provider —
	//    nobody binds, so the user isn't needed. Additionally the data DB
	//    has olcReadOnly=TRUE in consumer-only, so the add would be rejected
	//    with unwillingToPerform anyway. On in-place promotion to peer mode,
	//    the next reconcile flips olcReadOnly (step 7) before this step
	//    fires, and the bind user gets created cleanly.
	//    Deferred for a not-yet-restored bootstrapFrom database (ADR-014): its
	//    suffix base entry doesn't exist until the restore loads it, so adding
	//    cn=replication,<suffix> fails with "No Such Object" and wedges the DB in
	//    Degraded — which blocks the restore that is gated on the DB reaching
	//    Running (a deadlock). The backup itself contains cn=replication, so the
	//    post-restore add (restoreApplied=true) is idempotent.
	restorePending := sd.Spec.BootstrapFrom != nil && !sd.Status.RestoreApplied
	if sc.Spec.Replication.Enabled && sd.ReplicationEnabled() && !sc.IsConsumerOnly() && !restorePending {
		if err := r.ensureReplicationUser(ctx, sc, sd, rootPW); err != nil {
			log.Info("replication user not yet created (will retry)", "err", err)
			pendingWork = true
		}
	}

	// 10. Configure replication stanzas if replication is enabled.
	if sc.Spec.Replication.Enabled && sd.ReplicationEnabled() {
		skipped, err := r.reconcileReplication(ctx, sc, sd, configPW, healthyPods)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcileReplication: %w", err)
		}
		if skipped {
			pendingWork = true
		}
	}

	// 10. Evaluate DataPresent (pure observability — never drives reconciler
	//     behaviour, see ADR-012). Set before setStatus so the condition
	//     rides along in the same status patch.
	//
	//     DataObserved latches here: once the suffix's root entry has been seen
	//     on a pod, this database is known to have held data, and a later
	//     all-empty reading is a data-loss alert rather than a database still
	//     waiting for its first replication. One-way — nothing clears it, and
	//     nothing on the write path reads it (see seedNeeded).
	dataPresent, observed := r.evaluateDataPresent(ctx, sc, sd, rootPW)
	setCondition(&sd.Status.Conditions, dataPresent)
	if observed {
		sd.Status.DataObserved = true
	}

	// 11. Update status.
	phase := ldapv1alpha1.DatabasePhaseRunning
	reason := "Applied"
	msg := fmt.Sprintf("Database applied to %d pods", len(appliedPods))
	if pendingWork {
		phase = ldapv1alpha1.DatabasePhaseDegraded
		reason = "PartiallyApplied"
		msg = fmt.Sprintf("Database applied to %d pods, %d failed or pending",
			len(appliedPods), len(failedPods))
	}
	if len(appliedPods) == 0 {
		phase = ldapv1alpha1.DatabasePhaseError
		reason = "NoPodsReachable"
		msg = "Could not apply database to any pod"
	}
	r.setStatus(ctx, sd, phase, appliedPods, failedPods, reason, msg)

	if phase != ldapv1alpha1.DatabasePhaseRunning {
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}
	return ctrl.Result{}, nil
}

// ── Credentials ──────────────────────────────────────────────────────────────

// reconcileCredentials creates the per-database credentials secret if needed,
// or validates a user-provided Secret against the cluster's replication needs.
func (r *SlapdDatabaseReconciler) reconcileCredentials(
	ctx context.Context,
	sd *ldapv1alpha1.SlapdDatabase,
	sc *ldapv1alpha1.SlapdCluster,
) error {
	secretName := r.credentialsSecretName(sd)

	// User-provided: validate required keys are present. The operator never
	// patches missing keys into a BYO Secret (it would change the semantics of
	// "bring your own"), so a missing replication-password silently produces an
	// empty bind credential and a broken replication topology. Fail fast here
	// instead.
	if sd.Spec.Credentials.SecretName != "" {
		existing := &corev1.Secret{}
		if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: sd.Namespace}, existing); err != nil {
			return fmt.Errorf("read user-provided credentials Secret %s: %w", secretName, err)
		}
		if len(existing.Data["root-password"]) == 0 {
			return fmt.Errorf("user-provided credentials Secret %s is missing required key \"root-password\"", secretName)
		}
		if sc.NeedsAccesslog() && len(existing.Data["replication-password"]) == 0 {
			return fmt.Errorf(
				"user-provided credentials Secret %s is missing key \"replication-password\" "+
					"but the cluster needs it (replication.enabled=true with "+
					"replicas>1 or externalPeers); add the key (plaintext) before the operator can configure syncrepl",
				secretName)
		}
		return nil
	}

	// Check if auto-generated secret already exists.
	existing := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: sd.Namespace}, existing); err == nil {
		return nil
	} else if !errors.IsNotFound(err) {
		return err
	}

	rootPW, err := generatePassword(24)
	if err != nil {
		return fmt.Errorf("generate database root password: %w", err)
	}

	replPW, err := generatePassword(32)
	if err != nil {
		return fmt.Errorf("generate replication password: %w", err)
	}

	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: secretName, Namespace: sd.Namespace},
		Type:       corev1.SecretTypeOpaque,
		StringData: map[string]string{
			"root-password":        rootPW,
			"replication-password": replPW,
		},
	}
	// No SetControllerReference — the secret should survive CR deletion
	// when cleanupPolicy=Retain.
	return r.Create(ctx, secret)
}

func (r *SlapdDatabaseReconciler) credentialsSecretName(sd *ldapv1alpha1.SlapdDatabase) string {
	if sd.Spec.Credentials.SecretName != "" {
		return sd.Spec.Credentials.SecretName
	}
	return sd.Name + "-credentials"
}

func (r *SlapdDatabaseReconciler) getDatabaseRootPassword(ctx context.Context, sd *ldapv1alpha1.SlapdDatabase) (string, error) {
	secretName := r.credentialsSecretName(sd)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: sd.Namespace}, secret); err != nil {
		return "", fmt.Errorf("read database credentials secret %s: %w", secretName, err)
	}
	pw := string(secret.Data["root-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s is missing root-password key", secretName)
	}
	return pw, nil
}

func (r *SlapdDatabaseReconciler) getDatabaseReplPassword(ctx context.Context, sd *ldapv1alpha1.SlapdDatabase) (string, error) {
	secretName := r.credentialsSecretName(sd)
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: sd.Namespace}, secret); err != nil {
		return "", fmt.Errorf("read database credentials secret %s: %w", secretName, err)
	}
	return string(secret.Data["replication-password"]), nil
}

func (r *SlapdDatabaseReconciler) getClusterConfigPassword(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (string, error) {
	secretName := sc.Name + "-config-password"
	if sc.Spec.LDAP.CnConfigCredentials.SecretName != "" {
		secretName = sc.Spec.LDAP.CnConfigCredentials.SecretName
	}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, client.ObjectKey{Name: secretName, Namespace: sc.Namespace}, secret); err != nil {
		return "", fmt.Errorf("read config password secret %s: %w", secretName, err)
	}
	pw := string(secret.Data["root-password"])
	if pw == "" {
		return "", fmt.Errorf("secret %s is missing root-password key", secretName)
	}
	return pw, nil
}

// ── Per-Pod Database Reconciliation ──────────────────────────────────────────

// reconcilePodDatabase ensures the database exists on one pod and its ACLs/indices
// are up to date.
func (r *SlapdDatabaseReconciler) reconcilePodDatabase(
	ctx context.Context,
	host, configPW, rootPW string,
	sd *ldapv1alpha1.SlapdDatabase,
	sc *ldapv1alpha1.SlapdCluster,
	readOnly bool,
	ordinal int32,
) error {
	log := logf.FromContext(ctx)

	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", configPW); err != nil {
		return fmt.Errorf("bind cn=admin,cn=config at %s: %w", host, err)
	}

	// Check if database already exists.
	//
	// dataDN is this database's olcDatabase={N}mdb DN, and {N} is POSITIONAL:
	// slapd renumbers every database ordered after a deleted one, so any
	// cn=config database delete can invalidate a DN resolved before it. Every
	// step below that deletes a database therefore reports it, and dataDN is
	// re-resolved on the spot — see the two `if deleted` blocks. Do not add a
	// delete without one; and do not try to reason about which DNs a renumber
	// touches (only those ordered after the deletion, in principle) — one extra
	// search on a path that only fires when something was deleted is cheaper
	// than that arithmetic being subtly wrong.
	//
	// The bug this rule exists to prevent, observed live: the pre-ADR-019
	// cluster-shared log was created on the FIRST replicated database's
	// reconcile, so a database added later sits at a HIGHER index than the log.
	// Deleting the log during the ADR-019 R8 migration slid that database's
	// cached DN down onto the first database's accesslog DB, and
	// ensureAccesslogOverlay then made one journal journal into the other —
	// ADR-019 Fact 2, permanently, on every pod. ensureReadOnly would likewise
	// have set olcReadOnly on an accesslog database in consumer-only mode.
	dataDN, err := findDataDBDN(conn, sd.Spec.Suffix)
	if err != nil {
		// Database doesn't exist yet — create it.
		log.Info("creating database", "host", host, "suffix", sd.Spec.Suffix,
			"consumerOnly", sc.IsConsumerOnly())
		dataDN, err = r.createDatabase(conn, sd, sc, rootPW)
		if err != nil {
			return fmt.Errorf("create database at %s: %w", host, err)
		}
	}

	// Apply ACLs. When spec.acls is empty/omitted, we skip applyACLs entirely
	// so slapd's built-in default ("to * by * read", rootdn bypasses) takes
	// effect — see SlapdDatabase.spec.acls godoc and docs/ONBOARDING.md §ACLs.
	//
	// TODO: latent issue. applyACLs also prepends the cn=replication,<suffix>
	// read-all rule when replication is enabled (see line 462). Skipping it
	// here means that rule is not written when acls is empty. Currently
	// harmless because slapd's default already permits cn=replication to read
	// everything (along with everyone else), so replication works. If a future
	// change tightens the empty-acls default — or if someone adds a webhook
	// that injects a baseline rule — the replication ACL must move out of
	// applyACLs into a path that always runs when replication is enabled.
	if len(sd.Spec.ACLs) > 0 {
		if err := r.applyACLs(ctx, conn, host, dataDN, sd, sc); err != nil {
			return fmt.Errorf("apply ACLs at %s: %w", host, err)
		}
	}

	// Apply indices.
	if len(sd.Spec.Indices) > 0 {
		if err := r.applyIndices(ctx, conn, host, dataDN, sd.Spec.Indices); err != nil {
			return fmt.Errorf("apply indices at %s: %w", host, err)
		}
	}

	// ── Scale tunables, converged per pod on every reconcile (ADR-024) ───────
	//
	// All four run unconditionally, unlike the two blocks above: they carry
	// operator opinions and operator-owned contract, so none of them may ride
	// on a user field being non-empty. Between them they are the difference
	// between a cluster that works at fixture size and one that works at
	// production size — and every one of them is invisible below ~500 entries.

	// Baseline indices (ADR-024 R7): entryCSN + entryUUID are searched by
	// syncrepl itself; unindexed they are a full scan on a replication hot path.
	if err := r.ensureDataBaselineIndices(ctx, conn, host, dataDN); err != nil {
		return fmt.Errorf("ensure baseline indices at %s: %w", host, err)
	}

	// Map size (ADR-024 R4): set at creation, REPORTED (never written) on an
	// existing database — modifying olcDbMaxSize under a running slapd
	// segfaults it. See compareMaxSize.
	maxSize, err := desiredDataMaxSize(sd)
	if err != nil {
		return fmt.Errorf("spec.maxSize: %w", err)
	}
	if err := r.checkMaxSize(ctx, conn, host, dataDN, "data database", maxSize, sd); err != nil {
		return err
	}

	// Search limits (ADR-024 R5): unlimited by default, so a client
	// enumerating a real subtree is not silently handed the first 500 entries.
	if err := r.ensureSearchLimits(ctx, conn, host, dataDN, sd); err != nil {
		return fmt.Errorf("ensure search limits at %s: %w", host, err)
	}

	// Per-identity limits (ADR-024 R7): the replication identity's exemption
	// from those limits. Without it a consumer's syncrepl search caps at
	// slapd's default 500 entries and the directory stops replicating there —
	// silently, on a cluster reporting itself Synced (ADR-020 amendment).
	replicatingIdentity := sc.Spec.Replication.Enabled && sd.ReplicationEnabled() && !sc.IsConsumerOnly()
	if err := r.ensureLimits(ctx, conn, host, dataDN, desiredLimits(sd, replicatingIdentity)); err != nil {
		return fmt.Errorf("ensure limits at %s: %w", host, err)
	}

	// Durability and read-transaction tunables (ADR-024 R1): fsync mode,
	// checkpoint interval, read-txn bound, per-database monitor counters. noSync
	// used to be create-only — flipping it on a live database was a silent
	// no-op, the R4 violation this converges away.
	if err := r.ensureBackendTunables(ctx, conn, host, dataDN, sd, sc, ""); err != nil {
		return fmt.Errorf("ensure backend tunables at %s: %w", host, err)
	}

	// LMDB environment flags (ADR-024 R2/R4): create-only, so a divergence is
	// reported through TunablesConverged rather than written. Writing it is not
	// an option — adding writemap to a live database segfaults slapd.
	if err := r.checkEnvFlags(ctx, conn, host, dataDN, sd); err != nil {
		return err
	}

	// Manage the data DB's replication infrastructure. Two independent gates:
	//
	//   wantsSyncProv  — this cluster acts as a syncrepl provider. True in
	//                    peer mode on RW pods regardless of delta-sync.
	//                    Without the syncprov overlay slapd serves regular
	//                    LDAP but consumers fail with "got search entry
	//                    without Sync State control."
	//
	//   wantsAccesslog — this DB participates in delta-syncrepl. Additive
	//                    over syncprov: gated on syncprov being wanted AND
	//                    the per-DB DeltaSync flag. Requires this database's
	//                    own accesslog DB, cn=accesslog-<CR name> (ADR-019).
	//                    It used to be one cluster-shared cn=accesslog; that
	//                    is a correctness bug, not a simplification — a shared
	//                    log makes every write to one DB kick the other DB's
	//                    consumers into permanent full refresh (ADR-019
	//                    Facts 1-2), silently falsifies contextCSN (Fact 3),
	//                    and makes the per-CR accesslogPurge cluster-global
	//                    (Fact 4). Do not re-share it.
	//
	// Order of operations matters for transitions:
	//   adding   — accesslog DB before accesslog overlay (overlay references
	//              cn=accesslog-<db> via olcAccessLogDB; slapd validates at add).
	//   removing — accesslog overlay before accesslog DB (same reference,
	//              same validation, reverse direction).
	// syncprov has no dependencies; ordered freely relative to the others.
	//
	// A cluster created before ADR-019 with a single cluster-shared
	// cn=accesslog is NOT converged here. The operator used to migrate it
	// (R8); that is withdrawn — the delete authorised itself from other
	// databases' overlays, which the operator does not own, and a reference it
	// stranded is invisible until the pod's next restart, which it then
	// prevents (ADR-026, ADR-019 amendment 2026-09-14). The migration is a
	// documented manual runbook with the operator scaled to zero; slctl
	// inspect diagnoses the layout.
	//
	// RO StatefulSet pods (readOnly=true) never carry any of this — they are
	// consumers only, not providers, and have no accesslog volume; the logs
	// they read live on the RW providers (ADR-019 R3).
	wantsSyncProv := sc.Spec.Replication.Enabled && !readOnly && !sc.IsConsumerOnly()
	wantsAccesslog := wantsSyncProv && sd.DeltaSyncEnabled()

	if !readOnly {
		// Ensure the dynamic modules backing the overlays are loaded BEFORE
		// any ensure call below. Fresh bootstraps load them via slapd.conf
		// (bootstrap.sh), but only when replication was enabled at first-boot
		// time — a pod bootstrapped standalone and later transitioned to
		// replication (replicas 1→N + replication.enabled flip) still has a
		// cn=config without them, and every overlay add then fails with
		// "objectClass: value #N invalid per syntax" (the olcSyncProvConfig /
		// olcAccessLogConfig classes come from the modules). slapd loads
		// modules dynamically via cn=module ldapmodify, no restart needed.
		// See docs/reconcile-loop-fixes.md (2026-07-15).
		var wantModules []string
		if wantsAccesslog {
			wantModules = append(wantModules, "accesslog")
		}
		if wantsSyncProv {
			wantModules = append(wantModules, "syncprov")
		}
		if len(wantModules) > 0 {
			if err := r.ensureModulesLoaded(ctx, conn, host, wantModules); err != nil {
				return fmt.Errorf("ensure modules at %s: %w", host, err)
			}
		}

		// Ensure olcServerID matches this pod's ordinal-derived identity
		// (ADR-017, bare integer). bootstrap.sh emits it only at fresh
		// bootstrap, so a pod bootstrapped by an operator predating
		// sid-1-per-default (sid 0) or carrying the pre-ADR-017 URL list needs
		// reconciling. The desired value is ordinal-keyed and equals the live
		// sid of any correctly-booted pod, so a Replace here never changes a
		// running identity — only its stored representation.
		if err := r.ensureServerIDs(ctx, conn, host, sc, ordinal); err != nil {
			return fmt.Errorf("ensure serverIDs at %s: %w", host, err)
		}

		if !wantsAccesslog {
			if err := r.removeDataDBOverlay(ctx, conn, host, dataDN, "accesslog"); err != nil {
				return fmt.Errorf("remove accesslog overlay at %s: %w", host, err)
			}
			deleted, err := r.removeAccesslogDB(ctx, conn, host, sd.Name)
			if err != nil {
				return fmt.Errorf("remove accesslog DB at %s: %w", host, err)
			}
			if deleted {
				if dataDN, err = findDataDBDN(conn, sd.Spec.Suffix); err != nil {
					return fmt.Errorf("re-resolve data DB DN at %s: %w", host, err)
				}
			}
		}
		if !wantsSyncProv {
			if err := r.removeDataDBOverlay(ctx, conn, host, dataDN, "syncprov"); err != nil {
				return fmt.Errorf("remove syncprov overlay at %s: %w", host, err)
			}
		}
		if wantsSyncProv {
			if err := r.ensureSyncProvOverlay(ctx, conn, host, dataDN, sd); err != nil {
				return fmt.Errorf("ensure syncprov overlay at %s: %w", host, err)
			}
		}
		if wantsAccesslog {
			if err := r.ensureAccesslogDB(ctx, conn, host, sd, sc); err != nil {
				return fmt.Errorf("ensure accesslog DB at %s: %w", host, err)
			}
			if err := r.ensureAccesslogOverlay(ctx, conn, host, dataDN, sd); err != nil {
				return fmt.Errorf("ensure accesslog overlay at %s: %w", host, err)
			}
		}
	}

	// Align olcReadOnly on the data DB to the cluster's current mode. In
	// consumer-only mode the data DB rejects client writes; in peer mode it
	// accepts them. This is the load-bearing step for in-place mode
	// promotion/demotion (ADR-010 3d) — at each reconcile, the current
	// olcReadOnly value is read and ldapmodify'd to match desired only when
	// it differs (idempotent).
	if !readOnly {
		desiredReadOnly := sc.IsConsumerOnly()
		if err := r.ensureReadOnly(ctx, conn, host, dataDN, desiredReadOnly); err != nil {
			return fmt.Errorf("ensure olcReadOnly at %s: %w", host, err)
		}
	}

	return nil
}

// ensureReadOnly aligns the data DB's olcReadOnly attribute to the desired
// state. Idempotent: reads the current value and modifies only when it differs.
// In consumer-only mode (ADR-010) we set TRUE so clients can't write; in peer
// mode we explicitly set FALSE so a demoted-then-promoted cluster doesn't
// inherit a stale TRUE. The explicit "FALSE" write matters because slapd
// treats missing and FALSE as equivalent, but ldapmodify Replace on a missing
// attribute would error; we use Replace which slapd accepts as "set to this
// value, creating if needed."
func (r *SlapdDatabaseReconciler) ensureReadOnly(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	desired bool,
) error {
	log := logf.FromContext(ctx)

	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcReadOnly"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcReadOnly: %w", err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s", dataDN)
	}

	current := sr.Entries[0].GetEqualFoldAttributeValue("olcReadOnly")
	desiredStr := "FALSE"
	if desired {
		desiredStr = "TRUE"
	}
	// slapd normalizes missing → FALSE for boolean attributes; treat the two
	// as equivalent so we don't write FALSE on every reconcile.
	if (current == "" && !desired) || current == desiredStr {
		return nil
	}

	log.Info("aligning olcReadOnly", "host", host, "from", current, "to", desiredStr)
	modReq := ldap.NewModifyRequest(dataDN, nil)
	modReq.Replace("olcReadOnly", []string{desiredStr})
	return conn.Modify(modReq)
}

// createDatabase adds a new olcDatabase={N}mdb entry to cn=config.
// In consumer-only mode (ADR-010), the new entry is marked olcReadOnly=TRUE
// so client writes are rejected; data still arrives via syncrepl.
func (r *SlapdDatabaseReconciler) createDatabase(
	conn *ldap.Conn,
	sd *ldapv1alpha1.SlapdDatabase,
	sc *ldapv1alpha1.SlapdCluster,
	rootPW string,
) (string, error) {
	rootDN := sd.Spec.RootDN
	if rootDN == "" {
		rootDN = "cn=admin," + sd.Spec.Suffix
	}

	// Each database gets its own subdirectory under /data/, named after the
	// SlapdDatabase CR. The SlapdCluster controller passes database names to
	// the init container (DATABASE_DIRS env var) which creates the directories.
	dataDir := sd.Spec.DataDirectory
	if dataDir == "" {
		dataDir = sd.Name
	}

	rootPWHash, err := generateSSHAHash(rootPW)
	if err != nil {
		return "", fmt.Errorf("hash root password: %w", err)
	}

	dbDirectory := "/data/" + dataDir

	// OpenLDAP auto-assigns the {N} index for new databases.
	dn := "olcDatabase=mdb,cn=config"

	addReq := ldap.NewAddRequest(dn, nil)
	addReq.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMdbConfig"})
	addReq.Attribute("olcDatabase", []string{"mdb"})
	addReq.Attribute("olcSuffix", []string{sd.Spec.Suffix})
	addReq.Attribute("olcRootDN", []string{rootDN})
	addReq.Attribute("olcRootPW", []string{rootPWHash})
	addReq.Attribute("olcDbDirectory", []string{dbDirectory})

	// Map size: always written, from spec.maxSize or the operator default
	// (ADR-024 R1/R5). Leaving it out means back-mdb's own ~10 MB, which stops
	// accepting writes the moment the directory outgrows a fixture.
	maxSize, err := desiredDataMaxSize(sd)
	if err != nil {
		return "", fmt.Errorf("spec.maxSize: %w", err)
	}
	addReq.Attribute("olcDbMaxSize", []string{strconv.FormatInt(maxSize, 10)})

	if desiredNoSync(sd, sc) {
		addReq.Attribute("olcDbNoSync", []string{"TRUE"})
	}
	if v, write := desiredCheckpoint(sd); write {
		addReq.Attribute("olcDbCheckpoint", []string{v})
	}
	addReq.Attribute("olcDbRtxnSize", []string{strconv.FormatInt(int64(desiredRtxnSize(sd)), 10)})

	// LMDB environment flags are the one tunable here that MUST be set at
	// creation: adding "writemap" to olcDbEnvFlags on a live database segfaults
	// slapd (see desiredEnvFlags). Written once, compared forever.
	if flags := desiredEnvFlags(sd); len(flags) > 0 {
		addReq.Attribute("olcDbEnvFlags", flags)
	}

	// Search limits: slaptain's defaults, not slapd's 500-entry / 3600-second
	// ones (ADR-024 R5). Converged afterwards by ensureSearchLimits.
	if v, write := desiredSizeLimit(sd); write {
		addReq.Attribute("olcSizeLimit", []string{v})
	}
	if v, write := desiredTimeLimit(sd); write {
		addReq.Attribute("olcTimeLimit", []string{v})
	}

	// The baseline index set (objectClass + the two attributes syncrepl itself
	// searches on). spec.indices adds to this; it cannot take it away.
	addReq.Attribute("olcDbIndex", planDataBaselineIndices(nil))

	// Consumer-only mode: stamp olcReadOnly=TRUE on the data DB so the cluster
	// rejects client writes while still accepting syncrepl updates from
	// externalPeers. See ADR-010.
	if sc.IsConsumerOnly() {
		addReq.Attribute("olcReadOnly", []string{"TRUE"})
	}

	if err := conn.Add(addReq); err != nil {
		return "", err
	}

	// Find the actual DN that was assigned (with the {N} index).
	return findDataDBDN(conn, sd.Spec.Suffix)
}

// ── ACL Management ───────────────────────────────────────────────────────────

func (r *SlapdDatabaseReconciler) applyACLs(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	sd *ldapv1alpha1.SlapdDatabase,
	sc *ldapv1alpha1.SlapdCluster,
) error {
	log := logf.FromContext(ctx)

	// Build effective ACL list. When replication is active, prepend a rule
	// granting the replication bind DN read access to all attributes.
	acls := sd.Spec.ACLs
	if sc.NeedsAccesslog() {
		replACL := fmt.Sprintf(
			`to * by dn.exact="cn=replication,%s" read by * break`,
			sd.Spec.Suffix)
		acls = append([]string{replACL}, acls...)
	}

	// Read current olcAccess.
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcAccess"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcAccess: %w", err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s", dataDN)
	}

	current := sr.Entries[0].GetEqualFoldAttributeValues("olcAccess")
	if aclsMatch(current, acls) {
		log.V(1).Info("ACLs already up-to-date", "host", host)
		return nil
	}

	log.Info("replacing ACL rules", "host", host, "rules", len(acls))
	modReq := ldap.NewModifyRequest(dataDN, nil)
	modReq.Replace("olcAccess", acls)
	return conn.Modify(modReq)
}

// aclsMatch returns true when the stored olcAccess values (with {N} prefixes)
// match the desired rules (without prefixes).
func aclsMatch(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	for i, c := range current {
		bare := c
		if len(c) > 0 && c[0] == '{' {
			if idx := strings.Index(c, "}"); idx >= 0 {
				bare = c[idx+1:]
			}
		}
		if bare != desired[i] {
			return false
		}
	}
	return true
}

// ── Index Management ─────────────────────────────────────────────────────────

func (r *SlapdDatabaseReconciler) applyIndices(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	desired []string,
) error {
	log := logf.FromContext(ctx)

	// Read current olcDbIndex.
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcDbIndex"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcDbIndex: %w", err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s", dataDN)
	}

	current := sr.Entries[0].GetEqualFoldAttributeValues("olcDbIndex")

	// Subtract what is already indexed, per ATTRIBUTE — see planUserIndices for
	// why a per-value comparison is not good enough (back-mdb rejects the whole
	// modify with "duplicate index definition for attr <x>").
	missing := planUserIndices(current, desired)
	if len(missing) == 0 {
		log.V(1).Info("indices already up-to-date", "host", host)
		return nil
	}

	log.Info("adding indices", "host", host, "count", len(missing))
	modReq := ldap.NewModifyRequest(dataDN, nil)
	modReq.Add("olcDbIndex", missing)
	return conn.Modify(modReq)
}

// ── Seed Data ────────────────────────────────────────────────────────────────

// dataDBOverlays returns which replication-related overlays are currently
// present on the data DB's cn=config entry. Helper for the ensure/remove
// pair below.
func dataDBOverlays(conn *ldap.Conn, dataDN string) (hasAccesslog, hasSyncprov bool, err error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcOverlayConfig)", []string{"olcOverlay"}, nil,
	))
	if err != nil {
		return false, false, fmt.Errorf("search overlays under %s: %w", dataDN, err)
	}
	for _, entry := range sr.Entries {
		for _, ov := range entry.GetEqualFoldAttributeValues("olcOverlay") {
			switch stripOrderingPrefix(ov) {
			case "accesslog":
				hasAccesslog = true
			case "syncprov":
				hasSyncprov = true
			}
		}
	}
	return hasAccesslog, hasSyncprov, nil
}

// ensureSyncProvOverlay adds the syncprov overlay to the data DB's cn=config
// entry. Required for the cluster to act as a **syncrepl provider** of any
// flavour (plain syncrepl OR delta-syncrepl). Without syncprov, slapd serves
// regular LDAP searches but does not attach Sync State controls to results —
// consumers fail with "got search entry without Sync State control."
//
// Separate from accesslog: a peer-mode cluster acts as a provider regardless
// of whether it offers delta-sync. ensureAccesslogOverlay is the additional
// step that opts a provider into delta-sync.
//
// Also converges olcSpSessionlog on every reconcile (ADR-022), which is why
// this does not early-return on an already-present overlay: the sessionlog is
// on by default, so an overlay added by an operator predating ADR-022 — or a
// changed spec value, or a hand-edit — must be brought into line without
// recreating the overlay.
//
// Idempotent: checks for existing overlay before adding, and writes
// olcSpSessionlog only when it differs from desired.
func (r *SlapdDatabaseReconciler) ensureSyncProvOverlay(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	_, hasSyncprov, err := dataDBOverlays(conn, dataDN)
	if err != nil {
		return err
	}

	ops, sessionlog := desiredSessionlogOps(sd)

	if !hasSyncprov {
		log.Info("adding syncprov overlay to data database", "host", host, "dataDN", dataDN)
		syncprovDN := "olcOverlay=syncprov," + dataDN
		addReq := ldap.NewAddRequest(syncprovDN, nil)
		addReq.Attribute("objectClass", []string{"olcOverlayConfig", "olcSyncProvConfig"})
		addReq.Attribute("olcOverlay", []string{"syncprov"})
		if checkpoint := desiredSyncprovCheckpoint(sd); checkpoint != "" {
			addReq.Attribute("olcSpCheckpoint", []string{checkpoint})
		}
		if sessionlog {
			addReq.Attribute("olcSpSessionlog", []string{strconv.Itoa(int(ops))})
		}
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add syncprov overlay: %w", err)
			}
		}
	}

	return r.ensureSyncprovTunables(ctx, conn, host, dataDN, ops, sessionlog,
		desiredSyncprovCheckpoint(sd))
}

// ensureSyncprovTunables converges the two tunables the operator owns on the
// data DB's syncprov overlay: the in-memory sessionlog (ADR-022) and the
// checkpoint interval (ADR-024 R4). One search, up to two writes.
//
// The checkpoint used to be written only into the overlay-creation addReq,
// which meant a later spec edit was silently ignored on every pod whose overlay
// already existed — the write-once anti-pattern ADR-024 R4 forbids. Both
// attributes now follow the same observe/compare/write rule, on a live-modify
// path verified not to crash or hang slapd.
//
// The overlay's real DN carries a {N} ordering prefix assigned by slapd, so it
// is read back rather than reconstructed. No overlay means nothing to converge —
// the caller's add either has not happened yet or was removed concurrently, and
// the next reconcile handles it (ADR-001).
func (r *SlapdDatabaseReconciler) ensureSyncprovTunables(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	ops int32,
	sessionlogEnabled bool,
	checkpoint string,
) error {
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcSyncProvConfig)",
		[]string{"olcSpSessionlog", "olcSpCheckpoint"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search syncprov overlay under %s: %w", dataDN, err)
	}
	if len(sr.Entries) == 0 {
		return nil
	}
	overlayDN := sr.Entries[0].DN

	if err := r.ensureSyncprovSessionlog(ctx, conn, host, overlayDN,
		sr.Entries[0].GetEqualFoldAttributeValues("olcSpSessionlog"), ops, sessionlogEnabled); err != nil {
		return err
	}

	return ensureOverlayAttr(ctx, conn, host, overlayDN, "olcSpCheckpoint",
		sr.Entries[0].GetEqualFoldAttributeValues("olcSpCheckpoint"), checkpoint)
}

// defaultSyncprovSessionlogOps is the sessionlog size the operator applies when
// spec.replication.syncprovSessionlog is unset (ADR-022). One entry is a CSN, an
// entryUUID and an op tag, so 5000 of them stay well under a megabyte resident.
const defaultSyncprovSessionlogOps int32 = 5000

// desiredSessionlogOps resolves spec.replication.syncprovSessionlog into an
// operation count and whether the sessionlog is wanted at all (ADR-022):
// unset → the operator default, 0 → disabled, >0 → that count verbatim.
//
// Applies to the DATA database's syncprov overlay only. An accesslog DB's
// syncprov must never carry a sessionlog — see ADR-022 for why a successful
// replay there would displace the minCSN guard.
func desiredSessionlogOps(sd *ldapv1alpha1.SlapdDatabase) (int32, bool) {
	if sd.Spec.Replication.SyncprovSessionlog == nil {
		return defaultSyncprovSessionlogOps, true
	}
	ops := *sd.Spec.Replication.SyncprovSessionlog
	if ops <= 0 {
		return 0, false
	}
	return ops, true
}

// sessionlogAction is what planSessionlog decided to do with olcSpSessionlog.
type sessionlogAction int

const (
	sessionlogNoop sessionlogAction = iota
	sessionlogSet
	sessionlogRemove
)

// planSessionlog compares the live olcSpSessionlog values against desired and
// returns the single write needed, if any. Pure — the LDAP half only executes
// the verdict.
//
// A live value that does not parse as an integer is treated as differing, not
// as an error: cn=config holds whatever was last written there, and converging
// it is the point.
func planSessionlog(current []string, ops int32, enabled bool) (sessionlogAction, string) {
	if !enabled {
		if len(current) == 0 {
			return sessionlogNoop, ""
		}
		return sessionlogRemove, ""
	}
	want := strconv.Itoa(int(ops))
	if len(current) == 1 {
		if live, err := strconv.Atoi(strings.TrimSpace(current[0])); err == nil && live == int(ops) {
			return sessionlogNoop, ""
		}
	}
	return sessionlogSet, want
}

// ensureSyncprovSessionlog aligns olcSpSessionlog on the data DB's syncprov
// overlay to what planSessionlog wants. Same shape as ensureAccesslogACL:
// observe, compare, write only on a difference.
//
// The overlay DN and its current values are read by the caller
// (ensureSyncprovTunables), which converges the checkpoint off the same search.
func (r *SlapdDatabaseReconciler) ensureSyncprovSessionlog(
	ctx context.Context,
	conn *ldap.Conn,
	host, overlayDN string,
	current []string,
	ops int32,
	enabled bool,
) error {
	log := logf.FromContext(ctx)

	action, value := planSessionlog(current, ops, enabled)
	if action == sessionlogNoop {
		return nil
	}

	modReq := ldap.NewModifyRequest(overlayDN, nil)
	switch action {
	case sessionlogSet:
		log.Info("setting syncprov sessionlog on data database",
			"host", host, "dn", overlayDN, "ops", value)
		modReq.Replace("olcSpSessionlog", []string{value})
	case sessionlogRemove:
		log.Info("removing syncprov sessionlog from data database",
			"host", host, "dn", overlayDN)
		modReq.Delete("olcSpSessionlog", nil)
	case sessionlogNoop:
		return nil
	}
	if err := conn.Modify(modReq); err != nil {
		if action == sessionlogRemove && ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchAttribute) {
			return nil
		}
		return fmt.Errorf("set olcSpSessionlog on %s: %w", overlayDN, err)
	}
	return nil
}

// ensureAccesslogOverlay adds the accesslog overlay to the data DB's
// cn=config entry. Required for **delta-syncrepl** specifically: the overlay
// captures every write into *this database's own* accesslog DB
// (cn=accesslog-<CR name>, ADR-019 R1), where consumers pull change deltas
// instead of re-walking the full DIT on reconnect. Strictly an add-on to
// ensureSyncProvOverlay — the cluster must already be a syncrepl provider for
// the change journal to be useful.
//
// Idempotent: checks for existing overlay before adding.
func (r *SlapdDatabaseReconciler) ensureAccesslogOverlay(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	hasAccesslog, _, err := dataDBOverlays(conn, dataDN)
	if err != nil {
		return err
	}

	purge := desiredAccesslogPurge(sd)

	if !hasAccesslog {
		log.Info("adding accesslog overlay to data database", "host", host, "dataDN", dataDN)
		accesslogDN := "olcOverlay=accesslog," + dataDN
		addReq := ldap.NewAddRequest(accesslogDN, nil)
		addReq.Attribute("objectClass", []string{"olcOverlayConfig", "olcAccessLogConfig"})
		addReq.Attribute("olcOverlay", []string{"accesslog"})
		addReq.Attribute("olcAccessLogDB", []string{ldapv1alpha1.AccesslogSuffix(sd.Name)})
		addReq.Attribute("olcAccessLogOps", []string{"writes"})
		addReq.Attribute("olcAccessLogSuccess", []string{"TRUE"})
		if purge != "" {
			addReq.Attribute("olcAccessLogPurge", []string{purge})
		}
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add accesslog overlay: %w", err)
			}
		}
	}

	// An existing overlay's olcAccessLogDB is deliberately NOT converged here.
	// Not because it cannot be — the attribute IS live-modifiable, and slapd
	// validates it against an existing backend on the online path
	// (accesslog.c accesslog_cf_gen, reached with CONFIG_ONLINE_ADD) — but
	// because repointing a live journal is an unvetted runtime-behaviour change
	// of exactly the class ADR-024's amendment says must be vetted on a running
	// server, and it would be bought only for the withdrawn R8 migration.
	// A mismatch is therefore a diagnosis, not an automatic rewrite: slctl
	// inspect reports it, and an olcAccessLogDB naming a database the pod does
	// not have is reported as fatal-at-next-restart (ADR-026).
	//
	// olcAccessLogPurge is a different matter: it IS runtime-modifiable
	// (verified live — replace and delete, no crash, no hang), so it is
	// converged on every reconcile rather than written once at creation.
	// Leaving it write-once meant an operator-defaulted or edited purge window
	// never reached a pod whose overlay predated the change, and an unpurged
	// journal ends in stalled data writes.
	return r.ensureAccesslogPurge(ctx, conn, host, dataDN, purge)
}

// ensureAccesslogPurge converges olcAccessLogPurge on the data DB's accesslog
// overlay. Thin executor over planOverlayAttr; the overlay DN is read back
// because slapd assigns it a {N} ordering prefix. No overlay means nothing to
// converge — the next reconcile handles it (ADR-001).
func (r *SlapdDatabaseReconciler) ensureAccesslogPurge(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN, purge string,
) error {
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcAccessLogConfig)",
		[]string{"olcAccessLogPurge"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search accesslog overlay under %s: %w", dataDN, err)
	}
	if len(sr.Entries) == 0 {
		return nil
	}

	return ensureOverlayAttr(ctx, conn, host, sr.Entries[0].DN, "olcAccessLogPurge",
		sr.Entries[0].GetEqualFoldAttributeValues("olcAccessLogPurge"), purge)
}

// moduleBaseName normalises one olcModuleLoad value to a bare module name:
// strips the cn=config {n} ordering prefix, any directory path, and a
// trailing .la/.so[.N] extension. "{1}accesslog", "accesslog.la" and
// "/usr/lib/ldap/accesslog.so.2" all normalise to "accesslog".
func moduleBaseName(v string) string {
	if i := strings.Index(v, "}"); strings.HasPrefix(v, "{") && i > 0 {
		v = v[i+1:]
	}
	if i := strings.LastIndex(v, "/"); i >= 0 {
		v = v[i+1:]
	}
	if i := strings.Index(v, ".la"); i > 0 {
		v = v[:i]
	} else if i := strings.Index(v, ".so"); i > 0 {
		v = v[:i]
	}
	return v
}

// missingModules returns the wanted modules (in order) that are not present
// in the given olcModuleLoad values, comparing normalised base names.
func missingModules(loaded, wanted []string) []string {
	have := make(map[string]bool, len(loaded))
	for _, v := range loaded {
		have[moduleBaseName(v)] = true
	}
	var out []string
	for _, w := range wanted {
		if !have[moduleBaseName(w)] {
			out = append(out, w)
		}
	}
	return out
}

// ensureModulesLoaded adds any of the wanted dynamic modules missing from the
// pod's olcModuleList entry. Runtime counterpart to bootstrap.sh's moduleload
// lines: those only run on a FRESH bootstrap with replication enabled at that
// moment, so a standalone→replicated transition leaves already-bootstrapped
// pods without accesslog/syncprov — and cn=config is node-local (ADR-002), so
// no peer can supply them. slapd supports dynamic module loading via
// ldapmodify on cn=module{0},cn=config; no restart required.
//
// Idempotent: searches first, adds only what's missing, and tolerates
// AttributeOrValueExists from a concurrent reconcile (ADR-001).
func (r *SlapdDatabaseReconciler) ensureModulesLoaded(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	wanted []string,
) error {
	log := logf.FromContext(ctx)

	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcModuleList)",
		[]string{"olcModuleLoad"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search olcModuleList: %w", err)
	}

	// bootstrap.sh always converts a slapd.conf with at least "moduleload
	// back_mdb", so exactly one olcModuleList entry exists on every pod this
	// operator bootstrapped. No entry at all means a foreign/hand-rolled
	// config — refuse to guess an olcModulePath and surface it instead.
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no olcModuleList entry under cn=config on %s", host)
	}
	entry := sr.Entries[0]

	missing := missingModules(entry.GetAttributeValues("olcModuleLoad"), wanted)
	if len(missing) == 0 {
		return nil
	}

	log.Info("loading missing slapd modules", "host", host,
		"dn", entry.DN, "modules", missing)
	modReq := ldap.NewModifyRequest(entry.DN, nil)
	modReq.Add("olcModuleLoad", missing)
	if err := conn.Modify(modReq); err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultAttributeOrValueExists) {
			return fmt.Errorf("add olcModuleLoad %v: %w", missing, err)
		}
	}
	return nil
}

// desiredServerID computes this pod's OWN olcServerID value: the bare integer
// serverIDBase + ordinal + 1 (ADR-011, ADR-017). cn=config is node-local
// (ADR-002), so a pod only ever needs its own ID — the integer stamped into
// every CSN it writes. No URL, no peer list: the bare form drops the
// FQDN/cluster-domain self-match that made serverID a boot-time crash surface
// (ADR-015). Byte-identical to what bootstrap.sh emits at fresh bootstrap, so
// a healthy fresh cluster reconciles to a no-op.
//
// sid-1-per-default: unconditional for RW pods — a standalone, non-replicating
// cluster carries "1" too. Stamping the sid from birth keeps CSN history
// uniform (no sid-0 epoch baked into pre-scale-up entryCSNs). The value is
// ordinal-keyed and never changes for a given pod, so no topology transition
// (scale-up, scale-down, N→M) ever alters a pod's own identity — only which
// pods exist. That invariant is what lets ensureServerIDs Replace safely: for
// any correctly-booted pod (bare OR the pre-ADR-017 URL-list form) the desired
// bare value equals its live sid, so the Replace only rewrites the textual
// representation, never the running identity.
func desiredServerID(sc *ldapv1alpha1.SlapdCluster, ordinal int32) string {
	sid := sc.Spec.Replication.ServerIDBase + ordinal + 1
	return strconv.Itoa(int(sid))
}

// serverIDSetsEqual compares two olcServerID value lists as sets, tolerant of
// whitespace-run differences within a value (slapd stores what it was given,
// but be liberal in what we accept). Under the bare-integer scheme (ADR-017) a
// healthy pod holds a single value; the set comparison still cleanly detects a
// pod carrying a stale pre-ADR-017 multi-value URL list (len differs → Replace).
func serverIDSetsEqual(a, b []string) bool {
	norm := func(vs []string) map[string]bool {
		m := make(map[string]bool, len(vs))
		for _, v := range vs {
			m[strings.Join(strings.Fields(v), " ")] = true
		}
		return m
	}
	na, nb := norm(a), norm(b)
	if len(na) != len(nb) {
		return false
	}
	for k := range na {
		if !nb[k] {
			return false
		}
	}
	return true
}

// ensureServerIDs aligns this pod's olcServerID (on cn=config) with its
// ordinal-derived identity: the bare integer serverIDBase + ordinal + 1
// (ADR-017). cn=config is node-local (ADR-002), so each pod holds only its own
// ID — no peer list, no URL self-match. Operator-owned at runtime per ADR-003
// (amended): the bootstrap copy is only the fresh-bootstrap fast path and
// never changes afterwards, which is precisely what breaks standalone→HA
// transitions and N→M scale-out.
//
// The self-sid is ordinal-keyed (sid-1-per-default), so no topology transition
// ever changes a pod's own identity. In the healthy case desired == current
// and this is a no-op; the one case that Replaces is a pod still carrying the
// pre-ADR-017 multi-value URL list, whose matched sid equals the bare desired
// value — so the Replace swaps representation, not identity. Idempotent:
// read-compare-replace only on difference.
func (r *SlapdDatabaseReconciler) ensureServerIDs(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	sc *ldapv1alpha1.SlapdCluster,
	ordinal int32,
) error {
	log := logf.FromContext(ctx)

	desired := []string{desiredServerID(sc, ordinal)}

	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"olcServerID"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcServerID: %w", err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("cn=config not found on %s", host)
	}
	current := sr.Entries[0].GetAttributeValues("olcServerID")

	if serverIDSetsEqual(current, desired) {
		return nil
	}

	log.Info("aligning olcServerID list", "host", host,
		"current", current, "desired", desired)
	modReq := ldap.NewModifyRequest("cn=config", nil)
	modReq.Replace("olcServerID", desired)
	if err := conn.Modify(modReq); err != nil {
		return fmt.Errorf("replace olcServerID: %w", err)
	}
	return nil
}

// ensureAccesslogDB creates this database's own accesslog database in
// cn=config when missing. This is the runtime counterpart to the accesslog
// configuration the init container used to write at first boot (ADR-010 3e) —
// moving it to the operator lets consumer-only → peer promotion happen
// without a rolling restart, because the underlying /accesslog volume and
// modules are always provisioned on peer-eligible pods (see
// SlapdCluster.NeedsAccesslogVolume).
//
// The log is a **per-SlapdDatabase** resource, never cluster-shared (ADR-019).
// A single cn=accesslog fed by two replicated data DBs destroys delta-syncrepl:
// the consumer's log search carries no DN scoping, the foreign change is applied
// to the wrong backend, and the resulting NO_SUCH_OBJECT is one of the five
// result codes slapd treats as "log unusable" — so every write to one DB forces
// the other DB's consumers into a full refresh, forever, on a cluster that
// reports itself healthy (ADR-019 Facts 1-2). Re-sharing it is not a
// simplification.
//
// Adds three cn=config surfaces, all keyed on the CR name (ADR-019 R1/R5):
//
//	olcDatabase=mdb cn=accesslog-<db>  — the log DB itself, backed by
//	                                     /accesslog/<db>, indexed for replog ops
//	olcAccess on that DB               — read for cn=replication,<data suffix>,
//	                                     nothing for anyone else (ADR-020 R1)
//	olcOverlay=syncprov on that DB     — exposes the change journal to
//	                                     consumers via syncrepl
//
// The ACL is written in the same pass as the DB and the overlay, and converged
// on every reconcile the way applyACLs converges the data DB's (ADR-020 R5): a
// log with no olcAccess inherits the frontend default, which is *read*, so the
// journal would hand anonymous readers exactly the attributes the data DB's own
// ACLs deny them (a password change is a write, and reqMod carries the new
// value).
//
// Idempotent: each ldap.Add silently treats EntryAlreadyExists as success, and
// the ACL is only modified when it differs. Called per-pod; each SlapdDatabase
// CR converges only its own log.
func (r *SlapdDatabaseReconciler) ensureAccesslogDB(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	sd *ldapv1alpha1.SlapdDatabase,
	sc *ldapv1alpha1.SlapdCluster,
) error {
	log := logf.FromContext(ctx)

	logSuffix := ldapv1alpha1.AccesslogSuffix(sd.Name)
	filter := fmt.Sprintf("(&(objectClass=olcMdbConfig)(olcSuffix=%s))", logSuffix)

	// Check whether this database's log DB already exists. Without this we'd
	// ldapadd every reconcile and rely on EntryAlreadyExists — works, but
	// pollutes the debug log. A single scope-children search is cheap.
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, filter,
		[]string{"dn"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search for accesslog DB %s: %w", logSuffix, err)
	}

	var dbDN string
	if len(sr.Entries) > 0 {
		dbDN = sr.Entries[0].DN
	} else {
		log.Info("creating accesslog DB", "host", host, "suffix", logSuffix)
		addReq := ldap.NewAddRequest("olcDatabase=mdb,cn=config", nil)
		addReq.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMdbConfig"})
		addReq.Attribute("olcDatabase", []string{"mdb"})
		addReq.Attribute("olcSuffix", []string{logSuffix})
		addReq.Attribute("olcRootDN", []string{"cn=admin,cn=config"})
		// back-mdb does not create olcDbDirectory; the init container
		// provisions /accesslog/<db> from DATABASE_DIRS (ADR-019 R3).
		addReq.Attribute("olcDbDirectory", []string{ldapv1alpha1.AccesslogDir(sd.Name)})
		addReq.Attribute("olcDbIndex", planAccesslogIndices(nil))
		addReq.Attribute("olcAccess", []string{accesslogACL(sd.Spec.Suffix)})
		// A journal with no map size gets back-mdb's ~10 MB, and a full journal
		// is worse than a full data DB: it fails on write RATE, not data
		// volume, and delta-syncrepl stops advancing (ADR-024 R1).
		addReq.Attribute("olcDbMaxSize", []string{strconv.FormatInt(defaultAccesslogMaxSizeBytes, 10)})
		// And the replication identity must be able to read all of it — the
		// ACL says who, this says how much (ADR-020 amendment).
		addReq.Attribute("olcLimits", []string{replicationLimits(sd.Spec.Suffix)})
		// The journal inherits the cluster's durability posture — it is written
		// on the same hot path as the data it journals, so an fsync per record
		// on one and not the other buys nothing — on its own, longer checkpoint
		// interval (defaultAccesslogCheckpoint).
		if desiredNoSync(sd, sc) {
			addReq.Attribute("olcDbNoSync", []string{"TRUE"})
		}
		addReq.Attribute("olcDbCheckpoint", []string{defaultAccesslogCheckpoint})
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add accesslog DB %s: %w", logSuffix, err)
			}
		}
		// Re-find to learn the assigned {N} prefix.
		sr2, err := conn.Search(ldap.NewSearchRequest(
			"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
			1, 0, false, filter,
			[]string{"dn"}, nil,
		))
		if err != nil || len(sr2.Entries) == 0 {
			return fmt.Errorf("re-find accesslog DB %s after add: %w", logSuffix, err)
		}
		dbDN = sr2.Entries[0].DN
	}

	// Converge the ACL on every reconcile (ADR-020 R5). Covers a log created
	// by an operator predating ADR-020, and any hand-edit.
	if err := r.ensureAccesslogACL(ctx, conn, host, dbDN, sd.Spec.Suffix); err != nil {
		return err
	}

	// Same for the index set: a log DB created by an operator predating
	// accesslogIndexAttrs is missing reqDN, the attribute every out-of-order
	// modify resolution asserts on. back-mdb reindexes online after a cn=config
	// modify, so this converges without a restart.
	if err := r.ensureAccesslogIndices(ctx, conn, host, dbDN); err != nil {
		return err
	}

	// The replication identity's limits exemption converges onto a log DB
	// created by an operator predating ADR-024 — the population that silently
	// caps at 500 journal records (ADR-020 amendment). The journal's map size
	// can only be REPORTED on an existing log DB, for the same reason as the
	// data DB's.
	if err := r.checkMaxSize(ctx, conn, host, dbDN, "accesslog database",
		defaultAccesslogMaxSizeBytes, sd); err != nil {
		return err
	}
	if err := r.ensureLimits(ctx, conn, host, dbDN,
		[]string{replicationLimits(sd.Spec.Suffix)}); err != nil {
		return fmt.Errorf("ensure accesslog limits at %s: %w", host, err)
	}

	// Durability on the journal, on its own longer checkpoint interval. Same
	// convergence as the data database; the journal has no envFlags surface
	// because it has no CRD field to diverge from.
	if err := r.ensureBackendTunables(ctx, conn, host, dbDN, sd, sc,
		defaultAccesslogCheckpoint); err != nil {
		return fmt.Errorf("ensure accesslog tunables at %s: %w", host, err)
	}

	// Read this log DB's children once, and use them for two things: reaping
	// what must not be there, and deciding whether syncprov still needs adding.
	childSR, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"objectClass"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search children of accesslog DB %s: %w", dbDN, err)
	}
	children := make([]observedConfigEntry, 0, len(childSR.Entries))
	hasSyncprov := false
	for _, e := range childSR.Entries {
		classes := e.GetEqualFoldAttributeValues("objectClass")
		children = append(children, observedConfigEntry{DN: e.DN, Classes: classes})
		for _, cls := range classes {
			if strings.EqualFold(cls, "olcSyncProvConfig") {
				hasSyncprov = true
			}
		}
	}

	// Reap an accesslog overlay mis-attached to this log DB. Never legitimate,
	// and self-inflicted: see unwantedLogDBChildren for the mechanism and why
	// nothing else would ever remove it.
	for _, dn := range unwantedLogDBChildren(children) {
		log.Info("removing an accesslog overlay mis-attached to an accesslog DB",
			"host", host, "dn", dn, "logSuffix", logSuffix)
		if err := conn.Del(ldap.NewDelRequest(dn, nil)); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
				return fmt.Errorf("delete mis-attached accesslog overlay %s: %w", dn, err)
			}
		}
	}

	// Ensure syncprov overlay on the accesslog DB. Without it consumers can't
	// pull the change journal — the whole point of the DB is to be syncrepl-
	// provisionable.
	if !hasSyncprov {
		log.Info("adding syncprov overlay to accesslog DB", "host", host, "suffix", logSuffix)
		addReq := ldap.NewAddRequest("olcOverlay=syncprov,"+dbDN, nil)
		addReq.Attribute("objectClass", []string{"olcOverlayConfig", "olcSyncProvConfig"})
		addReq.Attribute("olcOverlay", []string{"syncprov"})
		addReq.Attribute("olcSpNoPresent", []string{"TRUE"})
		addReq.Attribute("olcSpReloadHint", []string{"TRUE"})
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add syncprov overlay to accesslog DB: %w", err)
			}
		}
	}
	return nil
}

// accesslogIndexAttrs is the equality index set every per-database accesslog DB
// carries — upstream's set for a delta-syncrepl log (slapo-accesslog(5)'s
// documented example and the OpenLDAP Administrator's Guide delta-syncrepl
// recipe both index exactly these).
//
// reqDN is the one that earns its keep: multi-provider out-of-order modify
// resolution searches the *local* log with (&(entryCSN>=…)(reqDN=…)…) on every
// conflicting write (syncrepl.c), so without it a write-contended mesh does an
// unindexed attribute assertion on a hot path. entryCSN and objectClass are the
// cheap companions — accesslog purge and the log's own syncprov scan them.
//
// Note what is NOT here: "default eq". Per slapd-mdb(5), `index default <type>`
// only sets the type used for attributes listed *without* one — "setting a
// default does not imply that all attributes will be indexed". Every value the
// operator writes names its types explicitly, so the old "default eq" indexed
// nothing and is dropped. It is not removed from DBs that already have it
// (harmless, and a Replace would force a needless reindex).
var accesslogIndexAttrs = []string{
	"entryCSN", "objectClass", "reqEnd", "reqResult", "reqStart", "reqDN",
}

// planAccesslogIndices returns the olcDbIndex values to ADD so that an accesslog
// DB covers accesslogIndexAttrs, given what it carries today. nil means nothing
// to do. Thin wrapper over planIndices, which the data DB's baseline set uses
// too — one planner, one set of rules about what counts as already-indexed.
func planAccesslogIndices(current []string) []string {
	return planIndices(current, accesslogIndexAttrs)
}

// ensureAccesslogIndices adds whatever planAccesslogIndices finds missing on a
// log DB. Converged on every reconcile, the way ensureAccesslogACL converges the
// ADR-020 rule: it is what upgrades a log DB created by an operator predating
// this index set (notably one with no reqDN index).
func (r *SlapdDatabaseReconciler) ensureAccesslogIndices(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
) error {
	log := logf.FromContext(ctx)

	sr, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcDbIndex"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcDbIndex on %s: %w", dbDN, err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s", dbDN)
	}

	missing := planAccesslogIndices(sr.Entries[0].GetEqualFoldAttributeValues("olcDbIndex"))
	if len(missing) == 0 {
		return nil
	}

	log.Info("adding missing accesslog indices", "host", host, "dn", dbDN, "add", missing)
	modReq := ldap.NewModifyRequest(dbDN, nil)
	modReq.Add("olcDbIndex", missing)
	return conn.Modify(modReq)
}

// accesslogACL returns the single olcAccess rule an accesslog database carries
// (ADR-020 R1): read for the replication bind DN of the database it journals,
// nothing for anyone else. dataSuffix is the *data* DB's suffix — the same
// identity and the same dn.exact form applyACLs grants on the data DB, so the
// two stay in step by construction.
//
// No explicit rootDN grant (ADR-020 R3): the log's olcRootDN is
// cn=admin,cn=config and a rootDN bypasses ACLs, so the operator's own
// cn=config work is unaffected. An ACL line restating a bypass is noise later
// readers mistake for a requirement.
//
// Offline paths are unaffected (ADR-020 R4): slapcat/slapadd in backup and
// restore Jobs read the LMDB files directly and never evaluate ACLs.
func accesslogACL(dataSuffix string) string {
	return fmt.Sprintf(`to * by dn.exact="cn=replication,%s" read by * none`, dataSuffix)
}

// ensureAccesslogACL aligns the accesslog DB's olcAccess to accesslogACL,
// modifying only when it differs. Same shape as applyACLs on the data DB.
func (r *SlapdDatabaseReconciler) ensureAccesslogACL(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN, dataSuffix string,
) error {
	log := logf.FromContext(ctx)

	desired := []string{accesslogACL(dataSuffix)}

	sr, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcAccess"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read olcAccess on %s: %w", dbDN, err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s", dbDN)
	}
	if aclsMatch(sr.Entries[0].GetEqualFoldAttributeValues("olcAccess"), desired) {
		return nil
	}

	log.Info("setting accesslog DB ACL", "host", host, "dn", dbDN)
	modReq := ldap.NewModifyRequest(dbDN, nil)
	modReq.Replace("olcAccess", desired)
	if err := conn.Modify(modReq); err != nil {
		return fmt.Errorf("set olcAccess on %s: %w", dbDN, err)
	}
	return nil
}

// removeAccesslogDB tears down this database's accesslog DB during peer →
// consumer-only demotion (ADR-010 3e). Removes the syncprov overlay first
// (children must go before parents in slapd's cn=config), then the DB entry
// itself. Scoped to cn=accesslog-<dbName> (ADR-019): with per-database logs a
// demotion of one SlapdDatabase can no longer remove a log another CR is still
// using.
//
// The underlying /accesslog volume is left mounted and the LMDB data files in
// /accesslog/<dbName> stay on disk — they're harmless when the DB entry isn't
// referenced. A later promotion re-creates the DB pointed at the same
// directory, and slapd re-attaches to whatever is there. (This is acceptable
// for the migration rollback use case; if you want a clean accesslog history on
// re-promotion, wipe /accesslog/<dbName> manually before promoting.)
//
// Idempotent: NoSuchObject is silently treated as success.
// Returns true when a database was actually deleted, so the caller can discard
// olcDatabase={N} DNs resolved earlier (see reconcilePodDatabase). This path is
// the ADR-010 peer → consumer-only demotion; the log normally sits at a HIGHER
// index than its data DB, in which case nothing shifts — but that is an
// accident of creation order, not a guarantee, and the caller must not depend
// on it.
func (r *SlapdDatabaseReconciler) removeAccesslogDB(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbName string,
) (bool, error) {
	logSuffix := ldapv1alpha1.AccesslogSuffix(dbName)
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		1, 0, false, fmt.Sprintf("(&(objectClass=olcMdbConfig)(olcSuffix=%s))", logSuffix),
		[]string{"dn"}, nil,
	))
	if err != nil {
		return false, fmt.Errorf("search accesslog DB %s: %w", logSuffix, err)
	}
	if len(sr.Entries) == 0 {
		return false, nil
	}
	if err := r.removeAccesslogDBAt(ctx, conn, host, sr.Entries[0].DN); err != nil {
		return false, err
	}
	return true, nil
}

// removeAccesslogDBAt deletes one accesslog database by its cn=config DN,
// children first (slapd does not cascade). Split out of removeAccesslogDB so the
// ADR-019 R8 migration can delete the legacy shared log, whose suffix is not
// derivable from any CR name.
//
// Idempotent: NoSuchObject is silently treated as success.
func (r *SlapdDatabaseReconciler) removeAccesslogDBAt(
	ctx context.Context,
	conn *ldap.Conn,
	host, dbDN string,
) error {
	log := logf.FromContext(ctx)

	// Remove children first — slapd doesn't cascade.
	childSR, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=*)", []string{"dn"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search accesslog DB children: %w", err)
	}
	for _, child := range childSR.Entries {
		log.Info("removing accesslog DB child", "host", host, "dn", child.DN)
		if err := conn.Del(ldap.NewDelRequest(child.DN, nil)); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
				return fmt.Errorf("delete %s: %w", child.DN, err)
			}
		}
	}

	log.Info("removing accesslog DB", "host", host, "dn", dbDN)
	if err := conn.Del(ldap.NewDelRequest(dbDN, nil)); err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return fmt.Errorf("delete accesslog DB: %w", err)
		}
	}
	return nil
}

// removeDataDBOverlay deletes a single named replication overlay (accesslog
// or syncprov) from the data DB's cn=config children. Entries are deleted by
// their actual DN — which carries slapd's {N} ordering prefix — since the
// bare-name DN may not resolve when slapd has reordered. Idempotent:
// NoSuchObject is success.
func (r *SlapdDatabaseReconciler) removeDataDBOverlay(
	ctx context.Context,
	conn *ldap.Conn,
	host, dataDN, overlayName string,
) error {
	log := logf.FromContext(ctx)

	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcOverlayConfig)", []string{"olcOverlay"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search overlays under %s: %w", dataDN, err)
	}

	for _, entry := range sr.Entries {
		vals := entry.GetEqualFoldAttributeValues("olcOverlay")
		if len(vals) == 0 || stripOrderingPrefix(vals[0]) != overlayName {
			continue
		}
		log.Info("removing data DB overlay", "host", host, "overlay", overlayName, "dn", entry.DN)
		if err := conn.Del(ldap.NewDelRequest(entry.DN, nil)); err != nil {
			if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
				return nil
			}
			return fmt.Errorf("delete overlay %s: %w", entry.DN, err)
		}
		return nil
	}
	return nil
}

// ensureReplicationUser creates the cn=replication,<suffix> bind user in the data
// tree on any reachable RW pod. This user is referenced by syncrepl stanzas.
// Idempotent: skips if the entry already exists.
func (r *SlapdDatabaseReconciler) ensureReplicationUser(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	rootPW string,
) error {
	log := logf.FromContext(ctx)

	replPassword, err := r.getDatabaseReplPassword(ctx, sd)
	if err != nil {
		return err
	}
	if replPassword == "" {
		return nil // No replication password configured.
	}

	rootDN := sd.Spec.RootDN
	if rootDN == "" {
		rootDN = "cn=admin," + sd.Spec.Suffix
	}
	replDN := "cn=replication," + sd.Spec.Suffix

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"

	// Try each RW pod.
	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.%s",
			sc.Name, i, headlessSvc, sc.Namespace, r.ClusterDomain)
		addr := host + ":" + strconv.Itoa(int(ldapContainerPort))

		conn, err := ldap.DialURL("ldap://"+addr,
			ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
		)
		if err != nil {
			continue
		}
		conn.SetTimeout(ldapRequestTimeout)

		if err := conn.Bind(rootDN, rootPW); err != nil {
			conn.Close()
			continue
		}

		exists, err := ldapEntryExists(conn, replDN)
		if err != nil {
			conn.Close()
			continue
		}
		if exists {
			conn.Close()
			log.V(1).Info("replication user already exists", "dn", replDN)
			return nil
		}

		// Create the replication bind user.
		replHash, err := generateSSHAHash(replPassword)
		if err != nil {
			conn.Close()
			return fmt.Errorf("hash replication password: %w", err)
		}

		addReq := ldap.NewAddRequest(replDN, nil)
		addReq.Attribute("objectClass", []string{"simpleSecurityObject", "organizationalRole"})
		addReq.Attribute("cn", []string{"replication"})
		addReq.Attribute("description", []string{"Syncrepl bind account"})
		addReq.Attribute("userPassword", []string{replHash})
		if err := conn.Add(addReq); err != nil {
			conn.Close()
			if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return nil
			}
			return fmt.Errorf("add replication user %s: %w", replDN, err)
		}

		conn.Close()
		log.Info("created replication user", "dn", replDN)
		return nil
	}

	return fmt.Errorf("no reachable RW pod for replication user creation")
}

// seedNeeded decides whether this reconcile should attempt to apply seed data.
//
// Two inputs, both on this CR: the user declared seed entries, and the one-way
// latch says they have not been settled yet. Nothing observed about the
// directory's current contents appears here, and nothing may be added — that
// is ADR-012's boundary: absence of data must never trigger a write (the
// reverted verifySeedExists), so the observability latches (DataObserved,
// RestoreApplied) are deliberately not consulted. Declining to seed on positive
// evidence of a foreign creator is a different decision, made inside
// applySeedData by the ADR-025 withhold belt.
func seedNeeded(sd *ldapv1alpha1.SlapdDatabase) bool {
	return sd.Spec.Seed != nil && len(sd.Spec.Seed.Entries) > 0 && !sd.Status.SeedApplied
}

// applySeedData applies seed entries to pod-0 deterministically. Targets pod-0
// only — never falls back to higher ordinals — so we never end up writing the
// same DNs from two different pods, which would create a CSN-conflict storm in
// a multi-master mesh (entries 4-6 lost in the resolution race; see ADR-012
// and reconcile-loop-fixes.md 2026-05-14 entry).
//
// After each entry's add, we re-bind-search to verify the entry actually
// persisted on the same connection. If any verification fails, the whole call
// returns error and Status.SeedApplied stays false; the next reconcile retries.
// Only on a complete, verified run does the caller flip SeedApplied=true. Seed
// is one-shot per cluster lifetime — the latch never reverts.
//
// withheld=true (ADR-025): pod-0's suffix entry already exists and carries a
// FOREIGN serverID in its entryCSN — another mesh member (the founder site)
// created this DIT and replication delivered it. No entry is written; the
// caller latches SeedApplied. Positive evidence only: an absent suffix, an
// unreadable entryCSN, or a local-sid creator all proceed with the normal
// idempotent per-entry loop (the local-sid case is our own earlier partial
// run, which per-entry idempotence already handles).
func (r *SlapdDatabaseReconciler) applySeedData(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	rootPW string,
) (withheld bool, err error) {
	log := logf.FromContext(ctx)

	headlessSvc := sc.Name + "-headless"
	host := fmt.Sprintf("%s-0.%s.%s.svc.%s",
		sc.Name, headlessSvc, sc.Namespace, r.ClusterDomain)
	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))

	rootDN := sd.Spec.RootDN
	if rootDN == "" {
		rootDN = "cn=admin," + sd.Spec.Suffix
	}

	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return false, fmt.Errorf("dial pod-0 (%s): %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind(rootDN, rootPW); err != nil {
		return false, fmt.Errorf("bind %s on pod-0: %w", rootDN, err)
	}

	// ADR-025 withhold probe: does the suffix already have a foreign creator?
	// Best-effort — any failure to read positive evidence falls through to the
	// normal seed path. (A GLUE suffix is invisible to this ordinary search;
	// seeding then aborts loudly on the added-but-not-visible verification
	// below, which is the deliberate failure direction: a human looks.)
	if csn, found := suffixEntryCSN(conn, sd.Spec.Suffix); found && seedCreatorIsForeign(csn, sc) {
		return true, nil
	}

	for i, entry := range sd.Spec.Seed.Entries {
		dn, err := r.applySeedEntry(conn, entry)
		if err != nil {
			return false, fmt.Errorf("seed entry %d (%s): %w", i, dn, err)
		}
		// Verify persistence: a successful Add doesn't guarantee the entry is
		// readable (server-side rejection that returned success, ACL quirk,
		// replication-layer interference). A base-scope search on the same
		// connection catches all of these before we move on.
		exists, err := ldapEntryExists(conn, dn)
		if err != nil {
			return false, fmt.Errorf("verify seed entry %d (%s): %w", i, dn, err)
		}
		if !exists {
			return false, fmt.Errorf("seed entry %d (%s) added but not visible — aborting", i, dn)
		}
	}

	log.Info("seed data applied and verified", "host", host, "entries", len(sd.Spec.Seed.Entries))
	return false, nil
}

// suffixEntryCSN base-searches the suffix entry and returns its entryCSN.
// found=false when the entry is absent, hidden, or the attribute unreadable —
// callers must treat that as "no evidence", never as a verdict.
func suffixEntryCSN(conn *ldap.Conn, suffix string) (string, bool) {
	res, err := conn.Search(ldap.NewSearchRequest(
		suffix, ldap.ScopeBaseObject, ldap.NeverDerefAliases, 0, 5, false,
		"(objectClass=*)", []string{"entryCSN"}, nil,
	))
	if err != nil || len(res.Entries) == 0 {
		return "", false
	}
	csn := res.Entries[0].GetEqualFoldAttributeValue("entryCSN")
	if csn == "" {
		return "", false
	}
	return csn, true
}

// evaluateDataPresent computes the DataPresent status condition: is the
// database suffix's root entry visible on every reached pod — every RW pod, and
// every read-only consumer pod when the cluster has any?
//
// Pure observability. ADR-012 is emphatic that this signal NEVER drives
// reconciler behaviour — no re-seed, no recreate, nothing. It exists so that
// monitoring/alerting can detect "we accidentally lost the directory" within
// one reconcile interval instead of waiting for a user to report failing
// queries. The reason field distinguishes:
//
//	RootEntryVisible  — root entry found on EVERY assessed pod. ConditionTrue.
//	GlueSuffix        — a ManageDsaIT probe positively identified a glue entry
//	                    on at least one pod (ADR-025). ConditionFalse — alert.
//	DataMissingOnPods — root entry visible on some assessed pods, hidden on at
//	                    least one WRITABLE one (the glue signature without the
//	                    confirmation — e.g. the probe could not use the
//	                    control). ConditionFalse — alert.
//	DataMissingOnReadOnlyPods
//	                  — every writable pod has it, a read-only replica does
//	                    not. ConditionFalse, but its own reason: this is the
//	                    one shape with a routine benign cause (an RO consumer
//	                    still performing its initial sync), so an alert rule
//	                    can hold it to a longer fuse than a writable pod's
//	                    divergence. It does NOT affect the database phase,
//	                    following the readOnlyReadyReplicas precedent.
//	DataMissing       — the database is known to have held data, and the root
//	                    entry is visible nowhere. ConditionFalse — alert.
//	NoDataYet         — nothing anywhere, and nothing ever seeded, restored or
//	                    observed: a database still waiting for its first data.
//	                    ConditionUnknown.
//	NoReachablePod    — no pod could be assessed (cluster churning, all pods
//	                    crash-looping). ConditionUnknown.
//
// Two properties are load-bearing and were both bought with incidents:
//
//   - Every assessed pod must show the entry (ADR-025 D1). The previous
//     any-pod-visible verdict read True across the 2026-09-13 incident, where
//     one pod's suffix had been demoted to a hidden glue while its peers
//     looked healthy.
//   - Read-only pods are in scope (2026-09-14). A glue propagates to them: the
//     incident site's RO pod carried the same glue with the same entryUUID as
//     its provider, a consumer having initial-synced the corruption faithfully
//     (ADR-025 evidence item 5). They are probed exactly like RW pods — the
//     data rootDN and its password are configured on RO pods too — and a glue
//     there reports GlueSuffix like anywhere else. Only the benign-transient
//     shape is given its own reason.
//   - The probe runs regardless of how (or whether) this database was seeded
//     (2026-09-14). It used to return early on !SeedApplied, which meant a
//     database that is never seeded — every non-founder site of a mesh, since
//     ADR-025 D1 tells peers to omit spec.seed, plus hot-migration (ADR-011)
//     and consumer-only (ADR-010) clusters — never ran the check at all. The
//     seed latch answers "did WE write the initial data", which stopped being
//     the same question as "is data expected here" the moment data started
//     arriving by replication. What the latch is still needed for is narrow and
//     lives in databaseKnownPopulated: telling an empty new database from one
//     that lost its contents.
//
// Aggregation is the pure aggregateDataPresent; observed=true reports that the
// root entry was seen on at least one pod this pass, for the caller's latch.
func (r *SlapdDatabaseReconciler) evaluateDataPresent(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	rootPW string,
) (cond metav1.Condition, observed bool) {
	cond = metav1.Condition{
		Type:               "DataPresent",
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	}

	rootDN := sd.Spec.RootDN
	if rootDN == "" {
		rootDN = "cn=admin," + sd.Spec.Suffix
	}
	targets := suffixProbeTargets(sc, r.ClusterDomain)

	results := make([]podPresence, 0, len(targets))
	for _, t := range targets {
		pod := t.pod
		addr := t.host + ":" + strconv.Itoa(int(ldapContainerPort))

		conn, err := ldap.DialURL("ldap://"+addr,
			ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
		)
		if err != nil {
			results = append(results, podPresence{pod: pod, readOnly: t.readOnly})
			continue
		}
		conn.SetTimeout(ldapRequestTimeout)
		if err := conn.Bind(rootDN, rootPW); err != nil {
			conn.Close()
			results = append(results, podPresence{pod: pod, readOnly: t.readOnly})
			continue
		}
		// The data rootDN bypasses ACLs, so a hidden entry here is hidden by
		// slapd's frontend — which is exactly what a glue is. The shared probe
		// asks again with ManageDsaIT to say so positively (ADR-025).
		obs := suffixprobe.Probe(conn, sd.Spec.Suffix)
		conn.Close()
		results = append(results, podPresence{
			pod:      pod,
			readOnly: t.readOnly,
			assessed: obs.Assessable(),
			present:  obs.Visible(),
			glued:    obs.Glue(),
		})
	}

	cond.Status, cond.Reason, cond.Message = aggregateDataPresent(
		sd.Spec.Suffix, results, databaseKnownPopulated(sd))
	return cond, dataWasObserved(results)
}

// applySeedEntry parses a simplified LDIF entry and adds it via LDAP.
// Format: first line is "dn: <dn>", remaining lines are "attr: value".
// Blank lines are ignored. Returns the parsed DN so the caller can verify
// post-add persistence even when the add was an idempotent no-op.
func (r *SlapdDatabaseReconciler) applySeedEntry(conn *ldap.Conn, entry string) (string, error) {
	lines := strings.Split(strings.TrimSpace(entry), "\n")
	if len(lines) == 0 {
		return "", nil
	}

	// Parse dn.
	if !strings.HasPrefix(strings.ToLower(lines[0]), "dn:") {
		return "", fmt.Errorf("seed entry must start with 'dn:', got %q", lines[0])
	}
	dn := strings.TrimSpace(lines[0][3:])

	// Idempotent: if the entry already exists on pod-0 (most likely from a
	// previous, partially-successful seed run that the caller retried), the
	// caller's post-add verification will confirm it's still there and we
	// move on without modifying it.
	exists, err := ldapEntryExists(conn, dn)
	if err != nil {
		return dn, fmt.Errorf("check existence of %s: %w", dn, err)
	}
	if exists {
		return dn, nil
	}

	// Parse attributes.
	attrs := make(map[string][]string)
	for _, line := range lines[1:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		key := strings.TrimSpace(parts[0])
		val := strings.TrimSpace(parts[1])
		attrs[key] = append(attrs[key], val)
	}

	addReq := ldap.NewAddRequest(dn, nil)
	for k, v := range attrs {
		addReq.Attribute(k, v)
	}
	if err := conn.Add(addReq); err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return dn, nil
		}
		return dn, fmt.Errorf("add %s: %w", dn, err)
	}
	return dn, nil
}

// ldapEntryExists returns true when the given DN exists in the directory.
func ldapEntryExists(conn *ldap.Conn, dn string) (bool, error) {
	req := ldap.NewSearchRequest(
		dn, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"dn"}, nil,
	)
	result, err := conn.Search(req)
	if err != nil {
		if ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
			return false, nil
		}
		return false, err
	}
	return len(result.Entries) > 0, nil
}

// ── Replication ──────────────────────────────────────────────────────────────

// reconcileReplication applies syncrepl stanzas for this database to each pod.
// Uses the database's ridBase for RID assignment. See ADR-003 amendment.
//
// healthyPods names the pods whose per-pod database reconcile completed in this
// pass; a pod outside the set is skipped and the caller requeues.
//
// Why a stanza must not be written to a pod whose database reconcile failed: a
// delta-sync stanza carries `logbase`, and the log it names is created by that
// very per-pod reconcile (ensureAccesslogDB). Writing the stanza first points
// the consumer at a search base the provider does not have — and that does not
// degrade gracefully. Observed live, upgrading a legacy cluster whose pods had
// not yet rolled (so /accesslog/<db> did not exist and the log add kept
// failing):
//
//	do_syncrep1: rid=101 starting refresh (sending cookie=...)
//	do_syncrep2: rid=101 LDAP_RES_SEARCH_RESULT (32) No such object
//	do_syncrepl: rid=101 rc -101 retrying
//
// Replication stopped outright for as long as the window lasted — an entry
// written on pod-0 was still absent on the other two pods minutes later. It is
// NOT the "falls back to full refresh" behaviour a missing logbase is often
// assumed to produce: the consumer never gets past the log search.
//
// Skipping is the conservative direction: a pod that already has working stanzas
// keeps them (nothing is removed), and a pod that has none simply waits, exactly
// as it already does for every other step of its database reconcile. It also
// matches the order ADR-010 prescribes for consumer-only → peer promotion, where
// the accesslog DB and the overlays are added *before* the in-cluster stanzas.
// The cost is that an unrelated per-pod failure also defers that pod's external
// (including plain-syncrepl, ADR-011) stanza updates; deferring one pod's
// external stanza by a reconcile is cheaper than the alternative, which is
// halting its replication until a human notices.
func (r *SlapdDatabaseReconciler) reconcileReplication(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	configPW string,
	healthyPods map[string]bool,
) (bool, error) {
	log := logf.FromContext(ctx)

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	// Skip only when there is genuinely no syncrepl work to do — i.e. no
	// in-cluster mesh AND no external peers. Consumer-only mode (where the
	// cluster pulls from externalPeers but produces no writes) MUST still
	// reach this function to write its external syncrepl stanzas — so we
	// deliberately do NOT use NeedsAccesslog() here (it returns false in
	// consumer-only and would silently disable the consumer behaviour).
	hasInClusterMesh := replicas > 1
	hasExternalPeers := len(sc.Spec.Replication.ExternalPeers) > 0
	if !sc.Spec.Replication.Enabled || (!hasInClusterMesh && !hasExternalPeers) {
		return false, nil
	}

	headlessSvc := sc.Name + "-headless"
	tlsEnabled := sc.Spec.LDAP.TLS.Enabled

	replPassword, err := r.getDatabaseReplPassword(ctx, sd)
	if err != nil {
		return false, err
	}

	// Resolve external peers. PodAddresses- and discovery-mode peers carry a list of
	// per-pod URIs and a ReplicasPerPeer fan-out factor; URI-mode peers carry a single
	// URI. Diagonal-first selection (URIs[(ordinal + k) % len(URIs)]) is applied
	// per-local-pod inside buildDatabaseSyncRepl.
	var externalPeers []resolvedExternalPeer
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		tlsCACertPath := ""
		if ep.TLSSecretName != "" {
			tlsCACertPath = "/etc/openldap/tls/peers/" + ep.Name + "/ca.crt"
		}

		password := ""
		if ep.BindPasswordSecretName != "" {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{
				Name: ep.BindPasswordSecretName, Namespace: sc.Namespace,
			}, secret); err != nil {
				log.Info("skipping external peer: cannot read bind password secret",
					"peer", ep.Name, "err", err)
				continue
			}
			password = string(secret.Data["password"])
			if password == "" {
				password = string(secret.Data["replication-password"])
			}
		}

		// Determine the list of addresses for podAddresses/discovery modes.
		// URI mode falls through with podAddrs empty.
		var podAddrs []string
		if ep.Discovery != nil {
			for _, ps := range sc.Status.ExternalPeerStatuses {
				if ps.Name == ep.Name {
					podAddrs = ps.DiscoveredAddresses
					break
				}
			}
			if len(podAddrs) == 0 {
				log.Info("skipping discovery peer: no addresses discovered yet",
					"peer", ep.Name)
				continue
			}
		} else if len(ep.PodAddresses) > 0 {
			podAddrs = ep.PodAddresses
		}

		rpp := int32(1)
		if ep.ReplicasPerPeer != nil && *ep.ReplicasPerPeer > 1 {
			rpp = *ep.ReplicasPerPeer
		}

		// Per-database bind identity: derived unless the peer overrides it.
		bindDN, bindPassword := externalBindIdentity(
			ep.BindDN, ep.BindPasswordSecretName, password, sd.Spec.Suffix, replPassword)

		if len(podAddrs) > 0 {
			port := ep.Port
			if port == 0 {
				port = 1025
			}
			scheme := "ldaps"
			if !tlsEnabled {
				scheme = "ldap"
				if port == 1025 {
					port = 1024
				}
			}
			uris := make([]string, len(podAddrs))
			for i, addr := range podAddrs {
				uris[i] = fmt.Sprintf("%s://%s:%d", scheme, addr, port)
			}
			externalPeers = append(externalPeers, resolvedExternalPeer{
				Name:            ep.Name,
				URIs:            uris,
				ReplicasPerPeer: rpp,
				BindDN:          bindDN,
				Password:        bindPassword,
				TLSCACertPath:   tlsCACertPath,
				PlainSyncRepl:   ep.SyncMode == "plain",
			})
		} else {
			externalPeers = append(externalPeers, resolvedExternalPeer{
				Name:            ep.Name,
				URIs:            []string{ep.URI},
				ReplicasPerPeer: 1,
				BindDN:          bindDN,
				Password:        bindPassword,
				TLSCACertPath:   tlsCACertPath,
				PlainSyncRepl:   ep.SyncMode == "plain",
			})
		}
	}

	skipped := false
	ridBase := int32(0)
	if sd.Spec.Replication.RIDBase != nil {
		ridBase = *sd.Spec.Replication.RIDBase
	}

	retryInterval := sc.Spec.Replication.Retry
	if retryInterval == "" {
		retryInterval = "10 +"
	}
	// Keepalive gets an operator default rather than staying empty (ADR-024 R5,
	// finding 11): a refreshAndPersist connection is idle by design between
	// writes, and an idle flow silently dropped by a firewall leaves a consumer
	// that has stopped consuming while still reporting itself Synced.
	keepalive := desiredKeepalive(sc)

	useDeltaSync := sd.DeltaSyncEnabled()

	// Resolve replication network IPs for in-cluster peers (Multus, ADR-007).
	// Only used when useForInCluster is true.
	var replNetIPs map[string]string
	if sc.Spec.Replication.Network != nil && sc.Spec.Replication.Network.UseForInCluster {
		replNetIPs = sc.Status.ReplicationNetworkIPs
	}

	// Consumer-only mode (ADR-010): no in-cluster mesh and no MultiProvider —
	// each pod independently consumes from externalPeers, the data DB is
	// olcReadOnly=TRUE, and slapd does not advertise as a write source.
	consumerOnly := sc.IsConsumerOnly()
	multiProvider := "TRUE"
	if consumerOnly {
		multiProvider = "FALSE"
	}

	// Apply to RW pods.
	for i := int32(0); i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", sc.Name, i)
		host := fmt.Sprintf("%s-%d.%s.%s.svc.%s",
			sc.Name, i, headlessSvc, sc.Namespace, r.ClusterDomain)

		if !healthyPods[podName] {
			log.Info("replication stanzas deferred: the pod's database reconcile "+
				"did not complete this pass (will retry)", "pod", podName)
			skipped = true
			continue
		}

		desired := buildDatabaseSyncRepl(
			sc.Name, headlessSvc, sc.Namespace, r.ClusterDomain, sd.Spec.Suffix, sd.Name,
			replicas, i, replPassword,
			tlsEnabled, ridBase,
			retryInterval, keepalive,
			useDeltaSync,
			ldapv1alpha1.ExternalLogBase(sd),
			externalPeers,
			replNetIPs,
			consumerOnly,
		)

		if err := r.applySyncreplToPod(ctx, host, configPW, sd.Spec.Suffix, desired, multiProvider); err != nil {
			log.Info("replication reconcile skipped for pod (will retry)",
				"ordinal", i, "host", host, "err", err)
			skipped = true
		}
	}

	// Apply to RO pods (all RW masters, no self-skip, no external peers, no mirrormode).
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			podName := fmt.Sprintf("%s-readonly-%d", sc.Name, i)
			host := fmt.Sprintf("%s-readonly-%d.%s.%s.svc.%s",
				sc.Name, i, roHeadless, sc.Namespace, r.ClusterDomain)

			// RO pods have no log of their own, but their stanzas still name the
			// RW providers' logs (ADR-019 R3) — and the reason to defer is the
			// same: their own database reconcile is what establishes the
			// preconditions the stanza assumes.
			if !healthyPods[podName] {
				log.Info("replication stanzas deferred for read-only pod: its database "+
					"reconcile did not complete this pass (will retry)", "pod", podName)
				skipped = true
				continue
			}

			desired := buildDatabaseSyncReplRO(
				sc.Name, headlessSvc, sc.Namespace, r.ClusterDomain, sd.Spec.Suffix, sd.Name,
				replicas, replPassword,
				tlsEnabled, ridBase,
				retryInterval, keepalive,
				useDeltaSync,
				replNetIPs,
			)

			if err := r.applySyncreplToPod(ctx, host, configPW, sd.Spec.Suffix, desired, ""); err != nil {
				log.Info("replication reconcile skipped for read-only pod (will retry)",
					"ordinal", i, "host", host, "err", err)
				skipped = true
			}
		}
	}

	return skipped, nil
}

// externalBindIdentity returns the bind DN and password an external peer's
// syncrepl stanzas use for THIS database.
//
// The bind identity is a per-database value, exactly like logbase: the same
// ExternalPeer list is applied to every SlapdDatabase, so a single peer-level
// value cannot be right for more than one of them (ADR-019 R9's axis argument,
// extended to the bind identity by the 2026-09-13 amendment). When the
// peer-level fields are unset, the identity therefore derives per database —
// cn=replication,<suffix> with the database's own replication password, the
// same identity the in-cluster stanzas bind as and the one ADR-020 R2 requires
// external consumers to present. Derived at the point of use, never written
// back (ADR-019 R10 pattern).
//
// Set fields win verbatim — the ADR-011 escape for foreign sources with their
// own bind accounts. The two fields default independently, with one deliberate
// asymmetry: a bindPasswordSecretName that was named but yielded no password
// (missing keys) keeps the empty credential rather than borrowing the per-DB
// one — "could not read it" is not "not set", and a silent substitution would
// mask the broken override; the stanza fails loudly at the consumer instead.
func externalBindIdentity(overrideBindDN, overrideSecretName, overridePassword, suffix, dbReplPassword string) (string, string) {
	bindDN := overrideBindDN
	if bindDN == "" {
		bindDN = "cn=replication," + suffix
	}
	password := overridePassword
	if overrideSecretName == "" {
		password = dbReplPassword
	}
	return bindDN, password
}

// resolvedExternalPeer holds an external peer with its full address list and
// fan-out factor. URI-mode peers have URIs of length 1 and ReplicasPerPeer=1.
// Multi-pod peers have URIs of length N (one per remote pod) and the fan-out
// is applied per-local-pod via diagonal-first selection in buildDatabaseSyncRepl.
type resolvedExternalPeer struct {
	Name            string
	URIs            []string
	ReplicasPerPeer int32
	BindDN          string
	Password        string
	TLSCACertPath   string
	// PlainSyncRepl, when true, suppresses delta-syncrepl options (no
	// logbase / syncdata=accesslog) for stanzas pointing at this peer. Set
	// from ExternalPeer.SyncMode="plain" — see ADR-011.
	PlainSyncRepl bool
}

// buildDatabaseSyncRepl computes syncrepl stanzas for one RW pod using per-database
// ridBase. In-cluster peer i → RID = ridBase + i + 1. External stanzas are numbered
// sequentially starting at ridBase + 50 + 1, in the order peers appear in the spec.
// For each multi-pod external peer, only ReplicasPerPeer remote URIs are selected,
// using diagonal-first assignment URIs[(ordinal + k) % len(URIs)]; this keeps each
// local pod talking to a distinct slice of the remote cluster while the remote
// cluster's own internal mesh fans the data out across all remote pods.
// When replNetIPs is non-nil and contains an IP for a peer pod, that IP is used instead of
// the headless DNS name (Multus replication network mode, see ADR-007).
func buildDatabaseSyncRepl(
	clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName string,
	replicas, ordinal int32,
	replPassword string,
	tlsEnabled bool,
	ridBase int32,
	retryInterval, keepalive string,
	deltaSync bool,
	externalLogSuffix string,
	externalPeers []resolvedExternalPeer,
	replNetIPs map[string]string,
	consumerOnly bool,
) []string {
	var stanzas []string

	peerScheme := "ldap"
	peerPort := int32(1024)
	tlsOpt := ""
	if tlsEnabled {
		peerScheme = "ldaps"
		peerPort = 1025
		tlsOpt = " tls_cacert=/etc/openldap/tls/ca.crt"
	}

	keepaliveOpt := ""
	if keepalive != "" {
		keepaliveOpt = fmt.Sprintf(" keepalive=%s", keepalive)
	}

	// In-cluster peers run our own layout, so logbase is this database's own
	// per-DB accesslog suffix (ADR-019 R1/R5). logfilter stays the
	// upstream-standard filter: with a per-database log the log is already
	// scoped, so reqDN scoping would be dead weight on an unindexed attribute
	// assertion and would mask a mis-derived logbase (ADR-019 R6).
	deltaSyncOpts := ""
	if deltaSync {
		deltaSyncOpts = fmt.Sprintf(` logbase="%s"`, ldapv1alpha1.AccesslogSuffix(dbName)) +
			` logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"` +
			` syncdata=accesslog`
	}

	// External peers evaluate logbase on *their* side (ADR-019 R9), so their
	// stanzas name externalLogSuffix — the caller's ldapv1alpha1.ExternalLogBase(sd):
	// this database's derived suffix by default, or the per-database override
	// for a peer that is not slaptain / does not use the same CR name.
	externalDeltaOpts := ""
	if deltaSync {
		externalDeltaOpts = fmt.Sprintf(` logbase="%s"`, externalLogSuffix) +
			` logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"` +
			` syncdata=accesslog`
	}

	// In-cluster stanzas. Skipped entirely in consumer-only mode (ADR-010):
	// each pod independently consumes from externalPeers and the cluster has
	// no internal multi-master mesh.
	inClusterReplicas := replicas
	if consumerOnly {
		inClusterReplicas = 0
	}
	for i := int32(0); i < inClusterReplicas; i++ {
		if i == ordinal {
			continue
		}
		rid := fmt.Sprintf("%03d", ridBase+i+1)
		peerPodName := fmt.Sprintf("%s-%d", clusterName, i)
		peerHost := fmt.Sprintf("%s.%s.%s.svc.%s", peerPodName, headlessSvc, namespace, clusterDomain)
		peerTLSOpt := tlsOpt
		// Use Multus replication network IP when available (ADR-007).
		// IP-based provider URIs won't match DNS SANs in the TLS cert, so
		// relax hostname verification to CA-only (tls_reqcert=allow).
		if ip, ok := replNetIPs[peerPodName]; ok {
			peerHost = ip
			if tlsEnabled {
				peerTLSOpt += " tls_reqcert=allow"
			}
		}
		providerURI := fmt.Sprintf("%s://%s:%d", peerScheme, peerHost, peerPort)

		stanza := fmt.Sprintf("rid=%s provider=%s"+
			" type=refreshAndPersist"+
			" searchbase=\"%s\""+
			" scope=sub"+
			" schemachecking=off"+
			" bindmethod=simple"+
			" binddn=\"cn=replication,%s\""+
			" credentials=%s"+
			"%s%s%s%s"+
			" retry=\"%s\"",
			rid, providerURI, suffix, suffix, replPassword,
			deltaSyncOpts, peerTLSOpt, keepaliveOpt, syncreplTimeoutOpts, retryInterval)
		stanzas = append(stanzas, stanza)
	}

	// External peer stanzas: RID = ridBase + 50 + offset + 1, offset incremented
	// across all emitted external stanzas (across peers and selected URIs).
	externalOffset := int32(0)
	for _, ep := range externalPeers {
		baseTLSOpt := ""
		if ep.TLSCACertPath != "" {
			baseTLSOpt = fmt.Sprintf(" tls_cacert=%s", ep.TLSCACertPath)
			if tlsEnabled {
				baseTLSOpt += " tls_cert=/etc/openldap/tls/tls.crt tls_key=/etc/openldap/tls/tls.key"
			}
		}

		n := int32(len(ep.URIs))
		if n == 0 {
			continue
		}
		rpp := ep.ReplicasPerPeer
		if rpp < 1 {
			rpp = 1
		}
		if rpp > n {
			rpp = n
		}

		// Per-peer delta opts: omit logbase / syncdata=accesslog when the peer
		// is configured for plain syncrepl (ADR-011) — typically a legacy
		// non-slaptain source that doesn't advertise an accesslog DB.
		epDeltaOpts := externalDeltaOpts
		if ep.PlainSyncRepl {
			epDeltaOpts = ""
		}

		for k := int32(0); k < rpp; k++ {
			uri := ep.URIs[(ordinal+k)%n]
			rid := fmt.Sprintf("%03d", ridBase+50+externalOffset+1)
			externalOffset++

			epTLSOpt := baseTLSOpt
			// IP-based provider URIs won't match DNS SANs → relax hostname verification.
			if net.ParseIP(strings.Split(strings.TrimPrefix(strings.TrimPrefix(uri, "ldaps://"), "ldap://"), ":")[0]) != nil {
				epTLSOpt += " tls_reqcert=allow"
			}

			stanza := fmt.Sprintf("rid=%s provider=%s"+
				" type=refreshAndPersist"+
				" searchbase=\"%s\""+
				" scope=sub"+
				" schemachecking=off"+
				" bindmethod=simple"+
				" binddn=\"%s\""+
				" credentials=%s"+
				"%s%s%s%s"+
				" retry=\"%s\"",
				rid, uri, suffix, ep.BindDN, ep.Password,
				epDeltaOpts, epTLSOpt, keepaliveOpt, syncreplTimeoutOpts, retryInterval)
			stanzas = append(stanzas, stanza)
		}
	}

	return stanzas
}

// buildDatabaseSyncReplRO computes syncrepl stanzas for a read-only replica.
// When replNetIPs is non-nil, uses Multus IPs for RW peer addresses.
func buildDatabaseSyncReplRO(
	clusterName, headlessSvc, namespace, clusterDomain, suffix, dbName string,
	rwReplicas int32,
	replPassword string,
	tlsEnabled bool,
	ridBase int32,
	retryInterval, keepalive string,
	deltaSync bool,
	replNetIPs map[string]string,
) []string {
	var stanzas []string

	peerScheme := "ldap"
	peerPort := int32(1024)
	tlsOpt := ""
	if tlsEnabled {
		peerScheme = "ldaps"
		peerPort = 1025
		tlsOpt = " tls_cacert=/etc/openldap/tls/ca.crt"
	}

	keepaliveOpt := ""
	if keepalive != "" {
		keepaliveOpt = fmt.Sprintf(" keepalive=%s", keepalive)
	}

	// A read-only replica has no accesslog of its own — it produces no writes
	// (ADR-019 R3 / the RO StatefulSet mounts no accesslog volume). Its
	// stanzas nevertheless carry logbase, because it names the *RW provider's*
	// log, which is that provider's per-database suffix (ADR-019 R5/R9).
	deltaSyncOpts := ""
	if deltaSync {
		deltaSyncOpts = fmt.Sprintf(` logbase="%s"`, ldapv1alpha1.AccesslogSuffix(dbName)) +
			` logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"` +
			` syncdata=accesslog`
	}

	for i := int32(0); i < rwReplicas; i++ {
		rid := fmt.Sprintf("%03d", ridBase+i+1)
		peerPodName := fmt.Sprintf("%s-%d", clusterName, i)
		peerHost := fmt.Sprintf("%s.%s.%s.svc.%s", peerPodName, headlessSvc, namespace, clusterDomain)
		peerTLSOpt := tlsOpt
		if ip, ok := replNetIPs[peerPodName]; ok {
			peerHost = ip
			if tlsEnabled {
				peerTLSOpt += " tls_reqcert=allow"
			}
		}
		providerURI := fmt.Sprintf("%s://%s:%d", peerScheme, peerHost, peerPort)

		stanza := fmt.Sprintf("rid=%s provider=%s"+
			" type=refreshAndPersist"+
			" searchbase=\"%s\""+
			" scope=sub"+
			" schemachecking=off"+
			" bindmethod=simple"+
			" binddn=\"cn=replication,%s\""+
			" credentials=%s"+
			"%s%s%s%s"+
			" retry=\"%s\"",
			rid, providerURI, suffix, suffix, replPassword,
			deltaSyncOpts, peerTLSOpt, keepaliveOpt, syncreplTimeoutOpts, retryInterval)
		stanzas = append(stanzas, stanza)
	}

	return stanzas
}

// applySyncreplToPod applies syncrepl + mirrormode to one pod's database entry.
func (r *SlapdDatabaseReconciler) applySyncreplToPod(
	ctx context.Context,
	host, configPW, suffix string,
	desired []string,
	mirrorMode string,
) error {
	log := logf.FromContext(ctx)

	addr := host + ":" + strconv.Itoa(int(ldapContainerPort))
	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", configPW); err != nil {
		return fmt.Errorf("bind at %s: %w", host, err)
	}

	dataDN, err := findDataDBDN(conn, suffix)
	if err != nil {
		return err
	}

	// Read current values.
	sr, err := conn.Search(ldap.NewSearchRequest(
		dataDN, ldap.ScopeBaseObject, ldap.NeverDerefAliases,
		1, 0, false, "(objectClass=*)", []string{"olcSyncRepl", "olcMultiProvider"}, nil,
	))
	if err != nil {
		return fmt.Errorf("read syncrepl at %s: %w", host, err)
	}
	if len(sr.Entries) == 0 {
		return fmt.Errorf("no entry at %s", dataDN)
	}

	currentSyncRepl := sr.Entries[0].GetEqualFoldAttributeValues("olcSyncRepl")
	currentMM := ""
	if vals := sr.Entries[0].GetEqualFoldAttributeValues("olcMultiProvider"); len(vals) > 0 {
		currentMM = vals[0]
	}

	syncreplChanged := !syncreplMatch(currentSyncRepl, desired)
	mmChanged := mirrorMode != "" && !strings.EqualFold(currentMM, mirrorMode)

	if !syncreplChanged && !mmChanged {
		log.V(1).Info("syncrepl already up-to-date", "host", host, "suffix", suffix)
		return nil
	}

	log.Info("replacing syncrepl stanzas", "host", host, "suffix", suffix, "stanzas", len(desired))
	modReq := ldap.NewModifyRequest(dataDN, nil)
	if syncreplChanged {
		modReq.Replace("olcSyncRepl", desired)
	}
	if mmChanged {
		modReq.Replace("olcMultiProvider", []string{mirrorMode})
	}
	return conn.Modify(modReq)
}

// syncreplMatch compares current and desired syncrepl stanzas (RID-based, order-insensitive).
func syncreplMatch(current, desired []string) bool {
	if len(current) != len(desired) {
		return false
	}
	currentByRID := parseSyncreplByRID(current)
	desiredByRID := parseSyncreplByRID(desired)
	if len(currentByRID) != len(desiredByRID) {
		return false
	}
	for rid, d := range desiredByRID {
		c, ok := currentByRID[rid]
		if !ok {
			return false
		}
		if normalizeSyncreplStanza(c) != normalizeSyncreplStanza(d) {
			return false
		}
	}
	return true
}

func parseSyncreplByRID(stanzas []string) map[string]string {
	result := make(map[string]string, len(stanzas))
	for _, s := range stanzas {
		bare := s
		if len(s) > 0 && s[0] == '{' {
			if idx := strings.Index(s, "}"); idx >= 0 {
				bare = s[idx+1:]
			}
		}
		bare = strings.TrimSpace(bare)
		if strings.HasPrefix(bare, "rid=") {
			parts := strings.SplitN(bare, " ", 2)
			rid := strings.TrimPrefix(parts[0], "rid=")
			result[rid] = bare
		}
	}
	return result
}

func normalizeSyncreplStanza(s string) string {
	fields := strings.Fields(s)
	sort.Strings(fields)
	return strings.Join(fields, " ")
}

// ── Deletion / Cleanup ───────────────────────────────────────────────────────

func (r *SlapdDatabaseReconciler) handleDeletion(ctx context.Context, sd *ldapv1alpha1.SlapdDatabase) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(sd, databaseFinalizer) {
		return ctrl.Result{}, nil
	}

	if sd.Spec.CleanupPolicy == ldapv1alpha1.CleanupPolicyDelete {
		log.Info("cleaning up database from cn=config", "suffix", sd.Spec.Suffix)

		sc := &ldapv1alpha1.SlapdCluster{}
		if err := r.Get(ctx, client.ObjectKey{
			Name: sd.Spec.ClusterRef, Namespace: sd.Namespace,
		}, sc); err != nil {
			if !errors.IsNotFound(err) {
				return ctrl.Result{}, err
			}
			// Cluster already gone — can't clean up, just remove finalizer.
			log.Info("cluster not found, skipping database cleanup")
		} else {
			// The cluster is still there, so the teardown is owed. A failure
			// past this point keeps the finalizer and retries with backoff —
			// ADR-005: "The finalizer is not removed until all reachable pods
			// have been processed. If a pod is permanently unreachable, the
			// finalizer blocks CR deletion — this is intentional [...] An admin
			// can manually remove the finalizer to force deletion." Removing it
			// on a failed teardown was the silent half of this defect: the CR
			// vanished and the database stayed served on every pod, which is
			// neither of the two policies the user can choose between.
			configPW, err := r.getClusterConfigPassword(ctx, sc)
			if err != nil {
				return ctrl.Result{}, fmt.Errorf(
					"read config password for cleanup (remove the %s finalizer by hand to force deletion): %w",
					databaseFinalizer, err)
			}
			if err := r.deleteFromAllPods(ctx, sc, sd, configPW); err != nil {
				return ctrl.Result{}, fmt.Errorf(
					"delete database from pods (remove the %s finalizer by hand to force deletion): %w",
					databaseFinalizer, err)
			}
		}
	} else {
		log.Info("cleanup policy is Retain, leaving database in slapd", "suffix", sd.Spec.Suffix)
	}

	controllerutil.RemoveFinalizer(sd, databaseFinalizer)
	if err := r.Update(ctx, sd); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// deleteFromAllPods removes the database from every pod's cn=config (ADR-002:
// cn=config is node-local, so this is once per pod — RO replicas included, see
// databasePodHosts).
//
// Every pod is attempted even after one fails, so a single unreachable pod does
// not hide the state of the others; the joined error is returned so the caller
// keeps the finalizer and retries.
func (r *SlapdDatabaseReconciler) deleteFromAllPods(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	configPW string,
) error {
	log := logf.FromContext(ctx)

	var errs []error
	for _, pod := range databasePodHosts(sc, r.ClusterDomain) {
		addr := fmt.Sprintf("%s:%d", pod.Host, ldapContainerPort)
		if err := r.deleteDatabaseFromPod(ctx, addr, configPW, sd); err != nil {
			log.Info("failed to delete database from pod (will retry)",
				"pod", pod.Name, "err", err)
			errs = append(errs, fmt.Errorf("pod %s: %w", pod.Name, err))
		}
	}
	return kerrors.NewAggregate(errs)
}

// deleteDatabaseFromPod removes one database and its accesslog DB from one
// pod's cn=config.
//
// Order is load-bearing, and it is the data database first. The accesslog
// overlay on the data DB carries olcAccessLogDB naming the log — deleting the
// log first would leave that reference dangling, and slapd validates
// olcAccessLogDB online but resolves it offline at accesslog_db_open, so the
// pod would keep serving and then refuse to start (ADR-026, the exact state
// that withdrew the ADR-019 R8 migration). Taking the data DB first means the
// reference dies with its referrer: a teardown interrupted between the two
// steps leaves an unreferenced log DB, which is inert and reaped on the retry.
//
// Deleting the data DB renumbers every olcDatabase={N} ordered after it
// (bconfig.c's "renumber siblings" loop) — on t3e the log sat at {2} above its
// data DB at {1} and would slide to {1}. ADR-026 R1 therefore forbids carrying
// a DN across step 1, and removeAccesslogDB satisfies that by construction: it
// resolves the log by its olcSuffix with its own search. Nothing else survives
// the call — the connection is closed on return and each pod gets a fresh one.
//
// Idempotent: a database already absent is success, so a retry after a partial
// teardown converges.
func (r *SlapdDatabaseReconciler) deleteDatabaseFromPod(
	ctx context.Context,
	addr, configPW string,
	sd *ldapv1alpha1.SlapdDatabase,
) error {
	log := logf.FromContext(ctx)

	conn, err := ldap.DialURL("ldap://"+addr,
		ldap.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}),
	)
	if err != nil {
		return err
	}
	defer conn.Close()
	conn.SetTimeout(ldapRequestTimeout)

	if err := conn.Bind("cn=admin,cn=config", configPW); err != nil {
		return err
	}

	// 1. The data DB, children first — slapd does not cascade.
	dataDN, err := findDataDBDNOptional(conn, sd.Spec.Suffix)
	if err != nil {
		return err
	}
	if dataDN != "" {
		sr, err := conn.Search(ldap.NewSearchRequest(
			dataDN, ldap.ScopeWholeSubtree, ldap.NeverDerefAliases,
			0, 0, false, "(objectClass=*)", []string{"dn"}, nil,
		))
		if err != nil {
			return fmt.Errorf("search subtree of %s: %w", dataDN, err)
		}
		dns := make([]string, 0, len(sr.Entries))
		for _, e := range sr.Entries {
			dns = append(dns, e.DN)
		}
		for _, dn := range planSubtreeDeletion(dns) {
			log.Info("removing database entry", "host", addr, "dn", dn)
			if err := conn.Del(ldap.NewDelRequest(dn, nil)); err != nil {
				if !ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchObject) {
					return fmt.Errorf("delete %s: %w", dn, err)
				}
			}
		}
	}

	// 2. This database's own accesslog DB (ADR-019: one log per data DB, so it
	// has exactly one referent and the CR being finalized is it). Authorised by
	// the operator's own evidence — the log's suffix is derived from the CR
	// name, not counted off other databases' overlays — so ADR-026 R2 is
	// satisfied where the withdrawn R8 reference count was not. Skipped on RO
	// pods by nature: they have no log, and removeAccesslogDB finds nothing.
	if _, err := r.removeAccesslogDB(ctx, conn, addr, sd.Name); err != nil {
		return err
	}
	return nil
}

// ── Shared Helpers ───────────────────────────────────────────────────────────

// findDataDBDN searches cn=config for the MDB database entry matching the
// suffix. Absence is an error: every caller but the teardown path needs the
// database to exist.
func findDataDBDN(conn *ldap.Conn, suffix string) (string, error) {
	dn, err := findDataDBDNOptional(conn, suffix)
	if err != nil {
		return "", err
	}
	if dn == "" {
		return "", fmt.Errorf("no database entry with olcSuffix=%s in cn=config", suffix)
	}
	return dn, nil
}

// findDataDBDNOptional is findDataDBDN for callers that treat absence as a
// legitimate answer rather than a failure — the teardown path, where a database
// already gone is the goal state and must not be confused with a search that
// could not be performed. Returns ("", nil) when no such database exists.
func findDataDBDNOptional(conn *ldap.Conn, suffix string) (string, error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false,
		fmt.Sprintf("(olcSuffix=%s)", ldap.EscapeFilter(suffix)),
		[]string{"dn"}, nil,
	))
	if err != nil {
		return "", fmt.Errorf("search cn=config for olcSuffix=%s: %w", suffix, err)
	}
	if len(sr.Entries) == 0 {
		return "", nil
	}
	return sr.Entries[0].DN, nil
}

// sanitizeSuffix converts an LDAP suffix into a filesystem-safe directory name.
// e.g. "o=myapp" → "myapp", "dc=example,dc=org" → "example".
func sanitizeSuffix(suffix string) string {
	parts := strings.SplitN(suffix, ",", 2)
	kv := strings.SplitN(parts[0], "=", 2)
	if len(kv) == 2 {
		return kv[1]
	}
	return suffix
}

// generateSSHAHash returns an OpenLDAP-compatible {SSHA} password hash.
func generateSSHAHash(password string) (string, error) {
	salt := make([]byte, 8)
	if _, err := rand.Read(salt); err != nil {
		return "", fmt.Errorf("generate salt: %w", err)
	}
	h := sha1.New()
	h.Write([]byte(password))
	h.Write(salt)
	digest := h.Sum(nil)
	combined := append(digest, salt...)
	return "{SSHA}" + base64.StdEncoding.EncodeToString(combined), nil
}

// ── Status ───────────────────────────────────────────────────────────────────

func (r *SlapdDatabaseReconciler) setStatus(
	ctx context.Context,
	sd *ldapv1alpha1.SlapdDatabase,
	phase ldapv1alpha1.SlapdDatabasePhase,
	appliedPods, failedPods []string,
	reason, message string,
) {
	sort.Strings(appliedPods)
	sort.Strings(failedPods)

	sd.Status.Phase = phase
	sd.Status.AppliedToPods = appliedPods
	sd.Status.FailedPods = failedPods
	sd.Status.ObservedGeneration = sd.Generation

	condStatus := metav1.ConditionFalse
	if phase == ldapv1alpha1.DatabasePhaseRunning {
		condStatus = metav1.ConditionTrue
	}
	setCondition(&sd.Status.Conditions, metav1.Condition{
		Type:               "Ready",
		Status:             condStatus,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sd.Generation,
	})

	statusPatch := &ldapv1alpha1.SlapdDatabase{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "ldap.chuck-chuck-chuck.net/v1alpha1",
			Kind:       "SlapdDatabase",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      sd.Name,
			Namespace: sd.Namespace,
		},
	}
	statusPatch.Status = sd.Status
	ac, err := applyConfiguration(statusPatch)
	if err != nil {
		logf.FromContext(ctx).Error(err, "failed to build SlapdDatabase status apply configuration")
		return
	}
	if err := r.Status().Apply(ctx, ac, client.ForceOwnership, client.FieldOwner(databaseFieldManager)); err != nil {
		logf.FromContext(ctx).Error(err, "failed to patch SlapdDatabase status")
	}
}

// SetupWithManager sets up the controller with the Manager.
// Watches SlapdCluster CRs so that changes to externalPeers (which live on
// the SlapdCluster spec) trigger reconciliation of all SlapdDatabases that
// reference that cluster. Without this, removing or adding external peers
// would not update syncrepl stanzas until the SlapdDatabase itself changed.
func (r *SlapdDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdDatabase{}).
		Watches(&ldapv1alpha1.SlapdCluster{}, handler.EnqueueRequestsFromMapFunc(
			func(ctx context.Context, obj client.Object) []ctrl.Request {
				sc, ok := obj.(*ldapv1alpha1.SlapdCluster)
				if !ok {
					return nil
				}
				// Find all SlapdDatabases that reference this cluster.
				var dbList ldapv1alpha1.SlapdDatabaseList
				if err := r.List(ctx, &dbList, client.InNamespace(sc.Namespace)); err != nil {
					return nil
				}
				var reqs []ctrl.Request
				for _, db := range dbList.Items {
					if db.Spec.ClusterRef == sc.Name {
						reqs = append(reqs, ctrl.Request{
							NamespacedName: client.ObjectKey{
								Name:      db.Name,
								Namespace: db.Namespace,
							},
						})
					}
				}
				return reqs
			},
		)).
		Named("slapddatabase").
		Complete(r)
}

// ── Overlay tunables: purge window and checkpoint (ADR-024 R4/R5) ───────────

// defaultAccesslogPurge is the purge window the operator applies when
// spec.replication.accesslogPurge is unset: keep 7 days of change journal,
// sweep once a day.
//
// Unset used to mean NO purge, which is not a neutral default — it is a
// slow-motion outage. The journal grows until it hits its map ceiling, and
// because the accesslog overlay sits in the DATA database's write path, the
// moment the journal cannot take a write neither can the data. The window is
// sized to survive a weekend-plus consumer outage: a consumer offline longer
// than the window loses its delta anchor and needs a full refresh, so the
// window trades journal bytes for how long a peer may stay away.
const defaultAccesslogPurge = "7+00:00 1+00:00"

// noneSentinel is the explicit opt-out spelling shared by the string-valued
// tunables that the operator defaults (keepalive, accesslogPurge): "" means
// "no opinion, use the operator default", "none" means "I really want none".
const noneSentinel = "none"

// desiredAccesslogPurge resolves spec.replication.accesslogPurge: unset gets
// the operator's window (see defaultAccesslogPurge for why "nothing" is not an
// option), "none" is the explicit way to ask for no purging at all, and any
// other value is passed through verbatim.
func desiredAccesslogPurge(sd *ldapv1alpha1.SlapdDatabase) string {
	if sd == nil {
		return defaultAccesslogPurge
	}
	v := strings.TrimSpace(sd.Spec.Replication.AccesslogPurge)
	switch {
	case v == "":
		return defaultAccesslogPurge
	case strings.EqualFold(v, noneSentinel):
		return ""
	default:
		return v
	}
}

// desiredSyncprovCheckpoint resolves spec.replication.syncprovCheckpoint.
// Unlike the purge window this has NO operator default: an unwritten
// olcSpCheckpoint means slapd's own contextCSN checkpointing behaviour, which
// is a performance trade rather than a correctness cliff. Unset and the "none"
// sentinel both mean "no attribute", which the convergence step removes if one
// is present.
func desiredSyncprovCheckpoint(sd *ldapv1alpha1.SlapdDatabase) string {
	if sd == nil {
		return ""
	}
	v := strings.TrimSpace(sd.Spec.Replication.SyncprovCheckpoint)
	if strings.EqualFold(v, noneSentinel) {
		return ""
	}
	return v
}

// overlayAttrAction is what planOverlayAttr decided to do with a single-valued
// string attribute on an overlay entry.
type overlayAttrAction int

const (
	overlayAttrNoop overlayAttrAction = iota
	overlayAttrSet
	overlayAttrRemove
)

// planOverlayAttr compares an overlay attribute's live values against the
// desired one and returns the single write needed, if any. Pure — the LDAP half
// only executes the verdict. Generalises planSessionlog (ADR-022) to the
// string-valued overlay tunables: olcAccessLogPurge and olcSpCheckpoint.
//
// An empty want means the attribute should not be present. Comparison
// normalises whitespace on both sides, because slapd echoes these values back
// in its own spacing and a respacing is not a difference worth a write. More
// than one live value is by definition not the single desired value, so it is
// replaced.
func planOverlayAttr(current []string, want string) (overlayAttrAction, string) {
	want = normalizeOverlayAttrValue(want)

	var live []string
	for _, v := range current {
		if n := normalizeOverlayAttrValue(v); n != "" {
			live = append(live, n)
		}
	}

	if want == "" {
		if len(live) == 0 {
			return overlayAttrNoop, ""
		}
		return overlayAttrRemove, ""
	}
	if len(live) == 1 && live[0] == want {
		return overlayAttrNoop, ""
	}
	return overlayAttrSet, want
}

// normalizeOverlayAttrValue collapses leading, trailing and internal whitespace
// runs to single spaces, so "500\t15" and " 500  15 " compare equal to "500 15".
func normalizeOverlayAttrValue(v string) string {
	return strings.Join(strings.Fields(v), " ")
}

// ensureOverlayAttr aligns one single-valued attribute on one overlay entry to
// what planOverlayAttr wants: observe, compare, write only on a difference.
//
// The overlay's real DN carries a {N} ordering prefix assigned by slapd, so the
// caller passes the DN it read back rather than one it reconstructed. Both
// attributes handled here were verified live-modifiable on a running slapd
// before being converged at all — the precondition ADR-024's amendments made
// mandatory for this class, after olcDbMaxSize was found to segfault and
// olcIdleTimeout to hang on exactly this kind of write.
func ensureOverlayAttr(
	ctx context.Context,
	conn *ldap.Conn,
	host, overlayDN, attr string,
	current []string,
	want string,
) error {
	log := logf.FromContext(ctx)

	action, value := planOverlayAttr(current, want)
	if action == overlayAttrNoop {
		return nil
	}

	modReq := ldap.NewModifyRequest(overlayDN, nil)
	switch action {
	case overlayAttrSet:
		log.Info("setting overlay attribute",
			"host", host, "dn", overlayDN, "attr", attr, "value", value)
		modReq.Replace(attr, []string{value})
	case overlayAttrRemove:
		log.Info("removing overlay attribute",
			"host", host, "dn", overlayDN, "attr", attr)
		modReq.Delete(attr, nil)
	case overlayAttrNoop:
		return nil
	}

	if err := conn.Modify(modReq); err != nil {
		if action == overlayAttrRemove && ldap.IsErrorWithCode(err, ldap.LDAPResultNoSuchAttribute) {
			return nil
		}
		return fmt.Errorf("set %s on %s: %w", attr, overlayDN, err)
	}
	return nil
}
