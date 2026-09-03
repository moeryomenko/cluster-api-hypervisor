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

// Package config resolves the provider configuration from environment
// variables. The quadlet unit that launches the provider passes host-specific
// paths and network settings as environment variables; this package turns
// that environment into a single Config value, applying an exact default for
// every variable that is unset or empty — except
// HYPERVISOR_SSH_PUBLIC_KEY_FILE, which has no default and stays empty when
// unset.
package config

import (
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
)

// Config is the resolved provider configuration. Each field names the
// resource it locates; values come from the HYPERVISOR_* environment
// variables described on the fields' defaults.
type Config struct {
	// BaseImage is the guest OS disk image the hypervisor boots.
	BaseImage string

	// Firmware is the firmware (OVMF-style) blob passed to the hypervisor.
	Firmware string

	// VMDiskDir is the directory holding per-machine VM disks.
	VMDiskDir string

	// SocketDir is the directory for the hypervisor control sockets.
	SocketDir string

	// StateDir is the provider state directory on the lab host.
	StateDir string

	// CHBinary is the cloud-hypervisor binary used to launch machines.
	CHBinary string

	// QemuImg is the qemu-img binary used to create and convert VM disks.
	QemuImg string

	// K8NetdSocket is the Unix control socket for the k8netd network daemon.
	K8NetdSocket string

	// NetworkCIDR is the IPv4 network served on the lab bridge.
	NetworkCIDR string

	// SSHPublicKeyFile is the path inside the provider container holding the
	// SSH public key for machines whose HypervisorConfig leaves
	// spec.sshPublicKey empty. Unlike every other variable it has no default:
	// unset means the file fallback is off.
	SSHPublicKeyFile string
}

// Default values used when the corresponding HYPERVISOR_* environment
// variable is unset or set to an empty string.
const (
	defaultBaseImage    = "build/k8labs-base.qcow2"
	defaultFirmware     = "build/CLOUDHV.fd"
	defaultVMDiskDir    = "build/vm-disks"
	defaultSocketDir    = "/tmp/ch-capi"
	defaultCHBinary     = "cloud-hypervisor"
	defaultQemuImg      = "qemu-img"
	defaultK8NetdSocket = "/run/user/1000/k8snet/control.sock"
	defaultNetworkCIDR  = "192.168.124.0/24"
)

// defaultStateDir returns the user-writable default for StateDir. It
// resolves to $HOME/.local/state/k8slab when HOME is set, otherwise falls
// back to /tmp/k8slab-state so the provider remains runnable in minimal
// environments. The result always contains "k8slab" and ".local/state" or
// "/tmp" and never starts with "/var/lib/".
func defaultStateDir() string {
	if home := os.Getenv("HOME"); home != "" {
		return filepath.Join(home, ".local", "state", "k8slab")
	}

	if xdg := os.Getenv("XDG_STATE_HOME"); xdg != "" {
		return filepath.Join(xdg, "k8slab")
	}

	return "/tmp/k8slab-state"
}

// Load resolves the provider configuration from the supplied environment
// lookup. A nil env function behaves as if every variable were unset. The
// lookup is called only with the exact HYPERVISOR_* variable names; an empty
// result is treated the same as an unset variable and falls back to the
// default. The single exception is HYPERVISOR_SSH_PUBLIC_KEY_FILE, which has
// no default and stays empty when unset. Load returns a non-nil error when
// HYPERVISOR_NETWORK_CIDR does not parse as an IPv4 network. No other
// validation is performed: paths are not required to exist and binaries are
// not required to be on the host PATH.
func Load(env func(string) string) (Config, error) {
	if env == nil {
		env = func(string) string { return "" }
	}

	cfg := Config{
		BaseImage:        valueOrDefault(env("HYPERVISOR_BASE_IMAGE"), defaultBaseImage),
		Firmware:         valueOrDefault(env("HYPERVISOR_FIRMWARE"), defaultFirmware),
		VMDiskDir:        valueOrDefault(env("HYPERVISOR_VM_DISKS_DIR"), defaultVMDiskDir),
		SocketDir:        valueOrDefault(env("HYPERVISOR_SOCKET_DIR"), defaultSocketDir),
		StateDir:         valueOrDefault(env("HYPERVISOR_STATE_DIR"), defaultStateDir()),
		CHBinary:         valueOrDefault(env("HYPERVISOR_CH_BINARY"), defaultCHBinary),
		QemuImg:          valueOrDefault(env("HYPERVISOR_QEMU_IMG"), defaultQemuImg),
		K8NetdSocket:     valueOrDefault(env("HYPERVISOR_K8NETD_SOCKET"), defaultK8NetdSocket),
		NetworkCIDR:      valueOrDefault(env("HYPERVISOR_NETWORK_CIDR"), defaultNetworkCIDR),
		SSHPublicKeyFile: env("HYPERVISOR_SSH_PUBLIC_KEY_FILE"),
	}

	if err := validateNetworkCIDR(cfg.NetworkCIDR); err != nil {
		return Config{}, err
	}

	return cfg, nil
}

// valueOrDefault returns value when it is non-empty and fallback otherwise.
func valueOrDefault(value, fallback string) string {
	if value == "" {
		return fallback
	}

	return value
}

// validateNetworkCIDR checks that cidr parses as an IPv4 network.
func validateNetworkCIDR(cidr string) error {
	prefix, err := netip.ParsePrefix(cidr)
	if err != nil {
		return fmt.Errorf("invalid network CIDR %q: %w", cidr, err)
	}

	if !prefix.Addr().Is4() {
		return fmt.Errorf("invalid network CIDR %q: not an IPv4 network", cidr)
	}

	return nil
}
