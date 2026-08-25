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

// HypervisorMachine controller contract, socket isolation: the per-machine
// VM client factory and its process-lifetime cache.
//
// These tests pin the wiring that keeps one cloud-hypervisor API socket tree
// per VM: the reconciler builds each machine's client through the
// NewVMClient factory seam with that machine's own directory
// <SocketDir>/<machine>, exactly once per machine for the provider process
// lifetime — reconciles reuse the cached instance so Manager.Start's
// started-state check prevents respawning the VMM — and reconcileDelete
// evicts the entry after Stop so a later re-create starts fresh. A single
// shared client wired once at startup would make every machine bind the same
// api.sock path, so concurrent boots collide by construction; a client built
// per reconcile would respawn the cloud-hypervisor process every time.
package controllers

import (
	"path/filepath"
	"slices"
	"sync"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
)

// recordingVMFactory captures every NewVMClient construction: the requested
// socket directories and binaries, in call order, plus the fake handed back
// per construction. It is safe for concurrent use, mirroring the production
// requirement that the factory is callable from concurrent workers.
type recordingVMFactory struct {
	mu       sync.Mutex
	dirs     []string
	binaries []string
	fakes    []*chclient.FakeClient
}

// newVMClient implements the NewVMClient seam: it records the arguments and
// returns a fresh fake bound to this construction.
func (f *recordingVMFactory) newVMClient(socketDir, binary string) chclient.Client {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.dirs = append(f.dirs, socketDir)
	f.binaries = append(f.binaries, binary)
	fake := &chclient.FakeClient{}
	f.fakes = append(f.fakes, fake)
	return fake
}

// snapshot returns the recorded socket directories and binaries under the
// lock.
func (f *recordingVMFactory) snapshot() (dirs, binaries []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.dirs), slices.Clone(f.binaries)
}

// TestMachineVMClientFactoryPerMachineSocketDirs pins the socket-tree
// contract: provisioning two machines constructs one client per machine,
// each bound to its own directory <SocketDir>/<machine> under the configured
// socket root, so no two machines share an api.sock path.
func TestMachineVMClientFactoryPerMachineSocketDirs(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-socket-tree", "capi-cluster")
	lmA := newLinkedMachine(t, c, lc, "node-a", true)
	lmB := newLinkedMachine(t, c, lc, "node-b", true)

	factory := &recordingVMFactory{}
	fx.r.NewVMClient = factory.newVMClient

	fx.reconcileMachine(t, lmA.hm)
	fx.reconcileMachine(t, lmB.hm)

	dirs, binaries := factory.snapshot()
	wantDirs := []string{
		filepath.Join(testSocketDir, lmA.name),
		filepath.Join(testSocketDir, lmB.name),
	}
	if !slices.Equal(dirs, wantDirs) {
		t.Errorf("factory socket dirs = %v, want %v", dirs, wantDirs)
	}
	if dirs[0] == dirs[1] {
		t.Errorf("both machines constructed clients over the same dir %q", dirs[0])
	}
	if factory.fakes[0] == factory.fakes[1] {
		t.Error("both machines received the same client instance, want distinct clients per machine")
	}
	for _, binary := range binaries {
		if binary != fx.r.Config.CHBinary {
			t.Errorf("factory binary = %q, want the provider config value %q", binary, fx.r.Config.CHBinary)
		}
	}
}

