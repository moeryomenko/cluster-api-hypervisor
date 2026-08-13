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

// The netlink bridge and TAP orchestration contract (test-first).
//
// This suite pins the host networking layer of the provider: it owns the lab
// bridge and one TAP per machine over netlink, replacing the host-side
// systemd-networkd .netdev/.network files. The real implementation wraps
// vishvananda/netlink; this contract keeps that dependency behind a narrow
// seam so the orchestration logic is testable without netlink calls or root.
//
// The contract, in prose:
//
//   - Link is the minimal view of a kernel link the orchestration needs: its
//     name, its kind ("bridge" for the lab bridge, "tuntap" for a machine
//     TAP), and the name of the bridge it is enslaved to (empty when the
//     link has no master). No other link attributes are part of the
//     contract.
//   - LinkOps is the injectable seam wrapping netlink. LinkByName returns
//     the link with the given name, or ErrLinkNotFound when no such link
//     exists; any other error means the lookup itself failed and must be
//     propagated unchanged. LinkAdd creates a link of the given kind with
//     the given name and is only ever called for a name that does not exist
//     yet: a correct Manager never double-creates. LinkSetMaster enslaves
//     the named link to the named master bridge. LinkDel removes the named
//     link and returns ErrLinkNotFound when the link does not exist, so
//     deletion can be idempotent.
//   - ErrLinkNotFound is the sentinel the Manager uses to distinguish "does
//     not exist" from real failures. Implementations may wrap it; the
//     Manager matches it with errors.Is.
//   - NewManager(ops LinkOps) *Manager builds the orchestrator over the
//     seam.
//   - EnsureBridge(name string) error creates the bridge when it is absent
//     (LinkAdd of kind "bridge") and is a no-op when a link with that name
//     already exists, whatever its kind: the existence check is by name
//     only, mirroring the pass condition that the bridge exists with the
//     expected name. A failed lookup or a failed create is returned
//     unchanged.
//   - EnsureTap(bridgeName, tapName string) error creates the machine TAP
//     when it is absent (LinkAdd of kind "tuntap") and then enslaves it to
//     the bridge (LinkSetMaster). When the TAP already exists it is
//     enslaved if it is not already mastered to this bridge and left alone
//     if it is; a TAP mastered to a different bridge is re-enslaved.
//     EnsureTap never creates the bridge: the cluster controller ensures
//     the bridge before the machine controller ensures TAPs, and enslaving
//     to a missing bridge surfaces the LinkSetMaster error. A TAP left
//     unenslaved by a failed attempt is recovered by the next EnsureTap
//     call, which finds it and enslaves it.
//   - DeleteTap(tapName string) error and DeleteBridge(name string) error
//     remove the link and are idempotent: deleting a link that does not
//     exist returns nil. Any other deletion error is returned unchanged.
//
// Empty names and non-bridge kinds are deliberately not validated here: link
// names are produced by the controllers from machine/cluster names, and the
// ops seam or the kernel surfaces invalid input as an error. The Manager's
// idempotency checks are by name only.
package networking_test

import (
	"errors"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/networking"
)

// Names and kinds used across the suite: the lab bridge and a machine TAP,
// matching the spec defaults (bridge "k8sbr0", TAP prefix "k8s-").
const (
	bridgeName = "k8sbr0"
	tapName    = "k8s-master-1"

	kindBridge = "bridge"
	kindTap    = "tuntap"

	// Operation keys recorded in the fake call log.
	opAdd       = "add"
	opByName    = "byname"
	opSetMaster = "setmaster"
	opDel       = "del"
)

// Sentinel errors the fake returns for injected failures and for natural
// kernel-state violations, so tests can assert propagation identity.
var (
	errAddBridge   = errors.New("fake: bridge add denied")
	errAddTap      = errors.New("fake: tap add denied")
	errLookup      = errors.New("fake: link lookup denied")
	errSetMaster   = errors.New("fake: enslave denied")
	errMasterGone  = errors.New("fake: master bridge not found")
	errDelete      = errors.New("fake: delete denied")
	errAlreadyLink = errors.New("fake: link already exists")
)

