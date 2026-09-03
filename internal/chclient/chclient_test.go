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

// VM control interface and programmable fake (test-first).
//
// This suite pins the contract of package chclient, the thin VM-control layer
// the Machine controller uses to drive one cloud-hypervisor VM: spawn the
// subprocess, boot the VM over the API socket, report its state, shut it
// down gracefully, and tear the process down. The layer wraps the process
// Manager and the HTTP Client from internal/ch.
//
// The contract, in prose:
//
//   - Client is an interface with four operations:
//     EnsureRunning(ctx) error spawns the cloud-hypervisor process when it
//     is not running and boots the VM through the API; it is a no-op when
//     the VM is already running.
//     Shutdown(ctx) error asks the VM to shut down gracefully through the
//     API.
//     Stop(ctx) error tears everything down: it stops the VM and the
//     cloud-hypervisor process.
//     Info(ctx) (ch.VMState, error) reports the current VM state. When the
//     VM is absent it returns ErrNotFound.
//   - ErrNotFound is the exported sentinel Info returns when the VM does not
//     exist. Callers detect an absent VM with errors.Is(err, ErrNotFound).
//   - FakeClient implements Client for controller tests. Its behaviour is
//     fully programmable through exported fields and every call is appended
//     to Calls:
//     State holds the VM state Info reports (for example "Running" or
//     "Shutdown"); InfoErr, EnsureRunningErr, ShutdownErr and StopErr hold
//     the error each operation returns when set; when an error field is nil
//     the operation succeeds. Calls records each invoked operation by name
//     in call order: "EnsureRunning", "Shutdown", "Stop", "Info".
//     The zero value is usable: every operation succeeds and no state is
//     reported.
//   - The fake returns a configured error unchanged, never wrapped, so tests
//     can compare it with ==; the absent-VM detection contract is errors.Is
//     against ErrNotFound.
package chclient_test

import (
	"errors"
	"slices"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
)

// The fake satisfies the interface; this is a compile-time assertion.
var _ chclient.Client = (*chclient.FakeClient)(nil)

// TestFakeClientImplementsClient proves the fake works through an interface
// value, which is how the Machine controller consumes it.
func TestFakeClientImplementsClient(t *testing.T) {
	var client chclient.Client = &chclient.FakeClient{State: ch.VMState("Running")}

	state, err := client.Info(t.Context())
	if err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}

	if state != ch.VMState("Running") {
		t.Fatalf("Info() state = %q, want %q", state, ch.VMState("Running"))
	}
}

// TestFakeEnsureRunning pins the default behaviour: success and a recorded
// call.
func TestFakeEnsureRunning(t *testing.T) {
	fake := new(chclient.FakeClient)

	if err := fake.EnsureRunning(t.Context()); err != nil {
		t.Fatalf("EnsureRunning() error = %v, want nil", err)
	}

	wantCalls := []string{"EnsureRunning"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Fatalf("Calls = %v, want %v", fake.Calls, wantCalls)
	}
}

// TestFakeShutdown pins the default behaviour: success and a recorded call.
func TestFakeShutdown(t *testing.T) {
	fake := new(chclient.FakeClient)

	if err := fake.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	wantCalls := []string{"Shutdown"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Fatalf("Calls = %v, want %v", fake.Calls, wantCalls)
	}
}

// TestFakeStop pins the default behaviour: success and a recorded call.
func TestFakeStop(t *testing.T) {
	fake := new(chclient.FakeClient)

	if err := fake.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}

	wantCalls := []string{"Stop"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Fatalf("Calls = %v, want %v", fake.Calls, wantCalls)
	}
}

// TestFakeInfo pins that Info reports the programmed state verbatim.
func TestFakeInfo(t *testing.T) {
	tests := []struct {
		name string
		give ch.VMState
		want ch.VMState
	}{
		{name: "running", give: ch.VMState("Running"), want: ch.VMState("Running")},
		{name: "stopped", give: ch.VMState("Shutdown"), want: ch.VMState("Shutdown")},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fake := &chclient.FakeClient{State: tt.give}

			state, err := fake.Info(t.Context())
			if err != nil {
				t.Fatalf("Info() error = %v, want nil", err)
			}

			if state != tt.want {
				t.Errorf("Info() state = %q, want %q", state, tt.want)
			}

			wantCalls := []string{"Info"}
			if !slices.Equal(fake.Calls, wantCalls) {
				t.Errorf("Calls = %v, want %v", fake.Calls, wantCalls)
			}
		})
	}
}

