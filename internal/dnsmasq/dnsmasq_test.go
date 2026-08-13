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

// The dnsmasq DNS-forwarder manager contract (test-first).
//
// This suite pins the provider's DNS integration: dnsmasq runs as a
// subprocess of the provider on the host network and forwards upstream DNS
// to 1.1.1.1 and 8.8.8.8 while serving the lab's bridge address
// 192.168.124.1 as the DNS server for cluster VMs. Machines use static IPs,
// so dnsmasq never runs a DHCP server: the rendered config contains no
// directive that enables DHCP.
//
// The contract, in prose:
//
//   - Config carries the cluster-network inputs the rendered config depends
//     on: the bridge interface to serve, the address to listen on, and the
//     ordered upstream resolvers. The shape mirrors the lab's
//     dnsmasq-k8sbr0.conf.
//   - Runner is the subprocess-lifecycle seam: Start spawns the program
//     named name with the given arguments, attaching the given writers for
//     its stdout and stderr, and returns once launched (dnsmasq runs in the
//     foreground, so the launch returns while the process keeps running);
//     Stop terminates a previously started process. Production uses the
//     host dnsmasq binary via os/exec; tests inject a recording fake so no
//     external process is ever started.
//   - NewManager(config Config, runner Runner, workDir string) *Manager
//     builds a manager for one cluster network. The manager writes its
//     rendered config into the work directory and runs dnsmasq against that
//     file.
//   - RenderConfig() ([]byte, error) renders the exact config fixture for
//     the configured inputs.
//   - Start(ctx context.Context) error writes the rendered config to the
//     work directory and invokes the runner once as
//     `dnsmasq --keep-in-foreground --conf-file=<path>`. A runner failure
//     (for example dnsmasq exiting because port 53 is already bound) is
//     returned, detectable with errors.Is against the runner's error.
//   - Stop(ctx context.Context) error stops the subprocess and is
//     idempotent: a second Stop, or a Stop before any Start, is a no-op.
//   - Restart(ctx context.Context) error stops the running subprocess, if
//     any, and starts it again through the same start path, rewriting the
//     config file.
package dnsmasq_test

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/dnsmasq"
)

// Compile-time pins: the Config struct, the constructor, the renderer, and
// the three lifecycle operations must exist with exactly these names,
// types, and signatures, and the fake below must satisfy the runner seam.
var (
	_ dnsmasq.Config                                                = dnsmasq.Config{BridgeName: "", ListenAddress: "", Upstream: nil}
	_ func(dnsmasq.Config, dnsmasq.Runner, string) *dnsmasq.Manager = dnsmasq.NewManager
	_ func(*dnsmasq.Manager) ([]byte, error)                        = (*dnsmasq.Manager).RenderConfig
	_ func(*dnsmasq.Manager, context.Context) error                 = (*dnsmasq.Manager).Start
	_ func(*dnsmasq.Manager, context.Context) error                 = (*dnsmasq.Manager).Stop
	_ func(*dnsmasq.Manager, context.Context) error                 = (*dnsmasq.Manager).Restart
	_ dnsmasq.Runner                                                = (*recordingRunner)(nil)
)

// startCall is one captured Start invocation: the program name, the exact
// argument list, and the writers the manager attached for the process
// output.
type startCall struct {
	name   string
	args   []string
	stdout io.Writer
	stderr io.Writer
}

// recordingRunner records every lifecycle call in order and returns canned
// errors: startErr is returned from every Start, stopErr from every Stop.
type recordingRunner struct {
	startErr  error
	stopErr   error
	calls     []string // "start" or "stop", in invocation order
	starts    []startCall
	stopCount int
}

// Start implements dnsmasq.Runner. The context is accepted and deliberately
// ignored: cancellation propagation is an implementation concern of the
// default exec runner, not part of this contract.
func (r *recordingRunner) Start(_ context.Context, name string, args []string, stdout, stderr io.Writer) error {
	r.calls = append(r.calls, "start")
	r.starts = append(r.starts, startCall{name: name, args: append([]string(nil), args...), stdout: stdout, stderr: stderr})

	return r.startErr
}

// Stop implements dnsmasq.Runner.
func (r *recordingRunner) Stop(context.Context) error {
	r.calls = append(r.calls, "stop")
	r.stopCount++

	return r.stopErr
}

// defaultConfig is the cluster network the fixture is rendered for: the
// k8sbr0 bridge serving 192.168.124.1 with the lab's two upstreams.
func defaultConfig() dnsmasq.Config {
	return dnsmasq.Config{
		BridgeName:    "k8sbr0",
		ListenAddress: "192.168.124.1",
		Upstream:      []string{"1.1.1.1", "8.8.8.8"},
	}
}

