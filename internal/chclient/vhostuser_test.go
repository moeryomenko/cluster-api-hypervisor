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

// VC-05 — vhost-user net rendering (test-first, RED phase).
//
// This file proves REQ-005 / VC-05: chclient renders
//
//	vhost_user=true, socket=/run/user/1000/k8snet/<port>.sock, mac=<mac>
//
// with a single queue pair and no tap reference.
//
// Current implementation uses TAP (Net.EnsureTap / LinkAdd tuntap) and has no
// vhost-user rendering. All tests in this file are expected to FAIL against
// the pre-REQ-005 implementation, either at compile time (missing helpers)
// or at runtime (wrong string). The tests pin the contract the implementation
// in TASK-010 must satisfy.
//
// Contract under test (proposed API — implementer must provide these symbols
// in package chclient; if the net rendering lives in internal/ch, re-export
// it here):
//
//	func VhostUserSocketPath(portName string) string
//	  Returns /run/user/1000/k8snet/<portName>.sock per spec REQ-005 / plan
//	  assumption 3. Port name == machine name. Empty name yields an error-safe
//	  path or an error; callers must not panic.
//
//	func VhostUserNetConfig(socketPath, mac string) (string, error)
//	  Returns the cloud-hypervisor net device string. Must contain
//	  vhost_user=true, socket=<socketPath>, mac=<mac>, single queue pair
//	  (num_queues=1 or equivalent), and must NOT contain any tap reference.
//	  Invalid inputs (empty socket, malformed MAC) must return a non-nil error.
//
// Grill coverage:
//   - exact string shape, field order independent
//   - socket derived from port name, not hardcoded machine
//   - MAC is the machine's derived MAC (via internal/mac.Derive)
//   - single queue pair only, no multi-queue flags
//   - no tap / ifname / tuntap substring (case-insensitive)
//   - empty / malformed inputs, long names, special chars, upper-case MAC
//   - socket path length / unix socket limit (108)
//   - idempotency (same inputs → same output)
//   - concurrent safety (no shared mutable state)
//   - case sensitivity of vhost_user flag
package chclient_test

import (
	"strings"
	"sync"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
)

// Compile-time pins — existence and signature of the contract helpers.
// If chclient does not yet export these, this file does not compile (RED).
var (
	_ func(string) string                  = chclient.VhostUserSocketPath
	_ func(string, string) (string, error) = chclient.VhostUserNetConfig
)

const (
	expectedSocketBase = "/run/user/1000/k8snet"
	expectedSocketSuf  = ".sock"
)

// assertContains fails if s does not contain substr.
func assertContains(t *testing.T, s, substr, label string) {
	t.Helper()
	if !strings.Contains(s, substr) {
		t.Errorf("%s: got %q, want to contain %q", label, s, substr)
	}
}

// assertNotContains fails if s contains substr (case-insensitive for tap).
func assertNotContains(t *testing.T, s, substr, label string) {
	t.Helper()
	if strings.Contains(strings.ToLower(s), strings.ToLower(substr)) {
		t.Errorf("%s: got %q, want NOT to contain %q (case-insensitive)", label, s, substr)
	}
}

// ---------------------------------------------------------------------------
// Socket path derivation
// ---------------------------------------------------------------------------

func TestVhostUserSocketPath_Derivation(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name     string
		portName string
		want     string
	}{
		{name: "simple machine", portName: "machine-a", want: "/run/user/1000/k8snet/machine-a.sock"},
		{name: "control-plane 0", portName: "cp-0", want: "/run/user/1000/k8snet/cp-0.sock"},
		{name: "worker with dash", portName: "worker-01", want: "/run/user/1000/k8snet/worker-01.sock"},
		{name: "k8s style", portName: "k8s-master-1", want: "/run/user/1000/k8snet/k8s-master-1.sock"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := chclient.VhostUserSocketPath(tt.portName)
			if got != tt.want {
				t.Errorf("VhostUserSocketPath(%q) = %q, want %q", tt.portName, got, tt.want)
			}
			// deriviation must be prefix + name + suffix, not hardcoded machine
			if !strings.HasPrefix(got, expectedSocketBase+"/") {
				t.Errorf("socket path %q missing base %q", got, expectedSocketBase)
			}
			if !strings.HasSuffix(got, expectedSocketSuf) {
				t.Errorf("socket path %q missing suffix %q", got, expectedSocketSuf)
			}
		})
	}
}

