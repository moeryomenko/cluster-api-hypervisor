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

// Package ch implements the cloud-hypervisor subprocess-per-Machine model:
// each Machine owns exactly one cloud-hypervisor process spawned against a
// unix API socket. A Manager owns exactly one such process, spawns it, waits
// for the socket to accept connections, and tears the process and its socket
// directory down on Stop.
package ch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// ManagerOption configures a Manager. Options are applied in order during
// NewManager; later options override earlier ones.
type ManagerOption func(*Manager)

// Manager owns one cloud-hypervisor subprocess and its API socket. It is safe
// for concurrent use; the lifecycle methods are idempotent and the zero value
// (before any Start) behaves as a stopped manager.
type Manager struct {
	mu sync.Mutex

	// binaryPath is the cloud-hypervisor executable. When empty, Start
	// resolves "cloud-hypervisor" through exec.LookPath.
	binaryPath string
	// socketDir is the directory that holds the API socket. When empty,
	// Start auto-creates a "ch-capi-*" directory under os.TempDir.
	socketDir string
	// netConfig is the cloud-hypervisor --net device string (for example the
	// vhost-user config rendered by internal/chclient) spawned with the
	// process. An empty value spawns without a net device.
	netConfig string

	cmd    *exec.Cmd
	done   chan struct{}
	pid    int
	stderr *lockedBuffer
	// exitErr is the error from cmd.Wait once the process has exited; it is
	// nil while the process runs and for a clean exit.
	exitErr error

	started          bool
	stopped          bool
	socketDirCreated bool
}

// lockedBuffer is an io.Writer that records writes in memory and is safe for
// concurrent reads and writes. It accumulates the child's stderr so error
// paths can surface what the process reported.
type lockedBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

// Write appends p to the buffer.
func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.Write(p)
}

// String returns the accumulated contents.
func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.b.String()
}

const (
	// startupWindow is how long Start watches a freshly spawned process
	// before declaring the spawn successful. A child that exits within this
	// window is treated as a crash-on-start.
	startupWindow = 500 * time.Millisecond
	// stopGracePeriod is how long Stop waits for SIGTERM to take effect
	// before escalating to SIGKILL.
	stopGracePeriod = 5 * time.Second
	// readyMinDelay and readyMaxDelay bound the exponential backoff used by
	// WaitReady while the API socket is not yet reachable.
	readyMinDelay = 50 * time.Millisecond
	readyMaxDelay = 500 * time.Millisecond
)

// NewManager constructs a Manager. The default binary is "cloud-hypervisor"
// resolved through exec.LookPath at Start time; the default socket directory
// is auto-created under os.TempDir with a "ch-capi-*" prefix, also at Start
// time. WithBinaryPath and WithSocketDir override these defaults.
func NewManager(opts ...ManagerOption) *Manager {
	m := &Manager{stderr: new(lockedBuffer)}
	for _, opt := range opts {
		opt(m)
	}
	return m
}

// WithBinaryPath sets the cloud-hypervisor binary to spawn. The path is used
// verbatim; relative paths resolve against the caller's working directory.
func WithBinaryPath(path string) ManagerOption {
	return func(m *Manager) { m.binaryPath = path }
}

// WithSocketDir sets the directory that holds the API socket. Start creates
// the directory if it does not exist and Stop removes it.
func WithSocketDir(dir string) ManagerOption {
	return func(m *Manager) { m.socketDir = dir }
}

// WithNetConfig sets the cloud-hypervisor --net device string spawned with
// the process. An empty value (the default) spawns without a net device.
func WithNetConfig(netConfig string) ManagerOption {
	return func(m *Manager) { m.netConfig = netConfig }
}

// SetNetConfig sets the --net device string used at the next process spawn.
// Call it before Start; after Start it only affects a subsequent process
// lifetime, because Start is idempotent and never re-spawns a running
// process.
func (m *Manager) SetNetConfig(netConfig string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.netConfig = netConfig
}

// Start spawns the cloud-hypervisor process with the two argv entries
// "--api-socket" and "path=<sock>", where <sock> is <socketDir>/api.sock,
// plus "--net <netConfig>" when a net device was configured through
// WithNetConfig or SetNetConfig, and returns the socket path. The socket
// directory is created if it does not exist. If the process exits within
// roughly half a second of spawning, Start returns an error that includes
// the captured stderr and an empty socket path. Start is idempotent: a
// second call returns the existing socket path without spawning again. The
// context controls startup only: cancelling it kills the process and cleans
// up.
func (m *Manager) Start(ctx context.Context) (string, error) {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return "", errors.New("process manager: Start called after Stop")
	}
	if m.started {
		socket := m.socketPath()
		m.mu.Unlock()
		return socket, nil
	}
	m.mu.Unlock()

	binary, err := m.resolveBinary()
	if err != nil {
		return "", err
	}

	m.mu.Lock()
	dir, err := m.ensureSocketDir()
	if err != nil {
		m.mu.Unlock()
		return "", err
	}
	m.socketDir = dir
	socket := m.socketPath()
	m.stderr = new(lockedBuffer)
	args := []string{"--api-socket", "path=" + socket}
	if m.netConfig != "" {
		args = append(args, "--net", m.netConfig)
	}
	cmd := exec.Command(binary, args...)
	cmd.Stderr = m.stderr
	if err := cmd.Start(); err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("start cloud-hypervisor binary %q: %w", binary, err)
	}
	m.cmd = cmd
	m.pid = cmd.Process.Pid
	m.done = make(chan struct{})
	m.exitErr = nil
	go m.wait()
	m.mu.Unlock()

	timer := time.NewTimer(startupWindow)
	defer timer.Stop()
	select {
	case <-m.done:
		return "", m.processExitedError()
	case <-ctx.Done():
		m.killProcess()
		<-m.done
		m.mu.Lock()
		m.stopped = true
		m.mu.Unlock()
		m.removeSocketDir()
		return "", ctx.Err()
	case <-timer.C:
		m.mu.Lock()
		m.started = true
		socket := m.socketPath()
		m.mu.Unlock()
		return socket, nil
	}
}

