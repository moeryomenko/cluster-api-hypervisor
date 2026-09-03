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

// PublishPort client and fake-server contract (test-first, RED).
//
// REQ-010 / TASK-013: the k8netd client gains
//
//	func (c *Client) PublishPort(ctx context.Context, port string, vmPort int32) (int32, error)
//
// issuing method "PublishPort" with params exactly {"port": ..., "vm_port": ...}
// and decoding the result envelope {"host_port": N}. Any other result shape is
// a loud ErrInternal, never a guess (same strictness as AllocateIP). Typed
// error codes map to the existing sentinels via mapRPCError.
//
// The fake server implements PublishPort out of the box with a deterministic
// allocator-like mapping: repeated identical calls return the same host_port,
// and distinct (port, vm_port) pairs get distinct allocations. Controller
// suites rely on this default behavior without registering handlers.
//
// This file is RED: Client.PublishPort does not exist yet, so the package
// does not compile ("c.PublishPort undefined"). After the client lands but
// before the fake implements PublishPort, the fake-behavior tests fail on
// the null-result decode.
package k8netd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
)

// newPublishTestServer starts a fake k8netd server on a temp socket.
func newPublishTestServer(t *testing.T) *fake.Server {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New %q: %v", sock, err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	return srv
}

// publishParams is the decoded wire params of a PublishPort request.
type publishParams struct {
	Port   string `json:"port"`
	VMPort int32  `json:"vm_port"`
}

// TestPublishPort_RequestWireShape pins the request contract: the method is
// named exactly "PublishPort", the envelope carries the contract version, and
// the params object carries exactly the two canonical keys "port" and
// "vm_port" with the caller's values — no aliases, no extra fields.
func TestPublishPort_RequestWireShape(t *testing.T) {
	t.Parallel()
	srv := newPublishTestServer(t)
	srv.SetResult("PublishPort", map[string]int32{"host_port": 20100})

	client := NewClient(srv.SocketPath())
	if _, err := client.PublishPort(context.Background(), "lab-cluster-cp-0", 6443); err != nil {
		t.Fatalf("PublishPort error = %v", err)
	}

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Requests len = %d, want 1", len(reqs))
	}

	req := reqs[0]
	if req.Method != "PublishPort" {
		t.Errorf("method = %q, want %q", req.Method, "PublishPort")
	}

	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", req.JSONRPC)
	}

	if req.Version != k8netdVersion {
		t.Errorf("version = %q, want contract version %q", req.Version, k8netdVersion)
	}

	var keys map[string]json.RawMessage
	if err := json.Unmarshal(req.Params, &keys); err != nil {
		t.Fatalf("unmarshal params %s: %v", string(req.Params), err)
	}

	if len(keys) != 2 {
		t.Errorf("params carry %d keys (%v), want exactly 2: port, vm_port", len(keys), string(req.Params))
	}

	var p publishParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatalf("unmarshal typed params %s: %v", string(req.Params), err)
	}

	if p.Port != "lab-cluster-cp-0" {
		t.Errorf("param port = %q, want %q", p.Port, "lab-cluster-cp-0")
	}

	if p.VMPort != 6443 {
		t.Errorf("param vm_port = %d, want 6443", p.VMPort)
	}
}

// TestPublishPort_ResultDecode pins the result contract: the daemon answers
// {"host_port": N} and the client returns exactly N.
func TestPublishPort_ResultDecode(t *testing.T) {
	t.Parallel()
	srv := newPublishTestServer(t)
	srv.SetResult("PublishPort", map[string]int64{"host_port": 20123})

	client := NewClient(srv.SocketPath())

	hostPort, err := client.PublishPort(context.Background(), "lab-cluster-cp-0", 6443)
	if err != nil {
		t.Fatalf("PublishPort error = %v", err)
	}

	if hostPort != 20123 {
		t.Errorf("PublishPort host_port = %d, want 20123", hostPort)
	}
}

