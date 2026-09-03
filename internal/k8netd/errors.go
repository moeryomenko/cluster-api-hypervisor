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

// Package k8netd implements a JSON-RPC 2.0 client for the k8netd daemon.
package k8netd

import (
	"errors"
	"fmt"
)

// Sentinel errors for k8netd typed error codes. Each maps to a contract code
// via mapRPCError and is usable with errors.Is.
var (
	// ErrNotFound indicates the requested resource does not exist (code not_found).
	ErrNotFound = errors.New("not_found")
	// ErrAlreadyExists indicates the resource already exists (code already_exists).
	ErrAlreadyExists = errors.New("already_exists")
	// ErrInvalidParams indicates invalid parameters or version mismatch (code invalid_params).
	ErrInvalidParams = errors.New("invalid_params")
	// ErrConflict indicates an idempotent create with differing params (code conflict).
	ErrConflict = errors.New("conflict")
	// ErrInternal indicates an internal server error or unknown code (code internal).
	ErrInternal = errors.New("internal")
)

// rpcError is the wire error shape returned by k8netd.
type rpcError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

// mapRPCError converts a wire RPC error code to a sentinel error wrapped with
// the message, preserving errors.Is semantics via %w.
func mapRPCError(code, message string) error {
	if message == "" {
		message = code
	}

	var sentinel error

	switch code {
	case "not_found":
		sentinel = ErrNotFound
	case "already_exists":
		sentinel = ErrAlreadyExists
	case "invalid_params":
		sentinel = ErrInvalidParams
	case "conflict":
		sentinel = ErrConflict
	case "internal":
		sentinel = ErrInternal
	default:
		sentinel = ErrInternal
	}

	return fmt.Errorf("%w: %s: %s", sentinel, code, message)
}
