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

// The provider configuration contract (test-first).
//
// This suite pins how the provider reads its host-specific configuration
// from the environment. The quadlet unit that launches the provider passes
// these values as environment variables; there are no spec fields for host
// paths. The package under test resolves the environment into a single
// Config value and validates the parts that can be validated without
// touching the host.
//
// The contract, in prose:
//
//   - Load(env func(string) string) (Config, error) reads the provider
//     configuration from the supplied environment lookup. A nil env
//     function behaves as if every variable were unset. The lookup is
//     called only with the exact variable names below.
//
//   - Nine variables are consumed, each with an exact default that is used
//     when the variable is unset (or set to an empty string):
//
//     HYPERVISOR_BASE_IMAGE        -> build/k8labs-base.qcow2
//     HYPERVISOR_FIRMWARE          -> build/CLOUDHV.fd
//     HYPERVISOR_VM_DISKS_DIR      -> build/vm-disks
//     HYPERVISOR_SOCKET_DIR        -> /tmp/ch-capi
//     HYPERVISOR_STATE_DIR         -> /var/lib/k8slab
//     HYPERVISOR_CH_BINARY         -> cloud-hypervisor
//     HYPERVISOR_QEMU_IMG          -> qemu-img
//     HYPERVISOR_DNSMASQ           -> dnsmasq
//     HYPERVISOR_NETWORK_CIDR      -> 192.168.124.0/24
//
//   - When a variable is set to a non-empty value, that value replaces the
//     default in the corresponding Config field.
//
//   - Config fields are named after the resource they locate:
//     BaseImage, Firmware, VMDiskDir, SocketDir, StateDir, CHBinary,
//     QemuImg, Dnsmasq, NetworkCIDR.
//
//   - Load returns a non-nil error when HYPERVISOR_NETWORK_CIDR does not
//     parse as an IPv4 network (for example "not-a-cidr"). No other
//     validation is performed: paths are not required to exist and binaries
//     are not required to be on the host PATH at load time.
package config_test

import (
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
)

func loadConfig(t *testing.T, env map[string]string) config.Config {
	t.Helper()

	lookup := func(string) string {
		return ""
	}
	if env != nil {
		lookup = func(name string) string {
			return env[name]
		}
	}

	got, err := config.Load(lookup)
	if err != nil {
		t.Fatalf("config.Load() returned unexpected error: %v", err)
	}

	return got
}

func TestLoadDefaults(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		env  map[string]string
		want config.Config
	}{
		{
			name: "no variables set",
			env:  nil,
			want: config.Config{
				BaseImage:   "build/k8labs-base.qcow2",
				Firmware:    "build/CLOUDHV.fd",
				VMDiskDir:   "build/vm-disks",
				SocketDir:   "/tmp/ch-capi",
				StateDir:    "/var/lib/k8slab",
				CHBinary:    "cloud-hypervisor",
				QemuImg:     "qemu-img",
				Dnsmasq:     "dnsmasq",
				NetworkCIDR: "192.168.124.0/24",
			},
		},
		{
			name: "empty string is treated as unset",
			env: map[string]string{
				"HYPERVISOR_BASE_IMAGE":   "",
				"HYPERVISOR_FIRMWARE":     "",
				"HYPERVISOR_VM_DISKS_DIR": "",
				"HYPERVISOR_SOCKET_DIR":   "",
				"HYPERVISOR_STATE_DIR":    "",
				"HYPERVISOR_CH_BINARY":    "",
				"HYPERVISOR_QEMU_IMG":     "",
				"HYPERVISOR_DNSMASQ":      "",
				"HYPERVISOR_NETWORK_CIDR": "",
			},
			want: config.Config{
				BaseImage:   "build/k8labs-base.qcow2",
				Firmware:    "build/CLOUDHV.fd",
				VMDiskDir:   "build/vm-disks",
				SocketDir:   "/tmp/ch-capi",
				StateDir:    "/var/lib/k8slab",
				CHBinary:    "cloud-hypervisor",
				QemuImg:     "qemu-img",
				Dnsmasq:     "dnsmasq",
				NetworkCIDR: "192.168.124.0/24",
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := loadConfig(t, tt.env)

			if got != tt.want {
				t.Errorf("config.Load() mismatch:\n got: %#v\nwant: %#v", got, tt.want)
			}
		})
	}
}