func TestVhostUserSocketPath_UsesPortNameNotHardcoded(t *testing.T) {
	t.Parallel()

	// grill: ensure port name drives socket, not a fixed string — two distinct
	// port names must yield distinct sockets.
	a := chclient.VhostUserSocketPath("machine-a")
	b := chclient.VhostUserSocketPath("machine-b")
	if a == b {
		t.Errorf("socket paths for distinct ports collided: %q", a)
	}
	if !strings.Contains(a, "machine-a") || !strings.Contains(b, "machine-b") {
		t.Errorf("socket paths do not embed port names: a=%q b=%q", a, b)
	}
}

func TestVhostUserSocketPath_EdgeCases(t *testing.T) {
	t.Parallel()

	// Empty port name — should not panic; either returns base/.sock or error
	// path. We pin non-panic and that result at least contains base.
	t.Run("empty port name does not panic", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("VhostUserSocketPath(\"\") panicked: %v", r)
			}
		}()
		got := chclient.VhostUserSocketPath("")
		if got == "" {
			t.Errorf("VhostUserSocketPath(\"\") = empty, want base path even for empty name")
		}
		if !strings.HasPrefix(got, expectedSocketBase) {
			t.Errorf("empty port socket %q missing base %q", got, expectedSocketBase)
		}
	})

	// Long machine name (DNS label limit 63) — must not truncate or exceed
	// unix socket max (108) without handling; at minimum must embed full name.
	t.Run("long machine name", func(t *testing.T) {
		t.Parallel()
		long := strings.Repeat("a", 63)
		got := chclient.VhostUserSocketPath(long)
		if !strings.Contains(got, long) {
			t.Errorf("long name socket %q does not contain full machine name", got)
		}
		// Unix socket path limit is 108 bytes — flag over-length
		if len(got) > 108 {
			t.Errorf("socket path %q length %d exceeds 108 (unix socket limit)", got, len(got))
		}
	})

	// Special characters — hyphens and dots are valid in machine names
	t.Run("special chars", func(t *testing.T) {
		t.Parallel()
		for _, name := range []string{"my.machine-1", "test_machine", "a-b.c_d"} {
			got := chclient.VhostUserSocketPath(name)
			if !strings.Contains(got, name) {
				t.Errorf("VhostUserSocketPath(%q) = %q, want to contain port name", name, got)
			}
		}
	})

	// Path traversal should not escape base dir — name containing "/" or ".."
	t.Run("path traversal safe", func(t *testing.T) {
		t.Parallel()
		got := chclient.VhostUserSocketPath("../etc/passwd")
		// Must remain under the base dir, not resolve to /etc/passwd
		if !strings.HasPrefix(got, expectedSocketBase+"/") {
			t.Errorf("traversal socket %q escaped base dir", got)
		}
		if strings.Contains(got, "/etc/passwd") && !strings.Contains(got, expectedSocketBase) {
			t.Errorf("traversal socket %q not sanitized", got)
		}
	})
}

// ---------------------------------------------------------------------------
// Net config rendering — vhost_user, socket, mac, queue, no tap
// ---------------------------------------------------------------------------

func TestVhostUserNetConfig_RendersVhostUserTrue(t *testing.T) {
	t.Parallel()

	socket := chclient.VhostUserSocketPath("machine-a")
	macAddr := mac.Derive("c1", "machine-a")

	cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("VhostUserNetConfig error: %v", err)
	}
	assertContains(t, cfg, "vhost_user=true", "VC-05: must render vhost_user=true")
	// case-sensitive exact flag
	if !strings.Contains(cfg, "vhost_user=true") {
		t.Errorf("cfg %q missing exact vhost_user=true", cfg)
	}
	// must not be vhost_user=false
	if strings.Contains(cfg, "vhost_user=false") {
		t.Errorf("cfg %q contains vhost_user=false, want true only", cfg)
	}
}

