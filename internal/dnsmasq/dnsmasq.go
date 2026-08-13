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

// Package dnsmasq implements the provider's DNS forwarder: dnsmasq runs as a
// subprocess of the provider on the host network and serves the cluster
// bridge address as DNS for cluster VMs, forwarding upstream queries to the
// pinned resolvers. Machines use static IPs, so the forwarder never runs a
// DHCP server: the rendered config contains no directive that enables DHCP.
// The Manager renders the config and owns the subprocess lifecycle behind
// the injectable Runner seam, so no real dnsmasq process is ever needed in
// tests.
package dnsmasq

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
)

// Config carries the cluster-network inputs the rendered config depends on:
// the bridge interface to serve DNS on, the address to listen on, and the
// ordered upstream resolvers.
type Config struct {
	BridgeName    string   // interface to serve DNS on (k8sbr0)
	ListenAddress string   // address to bind and serve (192.168.124.1)
	Upstream      []string // ordered upstream resolvers (1.1.1.1, 8.8.8.8)
}

// Runner is the subprocess-lifecycle seam of the manager: Start spawns the
// program named name with the given arguments, attaching the given writers
// for its stdout and stderr, and returns once launched while the process
// keeps running; Stop terminates a previously started process. Production
// uses the host dnsmasq binary via os/exec; tests inject a recording fake so
// no external process is ever started.
type Runner interface {
	Start(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error
	Stop(ctx context.Context) error
}

// Manager owns one cluster network's dnsmasq process: it renders the config
// into the work directory as dnsmasq.conf and runs the host dnsmasq binary
// against that file in the foreground, so the provider owns the process.
type Manager struct {
	config  Config
	runner  Runner
	workDir string
	started bool
}

// execRunner is the default Runner: it spawns the program on the host and
// keeps the process reference so Stop can terminate it.
type execRunner struct {
	mu  sync.Mutex
	cmd *exec.Cmd
}

// Start spawns name with args on the host, attaching the writers to the
// process output, and returns once launched.
func (r *execRunner) Start(ctx context.Context, name string, args []string, stdout, stderr io.Writer) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	if err := cmd.Start(); err != nil {
		return err
	}

	r.mu.Lock()
	r.cmd = cmd
	r.mu.Unlock()

	return nil
}

// Stop terminates the previously started process with SIGTERM and waits for
// it to exit. Stopping without a started process is a no-op.
func (r *execRunner) Stop(context.Context) error {
	r.mu.Lock()
	cmd := r.cmd
	r.cmd = nil
	r.mu.Unlock()

	if cmd == nil || cmd.Process == nil {
		return nil
	}
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("dnsmasq: signal subprocess: %w", err)
	}

	return cmd.Wait()
}

// NewManager builds a manager for one cluster network over the given runner,
// writing its rendered config into workDir. A nil runner is replaced with
// the default exec-based runner spawning the host dnsmasq binary.
func NewManager(config Config, runner Runner, workDir string) *Manager {
	if runner == nil {
		runner = &execRunner{}
	}

	return &Manager{config: config, runner: runner, workDir: workDir}
}

// RenderConfig renders the exact config fixture for the configured inputs:
// dnsmasq binds only the bridge interface and address (bind-interfaces),
// stays off the wildcard resolver sockets (except-interface=lo), disables
// DHCP on the bridge explicitly (no-dhcp-interface), and forwards only to
// the pinned upstreams (no-resolv plus one server line per resolver). The
// config contains no directive that enables DHCP.
func (m *Manager) RenderConfig() ([]byte, error) {
	var b strings.Builder
	fmt.Fprintf(&b, "interface=%s\n", m.config.BridgeName)
	b.WriteString("bind-interfaces\n")
	fmt.Fprintf(&b, "listen-address=%s\n", m.config.ListenAddress)
	b.WriteString("except-interface=lo\n")
	fmt.Fprintf(&b, "no-dhcp-interface=%s\n", m.config.BridgeName)
	b.WriteString("domain-needed\n")
	b.WriteString("bogus-priv\n")
	b.WriteString("no-resolv\n")
	for _, server := range m.config.Upstream {
		fmt.Fprintf(&b, "server=%s\n", server)
	}

	return []byte(b.String()), nil
}

// Start writes the rendered config to the work directory as dnsmasq.conf and
// invokes the runner once as `dnsmasq --keep-in-foreground
// --conf-file=<path>`. A runner failure is returned (detectable with
// errors.Is) with the config left on disk for diagnosis.
func (m *Manager) Start(ctx context.Context) error {
	config, err := m.RenderConfig()
	if err != nil {
		return fmt.Errorf("dnsmasq: render config: %w", err)
	}

	path := filepath.Join(m.workDir, "dnsmasq.conf")
	if err := os.WriteFile(path, config, 0o644); err != nil {
		return fmt.Errorf("dnsmasq: write config %q: %w", path, err)
	}

	if err := m.runner.Start(ctx, "dnsmasq", []string{"--keep-in-foreground", "--conf-file=" + path}, os.Stdout, os.Stderr); err != nil {
		return fmt.Errorf("dnsmasq: start subprocess: %w", err)
	}
	m.started = true

	return nil
}

// Stop stops the subprocess and is idempotent: a second Stop, or a Stop
// before any Start, is a no-op.
func (m *Manager) Stop(ctx context.Context) error {
	if !m.started {
		return nil
	}
	m.started = false

	if err := m.runner.Stop(ctx); err != nil {
		return fmt.Errorf("dnsmasq: stop subprocess: %w", err)
	}

	return nil
}

// Restart stops the running subprocess, if any, and starts it again through
// the same start path, rewriting the config file.
func (m *Manager) Restart(ctx context.Context) error {
	if err := m.Stop(ctx); err != nil {
		return err
	}

	return m.Start(ctx)
}