// Compile-time pins: the seam, the constructor, and the Manager methods must
// exist with exactly these names and signatures.
var (
	_ func(networking.LinkOps) *networking.Manager              = networking.NewManager
	_ func(*networking.Manager, string) error                   = (*networking.Manager).EnsureBridge
	_ func(*networking.Manager, string, string) error           = (*networking.Manager).EnsureTap
	_ func(*networking.Manager, string) error                   = (*networking.Manager).DeleteTap
	_ func(*networking.Manager, string) error                   = (*networking.Manager).DeleteBridge
	_ func(networking.LinkOps, string) (networking.Link, error) = networking.LinkOps.LinkByName
	_ func(networking.LinkOps, string, string) error            = networking.LinkOps.LinkAdd
	_ func(networking.LinkOps, string, string) error            = networking.LinkOps.LinkSetMaster
	_ func(networking.LinkOps, string) error                    = networking.LinkOps.LinkDel
	_ networking.LinkOps                                        = (*fakeLinkOps)(nil)
	_ networking.Link                                           = networking.Link{}
)

// fakeLink is the fake's view of a kernel link: kind and master bridge only,
// which is all the contract uses.
type fakeLink struct {
	kind   string
	master string
}

// fakeCall records one invocation of the ops seam.
type fakeCall struct {
	op     string
	kind   string
	name   string
	master string
}

// fakeLinkOps is an in-memory LinkOps. It records every call in invocation
// order and simulates kernel link state. Injecting an error under an
// operation key (see failAdd and the op constants) makes that operation
// fail with the injected error.
type fakeLinkOps struct {
	links map[string]fakeLink
	calls []fakeCall

	failBy map[string]error
}

// LinkByName implements LinkOps: it records the call and returns the link,
// or ErrLinkNotFound when no such link exists.
func (f *fakeLinkOps) LinkByName(name string) (networking.Link, error) {
	f.calls = append(f.calls, fakeCall{op: opByName, name: name})
	if err := f.fail(opByName); err != nil {
		return networking.Link{}, err
	}
	l, ok := f.links[name]
	if !ok {
		return networking.Link{}, networking.ErrLinkNotFound
	}

	return networking.Link{Name: name, Kind: l.kind, Master: l.master}, nil
}

// LinkAdd implements LinkOps: it records the call, fails when a link with
// the name already exists (a correct Manager never double-creates), and
// otherwise creates the link.
func (f *fakeLinkOps) LinkAdd(kind, name string) error {
	f.calls = append(f.calls, fakeCall{op: opAdd, kind: kind, name: name})
	if err := f.fail(failAdd(kind)); err != nil {
		return err
	}
	if _, ok := f.links[name]; ok {
		return errAlreadyLink
	}
	f.links[name] = fakeLink{kind: kind}

	return nil
}

// LinkSetMaster implements LinkOps: it records the call and enslaves the
// named link to the named master bridge. Enslaving a missing link returns
// ErrLinkNotFound; enslaving to a missing bridge (for example a bridge the
// cluster controller has not ensured yet) returns errMasterGone.
func (f *fakeLinkOps) LinkSetMaster(name, master string) error {
	f.calls = append(f.calls, fakeCall{op: opSetMaster, name: name, master: master})
	if err := f.fail(opSetMaster); err != nil {
		return err
	}
	l, ok := f.links[name]
	if !ok {
		return networking.ErrLinkNotFound
	}
	if _, ok := f.links[master]; !ok {
		return errMasterGone
	}
	l.master = master
	f.links[name] = l

	return nil
}

// LinkDel implements LinkOps: it records the call and removes the link.
// Deleting a link that does not exist returns ErrLinkNotFound so the
// Manager can treat deletion as idempotent.
func (f *fakeLinkOps) LinkDel(name string) error {
	f.calls = append(f.calls, fakeCall{op: opDel, name: name})
	if _, ok := f.links[name]; !ok {
		return networking.ErrLinkNotFound
	}
	if err := f.fail(opDel); err != nil {
		return err
	}
	delete(f.links, name)

	return nil
}

// fail returns the injected error for the operation key, if any.
func (f *fakeLinkOps) fail(key string) error {
	return f.failBy[key]
}

// failAdd returns the failure-injection key for a LinkAdd of the given kind.
func failAdd(kind string) string {
	return opAdd + ":" + kind
}

// newFake builds an empty fake.
func newFake() *fakeLinkOps {
	return &fakeLinkOps{
		links:  make(map[string]fakeLink),
		failBy: make(map[string]error),
	}
}

// mustManager builds a Manager over the fake.
func mustManager(t *testing.T, f *fakeLinkOps) *networking.Manager {
	t.Helper()

	return networking.NewManager(f)
}

// seedLink puts a link into the fake's state, as if it already existed
// before the test ran.
func seedLink(f *fakeLinkOps, name, kind, master string) {
	f.links[name] = fakeLink{kind: kind, master: master}
}

