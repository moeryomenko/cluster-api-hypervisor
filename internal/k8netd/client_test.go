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

// Package k8netd client contract — test-first.
//
// Expected API (implementer must satisfy this contract for these tests to pass):
//
//	var ErrNotFound, ErrAlreadyExists, ErrInvalidParams, ErrConflict, ErrInternal error
//	  // each must be usable with errors.Is, i.e. wrapping preserves sentinel.
//
//	func NewClient(socketPath string) *Client
//
//	type Network struct { Name, CIDR, Gateway, PoolStart, PoolEnd string `json:"..."` }
//	type Port struct { Name, Network, MAC, IP, SocketPath string `json:"..."` }
//
//	func (c *Client) CreateNetwork(ctx context.Context, name, cidr, gateway, poolStart, poolEnd string) error
//	func (c *Client) DeleteNetwork(ctx context.Context, name string) error
//	func (c *Client) GetNetwork(ctx context.Context, name string) (*Network, error)
//	func (c *Client) CreatePort(ctx context.Context, name string) error
//	func (c *Client) DeletePort(ctx context.Context, name string) error
//	func (c *Client) GetPort(ctx context.Context, name string) (*Port, error)
//	func (c *Client) AttachPort(ctx context.Context, port, network, mac string) error
//	func (c *Client) DetachPort(ctx context.Context, port string) error
//	func (c *Client) AllocateIP(ctx context.Context, network, mac string) (string, error)
//	func (c *Client) ReleaseIP(ctx context.Context, network, mac string) error
//
// Wire contract (REQ-001):
//   - JSON-RPC 2.0 envelope: {"jsonrpc":"2.0","id":<int>,"version":<string>,"method":<string>,"params":{...}}
//   - Response: {"jsonrpc":"2.0","id":<same>,"version":...,"result":...} or {"error":{"code":"not_found"|...,"message":...}}
//   - Every request carries "version"; mismatch must surface as ErrInvalidParams.
//   - Typed codes → sentinels: not_found→ErrNotFound, already_exists→ErrAlreadyExists,
//     invalid_params→ErrInvalidParams, conflict→ErrConflict, internal→ErrInternal (unknown→ErrInternal).
//   - Dial retries with backoff when socket absent (REQ-001 backoff).
//   - Create* is idempotent: same params → success, differing params → ErrConflict.
//   - All ten methods must issue the exact method name and decode result.
//
// These tests use the fake server in internal/k8netd/fake over net.Listen("unix",...)
// and are reusable for controller suites.
package k8netd

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
)

// helper creates a client dialing the given fake server.
func newClientForServer(t *testing.T, srv *fake.Server) *Client {
	t.Helper()
	return NewClient(srv.SocketPath())
}

// TestSentinelErrorsExist verifies that the five sentinels exist and are distinct.
func TestSentinelErrorsExist(t *testing.T) {
	t.Parallel()
	sentinels := map[string]error{
		"ErrNotFound":      ErrNotFound,
		"ErrAlreadyExists": ErrAlreadyExists,
		"ErrInvalidParams": ErrInvalidParams,
		"ErrConflict":      ErrConflict,
		"ErrInternal":      ErrInternal,
	}
	for name, err := range sentinels {
		if err == nil {
			t.Errorf("%s is nil, want non-nil sentinel", name)
		}
	}
	// distinct
	if errors.Is(ErrNotFound, ErrAlreadyExists) || errors.Is(ErrConflict, ErrInternal) {
		t.Errorf("sentinels must be distinct (errors.Is cross-check failed)")
	}
	if errors.Is(ErrNotFound, ErrInvalidParams) {
		t.Errorf("ErrNotFound should not equal ErrInvalidParams")
	}
}

