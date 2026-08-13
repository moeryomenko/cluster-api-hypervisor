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

// Package nft implements the provider's nftables NAT and forwarding layer:
// it owns the inet NAT table (the cluster's "k8slab" table by default) that
// gives cluster bridge VMs their outbound NAT and host-to-VM forwarding,
// replacing the lab's host-side nat.nft load. The Manager renders the
// ruleset and applies it through the nft binary behind the injectable Runner
// seam, so no root privileges and no real nftables state are ever needed in
// tests.
package nft

import (
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"
)

// Runner is the command-execution seam of the manager: Run executes the
// program named name with the given arguments, feeds the process the input
// read from stdin (nil when there is no input), and returns the combined
// output and error. Production uses the host nft binary via
// exec.CommandContext; tests inject a recording fake so no external tool is
// ever invoked.
type Runner interface {
	Run(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, error)
}

// Manager owns one cluster network's nftables state: the rendered ruleset
// names the given inet table and quotes the given bridge in its forwarding
// rules. The NAT source CIDR is the lab default 192.168.124.0/24.
type Manager struct {
	bridgeName string
	tableName  string
	runner     Runner
}

// execRunner is the default Runner: it executes the command on the host,
// feeding the input to its stdin, and returns the process combined output.
type execRunner struct{}

// Run executes name with args on the host with stdin attached and returns
// the combined output.
func (execRunner) Run(ctx context.Context, name string, args []string, stdin io.Reader) ([]byte, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdin = stdin

	return cmd.CombinedOutput()
}

// NewManager builds a manager for one cluster network over the given command
// runner. A nil runner is replaced with the default exec-based runner.
func NewManager(bridgeName, tableName string, runner Runner) *Manager {
	if runner == nil {
		runner = execRunner{}
	}

	return &Manager{bridgeName: bridgeName, tableName: tableName, runner: runner}
}

// Apply renders the ruleset for the inet table and runs the runner once as
// `nft -f -` with the ruleset fed on stdin. The rendered ruleset leads with
// a destroy of the table, so a re-apply replaces the table instead of
// duplicating its chains and rules: Apply is idempotent. A runner failure is
// returned unchanged (detectable with errors.Is against the runner's error).
func (m *Manager) Apply(ctx context.Context) error {
	_, err := m.runner.Run(ctx, "nft", []string{"-f", "-"}, strings.NewReader(m.ruleset()))

	return err
}

// Delete removes the table by running the runner once as
// `nft delete table inet <table>` with no stdin. Deleting an absent table is
// tolerated: when the runner's error or its captured output mentions "No
// such file or directory" or "does not exist", Delete returns nil. Any other
// failure is returned unchanged (detectable with errors.Is).
func (m *Manager) Delete(ctx context.Context) error {
	out, err := m.runner.Run(ctx, "nft", []string{"delete", "table", "inet", m.tableName}, nil)
	if err == nil {
		return nil
	}
	if isMissingTable(err, out) {
		return nil
	}

	return err
}

// ruleset renders the exact ruleset fixture: a leading destroy of the table
// (load-time idempotency) followed by the table definition mirroring the lab
// nat.nft semantics — the postrouting chain masquerades the lab source CIDR
// without qualifying the output interface (VM egress may leave via the
// direct uplink or the VPN tunnel, whichever route is active), and the
// forward chain accepts traffic to and from the bridge.
func (m *Manager) ruleset() string {
	return fmt.Sprintf(`destroy table inet %s

table inet %s {
	chain postrouting {
		type nat hook postrouting priority srcnat; policy accept
		ip saddr 192.168.124.0/24 masquerade
	}

	chain forward {
		type filter hook forward priority filter; policy accept
		iifname "%s" accept
		oifname "%s" accept
	}
}
`, m.tableName, m.tableName, m.bridgeName, m.bridgeName)
}

// isMissingTable reports whether the runner failure describes an absent
// table: the detail may appear in the captured output (the real shape of
// exec.ExitError from CombinedOutput on a failed nft delete) or in the error
// string itself.
func isMissingTable(err error, out []byte) bool {
	detail := err.Error() + "\n" + string(out)

	return strings.Contains(detail, "No such file or directory") || strings.Contains(detail, "does not exist")
}