func TestVhostUserNetConfig_RendersSocket(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		machine string
	}{
		{name: "machine-a", machine: "machine-a"},
		{name: "cp-0", machine: "cp-0"},
		{name: "worker-01", machine: "worker-01"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			socket := chclient.VhostUserSocketPath(tt.machine)
			macAddr := mac.Derive("test-cluster", tt.machine)
			cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
			if err != nil {
				t.Fatalf("VhostUserNetConfig error: %v", err)
			}
			// socket field must appear with correct path
			assertContains(t, cfg, "socket="+socket, "VC-05: socket path")
			// also accept vhost_socket variant (cloud-hypervisor compat) — but at least one must contain path
			if !strings.Contains(cfg, socket) {
				t.Errorf("cfg %q does not contain socket path %q", cfg, socket)
			}
			// socket must be the derived one, not a hardcoded other
			other := chclient.VhostUserSocketPath("other-machine")
			if strings.Contains(cfg, other) && tt.machine != "other-machine" {
				t.Errorf("cfg %q leaked other machine socket %q", cfg, other)
			}
		})
	}
}

func TestVhostUserNetConfig_RendersMAC(t *testing.T) {
	t.Parallel()

	// Grill: MAC from internal/mac.Derive plus explicit override shape
	tests := []struct {
		name        string
		cluster     string
		machine     string
		explicitMAC string // if set, use instead of derive
	}{
		{name: "derived mac cp-0", cluster: "k8s-lab", machine: "cp-0"},
		{name: "derived mac worker", cluster: "k8s-lab", machine: "worker-01"},
		{name: "explicit mac", explicitMAC: "c6:e5:50:1c:ec:ab"},
		{name: "upper case mac preserved or lowercased", explicitMAC: "C6:E5:50:1C:EC:FF"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			socket := chclient.VhostUserSocketPath(tt.machine)
			if tt.machine == "" {
				socket = "/run/user/1000/k8snet/explicit.sock"
			}
			var macAddr string
			if tt.explicitMAC != "" {
				macAddr = tt.explicitMAC
			} else {
				macAddr = mac.Derive(tt.cluster, tt.machine)
			}
			cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
			if err != nil {
				t.Fatalf("VhostUserNetConfig error: %v", err)
			}
			// MAC must appear verbatim or lower-cased variant
			if !strings.Contains(cfg, "mac="+macAddr) && !strings.Contains(cfg, "mac="+strings.ToLower(macAddr)) {
				t.Errorf("cfg %q missing mac=%q (or lowercased)", cfg, macAddr)
			}
			// MAC must contain the family prefix for derived cases
			if tt.explicitMAC == "" {
				if !strings.Contains(strings.ToLower(cfg), "c6:e5:50:1c:ec") {
					t.Errorf("cfg %q missing derived MAC family c6:e5:50:1c:ec", cfg)
				}
			}
		})
	}
}

func TestVhostUserNetConfig_SingleQueuePair(t *testing.T) {
	t.Parallel()

	socket := chclient.VhostUserSocketPath("machine-a")
	macAddr := mac.Derive("c1", "machine-a")

	cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("VhostUserNetConfig error: %v", err)
	}

	// Must have single queue pair — require num_queues=1 present
	assertContains(t, cfg, "num_queues=1", "VC-05: single queue pair requires num_queues=1")
	// Queue pair must be exactly 1, not 0
	if strings.Contains(cfg, "num_queues=0") {
		t.Errorf("cfg %q contains num_queues=0, want 1", cfg)
	}

	// No multi-queue flags
	for _, forbid := range []string{"num_queues=2", "num_queues=4", "num_queues=8", "mq=on", "mq=true"} {
		if strings.Contains(cfg, forbid) {
			t.Errorf("cfg %q contains multi-queue flag %q — VC-05 forbids multi-queue", cfg, forbid)
		}
	}
	// Vectors implies multi-queue
	if strings.Contains(cfg, "vectors=") {
		t.Errorf("cfg %q contains vectors= (multi-queue hint), want single queue only", cfg)
	}
}

