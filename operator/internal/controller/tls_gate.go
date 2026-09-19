package controller

import (
	"context"
	"fmt"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

// The certificate gate (ADR-029).
//
// slapd is handed its TLS material as a mounted Secret. Before this gate the
// operator never read that Secret: it copied the name into the pod's volume and
// left it there, so naming a Secret that did not exist produced pods stuck in
// ContainerCreating and a SlapdCluster that said nothing at all — no condition,
// no phase, no event. The diagnosis lived in `kubectl describe pod`, which is
// exactly where somebody who deployed a CR is not looking.
//
// The gate WITHHOLDS work and never undoes it. For a cluster that is already
// running, a Secret that disappears is reported and nothing else: the pods have
// it mounted and are still serving, and tearing down a working directory
// because a prerequisite object was deleted would turn a cosmetic problem into
// an outage. Same shape as ADR-027's identity gate and ADR-025's withhold belt.
const (
	condTLSReady              = "TLSReady"
	reasonTLSReady            = "CertificateAvailable"
	reasonTLSDisabled         = "TLSDisabled"
	reasonTLSSecretMissing    = "SecretMissing"
	reasonTLSSecretIncomplete = "SecretIncomplete"
	reasonTLSSecretNameEmpty  = "SecretNameEmpty"
)

// tlsVerdict is the gate's decision. Message is always populated and always
// names what to fix: a condition that says False without saying why leaves the
// user exactly where ContainerCreating did.
type tlsVerdict struct {
	Ready   bool
	Reason  string
	Message string
}

// evaluateTLSSecret decides whether slapd can be started with the TLS material
// described. It is pure — the caller does the Get and passes what it found — so
// every state a cluster can reach is reachable in a table test.
//
// ca.crt is deliberately NOT required. A certificate from a public CA (or
// cert-manager with a public issuer) embeds its chain in tls.crt and needs no
// separate bundle; the init container omits TLSCACertificateFile and slapd
// falls back to the system trust store, which is the right answer there
// (ADR-007).
func evaluateTLSSecret(enabled bool, secretName string, found bool, data map[string][]byte) tlsVerdict {
	if !enabled {
		return tlsVerdict{
			Ready:   true,
			Reason:  reasonTLSDisabled,
			Message: "spec.ldap.tls.enabled is false; slapd serves plaintext and needs no certificate",
		}
	}

	if secretName == "" {
		return tlsVerdict{
			Reason: reasonTLSSecretNameEmpty,
			Message: "spec.ldap.tls.enabled is true but spec.ldap.tls.secretName is empty; " +
				"name the Secret holding tls.crt and tls.key, or set enabled: false",
		}
	}

	if !found {
		return tlsVerdict{
			Reason: reasonTLSSecretMissing,
			Message: fmt.Sprintf("Secret %q does not exist; slapd is not started until it does. "+
				"Create it with tls.crt and tls.key (cert-manager, your own PKI, or tests/gencert.sh "+
				"— see docs/TLS.md), or set spec.ldap.tls.enabled: false", secretName),
		}
	}

	var missing []string
	for _, key := range []string{"tls.crt", "tls.key"} {
		if len(data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		return tlsVerdict{
			Reason: reasonTLSSecretIncomplete,
			Message: fmt.Sprintf("Secret %q exists but %v is missing or empty; slapd cannot start without it. "+
				"ca.crt is optional — a public-CA chain carries no separate bundle", secretName, missing),
		}
	}

	return tlsVerdict{
		Ready:   true,
		Reason:  reasonTLSReady,
		Message: fmt.Sprintf("Secret %q carries tls.crt and tls.key", secretName),
	}
}

// reconcileTLSGate reads the TLS Secret (when TLS is on) and records the
// verdict as the TLSReady condition. It returns whether the StatefulSet may be
// touched this pass.
func (r *SlapdClusterReconciler) reconcileTLSGate(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) (bool, error) {
	var (
		found bool
		data  map[string][]byte
	)
	if sc.Spec.LDAP.TLS.Enabled && sc.Spec.LDAP.TLS.SecretName != "" {
		secret := &corev1.Secret{}
		err := r.Get(ctx, types.NamespacedName{
			Namespace: sc.Namespace,
			Name:      sc.Spec.LDAP.TLS.SecretName,
		}, secret)
		switch {
		case err == nil:
			found, data = true, secret.Data
		case apierrors.IsNotFound(err):
			// Not an error: "not issued yet" is an ordinary state, and the
			// whole point of the gate is to name it rather than fail.
		default:
			// A real API failure (RBAC, connectivity). Do NOT read that as a
			// missing Secret — that would withhold a running cluster's
			// StatefulSet on evidence we do not have.
			return false, fmt.Errorf("reading TLS Secret %q: %w", sc.Spec.LDAP.TLS.SecretName, err)
		}
	}

	v := evaluateTLSSecret(sc.Spec.LDAP.TLS.Enabled, sc.Spec.LDAP.TLS.SecretName, found, data)

	status := metav1.ConditionTrue
	if !v.Ready {
		status = metav1.ConditionFalse
	}
	setCondition(&sc.Status.Conditions, metav1.Condition{
		Type:               condTLSReady,
		Status:             status,
		Reason:             v.Reason,
		Message:            v.Message,
		LastTransitionTime: metav1.Now(),
		ObservedGeneration: sc.Generation,
	})

	// No Kubernetes Event: no controller in this operator emits one, and the
	// condition plus the withholding log line is how every other gate here
	// reports (mesh resolution, the ADR-027 identity gate). Introducing an
	// EventRecorder for this one case would add wiring and RBAC to say a third
	// time what the condition already says. ADR-029 is amended accordingly.
	return v.Ready, nil
}

// observeAndApplyTLSBlockedStatus writes the status for a pass the gate held.
//
// The phase distinguishes the two cases deliberately. A cluster whose
// StatefulSet does not exist yet is Bootstrapping — it is waiting on a
// prerequisite and has never served. A cluster that HAS a StatefulSet keeps
// whatever its pods say it is: it is still serving, and calling a healthy
// directory Error because a Secret was deleted would be a false alarm, with the
// TLSReady condition carrying the real news.
func (r *SlapdClusterReconciler) observeAndApplyTLSBlockedStatus(ctx context.Context, sc *ldapv1alpha1.SlapdCluster) error {
	sts := &appsv1.StatefulSet{}
	err := r.Get(ctx, types.NamespacedName{Namespace: sc.Namespace, Name: sc.Name}, sts)
	switch {
	case apierrors.IsNotFound(err):
		sc.Status.Phase = ldapv1alpha1.PhaseBootstrapping
		sc.Status.ReadyReplicas = 0
	case err != nil:
		return fmt.Errorf("reading StatefulSet while TLS-blocked: %w", err)
	default:
		sc.Status.ReadyReplicas = sts.Status.ReadyReplicas
		sc.Status.Replicas = sts.Status.Replicas
		if sts.Status.ReadyReplicas < sc.Spec.Replicas {
			sc.Status.Phase = ldapv1alpha1.PhaseDegraded
		} else {
			sc.Status.Phase = ldapv1alpha1.PhaseRunning
		}
	}
	sc.Status.ObservedGeneration = sc.Generation
	return r.applyStatus(ctx, sc)
}
