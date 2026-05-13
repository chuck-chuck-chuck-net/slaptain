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
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
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

	// RW pods.
	for i := int32(0); i < replicas; i++ {
		podName := fmt.Sprintf("%s-%d", sc.Name, i)
		host := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
			podName, headlessSvc, sc.Namespace)
		if err := r.reconcilePodDatabase(ctx, host, configPW, rootPW, sd, sc, false); err != nil {
			log.Info("database reconcile skipped for pod (will retry)",
				"pod", podName, "err", err)
			failedPods = append(failedPods, podName)
		} else {
			appliedPods = append(appliedPods, podName)
		}
	}

	// RO pods.
	if sc.Spec.ReadReplicas > 0 {
		roHeadless := sc.Name + "-readonly-headless"
		for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
			podName := fmt.Sprintf("%s-readonly-%d", sc.Name, i)
			host := fmt.Sprintf("%s.%s.%s.svc.cluster.local",
				podName, roHeadless, sc.Namespace)
			if err := r.reconcilePodDatabase(ctx, host, configPW, rootPW, sd, sc, true); err != nil {
				log.Info("database reconcile skipped for read-only pod (will retry)",
					"pod", podName, "err", err)
				failedPods = append(failedPods, podName)
			} else {
				appliedPods = append(appliedPods, podName)
			}
		}
	}

	// Track whether any step was incomplete — forces requeue even if database
	// creation succeeded on all pods. Without this, the operator would stop
	// reconciling before seed/replication are applied (see reconcile-loop-fixes.md:
	// "Operator stops reconciling before all pods are configured").
	pendingWork := len(failedPods) > 0

	// 8. Seed initial data. Re-applied if the root entry is missing (e.g. after
	//    a pod restart that wiped the data directory and re-ran the init container).
	if sd.Spec.Seed != nil && len(sd.Spec.Seed.Entries) > 0 {
		needsSeed := !sd.Status.SeedApplied
		if sd.Status.SeedApplied {
			// Verify the seed is still present — a StatefulSet rolling restart
			// (triggered by DATABASE_DIRS update) re-runs the init container,
			// which re-creates cn=config from scratch. The new pod gets the
			// database re-created by reconcilePodDatabase above, but seed data
			// is only in the old (now-gone) LMDB files.
			needsSeed = !r.verifySeedExists(ctx, sc, sd, rootPW)
		}
		if needsSeed {
			if err := r.applySeedData(ctx, sc, sd, rootPW); err != nil {
				log.Info("seed data not yet applied (will retry)", "err", err)
				pendingWork = true
				sd.Status.SeedApplied = false
			} else {
				sd.Status.SeedApplied = true
			}
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
	if sc.Spec.Replication.Enabled && sd.Spec.Replication != nil && !sc.IsConsumerOnly() {
		if err := r.ensureReplicationUser(ctx, sc, sd, rootPW); err != nil {
			log.Info("replication user not yet created (will retry)", "err", err)
			pendingWork = true
		}
	}

	// 10. Configure replication stanzas if replication is enabled.
	if sc.Spec.Replication.Enabled && sd.Spec.Replication != nil {
		skipped, err := r.reconcileReplication(ctx, sc, sd, configPW)
		if err != nil {
			return ctrl.Result{}, fmt.Errorf("reconcileReplication: %w", err)
		}
		if skipped {
			pendingWork = true
		}
	}

	// 10. Update status.
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
	//                    the per-DB DeltaSync flag. Requires the cluster-
	//                    shared cn=accesslog DB.
	//
	// Order of operations matters for transitions:
	//   adding   — accesslog DB before accesslog overlay (overlay references
	//              cn=accesslog via olcAccessLogDB; slapd validates at add).
	//   removing — accesslog overlay before accesslog DB (same reference,
	//              same validation, reverse direction).
	// syncprov has no dependencies; ordered freely relative to the others.
	//
	// RO StatefulSet pods (readOnly=true) never carry any of this — they are
	// consumers only, not providers; cluster-shared DBs were created on the
	// RW path.
	wantsSyncProv := sc.Spec.Replication.Enabled && !readOnly && !sc.IsConsumerOnly()
	wantsAccesslog := wantsSyncProv && sd.DeltaSyncEnabled()

	if !readOnly {
		if !wantsAccesslog {
			if err := r.removeDataDBOverlay(ctx, conn, host, dataDN, "accesslog"); err != nil {
				return fmt.Errorf("remove accesslog overlay at %s: %w", host, err)
			}
			if err := r.removeAccesslogDB(ctx, conn, host); err != nil {
				return fmt.Errorf("remove accesslog DB at %s: %w", host, err)
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
			if err := r.ensureAccesslogDB(ctx, conn, host); err != nil {
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

	if sd.Spec.MaxSize != "" {
		addReq.Attribute("olcDbMaxSize", []string{sd.Spec.MaxSize})
	}

	if sd.Spec.NoSync {
		addReq.Attribute("olcDbNoSync", []string{"TRUE"})
	}

	// Default index on objectClass.
	addReq.Attribute("olcDbIndex", []string{"objectClass eq"})

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

	// Build set of current indices (normalized).
	currentSet := make(map[string]bool, len(current))
	for _, idx := range current {
		currentSet[normalizeIndex(idx)] = true
	}

	// Find missing indices.
	var missing []string
	for _, idx := range desired {
		if !currentSet[normalizeIndex(idx)] {
			missing = append(missing, idx)
		}
	}

	if len(missing) == 0 {
		log.V(1).Info("indices already up-to-date", "host", host)
		return nil
	}

	log.Info("adding indices", "host", host, "count", len(missing))
	modReq := ldap.NewModifyRequest(dataDN, nil)
	modReq.Add("olcDbIndex", missing)
	return conn.Modify(modReq)
}

func normalizeIndex(s string) string {
	return strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
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
// Idempotent: checks for existing overlay before adding.
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
	if hasSyncprov {
		return nil
	}

	log.Info("adding syncprov overlay to data database", "host", host, "dataDN", dataDN)
	syncprovDN := "olcOverlay=syncprov," + dataDN
	addReq := ldap.NewAddRequest(syncprovDN, nil)
	addReq.Attribute("objectClass", []string{"olcOverlayConfig", "olcSyncProvConfig"})
	addReq.Attribute("olcOverlay", []string{"syncprov"})
	if sd.Spec.Replication != nil && sd.Spec.Replication.SyncprovCheckpoint != "" {
		addReq.Attribute("olcSpCheckpoint", []string{sd.Spec.Replication.SyncprovCheckpoint})
	}
	if err := conn.Add(addReq); err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return fmt.Errorf("add syncprov overlay: %w", err)
		}
	}
	return nil
}

// ensureAccesslogOverlay adds the accesslog overlay to the data DB's
// cn=config entry. Required for **delta-syncrepl** specifically: the overlay
// captures every write into the cluster-shared cn=accesslog DB, where
// consumers pull change deltas instead of re-walking the full DIT on
// reconnect. Strictly an add-on to ensureSyncProvOverlay — the cluster must
// already be a syncrepl provider for the change journal to be useful.
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
	if hasAccesslog {
		return nil
	}

	log.Info("adding accesslog overlay to data database", "host", host, "dataDN", dataDN)
	accesslogDN := "olcOverlay=accesslog," + dataDN
	addReq := ldap.NewAddRequest(accesslogDN, nil)
	addReq.Attribute("objectClass", []string{"olcOverlayConfig", "olcAccessLogConfig"})
	addReq.Attribute("olcOverlay", []string{"accesslog"})
	addReq.Attribute("olcAccessLogDB", []string{"cn=accesslog"})
	addReq.Attribute("olcAccessLogOps", []string{"writes"})
	addReq.Attribute("olcAccessLogSuccess", []string{"TRUE"})
	if sd.Spec.Replication != nil && sd.Spec.Replication.AccesslogPurge != "" {
		addReq.Attribute("olcAccessLogPurge", []string{sd.Spec.Replication.AccesslogPurge})
	}
	if err := conn.Add(addReq); err != nil {
		if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
			return fmt.Errorf("add accesslog overlay: %w", err)
		}
	}
	return nil
}

// ensureAccesslogDB creates the cluster-shared cn=accesslog database in
// cn=config when missing. This is the runtime counterpart to the accesslog
// configuration the init container used to write at first boot (ADR-010 3e) —
// moving it to the operator lets consumer-only → peer promotion happen
// without a rolling restart, because the underlying /accesslog volume and
// modules are always provisioned on peer-eligible pods (see
// SlapdCluster.NeedsAccesslogVolume).
//
// Adds two cn=config entries:
//
//	olcDatabase=mdb cn=accesslog       — the accesslog DB itself, backed by
//	                                     /accesslog, indexed for replog ops
//	olcOverlay=syncprov on accesslog DB — exposes the change journal to
//	                                     consumers via syncrepl
//
// Idempotent: each ldap.Add silently treats EntryAlreadyExists as success.
// Called per-pod; the accesslog DB is shared across all SlapdDatabase CRs,
// so multiple SlapdDatabase reconciles converge on the same DB.
func (r *SlapdDatabaseReconciler) ensureAccesslogDB(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
) error {
	log := logf.FromContext(ctx)

	// Check whether the accesslog DB already exists. Without this we'd ldapadd
	// every reconcile and rely on EntryAlreadyExists — works, but pollutes the
	// debug log. A single scope-children search is cheap.
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(&(objectClass=olcMdbConfig)(olcSuffix=cn=accesslog))",
		[]string{"dn"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search for accesslog DB: %w", err)
	}

	var dbDN string
	if len(sr.Entries) > 0 {
		dbDN = sr.Entries[0].DN
	} else {
		log.Info("creating accesslog DB", "host", host)
		addReq := ldap.NewAddRequest("olcDatabase=mdb,cn=config", nil)
		addReq.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMdbConfig"})
		addReq.Attribute("olcDatabase", []string{"mdb"})
		addReq.Attribute("olcSuffix", []string{"cn=accesslog"})
		addReq.Attribute("olcRootDN", []string{"cn=admin,cn=config"})
		addReq.Attribute("olcDbDirectory", []string{"/accesslog"})
		addReq.Attribute("olcDbIndex", []string{
			"default eq",
			"reqEnd,reqResult,reqStart eq",
		})
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add accesslog DB: %w", err)
			}
		}
		// Re-find to learn the assigned {N} prefix.
		sr2, err := conn.Search(ldap.NewSearchRequest(
			"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
			1, 0, false, "(&(objectClass=olcMdbConfig)(olcSuffix=cn=accesslog))",
			[]string{"dn"}, nil,
		))
		if err != nil || len(sr2.Entries) == 0 {
			return fmt.Errorf("re-find accesslog DB after add: %w", err)
		}
		dbDN = sr2.Entries[0].DN
	}

	// Ensure syncprov overlay on the accesslog DB. Without it consumers can't
	// pull the change journal — the whole point of the DB is to be syncrepl-
	// provisionable.
	overlaySR, err := conn.Search(ldap.NewSearchRequest(
		dbDN, ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcSyncProvConfig)", []string{"dn"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search syncprov overlay on %s: %w", dbDN, err)
	}
	if len(overlaySR.Entries) == 0 {
		log.Info("adding syncprov overlay to accesslog DB", "host", host)
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

// removeAccesslogDB tears down the accesslog DB during peer → consumer-only
// demotion (ADR-010 3e). Removes the syncprov overlay first (children must go
// before parents in slapd's cn=config), then the DB entry itself.
//
// The underlying /accesslog volume is left mounted and the LMDB data files
// stay on disk — they're harmless when the DB entry isn't referenced. A
// later promotion re-creates the DB pointed at the same directory, and slapd
// re-attaches to whatever is there. (This is acceptable for the migration
// rollback use case; if you want a clean accesslog history on re-promotion,
// wipe /accesslog manually before promoting.)
//
// Idempotent: NoSuchObject is silently treated as success.
func (r *SlapdDatabaseReconciler) removeAccesslogDB(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
) error {
	log := logf.FromContext(ctx)

	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		1, 0, false, "(&(objectClass=olcMdbConfig)(olcSuffix=cn=accesslog))",
		[]string{"dn"}, nil,
	))
	if err != nil {
		return fmt.Errorf("search accesslog DB: %w", err)
	}
	if len(sr.Entries) == 0 {
		return nil
	}
	dbDN := sr.Entries[0].DN

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
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)
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

