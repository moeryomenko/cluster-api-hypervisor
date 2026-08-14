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

// The nftables NAT manager contract (test-first).
//
// This suite pins the provider's nftables integration: it owns the inet NAT
// table (the cluster's "k8slab" table by default) that gives cluster bridge
// VMs their outbound NAT and host-to-VM forwarding, replacing the lab's
// host-side nat.nft load. The manager renders the ruleset and applies it
// through the nft binary behind an injectable exec seam, so no root
// privileges and no real nftables state are ever needed in tests.
//
// The contract, in prose:
//
//   - Runner is the command-execution seam: Run executes the program named
//     name with the given arguments, feeds the process the ruleset read from
//     stdin (nil when there is no input), and returns the combined output
//     and error. Production uses the host nft binary via
//     exec.CommandContext; tests inject a recording fake so no external tool
//     is ever invoked. Unlike the confext seam, this one carries stdin: the
//     ruleset is applied with `nft -f -`, which reads its file from stdin.
//   - NewManager(bridgeName, tableName string, runner Runner) *Manager
//     builds a manager for one cluster network: the rendered ruleset names
//     the given table and quotes the given bridge in its forwarding rules.
//     The NAT source CIDR is the lab default 192.168.124.0/24.
//   - Apply(ctx context.Context) error renders the ruleset for the inet
//     table and runs the runner once as `nft -f -` with the ruleset fed on
//     stdin. The rendered ruleset leads with a destroy of the table so a
//     re-apply replaces, rather than duplicates, the previous load: Apply is
//     idempotent. A runner failure is returned unchanged (detectable with
//     errors.Is against the runner's error).
//   - Delete(ctx context.Context) error removes the table by running the
//     runner once as `nft delete table inet <table>` with no stdin. Deleting
//     an absent table is tolerated: when the runner's error or its captured
//     output mentions "No such file or directory" or "does not exist",
//     Delete returns nil; any other failure is returned unchanged
//     (detectable with errors.Is).
package nft_test

import (
	"context"
	"errors"
	"io"
	"reflect"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/nft"
)

// Compile-time pins: the constructor and the two lifecycle operations must
// exist with exactly these names and signatures, and the fake below must
// satisfy the runner seam.
var (
	_ func(string, string, nft.Runner) *nft.Manager = nft.NewManager
	_ func(*nft.Manager, context.Context) error     = (*nft.Manager).Apply
	_ func(*nft.Manager, context.Context) error     = (*nft.Manager).Delete
	_ nft.Runner                                    = (*fakeRunner)(nil)
)

// recordedCall is one captured command invocation: the program name, the
// exact argument list, and the bytes read from stdin (nil when the manager
// passed no input).
type recordedCall struct {
	name string
	args []string
	in   []byte
}

// fakeRunner records every invocation, including the stdin bytes, and
// returns a canned output. When failOn is non-zero the failOn-th invocation
// returns both the canned output and the canned error, letting tests pin
// both the success shape and the failure shape of a specific call.
type fakeRunner struct {
	calls  []recordedCall
	out    []byte
	err    error
	failOn int
}

// Run implements nft.Runner. The context is accepted and deliberately
// ignored: cancellation propagation is an implementation concern of the
// default exec runner, not part of this contract.
func (f *fakeRunner) Run(_ context.Context, name string, args []string, stdin io.Reader) ([]byte, error) {
	argsCopy := append([]string(nil), args...)
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.calls = append(f.calls, recordedCall{name: name, args: argsCopy, in: in})
	if f.failOn > 0 && len(f.calls) == f.failOn {
		return f.out, f.err
	}

	return f.out, nil
}

// wantRuleset is the exact rendered ruleset for the default names (table
// "k8slab", bridge "k8sbr0"): a leading destroy of the table followed by the
// table definition, mirroring the lab nat.nft semantics — the postrouting
// chain masquerades the lab source CIDR without qualifying the output
// interface (VM egress may leave via the direct uplink or the VPN tunnel,
// whichever route is active), and the forward chain accepts traffic to and
// from the bridge.
const wantRuleset = `destroy table inet k8slab

table inet k8slab {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept
		ip saddr 192.168.124.0/24 masquerade
	}

	chain forward {
		type filter hook forward priority filter; policy accept
		iifname "k8sbr0" accept
		oifname "k8sbr0" accept
	}
}
`

// wantApplyCall is the exact invocation Apply must produce: the nft binary
// with the -f - flags (read the ruleset from stdin) and the fixture bytes on
// stdin.
var wantApplyCall = recordedCall{
	name: "nft",
	args: []string{"-f", "-"},
	in:   []byte(wantRuleset),
}