// WaitReady polls the API socket until it accepts a connection, backing off
// exponentially between attempts. It returns an error including the captured
// stderr when the process exited before the socket became reachable, the
// context error when the caller cancels, and a context deadline error when
// the socket never becomes reachable before the timeout expires. It returns
// an error, never a panic, when Start already failed.
func (m *Manager) WaitReady(ctx context.Context, timeout time.Duration) error {
	m.mu.Lock()
	if !m.started {
		m.mu.Unlock()
		return errors.New("process manager: WaitReady requires a successful Start")
	}
	done := m.done
	socket := m.socketPath()
	m.mu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	delay := readyMinDelay
	for {
		conn, err := net.DialTimeout("unix", socket, readyMinDelay)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-done:
			return m.processExitedError()
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > readyMaxDelay {
			delay = readyMaxDelay
		}
	}
}

// Stop terminates the child with SIGTERM, escalates to SIGKILL if the process
// does not exit within a grace period, reaps the process, and removes the
// socket directory. It is idempotent, safe to call when Start was never
// called or already failed, and leaves no process running.
func (m *Manager) Stop(_ context.Context) error {
	m.mu.Lock()
	if m.stopped {
		m.mu.Unlock()
		return nil
	}
	cmd := m.cmd
	done := m.done
	m.mu.Unlock()

	if cmd != nil && cmd.Process != nil {
		select {
		case <-done:
			// The process already exited; nothing to signal.
		default:
			_ = cmd.Process.Signal(syscall.SIGTERM)
			select {
			case <-done:
			case <-time.After(stopGracePeriod):
				_ = cmd.Process.Kill()
				<-done
			}
		}
	}

	m.removeSocketDir()

	m.mu.Lock()
	m.stopped = true
	m.mu.Unlock()
	return nil
}

// PID returns the child's process id. It is 0 before Start and non-zero after
// Start while the process runs.
func (m *Manager) PID() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pid
}

// socketPath returns the API socket path for the configured socket directory.
// The caller must hold m.mu.
func (m *Manager) socketPath() string {
	if m.socketDir == "" {
		return ""
	}
	return filepath.Join(m.socketDir, "api.sock")
}

// resolveBinary returns the binary to spawn: the configured path when one was
// given, otherwise "cloud-hypervisor" resolved through exec.LookPath.
func (m *Manager) resolveBinary() (string, error) {
	m.mu.Lock()
	path := m.binaryPath
	m.mu.Unlock()
	if path != "" {
		return path, nil
	}
	resolved, err := exec.LookPath("cloud-hypervisor")
	if err != nil {
		return "", fmt.Errorf("resolve cloud-hypervisor binary from PATH: %w", err)
	}
	return resolved, nil
}

// ensureSocketDir creates the socket directory if it does not exist and
// records that the manager owns it. When no directory was configured, it
// auto-creates a "ch-capi-*" directory under os.TempDir. The caller must hold
// m.mu.
func (m *Manager) ensureSocketDir() (string, error) {
	if m.socketDir != "" {
		if err := os.MkdirAll(m.socketDir, 0o755); err != nil {
			return "", fmt.Errorf("create socket directory %q: %w", m.socketDir, err)
		}
		m.socketDirCreated = true
		return m.socketDir, nil
	}
	dir, err := os.MkdirTemp(os.TempDir(), "ch-capi-*")
	if err != nil {
		return "", fmt.Errorf("create default socket directory: %w", err)
	}
	m.socketDirCreated = true
	return dir, nil
}

// wait reaps the child and records the exit. It closes m.done so that Start,
// WaitReady, and Stop observe the exit; the channel close happens after the
// exit error and the final stderr are visible.
func (m *Manager) wait() {
	err := m.cmd.Wait()
	m.mu.Lock()
	m.exitErr = err
	close(m.done)
	m.mu.Unlock()
}

// killProcess sends SIGKILL to the child. It is used when a Start is cancelled
// during the startup window; a missing or already-dead process is ignored.
func (m *Manager) killProcess() {
	m.mu.Lock()
	cmd := m.cmd
	m.mu.Unlock()
	if cmd != nil && cmd.Process != nil {
		_ = cmd.Process.Kill()
	}
}

// removeSocketDir removes the socket directory the manager created, if any.
// It is a no-op for a manager whose Start never ran.
func (m *Manager) removeSocketDir() {
	m.mu.Lock()
	dir := m.socketDir
	created := m.socketDirCreated
	m.mu.Unlock()
	if created && dir != "" {
		_ = os.RemoveAll(dir)
	}
}

// processExitedError builds the error reported when the child exited before
// the API socket became ready. It includes the exit status and the captured
// stderr. It must only be called after m.done is closed, so the exit error
// and stderr are final.
func (m *Manager) processExitedError() error {
	msg := "cloud-hypervisor exited before becoming ready"
	if m.exitErr != nil {
		msg += fmt.Sprintf(": %v", m.exitErr)
	}
	if stderr := strings.TrimSpace(m.stderr.String()); stderr != "" {
		msg += fmt.Sprintf(": %s", stderr)
	}
	return errors.New(msg)
}