// TestMachineVMClientCachedAcrossReconciles pins the process-lifetime cache:
// reconciling the same machine twice constructs the client exactly once and
// drives that one instance both times — the boot lands on the first
// reconcile, the follow-up state poll on the second — so Manager.Start's
// started-state check (not a fresh manager per reconcile) governs spawning.
func TestMachineVMClientCachedAcrossReconciles(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-client-cache", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-a", true)

	factory := &recordingVMFactory{}
	fx.r.NewVMClient = factory.newVMClient

	fx.reconcileMachine(t, lm.hm)
	fx.reconcileMachine(t, lm.hm)

	dirs, _ := factory.snapshot()
	if len(dirs) != 1 {
		t.Fatalf("factory constructions across two reconciles = %d (%v), want exactly 1", len(dirs), dirs)
	}
	if wantDir := filepath.Join(testSocketDir, lm.name); dirs[0] != wantDir {
		t.Errorf("factory socket dir = %q, want %q", dirs[0], wantDir)
	}
	// Both reconciles drove the single constructed instance: EnsureRunning
	// ran once (the first reconcile's boot), and each reconcile's state poll
	// landed on the same cached client.
	wantCalls := []string{"EnsureRunning", "Info", "Info"}
	if calls := factory.fakes[0].Calls; !slices.Equal(calls, wantCalls) {
		t.Errorf("cached client calls across reconciles = %v, want %v", calls, wantCalls)
	}
}

// TestMachineDeleteEvictsCachedVMClient pins the delete-path eviction: after
// a successful teardown the cache no longer holds the machine's entry, so a
// later re-create of a machine with the same name builds a fresh client
// through the factory instead of reusing a manager whose Start refuses to
// run after Stop.
func TestMachineDeleteEvictsCachedVMClient(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-cache-evict", "capi-cluster")
	lmA := newLinkedMachine(t, c, lc, "node-a", false)

	factory := &recordingVMFactory{}
	fx.r.NewVMClient = factory.newVMClient

	fx.reconcileMachine(t, lmA.hm)
	if _, ok := fx.r.vmClients.Load(lmA.name); !ok {
		t.Fatal("no cache entry after the provisioning reconcile, want the client cached")
	}

	markMachineForDeletion(t, c, lmA.hm)
	fx.reconcileMachine(t, lmA.hm)
	assertMachineReclaimed(t, c, lmA.hm)

	if _, ok := fx.r.vmClients.Load(lmA.name); ok {
		t.Error("cache entry survived the delete reconcile, want it removed so a re-create starts fresh")
	}

	// A later re-create of a machine with this name builds a fresh client
	// through the factory again instead of resurrecting the stopped manager
	// (whose Start refuses to run after Stop).
	fresh := fx.r.vmClientFor(lmA.hm)
	if fresh == chclient.Client(factory.fakes[0]) {
		t.Error("vmClientFor after delete returned the evicted instance, want a freshly built client")
	}
	dirs, _ := factory.snapshot()
	if len(dirs) != 2 {
		t.Fatalf("factory constructions after delete = %d (%v), want 2", len(dirs), dirs)
	}
}

// TestMachineDeleteStopsOwnVMClient pins the teardown routing: deleting one
// machine shuts down and stops exactly that machine's client — built through
// the same factory with the machine's own socket directory, whose Stop
// removes that machine's socket tree — while another machine's client is
// never touched.
func TestMachineDeleteStopsOwnVMClient(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-delete-own-vm", "capi-cluster")
	lmA := newLinkedMachine(t, c, lc, "node-a", false)
	lmB := newLinkedMachine(t, c, lc, "node-b", false)

	clients := map[string]*chclient.FakeClient{
		lmA.name: {},
		lmB.name: {},
	}
	var mu sync.Mutex
	fx.r.NewVMClient = func(socketDir, _ string) chclient.Client {
		mu.Lock()
		defer mu.Unlock()
		return clients[filepath.Base(socketDir)]
	}

	markMachineForDeletion(t, c, lmA.hm)
	fx.reconcileMachine(t, lmA.hm)

	assertMachineReclaimed(t, c, lmA.hm)
	if calls := clients[lmA.name].Calls; !slices.Equal(calls, []string{"Shutdown", "Stop"}) {
		t.Errorf("deleted machine's client calls = %v, want [Shutdown Stop]", calls)
	}
	if calls := clients[lmB.name].Calls; len(calls) != 0 {
		t.Errorf("other machine's client was touched during deletion: %v", calls)
	}
}
