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

// VMClient EnsureRunning ordering contract (test-first).
//
// This suite pins how VMClient drives the cloud-hypervisor REST API once the
// subprocess and its socket are up. It is exercised against a real HTTP
// server on a unix socket that records the endpoint sequence and the pushed
// configuration, so no hypervisor is needed. The contract, in prose:
//
//   - A missing VM gets the full configuration pushed with vm.create and
//     then boots: the recorded sequence is info, create, boot. Both absent-VM
//     answers classify as missing: vm.info status 404, and status 500 with a
//     body naming the missing VM ("VM is not created") on some
//     cloud-hypervisor versions. The create body carries the firmware path,
//     every disk path, and the parsed vhost-user net parameters (socket,
//     mac, num_queues raised to the cloud-hypervisor floor of 2), plus
//     shared guest memory for the vhost-user device. The memory size is the
//     spec RAM converted from MiB to bytes (SetRAM), and the cpus section
//     carries the spec vCPU count as both boot_vcpus and max_vcpus (SetCPU);
//     an absent or non-positive CPU omits the cpus section so cloud-hypervisor
//     applies its own default, and an absent or non-positive RAM falls back to
//     the cloud-hypervisor default memory footprint.
//   - A VM already in the Created state keeps its configuration: the
//     sequence is info, boot with no second create (idempotent re-run).
//   - A Running VM is a no-op: the sequence is info only.
//   - A VM in any other reported state (for example Shutdown) boots again
//     without a re-push: info, boot.
//   - An Info failure that is not an absent-VM answer (a 500 without the
//     "VM is not created" body, or a transport fault) surfaces unchanged and
//     issues no create or boot.
//   - A configuration push without a firmware configured fails before any
//     API call beyond info.
package chclient

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// apiRecorder records the endpoint sequence and the vm.create body, and
// answers each endpoint with a scripted status.
type apiRecorder struct {
	mu        sync.Mutex
	sequence  []string
	createBod []byte

	infoStatus  int
	infoPayload string
}

func (r *apiRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.sequence = append(r.sequence, req.URL.Path)
	switch req.URL.Path {
	case "/api/v1/vm.info":
		w.WriteHeader(r.infoStatus)
		_, _ = w.Write([]byte(r.infoPayload))
	case "/api/v1/vm.create":
		body := make([]byte, req.ContentLength)
		_, _ = req.Body.Read(body)
		r.createBod = body
		w.WriteHeader(http.StatusNoContent)
	default:
		w.WriteHeader(http.StatusNoContent)
	}
}

func (r *apiRecorder) calls() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.sequence...)
}

func (r *apiRecorder) createBody() []byte {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]byte(nil), r.createBod...)
}

// newTestVMClient builds a VMClient whose manager is inert and whose API
// client points at a unix-socket test server. Only ensureBooted is exercised
// through it; the process-spawning half of EnsureRunning is covered by the
// Manager's own suite.
func newTestVMClient(t *testing.T, handler http.Handler) *VMClient {
	t.Helper()

	srv := httptest.NewUnstartedServer(handler)
	if err := srv.Listener.Close(); err != nil {
		t.Fatalf("close default listener: %v", err)
	}
	socketPath := filepath.Join(t.TempDir(), "api.sock")
	listener, err := net.Listen("unix", socketPath)
	if err != nil {
		t.Fatalf("listen on unix socket %s: %v", socketPath, err)
	}
	srv.Listener = listener
	srv.Start()
	t.Cleanup(srv.Close)

	return &VMClient{
		client: ch.NewClient(socketPath),
		socket: socketPath,
	}
}

