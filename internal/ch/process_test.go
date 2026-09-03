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

// Cloud-hypervisor subprocess manager contract (test-first).
//
// This suite pins the behavior of the Manager in this package, which owns one
// cloud-hypervisor process per VM (subprocess-per-Machine model, spec REQ-004
// reconcile step 7 and REQ-008 HYPERVISOR_CH_BINARY). It is exercised against
// a fake cloud-hypervisor binary (testdata/fake-cloud-hypervisor.sh, selected
// via WithBinaryPath) so no hypervisor is needed to run the suite. The fake
// records the argv it was spawned with, creates the API socket, and can be
// configured to exit early, delay the socket, ignore SIGTERM, or never create
// a socket.
//
// The contract, in prose:
//
//   - NewManager(opts ...ManagerOption) *Manager constructs a manager; the
//     functional options WithBinaryPath and WithSocketDir set the
//     cloud-hypervisor binary path and the socket directory respectively.
//   - Start(ctx) (socketPath string, err error) spawns the binary with the
//     argv entries "--api-socket" and "path=<sock>", where <sock> is
//     <socketDir>/api.sock, followed by "--seccomp false" (v48's filter kills
//     its own API thread with SIGSYS under container profiles), and returns
//     that socket path. The socket
//     directory is created if it does not exist, and an existing file at the
//     socket path — left behind by a previous unclean kill; a bind mount
//     persists it across provider restarts — is removed before the spawn,
//     since unix bind(2) on an existing path fails with AddrInUse even with
//     no listener. If the binary exits within
//     roughly half a second of spawning, Start returns an error that includes
//     the captured stderr and an empty socket path. Start is idempotent: a
//     second call returns the existing socket path without spawning again.
//     If the binary cannot be started at all (for example the path does not
//     exist), Start returns an error and an empty socket path.
//   - WaitReady(ctx, timeout) error polls the unix socket until it accepts a
//     connection, backing off between attempts. It returns an error when the
//     process has exited before the socket became reachable (that error
//     includes the captured stderr) and when the socket never becomes
//     reachable before the timeout expires (a context deadline error). It
//     returns an error, never a panic, when Start already failed.
//   - Stop(ctx) error terminates the child with SIGTERM, escalates to SIGKILL
//     if the process does not exit within a grace period, reaps the process,
//     and removes the socket directory. It is idempotent (a second Stop
//     returns nil), safe to call when Start was never called or already
//     failed, and leaves no process running.
//   - PID() int returns the child's process id; it is 0 before Start and
//     non-zero after Start while the process runs.
package ch_test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
)

// Compile-time pins: the manager and its functional options must exist with
// exactly these names and signatures.
var (
	_ *ch.Manager      = ch.NewManager()
	_ ch.ManagerOption = ch.WithBinaryPath("cloud-hypervisor")
	_ ch.ManagerOption = ch.WithSocketDir("/tmp")
	_ interface {
		Start(context.Context) (string, error)
		WaitReady(context.Context, time.Duration) error
		Stop(context.Context) error
		PID() int
	} = &ch.Manager{}
)

// TestManagerStartSpawnsWithAPISocketArg pins the spawn contract: the binary
// is invoked with "--api-socket path=<sock>", <sock> lives under the socket
// directory as api.sock, the directory is created, and the returned path
// matches. The fake records its argv so the exact argument shape is asserted.
func TestManagerStartSpawnsWithAPISocketArg(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("FAKE_CH_RECORD", record)

	socketDir := filepath.Join(t.TempDir(), "sockets")

	mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(socketDir))
	defer stopQuiet(t, mgr)

	socketPath, err := mgr.Start(t.Context())
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	wantSocket := filepath.Join(socketDir, "api.sock")
	if socketPath != wantSocket {
		t.Errorf("Start socket path = %q, want %q", socketPath, wantSocket)
	}

	if fi, err := os.Stat(socketDir); err != nil {
		t.Errorf("socket dir %q not created: %v", socketDir, err)
	} else if !fi.IsDir() {
		t.Errorf("socket dir path %q is not a directory", socketDir)
	}

	invocations := readRecord(t, record)
	if len(invocations) != 1 {
		t.Fatalf("fake binary spawned %d times, want 1", len(invocations))
	}

	wantArgs := []string{"--api-socket", "path=" + wantSocket, "--seccomp", "false"}
	if got := invocations[0].args; !slices.Equal(got, wantArgs) {
		t.Errorf("fake binary argv = %v, want %v", got, wantArgs)
	}

	waitForFile(t, wantSocket, 2*time.Second)

	if fi, err := os.Stat(wantSocket); err != nil {
		t.Errorf("socket %q not created: %v", wantSocket, err)
	} else if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("socket %q is not a unix socket (mode %v)", wantSocket, fi.Mode())
	}
}

