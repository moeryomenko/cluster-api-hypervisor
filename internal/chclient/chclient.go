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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// Client drives one cloud-hypervisor VM. Implementations must be safe for
// concurrent use because the Machine controller reconciles from concurrent
// workers.
type Client interface {
	// SetNetConfig supplies the cloud-hypervisor net device string (the
	// --net argv form) used to build the VM configuration pushed over the
	// API before the next boot. Call it before EnsureRunning so the VM boots
	// with its k8netd vhost-user network attached.
	SetNetConfig(netConfig string)
	// SetFirmware supplies the firmware image path the VM boots from. Call
	// it before EnsureRunning; without a firmware the VM has no boot medium
	// and cloud-hypervisor rejects the boot.
	SetFirmware(firmware string)
	// SetDiskPaths supplies the host-side disk images attached to the VM at
	// the next configuration push: the root qcow2 first, then any confext
	// data raws. Call it before EnsureRunning.
	SetDiskPaths(paths []string)
	// EnsureRunning spawns the cloud-hypervisor process when it is not
	// running, pushes the VM configuration over the API when the VM does not
	// exist yet, and boots the VM. It is a no-op when the VM is already
	// running.
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

	// netConfig holds the --net argv-form device string rendered by the
	// controller; firmware and diskPaths hold the boot medium and disk
	// images. All three are consumed by pushConfig when EnsureRunning
	// creates the VM.
	netConfig string
	firmware  string
	diskPaths []string
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

// SetNetConfig records the net device string used to build the VM
// configuration pushed over the API.
func (c *VMClient) SetNetConfig(netConfig string) {
	c.netConfig = netConfig
}

// SetFirmware records the firmware image path the VM boots from.
func (c *VMClient) SetFirmware(firmware string) {
	c.firmware = firmware
}

// SetDiskPaths records the disk images attached at the next configuration
// push.
func (c *VMClient) SetDiskPaths(paths []string) {
	c.diskPaths = paths
}

// EnsureRunning spawns the cloud-hypervisor process when it is not running,
// waits for the API socket, pushes the full VM configuration when the VM
// does not exist yet, and boots the VM. It is a no-op when the VM is already
// running, and boots without re-pushing an identical configuration when the
// VM exists in the Created state.
func (c *VMClient) EnsureRunning(ctx context.Context) error {
	if _, err := c.manager.Start(ctx); err != nil {
		return err
	}
	if err := c.manager.WaitReady(ctx, socketReadyTimeout); err != nil {
		return err
	}
	return c.ensureBooted(ctx)
}

// ensureBooted drives the API half of EnsureRunning once the process and its
// socket are up. A VM reporting Running needs nothing; a VM already created
// keeps its configuration and only boots again; a missing VM (the info
// endpoint answers 404, or 500 with an "VM is not created" body on some
// cloud-hypervisor versions) gets the full configuration pushed and then
// boots. Any other Info failure surfaces unchanged so transport faults are
// not mistaken for an absent VM.
func (c *VMClient) ensureBooted(ctx context.Context) error {
	state, err := c.client.Info(ctx)
	if err == nil {
		if state == ch.VMState("Running") {
			return nil
		}
		return c.client.Boot(ctx)
	}
	if !isVMAbsent(err) {
		return err
	}

	if err := c.pushConfig(ctx); err != nil {
		return err
	}
	return c.client.Boot(ctx)
}

// pushConfig builds the VM configuration from the recorded net device
// string, firmware, and disk paths, and pushes it with vm.create. The
// firmware is required: without it cloud-hypervisor accepts the create but
// rejects the later boot as not bootable, so the misconfiguration surfaces
// here instead. When a net device is configured, guest memory must be shared
// for vhost-user, so the memory section pins the cloud-hypervisor default
// size with shared=true; without a net device no memory section is sent and
// the library defaults apply untouched.
func (c *VMClient) pushConfig(ctx context.Context) error {
	if c.firmware == "" {
		return errors.New("push vm config: no firmware configured (SetFirmware)")
	}

	cfg := ch.VmConfig{
		Payload: &ch.PayloadConfig{Firmware: c.firmware},
	}
	for _, path := range c.diskPaths {
		cfg.Disks = append(cfg.Disks, ch.DiskConfig{Path: path})
	}
	if c.netConfig != "" {
		net, err := ch.ParseNetConfig(c.netConfig)
		if err != nil {
			return fmt.Errorf("push vm config: %w", err)
		}
		cfg.Net = []ch.NetConfig{net}
		cfg.Memory = &ch.MemoryConfig{Size: ch.DefaultMemorySize, Shared: true}
	}

	if err := c.client.Create(ctx, cfg); err != nil {
		return fmt.Errorf("push vm config: %w", err)
	}
	return nil
}

// vmAbsentBodyMarker is the body substring some cloud-hypervisor versions
// return with vm.info status 500 when the VM does not exist yet.
const vmAbsentBodyMarker = "vm is not created"

// isVMAbsent reports whether err is the cloud-hypervisor API's answer for a
// VM that does not exist yet. Most cloud-hypervisor versions answer vm.info
// with status 404; some versions answer status 500 with a body naming the
// missing VM ("VM is not created"), so both shapes classify as absent. Any
// other status or a non-status error is not an absent VM.
func isVMAbsent(err error) bool {
	var statusErr *ch.StatusError
	if !errors.As(err, &statusErr) {
		return false
	}
	if statusErr.StatusCode == http.StatusNotFound {
		return true
	}
	return statusErr.StatusCode == http.StatusInternalServerError &&
		strings.Contains(strings.ToLower(statusErr.Body), vmAbsentBodyMarker)
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