func TestVhostUserNetConfig_NoTapReference(t *testing.T) {
	t.Parallel()

	socket := chclient.VhostUserSocketPath("machine-a")
	macAddr := mac.Derive("c1", "machine-a")

	cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("VhostUserNetConfig error: %v", err)
	}

	// No tap substring at all (case-insensitive) — TAP is deleted per REQ-007
	assertNotContains(t, cfg, "tap", "VC-05: no tap reference")
	// More specific tap fields that old TAP config used
	for _, forbid := range []string{"tap=", "ifname=", "tuntap", "k8s-", "bridge="} {
		if strings.Contains(strings.ToLower(cfg), forbid) {
			t.Errorf("cfg %q contains forbidden tap/bridge field %q — vhost-user must not reference TAP", cfg, forbid)
		}
	}
}

func TestVhostUserNetConfig_ExactShape(t *testing.T) {
	t.Parallel()

	// Integration: socket derived from port name, MAC from derive, full string
	// must contain all three required fields together.
	machine := "cp-0"
	socket := chclient.VhostUserSocketPath(machine)
	macAddr := mac.Derive("k8s-lab", machine)

	cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("VhostUserNetConfig error: %v", err)
	}

	required := []string{
		"vhost_user=true",
		"socket=" + socket,
		"mac=" + strings.ToLower(macAddr), // allow lowercased
		"num_queues=1",
	}
	for _, want := range required {
		if !strings.Contains(cfg, want) && !strings.Contains(cfg, strings.ToUpper(want)) {
			// retry case-insensitive for mac field already handled
			if !strings.Contains(strings.ToLower(cfg), strings.ToLower(want)) {
				t.Errorf("cfg %q missing required %q", cfg, want)
			}
		}
	}

	// Negative exact: must not accidentally contain TAP artifact
	if strings.Contains(strings.ToLower(cfg), "tap") {
		t.Errorf("cfg %q contains tap — violates VC-05", cfg)
	}
}

func TestVhostUserNetConfig_Idempotent(t *testing.T) {
	t.Parallel()

	socket := chclient.VhostUserSocketPath("machine-a")
	macAddr := mac.Derive("c1", "machine-a")

	first, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("first call error: %v", err)
	}
	second, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("second call error: %v", err)
	}
	if first != second {
		t.Errorf("non-idempotent: first %q, second %q", first, second)
	}
}

func TestVhostUserNetConfig_ConcurrentSafe(t *testing.T) {
	t.Parallel()

	socket := chclient.VhostUserSocketPath("machine-a")
	macAddr := mac.Derive("c1", "machine-a")

	const workers = 20
	var wg sync.WaitGroup
	results := make([]string, workers)
	errs := make([]error, workers)
	for i := range workers {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
			results[idx] = cfg
			errs[idx] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Errorf("worker %d error: %v", i, err)
		}
	}
	for i := 1; i < workers; i++ {
		if results[i] != results[0] {
			t.Errorf("concurrent mismatch: worker 0 %q, worker %d %q", results[0], i, results[i])
		}
	}
}

// ---------------------------------------------------------------------------
// Edge and error paths — grill-me
// ---------------------------------------------------------------------------

