// Package cloudinit_test pins the contract for the CIDATA renderer that backs
// the HypervisorMachine's cloud-init disk.
//
// The renderer consumes one Data struct and produces the three cloud-init
// NoCloud parts the machine controller writes to the CIDATA disk:
//
//   - user-data (a #cloud-config document): the SSH public key injected into
//     the root user's ssh_authorized_keys, plus first-boot runcmd entries for
//     the root-resize helper and confext activation (copy the .raw images into
//     /var/lib/confexts/ and run systemd-confext refresh);
//   - meta-data: instance identity (instance-id and local-hostname);
//   - network-config (cloud-init network-config v2): the machine's static
//     address, gateway, and DNS nameserver for the primary interface.
//
// Render is all-or-nothing: it returns an error on any empty input field and
// never returns a partial set of parts.
package cloudinit_test

import (
	"fmt"
	"sort"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
)

func validData() cloudinit.Data {
	return cloudinit.Data{
		InstanceID:   "cah-lab-cp1",
		Hostname:     "cp1",
		SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIFakeKeyForContractTests",
	}
}

func keysOf(parts map[string][]byte) []string {
	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}
	sort.Strings(keys)

	return keys
}

func TestRenderProducesCompleteCIDATA(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	wantKeys := []string{"meta-data", "network-config", "user-data"}
	if got := keysOf(parts); !equalStrings(got, wantKeys) {
		t.Fatalf("Render produced parts %v, want exactly %v", got, wantKeys)
	}
	for _, key := range wantKeys {
		if len(parts[key]) == 0 {
			t.Errorf("part %q rendered empty", key)
		}
	}
}

func TestRenderUserDataInjectsSSHKeyIntoRootUser(t *testing.T) {
	data := validData()

	parts, err := cloudinit.Render(data)
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	userData := parts["user-data"]
	if !strings.HasPrefix(string(userData), "#cloud-config\n") {
		t.Errorf("user-data must start with the #cloud-config header, got:\n%s", userData)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(userData, &doc); err != nil {
		t.Fatalf("user-data is not valid YAML: %v", err)
	}

	users, ok := doc["users"].([]any)
	if !ok {
		t.Fatalf("user-data has no users list: %#v", doc["users"])
	}
	for _, entry := range users {
		user, ok := entry.(map[string]any)
		if !ok {
			continue // the convenience "default" entry renders as a scalar
		}
		if user["name"] != "root" {
			continue
		}

		keys, ok := user["ssh_authorized_keys"].([]any)
		if !ok {
			t.Fatalf("root user has no ssh_authorized_keys list: %#v", user)
		}
		for _, key := range keys {
			if key == data.SSHPublicKey {
				return
			}
		}
		t.Errorf("root user ssh_authorized_keys does not contain the public key: %#v", keys)
	}
	t.Fatal("user-data defines no root user with ssh_authorized_keys")
}

func TestRenderUserDataFirstBootRuncmd(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(parts["user-data"], &doc); err != nil {
		t.Fatalf("user-data is not valid YAML: %v", err)
	}

	runcmd, ok := doc["runcmd"].([]any)
	if !ok || len(runcmd) == 0 {
		t.Fatalf("user-data has no runcmd entries: %#v", doc["runcmd"])
	}

	var joined strings.Builder
	for _, entry := range runcmd {
		switch value := entry.(type) {
		case string:
			joined.WriteString(value)
		case []any:
			fmt.Fprintf(&joined, "%v", value)
		default:
			t.Errorf("unexpected runcmd entry type %T: %#v", entry, entry)
		}
		joined.WriteByte('\n')
	}

	commands := joined.String()
	for _, want := range []string{
		"/usr/local/sbin/resize-rootfs.sh", // first-boot root-resize helper
		".raw",                             // copy the confext images onto the node
		"/var/lib/confexts",                // confext activation target directory
		"systemd-confext refresh",          // merge the .raw images into the image
	} {
		if !strings.Contains(commands, want) {
			t.Errorf("runcmd does not cover %q; runcmd:\n%s", want, commands)
		}
	}
}

func TestRenderMetaDataPinsIdentity(t *testing.T) {
	data := validData()

	parts, err := cloudinit.Render(data)
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	want := "instance-id: " + data.InstanceID + "\n" +
		"local-hostname: " + data.Hostname + "\n"
	if got := string(parts["meta-data"]); got != want {
		t.Errorf("meta-data mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderNetworkConfigPinsStaticAddressing(t *testing.T) {
	data := validData()

	parts, err := cloudinit.Render(data)
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	// After DHCP rewiring (REQ-004), network-config is DHCP, not static.
	want := "version: 2\n" +
		"ethernets:\n" +
		"  id0:\n" +
		"    match:\n" +
		"      driver: virtio_net\n" +
		"    dhcp4: true\n"
	if got := string(parts["network-config"]); got != want {
		t.Errorf("network-config mismatch:\n got: %q\nwant: %q", got, want)
	}
}

func TestRenderedPartsParseAsYAML(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		t.Run(name, func(t *testing.T) {
			body := parts[name]
			if len(body) == 0 {
				t.Fatal("part rendered empty")
			}

			var doc map[string]any
			if err := yaml.Unmarshal(body, &doc); err != nil {
				t.Fatalf("%s is not valid YAML: %v", name, err)
			}
			if doc == nil {
				t.Fatalf("%s parsed to a nil document", name)
			}
		})
	}
}

func TestRenderRejectsEmptyInputs(t *testing.T) {
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
			data := validData()
			tt.mut(&data)

			parts, err := cloudinit.Render(data)
			if err == nil {
				t.Fatal("Render accepted data with an empty field")
			}
			if parts != nil {
				t.Errorf("Render returned partial parts on error: %v", parts)
			}
		})
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}

	return true
}