// scriptedVMClient returns a client wired to rec and pre-loaded with the
// boot configuration every happy-path case pushes: the vhost-user net device,
// the firmware, the disk images, and the spec cpu/ram shape (2 vCPUs, 2048
// MiB) the machine controller hands over.
func scriptedVMClient(t *testing.T, rec *apiRecorder) *VMClient {
	t.Helper()
	c := newTestVMClient(t, rec)
	c.SetNetConfig("vhost_user=true,socket=/run/user/1000/k8snet/node-1.sock,mac=c6:e5:50:1c:ec:ab,num_queues=1")
	c.SetFirmware("/build/CLOUDHV.fd")
	c.SetDiskPaths([]string{
		"/build/vm-disks/node-1-root.qcow2",
		"/build/vm-disks/node-1-data/z-kubelet.raw",
	})
	c.SetCPU(2)
	c.SetRAM(2048)
	return c
}

// TestEnsureBootedCreatesThenBoots pins the first-boot flow: info answers
// 404 (no VM), so the full config is pushed and the VM boots, in that order.
// The pushed config carries the spec cpu/ram shape: a cpus section with the
// vCPU count as boot_vcpus and max_vcpus, and a memory size converted from
// the spec RAM in MiB to bytes.
func TestEnsureBootedCreatesThenBoots(t *testing.T) {
	rec := &apiRecorder{infoStatus: http.StatusNotFound}
	c := scriptedVMClient(t, rec)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	want := []string{"/api/v1/vm.info", "/api/v1/vm.create", "/api/v1/vm.boot"}
	if got := rec.calls(); !equalStrings(got, want) {
		t.Fatalf("endpoint sequence = %v, want %v", got, want)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.createBody(), &cfg); err != nil {
		t.Fatalf("create body is not valid JSON: %v (body %q)", err, rec.createBody())
	}
	assertCreateField(t, cfg, "payload.firmware", "/build/CLOUDHV.fd")
	assertCreateField(t, cfg, "cpus.boot_vcpus", float64(2))
	assertCreateField(t, cfg, "cpus.max_vcpus", float64(2))
	assertCreateField(t, cfg, "disks.0.path", "/build/vm-disks/node-1-root.qcow2")
	assertCreateField(t, cfg, "disks.1.path", "/build/vm-disks/node-1-data/z-kubelet.raw")
	assertCreateField(t, cfg, "net.0.vhost_user", true)
	assertCreateField(t, cfg, "net.0.vhost_socket", "/run/user/1000/k8snet/node-1.sock")
	assertCreateField(t, cfg, "net.0.mac", "c6:e5:50:1c:ec:ab")
	assertCreateField(t, cfg, "net.0.num_queues", float64(2))
	assertCreateField(t, cfg, "memory.shared", true)
	assertCreateField(t, cfg, "memory.size", float64(2048*1024*1024))
}

// TestEnsureBootedAbsentCPUOmitsCpusField pins the graceful absent-CPU
// contract: a zero vCPU count (spec left unset) omits the cpus section so
// cloud-hypervisor applies its own default, while the spec-derived memory
// size is still pushed.
func TestEnsureBootedAbsentCPUOmitsCpusField(t *testing.T) {
	rec := &apiRecorder{infoStatus: http.StatusNotFound}
	c := scriptedVMClient(t, rec)
	c.SetCPU(0)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.createBody(), &cfg); err != nil {
		t.Fatalf("create body is not valid JSON: %v (body %q)", err, rec.createBody())
	}
	assertCreateFieldAbsent(t, cfg, "cpus")
	assertCreateField(t, cfg, "memory.size", float64(2048*1024*1024))
}

// TestEnsureBootedAbsentRAMFallsBackToDefault pins the graceful absent-RAM
// contract: a zero memory size (spec left unset) falls back to the
// cloud-hypervisor default footprint instead of pushing a zero-byte memory
// section, while the spec-derived vCPU count is still pushed.
func TestEnsureBootedAbsentRAMFallsBackToDefault(t *testing.T) {
	rec := &apiRecorder{infoStatus: http.StatusNotFound}
	c := scriptedVMClient(t, rec)
	c.SetRAM(0)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.createBody(), &cfg); err != nil {
		t.Fatalf("create body is not valid JSON: %v (body %q)", err, rec.createBody())
	}
	assertCreateField(t, cfg, "cpus.boot_vcpus", float64(2))
	assertCreateField(t, cfg, "cpus.max_vcpus", float64(2))
	assertCreateField(t, cfg, "memory.size", float64(ch.DefaultMemorySize))
}