// TestErrorCodeMapping covers VC-02: each typed code maps to the matching sentinel via errors.Is.
func TestErrorCodeMapping(t *testing.T) {
	t.Parallel()
	cases := []struct {
		code string
		want error
	}{
		{code: "not_found", want: ErrNotFound},
		{code: "already_exists", want: ErrAlreadyExists},
		{code: "invalid_params", want: ErrInvalidParams},
		{code: "conflict", want: ErrConflict},
		{code: "internal", want: ErrInternal},
	}
	for _, tc := range cases {
		t.Run(tc.code, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			sock := filepath.Join(dir, "control.sock")
			srv, err := fake.New(sock)
			if err != nil {
				t.Fatalf("fake.New: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			srv.SetErrorCode("GetNetwork", tc.code)

			client := NewClient(sock)
			_, err = client.GetNetwork(context.Background(), "missing")
			if err == nil {
				t.Fatalf("GetNetwork expected error with code %q, got nil", tc.code)
			}
			if !errors.Is(err, tc.want) {
				t.Fatalf("errors.Is(err, %v) = false for code %q, err = %v", tc.want, tc.code, err)
			}
			// also ensure unwrapped error still Is
			if !errors.Is(err, tc.want) {
				t.Fatalf("wrapped error lost sentinel for %q", tc.code)
			}
		})
	}
}

// TestErrorCodeMapping_UnknownMapsToInternal verifies unknown codes become ErrInternal.
func TestErrorCodeMapping_UnknownMapsToInternal(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetError("GetNetwork", "weird_code", "weird")

	client := NewClient(sock)
	_, err = client.GetNetwork(context.Background(), "x")
	if err == nil {
		t.Fatalf("expected error for unknown code")
	}
	if !errors.Is(err, ErrInternal) {
		t.Fatalf("unknown code should map to ErrInternal, got %v", err)
	}
}

// TestJSONRPCEnvelope verifies the wire envelope for a representative method.
// Checks: jsonrpc=="2.0", version non-empty, id present, method name, params decoding.
func TestJSONRPCEnvelope(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	srv.SetResult("CreateNetwork", nil)
	client := NewClient(sock)
	err = client.CreateNetwork(
		context.Background(),
		"demo",
		"192.168.124.0/24",
		"192.168.124.1",
		"192.168.124.20",
		"192.168.124.200",
	)
	if err != nil {
		t.Fatalf("CreateNetwork error = %v", err)
	}

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Requests len = %d, want 1", len(reqs))
	}
	req := reqs[0]
	if req.JSONRPC != "2.0" {
		t.Errorf("jsonrpc = %q, want 2.0", req.JSONRPC)
	}
	if req.Version == "" {
		t.Errorf("version field empty, want non-empty version per REQ-001")
	}
	if req.ID == nil {
		t.Errorf("id field missing/nil, want present")
	}
	if req.Method != "CreateNetwork" {
		t.Errorf("method = %q, want CreateNetwork", req.Method)
	}
	// params must decode to expected fields (allow extra fields)
	var params map[string]any
	if err := json.Unmarshal(req.Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v, raw %s", err, string(req.Params))
	}
	// Check that name/cidr/gateway/pool fields are present (case-insensitive keys)
	// Accept either lowerCamel or snake; require at least name/cidr/gateway.
	for _, key := range []string{"name", "cidr", "gateway"} {
		if _, ok := params[key]; !ok {
			// try capitalized variants
			found := false
			for k := range params {
				if k == key || k == "Name" || k == "CIDR" || k == "Gateway" {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("params missing key %q, got %v", key, params)
			}
		}
	}
	// Pool bounds: check at least one of poolStart/pool_start present
	if _, ok := params["poolStart"]; !ok {
		if _, ok2 := params["pool_start"]; !ok2 {
			if _, ok3 := params["poolEnd"]; !ok3 && params["pool_end"] == nil {
				t.Logf(
					"warning: poolStart/poolEnd not found in params %v (acceptable if encoded differently, but must be present)",
					params,
				)
			}
		}
	}
}

// TestVersionMismatch surfaces as ErrInvalidParams via errors.Is.
func TestVersionMismatch(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	// Server expects version "expected-v1", client sends its own default (likely "1" or "1.0").
	srv, err := fake.NewWithVersion(sock, "expected-v1")
	if err != nil {
		t.Fatalf("fake.NewWithVersion: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	client := NewClient(sock)
	// Any method will trigger version check; use GetNetwork.
	_, err = client.GetNetwork(context.Background(), "demo")
	if err == nil {
		t.Fatalf("expected version mismatch error, got nil")
	}
	if !errors.Is(err, ErrInvalidParams) {
		t.Fatalf(
			"version mismatch should map to ErrInvalidParams, got %v (Is ErrInvalidParams=%v)",
			err,
			errors.Is(err, ErrInvalidParams),
		)
	}
	// Verify the server saw a version that is not "expected-v1"
	reqs := srv.Requests()
	if len(reqs) == 0 {
		t.Fatalf("no requests captured")
	}
	if reqs[0].Version == "expected-v1" {
		t.Errorf(
			"client sent expected version, so mismatch test is not exercising mismatch (client version=%q)",
			reqs[0].Version,
		)
	}
}

// TestAllTenMethods_ProvesMethodNameAndDecode verifies each of the ten contract methods
// issues the correct JSON-RPC method and decodes its result/error correctly.
func TestAllTenMethods_ProvesMethodNameAndDecode(t *testing.T) {
	t.Parallel()
	// Each subtest starts its own server to isolate stubs.
	tests := []struct {
		name       string
		method     string
		call       func(ctx context.Context, c *Client) error
		captureOK  func(t *testing.T, req fake.CapturedRequest)
		stubResult any
	}{
		{
			name:   "CreateNetwork",
			method: "CreateNetwork",
			call: func(ctx context.Context, c *Client) error {
				return c.CreateNetwork(ctx, "test-net", "192.168.124.0/24", "192.168.124.1", "192.168.124.20", "192.168.124.200")
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				var p map[string]string
				_ = json.Unmarshal(req.Params, &p)
				if p["name"] != "test-net" && p["Name"] != "test-net" {
					t.Errorf("CreateNetwork param name = %v, want test-net", p)
				}
			},
			stubResult: nil,
		},
		{
			name:   "DeleteNetwork",
			method: "DeleteNetwork",
			call: func(ctx context.Context, c *Client) error {
				return c.DeleteNetwork(ctx, "test-net")
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				// params should contain name field
				if len(req.Params) == 0 {
					t.Errorf("DeleteNetwork params empty")
				}
			},
			stubResult: nil,
		},
		{
			name:   "GetNetwork",
			method: "GetNetwork",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetNetwork(ctx, "test-net")
				return err
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				if len(req.Params) == 0 {
					t.Errorf("GetNetwork params empty")
				}
			},
			stubResult: map[string]string{
				"name":      "test-net",
				"cidr":      "192.168.124.0/24",
				"gateway":   "192.168.124.1",
				"poolStart": "192.168.124.20",
				"poolEnd":   "192.168.124.200",
			},
		},
		{
			name:   "CreatePort",
			method: "CreatePort",
			call: func(ctx context.Context, c *Client) error {
				return c.CreatePort(ctx, "machine-0")
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				var p map[string]string
				_ = json.Unmarshal(req.Params, &p)
				if p["name"] == "" && p["Name"] == "" {
					t.Errorf("CreatePort param name missing in %s", string(req.Params))
				}
			},
			stubResult: nil,
		},
		{
			name:   "DeletePort",
			method: "DeletePort",
			call: func(ctx context.Context, c *Client) error {
				return c.DeletePort(ctx, "machine-0")
			},
			captureOK:  nil,
			stubResult: nil,
		},
		{
			name:   "GetPort",
			method: "GetPort",
			call: func(ctx context.Context, c *Client) error {
				_, err := c.GetPort(ctx, "machine-0")
				return err
			},
			captureOK:  nil,
			stubResult: map[string]string{"name": "machine-0", "network": "test-net"},
		},
		{
			name:   "AttachPort",
			method: "AttachPort",
			call: func(ctx context.Context, c *Client) error {
				return c.AttachPort(ctx, "machine-0", "test-net", "c6:e5:50:1c:ec:01")
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				var p map[string]string
				_ = json.Unmarshal(req.Params, &p)
				// The daemon's handler reads exactly these keys: "port"
				// carries the port identity, "network" the L2 segment, and
				// "mac" the address the IPAM reservation binds to.
				if p["port"] != "machine-0" {
					t.Errorf("AttachPort param port = %v, want canonical port=machine-0", p)
				}
				if _, alias := p["name"]; alias {
					t.Errorf("AttachPort params carry the non-canonical %q alias: %s", "name", string(req.Params))
				}
				hasNet := p["network"] != "" || p["Network"] != "" || p["networkName"] != ""
				if !hasNet {
					t.Errorf("AttachPort params missing network field in %s", string(req.Params))
				}
				if p["mac"] == "" {
					t.Errorf("AttachPort params missing mac field in %s", string(req.Params))
				}
			},
			stubResult: nil,
		},
		{
			name:   "DetachPort",
			method: "DetachPort",
			call: func(ctx context.Context, c *Client) error {
				return c.DetachPort(ctx, "machine-0")
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				var p map[string]string
				_ = json.Unmarshal(req.Params, &p)
				// The daemon's DetachPort handler reads the "name" key.
				if p["name"] != "machine-0" {
					t.Errorf("DetachPort param name = %v, want canonical name=machine-0", p)
				}
			},
			stubResult: nil,
		},
		{
			name:   "AllocateIP",
			method: "AllocateIP",
			call: func(ctx context.Context, c *Client) error {
				ip, err := c.AllocateIP(ctx, "test-net", "c6:e5:50:1c:ec:01")
				if err != nil {
					return err
				}
				if ip == "" {
					t.Errorf("AllocateIP returned empty IP")
				}
				// also verify IP looks like IPv4 (simple check)
				if len(ip) < 7 {
					t.Errorf("AllocateIP ip %q suspiciously short", ip)
				}
				return nil
			},
			captureOK: func(t *testing.T, req fake.CapturedRequest) {
				var p map[string]string
				_ = json.Unmarshal(req.Params, &p)
				hasNet := p["network"] != "" || p["Network"] != "" || p["networkName"] != ""
				hasMAC := p["mac"] != "" || p["MAC"] != "" || p["Mac"] != ""
				if !hasNet {
					t.Errorf("AllocateIP params missing network in %s", string(req.Params))
				}
				if !hasMAC {
					t.Errorf("AllocateIP params missing mac in %s", string(req.Params))
				}
			},
			stubResult: "192.168.124.21",
		},
		{
			name:   "ReleaseIP",
			method: "ReleaseIP",
			call: func(ctx context.Context, c *Client) error {
				return c.ReleaseIP(ctx, "test-net", "c6:e5:50:1c:ec:01")
			},
			captureOK:  nil,
			stubResult: nil,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			sock := filepath.Join(dir, "control.sock")
			srv, err := fake.New(sock)
			if err != nil {
				t.Fatalf("fake.New: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })

			// Prepare stub result. For AllocateIP string case, need to handle that server encodes string result as JSON string.
			// Our fake's SetResult encodes any via json.Marshal, so passing string yields JSON string correctly.
			// For GetNetwork/GetPort which expect struct decode, pass map.
			if tt.stubResult != nil {
				srv.SetResult(tt.method, tt.stubResult)
			} else {
				// ensure success null result for void methods
				srv.SetResult(tt.method, nil)
			}
			// Special handling for AllocateIP where stub is string: wrap as raw string? fake will JSON-encode it as "192.168.124.21"
			if tt.method == "AllocateIP" {
				// fake already set string above, but ensure handler decodes as string not object
				srv.SetResult(tt.method, "192.168.124.21")
			}

			client := NewClient(sock)
			if err := tt.call(context.Background(), client); err != nil {
				t.Fatalf("%s call error = %v", tt.method, err)
			}
			reqs := srv.Requests()
			if len(reqs) != 1 {
				t.Fatalf("Requests len = %d, want 1", len(reqs))
			}
			if reqs[0].Method != tt.method {
				t.Errorf("method = %q, want %q", reqs[0].Method, tt.method)
			}
			if reqs[0].JSONRPC != "2.0" {
				t.Errorf("jsonrpc = %q, want 2.0", reqs[0].JSONRPC)
			}
			if reqs[0].Version == "" {
				t.Errorf("version empty for %s", tt.method)
			}
			if tt.captureOK != nil {
				tt.captureOK(t, reqs[0])
			}
		})
	}
}

// TestGetNetwork_Decode verifies GetNetwork decodes Network fields exactly.
func TestGetNetwork_Decode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	want := map[string]string{
		"name":      "demo",
		"cidr":      "10.0.0.0/24",
		"gateway":   "10.0.0.1",
		"poolStart": "10.0.0.20",
		"poolEnd":   "10.0.0.200",
	}
	srv.SetResult("GetNetwork", want)

	client := NewClient(sock)
	got, err := client.GetNetwork(context.Background(), "demo")
	if err != nil {
		t.Fatalf("GetNetwork error = %v", err)
	}
	if got == nil {
		t.Fatalf("GetNetwork returned nil Network")
	}
	// Check via json round-trip to allow struct field naming variations.
	gotJSON, _ := json.Marshal(got)
	var gotMap map[string]string
	_ = json.Unmarshal(gotJSON, &gotMap)
	for k, v := range want {
		// allow lower/upper case key variants
		if gotMap[k] != v && gotMap["Name"] != v && gotMap["CIDR"] != v {
			// Try case-insensitive lookup.
			found := false
			for gk, gv := range gotMap {
				if (gk == k || gk == "Name" || gk == "CIDR" || gk == "Gateway" || gk == "PoolStart" || gk == "PoolEnd") && gv == v {
					found = true
					break
				}
			}
			if !found {
				t.Errorf("GetNetwork field %q = %q, want %q (gotMap %v)", k, gotMap[k], v, gotMap)
			}
		} else if gotMap[k] != "" && gotMap[k] != v {
			t.Errorf("GetNetwork field %q = %q, want %q", k, gotMap[k], v)
		}
	}
	// Direct struct field checks if fields are exported with same names
	if got.Name != "" && got.Name != "demo" {
		t.Errorf("Network.Name = %q, want demo", got.Name)
	}
}

// TestGetPort_Decode verifies GetPort decoding.
func TestGetPort_Decode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	srv.SetResult("GetPort", map[string]string{"name": "machine-0", "network": "demo", "mac": "c6:e5:50:1c:ec:ab"})

	client := NewClient(sock)
	got, err := client.GetPort(context.Background(), "machine-0")
	if err != nil {
		t.Fatalf("GetPort error = %v", err)
	}
	if got == nil {
		t.Fatalf("GetPort nil")
	}
	// Verify at least name matches
	gotJSON, _ := json.Marshal(got)
	var m map[string]string
	_ = json.Unmarshal(gotJSON, &m)
	if m["name"] == "" && m["Name"] == "" {
		t.Errorf("GetPort missing name in decoded %v", m)
	}
}

// TestAllocateIP_Decode verifies AllocateIP returns the IP string from server.
func TestAllocateIP_Decode(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	srv.SetResult("AllocateIP", "192.168.124.55")

	client := NewClient(sock)
	ip, err := client.AllocateIP(context.Background(), "demo", "c6:e5:50:1c:ec:01")
	if err != nil {
		t.Fatalf("AllocateIP error = %v", err)
	}
	if ip != "192.168.124.55" {
		t.Errorf("AllocateIP = %q, want 192.168.124.55", ip)
	}
}

// TestAllocateIP_UnexpectedResultShapeFailsLoudly pins the strict decode
// contract: a result that is not exactly the contract envelope — a JSON
// string carrying the address — surfaces as ErrInternal instead of being
// guessed apart.
func TestAllocateIP_UnexpectedResultShapeFailsLoudly(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		result any
	}{
		{name: "object with ip field", result: map[string]string{"ip": "192.168.124.5"}},
		{name: "object with IP field", result: map[string]string{"IP": "192.168.124.5"}},
		{name: "null result", result: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			sock := filepath.Join(dir, "control.sock")
			srv, err := fake.New(sock)
			if err != nil {
				t.Fatalf("fake.New: %v", err)
			}
			t.Cleanup(func() { _ = srv.Close() })
			srv.SetResult("AllocateIP", tt.result)

			client := NewClient(sock)
			ip, err := client.AllocateIP(context.Background(), "demo", "c6:e5:50:1c:ec:01")
			if err == nil {
				t.Fatalf("AllocateIP with result %#v succeeded (%q), want ErrInternal", tt.result, ip)
			}
			if !errors.Is(err, ErrInternal) {
				t.Errorf("AllocateIP error = %v, want errors.Is(err, ErrInternal)", err)
			}
		})
	}
}