// wantConfig is the exact rendered config for the default inputs, mirroring
// the lab's dnsmasq-k8sbr0.conf shape: dnsmasq binds only the bridge
// interface and address (bind-interfaces fails fast if the address is
// missing — the provider owns the bridge and creates it before starting
// dnsmasq), stays off the wildcard resolver sockets, disables DHCP on the
// bridge explicitly, and forwards only to the pinned upstreams (no-resolv).
// The config contains no directive that enables a DHCP server: Machines use
// static IPs.
const wantConfig = `interface=k8sbr0
bind-interfaces
listen-address=192.168.124.1
except-interface=lo
no-dhcp-interface=k8sbr0
domain-needed
bogus-priv
no-resolv
server=1.1.1.1
server=8.8.8.8
`

// wantStartArgs is the exact argument list Start must pass to the runner:
// dnsmasq stays in the foreground (the manager owns the process) and reads
// its config from the file the manager wrote — the dnsmasq.conf file inside
// the work directory.
func wantStartArgs(configPath string) []string {
	return []string{"--keep-in-foreground", "--conf-file=" + configPath}
}

// configPath is the config file Start must write inside the work directory.
func configPath(workDir string) string {
	return filepath.Join(workDir, "dnsmasq.conf")
}

// newManager builds a manager over the given runner and work directory with
// the default config.
func newManager(t *testing.T, runner dnsmasq.Runner, workDir string) *dnsmasq.Manager {
	t.Helper()

	return dnsmasq.NewManager(defaultConfig(), runner, workDir)
}