// wantCalls asserts the recorded call log matches the expected calls exactly,
// in order. It is the primary behavioral pin of the suite.
func wantCalls(t *testing.T, f *fakeLinkOps, want ...fakeCall) {
	t.Helper()
	if len(f.calls) != len(want) {
		t.Fatalf("call log = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatalf("call %d = %v, want %v (full log %v)", i, f.calls[i], want[i], f.calls)
		}
	}
}

// TestEnsureBridgeCreatesWhenAbsent pins the create path: an absent bridge
// is created with kind "bridge" and nothing else happens.
func TestEnsureBridgeCreatesWhenAbsent(t *testing.T) {
	f := newFake()
	m := mustManager(t, f)

	if err := m.EnsureBridge(bridgeName); err != nil {
		t.Fatalf("EnsureBridge(%q) error: %v", bridgeName, err)
	}

	wantCalls(t, f,
		fakeCall{op: opByName, name: bridgeName},
		fakeCall{op: opAdd, kind: kindBridge, name: bridgeName},
	)

	// The fake now reports the bridge as existing, with the right kind and
	// no master.
	got, err := f.LinkByName(bridgeName)
	if err != nil {
		t.Fatalf("LinkByName(%q) after create error: %v", bridgeName, err)
	}
	if got.Kind != kindBridge || got.Master != "" {
		t.Errorf("created link = %+v, want kind %q with no master", got, kindBridge)
	}
}

// TestEnsureBridgeNoOpWhenExists pins idempotency: an existing link with the
// bridge name is left untouched, whatever its kind. The existence check is
// by name only.
func TestEnsureBridgeNoOpWhenExists(t *testing.T) {
	tests := []struct {
		name string
		kind string
	}{
		{name: "existing bridge", kind: kindBridge},
		{name: "existing non-bridge link with the bridge name", kind: "veth"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			seedLink(f, bridgeName, tt.kind, "")
			m := mustManager(t, f)

			if err := m.EnsureBridge(bridgeName); err != nil {
				t.Fatalf("EnsureBridge(%q) error: %v", bridgeName, err)
			}

			wantCalls(t, f, fakeCall{op: opByName, name: bridgeName})
		})
	}
}

// TestEnsureTapCreatesAndEnslaves pins the create path: an absent TAP is
// created with kind "tuntap" and immediately enslaved to the bridge, in
// that order.
func TestEnsureTapCreatesAndEnslaves(t *testing.T) {
	f := newFake()
	seedLink(f, bridgeName, kindBridge, "")
	m := mustManager(t, f)

	if err := m.EnsureTap(bridgeName, tapName); err != nil {
		t.Fatalf("EnsureTap(%q, %q) error: %v", bridgeName, tapName, err)
	}

	wantCalls(t, f,
		fakeCall{op: opByName, name: tapName},
		fakeCall{op: opAdd, kind: kindTap, name: tapName},
		fakeCall{op: opSetMaster, name: tapName, master: bridgeName},
	)

	got, err := f.LinkByName(tapName)
	if err != nil {
		t.Fatalf("LinkByName(%q) after create error: %v", tapName, err)
	}
	if got.Kind != kindTap || got.Master != bridgeName {
		t.Errorf("created TAP = %+v, want kind %q enslaved to %q", got, kindTap, bridgeName)
	}
}

// TestEnsureTapExistingEnslaveIfNeeded pins the idempotent converge path: an
// existing TAP is enslaved only when it is not already mastered to the
// bridge, and never recreated.
func TestEnsureTapExistingEnslaveIfNeeded(t *testing.T) {
	tests := []struct {
		name       string
		seedMaster string
		wantCalls  []fakeCall
		wantMaster string
	}{
		{
			name:       "not yet mastered",
			seedMaster: "",
			wantCalls: []fakeCall{
				{op: opByName, name: tapName},
				{op: opSetMaster, name: tapName, master: bridgeName},
			},
			wantMaster: bridgeName,
		},
		{
			name:       "already mastered to the bridge",
			seedMaster: bridgeName,
			wantCalls:  []fakeCall{{op: opByName, name: tapName}},
			wantMaster: bridgeName,
		},
		{
			name:       "mastered to a different bridge",
			seedMaster: "k8sbr9",
			wantCalls: []fakeCall{
				{op: opByName, name: tapName},
				{op: opSetMaster, name: tapName, master: bridgeName},
			},
			wantMaster: bridgeName,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			seedLink(f, bridgeName, kindBridge, "")
			seedLink(f, tapName, kindTap, tt.seedMaster)
			m := mustManager(t, f)

			if err := m.EnsureTap(bridgeName, tapName); err != nil {
				t.Fatalf("EnsureTap(%q, %q) error: %v", bridgeName, tapName, err)
			}

			wantCalls(t, f, tt.wantCalls...)

			got, err := f.LinkByName(tapName)
			if err != nil {
				t.Fatalf("LinkByName(%q) error: %v", tapName, err)
			}
			if got.Master != tt.wantMaster {
				t.Errorf("TAP master = %q, want %q", got.Master, tt.wantMaster)
			}
		})
	}
}