// TestPublishPort_UnexpectedResultShapeFailsLoudly pins the strict decode
// contract: any result that is not exactly the {"host_port": N} envelope with
// a non-zero port surfaces as ErrInternal instead of being guessed apart.
func TestPublishPort_UnexpectedResultShapeFailsLoudly(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		result any
	}{
		{name: "bare JSON number", result: 20123},
		{name: "JSON string", result: "20123"},
		{name: "object without host_port", result: map[string]int{"port": 20123}},
		{name: "null result", result: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newPublishTestServer(t)
			srv.SetResult("PublishPort", tt.result)

			client := NewClient(srv.SocketPath())

			hostPort, err := client.PublishPort(context.Background(), "lab-cluster-cp-0", 6443)
			if err == nil {
				t.Fatalf(
					"PublishPort with result %#v succeeded (host_port %d), want ErrInternal",
					tt.result,
					hostPort,
				)
			}

			if !errors.Is(err, ErrInternal) {
				t.Errorf("PublishPort error = %v, want errors.Is(err, ErrInternal)", err)
			}
		})
	}
}

// TestPublishPort_TypedErrorMapping pins the error contract: k8netd typed
// codes surface as the matching sentinels. Publishing for an unknown port is
// not_found; range exhaustion is a typed RPC error carried by the conflict
// code class.
func TestPublishPort_TypedErrorMapping(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		code string
		want error
	}{
		{name: "unknown port is not_found", code: "not_found", want: ErrNotFound},
		{name: "range exhaustion is conflict-class", code: "conflict", want: ErrConflict},
		{name: "daemon failure is internal", code: "internal", want: ErrInternal},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			srv := newPublishTestServer(t)
			srv.SetError("PublishPort", tt.code, tt.code)

			client := NewClient(srv.SocketPath())

			_, err := client.PublishPort(context.Background(), "ghost-port", 6443)
			if err == nil {
				t.Fatalf("PublishPort expected error with code %q, got nil", tt.code)
			}

			if !errors.Is(err, tt.want) {
				t.Errorf("code %q error = %v, want errors.Is(err, %v)", tt.code, err, tt.want)
			}
		})
	}
}

// TestFakePublishPort_StableAcrossRepeatedCalls proves the fake server
// implements PublishPort with deterministic allocator semantics: two
// identical calls return the same positive host_port without any handler
// registration, so controller suites get idempotent behavior for free.
func TestFakePublishPort_StableAcrossRepeatedCalls(t *testing.T) {
	t.Parallel()
	srv := newPublishTestServer(t)

	client := NewClient(srv.SocketPath())

	first, err := client.PublishPort(context.Background(), "lab-cluster-cp-0", 6443)
	if err != nil {
		t.Fatalf("first PublishPort error = %v", err)
	}

	second, err := client.PublishPort(context.Background(), "lab-cluster-cp-0", 6443)
	if err != nil {
		t.Fatalf("second identical PublishPort error = %v", err)
	}

	if first <= 0 {
		t.Fatalf("first host_port = %d, want a positive allocated port", first)
	}

	if second != first {
		t.Errorf("re-published host_port = %d, want stable %d", second, first)
	}
}

// TestFakePublishPort_DistinctAllocationsPerKey proves the fake's mapping is
// keyed by the full (port, vm_port) pair: a different vm_port on the same
// port gets a distinct allocation, and a different port name gets a distinct
// allocation too — the property multi-cluster endpoint separation needs.
func TestFakePublishPort_DistinctAllocationsPerKey(t *testing.T) {
	t.Parallel()
	srv := newPublishTestServer(t)

	client := NewClient(srv.SocketPath())
	ctx := context.Background()

	apiOnCP0, err := client.PublishPort(ctx, "lab-cluster-cp-0", 6443)
	if err != nil {
		t.Fatalf("PublishPort(cp-0, 6443): %v", err)
	}

	sshOnCP0, err := client.PublishPort(ctx, "lab-cluster-cp-0", 22)
	if err != nil {
		t.Fatalf("PublishPort(cp-0, 22): %v", err)
	}

	apiOnCP1, err := client.PublishPort(ctx, "lab-cluster-cp-1", 6443)
	if err != nil {
		t.Fatalf("PublishPort(cp-1, 6443): %v", err)
	}

	if sshOnCP0 == apiOnCP0 {
		t.Errorf("vm_port 22 and 6443 on the same port share host_port %d, want distinct allocations", apiOnCP0)
	}

	if apiOnCP1 == apiOnCP0 {
		t.Errorf("two different ports share host_port %d for vm_port 6443, want distinct allocations", apiOnCP0)
	}
}
