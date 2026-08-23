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

// cloud-init DHCP contract — test-first, RED.
//
// REQ-004 / VC-04: network-config must render DHCP (dhcp4: true) instead of
// static addresses/gateway4. The IP/Gateway/DNS fields leave Data or become
// unused. The guest obtains its address from k8netd DHCP, not from cloud-init.
//
// Grill cases:
//   - Render succeeds without IP/Gateway/DNS (they are no longer required)
//   - network-config contains dhcp4:true and no static addresses/gateway4
//   - version:2, ethernets/id0/match/driver virtio_net preserved
//   - still valid YAML, still produces user-data and meta-data unchanged
//   - empty instance id / hostname / ssh key still errors
//   - Data struct no longer has IP/Gateway/DNS (or they are ignored)
//
// This file is RED: current Render requires IP/Gateway/DNS and emits static
// addresses/gateway4, so every DHCP assertion fails until TASK-008 rewires it.

package cloudinit_test

import (
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
)

func dhcpValidData() cloudinit.Data {
	// Minimal DHCP-mode data: only identity and SSH key. Static fields absent
	// or empty. The future Data struct may not have IP/Gateway/DNS at all;
	// this helper sets only the fields that remain via reflection so it
	// compiles both before and after the struct change.
	d := cloudinit.Data{
		InstanceID:   "cah-lab-cp1",
		Hostname:     "cp1",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForContractTests",
	}
	// If IP/Gateway/DNS fields still exist, keep them empty to test that they
	// are not required.
	rv := reflect.ValueOf(&d).Elem()
	for _, name := range []string{"IP", "Gateway", "DNS"} {
		if f := rv.FieldByName(name); f.IsValid() && f.CanSet() && f.Kind() == reflect.String {
			f.SetString("")
		}
	}
	return d
}

// TestCloudInit_DataStructHasNoStaticFields asserts IP/Gateway/DNS are gone.
func TestCloudInit_DataStructHasNoStaticFields(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeOf(cloudinit.Data{})
	for _, name := range []string{"IP", "Gateway", "DNS"} {
		if _, ok := rt.FieldByName(name); ok {
			t.Errorf("cloudinit.Data still has field %q; REQ-004 requires it removed or unused in DHCP mode", name)
		}
	}
}

// TestCloudInit_RenderSucceedsWithoutStaticNetworking asserts Render succeeds when IP/Gateway/DNS are empty.
func TestCloudInit_RenderSucceedsWithoutStaticNetworking(t *testing.T) {
	d := dhcpValidData()
	parts, err := cloudinit.Render(d)
	if err != nil {
		t.Fatalf(
			"cloudinit.Render with DHCP-mode data (no IP/Gateway/DNS) returned error: %v (want success, static fields must not be required)",
			err,
		)
	}
	if parts == nil {
		t.Fatal("cloudinit.Render returned nil parts with DHCP-mode data")
	}
	if len(parts) != 3 {
		t.Errorf("parts count = %d, want 3 (user-data, meta-data, network-config)", len(parts))
	}
}

// TestCloudInit_NetworkConfigRendersDHCP asserts dhcp4:true and no static addressing.
func TestCloudInit_NetworkConfigRendersDHCP(t *testing.T) {
	parts, err := cloudinit.Render(dhcpValidData())
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	nc := string(parts["network-config"])
	if nc == "" {
		t.Fatal("network-config empty")
	}
	lower := strings.ToLower(nc)
	if !strings.Contains(lower, "dhcp4") {
		t.Fatalf("network-config missing dhcp4 key; got:\n%s\nwant dhcp4:true", nc)
	}
	if !strings.Contains(lower, "true") {
		t.Errorf("network-config has dhcp4 but not true; got:\n%s", nc)
	}
	// Must not contain static addressing keys.
	for _, forbid := range []string{"addresses:", "gateway4", "gateway:"} {
		// Allow "addresses" only inside nameservers? But after DHCP, static addresses should be absent entirely.
		// The DHCP config should have no addresses list for id0.
		// We check that no CIDR-like address is present.
		if strings.Contains(nc, forbid) {
			// Check more precisely: if it contains a CIDR-like pattern, fail.
			// For now, any addresses/gateway4 presence is a failure for DHCP mode.
			// Exception: nameservers addresses may still be allowed? In DHCP mode, DNS is via DHCP, so nameservers should also be absent.
			// So we fail on any static key.
			t.Errorf("network-config must not contain %q in DHCP mode; got:\n%s", forbid, nc)
		}
	}
	// Also must not contain the old static IP literal if it was present via test data.
	if strings.Contains(nc, "192.168.124") {
		t.Errorf("network-config in DHCP mode must not contain static IP 192.168.124.* ; got:\n%s", nc)
	}
}

