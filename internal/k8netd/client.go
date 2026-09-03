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

package k8netd

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync/atomic"
	"time"
)

// k8netdVersion is the contract version sent with every request. The server
// rejects mismatched versions with invalid_params.
const k8netdVersion = "1.0"

// Client is a JSON-RPC 2.0 client for k8netd over a Unix socket.
type Client struct {
	socketPath string
	version    string
	nextID     atomic.Int64
}

// Network describes a k8netd network. JSON tags match the contract envelope.
type Network struct {
	Name      string `json:"name"`
	CIDR      string `json:"cidr"`
	Gateway   string `json:"gateway"`
	PoolStart string `json:"poolStart"`
	PoolEnd   string `json:"poolEnd"`
}

// Port describes a k8netd port (vhost-user backend endpoint).
type Port struct {
	Name       string `json:"name"`
	Network    string `json:"network"`
	MAC        string `json:"mac,omitempty"`
	IP         string `json:"ip,omitempty"`
	SocketPath string `json:"socketPath,omitempty"`
}

// NewClient creates a Client dialing the Unix socket at socketPath.
func NewClient(socketPath string) *Client {
	c := &Client{
		socketPath: socketPath,
		version:    k8netdVersion,
	}
	c.nextID.Store(0)

	return c
}

// CreateNetwork creates a network with the given CIDR, gateway and pool bounds.
func (c *Client) CreateNetwork(ctx context.Context, name, cidr, gateway, poolStart, poolEnd string) error {
	params := map[string]string{
		"name":      name,
		"cidr":      cidr,
		"gateway":   gateway,
		"poolStart": poolStart,
		"poolEnd":   poolEnd,
	}

	return c.call(ctx, "CreateNetwork", params, nil)
}

// DeleteNetwork deletes a network by name.
func (c *Client) DeleteNetwork(ctx context.Context, name string) error {
	params := map[string]string{"name": name}
	return c.call(ctx, "DeleteNetwork", params, nil)
}

