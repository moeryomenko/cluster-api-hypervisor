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

// Cloud-hypervisor HTTP API client contract (test-first).
//
// This suite pins the behavior of the Client in this package, which drives
// the cloud-hypervisor REST API over the unix api socket that the Manager
// creates for each subprocess-per-Machine VM. It is exercised against a real
// HTTP server listening on a unix socket created in a temporary directory, so
// no hypervisor is needed to run the suite. The server records every request
// it receives and answers with configurable status codes and bodies.
//
// The contract, in prose:
//
//   - NewClient(socketPath string) *Client constructs a client that talks to
//     the cloud-hypervisor API socket at socketPath. Construction does not
//     require the socket to exist yet: the VM may still be starting, so
//     connection problems surface on the first API call, not at
//     construction.
//   - Boot(ctx) error sends the VM boot request, PUT /api/v1/vm.boot, with
//     no request body. It returns nil for a 2xx response and a typed
//     StatusError carrying the status code for any non-2xx response.
//   - Shutdown(ctx) error sends the VM shutdown request, PUT
//     /api/v1/vm.shutdown, with no request body, with the same success and
//     error semantics as Boot.
//   - Info(ctx) (VMState, error) sends GET /api/v1/vm.info and parses the VM
//     state from the JSON response ("Running", "Shutdown", ...). It returns
//     a typed StatusError on a non-2xx response and a plain error when the
//     response body is not a valid VM state JSON document.
//   - Errors: a non-2xx HTTP response maps to *StatusError whose StatusCode
//     field holds the HTTP status. A transport failure (the socket does not
//     exist, the VM is gone) surfaces as a net.Error, never as a
//     StatusError. A cancelled or expired context surfaces as
//     context.Canceled or context.DeadlineExceeded respectively.
package ch_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// Compile-time pins: the client, its constructor, its methods, the state
// type, and the status error must exist with exactly these names and
// signatures.
var (
	_ *ch.Client      = ch.NewClient("/tmp/api.sock")
	_ ch.VMState      = "Running"
	_ *ch.StatusError = &ch.StatusError{}
	_ interface {
		Boot(context.Context) error
		Shutdown(context.Context) error
		Info(context.Context) (ch.VMState, error)
	} = &ch.Client{}
)

// TestClientBoot pins the boot request contract: PUT /api/v1/vm.boot with an
// empty body, nil for a 2xx response, and a typed StatusError carrying the
// status code for any non-2xx response.
func TestClientBoot(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		payload    string
		wantErr    bool
		wantStatus int
	}{
		{name: "no content", status: http.StatusNoContent},
		{name: "ok", status: http.StatusOK},
		{
			name:       "internal error",
			status:     http.StatusInternalServerError,
			payload:    `{"error":"internal fault"}`,
			wantErr:    true,
			wantStatus: http.StatusInternalServerError,
		},
		{
			name:       "not found",
			status:     http.StatusNotFound,
			payload:    `{"error":"VM not created"}`,
			wantErr:    true,
			wantStatus: http.StatusNotFound,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(tt.status, tt.payload)
			client := mustNewClient(t, newSocketServer(t, rec))

			err := client.Boot(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Fatal("Boot returned nil for a non-2xx response, want error")
				}
				assertStatusError(t, err, tt.wantStatus)
			} else if err != nil {
				t.Fatalf("Boot returned %v for a 2xx response, want nil", err)
			}

			method, path, body := rec.snapshot()
			if method != http.MethodPut {
				t.Errorf("Boot method = %q, want %q", method, http.MethodPut)
			}
			if path != "/api/v1/vm.boot" {
				t.Errorf("Boot path = %q, want %q", path, "/api/v1/vm.boot")
			}
			if len(body) != 0 {
				t.Errorf("Boot sent a %d-byte request body, want none", len(body))
			}
		})
	}
}

