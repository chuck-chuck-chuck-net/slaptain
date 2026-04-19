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

	// 4. Wait for cluster to be Running.
	if sc.Status.Phase != ldapv1alpha1.PhaseRunning {
		log.Info("waiting for cluster to be Running", "phase", sc.Status.Phase)
		r.setStatus(ctx, sd, ldapv1alpha1.DatabasePhasePending, nil, nil,
			"ClusterNotReady", fmt.Sprintf("SlapdCluster %q is %s", sd.Spec.ClusterRef, sc.Status.Phase))
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil
	}

	// 5. Reconcile credentials secret.
	if err := r.reconcileCredentials(ctx, sd); err != nil {
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
		if err := r.reconcilePodDatabase(ctx, host, configPW, rootPW, sd, sc); err != nil {
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
			if err := r.reconcilePodDatabase(ctx, host, configPW, rootPW, sd, sc); err != nil {
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

	// 8. Seed initial data (once, on first RW pod that's reachable).
	if !sd.Status.SeedApplied && sd.Spec.Seed != nil && len(sd.Spec.Seed.Entries) > 0 {
		if err := r.applySeedData(ctx, sc, sd, rootPW); err != nil {
			log.Info("seed data not yet applied (will retry)", "err", err)
			pendingWork = true
		} else {
			sd.Status.SeedApplied = true
		}
	}

	// 9. Configure replication stanzas if replication is enabled.
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

// reconcileCredentials creates the per-database credentials secret if needed.
func (r *SlapdDatabaseReconciler) reconcileCredentials(ctx context.Context, sd *ldapv1alpha1.SlapdDatabase) error {
	secretName := r.credentialsSecretName(sd)

	// If user-provided, nothing to do.
	if sd.Spec.Credentials.SecretName != "" {
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
		log.Info("creating database", "host", host, "suffix", sd.Spec.Suffix)
		dataDN, err = r.createDatabase(conn, sd, rootPW)
		if err != nil {
			return fmt.Errorf("create database at %s: %w", host, err)
		}
	}

	// Apply ACLs.
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

	return nil
}

// createDatabase adds a new olcDatabase={N}mdb entry to cn=config.
func (r *SlapdDatabaseReconciler) createDatabase(
	conn *ldap.Conn,
	sd *ldapv1alpha1.SlapdDatabase,
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

	// Build effective ACL list. When replication is enabled, prepend a rule
	// granting the replication bind DN read access to all attributes.
	acls := sd.Spec.ACLs
	replicationEnabled := sc.Spec.Replication.Enabled && sc.Spec.Replicas > 1
	if replicationEnabled || len(sc.Spec.Replication.ExternalPeers) > 0 {
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
	if replicas < 2 {
		return false, nil
	}

	headlessSvc := sc.Name + "-headless"
	tlsEnabled := sc.Spec.LDAP.TLS.Enabled

	replPassword, err := r.getDatabaseReplPassword(ctx, sd)
	if err != nil {
		return false, err
	}

	// Resolve external peers.
	var externalPeers []resolvedExternalPeer
	for _, ep := range sc.Spec.Replication.ExternalPeers {
		resolved := resolvedExternalPeer{
			Name:   ep.Name,
			URI:    ep.URI,
			BindDN: ep.BindDN,
		}
		if ep.TLSSecretName != "" {
			resolved.TLSCACertPath = "/etc/openldap/tls/peers/" + ep.Name + "/ca.crt"
		}
		if ep.BindPasswordSecretName != "" {
			secret := &corev1.Secret{}
			if err := r.Get(ctx, client.ObjectKey{
				Name: ep.BindPasswordSecretName, Namespace: sc.Namespace,
			}, secret); err != nil {
				log.Info("skipping external peer: cannot read bind password secret",
					"peer", ep.Name, "err", err)
				continue
			}
			resolved.Password = string(secret.Data["password"])
			if resolved.Password == "" {
				resolved.Password = string(secret.Data["replication-password"])
			}
		}
		externalPeers = append(externalPeers, resolved)
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

	useDeltaSync := sd.Spec.Replication != nil && sd.Spec.Replication.DeltaSync

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
		)

		if err := r.applySyncreplToPod(ctx, host, configPW, sd.Spec.Suffix, desired, "TRUE"); err != nil {
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

type resolvedExternalPeer struct {
	Name          string
	URI           string
	BindDN        string
	Password      string
	TLSCACertPath string
}

// buildDatabaseSyncRepl computes syncrepl stanzas for one RW pod using per-database
// ridBase. In-cluster peer i → RID = ridBase + i + 1, external peer j → ridBase + 50 + j + 1.
func buildDatabaseSyncRepl(
	clusterName, headlessSvc, namespace, suffix string,
	replicas, ordinal int32,
	replPassword string,
	tlsEnabled bool,
	ridBase int32,
	retryInterval, keepalive string,
	deltaSync bool,
	externalPeers []resolvedExternalPeer,
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

	// In-cluster stanzas.
	for i := int32(0); i < replicas; i++ {
		if i == ordinal {
			continue
		}
		rid := fmt.Sprintf("%03d", ridBase+i+1)
		peerHost := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", clusterName, i, headlessSvc, namespace)
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
			deltaSyncOpts, tlsOpt, keepaliveOpt, retryInterval)
		stanzas = append(stanzas, stanza)
	}

	// External peer stanzas: RID = ridBase + 50 + j + 1.
	for j, ep := range externalPeers {
		rid := fmt.Sprintf("%03d", ridBase+50+int32(j)+1)
		epTLSOpt := ""
		if ep.TLSCACertPath != "" {
			epTLSOpt = fmt.Sprintf(" tls_cacert=%s", ep.TLSCACertPath)
			if tlsEnabled {
				epTLSOpt += " tls_cert=/etc/openldap/tls/tls.crt tls_key=/etc/openldap/tls/tls.key"
			}
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
			rid, ep.URI, suffix, ep.BindDN, ep.Password,
			deltaSyncOpts, epTLSOpt, keepaliveOpt, retryInterval)
		stanzas = append(stanzas, stanza)
	}

	return stanzas
}

// buildDatabaseSyncReplRO computes syncrepl stanzas for a read-only replica.
func buildDatabaseSyncReplRO(
	clusterName, headlessSvc, namespace, suffix string,
	rwReplicas int32,
	replPassword string,
	tlsEnabled bool,
	ridBase int32,
	retryInterval, keepalive string,
	deltaSync bool,
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
		peerHost := fmt.Sprintf("%s-%d.%s.%s.svc.cluster.local", clusterName, i, headlessSvc, namespace)
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
			deltaSyncOpts, tlsOpt, keepaliveOpt, retryInterval)
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
func (r *SlapdDatabaseReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&ldapv1alpha1.SlapdDatabase{}).
		Named("slapddatabase").
		Complete(r)
}