// GetNetwork returns the network by name.
func (c *Client) GetNetwork(ctx context.Context, name string) (*Network, error) {
	params := map[string]string{"name": name}

	var out Network
	if err := c.call(ctx, "GetNetwork", params, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// CreatePort creates a port (vhost-user socket) by name.
func (c *Client) CreatePort(ctx context.Context, name string) error {
	params := map[string]string{"name": name}
	return c.call(ctx, "CreatePort", params, nil)
}

// DeletePort deletes a port by name.
func (c *Client) DeletePort(ctx context.Context, name string) error {
	params := map[string]string{"name": name}
	return c.call(ctx, "DeletePort", params, nil)
}

// GetPort returns the port by name.
func (c *Client) GetPort(ctx context.Context, name string) (*Port, error) {
	params := map[string]string{"name": name}

	var out Port
	if err := c.call(ctx, "GetPort", params, &out); err != nil {
		return nil, err
	}

	return &out, nil
}

// AttachPort attaches a port to a network under the given MAC address. The
// params travel under exactly the three canonical keys "port", "network",
// and "mac": the daemon's handler requires all three and binds the IPAM
// reservation to the MAC.
func (c *Client) AttachPort(ctx context.Context, port, network, mac string) error {
	params := map[string]string{
		"port":    port,
		"network": network,
		"mac":     mac,
	}

	return c.call(ctx, "AttachPort", params, nil)
}

// DetachPort detaches a port from its network. The port identity travels
// under the canonical "name" key, matching the daemon's DetachPort handler.
func (c *Client) DetachPort(ctx context.Context, port string) error {
	params := map[string]string{"name": port}
	return c.call(ctx, "DetachPort", params, nil)
}

// AllocateIP allocates an IP from the network's pool for the given MAC. The
// result must be exactly the contract envelope — a JSON string carrying the
// address; any other shape is a loud ErrInternal, never a guess.
func (c *Client) AllocateIP(ctx context.Context, network, mac string) (string, error) {
	params := map[string]string{
		"network": network,
		"mac":     mac,
	}

	var raw json.RawMessage
	if err := c.call(ctx, "AllocateIP", params, &raw); err != nil {
		return "", err
	}

	var ip string
	if err := json.Unmarshal(raw, &ip); err != nil {
		return "", fmt.Errorf("%w: AllocateIP result shape: want a JSON string, got %s", ErrInternal, string(raw))
	}

	if ip == "" {
		return "", fmt.Errorf("%w: AllocateIP returned an empty address", ErrInternal)
	}

	return ip, nil
}

// ReleaseIP releases the allocation for the given MAC on the network.
func (c *Client) ReleaseIP(ctx context.Context, network, mac string) error {
	params := map[string]string{
		"network": network,
		"mac":     mac,
	}

	return c.call(ctx, "ReleaseIP", params, nil)
}

// PublishPort asks the daemon to publish vmPort of the named port to the
// host, returning the allocated host port. The params travel under exactly
// the two canonical keys "port" and "vm_port" per the contract envelope. The
// result must be exactly the {"host_port": N} envelope with a positive port;
// any other shape is a loud ErrInternal, never a guess (same strictness as
// AllocateIP).
func (c *Client) PublishPort(ctx context.Context, port string, vmPort int32) (int32, error) {
	params := map[string]any{
		"port":    port,
		"vm_port": vmPort,
	}

	var raw json.RawMessage
	if err := c.call(ctx, "PublishPort", params, &raw); err != nil {
		return 0, err
	}

	var envelope struct {
		HostPort *int32 `json:"host_port"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return 0, fmt.Errorf("%w: PublishPort result shape: want {\"host_port\": N}, got %s", ErrInternal, string(raw))
	}

	if envelope.HostPort == nil || *envelope.HostPort <= 0 {
		return 0, fmt.Errorf("%w: PublishPort result shape: want a positive host_port, got %s", ErrInternal, string(raw))
	}

	return *envelope.HostPort, nil
}

// rpcRequest is the wire request envelope.
type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int64  `json:"id"`
	Version string `json:"version"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

// rpcResponse is the wire response envelope.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Version string          `json:"version,omitempty"`
	Result  json.RawMessage `json:"result"`
	Error   *rpcError       `json:"error,omitempty"`
}

// call performs a JSON-RPC call with backoff dial retry.
func (c *Client) call(ctx context.Context, method string, params any, result any) error {
	if ctx == nil {
		ctx = context.Background()
	}

	id := c.nextID.Add(1)
	req := rpcRequest{
		JSONRPC: "2.0",
		ID:      id,
		Version: c.version,
		Method:  method,
		Params:  params,
	}

	conn, err := c.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = conn.Close() }()

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		return fmt.Errorf("%w: encode request %q: %v", ErrInternal, method, err)
	}

	dec := json.NewDecoder(conn)

	var resp rpcResponse
	if err := dec.Decode(&resp); err != nil {
		return fmt.Errorf("%w: decode response %q: %v", ErrInternal, method, err)
	}

	if resp.Error != nil {
		return mapRPCError(resp.Error.Code, resp.Error.Message)
	}

	if result != nil {
		if len(resp.Result) == 0 || string(resp.Result) == "null" {
			// No result to decode for void methods.
			return nil
		}

		switch v := result.(type) {
		case *json.RawMessage:
			*v = resp.Result
		default:
			if err := json.Unmarshal(resp.Result, result); err != nil {
				return fmt.Errorf("%w: decode result %q: %v", ErrInternal, method, err)
			}
		}
	}

	return nil
}

// dial dials the Unix socket with bounded exponential backoff, retrying while
// the socket is absent until ctx is done or a default timeout expires.
func (c *Client) dial(ctx context.Context) (net.Conn, error) {
	start := time.Now()
	delay := 10 * time.Millisecond

	const (
		maxDelay       = 100 * time.Millisecond
		defaultTimeout = 2 * time.Second
	)

	var lastErr error

	for {
		conn, err := net.Dial("unix", c.socketPath)
		if err == nil {
			return conn, nil
		}

		lastErr = err

		// If context is done, return its error.
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		default:
		}

		// For contexts without deadline, bound by defaultTimeout.
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			if time.Since(start) >= defaultTimeout {
				return nil, fmt.Errorf("%w: dial %q: %v", ErrInternal, c.socketPath, lastErr)
			}
		}

		// Sleep with jitter respecting ctx.
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}

		delay = min(time.Duration(float64(delay)*1.5), maxDelay)
	}
}
