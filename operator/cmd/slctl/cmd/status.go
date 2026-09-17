package cmd

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	ldapv1alpha1 "github.com/chuck-chuck-chuck-net/slaptain/operator/api/v1alpha1"
)

type statusJSON struct {
	Name                  string `json:"name"`
	Namespace             string `json:"namespace"`
	Phase                 string `json:"phase"`
	Replicas              int32  `json:"replicas"`
	ReadyReplicas         int32  `json:"readyReplicas"`
	ReadOnlyReplicas      int32  `json:"readOnlyReplicas,omitempty"`
	ReadOnlyReadyReplicas int32  `json:"readOnlyReadyReplicas,omitempty"`
	ReplicationEnabled    bool   `json:"replicationEnabled"`
	TLSEnabled            bool   `json:"tlsEnabled"`
	Age                   string `json:"age"`
	// MeshRef / ExternalPeerSource / MeshNote describe where the external peer
	// set came from, and are emitted ONLY for a cluster that references a
	// SlapdMesh (ADR-028 §4). All three are omitempty and stay unset
	// otherwise, so a non-mesh cluster's JSON is byte-identical to what it has
	// always been.
	MeshRef            string                   `json:"meshRef,omitempty"`
	ExternalPeerSource string                   `json:"externalPeerSource,omitempty"`
	MeshNote           string                   `json:"meshNote,omitempty"`
	ExternalPeers      []externalPeerStatusJSON `json:"externalPeers,omitempty"`
	Conditions         []conditionJSON          `json:"conditions,omitempty"`
}

type externalPeerStatusJSON struct {
	Name      string `json:"name"`
	Connected bool   `json:"connected"`
	LastError string `json:"lastError,omitempty"`
}

type conditionJSON struct {
	Type    string `json:"type"`
	Status  string `json:"status"`
	Reason  string `json:"reason,omitempty"`
	Message string `json:"message,omitempty"`
}

var statusCmd = &cobra.Command{
	Use:               "status [name]",
	Short:             "Show SlapdCluster status",
	Long:              "Display status overview of one or all SlapdCluster resources: phase, replicas, replication, conditions.",
	Args:              cobra.MaximumNArgs(1),
	RunE:              runStatus,
	ValidArgsFunction: completeSlapdClusterNames,
}

func init() {
	rootCmd.AddCommand(statusCmd)
}

func runStatus(cmd *cobra.Command, args []string) error {
	ctx := context.Background()
	k8sClient, _, _, ns, err := initClient()
	if err != nil {
		return err
	}

	targets, err := resolveTargets(ctx, k8sClient, args, ns)
	if err != nil {
		return err
	}

	var jsonResults []statusJSON

	for i, key := range targets {
		sc := &ldapv1alpha1.SlapdCluster{}
		if err := k8sClient.Get(ctx, key, sc); err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "Error fetching %s/%s: %v\n", key.Namespace, key.Name, err)
			continue
		}

		// A mesh-driven cluster carries its cross-site wiring nowhere in its
		// own spec — the operator derives it in memory and never writes it
		// back (ADR-028 §4, MESH-PLAN Phase 4). Recover the resolved peer set
		// from what the operator published, before anything below reads
		// spec.replication.externalPeers, so the display cannot report "no
		// external peers" while cn=config carries the stanzas.
		peers := applyResolvedPeers(sc)

		if jsonOutput {
			jsonResults = append(jsonResults, buildStatusJSON(sc, peers))
		} else {
			if i > 0 {
				fmt.Println()
			}
			printStatusText(sc, peers)
		}
	}

	if jsonOutput {
		out, _ := json.MarshalIndent(jsonResults, "", "  ")
		fmt.Println(string(out))
	}
	return nil
}

func buildStatusJSON(sc *ldapv1alpha1.SlapdCluster, peers peerResolution) statusJSON {
	s := statusJSON{
		Name:                  sc.Name,
		Namespace:             sc.Namespace,
		Phase:                 string(sc.Status.Phase),
		Replicas:              sc.Status.Replicas,
		ReadyReplicas:         sc.Status.ReadyReplicas,
		ReadOnlyReplicas:      sc.Status.ReadOnlyReplicas,
		ReadOnlyReadyReplicas: sc.Status.ReadOnlyReadyReplicas,
		ReplicationEnabled:    sc.Spec.Replication.Enabled,
		TLSEnabled:            sc.Spec.LDAP.TLS.Enabled,
		Age:                   age(sc.CreationTimestamp),
	}
	if peers.Source != peerSourceSpec {
		s.MeshRef = peers.MeshRef
		s.ExternalPeerSource = string(peers.Source)
		s.MeshNote = peers.Note
	}
	for _, ep := range sc.Status.ExternalPeerStatuses {
		s.ExternalPeers = append(s.ExternalPeers, externalPeerStatusJSON{
			Name:      ep.Name,
			Connected: ep.Connected,
			LastError: ep.LastError,
		})
	}
	for _, c := range sc.Status.Conditions {
		s.Conditions = append(s.Conditions, conditionJSON{
			Type:    c.Type,
			Status:  string(c.Status),
			Reason:  c.Reason,
			Message: c.Message,
		})
	}
	return s
}