// TestManagerStartPrematureExitCapturesStderr pins the crash-on-start path:
// a binary that exits immediately must make Start fail, return an empty
// socket path, and surface the captured stderr in the error. Stop afterwards
// must stay safe.
func TestManagerStartPrematureExitCapturesStderr(t *testing.T) {
	const wantStderr = "fake-cloud-hypervisor: simulated startup failure"

	t.Setenv("FAKE_CH_EXIT", "1")
	t.Setenv("FAKE_CH_EXIT_MSG", wantStderr)

	mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

	socketPath, err := mgr.Start(t.Context())
	if err == nil {
		t.Fatalf("Start succeeded (socket %q) for a binary that exits immediately, want error", socketPath)
	}

	if socketPath != "" {
		t.Errorf("Start returned socket path %q together with an error, want empty", socketPath)
	}

	if !strings.Contains(err.Error(), wantStderr) {
		t.Errorf("Start error %q does not include the captured stderr %q", err, wantStderr)
	}

	if err := mgr.Stop(t.Context()); err != nil {
		t.Errorf("Stop after a failed Start: %v", err)
	}
}

// TestManagerStartIsIdempotent pins that a second Start reuses the first
// process: it returns the same socket path and does not spawn the binary
// again (the fake records exactly one invocation).
func TestManagerStartIsIdempotent(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("FAKE_CH_RECORD", record)

	socketDir := filepath.Join(t.TempDir(), "sockets")

	mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(socketDir))
	defer stopQuiet(t, mgr)

	first, err := mgr.Start(t.Context())
	if err != nil {
		t.Fatalf("first Start: %v", err)
	}

	second, err := mgr.Start(t.Context())
	if err != nil {
		t.Fatalf("second Start: %v", err)
	}

	if second != first {
		t.Errorf("second Start socket path = %q, want the first %q", second, first)
	}

	if got := len(readRecord(t, record)); got != 1 {
		t.Errorf("fake binary spawned %d times after two Starts, want 1", got)
	}
}

// TestManagerStartUnlinksStaleSocketFile pins the stale-socket tolerance
// contract: a previous unclean kill can leave the socket pathname behind (a
// bind mount persists it across provider restarts), and the child's bind(2)
// on the existing path fails with AddrInUse. Start must remove the stale file
// before spawning, so a Start over a directory holding a dummy api.sock
// succeeds and ends with a live socket at that path.
func TestManagerStartUnlinksStaleSocketFile(t *testing.T) {
	record := filepath.Join(t.TempDir(), "args.log")
	t.Setenv("FAKE_CH_RECORD", record)

	socketDir := filepath.Join(t.TempDir(), "sockets")
	if err := os.MkdirAll(socketDir, 0o755); err != nil {
		t.Fatalf("create socket dir: %v", err)
	}

	stale := filepath.Join(socketDir, "api.sock")
	if err := os.WriteFile(stale, []byte("stale socket left by an unclean kill"), 0o644); err != nil {
		t.Fatalf("write stale socket file: %v", err)
	}

	mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(socketDir))
	defer stopQuiet(t, mgr)

	socketPath, err := mgr.Start(t.Context())
	if err != nil {
		t.Fatalf("Start with a stale socket file present: %v", err)
	}

	if socketPath != stale {
		t.Errorf("Start socket path = %q, want %q", socketPath, stale)
	}

	waitForFile(t, stale, 2*time.Second)

	fi, err := os.Stat(stale)
	if err != nil {
		t.Fatalf("socket %q not created: %v", stale, err)
	}

	if fi.Mode()&os.ModeSocket == 0 {
		t.Errorf("path %q is not a unix socket after Start (mode %v): the stale file was not replaced", stale, fi.Mode())
	}
}