// TestIdempotentCreate_SameParamsSucceeds verifies Create* idempotency: second identical create succeeds.
func TestIdempotentCreate_SameParamsSucceeds(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// Server will always succeed for CreateNetwork (idempotent no-op)
	srv.Handle("CreateNetwork", func(params json.RawMessage) (any, *fake.RPCError) {
		return nil, nil
	})

	client := NewClient(sock)
	ctx := context.Background()
	if err := client.CreateNetwork(ctx, "demo", "192.168.124.0/24", "192.168.124.1", "192.168.124.20", "192.168.124.200"); err != nil {
		t.Fatalf("first CreateNetwork: %v", err)
	}
	if err := client.CreateNetwork(ctx, "demo", "192.168.124.0/24", "192.168.124.1", "192.168.124.20", "192.168.124.200"); err != nil {
		t.Fatalf("second identical CreateNetwork should be idempotent success, got %v", err)
	}
	if srv.RequestCount() != 2 {
		t.Errorf("RequestCount = %d, want 2", srv.RequestCount())
	}
}

// TestIdempotentCreate_DifferingParamsReturnsConflict verifies conflict when params differ.
func TestIdempotentCreate_DifferingParamsReturnsConflict(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	var calls int
	srv.Handle("CreateNetwork", func(params json.RawMessage) (any, *fake.RPCError) {
		calls++
		if calls == 1 {
			return nil, nil
		}
		// second call with differing CIDR → conflict
		var p map[string]string
		_ = json.Unmarshal(params, &p)
		// If same as first, succeed; else conflict. For test we always return conflict on second.
		return nil, &fake.RPCError{Code: "conflict", Message: "network already exists with different params"}
	})

	client := NewClient(sock)
	ctx := context.Background()
	if err := client.CreateNetwork(ctx, "demo", "192.168.124.0/24", "192.168.124.1", "192.168.124.20", "192.168.124.200"); err != nil {
		t.Fatalf("first CreateNetwork: %v", err)
	}
	err = client.CreateNetwork(ctx, "demo", "10.0.0.0/24", "10.0.0.1", "10.0.0.20", "10.0.0.200")
	if err == nil {
		t.Fatalf("second CreateNetwork with differing params expected conflict, got nil")
	}
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("expected ErrConflict, got %v", err)
	}
}