func printStatusText(sc *ldapv1alpha1.SlapdCluster, peers peerResolution) {
	fmt.Printf("SlapdCluster: %s/%s\n", sc.Namespace, sc.Name)
	printSeparator()
	fmt.Printf("  Phase:              %s\n", sc.Status.Phase)
	fmt.Printf("  Age:                %s\n", age(sc.CreationTimestamp))
	fmt.Printf("  Replicas:           %d/%d ready\n", sc.Status.ReadyReplicas, sc.Status.Replicas)
	if sc.Spec.ReadReplicas > 0 || sc.Status.ReadOnlyReplicas > 0 {
		fmt.Printf("  Read-Only:          %d/%d ready\n", sc.Status.ReadOnlyReadyReplicas, sc.Status.ReadOnlyReplicas)
	}
	fmt.Printf("  TLS:                %v\n", sc.Spec.LDAP.TLS.Enabled)
	fmt.Printf("  Replication:        %v\n", sc.Spec.Replication.Enabled)

	// Provenance, printed only for a mesh-driven cluster so every existing
	// example stays byte-identical. It comes BEFORE the peer list, because the
	// reader needs to know whether to trust that list — and because when the
	// list is empty this is the only thing distinguishing "no peers" from
	// "could not tell".
	printMeshProvenance(peers)

	if len(sc.Spec.Replication.ExternalPeers) > 0 {
		fmt.Println("  External Peers:")
		for _, ep := range sc.Spec.Replication.ExternalPeers {
			// Find status entry for this peer.
			var modeStr, replStr string
			for _, eps := range sc.Status.ExternalPeerStatuses {
				if eps.Name != ep.Name {
					continue
				}
				// Mode descriptor.
				if len(eps.DiscoveredAddresses) > 0 {
					rpp := int32(1)
					if ep.ReplicasPerPeer != nil && *ep.ReplicasPerPeer > 1 {
						rpp = *ep.ReplicasPerPeer
					}
					if rpp > 1 {
						sel := rpp
						if sel > int32(len(eps.DiscoveredAddresses)) {
							sel = int32(len(eps.DiscoveredAddresses))
						}
						modeStr = fmt.Sprintf("discovery (%d pod(s), rpp=%d/%d)", len(eps.DiscoveredAddresses), sel, len(eps.DiscoveredAddresses))
					} else {
						modeStr = fmt.Sprintf("discovery (%d pod(s))", len(eps.DiscoveredAddresses))
					}
				} else if eps.Connected {
					modeStr = "connected"
				} else if eps.LastError != "" {
					modeStr = "disconnected (" + eps.LastError + ")"
				} else {
					modeStr = "disconnected"
				}
				// Replication state.
				switch eps.ReplicationState {
				case "Synced":
					replStr = "Synced"
				case "Lagging":
					if eps.LagSeconds != "" {
						replStr = fmt.Sprintf("Lagging (%ss)", eps.LagSeconds)
					} else {
						replStr = "Lagging"
					}
				case "PartiallyVerified":
					replStr = "PartiallyVerified"
					if eps.LastError != "" {
						replStr += " (" + eps.LastError + ")"
					}
				case "Unreachable":
					replStr = "Unreachable"
				}
				break
			}
			rpp := int32(1)
			if ep.ReplicasPerPeer != nil && *ep.ReplicasPerPeer > 1 {
				rpp = *ep.ReplicasPerPeer
			}
			rppSuffix := ""
			if rpp > 1 {
				rppSuffix = fmt.Sprintf(", rpp=%d", rpp)
			}
			if modeStr == "" {
				if len(ep.PodAddresses) > 0 {
					modeStr = fmt.Sprintf("Multus (%d pod(s)%s)", len(ep.PodAddresses), rppSuffix)
				} else if ep.Discovery != nil {
					modeStr = "discovery (pending)"
				}
			}
			line := fmt.Sprintf("    %-20s %s", ep.Name, modeStr)
			if replStr != "" {
				line += "  " + replStr
			}
			fmt.Println(line)
		}
	}

	if len(sc.Status.Conditions) > 0 {
		fmt.Println("  Conditions:")
		for _, c := range sc.Status.Conditions {
			fmt.Printf("    %-24s %s", c.Type, c.Status)
			if c.Reason != "" {
				fmt.Printf("  (%s)", c.Reason)
			}
			if c.Message != "" {
				fmt.Printf("  %s", c.Message)
			}
			fmt.Println()
		}
	}

}

// printMeshProvenance says where the external peer set came from, and is
// silent for a cluster that hand-writes its peers.
//
// The UNRESOLVED case is the one that matters: it must be impossible to read
// that output as a healthy standalone cluster, so it is labelled and carries
// the operator's own reason verbatim.
func printMeshProvenance(peers peerResolution) {
	if peers.Source == peerSourceSpec {
		return
	}
	fmt.Printf("  Mesh:               %s", peers.MeshRef)
	if !peers.OK() {
		fmt.Print("  (UNRESOLVED)")
	}
	fmt.Println()
	for _, line := range wrapNote(peers.Note, 76) {
		fmt.Printf("    %s\n", line)
	}
}

// wrapNote breaks a note into terminal-width lines here rather than leaving it
// to the terminal, whose own wrapping would break the indent.
func wrapNote(note string, width int) []string {
	var lines []string
	var cur string
	for _, w := range strings.Fields(note) {
		switch {
		case cur == "":
			cur = w
		case len(cur)+1+len(w) <= width:
			cur += " " + w
		default:
			lines = append(lines, cur)
			cur = w
		}
	}
	if cur != "" {
		lines = append(lines, cur)
	}
	return lines
}

func age(t metav1.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t.Time)
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// phaseSymbol returns a visual indicator for the phase.
func phaseSymbol(phase ldapv1alpha1.SlapdClusterPhase) string {
	switch phase {
	case ldapv1alpha1.PhaseRunning:
		return "OK"
	case ldapv1alpha1.PhaseDegraded:
		return "WARN"
	case ldapv1alpha1.PhaseError:
		return "ERR"
	case ldapv1alpha1.PhaseBootstrapping:
		return "BOOT"
	default:
		return strings.ToUpper(string(phase))
	}
}