// TestEnsureBootedNegativeSpecValuesFallBack pins the graceful negative-spec
// contract: the webhook rejects CPU/RAM <= 0 at admission, but a machine that
// slips through (or a hand-built config) must not push a negative vCPU count
// or a negative memory size. Negative values behave like absent ones: no cpus
// section, and the default memory footprint.
func TestEnsureBootedNegativeSpecValuesFallBack(t *testing.T) {
	rec := &apiRecorder{infoStatus: http.StatusNotFound}
	c := scriptedVMClient(t, rec)
	c.SetCPU(-1)
	c.SetRAM(-1)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.createBody(), &cfg); err != nil {
		t.Fatalf("create body is not valid JSON: %v (body %q)", err, rec.createBody())
	}
	assertCreateFieldAbsent(t, cfg, "cpus")
	assertCreateField(t, cfg, "memory.size", float64(ch.DefaultMemorySize))
}

// TestEnsureBootedCIDATADiskReadonly pins requirement 5: the CIDATA disk is
// attached read-only. The vm.create body must carry readonly: true for the
// CIDATA disk entry while the root and confext disks stay writable (no
// readonly key on their entries).
func TestEnsureBootedCIDATADiskReadonly(t *testing.T) {
	rec := &apiRecorder{infoStatus: http.StatusNotFound}
	c := newTestVMClient(t, rec)
	c.SetNetConfig("vhost_user=true,socket=/run/user/1000/k8snet/node-1.sock,mac=c6:e5:50:1c:ec:ab,num_queues=1")
	c.SetFirmware("/build/CLOUDHV.fd")
	c.SetDiskPaths([]string{
		"/build/vm-disks/node-1-root.qcow2",
		"/build/vm-disks/node-1-cidata.img",
		"/build/vm-disks/node-1-data/z-kubelet.raw",
	})

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	var cfg map[string]any
	if err := json.Unmarshal(rec.createBody(), &cfg); err != nil {
		t.Fatalf("create body is not valid JSON: %v (body %q)", err, rec.createBody())
	}
	assertCreateField(t, cfg, "disks.1.path", "/build/vm-disks/node-1-cidata.img")
	assertCreateField(t, cfg, "disks.1.readonly", true)

	// The root and confext disks stay writable: no readonly key on their
	// entries. The CIDATA disk sits at index 1, so the writable entries are
	// disks[0] (root) and disks[2] (confext).
	disks, ok := cfg["disks"].([]any)
	if !ok || len(disks) != 3 {
		t.Fatalf("create body disks = %#v, want 3 entries", cfg["disks"])
	}
	for i, name := range []string{"root", "confext"} {
		idx := i * 2 // skip the CIDATA disk at index 1
		entry, ok := disks[idx].(map[string]any)
		if !ok {
			t.Fatalf("disks.%d = %#v, want an object", idx, disks[idx])
		}
		if _, ok := entry["readonly"]; ok {
			t.Errorf("disks.%d (%s) carries readonly %v, want writable", idx, name, entry["readonly"])
		}
	}
}

// TestEnsureBootedAbsentVMShapes pins that both absent-VM answers trigger
// the create-then-boot flow: vm.info status 404, and status 500 with an
// "VM is not created" body as some cloud-hypervisor versions answer. A 500
// without that body is not an absent VM and must surface unchanged.
func TestEnsureBootedAbsentVMShapes(t *testing.T) {
	tests := []struct {
		name        string
		infoStatus  int
		infoPayload string
		wantErr     bool
	}{
		{
			name:       "404 absent VM",
			infoStatus: http.StatusNotFound,
		},
		{
			name:        "500 with VM is not created body",
			infoStatus:  http.StatusInternalServerError,
			infoPayload: `["VM is not created"]`,
		},
		{
			name:        "500 with unrelated body surfaces",
			infoStatus:  http.StatusInternalServerError,
			infoPayload: `["Internal error"]`,
			wantErr:     true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := &apiRecorder{infoStatus: tt.infoStatus, infoPayload: tt.infoPayload}
			c := scriptedVMClient(t, rec)

			err := c.ensureBooted(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Fatal("ensureBooted() = nil, want error")
				}
				want := []string{"/api/v1/vm.info"}
				if got := rec.calls(); !equalStrings(got, want) {
					t.Fatalf("endpoint sequence = %v, want %v (no create/boot expected)", got, want)
				}
				return
			}
			if err != nil {
				t.Fatalf("ensureBooted() error = %v, want nil", err)
			}
			want := []string{"/api/v1/vm.info", "/api/v1/vm.create", "/api/v1/vm.boot"}
			if got := rec.calls(); !equalStrings(got, want) {
				t.Fatalf("endpoint sequence = %v, want %v", got, want)
			}
		})
	}
}