// TestApplyRendersExactRuleset pins the apply invocation: one runner call
// with the nft binary, the -f - flags (ruleset from stdin), and the exact
// rendered ruleset bytes on stdin, matching the fixture for the configured
// table and bridge names.
func TestApplyRendersExactRuleset(t *testing.T) {
	runner := &fakeRunner{}
	m := nft.NewManager("k8sbr0", "k8slab", runner)

	if err := m.Apply(t.Context()); err != nil {
		t.Fatalf("Apply error: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("Apply ran the runner %d times, want 1", len(runner.calls))
	}
	if !reflect.DeepEqual(runner.calls[0], wantApplyCall) {
		t.Errorf("Apply invocation = %+v, want %+v", runner.calls[0], wantApplyCall)
	}
}

// TestApplyIdempotent pins that Apply is re-appliable: a second apply
// produces the identical invocation (same binary, same flags, same rendered
// ruleset bytes) and succeeds. The destroy preamble in the rendered ruleset
// is what makes repeated loads replace the table instead of duplicating its
// chains and rules.
func TestApplyIdempotent(t *testing.T) {
	runner := &fakeRunner{}
	m := nft.NewManager("k8sbr0", "k8slab", runner)

	for i := 0; i < 2; i++ {
		if err := m.Apply(t.Context()); err != nil {
			t.Fatalf("Apply %d error: %v", i+1, err)
		}
	}

	want := []recordedCall{wantApplyCall, wantApplyCall}
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("Apply invocations = %+v, want %+v", runner.calls, want)
	}
}

// TestApplyPropagatesFailure pins the apply failure contract: a runner
// error surfaces from Apply, detectable with errors.Is against the runner's
// error, and the runner is invoked exactly once.
func TestApplyPropagatesFailure(t *testing.T) {
	boom := errors.New("nft: ruleset rejected")
	runner := &fakeRunner{err: boom, failOn: 1}
	m := nft.NewManager("k8sbr0", "k8slab", runner)

	err := m.Apply(t.Context())
	if err == nil {
		t.Fatal("Apply succeeded, want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Apply error %v does not wrap the runner error %v", err, boom)
	}
	if len(runner.calls) != 1 {
		t.Errorf("Apply ran the runner %d times on failure, want 1", len(runner.calls))
	}
}

// TestDeleteInvokesNftDeleteTable pins the delete invocation: one runner
// call with the nft binary, the delete table inet <table> arguments, and no
// stdin.
func TestDeleteInvokesNftDeleteTable(t *testing.T) {
	runner := &fakeRunner{}
	m := nft.NewManager("k8sbr0", "k8slab", runner)

	if err := m.Delete(t.Context()); err != nil {
		t.Fatalf("Delete error: %v", err)
	}

	if len(runner.calls) != 1 {
		t.Fatalf("Delete ran the runner %d times, want 1", len(runner.calls))
	}
	got := runner.calls[0]
	if got.name != "nft" {
		t.Errorf("Delete ran %q, want %q", got.name, "nft")
	}
	if !reflect.DeepEqual(got.args, []string{"delete", "table", "inet", "k8slab"}) {
		t.Errorf("Delete args = %v, want %v", got.args, []string{"delete", "table", "inet", "k8slab"})
	}
	if len(got.in) != 0 {
		t.Errorf("Delete fed %d bytes of stdin, want none", len(got.in))
	}
}

// TestDeleteToleratesMissingTable pins the delete idempotency: when the
// table is absent the runner fails in one of the two shapes the nft binary
// produces — the "No such file or directory" detail in the captured output
// with a generic exit-status error (the exec.CommandContext shape), or a
// "does not exist" message in the error itself — and Delete treats both as
// success, returning nil.
func TestDeleteToleratesMissingTable(t *testing.T) {
	tests := []struct {
		name   string
		runner *fakeRunner
	}{
		{
			name: "missing table detail in output",
			runner: &fakeRunner{
				out:    []byte("Error: Could not process rule: No such file or directory\n"),
				err:    errors.New("exit status 1"),
				failOn: 1,
			},
		},
		{
			name:   "missing table detail in error",
			runner: &fakeRunner{err: errors.New("nft: table 'inet k8slab' does not exist"), failOn: 1},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			m := nft.NewManager("k8sbr0", "k8slab", tt.runner)

			if err := m.Delete(t.Context()); err != nil {
				t.Errorf("Delete error: %v, want nil for an absent table", err)
			}
		})
	}
}

// TestDeletePropagatesFailure pins the delete failure contract: a runner
// error that does not describe a missing table surfaces from Delete,
// detectable with errors.Is against the runner's error.
func TestDeletePropagatesFailure(t *testing.T) {
	boom := errors.New("nft: operation not permitted")
	runner := &fakeRunner{err: boom, failOn: 1}
	m := nft.NewManager("k8sbr0", "k8slab", runner)

	err := m.Delete(t.Context())
	if err == nil {
		t.Fatal("Delete succeeded, want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("Delete error %v does not wrap the runner error %v", err, boom)
	}
}
