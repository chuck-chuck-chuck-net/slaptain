package controller

import (
	"context"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	ldap "github.com/go-ldap/ldap/v3"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The cluster-scoped half of the tunable work: attributes that live on
// cn=config itself rather than on a database, plus the monitor backend.
//
// This is the first place the SlapdCluster controller binds to a pod as
// cn=admin,cn=config — until now its only LDAP traffic was the CSN poll, which
// binds as the replication identity. The shape is deliberately the SlapdDatabase
// controller's: cn=config is node-local (ADR-002), so every pod is visited, read,
// compared and written independently, and a pod that cannot be reached is logged
// and retried rather than failing the reconcile. ADR-001's idempotency
// requirement is what makes that safe.

// reconcileTunables visits every pod of the cluster — read-write and read-only
// alike, because cn=config is node-local and an RO pod serves clients too.
//
// Deliberately best-effort: a pod that is mid-restart, or a cluster whose
// config Secret cannot be read, is logged and left for the next reconcile
// rather than failing the whole SlapdCluster pass. Nothing downstream of this
// depends on it having run, and a tunable that lands one reconcile later has
// cost nobody anything — whereas an unreachable pod failing the cluster's
// status update would hide the very outage that made it unreachable.
func (r *SlapdClusterReconciler) reconcileTunables(
	ctx context.Context,
	sc *ldapv1alpha1.SlapdCluster,
	dbInfos []databaseCSNInfo,
) {
	log := logf.FromContext(ctx)

	configPW, err := r.getConfigPassword(ctx, sc)
	if err != nil {
		log.Info("tunable convergence skipped: cannot read the cn=config password", "err", err)
		return
	}

	replicationDNs := make([]string, 0, len(dbInfos))
	for _, info := range dbInfos {
		replicationDNs = append(replicationDNs, info.bindDN)
	}

	replicas := sc.Spec.Replicas
	if replicas == 0 {
		replicas = 1
	}

	type target struct{ name, headless string }
	var targets []target
	for i := int32(0); i < replicas; i++ {
		targets = append(targets, target{
			name:     fmt.Sprintf("%s-%d", sc.Name, i),
			headless: sc.Name + "-headless",
		})
	}
	for i := int32(0); i < sc.Spec.ReadReplicas; i++ {
		targets = append(targets, target{
			name:     fmt.Sprintf("%s-readonly-%d", sc.Name, i),
			headless: sc.Name + "-readonly-headless",
		})
	}

	for _, t := range targets {
		host := fmt.Sprintf("%s.%s.%s.svc.%s", t.name, t.headless, sc.Namespace, r.ClusterDomain)
		if err := r.reconcilePodTunables(ctx, host, configPW, sc, replicationDNs); err != nil {
			log.Info("tunable convergence skipped for pod (will retry)",
				"pod", t.name, "err", err)
		}
	}
}

// reconcilePodTunables converges the server-global cn=config attributes and the
// monitor backend on one pod.
//
// replicationDNs are the replication identities of the cluster's databases; they
// are the only identities granted read on cn=monitor (ADR-008 reuse — see
// monitorACL).
func (r *SlapdClusterReconciler) reconcilePodTunables(
	ctx context.Context,
	host, configPW string,
	sc *ldapv1alpha1.SlapdCluster,
	replicationDNs []string,
) error {
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

	if err := r.ensureGlobalTunables(ctx, conn, host, sc); err != nil {
		return err
	}
	return r.ensureMonitorDatabase(ctx, conn, host, sc, replicationDNs)
}

// ensureGlobalTunables read-compare-writes the cn=config entry's own attributes:
// the two connection lifetimes, the tool thread count, the TLS posture and the
// password hashing policy.
//
// Each one was modified against a running OpenLDAP 2.7.1 pod before being
// classified R1; all five took the change without incident, which is the whole
// evidentiary difference between this function and checkEnvFlags.
func (r *SlapdClusterReconciler) ensureGlobalTunables(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	sc *ldapv1alpha1.SlapdCluster,
) error {
	log := logf.FromContext(ctx)

	cipher, writeCipher := desiredTLSCipherSuite(sc)

	wants := []struct {
		attr  string
		value string
		write bool
	}{
		{"olcIdleTimeout", strconv.FormatInt(int64(desiredIdleTimeout(sc)), 10), true},
		{"olcWriteTimeout", strconv.FormatInt(int64(desiredWriteTimeout(sc)), 10), true},
		{"olcToolThreads", strconv.FormatInt(int64(desiredToolThreads(sc)), 10), true},
		{"olcTLSProtocolMin", desiredTLSProtocolMin(sc), true},
		{"olcPasswordHash", desiredPasswordHash(sc), true},
		{"olcTLSCipherSuite", cipher, writeCipher},
	}

	for _, w := range wants {
		current, err := readConfigAttr(conn, "cn=config", w.attr)
		if err != nil {
			return err
		}
		if !w.write {
			if len(current) == 0 {
				continue
			}
			log.Info("removing global tunable", "host", host, "attr", w.attr)
			modReq := ldap.NewModifyRequest("cn=config", nil)
			modReq.Delete(w.attr, nil)
			if err := conn.Modify(modReq); err != nil {
				return fmt.Errorf("delete %s on cn=config at %s: %w", w.attr, host, err)
			}
			continue
		}
		if len(current) == 1 && strings.EqualFold(strings.TrimSpace(current[0]), w.value) {
			continue
		}
		log.Info("aligning global tunable", "host", host, "attr", w.attr,
			"from", current, "to", w.value)
		modReq := ldap.NewModifyRequest("cn=config", nil)
		modReq.Replace(w.attr, []string{w.value})
		if err := conn.Modify(modReq); err != nil {
			return fmt.Errorf("set %s on cn=config at %s: %w", w.attr, host, err)
		}
	}
	return nil
}

// ensureMonitorDatabase loads back_monitor and creates the monitor database on
// this pod, then converges its ACL.
//
// Both halves work against a running slapd — verified live on 2.7.1: the module
// load is an ldapmodify on cn=module{0}, the database is an ldapadd, and the
// resulting cn=monitor answers immediately without a restart. That is what makes
// cn=monitor an R1 attribute family rather than a bootstrap-time one, and why the
// init container is untouched by this work.
//
// Turning monitoring off does NOT delete an existing monitor database: deleting a
// cn=config database renumbers every database ordered after it, which is the
// hazard ADR-019's "never reuse an olcDatabase={N} DN" rule exists for. Opting
// out stops the operator creating one; removing one already there is a deliberate
// human act.
func (r *SlapdClusterReconciler) ensureMonitorDatabase(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	sc *ldapv1alpha1.SlapdCluster,
	replicationDNs []string,
) error {
	log := logf.FromContext(ctx)

	dbDN, err := findMonitorDBDN(conn)
	if err != nil {
		return err
	}

	if !monitoringEnabled(sc) {
		if dbDN != "" {
			log.V(1).Info("monitoring disabled but a monitor database exists; "+
				"leaving it in place (deleting it would renumber every database "+
				"ordered after it — ADR-019)", "host", host, "dn", dbDN)
		}
		return nil
	}

	if dbDN == "" {
		// back_monitor must be loaded before the database can be added. The
		// module list is additive and slapd ignores a duplicate load, but we
		// read first anyway so a steady-state reconcile writes nothing.
		if err := ensureModuleLoaded(conn, "back_monitor"); err != nil {
			return fmt.Errorf("load back_monitor at %s: %w", host, err)
		}

		log.Info("creating monitor database", "host", host)
		addReq := ldap.NewAddRequest("olcDatabase=monitor,cn=config", nil)
		addReq.Attribute("objectClass", []string{"olcDatabaseConfig", "olcMonitorConfig"})
		addReq.Attribute("olcDatabase", []string{"monitor"})
		// The config rootDN, not a data one: cn=monitor is a server-scope tree
		// and has no data admin of its own.
		addReq.Attribute("olcRootDN", []string{"cn=admin,cn=config"})
		addReq.Attribute("olcAccess", []string{"{0}" + monitorACL(replicationDNs)})
		if err := conn.Add(addReq); err != nil {
			if !ldap.IsErrorWithCode(err, ldap.LDAPResultEntryAlreadyExists) {
				return fmt.Errorf("add monitor database at %s: %w", host, err)
			}
		}
		if dbDN, err = findMonitorDBDN(conn); err != nil || dbDN == "" {
			return fmt.Errorf("re-find monitor database at %s after add: %w", host, err)
		}
	}

	// Converge the ACL: the set of replication identities changes whenever a
	// SlapdDatabase is added or removed, and the SlapdCluster controller is the
	// only place that sees all of them at once.
	desired := []string{monitorACL(replicationDNs)}
	current, err := readConfigAttr(conn, dbDN, "olcAccess")
	if err != nil {
		return err
	}
	if aclsMatch(current, desired) {
		return nil
	}
	log.Info("aligning monitor ACL", "host", host, "dn", dbDN, "identities", len(replicationDNs))
	modReq := ldap.NewModifyRequest(dbDN, nil)
	modReq.Replace("olcAccess", desired)
	return conn.Modify(modReq)
}

// findMonitorDBDN returns this pod's monitor database DN, or "" when there is
// none. Searched by objectClass rather than by a remembered {N}, for the reason
// spelled out at findDataDBDN: the index is positional and any database delete
// can move it.
func findMonitorDBDN(conn *ldap.Conn) (string, error) {
	sr, err := conn.Search(ldap.NewSearchRequest(
		"cn=config", ldap.ScopeSingleLevel, ldap.NeverDerefAliases,
		0, 0, false, "(objectClass=olcMonitorConfig)", []string{"dn"}, nil,
	))
	if err != nil {
		return "", fmt.Errorf("search for the monitor database: %w", err)
	}
	if len(sr.Entries) == 0 {
		return "", nil
	}
	return sr.Entries[0].DN, nil
}

// ensureModuleLoaded adds one module to cn=module{0} when it is not already
// listed. Separate from the SlapdDatabase controller's ensureModulesLoaded
// because this runs from the other controller and loads a different module for a
// different reason; the stored values carry slapd's {N} prefixes, so the
// comparison is a suffix match rather than an equality one.
func ensureModuleLoaded(conn *ldap.Conn, module string) error {
	const dn = "cn=module{0},cn=config"
	current, err := readConfigAttr(conn, dn, "olcModuleLoad")
	if err != nil {
		return err
	}
	for _, v := range current {
		name := v
		if i := strings.Index(name, "}"); i >= 0 && strings.HasPrefix(name, "{") {
			name = name[i+1:]
		}
		if strings.EqualFold(strings.TrimSpace(name), module) {
			return nil
		}
	}
	modReq := ldap.NewModifyRequest(dn, nil)
	modReq.Add("olcModuleLoad", []string{module})
	return conn.Modify(modReq)
}
