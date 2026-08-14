/*
Copyright 2026 The cluster-api-hypervisor Authors.

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

// The deterministic MAC derivation contract (test-first).
//
// This suite pins the behavior of the single exported function in this
// package: Derive takes a cluster name and a machine name and returns the
// machine's MAC address as a string. The machine contract requires the
// address to be deterministic (the same cluster/machine pair always produces
// the same address, however often or from wherever it is derived), to belong
// to the locally administered c6:e5:50:1c:ec family (the first five octets
// are always exactly that prefix and the last octet is lower-case hex), and
// to be distinct enough that different machines in one cluster, and the same
// machine name in different clusters, do not collapse onto one address in
// the pinned test set.
//
// The contract, in prose:
//
//   - Derive(clusterName, machineName string) string returns a MAC address
//     string of the form c6:e5:50:1c:ec:xx, where xx is derived from the
//     cluster/machine pair. The exact derivation is deliberately not pinned:
//     any stable hash over the pair is acceptable as long as the format, the
//     family prefix, determinism, and the distinctness of the pinned inputs
//     all hold. The derivation input must cover both names: machines are
//     addressed within their cluster, so the same machine name in two
//     clusters must not collide.
//   - The optional explicit MAC override (spec.mac) is not part of this
//     package: the machine controller bypasses Derive at its call site when
//     an explicit MAC is configured. This package only derives the default.
//   - Empty names are tolerated: Derive must not panic for an empty cluster
//     name, an empty machine name, or both, and the result still matches the
//     family format.
package mac_test

import (
	"fmt"
	"regexp"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
)

// macFamily pins the address shape the derived MACs must belong to: five
// fixed family octets followed by one derived octet, lower-case hex,
// colon-separated.
var macFamily = regexp.MustCompile(`^c6:e5:50:1c:ec:[0-9a-f]{2}$`)

// Compile-time pin: Derive must exist with exactly this signature.
var (
	_ func(clusterName, machineName string) string = mac.Derive
)

// TestDeriveDeterministic pins determinism: repeated derivation for the same
// cluster/machine pair returns the same address.
func TestDeriveDeterministic(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		machineName string
	}{
		{name: "single letters", clusterName: "c1", machineName: "m1"},
		{name: "k8s style names", clusterName: "k8s-lab", machineName: "cp-1"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			first := mac.Derive(tt.clusterName, tt.machineName)
			for range 5 {
				if got := mac.Derive(tt.clusterName, tt.machineName); got != first {
					t.Fatalf("Derive(%q, %q) = %q, want the stable address %q", tt.clusterName, tt.machineName, got, first)
				}
			}
		})
	}
}

// TestDeriveFormat pins the address shape: five fixed family octets, one
// derived lower-case hex octet, colon-separated.
func TestDeriveFormat(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		machineName string
	}{
		{name: "single letters", clusterName: "c1", machineName: "m1"},
		{name: "k8s style names", clusterName: "k8s-lab", machineName: "cp-1"},
		{name: "long names", clusterName: "research-cluster-a", machineName: "worker-node-000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mac.Derive(tt.clusterName, tt.machineName)
			if !macFamily.MatchString(got) {
				t.Errorf("Derive(%q, %q) = %q, want the family format %s", tt.clusterName, tt.machineName, got, macFamily)
			}
		})
	}
}

// TestDeriveDistinct pins that the derivation input covers both names: two
// machines in one cluster must not collide, and the same machine name in two
// clusters must not collide.
func TestDeriveDistinct(t *testing.T) {
	// Two machines in one cluster: the machine name must participate in the
	// derivation.
	if a, b := mac.Derive("c1", "m1"), mac.Derive("c1", "m2"); a == b {
		t.Errorf("Derive for distinct machines in one cluster collided: %q", a)
	}

	// The same machine name in two clusters: the cluster name must
	// participate in the derivation.
	if a, b := mac.Derive("c1", "m1"), mac.Derive("c2", "m1"); a == b {
		t.Errorf("Derive for the same machine name in distinct clusters collided: %q", a)
	}
}

// TestDeriveSetStability pins a whole machine set: every address in a
// realistic cluster belongs to the family prefix, no two machines share an
// address, and re-deriving the set reproduces every address.
func TestDeriveSetStability(t *testing.T) {
	const (
		clusterName = "k8s-lab"
		machines    = 16
	)

	byMachine := make(map[string]string, machines) // machine -> address
	byAddr := make(map[string]string, machines)    // address -> machine
	for i := range machines {
		machineName := fmt.Sprintf("worker-%02d", i)
		addr := mac.Derive(clusterName, machineName)

		if !macFamily.MatchString(addr) {
			t.Errorf("Derive(%q, %q) = %q, want the family format %s", clusterName, machineName, addr, macFamily)
		}
		if other, dup := byAddr[addr]; dup {
			t.Errorf("Derive(%q, %q) = %q, already assigned to %q", clusterName, machineName, addr, other)
		}
		byAddr[addr] = machineName
		byMachine[machineName] = addr
	}

	// Re-deriving the set must reproduce every address: determinism holds
	// across the whole set, not just for one pair.
	for i := range machines {
		machineName := fmt.Sprintf("worker-%02d", i)
		if addr := mac.Derive(clusterName, machineName); addr != byMachine[machineName] {
			t.Errorf(
				"Derive(%q, %q) changed between passes: first %q, second %q",
				clusterName,
				machineName,
				byMachine[machineName],
				addr,
			)
		}
	}
}

// TestDeriveEmptyInputs pins the degenerate inputs: empty cluster and/or
// machine names must not panic and must still produce a family-format
// address.
func TestDeriveEmptyInputs(t *testing.T) {
	tests := []struct {
		name        string
		clusterName string
		machineName string
	}{
		{name: "both empty", clusterName: "", machineName: ""},
		{name: "empty cluster", clusterName: "", machineName: "m1"},
		{name: "empty machine", clusterName: "c1", machineName: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := mac.Derive(tt.clusterName, tt.machineName)
			if !macFamily.MatchString(got) {
				t.Errorf("Derive(%q, %q) = %q, want the family format %s", tt.clusterName, tt.machineName, got, macFamily)
			}
		})
	}
}