// TestDeleteTapIdempotent pins deletion of a machine TAP: the link is
// removed, and deleting a TAP that does not exist is tolerated (nil).
func TestDeleteTapIdempotent(t *testing.T) {
	tests := []struct {
		name string
		seed bool
	}{
		{name: "removes an existing TAP", seed: true},
		{name: "missing TAP is tolerated", seed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			if tt.seed {
				seedLink(f, tapName, kindTap, bridgeName)
			}
			m := mustManager(t, f)

			if err := m.DeleteTap(tapName); err != nil {
				t.Fatalf("DeleteTap(%q) error: %v", tapName, err)
			}

			wantCalls(t, f, fakeCall{op: opDel, name: tapName})

			// The TAP is gone after deletion whether it existed before
			// or not.
			if _, err := f.LinkByName(tapName); !errors.Is(err, networking.ErrLinkNotFound) {
				t.Errorf("LinkByName(%q) after delete = %v, want ErrLinkNotFound", tapName, err)
			}
		})
	}
}

// TestDeleteBridgeIdempotent pins deletion of the lab bridge: the link is
// removed, and deleting a bridge that does not exist is tolerated (nil).
func TestDeleteBridgeIdempotent(t *testing.T) {
	tests := []struct {
		name string
		seed bool
	}{
		{name: "removes an existing bridge", seed: true},
		{name: "missing bridge is tolerated", seed: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake()
			if tt.seed {
				seedLink(f, bridgeName, kindBridge, "")
			}
			m := mustManager(t, f)

			if err := m.DeleteBridge(bridgeName); err != nil {
				t.Fatalf("DeleteBridge(%q) error: %v", bridgeName, err)
			}

			wantCalls(t, f, fakeCall{op: opDel, name: bridgeName})

			// The bridge is gone after deletion whether it existed
			// before or not.
			if _, err := f.LinkByName(bridgeName); !errors.Is(err, networking.ErrLinkNotFound) {
				t.Errorf("LinkByName(%q) after delete = %v, want ErrLinkNotFound", bridgeName, err)
			}
		})
	}
}