// TestManagerWaitReady pins the readiness polling contract for all three
// outcomes: reachable socket, process exit before readiness, and timeout.
func TestManagerWaitReady(t *testing.T) {
	t.Run("socket appears after a delay", func(t *testing.T) {
		t.Setenv("FAKE_CH_SOCKET_DELAY", "1")

		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))
		defer stopQuiet(t, mgr)

		if _, err := mgr.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		if err := mgr.WaitReady(t.Context(), 5*time.Second); err != nil {
			t.Fatalf("WaitReady: %v", err)
		}
	})

	t.Run("process exits before the socket is ready", func(t *testing.T) {
		const wantStderr = "fake-cloud-hypervisor: crashed before binding"

		t.Setenv("FAKE_CH_EXIT", "1")
		t.Setenv("FAKE_CH_EXIT_MSG", wantStderr)
		t.Setenv("FAKE_CH_EXIT_DELAY", "1")

		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))
		defer stopQuiet(t, mgr)

		if _, err := mgr.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		err := mgr.WaitReady(t.Context(), 5*time.Second)
		if err == nil {
			t.Fatal("WaitReady succeeded although the process exited before binding the socket")
		}

		if !strings.Contains(err.Error(), wantStderr) {
			t.Errorf("WaitReady error %q does not include the captured stderr %q", err, wantStderr)
		}
	})

	t.Run("process already exited before WaitReady", func(t *testing.T) {
		t.Setenv("FAKE_CH_EXIT", "1")

		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))
		defer stopQuiet(t, mgr)

		if _, err := mgr.Start(t.Context()); err == nil {
			t.Fatal("Start succeeded although the binary exits immediately")
		}

		if err := mgr.WaitReady(t.Context(), 2*time.Second); err == nil {
			t.Fatal("WaitReady succeeded although Start already failed")
		}
	})

	t.Run("times out while the process stays up without a socket", func(t *testing.T) {
		t.Setenv("FAKE_CH_NO_SOCKET", "1")

		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))
		defer stopQuiet(t, mgr)

		if _, err := mgr.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		err := mgr.WaitReady(t.Context(), 300*time.Millisecond)
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("WaitReady error = %v, want context.DeadlineExceeded", err)
		}
	})
}

// TestManagerStop pins the shutdown contract: SIGTERM is used and recorded by
// the fake, SIGKILL escalation terminates a process that ignores SIGTERM,
// repeated Stops return nil, and Stop without a Start returns nil.
func TestManagerStop(t *testing.T) {
	t.Run("SIGTERM stops the process", func(t *testing.T) {
		signalFile := filepath.Join(t.TempDir(), "signals.log")
		t.Setenv("FAKE_CH_SIGNAL_FILE", signalFile)
		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

		if _, err := mgr.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		pid := mgr.PID()
		if pid <= 0 {
			t.Fatalf("PID = %d after Start, want > 0", pid)
		}

		if err := mgr.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		assertProcessGone(t, pid)

		if got, err := os.ReadFile(signalFile); err != nil || !strings.Contains(string(got), "SIGTERM") {
			t.Errorf("SIGTERM marker file %s = %q (read err %v), want it to record SIGTERM", signalFile, got, err)
		}
	})

	t.Run("escalates to SIGKILL when SIGTERM is ignored", func(t *testing.T) {
		signalFile := filepath.Join(t.TempDir(), "signals.log")
		t.Setenv("FAKE_CH_SIGNAL_FILE", signalFile)
		t.Setenv("FAKE_CH_IGNORE_TERM", "1")
		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

		if _, err := mgr.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		pid := mgr.PID()
		if err := mgr.Stop(t.Context()); err != nil {
			t.Fatalf("Stop: %v", err)
		}

		assertProcessGone(t, pid)

		if got, err := os.ReadFile(signalFile); err == nil && strings.Contains(string(got), "SIGTERM") {
			t.Errorf("process recorded SIGTERM although it was configured to ignore it: %q", got)
		}
	})

	t.Run("second Stop returns nil", func(t *testing.T) {
		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

		if _, err := mgr.Start(t.Context()); err != nil {
			t.Fatalf("Start: %v", err)
		}

		if err := mgr.Stop(t.Context()); err != nil {
			t.Fatalf("first Stop: %v", err)
		}

		if err := mgr.Stop(t.Context()); err != nil {
			t.Errorf("second Stop: %v", err)
		}
	})

	t.Run("Stop without Start returns nil", func(t *testing.T) {
		mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

		if err := mgr.Stop(t.Context()); err != nil {
			t.Errorf("Stop on a never-started manager: %v", err)
		}
	})
}