// TestRenderConfigExactFixture pins the rendered config: byte-for-byte
// equal to the fixture for the default inputs — the listen-address, the
// upstream server directives, the interface/bind/no-resolv lines, and no
// DHCP. The absence of DHCP is pinned twice: no dhcp-range directive
// anywhere (the directive that would activate a DHCP server) and no line at
// all beginning with the dhcp- prefix (the whole DHCP-enabling directive
// family). The no-dhcp-interface line is the opposite — it disables DHCP on
// the bridge — so it is the only dhcp-related line allowed.
func TestRenderConfigExactFixture(t *testing.T) {
	m := newManager(t, &recordingRunner{}, t.TempDir())

	got, err := m.RenderConfig()
	if err != nil {
		t.Fatalf("RenderConfig error: %v", err)
	}

	if string(got) != wantConfig {
		t.Errorf("RenderConfig output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, wantConfig)
	}
	if strings.Contains(string(got), "dhcp-range") {
		t.Errorf("RenderConfig enables DHCP (dhcp-range found):\n%s", got)
	}
	if regexp.MustCompile(`(?m)^dhcp-`).Match(got) {
		t.Errorf("RenderConfig contains a DHCP-enabling directive:\n%s", got)
	}
}

// TestRenderConfigEmptyUpstream pins the renderer's handling of no upstream
// resolvers: the config renders without any server directives (with
// no-resolv dnsmasq reads no resolv.conf either, so the forwarder simply
// has nothing to forward to) while every other line stays identical.
func TestRenderConfigEmptyUpstream(t *testing.T) {
	cfg := defaultConfig()
	cfg.Upstream = nil
	m := dnsmasq.NewManager(cfg, &recordingRunner{}, t.TempDir())

	got, err := m.RenderConfig()
	if err != nil {
		t.Fatalf("RenderConfig error: %v", err)
	}

	want := `interface=k8sbr0
bind-interfaces
listen-address=192.168.124.1
except-interface=lo
no-dhcp-interface=k8sbr0
domain-needed
bogus-priv
no-resolv
`
	if string(got) != want {
		t.Errorf("RenderConfig output mismatch\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// TestStartWritesConfigAndInvokesRunner pins the start path: Start writes
// the rendered config to the work directory as dnsmasq.conf and invokes the
// runner exactly once as `dnsmasq --keep-in-foreground --conf-file=<path>`,
// attaching non-nil writers for the process output.
func TestStartWritesConfigAndInvokesRunner(t *testing.T) {
	workDir := t.TempDir()
	runner := &recordingRunner{}
	m := newManager(t, runner, workDir)

	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start error: %v", err)
	}

	got, err := os.ReadFile(configPath(workDir))
	if err != nil {
		t.Fatalf("Start did not write the config file: %v", err)
	}
	if string(got) != wantConfig {
		t.Errorf("written config mismatch\n--- got ---\n%s\n--- want ---\n%s", got, wantConfig)
	}

	if len(runner.starts) != 1 {
		t.Fatalf("Start invoked the runner %d times, want 1", len(runner.starts))
	}
	gotCall := runner.starts[0]
	if gotCall.name != "dnsmasq" {
		t.Errorf("Start ran %q, want %q", gotCall.name, "dnsmasq")
	}
	if !reflect.DeepEqual(gotCall.args, wantStartArgs(configPath(workDir))) {
		t.Errorf("Start args = %v, want %v", gotCall.args, wantStartArgs(configPath(workDir)))
	}
	if gotCall.stdout == nil || gotCall.stderr == nil {
		t.Errorf("Start attached nil output writers, want non-nil stdout and stderr")
	}
}

// TestStartPropagatesFailure pins the start failure contract: a runner
// error surfaces from Start, detectable with errors.Is against the runner's
// error, the runner is invoked exactly once, and the config file is written
// before the subprocess is spawned — a failed start still leaves the
// rendered config on disk for diagnosis.
func TestStartPropagatesFailure(t *testing.T) {
	workDir := t.TempDir()
	boom := errors.New("dnsmasq: failed to bind socket: address already in use")
	runner := &recordingRunner{startErr: boom}
	m := newManager(t, runner, workDir)

	err := m.Start(t.Context())
	if err == nil {
		t.Fatal("Start succeeded, want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Start error %v does not wrap the runner error %v", err, boom)
	}
	if len(runner.starts) != 1 {
		t.Errorf("Start invoked the runner %d times on failure, want 1", len(runner.starts))
	}
	if _, statErr := os.Stat(configPath(workDir)); statErr != nil {
		t.Errorf("config file not written before the failed start: %v", statErr)
	}
}

// TestStopIdempotent pins the stop path: after a successful start, the
// first Stop invokes the runner once and the second Stop is a no-op — the
// manager tracks its subprocess so a repeated stop does not signal a dead
// process twice.
func TestStopIdempotent(t *testing.T) {
	runner := &recordingRunner{}
	m := newManager(t, runner, t.TempDir())

	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := m.Stop(t.Context()); err != nil {
		t.Fatalf("Stop error: %v", err)
	}
	if err := m.Stop(t.Context()); err != nil {
		t.Fatalf("second Stop error: %v", err)
	}

	if runner.stopCount != 1 {
		t.Errorf("Stop invoked the runner %d times, want 1", runner.stopCount)
	}
	if !reflect.DeepEqual(runner.calls, []string{"start", "stop"}) {
		t.Errorf("lifecycle calls = %v, want [start stop]", runner.calls)
	}
}

// TestStopBeforeStartIsNoop pins the stop edge: stopping a manager that
// never started is a no-op — no error and no runner invocation.
func TestStopBeforeStartIsNoop(t *testing.T) {
	runner := &recordingRunner{}
	m := newManager(t, runner, t.TempDir())

	if err := m.Stop(t.Context()); err != nil {
		t.Fatalf("Stop before Start error: %v", err)
	}
	if runner.stopCount != 0 {
		t.Errorf("Stop before Start invoked the runner %d times, want 0", runner.stopCount)
	}
}

// TestRestartStopsThenStarts pins the restart path: Restart stops the
// running subprocess and starts it again through the same start path —
// rewriting the config file — in that order.
func TestRestartStopsThenStarts(t *testing.T) {
	workDir := t.TempDir()
	runner := &recordingRunner{}
	m := newManager(t, runner, workDir)

	if err := m.Start(t.Context()); err != nil {
		t.Fatalf("Start error: %v", err)
	}
	if err := m.Restart(t.Context()); err != nil {
		t.Fatalf("Restart error: %v", err)
	}

	if !reflect.DeepEqual(runner.calls, []string{"start", "stop", "start"}) {
		t.Errorf("lifecycle calls = %v, want [start stop start]", runner.calls)
	}
	if len(runner.starts) != 2 {
		t.Errorf("Restart started the subprocess %d times in total, want 2", len(runner.starts))
	}
	if _, statErr := os.Stat(configPath(workDir)); statErr != nil {
		t.Errorf("config file missing after restart: %v", statErr)
	}
}

// TestRestartBeforeStartStartsOnly pins the restart edge: Restart on a
// manager that never started just starts the subprocess — the stop half is
// a no-op.
func TestRestartBeforeStartStartsOnly(t *testing.T) {
	runner := &recordingRunner{}
	m := newManager(t, runner, t.TempDir())

	if err := m.Restart(t.Context()); err != nil {
		t.Fatalf("Restart error: %v", err)
	}
	if !reflect.DeepEqual(runner.calls, []string{"start"}) {
		t.Errorf("lifecycle calls = %v, want [start]", runner.calls)
	}
}