func TestLoadOverrides(t *testing.T) {
	t.Parallel()

	overrides := map[string]string{
		"HYPERVISOR_BASE_IMAGE":   "/opt/hypervisor/base.qcow2",
		"HYPERVISOR_FIRMWARE":     "/opt/hypervisor/firmware.bin",
		"HYPERVISOR_VM_DISKS_DIR": "/srv/vm-disks",
		"HYPERVISOR_SOCKET_DIR":   "/run/ch-capi",
		"HYPERVISOR_STATE_DIR":    "/var/lib/ch-capi",
		"HYPERVISOR_CH_BINARY":    "/usr/local/bin/cloud-hypervisor",
		"HYPERVISOR_QEMU_IMG":     "/usr/bin/qemu-img",
		"HYPERVISOR_DNSMASQ":      "/usr/bin/dnsmasq",
		"HYPERVISOR_NETWORK_CIDR": "10.10.0.0/16",
	}

	want := config.Config{
		BaseImage:   "/opt/hypervisor/base.qcow2",
		Firmware:    "/opt/hypervisor/firmware.bin",
		VMDiskDir:   "/srv/vm-disks",
		SocketDir:   "/run/ch-capi",
		StateDir:    "/var/lib/ch-capi",
		CHBinary:    "/usr/local/bin/cloud-hypervisor",
		QemuImg:     "/usr/bin/qemu-img",
		Dnsmasq:     "/usr/bin/dnsmasq",
		NetworkCIDR: "10.10.0.0/16",
	}

	got := loadConfig(t, overrides)

	if got != want {
		t.Errorf("config.Load() mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestLoadNetworkCIDRValidation(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name string
		cidr string
	}{
		{name: "not a cidr", cidr: "not-a-cidr"},
		{name: "missing prefix length", cidr: "192.168.124.0"},
		{name: "prefix length out of range", cidr: "192.168.124.0/99"},
		{name: "octet out of range", cidr: "192.168.124.999/24"},
		{name: "empty string", cidr: ""},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			_, err := config.Load(func(name string) string {
				if name == "HYPERVISOR_NETWORK_CIDR" {
					return tt.cidr
				}
				return ""
			})
			if err == nil {
				t.Fatalf("config.Load() with network CIDR %q: expected error, got nil", tt.cidr)
			}

			if !strings.Contains(strings.ToLower(err.Error()), "cidr") {
				t.Errorf("config.Load() error %q does not mention the offending network CIDR", err)
			}
		})
	}
}

func TestLoadMixedDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"HYPERVISOR_BASE_IMAGE":   "/data/images/base.qcow2",
		"HYPERVISOR_STATE_DIR":    "/mnt/lab-state",
		"HYPERVISOR_NETWORK_CIDR": "10.20.0.0/24",
	}

	want := config.Config{
		BaseImage:   "/data/images/base.qcow2",
		Firmware:    "build/CLOUDHV.fd",
		VMDiskDir:   "build/vm-disks",
		SocketDir:   "/tmp/ch-capi",
		StateDir:    "/mnt/lab-state",
		CHBinary:    "cloud-hypervisor",
		QemuImg:     "qemu-img",
		Dnsmasq:     "dnsmasq",
		NetworkCIDR: "10.20.0.0/24",
	}

	got := loadConfig(t, env)

	if got != want {
		t.Errorf("config.Load() mismatch:\n got: %#v\nwant: %#v", got, want)
	}
}