// TestClientShutdown pins the shutdown request contract: PUT
// /api/v1/vm.shutdown with an empty body, nil for a 2xx response, and a typed
// StatusError carrying the status code for any non-2xx response.
func TestClientShutdown(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		payload    string
		wantErr    bool
		wantStatus int
	}{
		{name: "no content", status: http.StatusNoContent},
		{
			name:       "not running",
			status:     http.StatusMethodNotAllowed,
			payload:    `{"error":"VM not started"}`,
			wantErr:    true,
			wantStatus: http.StatusMethodNotAllowed,
		},
		{
			name:       "internal error",
			status:     http.StatusInternalServerError,
			payload:    `{"error":"internal fault"}`,
			wantErr:    true,
			wantStatus: http.StatusInternalServerError,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(tt.status, tt.payload)
			client := mustNewClient(t, newSocketServer(t, rec))

			err := client.Shutdown(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Fatal("Shutdown returned nil for a non-2xx response, want error")
				}
				assertStatusError(t, err, tt.wantStatus)
			} else if err != nil {
				t.Fatalf("Shutdown returned %v for a 2xx response, want nil", err)
			}

			method, path, body := rec.snapshot()
			if method != http.MethodPut {
				t.Errorf("Shutdown method = %q, want %q", method, http.MethodPut)
			}
			if path != "/api/v1/vm.shutdown" {
				t.Errorf("Shutdown path = %q, want %q", path, "/api/v1/vm.shutdown")
			}
			if len(body) != 0 {
				t.Errorf("Shutdown sent a %d-byte request body, want none", len(body))
			}
		})
	}
}

// TestClientInfo pins the info request contract: GET /api/v1/vm.info, parsing
// the VM state from the JSON response, with a typed StatusError on non-2xx
// and a plain error when the body is not valid JSON.
func TestClientInfo(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		payload    string
		wantState  string
		wantErr    bool
		wantStatus int
	}{
		{
			name:      "running",
			status:    http.StatusOK,
			payload:   `{"config":{"payload":{"kernel":"/vmlinuz"}},"state":"Running"}`,
			wantState: "Running",
		},
		{name: "shutdown", status: http.StatusOK, payload: `{"state":"Shutdown"}`, wantState: "Shutdown"},
		{name: "created", status: http.StatusOK, payload: `{"state":"Created"}`, wantState: "Created"},
		{
			name:       "internal error",
			status:     http.StatusInternalServerError,
			payload:    `{"error":"internal fault"}`,
			wantErr:    true,
			wantStatus: http.StatusInternalServerError,
		},
		{name: "malformed json", status: http.StatusOK, payload: `not json`, wantErr: true},
		{name: "empty body", status: http.StatusOK, payload: ``, wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			rec := newRecorder(tt.status, tt.payload)
			client := mustNewClient(t, newSocketServer(t, rec))

			state, err := client.Info(t.Context())
			if tt.wantErr {
				if err == nil {
					t.Fatal("Info returned nil error, want error")
				}
				if tt.wantStatus != 0 {
					assertStatusError(t, err, tt.wantStatus)
				} else {
					var statusErr *ch.StatusError
					if errors.As(err, &statusErr) {
						t.Errorf("Info decode failure surfaced as %T with status %d, want a non-status error", statusErr, statusErr.StatusCode)
					}
				}
				return
			}
			if err != nil {
				t.Fatalf("Info returned %v, want nil", err)
			}
			if string(state) != tt.wantState {
				t.Errorf("Info state = %q, want %q", string(state), tt.wantState)
			}

			method, path, _ := rec.snapshot()
			if method != http.MethodGet {
				t.Errorf("Info method = %q, want %q", method, http.MethodGet)
			}
			if path != "/api/v1/vm.info" {
				t.Errorf("Info path = %q, want %q", path, "/api/v1/vm.info")
			}
		})
	}
}

