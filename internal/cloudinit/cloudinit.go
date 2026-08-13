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

// Package cloudinit renders the cloud-init NoCloud CIDATA parts — user-data,
// meta-data and network-config — that the HypervisorMachine controller writes
// to the machine's CIDATA disk. user-data injects the SSH public key into the
// root user and carries the first-boot runcmd entries for the root-resize
// helper and confext activation; meta-data pins the instance identity;
// network-config carries the machine's static address, gateway and DNS
// nameserver for the primary virtio interface.
package cloudinit

import "fmt"

// Data carries the per-machine values the CIDATA renderer needs: the instance
// identity, the SSH public key to inject into root, and the static network
// addressing (IP, gateway, DNS) allocated for the machine.
type Data struct {
	InstanceID   string
	Hostname     string
	SSHPublicKey string
	IP           string
	Gateway      string
	DNS          string
}

// Render produces the three cloud-init NoCloud parts for one machine. The
// returned map contains exactly the keys "user-data", "meta-data", and
// "network-config". Rendering is all-or-nothing: any empty field in d yields
// a non-nil error and a nil map, never a partial set of parts.
func Render(d Data) (map[string][]byte, error) {
	for _, field := range []struct{ name, value string }{
		{name: "instance id", value: d.InstanceID},
		{name: "hostname", value: d.Hostname},
		{name: "ssh public key", value: d.SSHPublicKey},
		{name: "ip", value: d.IP},
		{name: "gateway", value: d.Gateway},
		{name: "dns", value: d.DNS},
	} {
		if field.value == "" {
			return nil, fmt.Errorf("cloudinit: %s must not be empty", field.name)
		}
	}

	userData := "#cloud-config\n" +
		"users:\n" +
		"  - default\n" +
		"  - name: root\n" +
		"    ssh_authorized_keys:\n" +
		"      - " + d.SSHPublicKey + "\n" +
		"ssh_pwauth: false\n" +
		"disable_root: false\n" +
		"\n" +
		"# Grow the root partition/LVM to the disk once on first boot, then\n" +
		"# activate the confext images carried on the confext data disk.\n" +
		"runcmd:\n" +
		"  - [ /usr/local/sbin/resize-rootfs.sh ]\n" +
		"  - [ bash, -c, \"mkdir -p /var/lib/confexts /mnt/confexts && mount /dev/vdb /mnt/confexts && cp /mnt/confexts/*.raw /var/lib/confexts/ && umount /mnt/confexts && systemd-confext refresh\" ]\n"

	metaData := fmt.Sprintf("instance-id: %s\nlocal-hostname: %s\n", d.InstanceID, d.Hostname)

	networkConfig := fmt.Sprintf("version: 2\n"+
		"ethernets:\n"+
		"  id0:\n"+
		"    match:\n"+
		"      driver: virtio_net\n"+
		"    addresses:\n"+
		"      - %s/24\n"+
		"    gateway4: %s\n"+
		"    nameservers:\n"+
		"      addresses:\n"+
		"        - %s\n", d.IP, d.Gateway, d.DNS)

	return map[string][]byte{
		"user-data":      []byte(userData),
		"meta-data":      []byte(metaData),
		"network-config": []byte(networkConfig),
	}, nil
}
