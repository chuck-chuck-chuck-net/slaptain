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

package v1alpha1

import "testing"

func TestNetworkMode(t *testing.T) {
	cases := []struct {
		name    string
		network *ReplicationNetworkConfig
		want    string
	}{
		{name: "no network configured", network: nil, want: ""},
		{name: "unset mode, no multusNetwork -> pod-routed default", network: &ReplicationNetworkConfig{}, want: NetworkModePodRouted},
		{name: "unset mode, multusNetwork set -> infers multus", network: &ReplicationNetworkConfig{MultusNetwork: "infra/repl-net"}, want: NetworkModeMultus},
		{name: "explicit multus", network: &ReplicationNetworkConfig{Mode: NetworkModeMultus, MultusNetwork: "infra/repl-net"}, want: NetworkModeMultus},
		{name: "explicit pod-routed", network: &ReplicationNetworkConfig{Mode: NetworkModePodRouted}, want: NetworkModePodRouted},
		{name: "explicit pod-routed wins over multusNetwork", network: &ReplicationNetworkConfig{Mode: NetworkModePodRouted, MultusNetwork: "infra/repl-net"}, want: NetworkModePodRouted},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sc := &SlapdCluster{}
			sc.Spec.Replication.Network = tc.network
			if got := sc.NetworkMode(); got != tc.want {
				t.Fatalf("NetworkMode() = %q, want %q", got, tc.want)
			}
		})
	}
}
