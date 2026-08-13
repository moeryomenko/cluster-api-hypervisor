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

// Package networking implements the provider's host networking layer: it owns
// the lab bridge and one TAP per machine over netlink, replacing the
// host-side systemd-networkd .netdev/.network files. The orchestration logic
// lives in Manager behind the injectable LinkOps seam, and the netlinkOps
// implementation drives the host kernel through vishvananda/netlink.
package networking

import (
	"errors"
	"fmt"

	"github.com/vishvananda/netlink"
)

// ErrLinkNotFound reports that a link with the requested name does not
// exist. Implementations may wrap it; callers match it with errors.Is.
var ErrLinkNotFound = errors.New("networking: link not found")

// Link is the minimal view of a kernel link the orchestration needs: its
// name, its kind ("bridge" for the lab bridge, "tuntap" for a machine TAP),
// and the name of the bridge it is enslaved to (empty when the link has no
// master). No other link attributes are part of the contract.
type Link struct {
	Name   string
	Kind   string
	Master string
}

// LinkOps is the injectable seam wrapping netlink. LinkByName returns the
// link with the given name, or ErrLinkNotFound when no such link exists; any
// other error means the lookup itself failed. LinkAdd creates a link of the
// given kind with the given name and is only ever called for a name that does
// not exist yet. LinkSetMaster enslaves the named link to the named master
// bridge. LinkDel removes the named link and returns ErrLinkNotFound when the
// link does not exist, so deletion can be idempotent.
type LinkOps interface {
	LinkByName(name string) (Link, error)
	LinkAdd(kind, name string) error
	LinkSetMaster(name, master string) error
	LinkDel(name string) error
}

// Manager orchestrates the lab bridge and per-machine TAPs over a LinkOps
// seam. Every operation is idempotent: bridges and TAPs are only created when
// absent, TAP enslavement converges to the requested bridge, and deletion
// tolerates a missing link.
type Manager struct {
	ops LinkOps
}

// NewManager builds an orchestrator over the given link operations seam.
func NewManager(ops LinkOps) *Manager {
	return &Manager{ops: ops}
}

// EnsureBridge creates the lab bridge with the given name when no link with
// that name exists yet and is a no-op when a link with that name already
// exists, whatever its kind: the existence check is by name only. A failed
// lookup or a failed create is returned unchanged.
func (m *Manager) EnsureBridge(name string) error {
	_, err := m.ops.LinkByName(name)
	if err == nil {
		return nil
	}
	if !errors.Is(err, ErrLinkNotFound) {
		return err
	}

	return m.ops.LinkAdd("bridge", name)
}

// EnsureTap creates the machine TAP with the given name and enslaves it to
// the bridge when the TAP does not exist yet. When the TAP already exists it
// is enslaved if it is not already mastered to the bridge and left alone if
// it is; a TAP mastered to a different bridge is re-enslaved. EnsureTap never
// creates the bridge, so enslaving to a missing bridge surfaces the
// LinkSetMaster error. A TAP left unenslaved by a failed attempt is recovered
// by the next call, which finds it and enslaves it.
func (m *Manager) EnsureTap(bridgeName, tapName string) error {
	l, err := m.ops.LinkByName(tapName)
	switch {
	case err == nil:
		if l.Master == bridgeName {
			return nil
		}
		return m.ops.LinkSetMaster(tapName, bridgeName)
	case !errors.Is(err, ErrLinkNotFound):
		return err
	}

	if err := m.ops.LinkAdd("tuntap", tapName); err != nil {
		return err
	}

	return m.ops.LinkSetMaster(tapName, bridgeName)
}

// DeleteTap removes the machine TAP with the given name. Deleting a TAP that
// does not exist is tolerated; any other deletion error is returned
// unchanged.
func (m *Manager) DeleteTap(name string) error {
	return m.deleteLink(name)
}

// DeleteBridge removes the lab bridge with the given name. Deleting a bridge
// that does not exist is tolerated; any other deletion error is returned
// unchanged.
func (m *Manager) DeleteBridge(name string) error {
	return m.deleteLink(name)
}

// deleteLink removes the named link, treating a missing link as already
// removed.
func (m *Manager) deleteLink(name string) error {
	err := m.ops.LinkDel(name)
	if errors.Is(err, ErrLinkNotFound) {
		return nil
	}

	return err
}

// netlinkOps is a LinkOps backed by vishvananda/netlink against the host
// kernel.
type netlinkOps struct{}

// NewNetlinkOps returns a LinkOps backed by the host kernel via
// vishvananda/netlink. It requires the host's network administrator
// privileges to create links and modify bridge membership.
func NewNetlinkOps() LinkOps {
	return netlinkOps{}
}

// LinkByName implements LinkOps: it wraps netlink.LinkByName and maps the
// netlink not-found error to ErrLinkNotFound.
func (netlinkOps) LinkByName(name string) (Link, error) {
	l, err := netlink.LinkByName(name)
	if err != nil {
		return Link{}, linkError(err)
	}

	link := Link{
		Name: l.Attrs().Name,
		Kind: l.Type(),
	}
	if masterIndex := l.Attrs().MasterIndex; masterIndex != 0 {
		master, err := netlink.LinkByIndex(masterIndex)
		if err != nil {
			return Link{}, err
		}
		link.Master = master.Attrs().Name
	}

	return link, nil
}

// LinkAdd implements LinkOps: it creates a link of the given kind with the
// given name. The supported kinds are "bridge" and "tuntap".
func (netlinkOps) LinkAdd(kind, name string) error {
	var link netlink.Link
	switch kind {
	case "bridge":
		link = &netlink.Bridge{LinkAttrs: netlink.LinkAttrs{Name: name}}
	case "tuntap":
		link = &netlink.Tuntap{
			LinkAttrs: netlink.LinkAttrs{Name: name},
			Mode:      netlink.TUNTAP_MODE_TAP,
		}
	default:
		return fmt.Errorf("networking: unsupported link kind %q", kind)
	}

	return netlink.LinkAdd(link)
}

// LinkSetMaster implements LinkOps: it enslaves the named link to the named
// master bridge.
func (netlinkOps) LinkSetMaster(name, master string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return linkError(err)
	}
	masterLink, err := netlink.LinkByName(master)
	if err != nil {
		return err
	}

	return netlink.LinkSetMaster(link, masterLink)
}

// LinkDel implements LinkOps: it removes the named link, reporting
// ErrLinkNotFound when the link does not exist.
func (netlinkOps) LinkDel(name string) error {
	link, err := netlink.LinkByName(name)
	if err != nil {
		return linkError(err)
	}

	return netlink.LinkDel(link)
}

// linkError maps a netlink lookup failure to ErrLinkNotFound when the link
// does not exist and returns the error unchanged otherwise.
func linkError(err error) error {
	var notFound netlink.LinkNotFoundError
	if errors.As(err, &notFound) {
		return ErrLinkNotFound
	}

	return err
}
