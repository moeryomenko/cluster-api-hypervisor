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

// Package ipam implements the per-cluster static IPv4 allocator that hands
// out machine addresses from the cluster's network pool. One Allocator is
// built per cluster from that cluster's CIDR, gateway, and allocatable pool
// bounds; allocators share no state, so two clusters never hand out the same
// address. Addresses are stable per machine (a key always maps to the same
// address), are freed on machine deletion, and the freed address is the next
// one handed out (first-fit). The gateway, the network address, and the
// broadcast address are never allocatable.
package ipam

import (
	"errors"
	"fmt"
	"net/netip"
)

// Allocator hands out static IPv4 addresses from one cluster's allocatable
// pool. The pool is the inclusive range [start, end] of the cluster CIDR;
// allocation is first-fit, stable per key, and releases make the freed
// address the next one allocated.
type Allocator struct {
	start   uint32 // first allocatable address, numeric form
	end     uint32 // last allocatable address, numeric form
	keyToIP map[string]netip.Addr
	ipToKey map[netip.Addr]string
}

// ErrPoolExhausted reports that the allocatable pool holds no free address.
var ErrPoolExhausted = errors.New("ipam: pool exhausted")

// NewAllocator constructs an Allocator for clusterCIDR with the allocatable
// pool startIP..endIP (inclusive) and the given gateway. The CIDR must be a
// valid IPv4 network; the gateway must be a valid IPv4 address inside the
// CIDR and outside the pool; the pool bounds must be valid IPv4 addresses
// inside the CIDR, must not be the network or broadcast address, and must be
// ordered with startIP not after endIP. A pool that includes the gateway is
// rejected, because the gateway address is never allocatable.
func NewAllocator(clusterCIDR, gateway, startIP, endIP string) (*Allocator, error) {
	prefix, err := netip.ParsePrefix(clusterCIDR)
	if err != nil {
		return nil, fmt.Errorf("parse cluster CIDR %q: %w", clusterCIDR, err)
	}
	if !prefix.Addr().Is4() {
		return nil, fmt.Errorf("cluster CIDR %q is not IPv4", clusterCIDR)
	}

	gw, err := netip.ParseAddr(gateway)
	if err != nil {
		return nil, fmt.Errorf("parse gateway %q: %w", gateway, err)
	}
	if !gw.Is4() || !prefix.Contains(gw) {
		return nil, fmt.Errorf("gateway %q is not an IPv4 address inside %q", gateway, clusterCIDR)
	}

	start, err := netip.ParseAddr(startIP)
	if err != nil {
		return nil, fmt.Errorf("parse pool start %q: %w", startIP, err)
	}
	end, err := netip.ParseAddr(endIP)
	if err != nil {
		return nil, fmt.Errorf("parse pool end %q: %w", endIP, err)
	}

	if !start.Is4() || !prefix.Contains(start) {
		return nil, fmt.Errorf("pool start %q is not an IPv4 address inside %q", startIP, clusterCIDR)
	}
	if !end.Is4() || !prefix.Contains(end) {
		return nil, fmt.Errorf("pool end %q is not an IPv4 address inside %q", endIP, clusterCIDR)
	}

	// The network and broadcast addresses are reserved by the network itself
	// and can never belong to a machine.
	network := addrToUint32(prefix.Masked().Addr())
	broadcast := addrToUint32(ipv4Broadcast(prefix))
	startN := addrToUint32(start)
	endN := addrToUint32(end)
	if startN == network || startN == broadcast {
		return nil, fmt.Errorf("pool start %q is the network or broadcast address of %q", startIP, clusterCIDR)
	}
	if endN == network || endN == broadcast {
		return nil, fmt.Errorf("pool end %q is the network or broadcast address of %q", endIP, clusterCIDR)
	}
	if startN > endN {
		return nil, fmt.Errorf("pool start %q is after pool end %q", startIP, endIP)
	}

	// The gateway is the host-side address; a pool that includes it would
	// hand the host's own address to a machine.
	gwN := addrToUint32(gw)
	if startN <= gwN && gwN <= endN {
		return nil, fmt.Errorf("pool %s..%s includes the gateway %s", startIP, endIP, gateway)
	}

	return &Allocator{
		start:   startN,
		end:     endN,
		keyToIP: make(map[string]netip.Addr),
		ipToKey: make(map[netip.Addr]string),
	}, nil
}

// Allocate returns the address for key, allocating the lowest free address in
// the pool when key holds none yet. Allocation is stable: the same key always
// gets the same address, and a repeated call for an already-allocated key
// returns the existing address without consuming a second one. The error is
// ErrPoolExhausted when every pool address is taken.
func (a *Allocator) Allocate(key string) (string, error) {
	if ip, ok := a.keyToIP[key]; ok {
		return ip.String(), nil
	}

	for v := a.start; v <= a.end; v++ {
		ip := addrFromUint32(v)
		if _, taken := a.ipToKey[ip]; taken {
			continue
		}
		a.keyToIP[key] = ip
		a.ipToKey[ip] = key
		return ip.String(), nil
	}

	return "", fmt.Errorf("%w for %q", ErrPoolExhausted, key)
}

// Release frees the address held by key so the next Allocate hands it out
// again, first free address winning. Releasing a key that holds no address is
// a no-op.
func (a *Allocator) Release(key string) {
	ip, ok := a.keyToIP[key]
	if !ok {
		return
	}
	delete(a.keyToIP, key)
	delete(a.ipToKey, ip)
}

// Reserve claims the specific address ip for key, for example to re-assert an
// address already recorded in machine status. A later Allocate for the same
// key returns the reserved address; other keys skip it. Reserving an address
// outside the pool or already held by another key returns an error. Reserving
// a different address while key already holds one moves the key to the new
// address and frees the old one.
func (a *Allocator) Reserve(key string, ip string) error {
	addr, err := netip.ParseAddr(ip)
	if err != nil {
		return fmt.Errorf("parse address %q: %w", ip, err)
	}
	if !addr.Is4() || !a.inPool(addr) {
		return fmt.Errorf("address %q is outside the allocatable pool", ip)
	}
	if owner, ok := a.ipToKey[addr]; ok && owner != key {
		return fmt.Errorf("address %q is already held by %q", ip, owner)
	}

	if old, ok := a.keyToIP[key]; ok && old != addr {
		delete(a.keyToIP, key)
		delete(a.ipToKey, old)
	}
	a.keyToIP[key] = addr
	a.ipToKey[addr] = key

	return nil
}

// inPool reports whether addr falls inside the allocatable range.
func (a *Allocator) inPool(addr netip.Addr) bool {
	v := addrToUint32(addr)
	return v >= a.start && v <= a.end
}

// ipv4Broadcast returns the broadcast address of an IPv4 prefix.
func ipv4Broadcast(prefix netip.Prefix) netip.Addr {
	network := addrToUint32(prefix.Masked().Addr())
	// Host bits of the mask, shifted out of the all-ones word. For a /32 the
	// shift count equals the width and yields zero, so the broadcast collapses
	// onto the network address as expected.
	hostMask := ^uint32(0) >> uint(prefix.Bits())
	return addrFromUint32(network | hostMask)
}

// addrToUint32 renders an IPv4 address as its big-endian 32-bit value.
func addrToUint32(addr netip.Addr) uint32 {
	b := addr.As4()
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

// addrFromUint32 renders a big-endian 32-bit value as an IPv4 address.
func addrFromUint32(v uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{byte(v >> 24), byte(v >> 16), byte(v >> 8), byte(v)})
}