// TestCloudInit_NetworkConfigParsesAsDHCPStruct asserts YAML parses and has expected structure.
func TestCloudInit_NetworkConfigParsesAsDHCPStruct(t *testing.T) {
	parts, err := cloudinit.Render(dhcpValidData())
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(parts["network-config"], &doc); err != nil {
		t.Fatalf("network-config not valid YAML: %v\n%s", err, string(parts["network-config"]))
	}
	// version: 2
	if doc["version"] != 2 && doc["version"] != float64(2) && doc["version"] != "2" {
		t.Errorf("network-config version = %v, want 2; doc=%v", doc["version"], doc)
	}
	eths, ok := doc["ethernets"].(map[string]any)
	if !ok {
		t.Fatalf("network-config ethernets missing or not a map: %v", doc["ethernets"])
	}
	id0, ok := eths["id0"].(map[string]any)
	if !ok {
		t.Fatalf("ethernets.id0 missing: %v", eths)
	}
	// dhcp4 must be true
	if v, ok := id0["dhcp4"]; !ok {
		t.Errorf("ethernets.id0.dhcp4 missing; want true; id0=%v", id0)
	} else if v != true {
		t.Errorf("ethernets.id0.dhcp4 = %v, want true", v)
	}
	// addresses must be absent
	if _, ok := id0["addresses"]; ok {
		t.Errorf("ethernets.id0.addresses present in DHCP mode, want absent; id0=%v", id0)
	}
	if _, ok := id0["gateway4"]; ok {
		t.Errorf("ethernets.id0.gateway4 present in DHCP mode, want absent; id0=%v", id0)
	}
	// match.driver virtio_net still present.
	m, ok := id0["match"].(map[string]any)
	if !ok {
		t.Fatalf("ethernets.id0.match missing: %v", id0)
	}
	if m["driver"] != "virtio_net" {
		t.Errorf("ethernets.id0.match.driver = %v, want virtio_net", m["driver"])
	}
}

// TestCloudInit_NetworkConfigRendersDHCPNoGatewayOrNameservers asserts no gateway4 or nameservers in DHCP.
func TestCloudInit_NetworkConfigRendersDHCPNoGatewayOrNameservers(t *testing.T) {
	parts, err := cloudinit.Render(dhcpValidData())
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	nc := string(parts["network-config"])
	var doc map[string]any
	if err := yaml.Unmarshal([]byte(nc), &doc); err != nil {
		t.Fatalf("yaml unmarshal: %v", err)
	}
	eths := doc["ethernets"].(map[string]any)
	id0 := eths["id0"].(map[string]any)
	if _, ok := id0["gateway4"]; ok {
		t.Errorf("gateway4 should be absent in DHCP mode")
	}
	if _, ok := id0["nameservers"]; ok {
		t.Errorf("nameservers should be absent in DHCP mode (DHCP provides DNS)")
	}
}

// TestCloudInit_UserDataAndMetaDataUnchanged ensures other parts still render correctly with DHCP data.
func TestCloudInit_UserDataAndMetaDataUnchanged(t *testing.T) {
	d := dhcpValidData()
	parts, err := cloudinit.Render(d)
	if err != nil {
		t.Fatalf("Render error: %v", err)
	}
	ud := string(parts["user-data"])
	if !strings.HasPrefix(ud, "#cloud-config") {
		t.Errorf("user-data missing #cloud-config header; got:\n%s", ud)
	}
	if !strings.Contains(ud, d.SSHPublicKey) {
		t.Errorf("user-data missing ssh public key; got:\n%s", ud)
	}
	md := string(parts["meta-data"])
	if !strings.Contains(md, d.InstanceID) {
		t.Errorf("meta-data missing instance-id %q; got %q", d.InstanceID, md)
	}
	if !strings.Contains(md, d.Hostname) {
		t.Errorf("meta-data missing hostname %q; got %q", d.Hostname, md)
	}
}

// TestCloudInit_RenderStillRejectsEmptyIdentity asserts empty identity fields still error even in DHCP mode.
func TestCloudInit_RenderStillRejectsEmptyIdentity(t *testing.T) {
	base := dhcpValidData()
	tests := []struct {
		name string
		mut  func(*cloudinit.Data)
	}{
		{name: "empty instance id", mut: func(d *cloudinit.Data) { d.InstanceID = "" }},
		{name: "empty hostname", mut: func(d *cloudinit.Data) { d.Hostname = "" }},
		{name: "empty ssh public key", mut: func(d *cloudinit.Data) { d.SSHPublicKey = "" }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			d := base
			tt.mut(&d)
			parts, err := cloudinit.Render(d)
			if err == nil {
				t.Fatalf("Render with %s succeeded, want error; parts=%v", tt.name, parts)
			}
			if parts != nil {
				t.Errorf("Render returned non-nil parts on error: %v", parts)
			}
		})
	}
}

// TestCloudInit_RenderWithStaticIPIgnored ensures if IP is passed alongside DHCP, it does not leak into output.
func TestCloudInit_RenderWithStaticIPIgnored(t *testing.T) {
	d := dhcpValidData()
	// Try to set IP if field exists — it should be ignored.
	rv := reflect.ValueOf(&d).Elem()
	if f := rv.FieldByName("IP"); f.IsValid() && f.CanSet() {
		f.SetString("192.168.124.99")
	}
	if f := rv.FieldByName("Gateway"); f.IsValid() && f.CanSet() {
		f.SetString("192.168.124.1")
	}
	if f := rv.FieldByName("DNS"); f.IsValid() && f.CanSet() {
		f.SetString("192.168.124.1")
	}
	parts, err := cloudinit.Render(d)
	if err != nil {
		t.Fatalf("Render error with stray static fields: %v", err)
	}
	nc := string(parts["network-config"])
	if strings.Contains(nc, "192.168.124.99") || strings.Contains(nc, "addresses:") || strings.Contains(nc, "gateway4") {
		t.Errorf("network-config leaked static IP despite DHCP mode; got:\n%s", nc)
	}
	if !strings.Contains(strings.ToLower(nc), "dhcp4") {
		t.Errorf("network-config missing dhcp4 after static fields ignored; got:\n%s", nc)
	}
}