// TestClientConnectionFailure pins the transport-failure contract: when the
// socket does not exist (the VM is gone or never started), the API call fails
// with a transport error, never a StatusError.
func TestClientConnectionFailure(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "missing.sock")
	client := mustNewClient(t, missing)

	err := client.Boot(t.Context())
	if err == nil {
		t.Fatal("Boot succeeded against a nonexistent socket, want error")
	}

	var netErr net.Error
	if !errors.As(err, &netErr) {
		t.Errorf("connection error %v does not wrap net.Error", err)
	}
	var statusErr *ch.StatusError
	if errors.As(err, &statusErr) {
		t.Errorf("connection failure surfaced as %T with status %d, want a transport error", statusErr, statusErr.StatusCode)
	}
}

// TestClientContextCancellation pins that a cancelled context aborts the
// request before it is sent: the call returns context.Canceled.
func TestClientContextCancellation(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	})
	client := mustNewClient(t, newSocketServer(t, handler))

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	err := client.Boot(ctx)
	if err == nil {
		t.Fatal("Boot with a cancelled context returned nil, want error")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("Boot error = %v, want context.Canceled", err)
	}
}

// TestClientContextCancellationInFlight pins that cancelling the context while
// a request is in flight aborts it: the call returns context.Canceled.
func TestClientContextCancellationInFlight(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	})
	client := mustNewClient(t, newSocketServer(t, handler))

	ctx, cancel := context.WithCancel(t.Context())

	errCh := make(chan error, 1)
	go func() {
		errCh <- client.Boot(ctx)
	}()

	time.Sleep(50 * time.Millisecond)
	cancel()

	select {
	case err := <-errCh:
		if err == nil {
			t.Fatal("Boot after in-flight cancellation returned nil, want error")
		}
		if !errors.Is(err, context.Canceled) {
			t.Errorf("Boot error = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Boot did not return after the context was cancelled")
	}
}

// TestClientContextDeadline pins that an expired context deadline aborts a
// request the server is still processing: the call returns
// context.DeadlineExceeded.
func TestClientContextDeadline(t *testing.T) {
	handler := http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		<-req.Context().Done()
	})
	client := mustNewClient(t, newSocketServer(t, handler))

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := client.Boot(ctx)
	if err == nil {
		t.Fatal("Boot with an expiring context returned nil, want error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("Boot error = %v, want context.DeadlineExceeded", err)
	}
}

// mustNewClient builds a client for socketPath and fails the test if the
// constructor misbehaves.
func mustNewClient(t *testing.T, socketPath string) *ch.Client {
	t.Helper()

	client := ch.NewClient(socketPath)
	if client == nil {
		t.Fatal("NewClient returned nil")
	}
	return client
}

// assertStatusError fails unless err is a *StatusError carrying wantStatus.
func assertStatusError(t *testing.T, err error, wantStatus int) {
	t.Helper()

	var statusErr *ch.StatusError
	if !errors.As(err, &statusErr) {
		t.Fatalf("error %v does not wrap *StatusError", err)
	}
	if statusErr.StatusCode != wantStatus {
		t.Errorf("StatusError.StatusCode = %d, want %d", statusErr.StatusCode, wantStatus)
	}
}

// requestRecorder is a test HTTP handler that records the last request and
// responds with a fixed status code and body. It is safe for concurrent use
// so the -race detector stays quiet when the test goroutine reads what the
// server goroutine wrote.
type requestRecorder struct {
	mu      sync.Mutex
	method  string
	path    string
	body    []byte
	status  int
	payload string
}

func newRecorder(status int, payload string) *requestRecorder {
	return &requestRecorder{status: status, payload: payload}
}

func (r *requestRecorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()

	r.method = req.Method
	r.path = req.URL.Path
	r.body, _ = io.ReadAll(req.Body)

	w.WriteHeader(r.status)
	if r.payload != "" {
		_, _ = fmt.Fprint(w, r.payload)
	}
}

// snapshot returns the recorded request fields under the lock.
func (r *requestRecorder) snapshot() (method, path string, body []byte) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.method, r.path, append([]byte(nil), r.body...)
}

// newSocketServer starts an HTTP server on a unix socket in a temporary
// directory and returns the socket path. The server is closed when the test
// ends.
func newSocketServer(t *testing.T, handler http.Handler) string {
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

	return socketPath
}
