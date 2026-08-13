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

package ch

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
)

// VMState is the run state of a cloud-hypervisor VM reported by the vm.info
// endpoint, for example "Created", "Running", or "Shutdown".
type VMState string

// StatusError reports that the cloud-hypervisor API answered with a status
// code outside the 2xx range. StatusCode holds the HTTP status code.
type StatusError struct {
	StatusCode int
}

// Error implements the error interface.
func (e *StatusError) Error() string {
	return fmt.Sprintf("cloud-hypervisor API returned status %d", e.StatusCode)
}

// Client drives the cloud-hypervisor REST API over the unix api socket that
// the Manager creates for a VM. The underlying http.Client is safe for
// concurrent use.
type Client struct {
	httpClient *http.Client
}

// NewClient constructs a Client that talks to the cloud-hypervisor API
// socket at socketPath. It does not require the socket to exist yet: a VM may
// still be starting, so connection failures surface on the first API call,
// not at construction.
func NewClient(socketPath string) *Client {
	return &Client{
		httpClient: &http.Client{
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					// The request URL host is arbitrary; the dialer always
					// connects to the API socket.
					return (&net.Dialer{}).DialContext(ctx, "unix", socketPath)
				},
			},
		},
	}
}

// Boot sends PUT /api/v1/vm.boot with no request body. It returns nil when
// the API accepts the request, a *StatusError carrying the status code for
// any non-2xx response, and the underlying transport error (a net.Error)
// when the socket cannot be reached.
func (c *Client) Boot(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPut, vmBootPath)
	return err
}

// Shutdown sends PUT /api/v1/vm.shutdown with no request body, with the same
// success and error semantics as Boot.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPut, vmShutdownPath)
	return err
}

// Info sends GET /api/v1/vm.info and returns the VM state parsed from the
// JSON response. A non-2xx response maps to a *StatusError carrying the
// status code; a response body that is not a valid vm.info document maps to
// a plain (non-status) error.
func (c *Client) Info(ctx context.Context) (VMState, error) {
	body, err := c.do(ctx, http.MethodGet, vmInfoPath)
	if err != nil {
		return "", err
	}

	var info vmInfo
	if err := json.Unmarshal(body, &info); err != nil {
		return "", err
	}
	return info.State, nil
}

const (
	// apiBaseURL is the request URL origin. The dialer ignores the host and
	// always connects to the API socket, so the exact origin is irrelevant.
	apiBaseURL = "http://localhost"

	vmBootPath     = "/api/v1/vm.boot"
	vmShutdownPath = "/api/v1/vm.shutdown"
	vmInfoPath     = "/api/v1/vm.info"
)

// vmInfo is the subset of the vm.info response the client reads.
type vmInfo struct {
	State VMState `json:"state"`
}

// do sends an HTTP request to the API socket. For a 2xx response it returns
// the response body; any other status is reported as a *StatusError carrying
// the status code. A failure to reach the socket surfaces as the underlying
// transport error (a net.Error); a cancelled or expired context surfaces as
// context.Canceled or context.DeadlineExceeded respectively.
func (c *Client) do(ctx context.Context, method, path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBaseURL+path, nil)
	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		_ = resp.Body.Close()
		return nil, &StatusError{StatusCode: resp.StatusCode}
	}

	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return nil, err
	}
	return body, nil
}
