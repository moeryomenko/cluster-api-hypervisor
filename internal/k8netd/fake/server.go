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

// Package fake provides a reusable fake k8netd JSON-RPC 2.0 server over a Unix
// socket. It is safe for controller suites and for the client package tests.
// The server speaks the contract from .specs/k8netd-contract/spec.md REQ-001:
// JSON-RPC 2.0 envelope with a top-level "version" field and typed error codes
// (not_found, already_exists, invalid_params, conflict, internal). Tests inject
// per-method handlers or canned results/errors and inspect captured requests.
//
// Transport framing: one JSON value per connection (json.Decoder/Encoder), no
// Content-Length. Each client dials, writes a single request, reads a single
// response, closes.
package fake

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"
)

// CapturedRequest records one inbound JSON-RPC request.
type CapturedRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Version string          `json:"version"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
	Raw     json.RawMessage
}

// rpcRequest is the wire request.
type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Version string          `json:"version"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// rpcResponse is the wire response.
type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id"`
	Version string    `json:"version,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *RPCError `json:"error,omitempty"`
}

// RPCError is the wire error with machine-readable code. Codes are contract
// codes: not_found, already_exists, invalid_params, conflict, internal.
type RPCError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// HandlerFunc handles one method and returns a result or an error to send.
// Return (result, nil) on success, (nil, err) on typed error. If err is nil
// and result is nil, the response carries "result":null.
type HandlerFunc func(params json.RawMessage) (any, *RPCError)

// publishKey is the allocation key of the built-in PublishPort handler: the
// k8netd port name and the VM-side port published from it.
type publishKey struct {
	Port   string `json:"port"`
	VMPort int32  `json:"vm_port"`
}

// fakePublishBasePort is the first host port the built-in PublishPort
// allocator hands out, mirroring the daemon's default publish range start
// (REQ-010).
const fakePublishBasePort = 20000

// Server is a fake k8netd control-plane server.
type Server struct {
	mu              sync.Mutex
	path            string
	listener        net.Listener
	requests        []CapturedRequest
	handlers        map[string]HandlerFunc
	results         map[string]any
	errors          map[string]*RPCError
	expectedVersion string
	published       map[publishKey]int32
	nextHostPort    int32
	closed          bool
}

// New creates and starts a Server listening on socketPath. The caller should
// defer s.Close(). It removes any stale socket file before binding.
//
// Example:
//
//	dir := t.TempDir()
//	sock := filepath.Join(dir, "control.sock")
//	srv, err := fake.New(sock)
//	t.Cleanup(func(){ srv.Close() })
func New(socketPath string) (*Server, error) {
	_ = os.Remove(socketPath) // unlink stale file, per REQ-010

	l, err := net.Listen("unix", socketPath)
	if err != nil {
		return nil, fmt.Errorf("fake k8netd listen %q: %w", socketPath, err)
	}

	s := &Server{
		path:            socketPath,
		listener:        l,
		handlers:        make(map[string]HandlerFunc),
		results:         make(map[string]any),
		errors:          make(map[string]*RPCError),
		expectedVersion: "", // empty means accept any version
		published:       make(map[publishKey]int32),
		nextHostPort:    fakePublishBasePort,
	}
	go s.serve()

	return s, nil
}

// NewWithVersion is like New but enforces that each request's version equals
// expectedVersion; a mismatch yields an invalid_params error.
func NewWithVersion(socketPath, expectedVersion string) (*Server, error) {
	s, err := New(socketPath)
	if err != nil {
		return nil, err
	}

	s.expectedVersion = expectedVersion

	return s, nil
}

// SocketPath returns the Unix socket path this server listens on.
func (s *Server) SocketPath() string { return s.path }

// Handle registers a handler for method. It replaces any canned result/error.
func (s *Server) Handle(method string, fn HandlerFunc) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.handlers[method] = fn
	delete(s.results, method)
	delete(s.errors, method)
}

// SetResult makes method return result on success. Pass nil for null result.
func (s *Server) SetResult(method string, result any) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.results[method] = result
	delete(s.handlers, method)
	delete(s.errors, method)
}

// SetError makes method return the typed error code with message. Codes are
// contract codes: not_found, already_exists, invalid_params, conflict, internal.
func (s *Server) SetError(method, code, message string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.errors[method] = &RPCError{Code: code, Message: message}
	delete(s.handlers, method)
	delete(s.results, method)
}

// SetErrorCode is a shorthand that uses the code as message.
func (s *Server) SetErrorCode(method, code string) {
	s.SetError(method, code, code)
}

// SetExpectedVersion enforces version matching after creation.
func (s *Server) SetExpectedVersion(v string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.expectedVersion = v
}

// Requests returns a copy of all captured requests in order.
func (s *Server) Requests() []CapturedRequest {
	s.mu.Lock()
	defer s.mu.Unlock()

	out := make([]CapturedRequest, len(s.requests))
	copy(out, s.requests)

	return out
}

// RequestCount returns the number of captured requests.
func (s *Server) RequestCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()

	return len(s.requests)
}

// IsSet reports whether method already has a canned result, error or handler.
func (s *Server) IsSet(method string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, hasResult := s.results[method]
	_, hasErr := s.errors[method]
	_, hasHandler := s.handlers[method]

	return hasResult || hasErr || hasHandler
}

// Reset clears captured requests and per-method stubs, but keeps the listener.
func (s *Server) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.requests = nil
	s.handlers = make(map[string]HandlerFunc)
	s.results = make(map[string]any)
	s.errors = make(map[string]*RPCError)
}

// Close stops the server and removes the socket file.
func (s *Server) Close() error {
	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil
	}

	s.closed = true
	s.mu.Unlock()

	if s.listener != nil {
		_ = s.listener.Close()
	}

	_ = os.Remove(s.path)

	return nil
}

func (s *Server) serve() {
	for {
		conn, err := s.listener.Accept()
		if err != nil {
			return
		}

		go s.handleConn(conn)
	}
}

func (s *Server) handleConn(conn net.Conn) {
	defer func() { _ = conn.Close() }()

	dec := json.NewDecoder(conn)
	enc := json.NewEncoder(conn)

	var (
		req rpcRequest
		raw json.RawMessage
	)

	if err := dec.Decode(&req); err != nil {
		_ = enc.Encode(rpcResponse{
			JSONRPC: "2.0",
			ID:      nil,
			Error:   &RPCError{Code: "invalid_params", Message: fmt.Sprintf("decode error: %v", err)},
		})

		return
	}

	b, _ := json.Marshal(req)
	raw = b

	s.mu.Lock()
	s.requests = append(s.requests, CapturedRequest{
		JSONRPC: req.JSONRPC,
		ID:      req.ID,
		Version: req.Version,
		Method:  req.Method,
		Params:  req.Params,
		Raw:     raw,
	})
	expectedVersion := s.expectedVersion
	handler := s.handlers[req.Method]
	cannedResult, hasResult := s.results[req.Method]
	cannedErr, hasErr := s.errors[req.Method]
	s.mu.Unlock()

	if req.JSONRPC != "2.0" {
		_ = enc.Encode(
			rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: "invalid_params", Message: "jsonrpc must be 2.0"}},
		)

		return
	}

	if req.Method == "" {
		_ = enc.Encode(
			rpcResponse{JSONRPC: "2.0", ID: req.ID, Error: &RPCError{Code: "invalid_params", Message: "method required"}},
		)

		return
	}

	if expectedVersion != "" && req.Version != expectedVersion {
		_ = enc.Encode(rpcResponse{
			JSONRPC: "2.0",
			ID:      req.ID,
			Version: req.Version,
			Error: &RPCError{
				Code:    "invalid_params",
				Message: fmt.Sprintf("version mismatch: got %q want %q", req.Version, expectedVersion),
			},
		})

		return
	}

	if hasErr {
		_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Error: cannedErr})
		return
	}

	if handler != nil {
		result, herr := handler(req.Params)
		if herr != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Error: herr})
			return
		}

		_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Result: result})

		return
	}

	if hasResult {
		_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Result: cannedResult})
		return
	}

	if req.Method == "PublishPort" {
		result, perr := s.builtinPublishPort(req.Params)
		if perr != nil {
			_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Error: perr})
			return
		}

		_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Result: result})

		return
	}

	_ = enc.Encode(rpcResponse{JSONRPC: "2.0", ID: req.ID, Version: req.Version, Result: nil})
}

// builtinPublishPort serves PublishPort when no handler, canned result, or
// canned error is registered for the method: allocations are keyed by the
// full (port, vm_port) pair, an idempotent re-publish returns the same host
// port, and distinct pairs get distinct ports — the deterministic allocator
// semantics controller suites rely on without registering handlers. A
// registered handler via Handle (or SetResult/SetError) keeps shadowing this
// default.
func (s *Server) builtinPublishPort(params json.RawMessage) (any, *RPCError) {
	var key publishKey
	if err := json.Unmarshal(params, &key); err != nil {
		return nil, &RPCError{Code: "invalid_params", Message: fmt.Sprintf("decode PublishPort params: %v", err)}
	}

	if key.Port == "" || key.VMPort <= 0 {
		return nil, &RPCError{Code: "invalid_params", Message: "PublishPort params must carry port and vm_port"}
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.published == nil {
		s.published = make(map[publishKey]int32)
	}

	if host, ok := s.published[key]; ok {
		return map[string]int32{"host_port": host}, nil
	}

	host := s.nextHostPort
	s.nextHostPort++
	s.published[key] = host

	return map[string]int32{"host_port": host}, nil
}
