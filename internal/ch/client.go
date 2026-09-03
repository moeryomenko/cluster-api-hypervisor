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
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
)

// VMState is the run state of a cloud-hypervisor VM reported by the vm.info
// endpoint, for example "Created", "Running", or "Shutdown".
type VMState string

// StatusError reports that the cloud-hypervisor API answered with a status
// code outside the 2xx range. StatusCode holds the HTTP status code; Body
// holds the truncated response body when the API returned one, so the API's
// own reason (for example ["The VM could not boot","VM config is missing"])
// reaches events and logs instead of a bare status code.
type StatusError struct {
	StatusCode int
	Body       string
}

// statusErrorBodyLimit bounds how much of a non-2xx response body is
// captured into StatusError.Body: cloud-hypervisor error bodies are small
// JSON arrays, and an unbounded capture could balloon events and logs.
const statusErrorBodyLimit = 4 * 1024

// Error implements the error interface.
func (e *StatusError) Error() string {
	msg := fmt.Sprintf("cloud-hypervisor API returned status %d", e.StatusCode)
	if body := strings.TrimSpace(e.Body); body != "" {
		msg += fmt.Sprintf(": %s", body)
	}

	return msg
}

// DefaultMemorySize is cloud-hypervisor's default guest memory size in
// bytes (512 MiB, as reported by vm.info for a config without an explicit
// memory section). It pins the size sent alongside shared=true, so a
// vhost-user VM keeps the default footprint.
const DefaultMemorySize int64 = 512 * 1024 * 1024

// PayloadConfig is the boot-payload section of a VmConfig: the firmware blob
// the VM boots from. Field names follow the cloud-hypervisor REST API JSON
// schema (verified against cloud-hypervisor 48 and 53).
type PayloadConfig struct {
	// Firmware is the path of the firmware image the VM boots from.
	Firmware string `json:"firmware,omitempty"`
}

// CpusConfig is the vCPU section of a VmConfig. BootVCPUs is the vCPU count
// the VM boots with; MaxVCPUs is the ceiling the guest can hot-add up to.
// Both carry the same spec value: the provider does not expose hot-add.
type CpusConfig struct {
	BootVCPUs int `json:"boot_vcpus"`
	MaxVCPUs  int `json:"max_vcpus"`
}

// MemoryConfig is the guest memory section of a VmConfig. Size is in bytes.
// Shared must be true when any vhost-user device is attached: cloud-hypervisor
// rejects booting such a VM on private memory.
type MemoryConfig struct {
	Size   int64 `json:"size"`
	Shared bool  `json:"shared"`
}

// DiskConfig is one disk entry of a VmConfig. Path is the host-side image
// file (qcow2 or raw) backing the virtio-blk device. Readonly attaches the
// disk read-only (readonly: true in the vm.create body); the CIDATA disk is
// the only read-only disk — the guest must not rewrite its cloud-init parts.
type DiskConfig struct {
	Path     string `json:"path"`
	Readonly bool   `json:"readonly,omitempty"`
}

// NetConfig is one net device entry of a VmConfig. VhostUser with
// VhostSocket selects a userspace vhost-user backend that cloud-hypervisor
// dials as a client; NumQueues counts virtqueues and must be at least 2 (one
// rx plus one tx queue). Field names follow the cloud-hypervisor REST API
// JSON schema; note the argv form spells the socket parameter "socket" while
// the JSON form uses "vhost_socket".
type NetConfig struct {
	VhostUser   bool   `json:"vhost_user"`
	VhostSocket string `json:"vhost_socket,omitempty"`
	MAC         string `json:"mac,omitempty"`
	NumQueues   int    `json:"num_queues"`
}

// VmConfig is the full VM configuration pushed to PUT /api/v1/vm.create
// before Boot. Fields left nil or empty let cloud-hypervisor apply its own
// defaults, matching the previous argv-less spawn behavior for cpus and
// memory size.
type VmConfig struct {
	Payload *PayloadConfig `json:"payload,omitempty"`
	Cpus    *CpusConfig    `json:"cpus,omitempty"`
	Memory  *MemoryConfig  `json:"memory,omitempty"`
	Disks   []DiskConfig   `json:"disks,omitempty"`
	Net     []NetConfig    `json:"net,omitempty"`
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
	_, err := c.do(ctx, http.MethodPut, vmBootPath, nil)
	return err
}

// Shutdown sends PUT /api/v1/vm.shutdown with no request body, with the same
// success and error semantics as Boot.
func (c *Client) Shutdown(ctx context.Context) error {
	_, err := c.do(ctx, http.MethodPut, vmShutdownPath, nil)
	return err
}

// Create sends PUT /api/v1/vm.create with cfg as the JSON request body,
// pushing the full VM configuration (firmware, disks, net devices, memory)
// into a not-yet-created VM. The API rejects a second create for an already
// created VM with status 500, which surfaces as a *StatusError; callers gate
// the create on Info reporting no VM (status 404, or status 500 with an
// "VM is not created" body on some cloud-hypervisor versions). Success and
// error semantics match Boot otherwise.
func (c *Client) Create(ctx context.Context, cfg VmConfig) error {
	body, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal vm.create config: %w", err)
	}

	_, err = c.do(ctx, http.MethodPut, vmCreatePath, body)

	return err
}

// Info sends GET /api/v1/vm.info and returns the VM state parsed from the
// JSON response. A non-2xx response maps to a *StatusError carrying the
// status code; a response body that is not a valid vm.info document maps to
// a plain (non-status) error.
func (c *Client) Info(ctx context.Context) (VMState, error) {
	body, err := c.do(ctx, http.MethodGet, vmInfoPath, nil)
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

	vmCreatePath   = "/api/v1/vm.create"
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
// the status code and the (truncated) response body. A failure to reach the
// socket surfaces as the underlying transport error (a net.Error); a
// cancelled or expired context surfaces as context.Canceled or
// context.DeadlineExceeded respectively.
func (c *Client) do(ctx context.Context, method, path string, body []byte) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, method, apiBaseURL+path, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	if err != nil {
		return nil, err
	}

	resp, err := c.httpClient.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < http.StatusOK || resp.StatusCode > 299 {
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, statusErrorBodyLimit))
		_ = resp.Body.Close()

		return nil, &StatusError{StatusCode: resp.StatusCode, Body: string(errBody)}
	}

	respBody, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()

	if err != nil {
		return nil, err
	}

	return respBody, nil
}
