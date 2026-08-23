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

// Package chclient is the thin VM-control layer the Machine controller uses
// to drive one cloud-hypervisor VM: spawn the subprocess, boot the VM over
// the API socket, report its state, shut it down gracefully, and tear the
// process down. The layer wraps the process Manager and the HTTP Client from
// internal/ch. The Client interface is implemented by VMClient for real VMs
// and by FakeClient in controller tests.
package chclient

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// Client drives one cloud-hypervisor VM. Implementations must be safe for
// concurrent use because the Machine controller reconciles from concurrent
// workers.
type Client interface {
	// SetNetConfig supplies the cloud-hypervisor net device string (--net)
	// used when the client next spawns the VM process. Call it before
	// EnsureRunning so the VM boots with its k8netd vhost-user network
	// attached.
	SetNetConfig(netConfig string)
	// EnsureRunning spawns the cloud-hypervisor process when it is not
	// running and boots the VM through the API. It is a no-op when the VM is
	// already running.
	EnsureRunning(ctx context.Context) error
	// Shutdown asks the VM to shut down gracefully through the API.
	Shutdown(ctx context.Context) error
	// Stop tears everything down: it stops the VM and the cloud-hypervisor
	// process.
	Stop(ctx context.Context) error
	// Info reports the current VM state. When the VM is absent it returns
	// ErrNotFound.
	Info(ctx context.Context) (ch.VMState, error)
}

// VMClient drives one cloud-hypervisor VM through a ch.Manager that owns the
// subprocess and a ch.Client that talks to the API socket. It is safe for
// concurrent use because both components are.
type VMClient struct {
	manager *ch.Manager
	client  *ch.Client
	socket  string
}

// socketReadyTimeout bounds how long EnsureRunning waits for the API socket
// to accept connections after spawning the cloud-hypervisor process.
const socketReadyTimeout = 10 * time.Second

// ErrNotFound is the sentinel Info returns when the VM does not exist.
// Callers detect an absent VM with errors.Is(err, ErrNotFound).
var ErrNotFound = errors.New("vm not found")

// NewVMClient constructs a VMClient for one VM. socketDir is the directory
// that holds the API socket; the manager creates it on the first
// EnsureRunning and removes it on Stop. binaryPath is the cloud-hypervisor
// executable, or empty to resolve "cloud-hypervisor" from PATH.
func NewVMClient(socketDir, binaryPath string) *VMClient {
	return &VMClient{
		manager: ch.NewManager(ch.WithSocketDir(socketDir), ch.WithBinaryPath(binaryPath)),
		client:  ch.NewClient(filepath.Join(socketDir, "api.sock")),
		socket:  filepath.Join(socketDir, "api.sock"),
	}
}

// SetNetConfig forwards the net device string to the process manager; it is
// used at the next process spawn.
func (c *VMClient) SetNetConfig(netConfig string) {
	c.manager.SetNetConfig(netConfig)
}

// EnsureRunning spawns the cloud-hypervisor process when it is not running,
// waits for the API socket, and boots the VM through the API. It is a no-op
// when the VM is already running.
func (c *VMClient) EnsureRunning(ctx context.Context) error {
	if _, err := c.manager.Start(ctx); err != nil {
		return err
	}
	if err := c.manager.WaitReady(ctx, socketReadyTimeout); err != nil {
		return err
	}

	state, err := c.client.Info(ctx)
	if err == nil && state == ch.VMState("Running") {
		return nil
	}
	return c.client.Boot(ctx)
}

// Shutdown asks the VM to shut down gracefully through the API. When the VM
// is absent, the socket does not exist and Shutdown returns ErrNotFound.
func (c *VMClient) Shutdown(ctx context.Context) error {
	if _, err := os.Stat(c.socket); errors.Is(err, os.ErrNotExist) {
		return ErrNotFound
	}
	return c.client.Shutdown(ctx)
}

// Stop terminates the cloud-hypervisor process and removes the socket
// directory. It is idempotent and safe to call when the VM was never started.
func (c *VMClient) Stop(ctx context.Context) error {
	return c.manager.Stop(ctx)
}

// Info reports the current VM state from the API. When the VM is absent, the
// socket does not exist because the process was never started or has been
// torn down, and Info returns ErrNotFound.
func (c *VMClient) Info(ctx context.Context) (ch.VMState, error) {
	if _, err := os.Stat(c.socket); errors.Is(err, os.ErrNotExist) {
		return "", ErrNotFound
	}
	return c.client.Info(ctx)
}