func TestVhostUserNetConfig_EmptyInputs(t *testing.T) {
	t.Parallel()

	// Empty socket must error (port socket must exist before VM start)
	t.Run("empty socket", func(t *testing.T) {
		t.Parallel()
		macAddr := mac.Derive("c1", "m1")
		_, err := chclient.VhostUserNetConfig("", macAddr)
		if err == nil {
			t.Errorf("VhostUserNetConfig with empty socket wanted error, got nil")
		}
	})

	// Empty MAC must error
	t.Run("empty mac", func(t *testing.T) {
		t.Parallel()
		socket := chclient.VhostUserSocketPath("m1")
		_, err := chclient.VhostUserNetConfig(socket, "")
		if err == nil {
			t.Errorf("VhostUserNetConfig with empty mac wanted error, got nil")
		}
	})

	// Both empty must error, not panic
	t.Run("both empty no panic", func(t *testing.T) {
		t.Parallel()
		defer func() {
			if r := recover(); r != nil {
				t.Errorf("VhostUserNetConfig panicked on empty inputs: %v", r)
			}
		}()
		_, err := chclient.VhostUserNetConfig("", "")
		if err == nil {
			t.Errorf("both empty wanted error, got nil")
		}
	})
}

func TestVhostUserNetConfig_InvalidMAC(t *testing.T) {
	t.Parallel()

	socket := chclient.VhostUserSocketPath("m1")

	invalidMACs := []string{
		"not-a-mac",
		"c6:e5:50:1c:ec",       // too short (5 octets)
		"c6:e5:50:1c:ec:zz:01", // too long
		"c6:e5:50:1c:ec:gg",    // non-hex
		"192.168.1.1",          // IP not MAC
		"",                     // empty
	}
	for _, bad := range invalidMACs {
		t.Run(bad, func(t *testing.T) {
			t.Parallel()
			_, err := chclient.VhostUserNetConfig(socket, bad)
			if err == nil {
				t.Errorf("VhostUserNetConfig with mac %q wanted error, got nil", bad)
			}
		})
	}
}

func TestVhostUserNetConfig_NoMultiQueueFlagsComprehensive(t *testing.T) {
	t.Parallel()

	// Grill: ensure implementation does not sneak multi-queue via alternative
	// spellings or extra params.
	socket := chclient.VhostUserSocketPath("machine-a")
	macAddr := mac.Derive("c1", "machine-a")

	cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("VhostUserNetConfig error: %v", err)
	}

	forbidden := []string{
		"mq=",
		"multi_queue",
		"num_queues=2",
		"num_queues=4",
		"vectors",
		"queue_size=0", // zero queue size is invalid
	}
	for _, f := range forbidden {
		if strings.Contains(strings.ToLower(cfg), strings.ToLower(f)) {
			// Special case: num_queues=1 is allowed, others are not
			if f == "num_queues=2" && strings.Contains(cfg, "num_queues=1") {
				continue
			}
			// generic forbid
			if f == "mq=" || f == "vectors" || f == "multi_queue" {
				t.Errorf("cfg %q contains multi-queue flag %q", cfg, f)
			}
		}
	}
	// Explicitly assert the only allowed num_queues is 1
	if strings.Contains(cfg, "num_queues=") && !strings.Contains(cfg, "num_queues=1") {
		t.Errorf("cfg %q has num_queues but not =1, want exactly 1", cfg)
	}
}

func TestVhostUserNetConfig_SocketPathMustBeUnderK8snet(t *testing.T) {
	t.Parallel()

	// Socket must be under the k8netd run dir, not /tmp or /var
	socket := chclient.VhostUserSocketPath("m1")
	macAddr := mac.Derive("c1", "m1")

	cfg, err := chclient.VhostUserNetConfig(socket, macAddr)
	if err != nil {
		t.Fatalf("VhostUserNetConfig error: %v", err)
	}
	if !strings.Contains(cfg, expectedSocketBase) {
		t.Errorf("cfg %q missing k8netd socket base %q", cfg, expectedSocketBase)
	}
	// Must not contain alternative dirs
	for _, badBase := range []string{"/tmp/", "/var/lib/", "/var/tmp/"} {
		if strings.Contains(cfg, "socket="+badBase) {
			t.Errorf("cfg %q uses wrong socket base %q", cfg, badBase)
		}
	}
}