// TestManagerPID pins the process id reporting: zero before Start, non-zero
// and live after Start.
func TestManagerPID(t *testing.T) {
	mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

	if got := mgr.PID(); got != 0 {
		t.Errorf("PID before Start = %d, want 0", got)
	}

	if _, err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	pid := mgr.PID()
	if pid <= 0 {
		t.Fatalf("PID after Start = %d, want > 0", pid)
	}

	if err := syscall.Kill(pid, syscall.Signal(0)); err != nil {
		t.Errorf("PID %d does not refer to a live process: %v", pid, err)
	}

	if err := mgr.Stop(t.Context()); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// TestManagerStopRemovesSocketDir pins the cleanup contract: Stop removes the
// socket directory it created.
func TestManagerStopRemovesSocketDir(t *testing.T) {
	socketDir := filepath.Join(t.TempDir(), "sockets")
	mgr := ch.NewManager(ch.WithBinaryPath(fakePath(t)), ch.WithSocketDir(socketDir))

	if _, err := mgr.Start(t.Context()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	socketPath := filepath.Join(socketDir, "api.sock")
	if _, err := os.Stat(socketPath); err != nil {
		t.Fatalf("socket %q not created: %v", socketPath, err)
	}

	if err := mgr.Stop(t.Context()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if _, err := os.Stat(socketDir); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("socket dir %q still exists after Stop (stat err = %v), want os.ErrNotExist", socketDir, err)
	}
}

// TestManagerStartWithMissingBinary pins the failure mode for a binary that
// cannot be executed at all: Start returns an error with an empty socket
// path, and Stop afterwards stays safe.
func TestManagerStartWithMissingBinary(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-cloud-hypervisor")
	mgr := ch.NewManager(ch.WithBinaryPath(missing), ch.WithSocketDir(filepath.Join(t.TempDir(), "sockets")))

	socketPath, err := mgr.Start(t.Context())
	if err == nil {
		t.Fatalf("Start with a missing binary succeeded (socket %q), want error", socketPath)
	}

	if socketPath != "" {
		t.Errorf("Start returned socket path %q together with an error, want empty", socketPath)
	}

	if err := mgr.Stop(t.Context()); err != nil {
		t.Errorf("Stop after a failed Start: %v", err)
	}
}

// fakePath returns the committed fake cloud-hypervisor script, made
// executable. The test working directory is this package's directory, so the
// relative testdata path is correct.
func fakePath(t *testing.T) string {
	t.Helper()

	path, err := filepath.Abs(filepath.Join("testdata", "fake-cloud-hypervisor.sh"))
	if err != nil {
		t.Fatalf("resolve fake binary path: %v", err)
	}

	if err := os.Chmod(path, 0o755); err != nil {
		t.Fatalf("make fake binary executable: %v", err)
	}

	return path
}

// fakeInvocation is one recorded spawn of the fake binary.
type fakeInvocation struct {
	args []string
}

// readRecord parses the fake binary's invocation log: FAKE_INVOCATION header
// lines separate one spawn from the next, with each argv entry on its own
// line. The returned slice preserves spawn order.
func readRecord(t *testing.T, path string) []fakeInvocation {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fake invocation record %s: %v", path, err)
	}

	var invocations []fakeInvocation

	for block := range strings.SplitSeq(string(data), "FAKE_INVOCATION\n") {
		block = strings.TrimSpace(block)
		if block == "" {
			continue
		}

		invocations = append(invocations, fakeInvocation{args: strings.Split(block, "\n")})
	}

	return invocations
}

// waitForFile polls until path exists or the timeout expires.
func waitForFile(t *testing.T, path string, timeout time.Duration) {
	t.Helper()

	deadline := time.Now().Add(timeout)

	for {
		if _, err := os.Stat(path); err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("file %s never appeared within %v", path, timeout)
		}

		time.Sleep(20 * time.Millisecond)
	}
}

// assertProcessGone fails the test unless pid no longer refers to a process
// (the process was reaped, so kill(0) reports ESRCH).
func assertProcessGone(t *testing.T, pid int) {
	t.Helper()

	if err := syscall.Kill(pid, syscall.Signal(0)); !errors.Is(err, syscall.ESRCH) {
		t.Errorf("process %d still alive after Stop (kill(0) err = %v), want ESRCH", pid, err)
	}
}

// stopQuiet stops the manager during test teardown, tolerating an error so
// cleanup never masks the assertion that failed first.
func stopQuiet(t *testing.T, mgr *ch.Manager) {
	t.Helper()

	if err := mgr.Stop(t.Context()); err != nil {
		t.Logf("cleanup Stop: %v", err)
	}
}
