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

// The static IP allocator contract (test-first).
//
// This suite pins the behavior of the per-cluster IPAM allocator in this
// package. The cluster controller constructs one allocator per cluster from
// that cluster's network configuration (cluster CIDR, gateway, and the
// allocatable pool bounds) and uses it to hand out static machine addresses;
// the machine controller records the address in machine status, and the
// freed address is reused after the machine is deleted. There is no
// cross-cluster state: two allocators never share an allocation.
//
// The contract, in prose:
//
//   - NewAllocator(clusterCIDR, gateway, startIP, endIP string)
//     (*Allocator, error) constructs an allocator for one cluster. The pool
//     is the inclusive IPv4 range startIP..endIP inside clusterCIDR. The
//     constructor rejects an invalid CIDR, a non-IPv4 CIDR, a gateway that
//     is missing or outside the CIDR, range bounds that are missing, outside
//     the CIDR, equal to the network or broadcast address, or ordered with
//     endIP before startIP, and any range that includes the gateway address:
//     the gateway, the network address, and the broadcast address are never
//     allocatable. For the default lab network (clusterCIDR
//     192.168.124.0/24, gateway 192.168.124.1) the allocatable pool is
//     192.168.124.20..192.168.124.200.
//   - Allocate(key string) (string, error) returns the first free address in
//     the pool for key. It is deterministic and stable: the same key always
//     gets the same address, repeated calls for an already-allocated key do
//     not consume a second address, and two distinct keys never share an
//     address. When the pool is exhausted Allocate returns an error.
//   - Release(key string) frees the key's address so the next Allocate can
//     reuse it, first free address winning. Releasing a key that holds no
//     address is a no-op.
//   - Reserve(key string, ip string) error claims a specific in-range
//     address for key (for example to re-assert an address already recorded
//     in machine status). A later Allocate for the same key returns the
//     reserved address; other keys skip it. Reserving an out-of-range or
//     already-held address returns an error.
package ipam_test

import (
	"fmt"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ipam"
)

// Default lab network constants: the pool the machine contract allocates
// from. The gateway, the network address, and the broadcast address are
// excluded; the allocatable range is inclusive.
const (
	poolCIDR    = "192.168.124.0/24"
	poolGateway = "192.168.124.1"
	poolStart   = "192.168.124.20"
	poolEnd     = "192.168.124.200"

	// poolSize is the number of allocatable addresses in the default pool
	// (192.168.124.20..192.168.124.200 inclusive).
	poolSize = 200 - 20 + 1
)

// Compile-time pins: the constructor and the three methods must exist with
// exactly these names and signatures.
var (
	_ func(clusterCIDR, gateway, startIP, endIP string) (*ipam.Allocator, error) = ipam.NewAllocator
	_ func(*ipam.Allocator, string) (string, error)                              = (*ipam.Allocator).Allocate
	_ func(*ipam.Allocator, string)                                              = (*ipam.Allocator).Release
	_ func(*ipam.Allocator, string, string) error                                = (*ipam.Allocator).Reserve
)

// mustAllocator constructs an allocator for the given network and fails the
// test if the constructor rejects it.
func mustAllocator(t *testing.T, clusterCIDR, gateway, startIP, endIP string) *ipam.Allocator {
	t.Helper()

	a, err := ipam.NewAllocator(clusterCIDR, gateway, startIP, endIP)
	if err != nil {
		t.Fatalf("NewAllocator(%q, %q, %q, %q) error: %v", clusterCIDR, gateway, startIP, endIP, err)
	}

	return a
}

// mustAllocate allocates an address for key and fails the test on error.
func mustAllocate(t *testing.T, a *ipam.Allocator, key string) string {
	t.Helper()

	got, err := a.Allocate(key)
	if err != nil {
		t.Fatalf("Allocate(%q) error: %v", key, err)
	}

	return got
}

// TestAllocateFirstFit pins the first-fit order: sequential allocations take
// the pool start, then the next address, and so on.
func TestAllocateFirstFit(t *testing.T) {
	a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)

	want := []string{"192.168.124.20", "192.168.124.21", "192.168.124.22"}
	keys := []string{"machine-a", "machine-b", "machine-c"}
	for i, key := range keys {
		if got := mustAllocate(t, a, key); got != want[i] {
			t.Errorf("Allocate(%q) = %q, want %q", key, got, want[i])
		}
	}
}