// verifySeedExists checks whether the first seed entry (typically the root DN)
// exists on any reachable RW pod. Returns false if the entry is missing, indicating
// that seed data needs to be re-applied (e.g. after a StatefulSet rolling restart).
func (r *SlapdDatabaseReconciler) verifySeedExists(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	rootPW string,
) bool {
	if len(sd.Spec.Seed.Entries) == 0 {
		return true
	}

	// Parse the DN of the first seed entry.
	firstEntry := strings.TrimSpace(sd.Spec.Seed.Entries[0])
	lines := strings.SplitN(firstEntry, "\n", 2)
	if len(lines) == 0 || !strings.HasPrefix(strings.ToLower(lines[0]), "dn:") {
		return true // Can't parse — assume exists to avoid infinite loop.
	}
	dn := strings.TrimSpace(lines[0][3:])

	rootDN := sd.Spec.RootDN
	if rootDN == "" {
		rootDN = "cn=admin," + sd.Spec.Suffix
	}

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"

	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)
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

		exists, err := ldapEntryExists(conn, dn)
		conn.Close()
		if err != nil {
			continue
		}
		return exists
	}

	return false // No pod reachable — assume missing.
}

// applySeedData connects to the first reachable RW pod and applies seed entries.
func (r *SlapdDatabaseReconciler) applySeedData(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	rootPW string,
) error {
	log := logf.FromContext(ctx)

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"

	rootDN := sd.Spec.RootDN
	if rootDN == "" {
		rootDN = "cn=admin," + sd.Spec.Suffix
	}

	// Try each RW pod until one works.
	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)
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

		// Apply each seed entry.
		allOK := true
		for _, entry := range sd.Spec.Seed.Entries {
			if err := r.applySeedEntry(conn, entry); err != nil {
				log.Info("seed entry failed", "host", host, "err", err)
				allOK = false
			}
		}
		conn.Close()

		if allOK {
			log.Info("seed data applied", "host", host, "entries", len(sd.Spec.Seed.Entries))
			return nil
		}
		return fmt.Errorf("some seed entries failed on %s", host)
	}

	return fmt.Errorf("no reachable RW pod for seed data")
}

