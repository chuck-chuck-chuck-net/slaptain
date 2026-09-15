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
// cn=config itself rather than on a database.
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
) {
	log := logf.FromContext(ctx)

	configPW, err := r.getConfigPassword(ctx, sc)
	if err != nil {
		log.Info("tunable convergence skipped: cannot read the cn=config password", "err", err)
		return
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
		if err := r.reconcilePodTunables(ctx, host, configPW, sc); err != nil {
			log.Info("per-pod infrastructure convergence skipped for pod (will retry)",
				"pod", t.name, "err", err)
		}
	}
}

// reconcilePodInfrastructure is the per-pod work this controller owns on an
// already-bound connection. Kept separate from reconcilePodTunables so the
// connection setup has one home and this list can grow without it.
//
// Order is intentional but not load-bearing: nothing here depends on anything
// else here. It is best-effort as a whole — see reconcileTunables — so a pod
// that is mid-restart costs a log line and a retry, never a failed reconcile.
func (r *SlapdClusterReconciler) reconcilePodInfrastructure(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	sc *ldapv1alpha1.SlapdCluster,
) error {
	// Every step runs even if an earlier one fails, and all failures are
	// reported together (runConvergenceSteps). This used to short-circuit, and
	// the auth database — listed second, and a hard precondition for
	// replication since ADR-027's cutover — was consequently never created on
	// any cluster whose global tunables slapd refused. See podinfra.go for the
	// measurement that found it.
	return runConvergenceSteps(
		convergenceStep{"global tunables", func() error {
			return r.ensureGlobalTunables(ctx, conn, host, sc)
		}},
		// The node-local authentication database (ADR-027 decision 7): this
		// controller is its single creator, on every pod, read-write and
		// read-only alike. Its per-database identity ENTRIES belong to the
		// SlapdDatabase controller and are written separately.
		convergenceStep{"auth database", func() error {
			return r.ensureAuthDB(ctx, conn, host)
		}},
	)
}

// reconcilePodTunables dials one pod, binds as cn=admin,cn=config, and runs the
// per-pod infrastructure convergence this controller owns.
func (r *SlapdClusterReconciler) reconcilePodTunables(
	ctx context.Context,
	host, configPW string,
	sc *ldapv1alpha1.SlapdCluster,
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

	return r.reconcilePodInfrastructure(ctx, conn, host, sc)
}

// ensureGlobalTunables read-compare-writes the cn=config entry's own attributes:
// the tool thread count, the TLS posture and the password hashing policy.
//
// Each was modified against a running OpenLDAP 2.7.1 pod before being classified
// R1, and each took the change without incident. That empirical step is not
// ceremony: two attributes the same review proposed for this list did NOT
// survive it, and are not here.
func (r *SlapdClusterReconciler) ensureGlobalTunables(
	ctx context.Context,
	conn *ldap.Conn,
	host string,
	sc *ldapv1alpha1.SlapdCluster,
) error {
	log := logf.FromContext(ctx)

	cipher, writeCipher := desiredTLSCipherSuite(sc)

	// NOT here, deliberately: olcIdleTimeout and olcWriteTimeout.
	//
	// The production-config review proposed both as converged cluster tunables
	// and this function shipped them that way for exactly one deployment. On
	// t3e they took down the entire cluster twice: an ldapmodify of either
	// attribute against a running slapd 2.7.1 HANGS the process — not a crash, a
	// hang. The modify's CSN is queued and the operation never returns:
	//
	//	conn=1050 op=2 MOD dn="cn=config"
	//	conn=1050 op=2 MOD attr=olcIdleTimeout
	//	slap_get_csn: conn=1050 op=2 generated new csn=…
	//	slap_queue_csn: queueing …
	//	<nothing, ever>
	//
	// From then on the pod answers nothing at all — not even an anonymous
	// rootDSE — and a SIGTERM sticks in "slapd shutdown: waiting for 2
	// operations/tasks to finish". Observed on three of four pods in one
	// deployment and on all three RW pods in another; recovered only by
	// restarting every pod. The mechanism is the daemon thread: writing a
	// non-zero global_idletimeout arms connections_timeout_idle, which walks the
	// connection table (connections_mutex, then each connection's own mutex)
	// from the daemon loop while the modify that armed it is still executing
	// inside one of those connections.
	//
	// Both values are FINE when they come from the boot config — slapd-0 ran for
	// twenty minutes with olcIdleTimeout 3600 and olcWriteTimeout 300 loaded at
	// startup and served normally throughout. So the finding is real and the
	// placement is wrong: these are ADR-024 R2, bootstrap-time, not R1. They are
	// recorded in docs/BACKLOG.md with this evidence rather than shipped as an
	// API field that does nothing (ADR-024 R4 — a field that cannot be honoured
	// is not offered).
	wants := []globalTunable{
		{"olcToolThreads", strconv.FormatInt(int64(desiredToolThreads(sc)), 10), true},
		{"olcPasswordHash", desiredPasswordHash(sc), true},
	}
	// TLS attributes are appended only where slapd will accept them. With TLS
	// disabled it answers err=53 to every one of these on every reconcile,
	// forever. They are LEFT ALONE rather than given write=false: that branch
	// DELETES the attribute, which the same server refuses for the same reason,
	// so it would swap one impossible modify for another.
	if tlsTunablesWritable(sc) {
		wants = append(wants,
			globalTunable{"olcTLSProtocolMin", desiredTLSProtocolMin(sc), true},
			globalTunable{"olcTLSCipherSuite", cipher, writeCipher},
		)
	}

	// One attribute per step, so an attribute slapd refuses cannot stop the
	// others from converging. Same rule as reconcilePodInfrastructure, one level
	// down, and for the same reason: these attributes are independent of each
	// other, and the short-circuit that used to be here is what let a single
	// rejected TLS setting silently abandon the rest of the list.
	steps := make([]convergenceStep, 0, len(wants))
	for _, w := range wants {
		steps = append(steps, convergenceStep{w.attr, func() error {
			current, err := readConfigAttr(conn, "cn=config", w.attr)
			if err != nil {
				return err
			}
			if !w.write {
				if len(current) == 0 {
					return nil
				}
				log.Info("removing global tunable", "host", host, "attr", w.attr)
				modReq := ldap.NewModifyRequest("cn=config", nil)
				modReq.Delete(w.attr, nil)
				if err := conn.Modify(modReq); err != nil {
					return fmt.Errorf("delete %s on cn=config at %s: %w", w.attr, host, err)
				}
				return nil
			}
			if len(current) == 1 && strings.EqualFold(strings.TrimSpace(current[0]), w.value) {
				return nil
			}
			log.Info("aligning global tunable", "host", host, "attr", w.attr,
				"from", current, "to", w.value)
			modReq := ldap.NewModifyRequest("cn=config", nil)
			modReq.Replace(w.attr, []string{w.value})
			if err := conn.Modify(modReq); err != nil {
				return fmt.Errorf("set %s on cn=config at %s: %w", w.attr, host, err)
			}
			return nil
		}})
	}
	return runConvergenceSteps(steps...)
}