// TestAllocateStablePerKey pins stability: the same key always gets the same
// address, and a repeated call does not consume a second address.
func TestAllocateStablePerKey(t *testing.T) {
	a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)

	first := mustAllocate(t, a, "machine-a")
	second := mustAllocate(t, a, "machine-a")
	if first != "192.168.124.20" || second != first {
		t.Fatalf("Allocate twice for the same key: first %q, second %q, want %q for both", first, second, "192.168.124.20")
	}

	// The repeated call must not have burned a second address: the next
	// distinct key gets the next pool address, not the one after it.
	if got := mustAllocate(t, a, "machine-b"); got != "192.168.124.21" {
		t.Errorf("Allocate after a repeat call = %q, want %q", got, "192.168.124.21")
	}
}

// TestAllocateDistinctKeys pins uniqueness: distinct keys never share an
// address, and the sequence matches the first-fit order.
func TestAllocateDistinctKeys(t *testing.T) {
	a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)

	seen := make(map[string]string, 10)
	for i := range 10 {
		key := fmt.Sprintf("machine-%d", i)
		got := mustAllocate(t, a, key)
		if other, dup := seen[got]; dup {
			t.Fatalf("Allocate(%q) returned %q already handed out to %q", key, got, other)
		}
		seen[got] = key

		if want := fmt.Sprintf("192.168.124.%d", 20+i); got != want {
			t.Errorf("Allocate(%q) = %q, want %q", key, got, want)
		}
	}
}

// TestReleaseReusesAddress pins release semantics: the freed address is the
// first free address, so a subsequent allocation reuses it; surviving
// allocations are untouched; releasing a key that holds no address is a
// no-op.
func TestReleaseReusesAddress(t *testing.T) {
	a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)

	ipA := mustAllocate(t, a, "machine-a")
	ipB := mustAllocate(t, a, "machine-b")
	if ipA != "192.168.124.20" || ipB != "192.168.124.21" {
		t.Fatalf("initial allocations: A %q, B %q", ipA, ipB)
	}

	// Releasing a key that holds no address must not disturb the pool.
	a.Release("never-allocated")

	a.Release("machine-a")
	if got := mustAllocate(t, a, "machine-c"); got != ipA {
		t.Errorf("Allocate after release = %q, want the freed address %q", got, ipA)
	}

	// The surviving allocation keeps its address.
	if got := mustAllocate(t, a, "machine-b"); got != ipB {
		t.Errorf("Allocate for the surviving key = %q, want %q", got, ipB)
	}

	// The same freed address is reused again after a second release.
	a.Release("machine-c")
	if got := mustAllocate(t, a, "machine-d"); got != ipA {
		t.Errorf("Allocate after second release = %q, want the freed address %q", got, ipA)
	}
}

// TestAllocateExhaustion pins the pool boundary: every pool address is
// allocatable exactly once, the last one is the pool end, and the next
// allocation fails.
func TestAllocateExhaustion(t *testing.T) {
	a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)

	for i := 0; i < poolSize; i++ {
		got := mustAllocate(t, a, fmt.Sprintf("machine-%03d", i))
		// The last iteration (i = poolSize-1 = 180) must yield
		// 192.168.124.200, the pool end.
		if want := fmt.Sprintf("192.168.124.%d", 20+i); got != want {
			t.Fatalf("Allocate(%d) = %q, want %q", i, got, want)
		}
	}
	if got, err := a.Allocate("machine-last"); err == nil {
		t.Errorf("Allocate on an exhausted pool = %q, want an error", got)
	}

	t.Run("single address pool", func(t *testing.T) {
		single := mustAllocator(t, poolCIDR, poolGateway, "192.168.124.50", "192.168.124.50")
		if got := mustAllocate(t, single, "machine-a"); got != "192.168.124.50" {
			t.Fatalf("Allocate = %q, want %q", got, "192.168.124.50")
		}
		if got, err := single.Allocate("machine-b"); err == nil {
			t.Errorf("Allocate on an exhausted single-address pool = %q, want an error", got)
		}
	})
}

// TestGatewayNotAllocatable pins the gateway exclusion: the host's address
// can never be handed out, and a pool that includes it is invalid at
// construction.
func TestGatewayNotAllocatable(t *testing.T) {
	// The gateway is never part of the pool: reserving it fails.
	a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)
	if err := a.Reserve("machine-a", poolGateway); err == nil {
		t.Error("Reserve of the gateway address succeeded, want an error")
	}

	// A pool that includes the gateway is rejected instead of handing the
	// host's address out.
	if _, err := ipam.NewAllocator(poolCIDR, poolGateway, poolGateway, "192.168.124.10"); err == nil {
		t.Error("NewAllocator with a range starting at the gateway succeeded, want an error")
	}
}

