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

package fake_test

import (
	"encoding/json"
	"net"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
)

type jsonRPCRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id"`
	Version string `json:"version,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type jsonRPCResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Version string          `json:"version,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func dialAndCall(t *testing.T, socketPath string, req jsonRPCRequest) jsonRPCResponse {
	t.Helper()

	conn, err := net.Dial("unix", socketPath)
	if err != nil {
		t.Fatalf("dial %q: %v", socketPath, err)
	}

	defer func() { _ = conn.Close() }()

	if err := json.NewEncoder(conn).Encode(req); err != nil {
		t.Fatalf("encode request: %v", err)
	}

	var resp jsonRPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	return resp
}

func TestFakeServer_ListensOnUnixSocket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	resp := dialAndCall(
		t,
		sock,
		jsonRPCRequest{JSONRPC: "2.0", ID: 1, Version: "1", Method: "GetNetwork", Params: map[string]string{"name": "test"}},
	)
	if resp.JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want %q", resp.JSONRPC, "2.0")
	}

	if resp.Error != nil {
		t.Errorf("unexpected error: %+v", resp.Error)
	}

	if srv.RequestCount() != 1 {
		t.Errorf("RequestCount = %d, want 1", srv.RequestCount())
	}
}

func TestFakeServer_CapturesMethodAndParams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	_ = dialAndCall(
		t,
		sock,
		jsonRPCRequest{
			JSONRPC: "2.0",
			ID:      42,
			Version: "1",
			Method:  "CreateNetwork",
			Params:  map[string]string{"name": "demo", "cidr": "192.168.124.0/24"},
		},
	)

	reqs := srv.Requests()
	if len(reqs) != 1 {
		t.Fatalf("Requests len = %d, want 1", len(reqs))
	}

	if reqs[0].Method != "CreateNetwork" {
		t.Errorf("Method = %q, want CreateNetwork", reqs[0].Method)
	}

	if reqs[0].Version != "1" {
		t.Errorf("Version = %q, want 1", reqs[0].Version)
	}

	if reqs[0].JSONRPC != "2.0" {
		t.Errorf("JSONRPC = %q, want 2.0", reqs[0].JSONRPC)
	}

	var params map[string]string
	if err := json.Unmarshal(reqs[0].Params, &params); err != nil {
		t.Fatalf("unmarshal params: %v", err)
	}

	if params["name"] != "demo" {
		t.Errorf("param name = %q, want demo", params["name"])
	}
}

func TestFakeServer_TypedErrorCodes(t *testing.T) {
	t.Parallel()

	codes := []string{"not_found", "already_exists", "invalid_params", "conflict", "internal"}
	for _, code := range codes {
		t.Run(code, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			sock := filepath.Join(dir, "control.sock")

			srv, err := fake.New(sock)
			if err != nil {
				t.Fatalf("New() error = %v", err)
			}

			t.Cleanup(func() { _ = srv.Close() })
			srv.SetErrorCode("GetNetwork", code)

			resp := dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "GetNetwork"})
			if resp.Error == nil {
				t.Fatalf("expected error with code %q, got success", code)
			}

			if resp.Error.Code != code {
				t.Errorf("error code = %q, want %q", resp.Error.Code, code)
			}
		})
	}
}

func TestFakeServer_HandlerOverridesCannedResult(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	srv.SetResult("GetNetwork", map[string]string{"name": "canned"})

	resp := dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "GetNetwork"})

	var out map[string]string

	_ = json.Unmarshal(resp.Result, &out)
	if out["name"] != "canned" {
		t.Fatalf("canned result = %v, want canned", out)
	}

	srv.Handle("GetNetwork", func(params json.RawMessage) (any, *fake.RPCError) {
		return map[string]string{"name": "handled"}, nil
	})

	resp = dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 2, Method: "GetNetwork"})

	_ = json.Unmarshal(resp.Result, &out)
	if out["name"] != "handled" {
		t.Fatalf("handled result = %v, want handled", out)
	}

	srv.Reset()
	srv.SetResult("GetNetwork", map[string]string{"name": "after-reset"})

	resp = dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 3, Method: "GetNetwork"})

	_ = json.Unmarshal(resp.Result, &out)
	if out["name"] != "after-reset" {
		t.Errorf("after reset result = %v, want after-reset", out)
	}
}