// TestIsVMAbsent pins the absent-VM classification: status 404 always, and
// status 500 only when the captured body names the missing VM,
// case-insensitively and through wrapped error chains.
func TestIsVMAbsent(t *testing.T) {
	tests := []struct {
		name string
		give error
		want bool
	}{
		{name: "404 is absent", give: &ch.StatusError{StatusCode: http.StatusNotFound}, want: true},
		{
			name: "500 with marker body is absent",
			give: &ch.StatusError{StatusCode: http.StatusInternalServerError, Body: `["VM is not created"]`},
			want: true,
		},
		{
			name: "500 with lowercase marker body is absent",
			give: &ch.StatusError{StatusCode: http.StatusInternalServerError, Body: `{"error":"vm is not created"}`},
			want: true,
		},
		{
			name: "wrapped 404 is absent",
			give: fmt.Errorf("ensure booted: %w", &ch.StatusError{StatusCode: http.StatusNotFound}),
			want: true,
		},
		{
			name: "500 without marker body is not absent",
			give: &ch.StatusError{StatusCode: http.StatusInternalServerError, Body: `["Internal error"]`},
			want: false,
		},
		{
			name: "403 is not absent",
			give: &ch.StatusError{StatusCode: http.StatusForbidden, Body: "forbidden"},
			want: false,
		},
		{name: "transport error is not absent", give: errors.New("connection refused"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isVMAbsent(tt.give); got != tt.want {
				t.Errorf("isVMAbsent(%v) = %v, want %v", tt.give, got, tt.want)
			}
		})
	}
}

// TestEnsureBootedCreatedStateSkipsCreate pins idempotency: a re-run against
// a VM still in the Created state boots again but never re-pushes the
// configuration.
func TestEnsureBootedCreatedStateSkipsCreate(t *testing.T) {
	rec := &apiRecorder{
		infoStatus:  http.StatusOK,
		infoPayload: `{"state":"Created"}`,
	}
	c := scriptedVMClient(t, rec)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	want := []string{"/api/v1/vm.info", "/api/v1/vm.boot"}
	if got := rec.calls(); !equalStrings(got, want) {
		t.Fatalf("endpoint sequence = %v, want %v", got, want)
	}
	if len(rec.createBody()) != 0 {
		t.Errorf("a create body was pushed (%q), want none", rec.createBody())
	}
}

// TestEnsureBootedRunningIsNoOp pins that a running VM triggers no further
// API traffic.
func TestEnsureBootedRunningIsNoOp(t *testing.T) {
	rec := &apiRecorder{
		infoStatus:  http.StatusOK,
		infoPayload: `{"state":"Running"}`,
	}
	c := scriptedVMClient(t, rec)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	want := []string{"/api/v1/vm.info"}
	if got := rec.calls(); !equalStrings(got, want) {
		t.Fatalf("endpoint sequence = %v, want %v", got, want)
	}
}