// applySeedEntry parses a simplified LDIF entry and adds it via LDAP.
// Format: first line is "dn: <dn>", remaining lines are "attr: value".
// Blank lines are ignored.
func (r *SlapdDatabaseReconciler) applySeedEntry(conn *ldap.Conn, entry string) error {
	lines := strings.Split(strings.TrimSpace(entry), "\n")
	if len(lines) == 0 {
		return nil
	}

	// Parse dn.
	if !strings.HasPrefix(strings.ToLower(lines[0]), "dn:") {
		return fmt.Errorf("seed entry must start with 'dn:', got %q", lines[0])
	}
	dn := strings.TrimSpace(lines[0][3:])

	// Check if entry already exists (idempotent).
	exists, err := ldapEntryExists(conn, dn)
	if err != nil {
		return fmt.Errorf("check existence of %s: %w", dn, err)
	}
	if exists {
		return nil
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
			return nil
		}
		return fmt.Errorf("add %s: %w", dn, err)
	}
	return nil
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
func (r *SlapdDatabaseReconciler) reconcileReplication(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	configPW string,
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
				BindDN:          ep.BindDN,
				Password:        password,
				TLSCACertPath:   tlsCACertPath,
				PlainSyncRepl:   ep.SyncMode == "plain",
			})
		} else {
			externalPeers = append(externalPeers, resolvedExternalPeer{
				Name:            ep.Name,
				URIs:            []string{ep.URI},
				ReplicasPerPeer: 1,
				BindDN:          ep.BindDN,
				Password:        password,
				TLSCACertPath:   tlsCACertPath,
				PlainSyncRepl:   ep.SyncMode == "plain",
			})
		}
	}

	skipped := false
	ridBase := int32(0)
	if sd.Spec.Replication != nil {
		ridBase = sd.Spec.Replication.RIDBase
	}

	retryInterval := sc.Spec.Replication.Retry
	if retryInterval == "" {
		retryInterval = "10 +"
	}
	keepalive := sc.Spec.Replication.Keepalive

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
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local",
			sc.Name, i, headlessSvc, sc.Namespace)

		desired := buildDatabaseSyncRepl(
			sc.Name, headlessSvc, sc.Namespace, sd.Spec.Suffix,
			replicas, i, replPassword,
			tlsEnabled, ridBase,
			retryInterval, keepalive,
			useDeltaSync,
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
			host := fmt.Sprintf("%s-readonly-%d.%s.%s.svc.cluster.local",
				sc.Name, i, roHeadless, sc.Namespace)

			desired := buildDatabaseSyncReplRO(
				sc.Name, headlessSvc, sc.Namespace, sd.Spec.Suffix,
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
	clusterName, headlessSvc, namespace, suffix string,
	replicas, ordinal int32,
	replPassword string,
	tlsEnabled bool,
	ridBase int32,
	retryInterval, keepalive string,
	deltaSync bool,
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

	deltaSyncOpts := ""
	if deltaSync {
		deltaSyncOpts = ` logbase="cn=accesslog"` +
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
		peerHost := fmt.Sprintf("%s.%s.%s.svc.cluster.local", peerPodName, headlessSvc, namespace)
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
			"%s%s%s"+
			" retry=\"%s\"",
			rid, providerURI, suffix, suffix, replPassword,
			deltaSyncOpts, peerTLSOpt, keepaliveOpt, retryInterval)
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
		epDeltaOpts := deltaSyncOpts
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
				"%s%s%s"+
				" retry=\"%s\"",
				rid, uri, suffix, ep.BindDN, ep.Password,
				epDeltaOpts, epTLSOpt, keepaliveOpt, retryInterval)
			stanzas = append(stanzas, stanza)
		}
	}

	return stanzas
}