func TestFakeServer_VersionMismatchReturnsInvalidParams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.NewWithVersion(sock, "1.0")
	if err != nil {
		t.Fatalf("NewWithVersion() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	resp := dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Version: "9.9", Method: "GetNetwork"})
	if resp.Error == nil {
		t.Fatalf("expected version mismatch error, got success")
	}

	if resp.Error.Code != "invalid_params" {
		t.Errorf("error code = %q, want invalid_params", resp.Error.Code)
	}
}

func TestFakeServer_InvalidEnvelopeReturnsInvalidParams(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	conn, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()

	_, _ = conn.Write([]byte(`{"id":1,"method":"GetNetwork","params":{}}` + "\n"))

	var resp jsonRPCResponse
	if err := json.NewDecoder(conn).Decode(&resp); err != nil {
		t.Fatalf("decode response: %v", err)
	}

	if resp.Error == nil || resp.Error.Code != "invalid_params" {
		t.Errorf("expected invalid_params for missing jsonrpc, got %+v", resp.Error)
	}
}

func TestFakeServer_ConcurrentConnections(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	const n = 20

	done := make(chan struct{}, n)
	for i := range n {
		go func(id int) {
			resp := dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: id, Method: "GetNetwork"})
			if resp.JSONRPC != "2.0" {
				t.Errorf("concurrent call %d: JSONRPC = %q", id, resp.JSONRPC)
			}

			done <- struct{}{}
		}(i)
	}

	for range n {
		<-done
	}

	if srv.RequestCount() != n {
		t.Errorf("RequestCount = %d, want %d", srv.RequestCount(), n)
	}
}

func TestFakeServer_ReusableForControllerSuites(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })

	srv.SetResult("CreateNetwork", map[string]string{"name": "test"})
	srv.SetResult("CreatePort", nil)
	srv.SetResult("AllocateIP", map[string]string{"ip": "192.168.124.20"})

	for _, method := range []string{"CreateNetwork", "CreatePort", "AllocateIP"} {
		_ = dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: method})
	}

	reqs := srv.Requests()
	if len(reqs) != 3 {
		t.Fatalf("Requests len = %d, want 3", len(reqs))
	}

	wantOrder := []string{"CreateNetwork", "CreatePort", "AllocateIP"}
	for i, want := range wantOrder {
		if reqs[i].Method != want {
			t.Errorf("request %d method = %q, want %q", i, reqs[i].Method, want)
		}
	}
}

func TestFakeServer_CloseRemovesSocket(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	if err := srv.Close(); err != nil {
		t.Fatalf("Close() error = %v", err)
	}

	if _, err := net.Dial("unix", sock); err == nil {
		t.Fatalf("dial after Close succeeded, want error")
	}
}

func TestFakeServer_SetErrorWithMessage(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	sock := filepath.Join(dir, "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("New() error = %v", err)
	}

	t.Cleanup(func() { _ = srv.Close() })
	srv.SetError("DeleteNetwork", "conflict", "network in use")

	resp := dialAndCall(t, sock, jsonRPCRequest{JSONRPC: "2.0", ID: 1, Method: "DeleteNetwork"})
	if resp.Error == nil {
		t.Fatalf("expected error, got success")
	}

	if resp.Error.Code != "conflict" {
		t.Errorf("code = %q, want conflict", resp.Error.Code)
	}

	if resp.Error.Message != "network in use" {
		t.Errorf("message = %q, want %q", resp.Error.Message, "network in use")
	}
}
