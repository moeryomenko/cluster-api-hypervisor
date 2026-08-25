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

// HypervisorMachine SSH public key resolution contract (TASK-021 P2).
//
// The CIDATA render resolves the SSH public key for one machine in a fixed
// order: the linked HypervisorConfig's spec.sshPublicKey first, then — when
// that is empty and HYPERVISOR_SSH_PUBLIC_KEY_FILE is set — the trimmed
// content of the file the variable names. The file content must be non-empty
// after trimming; a missing or empty file, and an empty spec key with the
// variable unset, fail the render with an error naming
// HYPERVISOR_SSH_PUBLIC_KEY_FILE.
//
// Grill cases covered:
//   - resolution order: a non-empty spec key wins over the env-named file
//   - a missing env-named file fails naming HYPERVISOR_SSH_PUBLIC_KEY_FILE
//   - an empty spec key with an existing file renders with the trimmed file
//     content

package controllers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// clearConfigSSHPublicKey empties spec.sshPublicKey on the linked bootstrap
// config, standing in for a cluster template that ships no key.
func clearConfigSSHPublicKey(t *testing.T, c client.Client, lm *linkedMachine) {
	t.Helper()
	cfg := lm.config.DeepCopy()
	cfg.Spec.SSHPublicKey = ""
	if err := c.Update(t.Context(), cfg); err != nil {
		t.Fatalf("clear spec.sshPublicKey on %q: %v", cfg.Name, err)
	}
}

// writeSSHKeyFile writes content to a temp file and returns its path, standing
// in for the build-dir key mounted into the provider container.
func writeSSHKeyFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "ssh-lab.pub")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write ssh key file: %v", err)
	}
	return path
}

// TestMachineSSHKeyResolutionOrder pins the resolution order: a non-empty
// spec.sshPublicKey wins over the HYPERVISOR_SSH_PUBLIC_KEY_FILE file, which
// is never read in that case.
func TestMachineSSHKeyResolutionOrder(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-sshkey-order", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	const fileKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIfile-key-must-lose"
	fx.r.Config.SSHPublicKeyFile = writeSSHKeyFile(t, fileKey+"\n")

	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	if got := len(fx.render.calls); got != 1 {
		t.Fatalf("CIDATA render called %d times, want 1", got)
	}
	if got := fx.render.calls[0].SSHPublicKey; got != testSSHPublicKey {
		t.Errorf("render ssh public key = %q, want the spec key %q", got, testSSHPublicKey)
	}
}

// TestMachineSSHKeyFileMissingNamesEnvVar pins the failure contract: an empty
// spec key with HYPERVISOR_SSH_PUBLIC_KEY_FILE naming a missing file fails the
// reconcile with an error naming the variable.
func TestMachineSSHKeyFileMissingNamesEnvVar(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-sshkey-missing", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)
	clearConfigSSHPublicKey(t, c, lm)

	fx.r.Config.SSHPublicKeyFile = filepath.Join(t.TempDir(), "does-not-exist.pub")

	_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)})
	if err == nil {
		t.Fatal("Reconcile succeeded with a missing ssh key file, want an error")
	}
	if !strings.Contains(err.Error(), "HYPERVISOR_SSH_PUBLIC_KEY_FILE") {
		t.Errorf("Reconcile error %v does not name HYPERVISOR_SSH_PUBLIC_KEY_FILE", err)
	}
}

// TestMachineSSHKeyUnsetEnvKeepsFailureNamingEnvVar pins the feature-off
// contract: an empty spec key with HYPERVISOR_SSH_PUBLIC_KEY_FILE unset keeps
// the render-time failure, and the error names the variable so the knob is
// discoverable.
func TestMachineSSHKeyUnsetEnvKeepsFailureNamingEnvVar(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-sshkey-unset", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)
	clearConfigSSHPublicKey(t, c, lm)

	_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)})
	if err == nil {
		t.Fatal("Reconcile succeeded with no resolvable ssh key, want an error")
	}
	if !strings.Contains(err.Error(), "HYPERVISOR_SSH_PUBLIC_KEY_FILE") {
		t.Errorf("Reconcile error %v does not name HYPERVISOR_SSH_PUBLIC_KEY_FILE", err)
	}
}

// TestMachineSSHRendersWithFileKey pins the fallback happy path: an empty
// spec key with HYPERVISOR_SSH_PUBLIC_KEY_FILE naming an existing file renders
// the CIDATA parts with the trimmed file content.
func TestMachineSSHRendersWithFileKey(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-sshkey-file", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)
	clearConfigSSHPublicKey(t, c, lm)

	const fileKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIfile-key-wins"
	fx.r.Config.SSHPublicKeyFile = writeSSHKeyFile(t, "  "+fileKey+"  \n")

	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	if got := len(fx.render.calls); got != 1 {
		t.Fatalf("CIDATA render called %d times, want 1", got)
	}
	if got := fx.render.calls[0].SSHPublicKey; got != fileKey {
		t.Errorf("render ssh public key = %q, want the trimmed file key %q", got, fileKey)
	}
}