// TestEnsureBootedShutdownBootsAgain pins that a VM reporting Shutdown (a
// guest-initiated shutdown leaves the process alive) boots again without a
// configuration re-push, matching the pre-existing boot-only behavior.
func TestEnsureBootedShutdownBootsAgain(t *testing.T) {
	rec := &apiRecorder{
		infoStatus:  http.StatusOK,
		infoPayload: `{"state":"Shutdown"}`,
	}
	c := scriptedVMClient(t, rec)

	if err := c.ensureBooted(t.Context()); err != nil {
		t.Fatalf("ensureBooted() error = %v, want nil", err)
	}

	want := []string{"/api/v1/vm.info", "/api/v1/vm.boot"}
	if got := rec.calls(); !equalStrings(got, want) {
		t.Fatalf("endpoint sequence = %v, want %v", got, want)
	}
}

// TestEnsureBootedTransportErrorSurfaces pins that an Info failure which is
// not the absent-VM 404 — here the socket refusing connections — surfaces
// unchanged and issues no create or boot.
func TestEnsureBootedTransportErrorSurfaces(t *testing.T) {
	c := newTestVMClient(t, &apiRecorder{})
	// Point the client at a socket path no server listens on.
	c.client = ch.NewClient(filepath.Join(t.TempDir(), "missing.sock"))

	err := c.ensureBooted(t.Context())
	if err == nil {
		t.Fatal("ensureBooted() = nil against a dead socket, want error")
	}
	if _, ok := errors.AsType[*ch.StatusError](err); ok {
		t.Errorf("error %v is a StatusError, want a transport error", err)
	}
}

// TestEnsureBootedRequiresFirmware pins the misconfiguration guard: pushing
// a config without a firmware fails naming SetFirmware instead of producing
// a VM cloud-hypervisor would refuse to boot.
func TestEnsureBootedRequiresFirmware(t *testing.T) {
	rec := &apiRecorder{infoStatus: http.StatusNotFound}
	c := newTestVMClient(t, rec)
	c.SetDiskPaths([]string{"/build/vm-disks/node-1-root.qcow2"})

	err := c.ensureBooted(t.Context())
	if err == nil {
		t.Fatal("ensureBooted() without firmware = nil, want error")
	}
	if !strings.Contains(err.Error(), "SetFirmware") {
		t.Errorf("error %v does not name SetFirmware", err)
	}
	want := []string{"/api/v1/vm.info"}
	if got := rec.calls(); !equalStrings(got, want) {
		t.Fatalf("endpoint sequence = %v, want %v (no create/boot expected)", got, want)
	}
}

// equalStrings compares two string slices for element-wise equality.
func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

// assertCreateField walks a decoded JSON document along a dotted path of
// object keys and numeric array indices and fails unless the leaf equals
// want.
func assertCreateField(t *testing.T, doc map[string]any, path string, want any) {
	t.Helper()

	cur := any(doc)
	for seg := range strings.SplitSeq(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				t.Fatalf("JSON path %q: key %q missing in %v", path, seg, node)
			}
			cur = next
		case []any:
			idx := 0
			for _, c := range seg {
				if c < '0' || c > '9' {
					t.Fatalf("JSON path %q: segment %q is not an index", path, seg)
				}
				idx = idx*10 + int(c-'0')
			}
			if idx >= len(node) {
				t.Fatalf("JSON path %q: index %d out of range (%d entries)", path, idx, len(node))
			}
			cur = node[idx]
		default:
			t.Fatalf("JSON path %q: segment %q traverses non-container %T", path, seg, cur)
		}
	}
	if cur != want {
		t.Errorf("JSON path %q = %v, want %v", path, cur, want)
	}
}

// assertCreateFieldAbsent fails unless the dotted path is absent from the
// decoded JSON document. It pins omission contracts (for example no cpus
// section when the spec leaves CPU unset).
func assertCreateFieldAbsent(t *testing.T, doc map[string]any, path string) {
	t.Helper()

	cur := any(doc)
	for seg := range strings.SplitSeq(path, ".") {
		switch node := cur.(type) {
		case map[string]any:
			next, ok := node[seg]
			if !ok {
				return
			}
			cur = next
		default:
			t.Fatalf("JSON path %q: segment %q traverses non-container %T", path, seg, cur)
		}
	}
	t.Errorf("JSON path %q present as %v, want absent", path, cur)
}