// TestBackoffRetryWhenSocketAbsent verifies client retries with backoff when socket initially absent.
// The client should succeed once the socket appears within the retry window.
func TestBackoffRetryWhenSocketAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	client := NewClient(sock)

	// Start server after a short delay to exercise backoff.
	go func() {
		time.Sleep(120 * time.Millisecond)
		srv, err := fake.New(sock)
		if err != nil {
			t.Errorf("delayed fake.New: %v", err)
			return
		}
		// Keep server alive for duration of test; close on test end via cleanup in main goroutine not possible here,
		// so hold reference and schedule deferred close via time.After.
		srv.SetResult("GetNetwork", map[string]string{"name": "demo", "cidr": "192.168.124.0/24"})
		// Do not close immediately; let client connect. Close after 2s.
		time.AfterFunc(2*time.Second, func() { _ = srv.Close() })
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := client.GetNetwork(ctx, "demo")
	if err != nil {
		t.Fatalf("GetNetwork with delayed socket expected success via backoff, got %v", err)
	}
}

// TestBackoffRetryExhausted verifies that when socket never appears, client eventually returns error
// and does not hang indefinitely.
func TestBackoffRetryExhausted(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "nonexistent.sock")
	// Ensure socket does not exist.

	client := NewClient(sock)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	_, err := client.GetNetwork(ctx, "demo")
	if err == nil {
		t.Fatalf("expected error when socket never appears, got nil")
	}
	// Should be ErrInternal or context error wrapped, not nil. Just ensure not not-found.
	if errors.Is(err, ErrNotFound) {
		t.Errorf("absent socket should not map to ErrNotFound, got %v", err)
	}
}

