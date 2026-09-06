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

// Contract tests for the etcd snapshot capture client.
//
// Capture POSTs the v3 grpc-gateway kv snapshot endpoint and returns the
// body verbatim: a 200 with bytes succeeds, a non-200 answer, an empty 200
// body, and a connection failure surface as errors. The tests run against
// real httptest servers, never a live VM.

package etcdsnap_test

import (
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/etcdsnap"
)

// snapshotServer serves the given body at the snapshot endpoint and records
// the request method it saw.
func snapshotServer(t *testing.T, status int, body []byte) (*httptest.Server, *string) {
	t.Helper()

	var method string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method = r.Method
		if r.URL.Path != "/v3/kv/snapshot" {
			http.NotFound(w, r)
			return
		}

		w.WriteHeader(status)
		_, _ = w.Write(body)
	}))
	t.Cleanup(server.Close)

	return server, &method
}

// serverHostPort splits an httptest server URL into host and port for Capture.
func serverHostPort(t *testing.T, server *httptest.Server) (string, int32) {
	t.Helper()

	hostPort := strings.TrimPrefix(server.URL, "http://")
	host, portStr, _ := strings.Cut(hostPort, ":")

	port, err := strconv.Atoi(portStr)
	if err != nil {
		t.Fatalf("parse test server port: %v", err)
	}

	return host, int32(port)
}

// TestCapture pins the happy path: the snapshot bytes come back verbatim and
// the request is a POST against the snapshot endpoint.
func TestCapture(t *testing.T) {
	want := []byte{0x53, 0x51, 0x4c, 0x69, 0x74, 0x65, 0x00, 0x01, 0x02}
	server, method := snapshotServer(t, http.StatusOK, want)
	host, port := serverHostPort(t, server)

	got, err := etcdsnap.Capture(t.Context(), host, port)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}

	if !slices.Equal(got, want) {
		t.Errorf("Capture = %v, want %v", got, want)
	}

	if *method != http.MethodPost {
		t.Errorf("request method = %q, want %q", *method, http.MethodPost)
	}
}

// TestCaptureFailures pins the error paths: non-200 answers, empty 200
// bodies, and unreachable hosts all surface as errors.
func TestCaptureFailures(t *testing.T) {
	t.Run("non-200 answer is an error", func(t *testing.T) {
		server, _ := snapshotServer(t, http.StatusServiceUnavailable, []byte("etcdserver: no leader"))

		host, port := serverHostPort(t, server)
		if _, err := etcdsnap.Capture(t.Context(), host, port); err == nil {
			t.Error("Capture on 503: want error, got nil")
		}
	})

	t.Run("empty 200 body is an error", func(t *testing.T) {
		server, _ := snapshotServer(t, http.StatusOK, nil)

		host, port := serverHostPort(t, server)
		if _, err := etcdsnap.Capture(t.Context(), host, port); err == nil {
			t.Error("Capture on empty 200: want error, got nil")
		}
	})

	t.Run("unreachable host is an error", func(t *testing.T) {
		if _, err := etcdsnap.Capture(t.Context(), "127.0.0.1", 1); err == nil {
			t.Error("Capture on a closed port: want error, got nil")
		}
	})
}
