package controller

import "testing"

// The gate decides on evidence it was handed, so the decision is pure and the
// only thing the controller adds is the Get. Every row here is a state a real
// cluster reaches: a cert that has not been issued yet, a Secret created empty
// by a templating mistake, a chart that enabled TLS without naming a Secret.
func TestEvaluateTLSSecret(t *testing.T) {
	const complete = `{"tls.crt":"x","tls.key":"y"}`
	_ = complete

	tests := []struct {
		name       string
		enabled    bool
		secretName string
		found      bool
		data       map[string][]byte
		wantReady  bool
		wantReason string
	}{
		{
			name:       "TLS off: nothing to check, and no Secret is required",
			enabled:    false,
			wantReady:  true,
			wantReason: reasonTLSDisabled,
		},
		{
			name:       "TLS off with a Secret named anyway: still nothing to check",
			enabled:    false,
			secretName: "slapd-tls",
			found:      false,
			wantReady:  true,
			wantReason: reasonTLSDisabled,
		},
		{
			name:       "TLS on, complete Secret: ready",
			enabled:    true,
			secretName: "slapd-tls",
			found:      true,
			data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y")},
			wantReady:  true,
			wantReason: reasonTLSReady,
		},
		{
			name:       "ca.crt is optional — a public-CA chain carries no separate bundle (ADR-007)",
			enabled:    true,
			secretName: "slapd-tls",
			found:      true,
			data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("y")},
			wantReady:  true,
			wantReason: reasonTLSReady,
		},
		{
			name:       "TLS on, Secret absent: the cert has not been issued yet",
			enabled:    true,
			secretName: "slapd-tls",
			found:      false,
			wantReady:  false,
			wantReason: reasonTLSSecretMissing,
		},
		{
			name:       "TLS on, Secret present but empty",
			enabled:    true,
			secretName: "slapd-tls",
			found:      true,
			data:       map[string][]byte{},
			wantReady:  false,
			wantReason: reasonTLSSecretIncomplete,
		},
		{
			name:       "TLS on, key without certificate",
			enabled:    true,
			secretName: "slapd-tls",
			found:      true,
			data:       map[string][]byte{"tls.key": []byte("y")},
			wantReady:  false,
			wantReason: reasonTLSSecretIncomplete,
		},
		{
			name:       "TLS on, certificate without key",
			enabled:    true,
			secretName: "slapd-tls",
			found:      true,
			data:       map[string][]byte{"tls.crt": []byte("x")},
			wantReady:  false,
			wantReason: reasonTLSSecretIncomplete,
		},
		{
			name:       "TLS on, a present but EMPTY tls.key: as unusable as an absent one",
			enabled:    true,
			secretName: "slapd-tls",
			found:      true,
			data:       map[string][]byte{"tls.crt": []byte("x"), "tls.key": []byte("")},
			wantReady:  false,
			wantReason: reasonTLSSecretIncomplete,
		},
		{
			name:       "TLS on with no secretName: nothing to mount",
			enabled:    true,
			secretName: "",
			wantReady:  false,
			wantReason: reasonTLSSecretNameEmpty,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := evaluateTLSSecret(tc.enabled, tc.secretName, tc.found, tc.data)
			if got.Ready != tc.wantReady {
				t.Errorf("Ready: got %v, want %v (reason %q, msg %q)", got.Ready, tc.wantReady, got.Reason, got.Message)
			}
			if got.Reason != tc.wantReason {
				t.Errorf("Reason: got %q, want %q", got.Reason, tc.wantReason)
			}
			if got.Message == "" {
				t.Error("Message is empty: the whole point of the gate is that the CR says what to fix")
			}
			if !got.Ready && tc.secretName != "" {
				if !contains(got.Message, tc.secretName) {
					t.Errorf("Message %q does not name the Secret %q the user has to create", got.Message, tc.secretName)
				}
			}
		})
	}
}

func contains(haystack, needle string) bool {
	return len(needle) > 0 && len(haystack) >= len(needle) &&
		(haystack == needle || indexOf(haystack, needle) >= 0)
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}
	return -1
}