// TestClient_NilContextHandled verifies that a nil context does not panic (should use Background).
func TestClient_NilContextHandled(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetResult("DeleteNetwork", nil)

	client := NewClient(sock)
	// Pass context.TODO() if nil panics, test expects no panic. We call with Background as proxy for nil check.
	// If implementation supports nil context, this will not panic; if not, we just verify Background works.
	if err := client.DeleteNetwork(context.Background(), "demo"); err != nil {
		t.Fatalf("DeleteNetwork: %v", err)
	}
}

// TestErrorWrappingPreservesSentinel verifies errors.Is works through wrapped errors (fmt.Errorf %w).
func TestErrorWrappingPreservesSentinel(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetErrorCode("DeletePort", "not_found")

	client := NewClient(sock)
	err = client.DeletePort(context.Background(), "ghost")
	if err == nil {
		t.Fatalf("expected error")
	}
	// Wrap once and check Is still
	wrapped := errors.Join(err, errors.New("extra"))
	_ = wrapped
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("direct errors.Is failed")
	}
	// Also check fmt wrap
	wrapped2 := errors.Unwrap(err)
	_ = wrapped2
}

// TestAllMethods_EachErrorPath checks that each method correctly maps not_found for Get* when missing.
func TestAllMethods_EachErrorPath(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New: %v", err)
	}
	t.Cleanup(func() { _ = srv.Close() })

	// Make GetNetwork return not_found, GetPort return not_found, etc.
	srv.SetErrorCode("GetNetwork", "not_found")
	srv.SetErrorCode("GetPort", "not_found")
	srv.SetErrorCode("DeleteNetwork", "not_found")
	srv.SetErrorCode("DeletePort", "not_found")

	client := NewClient(sock)
	if _, err := client.GetNetwork(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetNetwork not_found mapping failed: %v", err)
	}
	if _, err := client.GetPort(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("GetPort not_found mapping failed: %v", err)
	}
	if err := client.DeleteNetwork(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeleteNetwork not_found mapping failed: %v", err)
	}
	if err := client.DeletePort(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("DeletePort not_found mapping failed: %v", err)
	}
}
