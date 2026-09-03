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

// joinedRuncmd extracts the runcmd entries of a rendered user-data document
// and joins them into one string, so tests can assert on the shell commands
// cloud-init will run. List-form entries (the [bash, -c, "..."] shape) are
// rendered with fmt so the inner command text is included.
func joinedRuncmd(t *testing.T, userData []byte) string {
	t.Helper()

	var doc map[string]any
	if err := yaml.Unmarshal(userData, &doc); err != nil {
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

	return joined.String()
}

// TestRenderUserDataFirstBootRuncmd pins the first-boot runcmd contract: the
// root-resize helper runs first, then every confext data disk is activated
// into /var/lib/confexts/ and systemd-confext merges them. The confext
// activation must match the packager output format: each confext disk is a
// squashfs of the confext TREE (etc/, extension-release.d/), so mounting it
// exposes the tree, never *.raw files — copying /mnt/confexts/*.raw would
// copy nothing. The runcmd must also iterate ALL confext disks (vdc, vdd,
// ...) because the CIDATA disk occupies /dev/vdb; a hardcoded /dev/vdb would
// mount the CIDATA disk and leave every confext inactive.
func TestRenderUserDataFirstBootRuncmd(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	commands := joinedRuncmd(t, parts["user-data"])

	// The first-boot root-resize helper stays.
	if !strings.Contains(commands, "/usr/local/sbin/resize-rootfs.sh") {
		t.Errorf("runcmd does not cover the root-resize helper; runcmd:\n%s", commands)
	}

	// The confext activation must not hardcode the first confext disk: the
	// CIDATA disk now occupies /dev/vdb, so /dev/vdb is the CIDATA disk, not
	// a confext.
	if strings.Contains(commands, "/dev/vdb") {
		t.Errorf("runcmd hardcodes /dev/vdb; it must iterate all confext disks (vdc, vdd, ...):\n%s", commands)
	}

	if !strings.Contains(commands, "/dev/vd") {
		t.Errorf("runcmd does not iterate the virtio block devices (/dev/vd*); runcmd:\n%s", commands)
	}

	// The CIDATA disk must be excluded from confext activation; the disk is
	// formatted with the CIDATA label, so the runcmd skips it by label.
	if !strings.Contains(commands, "CIDATA") {
		t.Errorf("runcmd does not skip the CIDATA disk by label; runcmd:\n%s", commands)
	}

	// The confext name is derived from the extension-release metadata inside
	// the mounted tree, so each image lands in /var/lib/confexts/ under a
	// name systemd-confext can match (image file <name>.raw or directory
	// <name>/).
	if !strings.Contains(commands, "extension-release") {
		t.Errorf("runcmd does not derive the confext name from extension-release; runcmd:\n%s", commands)
	}

	if !strings.Contains(commands, "/var/lib/confexts") {
		t.Errorf("runcmd does not target /var/lib/confexts; runcmd:\n%s", commands)
	}

	if !strings.Contains(commands, "systemd-confext refresh") {
		t.Errorf("runcmd does not run systemd-confext refresh; runcmd:\n%s", commands)
	}

	// The copy semantics must match the packager output: a confext disk is a
	// squashfs of the confext TREE, so the mounted tree contains etc/ and
	// extension-release.d/, never *.raw files. Copying *.raw from the mount
	// would copy nothing and leave the confexts inactive.
	if strings.Contains(commands, "*.raw") {
		t.Errorf(
			"runcmd copies *.raw from the mounted tree; the confext disk is a squashfs of the tree, not a directory of .raw files:\n%s",
			commands,
		)
	}
}

// TestRenderUserDataRuncmdActivatesAllConfextDisks pins requirement 3: the
// confext activation runcmd must iterate every confext data disk, not just
// the first one. The CIDATA disk occupies /dev/vdb after the fix, so a
// hardcoded /dev/vdb would mount the CIDATA disk and leave every confext
// inactive; the runcmd must walk /dev/vd* and skip the CIDATA disk by label.
func TestRenderUserDataRuncmdActivatesAllConfextDisks(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	commands := joinedRuncmd(t, parts["user-data"])

	if strings.Contains(commands, "/dev/vdb") {
		t.Errorf("runcmd hardcodes /dev/vdb; it must iterate all confext disks:\n%s", commands)
	}

	if !strings.Contains(commands, "for d in") {
		t.Errorf("runcmd has no loop over the confext disks; runcmd:\n%s", commands)
	}

	if !strings.Contains(commands, "/dev/vd") {
		t.Errorf("runcmd does not walk /dev/vd*; runcmd:\n%s", commands)
	}

	if !strings.Contains(commands, "CIDATA") {
		t.Errorf("runcmd does not exclude the CIDATA disk (label CIDATA); runcmd:\n%s", commands)
	}
}

// confextDevice models one virtio block device the first-boot confext
// activation loop sees: its path, the blkid LABEL and TYPE the loop can
// probe, and the extension-release name the mounted tree carries (empty when
// the tree has no extension-release.d — the root disk's case).
type confextDevice struct {
	path        string
	label       string
	fsType      string
	releaseName string
}

// simulateConfextRuncmd evaluates the confext-activation runcmd against the
// device model and returns the copy operations it would perform, keyed by
// device path. It implements the shell semantics of the constructs the
// renderer emits: the /dev/vd* loop, the CIDATA label skip, the
// extension-release glob (which degrades to the literal "*" when it does not
// match), and the copy to /var/lib/confexts/<name>.raw. The guards a correct
// fix adds are recognized: an explicit root-disk skip, a filesystem-type
// check, or a glob-match guard.
func simulateConfextRuncmd(commands string, devices []confextDevice) map[string]string {
	copied := make(map[string]string)

	for _, dev := range devices {
		if confextRuncmdSkips(commands, dev) {
			continue
		}

		name := dev.releaseName
		if name == "" {
			// The extension-release glob does not match; bash (without
			// nullglob) passes the literal glob to basename, so the name
			// degrades to "*".
			name = "*"
		}

		copied[dev.path] = "/var/lib/confexts/" + name + ".raw"
	}

	return copied
}

// confextRuncmdSkips reports whether the runcmd skips the device before the
// copy. The CIDATA label skip is always present; the root-disk exclusion is
// the behavior this suite pins, and a correct fix implements it with one of
// the recognized guards.
func confextRuncmdSkips(commands string, dev confextDevice) bool {
	// The CIDATA disk is skipped by its label.
	if dev.label == "CIDATA" && strings.Contains(commands, "CIDATA") {
		return true
	}
	// The root disk is skipped by name.
	if dev.path == "/dev/vda" && strings.Contains(commands, "/dev/vda") {
		return true
	}
	// The root disk's PARTITIONS (/dev/vda1, /dev/vda2, ...) are skipped by a
	// /dev/vda* prefix guard (e.g. `case "$d" in /dev/vda*) continue;; esac`).
	if strings.HasPrefix(dev.path, "/dev/vda") && rootDiskPrefixGuardPresent(commands) {
		return true
	}
	// Only squashfs disks are confexts; the root disk is ext4/xfs/btrfs.
	if strings.Contains(commands, "squashfs") && dev.fsType != "squashfs" {
		return true
	}
	// The copy is guarded on the extension-release glob matching.
	if dev.releaseName == "" && globGuardPresent(commands) {
		return true
	}

	return false
}

// rootDiskPrefixGuardPresent reports whether the runcmd excludes the whole
// root disk AND its partitions via a /dev/vda* prefix guard. The fix skips
// /dev/vda1/vda2/vda3 (which carry no extension-release and would otherwise be
// copied as a literal *.raw) with a case pattern such as
// `case "$d" in /dev/vda*) continue;; esac`.
func rootDiskPrefixGuardPresent(commands string) bool {
	return strings.Contains(commands, "/dev/vda*")
}

// globGuardPresent reports whether the runcmd requires the extension-release
// glob to match before copying: a conditional that skips the device when the
// derived name is the degraded "*" or when the glob file is absent.
func globGuardPresent(commands string) bool {
	return strings.Contains(commands, `"$name" = "*"`) ||
		strings.Contains(commands, `"$name" != "*"`) ||
		strings.Contains(commands, `"$name" = "extension-release.*"`) ||
		strings.Contains(commands, `-f /mnt/confexts/etc/extension-release.d/extension-release.*`) ||
		strings.Contains(commands, "compgen")
}

// TestRenderUserDataRuncmdSkipsRootDisk pins the root-disk exclusion: the
// first-boot confext activation loop walks every virtio block device
// (/dev/vd*), so it sees the root disk /dev/vda AND its partitions
// (/dev/vda1, /dev/vda2, /dev/vda3) alongside the CIDATA disk and the confext
// data disks. The root disk and its partitions carry no extension-release
// metadata, so a loop that copies unconditionally after the glob degrades
// the confext name to "*" and copies the entire root disk/partition to
// /var/lib/confexts/*.raw. The runcmd must exclude the root disk AND its
// partitions — by skipping /dev/vda and /dev/vda*, by processing only
// squashfs disks, or by requiring the extension-release glob to match before
// copying — while still copying every confext disk and skipping the CIDATA
// disk by label.
func TestRenderUserDataRuncmdSkipsRootDisk(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	commands := joinedRuncmd(t, parts["user-data"])

	devices := []confextDevice{
		{path: "/dev/vda", fsType: "ext4"},  // root disk: no extension-release
		{path: "/dev/vda1", fsType: "ext4"}, // root partition: no extension-release
		{path: "/dev/vda2", fsType: "ext4"}, // root partition: no extension-release
		{path: "/dev/vda3", fsType: "ext4"}, // root partition: no extension-release
		{path: "/dev/vdb", label: "CIDATA", fsType: "vfat"},
		{path: "/dev/vdc", fsType: "squashfs", releaseName: "z-kubelet-node1"},
	}
	copied := simulateConfextRuncmd(commands, devices)

	for _, root := range []string{"/dev/vda", "/dev/vda1", "/dev/vda2", "/dev/vda3"} {
		if dest, ok := copied[root]; ok {
			t.Errorf(
				"runcmd copies the root disk/partition %s to %q; the root disk and its partitions must be excluded from confext activation:\n%s",
				root,
				dest,
				commands,
			)
		}
	}

	if dest, ok := copied["/dev/vdb"]; ok {
		t.Errorf(
			"runcmd copies the CIDATA disk /dev/vdb to %q; the CIDATA disk must be skipped by label:\n%s",
			dest,
			commands,
		)
	}

	if got := copied["/dev/vdc"]; got != "/var/lib/confexts/z-kubelet-node1.raw" {
		t.Errorf(
			"runcmd copies the confext disk /dev/vdc to %q, want /var/lib/confexts/z-kubelet-node1.raw:\n%s",
			got,
			commands,
		)
	}
}

// TestRenderUserDataRuncmdSkipsDiskWithoutExtensionRelease pins requirement 2:
// a disk whose extension-release glob does not match must be skipped, not
// copied as a literal *.raw. The current runcmd degrades the confext name to
// "*" when the glob does not match and copies the whole disk to
// /var/lib/confexts/*.raw; the fix must guard the copy on the glob matching
// (skip the device when the derived name is the degraded "*").
func TestRenderUserDataRuncmdSkipsDiskWithoutExtensionRelease(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	commands := joinedRuncmd(t, parts["user-data"])

	devices := []confextDevice{
		// An extra data disk with no extension-release.d: not a confext.
		{path: "/dev/vdd", fsType: "ext4"},
		// A real confext disk must still be copied.
		{path: "/dev/vdc", fsType: "squashfs", releaseName: "z-etcd"},
	}
	copied := simulateConfextRuncmd(commands, devices)

	if dest, ok := copied["/dev/vdd"]; ok {
		t.Errorf(
			"runcmd copies the no-extension-release disk /dev/vdd to %q; a disk without extension-release must be skipped, not copied:\n%s",
			dest,
			commands,
		)
	}

	if got := copied["/dev/vdc"]; got != "/var/lib/confexts/z-etcd.raw" {
		t.Errorf("runcmd copies the confext disk /dev/vdc to %q, want /var/lib/confexts/z-etcd.raw:\n%s", got, commands)
	}
}

// TestRenderUserDataRuncmdNeverProducesStarRaw pins requirement 4: the runcmd
// must never be able to produce a literal /var/lib/confexts/*.raw file. It
// simulates the runcmd over a full device set (root disk + partitions + CIDATA
// + confexts + a no-extension-release data disk) and asserts no copy lands on
// the literal *.raw destination. The current runcmd's quoted-glob quirk
// (`cp "$d" /var/lib/confexts/"$name".raw` with name degraded to "*") produces
// exactly this file for the root partitions and any no-extension-release disk.
func TestRenderUserDataRuncmdNeverProducesStarRaw(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	commands := joinedRuncmd(t, parts["user-data"])

	devices := []confextDevice{
		{path: "/dev/vda", fsType: "ext4"},
		{path: "/dev/vda1", fsType: "ext4"},
		{path: "/dev/vda2", fsType: "ext4"},
		{path: "/dev/vda3", fsType: "ext4"},
		{path: "/dev/vdb", label: "CIDATA", fsType: "vfat"},
		{path: "/dev/vdc", fsType: "squashfs", releaseName: "z-kubelet-node1"},
		{path: "/dev/vdd", fsType: "ext4"}, // no extension-release
	}
	copied := simulateConfextRuncmd(commands, devices)

	for path, dest := range copied {
		if dest == "/var/lib/confexts/*.raw" {
			t.Errorf(
				"runcmd produces the literal *.raw file for %s; the quoted-glob quirk must be eliminated:\n%s",
				path,
				commands,
			)
		}
	}
}

// TestRenderUserDataRuncmdValidWithNoConfextDisks pins the edge case where no
// confext disks are attached: the runcmd must still render as valid YAML and
// carry the confext-activation loop. The runcmd is static, so this holds both
// before and after the fix.
func TestRenderUserDataRuncmdValidWithNoConfextDisks(t *testing.T) {
	parts, err := cloudinit.Render(validData())
	if err != nil {
		t.Fatalf("Render with valid data returned error: %v", err)
	}

	var doc map[string]any
	if err := yaml.Unmarshal(parts["user-data"], &doc); err != nil {
		t.Fatalf("user-data is not valid YAML: %v", err)
	}

	commands := joinedRuncmd(t, parts["user-data"])
	if !strings.Contains(commands, "for d in") || !strings.Contains(commands, "/dev/vd") {
		t.Errorf("runcmd does not carry the confext-activation loop; runcmd:\n%s", commands)
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