// TestNewAllocatorValidation pins the constructor's rejection rules.
func TestNewAllocatorValidation(t *testing.T) {
	tests := []struct {
		name        string
		clusterCIDR string
		gateway     string
		startIP     string
		endIP       string
	}{
		{"invalid CIDR", "not-a-cidr", poolGateway, poolStart, poolEnd},
		{"IPv6 CIDR", "fd00::/64", "fd00::1", "fd00::10", "fd00::20"},
		{"missing gateway", poolCIDR, "", poolStart, poolEnd},
		{"malformed gateway", poolCIDR, "not-an-ip", poolStart, poolEnd},
		{"gateway outside CIDR", poolCIDR, "10.0.0.1", poolStart, poolEnd},
		{"missing start", poolCIDR, poolGateway, "", poolEnd},
		{"missing end", poolCIDR, poolGateway, poolStart, ""},
		{"malformed start", poolCIDR, poolGateway, "not-an-ip", poolEnd},
		{"malformed end", poolCIDR, poolGateway, poolStart, "not-an-ip"},
		{"start outside CIDR", poolCIDR, poolGateway, "10.0.0.1", poolEnd},
		{"end outside CIDR", poolCIDR, poolGateway, poolStart, "10.0.0.1"},
		{"start is network address", poolCIDR, poolGateway, "192.168.124.0", poolEnd},
		{"end is broadcast address", poolCIDR, poolGateway, poolStart, "192.168.124.255"},
		{"start after end", poolCIDR, poolGateway, poolEnd, poolStart},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := ipam.NewAllocator(tt.clusterCIDR, tt.gateway, tt.startIP, tt.endIP); err == nil {
				t.Errorf("NewAllocator(%q, %q, %q, %q) succeeded, want an error", tt.clusterCIDR, tt.gateway, tt.startIP, tt.endIP)
			}
		})
	}
}

// TestReserve pins the reserve contract: an in-range reservation is skipped
// by first-fit for other keys and returned to its own key; out-of-range and
// conflicting reservations fail.
func TestReserve(t *testing.T) {
	t.Run("reserved address is skipped and returned to its key", func(t *testing.T) {
		a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)
		if err := a.Reserve("machine-b", "192.168.124.50"); err != nil {
			t.Fatalf("Reserve(%q, %q) error: %v", "machine-b", "192.168.124.50", err)
		}

		if got := mustAllocate(t, a, "machine-a"); got != "192.168.124.20" {
			t.Errorf("Allocate before the reserved address = %q, want %q", got, "192.168.124.20")
		}
		// First-fit continues past the reserved address instead of taking it.
		if got := mustAllocate(t, a, "machine-c"); got != "192.168.124.21" {
			t.Errorf("Allocate past the reserved address = %q, want %q", got, "192.168.124.21")
		}
		if got := mustAllocate(t, a, "machine-b"); got != "192.168.124.50" {
			t.Errorf("Allocate for the reserved key = %q, want %q", got, "192.168.124.50")
		}
	})

	t.Run("out of range", func(t *testing.T) {
		a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)
		for _, ip := range []string{
			"192.168.124.19",  // below the pool start
			"192.168.124.201", // above the pool end
			poolGateway,       // the host's address
			"192.168.124.0",   // the network address
			"192.168.124.255", // the broadcast address
			"not-an-ip",
		} {
			if err := a.Reserve("machine-a", ip); err == nil {
				t.Errorf("Reserve(%q) succeeded, want an error", ip)
			}
		}
	})

	t.Run("conflicts with an allocated address", func(t *testing.T) {
		a := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)
		mustAllocate(t, a, "machine-a")
		if err := a.Reserve("machine-b", "192.168.124.20"); err == nil {
			t.Error("Reserve of an address already allocated to another key succeeded, want an error")
		}
	})
}

// TestAllocatorIndependence pins that allocators hold no shared state: two
// instances with the same configuration produce the same first-fit sequence,
// and releasing in one never frees an address in the other.
func TestAllocatorIndependence(t *testing.T) {
	first := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)
	second := mustAllocator(t, poolCIDR, poolGateway, poolStart, poolEnd)

	firstIP := mustAllocate(t, first, "machine-a")
	secondIP := mustAllocate(t, second, "machine-a")
	if firstIP != secondIP || firstIP != "192.168.124.20" {
		t.Fatalf("allocators diverged: first %q, second %q", firstIP, secondIP)
	}

	first.Release("machine-a")
	if got := mustAllocate(t, second, "machine-b"); got != "192.168.124.21" {
		t.Errorf("release leaked across allocators: second Allocate = %q, want %q", got, "192.168.124.21")
	}
}