// TestFakeInfoNotFound pins the absent-VM contract: Info returns the exported
// ErrNotFound sentinel, detectable with errors.Is, and records the call.
func TestFakeInfoNotFound(t *testing.T) {
	fake := &chclient.FakeClient{InfoErr: chclient.ErrNotFound}

	state, err := fake.Info(t.Context())
	if !errors.Is(err, chclient.ErrNotFound) {
		t.Fatalf("Info() error = %v, want errors.Is(err, ErrNotFound)", err)
	}

	if state != "" {
		t.Errorf("Info() state = %q, want empty on error", state)
	}

	wantCalls := []string{"Info"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Errorf("Calls = %v, want %v", fake.Calls, wantCalls)
	}
}

// TestFakeProgrammableErrors pins that each operation returns its configured
// error unchanged (same error value) and still records the call.
func TestFakeProgrammableErrors(t *testing.T) {
	ensureRunningErr := errors.New("ensure running failed")
	shutdownErr := errors.New("shutdown failed")
	stopErr := errors.New("stop failed")
	infoErr := errors.New("info failed")

	tests := []struct {
		name    string
		fake    *chclient.FakeClient
		invoke  func(*chclient.FakeClient) error
		wantErr error
	}{
		{
			name:    "EnsureRunning",
			fake:    &chclient.FakeClient{EnsureRunningErr: ensureRunningErr},
			invoke:  func(f *chclient.FakeClient) error { return f.EnsureRunning(t.Context()) },
			wantErr: ensureRunningErr,
		},
		{
			name:    "Shutdown",
			fake:    &chclient.FakeClient{ShutdownErr: shutdownErr},
			invoke:  func(f *chclient.FakeClient) error { return f.Shutdown(t.Context()) },
			wantErr: shutdownErr,
		},
		{
			name:    "Stop",
			fake:    &chclient.FakeClient{StopErr: stopErr},
			invoke:  func(f *chclient.FakeClient) error { return f.Stop(t.Context()) },
			wantErr: stopErr,
		},
		{
			name: "Info",
			fake: &chclient.FakeClient{InfoErr: infoErr},
			invoke: func(f *chclient.FakeClient) error {
				_, err := f.Info(t.Context())
				return err
			},
			wantErr: infoErr,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotErr := tt.invoke(tt.fake)
			if gotErr != tt.wantErr {
				t.Fatalf("%s error = %v, want %v (returned unchanged)", tt.name, gotErr, tt.wantErr)
			}

			wantCalls := []string{tt.name}
			if !slices.Equal(tt.fake.Calls, wantCalls) {
				t.Errorf("Calls = %v, want %v", tt.fake.Calls, wantCalls)
			}
		})
	}
}

// TestFakeCallLogStartsEmpty pins that a fresh fake has an empty call log.
func TestFakeCallLogStartsEmpty(t *testing.T) {
	fake := new(chclient.FakeClient)

	if len(fake.Calls) != 0 {
		t.Fatalf("Calls = %v, want empty", fake.Calls)
	}
}

// TestFakeCallLogSequence pins that Calls reflects the invocation order and
// that repeated invocations append rather than overwrite.
func TestFakeCallLogSequence(t *testing.T) {
	fake := &chclient.FakeClient{State: ch.VMState("Running")}

	if err := fake.EnsureRunning(t.Context()); err != nil {
		t.Fatalf("EnsureRunning() error = %v, want nil", err)
	}

	if _, err := fake.Info(t.Context()); err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}

	if _, err := fake.Info(t.Context()); err != nil {
		t.Fatalf("Info() error = %v, want nil", err)
	}

	if err := fake.Shutdown(t.Context()); err != nil {
		t.Fatalf("Shutdown() error = %v, want nil", err)
	}

	if err := fake.Stop(t.Context()); err != nil {
		t.Fatalf("Stop() error = %v, want nil", err)
	}

	wantCalls := []string{"EnsureRunning", "Info", "Info", "Shutdown", "Stop"}
	if !slices.Equal(fake.Calls, wantCalls) {
		t.Fatalf("Calls = %v, want %v", fake.Calls, wantCalls)
	}
}