// TestErrorPropagation pins that real failures are returned unchanged: the
// Manager must neither swallow nor retry them.
func TestErrorPropagation(t *testing.T) {
	t.Run("bridge create fails", func(t *testing.T) {
		f := newFake()
		f.failBy[failAdd(kindBridge)] = errAddBridge
		m := mustManager(t, f)

		err := m.EnsureBridge(bridgeName)
		if !errors.Is(err, errAddBridge) {
			t.Fatalf("EnsureBridge(%q) error = %v, want %v", bridgeName, err, errAddBridge)
		}

		wantCalls(t, f,
			fakeCall{op: opByName, name: bridgeName},
			fakeCall{op: opAdd, kind: kindBridge, name: bridgeName},
		)
	})

	t.Run("tap create fails before enslaving", func(t *testing.T) {
		f := newFake()
		seedLink(f, bridgeName, kindBridge, "")
		f.failBy[failAdd(kindTap)] = errAddTap
		m := mustManager(t, f)

		err := m.EnsureTap(bridgeName, tapName)
		if !errors.Is(err, errAddTap) {
			t.Fatalf("EnsureTap(%q, %q) error = %v, want %v", bridgeName, tapName, err, errAddTap)
		}

		// The failed create must not be followed by an enslave attempt.
		wantCalls(t, f,
			fakeCall{op: opByName, name: tapName},
			fakeCall{op: opAdd, kind: kindTap, name: tapName},
		)
	})

	t.Run("enslave fails", func(t *testing.T) {
		f := newFake()
		seedLink(f, bridgeName, kindBridge, "")
		seedLink(f, tapName, kindTap, "")
		f.failBy[opSetMaster] = errSetMaster
		m := mustManager(t, f)

		err := m.EnsureTap(bridgeName, tapName)
		if !errors.Is(err, errSetMaster) {
			t.Fatalf("EnsureTap(%q, %q) error = %v, want %v", bridgeName, tapName, err, errSetMaster)
		}

		wantCalls(t, f,
			fakeCall{op: opByName, name: tapName},
			fakeCall{op: opSetMaster, name: tapName, master: bridgeName},
		)
	})

	t.Run("enslave fails when the bridge is missing", func(t *testing.T) {
		f := newFake()
		m := mustManager(t, f)

		err := m.EnsureTap(bridgeName, tapName)
		if !errors.Is(err, errMasterGone) {
			t.Fatalf("EnsureTap(%q, %q) without a bridge error = %v, want %v", bridgeName, tapName, err, errMasterGone)
		}

		wantCalls(t, f,
			fakeCall{op: opByName, name: tapName},
			fakeCall{op: opAdd, kind: kindTap, name: tapName},
			fakeCall{op: opSetMaster, name: tapName, master: bridgeName},
		)
	})

	t.Run("lookup error propagates without creating", func(t *testing.T) {
		for _, tt := range []struct {
			name string
			key  string
			run  func(*networking.Manager) error
		}{
			{
				name: "EnsureBridge",
				key:  bridgeName,
				run: func(m *networking.Manager) error {
					return m.EnsureBridge(bridgeName)
				},
			},
			{
				name: "EnsureTap",
				key:  tapName,
				run: func(m *networking.Manager) error {
					return m.EnsureTap(bridgeName, tapName)
				},
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFake()
				f.failBy[opByName] = errLookup
				m := mustManager(t, f)

				if err := tt.run(m); !errors.Is(err, errLookup) {
					t.Fatalf("error = %v, want %v", err, errLookup)
				}

				// A failed lookup must not be followed by a create.
				wantCalls(t, f, fakeCall{op: opByName, name: tt.key})
			})
		}
	})

	t.Run("delete error propagates", func(t *testing.T) {
		// The delete path must not swallow a real deletion failure.
		for _, tt := range []struct {
			name   string
			seed   func(*fakeLinkOps)
			delete func(*networking.Manager) error
		}{
			{
				name: "DeleteTap",
				seed: func(f *fakeLinkOps) { seedLink(f, tapName, kindTap, bridgeName) },
				delete: func(m *networking.Manager) error {
					return m.DeleteTap(tapName)
				},
			},
			{
				name: "DeleteBridge",
				seed: func(f *fakeLinkOps) { seedLink(f, bridgeName, kindBridge, "") },
				delete: func(m *networking.Manager) error {
					return m.DeleteBridge(bridgeName)
				},
			},
		} {
			t.Run(tt.name, func(t *testing.T) {
				f := newFake()
				tt.seed(f)
				f.failBy[opDel] = errDelete
				m := mustManager(t, f)

				if err := tt.delete(m); !errors.Is(err, errDelete) {
					t.Fatalf("delete error = %v, want %v", err, errDelete)
				}
			})
		}
	})
}

// TestEnsureTapRecoversAfterFailedEnslave pins reconcile convergence: when
// the first EnsureTap call creates the TAP but fails to enslave it (the
// bridge does not exist yet), the next EnsureTap call after the bridge is
// ensured finds the TAP, enslaves it, and does not recreate it.
func TestEnsureTapRecoversAfterFailedEnslave(t *testing.T) {
	f := newFake()
	m := mustManager(t, f)

	// First attempt: the bridge is missing, so enslaving fails after the
	// TAP was created. The error is surfaced.
	if err := m.EnsureTap(bridgeName, tapName); !errors.Is(err, errMasterGone) {
		t.Fatalf("EnsureTap(%q, %q) without a bridge error = %v, want %v", bridgeName, tapName, err, errMasterGone)
	}

	// The cluster controller ensures the bridge.
	if err := m.EnsureBridge(bridgeName); err != nil {
		t.Fatalf("EnsureBridge(%q) error: %v", bridgeName, err)
	}

	// Second attempt converges: the existing TAP is enslaved, not recreated.
	if err := m.EnsureTap(bridgeName, tapName); err != nil {
		t.Fatalf("EnsureTap(%q, %q) after bridge create error: %v", bridgeName, tapName, err)
	}

	wantCalls(t, f,
		fakeCall{op: opByName, name: tapName},
		fakeCall{op: opAdd, kind: kindTap, name: tapName},
		fakeCall{op: opSetMaster, name: tapName, master: bridgeName},
		fakeCall{op: opByName, name: bridgeName},
		fakeCall{op: opAdd, kind: kindBridge, name: bridgeName},
		fakeCall{op: opByName, name: tapName},
		fakeCall{op: opSetMaster, name: tapName, master: bridgeName},
	)
}
