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

// Manager flag contract (test-first): the provider binary must document the
// startup flags it accepts via --help. The pinned contract flags are
// --webhook-cert-dir, --webhook-port, --health-addr, --kubeconfig, and one
// per-controller concurrency placeholder, --hypervisorcluster-concurrency.
// Additional concurrency and environment flags may exist; only these five are
// pinned here. While main.go is the empty stub the binary ignores --help and
// prints nothing, so this suite fails (red phase).
package main

import (
	"bytes"
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"
)

// requiredStartupFlags are the flags the manager must accept and document.
var requiredStartupFlags = []string{
	"webhook-cert-dir",
	"webhook-port",
	"health-addr",
	"kubeconfig",
	"hypervisorcluster-concurrency",
}

// TestManagerFlagContract runs the provider binary with --help and asserts
// that help exits cleanly and documents every pinned startup flag.
func TestManagerFlagContract(t *testing.T) {
	bin := buildManagerBinary(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--help")

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout

	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("manager --help exited with an error: %v (stderr: %s)", err, stderr.String())
	}

	output := stdout.String() + stderr.String()
	for _, flag := range requiredStartupFlags {
		if !strings.Contains(output, flag) {
			t.Errorf("manager --help does not document the %q flag", flag)
		}
	}
}