// buildDatabaseSyncReplRO computes syncrepl stanzas for a read-only replica.
// When replNetIPs is non-nil, uses Multus IPs for RW peer addresses.
func buildDatabaseSyncReplRO(
	clusterName, headlessSvc, namespace, suffix string,
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

	deltaSyncOpts := ""
	if deltaSync {
		deltaSyncOpts = ` logbase="cn=accesslog"` +
			` logfilter="(&(objectClass=auditWriteObject)(reqResult=0))"` +
			` syncdata=accesslog`
	}

	for i := int32(0); i < rwReplicas; i++ {
		rid := fmt.Sprintf("%03d", ridBase+i+1)
		peerPodName := fmt.Sprintf("%s-%d", clusterName, i)
		peerHost := fmt.Sprintf("%s.%s.%s.svc.cluster.local", peerPodName, headlessSvc, namespace)
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
			"%s%s%s"+
			" retry=\"%s\"",
			rid, providerURI, suffix, suffix, replPassword,
			deltaSyncOpts, peerTLSOpt, keepaliveOpt, retryInterval)
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
			configPW, err := r.getClusterConfigPassword(ctx, sc)
			if err != nil {
				log.Info("cannot read config password for cleanup, removing finalizer anyway", "err", err)
			} else {
				r.deleteFromAllPods(ctx, sc, sd, configPW)
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

// deleteFromAllPods removes the olcDatabase entry from each reachable pod's cn=config.
func (r *SlapdDatabaseReconciler) deleteFromAllPods(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	sd *ldapv1alpha1.SlapdDatabase,
	configPW string,
) {
	log := logf.FromContext(ctx)

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}
	headlessSvc := sc.Name + "-headless"

	for i := int32(0); i < replicas; i++ {
		host := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local:%d",
			sc.Name, i, headlessSvc, sc.Namespace, ldapContainerPort)
		if err := r.deleteDatabaseFromPod(host, configPW, sd.Spec.Suffix); err != nil {
			log.Info("failed to delete database from pod (best effort)", "ordinal", i, "err", err)
		}
	}
}

func (r *SlapdDatabaseReconciler) deleteDatabaseFromPod(addr, configPW, suffix string) error {
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

	dataDN, err := findDataDBDN(conn, suffix)
	if err != nil {
		return err // Database doesn't exist, nothing to delete.
	}

	return conn.Del(ldap.NewDelRequest(dataDN, nil))
}

// ── Shared Helpers ───────────────────────────────────────────────────────────

// findDataDBDN searches cn=config for the MDB database entry matching the suffix.
func findDataDBDN(conn *ldap.Conn, suffix string) (string, error) {
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
		return "", fmt.Errorf("no database entry with olcSuffix=%s in cn=config", suffix)
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
	if err := r.Status().Patch(ctx, statusPatch, client.Apply, client.ForceOwnership, client.FieldOwner(databaseFieldManager)); err != nil {
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
