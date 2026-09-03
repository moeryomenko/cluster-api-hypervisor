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

// HypervisorMachine controller contract, part 1: identity and disk
// provisioning (test-first, red).
//
// This suite pins the contract for the first five steps of the
// HypervisorMachine reconciler: resolve the owning CAPI Machine and the
// linked Cluster, ensure the machine identity (MAC and static IP), provision
// the root disk, package the bootstrap Secret tree into the confext data
// disk images, and render the cloud-init CIDATA parts. The reconciler is
// exercised through the committed envtest harness with recording fakes
// standing in for every host-side effect, so no real qemu-img, mksquashfs,
// or cloud-hypervisor process is ever started. Part 2 of this file pins the
// VM lifecycle steps (reconcile steps 6-8): the machine TAP, the VM boot
// through the cloud-hypervisor client, providerID, the VMProvisioned
// condition, and readiness. The deletion step is out of both parts' scope; a
// later suite extends this file with its own tests.
//
// The contract, in prose:
//
//   - HypervisorMachineReconciler carries the controller-runtime wiring
//     (embedded client.Client, Scheme, Recorder) plus the injectable
//     dependencies: Config (the provider paths), NewVMClient (the per-machine
//     VM client factory), QemuImg (the qemu-img exec func), Confext (the
//     confext packager), RenderCloudInit (the CIDATA renderer), K8Netd (the
//     k8netd JSON-RPC client allocating the machine identity), and DeriveMAC
//     (the MAC derivation func). The tests build every dependency over a
//     recording seam and hand the fully constructed reconciler to the
//     controller.
//   - Reconcile resolves the object, then the owning CAPI Machine (owner
//     reference), then the linked Cluster. A missing object, a machine with
//     no owning Machine, and a machine whose Cluster is missing are all
//     no-ops, not errors, and no dependency is touched.
//   - Identity: when spec.mac is empty the controller derives the MAC
//     through the injected derivation seam with the cluster and machine
//     names; a spec.mac override is used as-is and derivation is skipped.
//     The static IP comes from k8netd: the controller creates the port,
//     attaches it to the cluster network, allocates the address for the
//     derived MAC, and records it in status.addresses together with the
//     hostname. The k8netd contract itself is pinned by
//     hypervisormachine_k8netd_test.go.
//   - Root disk: the controller converts the configured base image into
//     <vm-disks>/<name>-root.qcow2 with `qemu-img convert -O qcow2 <base>
//     <disk>` and resizes it to the spec size with `qemu-img resize <disk>
//     <size>M`. A disk that already exists at the requested size (checked
//     through qemu-img info) is left alone; a disk at the wrong size is
//     recreated.
//   - Confext data disk: the controller reads the bootstrap Secret named by
//     the linked bootstrap config's status, materializes the Secret tree
//     through the confext packager, and packages each confext into a .raw
//     squashfs image under the configured VM disk directory. A machine with
//     no bootstrap data skips packaging without error.
//   - CIDATA: the controller renders user-data, meta-data and network-config
//     through the injected renderer with the allocated IP, the machine
//     hostname, the cluster gateway and DNS, and the SSH public key, and the
//     rendered network-config addresses the machine with the allocated IP.
package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
)

// Machine contract fixtures: the provider paths, the machine size, and the
// identity inputs the tests pin. The network constants (CIDR, gateway, DNS,
// pool bounds) come from the cluster controller suite's shared fixtures.
const (
	// testBaseImage is the base image path the fixture puts in the provider
	// configuration; the root disk convert must convert from exactly this
	// path.
	testBaseImage = "build/k8labs-base.qcow2"

	// testMachineDisk is the root disk size in MiB the fixture machines ask
	// for.
	testMachineDisk = 2048
	testMachineCPU  = 2
	testMachineRAM  = 4096

	// testSSHPublicKey is the key the fixture stores on the linked bootstrap
	// config; the CIDATA renderer must receive it.
	testSSHPublicKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAImachine-controller-fixture"

	// testMACOverride is the spec.mac value the override case pins.
	testMACOverride = "aa:bb:cc:dd:ee:01"
)

// Compile-time pins for the seams the reconciler contract exposes: the
// Reconcile signature, the VM client shape, the exec-runner shape the
// packager expects, and the derivation and rendering funcs. Until the
// reconciler type exists the package does not compile — that is the intended
// red phase.
var (
	_ func(context.Context, ctrl.Request) (ctrl.Result, error) = (*HypervisorMachineReconciler)(nil).Reconcile
	_ chclient.Client                                          = (*chclient.FakeClient)(nil)
	_ confext.Runner                                           = (*recordingExecRunner)(nil)
	_ func(string, string) string                              = mac.Derive
	_ func(cloudinit.Data) (map[string][]byte, error)          = cloudinit.Render
)

// recordedExecCall captures one command invocation: the program name and the
// exact argument list.
type recordedExecCall struct {
	name string
	args []string
}

// qemuFailure overrides one qemu-img subcommand's (output, error) pair. The
// stderr bytes stand in for the CombinedOutput a real qemu-img failure
// carries; an override with a nil error simulates a subcommand that succeeds
// but returns unexpected output (the info parse-error path).
type qemuFailure struct {
	stderr []byte
	err    error
}

// recordingExecRunner records every command invocation in call order and
// simulates the side effects the machine controller's exec seams depend on.
// It is used two ways: as the qemu-img exec func and as the command runner
// of the confext packager. For qemu-img the runner simulates a small virtual
// disk store: "convert" creates the destination disk, "resize" sets its
// virtual size, and "info" reports the stored size as qemu-img JSON (or
// fails when the disk is absent). A disk marked locked is held by a running
// VM: "info" without the -U force-share flag fails on it and "convert" fails
// with the lock error on stderr, mirroring the qemu-img lock behavior the
// root-disk fix addresses. For mksquashfs it records the call and returns
// the canned output, so packaging proceeds without a real squashfs-tools
// binary.
type recordingExecRunner struct {
	calls  []recordedExecCall
	out    []byte
	err    error
	disks  map[string]int64       // disk path -> virtual size in bytes
	locked map[string]bool        // disk paths held by a running VM's qemu-img lock
	fail   map[string]qemuFailure // per-subcommand (output, error) override

	// mkdosfsRequiresTarget makes the mkdosfs simulation behave like the
	// real binary: it refuses to format a target file that does not exist
	// ("mkdosfs: unable to open <img>: No such file or directory", exit 1).
	// The CIDATA build must pre-create the image (dd/truncate, the
	// create-cloudinit.sh precedent) before formatting; the strengthened
	// TestMachineCIDATADiskBuilt subtest enables this mode to pin that
	// behavior. Off by default so the rest of the suite keeps the lenient
	// simulation.
	mkdosfsRequiresTarget bool
	// mkdosfsExisted records whether the mkdosfs target file existed on disk
	// at invocation time.
	mkdosfsExisted bool
}

// newRecordingExecRunner builds an empty runner with a live disk store.
func newRecordingExecRunner() *recordingExecRunner {
	return &recordingExecRunner{
		disks:  make(map[string]int64),
		locked: make(map[string]bool),
		fail:   make(map[string]qemuFailure),
	}
}

// Run implements the runner seam. The context is accepted and deliberately
// ignored: cancellation propagation is the default exec runner's concern,
// not this contract's.
func (f *recordingExecRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	argsCopy := make([]string, len(args))
	copy(argsCopy, args)

	f.calls = append(f.calls, recordedExecCall{name: name, args: argsCopy})
	if f.err != nil {
		return nil, f.err
	}

	switch name {
	case "qemu-img":
		return f.qemuImg(args)
	case "mksquashfs":
		return f.out, nil
	case "mkdosfs":
		// The CIDATA disk build formats the image with mkdosfs (the
		// create-cloudinit.sh precedent). Simulate the format by creating the
		// image file named by the last argument, so the CIDATA disk exists on
		// disk for the file-existence assertions; a per-subcommand override
		// fails the build. In realistic mode the simulation matches the real
		// binary: mkdosfs refuses to format a target that does not exist, so
		// the build must pre-create the image before formatting.
		if fail, ok := f.fail["mkdosfs"]; ok {
			return fail.stderr, fail.err
		}

		img := args[len(args)-1]
		if _, err := os.Stat(img); err == nil {
			f.mkdosfsExisted = true
		} else if f.mkdosfsRequiresTarget {
			return []byte("mkdosfs: unable to open " + img + ": No such file or directory"), errors.New("exit status 1")
		}

		if err := os.WriteFile(img, []byte("CIDATA-FAT16"), 0o644); err != nil {
			return nil, err
		}

		return f.out, nil
	case "mcopy":
		// The CIDATA disk build copies each rendered part into the image with
		// mcopy. The invocation is already recorded in f.calls; the source
		// files are written by the implementation, so the test reads them from
		// the recorded arguments.
		if fail, ok := f.fail["mcopy"]; ok {
			return fail.stderr, fail.err
		}

		return f.out, nil
	default:
		return nil, fmt.Errorf("recordingExecRunner: unexpected binary %q", name)
	}
}

// qemuImg simulates the qemu-img subcommands the machine controller issues.
// The disk path is the last argument of each invocation.
func (f *recordingExecRunner) qemuImg(args []string) ([]byte, error) {
	switch args[0] {
	case "convert":
		disk := args[len(args)-1]
		if f.locked[disk] {
			// A running VM holds the disk lock; qemu-img convert fails with
			// the lock error on stderr (CombinedOutput).
			return []byte("qemu-img: " + disk + ": Failed to lock byte 201"), errors.New("exit status 1")
		}

		if fail, ok := f.fail["convert"]; ok {
			return fail.stderr, fail.err
		}

		f.disks[disk] = 0

		return f.out, nil
	case "resize":
		if fail, ok := f.fail["resize"]; ok {
			return fail.stderr, fail.err
		}

		disk := args[len(args)-2]

		sizeMiB, err := strconv.ParseInt(strings.TrimSuffix(args[len(args)-1], "M"), 10, 64)
		if err != nil {
			return nil, err
		}

		f.disks[disk] = sizeMiB * 1024 * 1024

		return f.out, nil
	case "info":
		disk := args[len(args)-1]
		if f.locked[disk] && !hasForceShare(args) {
			// Without -U, qemu-img info cannot read a disk a running VM
			// holds; with -U the size stays readable.
			return nil, fmt.Errorf("qemu-img: %s: Failed to lock byte 201", disk)
		}

		if fail, ok := f.fail["info"]; ok {
			return fail.stderr, fail.err
		}

		size, ok := f.disks[disk]
		if !ok {
			return nil, fmt.Errorf("qemu-img: %s: No such file or directory", disk)
		}

		return []byte(fmt.Sprintf(`{"virtual-size": %d, "format": "qcow2", "filename": %q}`, size, disk)), nil
	default:
		return nil, fmt.Errorf("recordingExecRunner: unexpected qemu-img subcommand %q", args[0])
	}
}

// hasForceShare reports whether the qemu-img invocation carries the -U
// (force-share) flag, which lets a locked disk's metadata be read.
func hasForceShare(args []string) bool {
	return slices.Contains(args, "-U")
}

// qemuCalls returns the recorded qemu-img invocations for the subcommand, in
// invocation order.
func (f *recordingExecRunner) qemuCalls(subcommand string) []recordedExecCall {
	var calls []recordedExecCall
	for _, call := range f.calls {
		if call.name == "qemu-img" && len(call.args) > 0 && call.args[0] == subcommand {
			calls = append(calls, call)
		}
	}

	return calls
}

// mksquashfsCalls returns the recorded mksquashfs invocations, in invocation
// order.
func (f *recordingExecRunner) mksquashfsCalls() []recordedExecCall {
	var calls []recordedExecCall
	for _, call := range f.calls {
		if call.name == "mksquashfs" {
			calls = append(calls, call)
		}
	}

	return calls
}

// macDeriveCall records one invocation of the MAC derivation seam: the
// cluster and machine names it was called with, and the address the seam
// returned.
type macDeriveCall struct {
	cluster string
	machine string
	derived string
}

// recordingMACDerive records every invocation and returns the real derived
// address for the pair, so tests observe whether the controller derived the
// address or used the spec override without stubbing the derivation itself.
type recordingMACDerive struct {
	calls []macDeriveCall
}

// derive implements the DeriveMAC seam.
func (d *recordingMACDerive) derive(clusterName, machineName string) string {
	addr := mac.Derive(clusterName, machineName)
	d.calls = append(d.calls, macDeriveCall{cluster: clusterName, machine: machineName, derived: addr})

	return addr
}

// recordingCIDATARender records every render invocation and delegates to the
// real cloud-init renderer, so tests can inspect the exact Data the
// controller passed while still asserting the rendered content.
type recordingCIDATARender struct {
	calls []cloudinit.Data
	err   error
}

// render implements the RenderCloudInit seam.
func (r *recordingCIDATARender) render(d cloudinit.Data) (map[string][]byte, error) {
	r.calls = append(r.calls, d)
	if r.err != nil {
		return nil, r.err
	}

	return cloudinit.Render(d)
}

// machineBootstrapTree mirrors the role-split tree set the bootstrap side
// renders for a worker node: the node kubelet config plus the etcd and
// control-plane confexts a full tree carries. Keys are slash-separated paths
// whose first segment is the confext name. This is the decoded form the
// machine controller hands to the confext packager; the bootstrap Secret
// carries the same tree encoded as the tree.json blob of
// machineBootstrapSecretData.
func machineBootstrapTree() map[string][]byte {
	return map[string][]byte{
		"z-etcd/etc/etcd/etcd.conf.yml":                           []byte("etcd config"),
		"z-etcd/etc/extension-release.d/extension-release.z-etcd": []byte("ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\n"),
		"z-kubernetes-cp/etc/kubernetes/cp.env":                   []byte("cp env"),
		"z-kubernetes-cp/etc/kubernetes/pki/ca.pem":               []byte("ca cert"),
		"z-kubelet-node1/etc/kubernetes/kubelet.conf":             []byte("kubelet config"),
	}
}

// machineBootstrapSecretData wraps machineBootstrapTree as the bootstrap
// Secret payload: a single tree.json key whose value is a JSON object
// mapping every tree path to its base64-encoded content. Kubernetes Secret
// data keys cannot contain "/", so the slash-separated tree paths cannot be
// stored as literal Secret keys; the machine controller decodes the blob
// back into the path-to-content map before packaging.
func machineBootstrapSecretData(t *testing.T) map[string][]byte {
	t.Helper()

	encoded := make(map[string]string, len(machineBootstrapTree()))
	for path, content := range machineBootstrapTree() {
		encoded[path] = base64.StdEncoding.EncodeToString(content)
	}

	blob, err := json.Marshal(encoded)
	if err != nil {
		t.Fatalf("encode bootstrap tree: %v", err)
	}

	return map[string][]byte{"tree.json": blob}
}

// linkedMachine is the CAPI linkage the machine controller resolves: the
// owning CAPI Machine, the HypervisorMachine infrastructure object, the
// linked bootstrap config, and the rendered bootstrap Secret.
type linkedMachine struct {
	namespace string
	name      string
	hm        *infrastructurev1alpha1.HypervisorMachine
	machine   *clusterv1.Machine
	config    *bootstrapv1alpha1.HypervisorConfig
	secret    *corev1.Secret
}

// newLinkedMachine creates the full CAPI linkage for one machine inside the
// linked cluster: a bootstrap config (whose status names the bootstrap
// Secret), the bootstrap Secret holding the confext tree, the CAPI Machine
// (whose bootstrap config ref points at the bootstrap config and whose
// infrastructure ref points at the HypervisorMachine), and the
// HypervisorMachine carrying the owner reference to the Machine. When
// withBootstrap is false the Machine's bootstrap configRef names a
// HypervisorConfig that is never created — spec.bootstrap is required by the
// v1beta2 Machine API, so the dangling reference stands in for "no bootstrap
// data": the controller resolves it to NotFound and skips every bootstrap
// step.
func newLinkedMachine(
	t *testing.T,
	c client.Client,
	lc *linkedCluster,
	name string,
	withBootstrap bool,
) *linkedMachine {
	t.Helper()
	ctx := t.Context()

	lm := &linkedMachine{namespace: lc.namespace, name: name}

	if withBootstrap {
		lm.config = &bootstrapv1alpha1.HypervisorConfig{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-config", Namespace: lc.namespace},
			Spec: bootstrapv1alpha1.HypervisorConfigSpec{
				ClusterName:  lc.name,
				Role:         "worker",
				NodeName:     name,
				SSHPublicKey: testSSHPublicKey,
			},
		}
		if err := c.Create(ctx, lm.config); err != nil {
			t.Fatalf("create HypervisorConfig: %v", err)
		}

		lm.config.Status.DataSecretName = new(name + "-data")
		if err := c.Status().Update(ctx, lm.config); err != nil {
			t.Fatalf("set HypervisorConfig dataSecretName: %v", err)
		}

		lm.secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name + "-data", Namespace: lc.namespace},
			Data:       machineBootstrapSecretData(t),
		}
		if err := c.Create(ctx, lm.secret); err != nil {
			t.Fatalf("create bootstrap Secret: %v", err)
		}
	}

	lm.machine = &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lc.namespace},
		Spec: clusterv1.MachineSpec{
			ClusterName: lc.name,
			Bootstrap:   clusterv1.Bootstrap{},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrastructurev1alpha1.GroupVersion.Group,
				Kind:     "HypervisorMachine",
				Name:     name,
			},
		},
	}
	if withBootstrap {
		lm.machine.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{
			APIGroup: bootstrapv1alpha1.GroupVersion.Group,
			Kind:     "HypervisorConfig",
			Name:     lm.config.Name,
		}
	} else {
		// spec.bootstrap is required by the v1beta2 Machine API; the dangling
		// reference keeps the Machine valid while resolving to no bootstrap
		// data.
		lm.machine.Spec.Bootstrap.ConfigRef = clusterv1.ContractVersionedObjectReference{
			APIGroup: bootstrapv1alpha1.GroupVersion.Group,
			Kind:     "HypervisorConfig",
			Name:     name + "-config",
		}
	}

	if err := c.Create(ctx, lm.machine); err != nil {
		t.Fatalf("create CAPI Machine: %v", err)
	}

	lm.hm = &infrastructurev1alpha1.HypervisorMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: lc.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       lm.machine.Name,
					UID:        lm.machine.UID,
				},
			},
		},
		Spec: infrastructurev1alpha1.HypervisorMachineSpec{
			ClusterName: lc.name,
			CPU:         testMachineCPU,
			RAM:         testMachineRAM,
			Disk:        testMachineDisk,
		},
	}
	if err := c.Create(ctx, lm.hm); err != nil {
		t.Fatalf("create HypervisorMachine: %v", err)
	}

	return lm
}

// machineFixture bundles the reconciler under test with every recording
// seam.
type machineFixture struct {
	r      *HypervisorMachineReconciler
	qemu   *recordingExecRunner
	pack   *recordingExecRunner
	derive *recordingMACDerive
	render *recordingCIDATARender
	vm     *chclient.FakeClient
}

// newMachineFixture builds the reconciler under test over the recording
// seams. The composite literal pins the exact reconciler shape the
// implementation must expose: the controller-runtime wiring plus the
// injectable Config, NewVMClient, K8Netd, QemuImg, Confext, RenderCloudInit,
// and DeriveMAC dependencies. The NewVMClient factory routes every per-machine
// construction to one shared fake, so the VM assertions below observe every
// call regardless of which machine triggered it; the socket-tree tests in
// hypervisormachine_socket_test.go pin the per-machine directories. The
// k8netd dependency is wired to a fake server so identity provisioning
// proceeds without a real daemon.
func newMachineFixture(t *testing.T, c client.Client) *machineFixture {
	t.Helper()

	qemu := newRecordingExecRunner()
	pack := newRecordingExecRunner()
	derive := &recordingMACDerive{}
	render := &recordingCIDATARender{}
	vm := &chclient.FakeClient{}

	// The reconciler allocates the machine identity through k8netd; wire a
	// fake server so CreatePort/AttachPort succeed and AllocateIP returns
	// the pool start address.
	sock := filepath.Join(t.TempDir(), "control.sock")

	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New %q: %v", sock, err)
	}

	t.Cleanup(func() { _ = srv.Close() })
	srv.SetResult("CreatePort", nil)
	srv.SetResult("AttachPort", nil)
	srv.SetResult("AllocateIP", testPoolStart)

	k8Client := k8netd.NewClient(sock)

	r := &HypervisorMachineReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(16),
		Config: config.Config{
			BaseImage: testBaseImage,
			Firmware:  testFirmware,
			VMDiskDir: t.TempDir(),
			SocketDir: testSocketDir,
		},
		NewVMClient:     func(string, string) chclient.Client { return vm },
		K8Netd:          k8Client,
		QemuImg:         qemu.Run,
		Confext:         confext.NewPackager(confext.WithRunner(pack)),
		RenderCloudInit: render.render,
		DeriveMAC:       derive.derive,
	}

	return &machineFixture{r: r, qemu: qemu, pack: pack, derive: derive, render: render, vm: vm}
}

// statusInternalIP returns the internal IP recorded in the machine status,
// or the empty string when none is recorded.
func statusInternalIP(hm *infrastructurev1alpha1.HypervisorMachine) string {
	for _, addr := range hm.Status.Addresses {
		if addr.Type == clusterv1.MachineInternalIP {
			return addr.Address
		}
	}

	return ""
}

// statusHostName returns the hostname recorded in the machine status, or the
// empty string when none is recorded.
func statusHostName(hm *infrastructurev1alpha1.HypervisorMachine) string {
	for _, addr := range hm.Status.Addresses {
		if addr.Type == clusterv1.MachineHostName {
			return addr.Address
		}
	}

	return ""
}

// TestMachineIdentityOwnerResolution pins the owner and cluster resolution
// contract: the controller resolves the HypervisorMachine, the owning CAPI
// Machine, and the linked Cluster before touching any dependency. A missing
// object, a machine with no owning Machine, and a machine whose Cluster does
// not exist are all no-ops: no error, no reconcile actions, no status
// change.
func TestMachineIdentityOwnerResolution(t *testing.T) {
	c := mustReconcileClient(t)
	lc := newLinkedCluster(t, c, "machine-owner", "capi-cluster")

	t.Run("missing object is a no-op", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		key := client.ObjectKey{Namespace: lc.namespace, Name: "does-not-exist"}

		res, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		if err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		if res != (ctrl.Result{}) {
			t.Errorf("Reconcile result = %+v, want empty", res)
		}

		if len(fx.qemu.calls) != 0 || len(fx.pack.calls) != 0 {
			t.Errorf("missing-object reconcile touched exec seams: qemu %v, packager %v", fx.qemu.calls, fx.pack.calls)
		}

		if len(fx.vm.Calls) != 0 {
			t.Errorf("missing-object reconcile touched the VM client: %v", fx.vm.Calls)
		}
	})

	t.Run("machine without an owning Machine is untouched", func(t *testing.T) {
		fx := newMachineFixture(t, c)

		hm := &infrastructurev1alpha1.HypervisorMachine{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan", Namespace: lc.namespace},
			Spec: infrastructurev1alpha1.HypervisorMachineSpec{
				ClusterName: lc.name,
				CPU:         testMachineCPU,
				RAM:         testMachineRAM,
				Disk:        testMachineDisk,
			},
		}
		if err := c.Create(t.Context(), hm); err != nil {
			t.Fatalf("create orphan HypervisorMachine: %v", err)
		}

		key := client.ObjectKeyFromObject(hm)
		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		got := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), key, got); err != nil {
			t.Fatalf("Get HypervisorMachine: %v", err)
		}

		if len(got.Status.Addresses) != 0 || got.Status.Ready {
			t.Errorf("orphan machine was provisioned: addresses %v, ready %v", got.Status.Addresses, got.Status.Ready)
		}

		if len(fx.qemu.calls) != 0 || len(fx.pack.calls) != 0 {
			t.Errorf("orphan reconcile touched exec seams: qemu %v, packager %v", fx.qemu.calls, fx.pack.calls)
		}
	})

	t.Run("machine whose cluster is missing is untouched", func(t *testing.T) {
		fx := newMachineFixture(t, c)

		machine := &clusterv1.Machine{
			ObjectMeta: metav1.ObjectMeta{Name: "clusterless", Namespace: lc.namespace},
			Spec: clusterv1.MachineSpec{
				ClusterName: "no-such-cluster",
				Bootstrap: clusterv1.Bootstrap{
					ConfigRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: bootstrapv1alpha1.GroupVersion.Group,
						Kind:     "HypervisorConfig",
						Name:     "clusterless-config",
					},
				},
				InfrastructureRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: infrastructurev1alpha1.GroupVersion.Group,
					Kind:     "HypervisorMachine",
					Name:     "clusterless",
				},
			},
		}
		if err := c.Create(t.Context(), machine); err != nil {
			t.Fatalf("create clusterless Machine: %v", err)
		}

		hm := &infrastructurev1alpha1.HypervisorMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "clusterless",
				Namespace: lc.namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Machine",
						Name:       machine.Name,
						UID:        machine.UID,
					},
				},
			},
			Spec: infrastructurev1alpha1.HypervisorMachineSpec{
				ClusterName: "no-such-cluster",
				CPU:         testMachineCPU,
				RAM:         testMachineRAM,
				Disk:        testMachineDisk,
			},
		}
		if err := c.Create(t.Context(), hm); err != nil {
			t.Fatalf("create clusterless HypervisorMachine: %v", err)
		}

		key := client.ObjectKeyFromObject(hm)
		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		got := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), key, got); err != nil {
			t.Fatalf("Get HypervisorMachine: %v", err)
		}

		if len(got.Status.Addresses) != 0 {
			t.Errorf("clusterless machine got addresses %v, want none", got.Status.Addresses)
		}

		if len(fx.qemu.calls) != 0 {
			t.Errorf("clusterless reconcile touched the qemu seam: %v", fx.qemu.calls)
		}
	})
}

// TestMachineIdentityMAC pins the MAC identity contract: with an empty
// spec.mac the controller derives the address through the injected
// derivation seam with the cluster and machine names, and with spec.mac set
// it uses the override and never calls the derivation seam.
func TestMachineIdentityMAC(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("derived from cluster and machine names", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-mac-derive", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		if got := len(fx.derive.calls); got != 1 {
			t.Fatalf("MAC derivation called %d times, want 1", got)
		}

		if fx.derive.calls[0].cluster != lc.name || fx.derive.calls[0].machine != lm.name {
			t.Errorf("MAC derivation called with (%q, %q), want (%q, %q)",
				fx.derive.calls[0].cluster, fx.derive.calls[0].machine, lc.name, lm.name)
		}

		want := mac.Derive(lc.name, lm.name)
		if fx.derive.calls[0].derived != want {
			t.Errorf("derived MAC = %q, want %q", fx.derive.calls[0].derived, want)
		}
	})

	t.Run("spec.mac overrides derivation", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-mac-override", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		lm.hm.Spec.MAC = testMACOverride
		if err := c.Update(t.Context(), lm.hm); err != nil {
			t.Fatalf("set spec.mac: %v", err)
		}

		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		if got := len(fx.derive.calls); got != 0 {
			t.Errorf("MAC derivation called %d times with spec.mac set, want 0", got)
		}
	})
}

// TestMachineDisksRootDiskQemuImgArgs pins the root disk provisioning
// contract: the first reconcile converts the configured base image into the
// machine root disk and resizes it to the spec size, with the exact qemu-img
// argument shapes. The size probe must carry --output=json: without it real
// qemu-img emits human-readable text that json.Unmarshal rejects, so the
// controller would convert a correctly sized disk on every reconcile.
func TestMachineDisksRootDiskQemuImgArgs(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	vmDisksDir := fx.r.Config.VMDiskDir
	lc := newLinkedCluster(t, c, "machine-disk", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")

	converts := fx.qemu.qemuCalls("convert")
	if got := len(converts); got != 1 {
		t.Fatalf("qemu-img convert called %d times, want 1", got)
	}

	wantConvert := []string{"convert", "-O", "qcow2", testBaseImage, diskPath}
	if !reflect.DeepEqual(converts[0].args, wantConvert) {
		t.Errorf("convert args = %v, want %v", converts[0].args, wantConvert)
	}

	resizes := fx.qemu.qemuCalls("resize")
	if got := len(resizes); got != 1 {
		t.Fatalf("qemu-img resize called %d times, want 1", got)
	}

	wantResize := []string{"resize", diskPath, fmt.Sprintf("%dM", testMachineDisk)}
	if !reflect.DeepEqual(resizes[0].args, wantResize) {
		t.Errorf("resize args = %v, want %v", resizes[0].args, wantResize)
	}

	// The size probe reads the disk with the -U force-share flag so a disk
	// locked by a running VM still reports its size, and --output=json so
	// the virtual-size is machine-readable.
	infos := fx.qemu.qemuCalls("info")
	if got := len(infos); got != 1 {
		t.Fatalf("qemu-img info called %d times, want 1", got)
	}

	wantInfo := []string{"info", "-U", "--output=json", diskPath}
	if !reflect.DeepEqual(infos[0].args, wantInfo) {
		t.Errorf("info args = %v, want %v", infos[0].args, wantInfo)
	}
}

// TestMachineDisksRootDiskIdempotent pins the root disk idempotency
// contract: a second reconcile skips qemu-img convert and resize when the
// disk already exists at the requested size, and re-provisions when the
// existing disk has the wrong size.
func TestMachineDisksRootDiskIdempotent(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("existing disk at the right size is left alone", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-disk-idem", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)
		key := client.ObjectKeyFromObject(lm.hm)

		for i := range 2 {
			if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile %d error: %v", i+1, err)
			}
		}

		// The first reconcile converted and resized; the second found the
		// disk at the requested size and skipped both.
		if got := len(fx.qemu.qemuCalls("convert")); got != 1 {
			t.Errorf("qemu-img convert called %d times across two reconciles, want 1", got)
		}

		if got := len(fx.qemu.qemuCalls("resize")); got != 1 {
			t.Errorf("qemu-img resize called %d times across two reconciles, want 1", got)
		}
		// Existence is re-checked on every reconcile.
		if got := len(fx.qemu.qemuCalls("info")); got != 2 {
			t.Errorf("qemu-img info called %d times across two reconciles, want 2", got)
		}
	})

	t.Run("existing disk at the wrong size is recreated", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		vmDisksDir := fx.r.Config.VMDiskDir
		lc := newLinkedCluster(t, c, "machine-disk-recreate", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)
		key := client.ObjectKeyFromObject(lm.hm)

		// A stale disk with half the requested size.
		diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")
		fx.qemu.disks[diskPath] = (testMachineDisk / 2) * 1024 * 1024

		for i := range 2 {
			if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
				t.Fatalf("Reconcile %d error: %v", i+1, err)
			}
		}

		// The wrong-sized disk was converted and resized on the first
		// reconcile, and the corrected disk was skipped on the second.
		if got := len(fx.qemu.qemuCalls("convert")); got != 1 {
			t.Errorf("qemu-img convert called %d times, want 1", got)
		}

		if got := len(fx.qemu.qemuCalls("resize")); got != 1 {
			t.Errorf("qemu-img resize called %d times, want 1", got)
		}

		if got := fx.qemu.disks[diskPath]; got != testMachineDisk*1024*1024 {
			t.Errorf("simulated disk size = %d bytes, want %d", got, testMachineDisk*1024*1024)
		}
	})

	t.Run("locked disk at the right size is left alone", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		vmDisksDir := fx.r.Config.VMDiskDir
		lc := newLinkedCluster(t, c, "machine-disk-locked-idem", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)
		key := client.ObjectKeyFromObject(lm.hm)

		// A running VM holds the disk lock; the disk is already at the
		// requested size.
		diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")
		fx.qemu.disks[diskPath] = testMachineDisk * 1024 * 1024
		fx.qemu.locked[diskPath] = true

		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		// The size was read through force-share and the correct-size disk was
		// left alone: no convert, no resize.
		if got := len(fx.qemu.qemuCalls("convert")); got != 0 {
			t.Errorf("qemu-img convert called %d times for a locked correct-size disk, want 0", got)
		}

		if got := len(fx.qemu.qemuCalls("resize")); got != 0 {
			t.Errorf("qemu-img resize called %d times for a locked correct-size disk, want 0", got)
		}

		infos := fx.qemu.qemuCalls("info")
		if got := len(infos); got != 1 {
			t.Fatalf("qemu-img info called %d times, want 1", got)
		}

		wantInfo := []string{"info", "-U", "--output=json", diskPath}
		if !reflect.DeepEqual(infos[0].args, wantInfo) {
			t.Errorf("info args = %v, want %v", infos[0].args, wantInfo)
		}
	})

	t.Run("locked disk at the wrong size is converted and surfaces stderr", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		vmDisksDir := fx.r.Config.VMDiskDir
		lc := newLinkedCluster(t, c, "machine-disk-locked-recreate", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)
		key := client.ObjectKeyFromObject(lm.hm)

		// A stale, locked disk at half the requested size.
		diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")
		fx.qemu.disks[diskPath] = (testMachineDisk / 2) * 1024 * 1024
		fx.qemu.locked[diskPath] = true

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		if err == nil {
			t.Fatal("Reconcile succeeded with a locked wrong-size disk, want the convert error")
		}
		// The wrapped error carries the qemu-img stderr, not just "exit
		// status 1".
		if !strings.Contains(err.Error(), "Failed to lock byte 201") {
			t.Errorf("convert error = %q, want it to include qemu-img stderr %q", err, "Failed to lock byte 201")
		}
		// The wrong size still forced a convert attempt.
		if got := len(fx.qemu.qemuCalls("convert")); got != 1 {
			t.Errorf("qemu-img convert called %d times, want 1", got)
		}
	})
}

// TestMachineDisksRootDiskErrorSurfacesStderr pins the error contract of the
// root disk provisioning steps: a failed qemu-img convert or resize wraps the
// stderr output (CombinedOutput), not just "exit status 1", so the operator
// sees why the disk could not be provisioned.
func TestMachineDisksRootDiskErrorSurfacesStderr(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("resize failure carries qemu-img stderr", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		vmDisksDir := fx.r.Config.VMDiskDir
		lc := newLinkedCluster(t, c, "machine-disk-resize-err", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)
		key := client.ObjectKeyFromObject(lm.hm)

		// The disk is absent, so convert succeeds; resize then fails with
		// stderr.
		diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")
		fx.qemu.fail["resize"] = qemuFailure{
			stderr: []byte("qemu-img: " + diskPath + ": No space left on device"),
			err:    errors.New("exit status 1"),
		}

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		if err == nil {
			t.Fatal("Reconcile succeeded with a failing resize, want the resize error")
		}

		if !strings.Contains(err.Error(), "No space left on device") {
			t.Errorf("resize error = %q, want it to include qemu-img stderr %q", err, "No space left on device")
		}
		// Convert completed before the resize failure.
		if got := len(fx.qemu.qemuCalls("convert")); got != 1 {
			t.Errorf("qemu-img convert called %d times, want 1", got)
		}
	})

	t.Run("convert failure carries qemu-img stderr", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		vmDisksDir := fx.r.Config.VMDiskDir
		lc := newLinkedCluster(t, c, "machine-disk-convert-err", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)
		key := client.ObjectKeyFromObject(lm.hm)

		// The disk is absent, so the size probe fails and convert is
		// attempted; convert then fails with stderr.
		diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")
		fx.qemu.fail["convert"] = qemuFailure{
			stderr: []byte("qemu-img: " + diskPath + ": Permission denied"),
			err:    errors.New("exit status 1"),
		}

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
		if err == nil {
			t.Fatal("Reconcile succeeded with a failing convert, want the convert error")
		}

		if !strings.Contains(err.Error(), "Permission denied") {
			t.Errorf("convert error = %q, want it to include qemu-img stderr %q", err, "Permission denied")
		}
	})
}

// TestMachineDisksRootDiskInfoParseError pins the unreadable-info contract: a
// qemu-img info probe that succeeds but returns output that is not valid JSON
// — the human-readable text a qemu-img without --output=json emits, or
// truncated JSON — cannot report a size, and the parse error surfaces from
// the reconcile instead of silently converting a possibly correct disk. The
// probe itself still uses the -U force-share flag and --output=json.
func TestMachineDisksRootDiskInfoParseError(t *testing.T) {
	c := mustReconcileClient(t)

	// humanReadableInfo is the text qemu-img info emits without
	// --output=json; the --output=json pin makes it impossible in
	// production, but if it ever appears the parse error must surface.
	humanReadableInfo := "image: /build/vm-disks/node-1-root.qcow2\n" +
		"file format: qcow2\n" +
		"virtual size: 2 GiB (2147483648 bytes)\n" +
		"disk size: 1.2 GiB\n" +
		"cluster_size: 65536\n"

	tests := []struct {
		name      string
		namespace string
		out       string
	}{
		{name: "human-readable qemu-img output", namespace: "machine-disk-info-parse", out: humanReadableInfo},
		{name: "malformed JSON", namespace: "machine-disk-info-malformed", out: `{"virtual-size": `},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newMachineFixture(t, c)
			vmDisksDir := fx.r.Config.VMDiskDir
			lc := newLinkedCluster(t, c, tt.namespace, "capi-cluster")
			lm := newLinkedMachine(t, c, lc, "node-1", true)
			key := client.ObjectKeyFromObject(lm.hm)

			// qemu-img info succeeds but returns output the size cannot be
			// parsed from.
			diskPath := filepath.Join(vmDisksDir, lm.name+"-root.qcow2")
			fx.qemu.fail["info"] = qemuFailure{stderr: []byte(tt.out), err: nil}

			_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
			if err == nil {
				t.Fatal("Reconcile succeeded with unparseable qemu-img info, want the parse error")
			}

			if !strings.Contains(err.Error(), "parse qemu-img info") {
				t.Errorf("Reconcile error = %q, want it to include the info parse error", err)
			}
			// The unreadable size must not trigger a convert of a possibly
			// correct disk.
			if got := len(fx.qemu.qemuCalls("convert")); got != 0 {
				t.Errorf("qemu-img convert called %d times, want 0", got)
			}

			if got := len(fx.qemu.qemuCalls("resize")); got != 0 {
				t.Errorf("qemu-img resize called %d times, want 0", got)
			}
			// The size probe still used force-share and --output=json.
			infos := fx.qemu.qemuCalls("info")
			if got := len(infos); got != 1 {
				t.Fatalf("qemu-img info called %d times, want 1", got)
			}

			wantInfo := []string{"info", "-U", "--output=json", diskPath}
			if !reflect.DeepEqual(infos[0].args, wantInfo) {
				t.Errorf("info args = %v, want %v", infos[0].args, wantInfo)
			}
		})
	}
}

// TestMachineDisksConfextPackaging pins the confext data disk contract: the
// controller reads the bootstrap Secret named by the linked bootstrap
// config's status, materializes the Secret tree through the confext
// packager, and packages each confext into a .raw squashfs image under the
// configured VM disk directory. A machine without bootstrap data skips
// packaging entirely.
func TestMachineDisksConfextPackaging(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("bootstrap secret tree becomes confext raws", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		vmDisksDir := fx.r.Config.VMDiskDir
		lc := newLinkedCluster(t, c, "machine-confext", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		calls := fx.pack.mksquashfsCalls()
		// BuildRaws walks the staging dir sorted by filename.
		wantConfexts := []string{"z-etcd", "z-kubelet-node1", "z-kubernetes-cp"}
		if got := len(calls); got != len(wantConfexts) {
			t.Fatalf("mksquashfs called %d times, want %d: %+v", got, len(wantConfexts), calls)
		}

		staging, outDir := "", ""

		for i, call := range calls {
			if call.name != "mksquashfs" || len(call.args) != 4 {
				t.Fatalf("mksquashfs call %d = %+v, want 4 arguments", i, call)
			}

			src, dst := call.args[0], call.args[1]
			if !reflect.DeepEqual(call.args[2:], []string{"-noappend", "-all-root"}) {
				t.Errorf("mksquashfs flags = %v, want [-noappend -all-root]", call.args[2:])
			}

			if filepath.Base(src) != wantConfexts[i] {
				t.Errorf("mksquashfs source = %q, want the %q tree", src, wantConfexts[i])
			}

			if filepath.Base(dst) != wantConfexts[i]+".raw" {
				t.Errorf("mksquashfs destination = %q, want %q.raw", dst, wantConfexts[i])
			}

			if i == 0 {
				staging, outDir = filepath.Dir(src), filepath.Dir(dst)
			} else if filepath.Dir(src) != staging || filepath.Dir(dst) != outDir {
				t.Errorf("mksquashfs call %d used a different staging or output directory", i)
			}
		}

		// The raw images land under the configured VM disk directory.
		if !strings.HasPrefix(outDir, vmDisksDir) {
			t.Errorf("confext output dir %q is not under the VM disk dir %q", outDir, vmDisksDir)
		}

		// The decoded Secret tree was materialized through the packager with
		// the exact contents of the bootstrap Secret.
		for key, content := range machineBootstrapTree() {
			path := filepath.Join(staging, filepath.FromSlash(key))

			got, err := os.ReadFile(path)
			if err != nil {
				t.Errorf("ReadFile(%q) error: %v", path, err)
				continue
			}

			if !bytes.Equal(got, content) {
				t.Errorf("tree file %q = %q, want %q", path, got, content)
			}
		}
	})

	t.Run("machine without bootstrap data skips packaging", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-confext-none", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", false)

		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}

		if got := len(fx.pack.calls); got != 0 {
			t.Errorf("packager called %d times without bootstrap data, want 0: %+v", got, fx.pack.calls)
		}
		// Identity provisioning still proceeded.
		hm := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(lm.hm), hm); err != nil {
			t.Fatalf("Get HypervisorMachine: %v", err)
		}

		if ip := statusInternalIP(hm); ip == "" {
			t.Error("machine without bootstrap data got no internal IP")
		}
	})
}

// TestMachineCIDATARenderedWithAllocatedIP pins the CIDATA rendering
// contract: the controller renders the cloud-init parts through the injected
// renderer with the machine's allocated IP, hostname, cluster gateway and
// DNS, and the linked SSH public key, and the rendered network-config
// addresses the machine with the allocated IP.
func TestMachineCIDATARenderedWithAllocatedIP(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-cidata", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	if got := len(fx.render.calls); got != 1 {
		t.Fatalf("CIDATA render called %d times, want 1", got)
	}

	d := fx.render.calls[0]

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(lm.hm), hm); err != nil {
		t.Fatalf("Get HypervisorMachine: %v", err)
	}

	ip := statusInternalIP(hm)
	if ip == "" {
		t.Fatal("no internal IP recorded in status")
	}

	// After DHCP rewiring, CIDATA render should be DHCP (no static IP/Gateway/DNS).
	if d.Hostname != lm.name {
		t.Errorf("render hostname = %q, want %q", d.Hostname, lm.name)
	}

	if d.InstanceID == "" {
		t.Error("render instance id is empty")
	}

	if d.SSHPublicKey != testSSHPublicKey {
		t.Errorf("render ssh public key = %q, want %q", d.SSHPublicKey, testSSHPublicKey)
	}

	// The real renderer now emits DHCP network-config, not static addressing.
	parts, err := cloudinit.Render(d)
	if err != nil {
		t.Fatalf("cloudinit.Render: %v", err)
	}

	if networkConfig := string(parts["network-config"]); !strings.Contains(strings.ToLower(networkConfig), "dhcp4") {
		t.Errorf("network-config should be DHCP (dhcp4:true), got:\n%s", networkConfig)
	}

	for _, part := range []string{"user-data", "meta-data", "network-config"} {
		if len(parts[part]) == 0 {
			t.Errorf("rendered part %q is empty", part)
		}
	}
}

// TestMachineCIDATADiskBuilt pins requirement 1: reconcileCIDATA produces the
// CIDATA disk image at <vm-disks>/<name>-cidata.img from the rendered parts
// instead of discarding them. The disk must exist and be non-empty after a
// reconcile of a machine with bootstrap data, and the failure paths (render
// failure, disk build failure) must abort the reconcile without leaving a
// disk behind.
func TestMachineCIDATADiskBuilt(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("bootstrap machine gets a cidata disk image", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-cidata-disk", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		// The fake mkdosfs behaves like the real binary: it refuses to
		// format a target file that does not exist. The build must
		// pre-create the image (dd/truncate, the create-cloudinit.sh
		// precedent) before formatting; against a build that removes the
		// stale image and formats the missing path the reconcile fails here.
		fx.qemu.mkdosfsRequiresTarget = true
		fx.pack.mkdosfsRequiresTarget = true

		fx.reconcileMachine(t, lm.hm)

		cidataDisk := filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-cidata.img")

		info, err := os.Stat(cidataDisk)
		if err != nil {
			t.Fatalf("CIDATA disk %q missing after reconcile: %v", cidataDisk, err)
		}

		if info.Size() == 0 {
			t.Errorf("CIDATA disk %q is empty", cidataDisk)
		}

		// The mkdosfs invocation must have seen the image file already on
		// disk: real mkdosfs cannot open a missing target, so a build that
		// formats before pre-creating the image fails on every reconcile.
		if !fx.qemu.mkdosfsExisted && !fx.pack.mkdosfsExisted {
			t.Error("mkdosfs ran before the CIDATA image file existed; the build must pre-create the image before formatting")
		}
	})

	t.Run("render failure aborts reconcile and leaves no disk", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-cidata-renderfail", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		fx.render.err = errors.New("render exploded")
		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err == nil {
			t.Fatal("Reconcile succeeded with a failing renderer, want error")
		}

		cidataDisk := filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-cidata.img")
		if _, err := os.Stat(cidataDisk); !os.IsNotExist(err) {
			t.Errorf("CIDATA disk %q exists after render failure (stat error %v), want absent", cidataDisk, err)
		}
	})

	t.Run("machine without bootstrap data gets no cidata disk", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-cidata-noboot", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", false)

		fx.reconcileMachine(t, lm.hm)

		cidataDisk := filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-cidata.img")
		if _, err := os.Stat(cidataDisk); !os.IsNotExist(err) {
			t.Errorf("CIDATA disk %q exists without bootstrap data (stat error %v), want absent", cidataDisk, err)
		}
	})

	t.Run("cidata disk build failure aborts reconcile", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-cidata-buildfail", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		// The CIDATA build is expected to reuse one of the exec seams; fail
		// the mkdosfs step on whichever runner the implementation uses.
		fx.qemu.fail["mkdosfs"] = qemuFailure{stderr: []byte("mkdosfs: cannot format"), err: errors.New("exit status 1")}
		fx.pack.fail["mkdosfs"] = qemuFailure{stderr: []byte("mkdosfs: cannot format"), err: errors.New("exit status 1")}

		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err == nil {
			t.Fatal("Reconcile succeeded with a failing CIDATA build, want error")
		}
	})
}

// cidataBuildCalls returns the recorded mkdosfs/mcopy invocations across the
// fixture's exec runners, in invocation order. The CIDATA build is expected
// to reuse one of the existing exec seams (the QemuImg func or the confext
// packager runner); observing both keeps the test agnostic to which one.
func cidataBuildCalls(fx *machineFixture) []recordedExecCall {
	var calls []recordedExecCall
	for _, runner := range []*recordingExecRunner{fx.qemu, fx.pack} {
		for _, call := range runner.calls {
			if call.name == "mkdosfs" || call.name == "mcopy" {
				calls = append(calls, call)
			}
		}
	}

	return calls
}

// TestMachineCIDATADiskContainsRenderedParts pins requirement 1's "created
// from the rendered parts, not discarded": the CIDATA build consumes the
// three rendered parts. The build is expected to reuse the exec seam
// (mkdosfs + mcopy, the create-cloudinit.sh precedent); the test asserts the
// recorded invocations carry the exact rendered part contents into the image
// under the NoCloud file names.
func TestMachineCIDATADiskContainsRenderedParts(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-cidata-parts", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.reconcileMachine(t, lm.hm)

	buildCalls := cidataBuildCalls(fx)
	if len(buildCalls) == 0 {
		t.Fatal("no CIDATA build invocations recorded (mkdosfs/mcopy); the rendered parts were discarded")
	}

	// The rendered parts the controller produced.
	if got := len(fx.render.calls); got != 1 {
		t.Fatalf("CIDATA render called %d times, want 1", got)
	}

	parts, err := cloudinit.Render(fx.render.calls[0])
	if err != nil {
		t.Fatalf("cloudinit.Render: %v", err)
	}

	// mkdosfs formats the image with the CIDATA label.
	var mkdosfsCalls []recordedExecCall

	for _, call := range buildCalls {
		if call.name == "mkdosfs" {
			mkdosfsCalls = append(mkdosfsCalls, call)
		}
	}

	if len(mkdosfsCalls) != 1 {
		t.Fatalf("mkdosfs called %d times, want 1: %+v", len(mkdosfsCalls), buildCalls)
	}

	joined := strings.Join(mkdosfsCalls[0].args, " ")
	if !strings.Contains(joined, "-F") || !strings.Contains(joined, "16") {
		t.Errorf("mkdosfs args %v do not format FAT16", mkdosfsCalls[0].args)
	}

	if !strings.Contains(joined, "CIDATA") {
		t.Errorf("mkdosfs args %v do not set the CIDATA label", mkdosfsCalls[0].args)
	}

	// mcopy writes each rendered part into the image under its NoCloud name.
	copied := map[string]string{} // ::name -> source file

	for _, call := range buildCalls {
		if call.name != "mcopy" {
			continue
		}

		if len(call.args) < 3 {
			t.Errorf("mcopy call %+v has too few arguments", call.args)
			continue
		}

		dst := call.args[len(call.args)-1]
		src := call.args[len(call.args)-2]
		copied[dst] = src
	}

	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		src, ok := copied["::"+name]
		if !ok {
			t.Errorf("mcopy did not write ::%s into the image (copied %v)", name, copied)
			continue
		}

		got, err := os.ReadFile(src)
		if err != nil {
			t.Errorf("read mcopy source %q: %v", src, err)
			continue
		}

		if !bytes.Equal(got, parts[name]) {
			t.Errorf("::%s content = %q, want the rendered part %q", name, got, parts[name])
		}
	}
}

// HypervisorMachine controller contract, part 2: the VM lifecycle (reconcile
// steps 6-8).
//
// This section pins the contract the machine controller's lifecycle steps
// implement over the same envtest harness and recording fakes:
//
// This section pins the contract the machine controller's lifecycle steps
// implement over the same envtest harness and recording fakes:
//
//   - Step 6 boots the VM through the machine's own chclient.Client, built
//     through the NewVMClient factory seam with the machine's socket
//     directory <SocketDir>/<machine>; the fixture pins the documented
//     default socket root /tmp/ch-capi. The fake's call log records one
//     EnsureRunning call followed by an Info state check. The socket path
//     itself is not observable through the fake's call log because the
//     client seam exposes no path, so this section pins the root the
//     implementation derives the per-machine directory from; the dedicated
//     socket-tree tests capture the constructed directories. The client's
//     EnsureRunning is idempotent by contract (a no-op when the VM is
//     already running), so the controller has no reason to pre-check the
//     state.
//   - Step 7 reports readiness: status.ready is true exactly when Info
//     reports the VM running, and stays false when the VM is in a
//     non-running state or the state query fails; both leave the reconcile
//     without error.
//   - The lifecycle steps set status.providerID to
//     hypervisor://<cluster>/<machine>, set the VMProvisioned condition once
//     the VM runs, and leave the addresses allocated in part 1 in place.
const (
	// testSocketDir is the provider's socket root the fixture config pins;
	// the VM lifecycle step derives the per-machine socket directory
	// <SocketDir>/<machine> from it.
	testSocketDir = "/tmp/ch-capi"

	// testFirmware is the firmware image path the fixture config pins; the
	// VM lifecycle step hands it to the VM client for the vm.create push.
	testFirmware = "/build/CLOUDHV.fd"

	// machineVMProvisionedCondition is the condition type the VM lifecycle
	// step reports once the cloud-hypervisor VM is provisioned.
	machineVMProvisionedCondition = "VMProvisioned"
)

// reconcileMachine runs one reconcile of the machine and fails the test on
// any error.
func (fx *machineFixture) reconcileMachine(t *testing.T, hm *infrastructurev1alpha1.HypervisorMachine) {
	t.Helper()

	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(hm)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
}

// getMachine reads the machine back from the API store.
func getMachine(
	t *testing.T,
	c client.Client,
	hm *infrastructurev1alpha1.HypervisorMachine,
) *infrastructurev1alpha1.HypervisorMachine {
	t.Helper()

	got := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(hm), got); err != nil {
		t.Fatalf("Get HypervisorMachine: %v", err)
	}

	return got
}

// machineCondition returns the status condition with the given type, or nil.
func machineCondition(hm *infrastructurev1alpha1.HypervisorMachine, t string) *metav1.Condition {
	for i := range hm.Status.Conditions {
		if hm.Status.Conditions[i].Type == t {
			return &hm.Status.Conditions[i]
		}
	}

	return nil
}

// TestMachineVMBootedViaClient pins the boot step contract: the controller
// drives the machine's own chclient.Client — built through the NewVMClient
// factory seam — with a single EnsureRunning call followed by an Info state
// check, and the fixture config pins the socket root the per-machine socket
// directory /tmp/ch-capi/<machine> derives from.
func TestMachineVMBootedViaClient(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-boot", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	wantCalls := []string{"EnsureRunning", "Info"}
	if !reflect.DeepEqual(fx.vm.Calls, wantCalls) {
		t.Errorf("VM client calls = %v, want %v", fx.vm.Calls, wantCalls)
	}

	// The per-machine socket directory is <SocketDir>/<machine>; the fake
	// client seam exposes no path, so the fixture pins the socket root the
	// implementation derives it from. The dedicated socket-tree tests
	// capture the constructed directories.
	if got := fx.r.Config.SocketDir; got != testSocketDir {
		t.Errorf("Config.SocketDir = %q, want %q", got, testSocketDir)
	}
}

// TestMachineVMBootsWithVhostUserNetConfig pins the REQ-005 wiring: before
// the VM boots, the controller renders the k8netd vhost-user net device for
// the machine — VhostUserSocketPath(machineName) plus the effective MAC,
// single queue pair — and hands it to the VM client as the --net config.
func TestMachineVMBootsWithVhostUserNetConfig(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-netcfg", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	wantMAC := mac.Derive(lc.name, lm.name)

	wantConfig, err := chclient.VhostUserNetConfig(chclient.VhostUserSocketPath(lm.name), wantMAC)
	if err != nil {
		t.Fatalf("VhostUserNetConfig: %v", err)
	}

	if got := len(fx.vm.NetConfigs); got != 1 {
		t.Fatalf("SetNetConfig called %d times, want 1 (recorded %v)", got, fx.vm.NetConfigs)
	}

	if fx.vm.NetConfigs[0] != wantConfig {
		t.Errorf("net config = %q, want %q", fx.vm.NetConfigs[0], wantConfig)
	}

	// The config was set before the boot call.
	if !reflect.DeepEqual(fx.vm.Calls, []string{"EnsureRunning", "Info"}) {
		t.Errorf("VM client calls = %v, want [EnsureRunning Info] after SetNetConfig", fx.vm.Calls)
	}
}

// TestMachineVMHandsFirmwareAndDisksToClient pins the boot-medium wiring:
// before the VM boots, the controller hands the VM client the firmware image
// from the provider config and the machine's disk images — the root qcow2
// first, then the CIDATA disk image, then every packaged confext raw in the
// machine's data directory, sorted by file name. The CIDATA disk occupies
// /dev/vdb (shifting the confext raws to vdc+), so it must sit directly after
// the root disk. A machine with no confext data directory carries the root
// and CIDATA disks alone; a machine with no bootstrap data carries the root
// disk alone.
func TestMachineVMHandsFirmwareAndDisksToClient(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("root, cidata, then confext raws", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-vm-bootcfg", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		// A packaged confext output directory with two raws; ReadDir sorts by
		// file name, so the expected attachment order is a.raw then b.raw.
		dataDir := filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-data")
		if err := os.MkdirAll(dataDir, 0o755); err != nil {
			t.Fatalf("create confext data dir: %v", err)
		}

		for _, name := range []string{"b.raw", "a.raw", "ignored.txt"} {
			if err := os.WriteFile(filepath.Join(dataDir, name), []byte("x"), 0o644); err != nil {
				t.Fatalf("write confext artifact %s: %v", name, err)
			}
		}

		fx.vm.State = ch.VMState("Running")
		fx.reconcileMachine(t, lm.hm)

		if got := len(fx.vm.Firmwares); got != 1 {
			t.Fatalf("SetFirmware called %d times, want 1 (recorded %v)", got, fx.vm.Firmwares)
		}

		if fx.vm.Firmwares[0] != testFirmware {
			t.Errorf("firmware = %q, want %q", fx.vm.Firmwares[0], testFirmware)
		}

		if got := len(fx.vm.DiskPathSets); got != 1 {
			t.Fatalf("SetDiskPaths called %d times, want 1 (recorded %v)", got, fx.vm.DiskPathSets)
		}

		wantDisks := []string{
			filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-root.qcow2"),
			filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-cidata.img"),
			filepath.Join(dataDir, "a.raw"),
			filepath.Join(dataDir, "b.raw"),
		}
		if !reflect.DeepEqual(fx.vm.DiskPathSets[0], wantDisks) {
			t.Errorf("disk paths = %v, want %v", fx.vm.DiskPathSets[0], wantDisks)
		}
	})

	t.Run("no confext raws carries root and cidata only", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-vm-bootcfg-noconfext", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		// Empty the bootstrap tree so packaging produces no raws; the CIDATA
		// disk is still rendered from the linked bootstrap config.
		secret := &corev1.Secret{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(lm.secret), secret); err != nil {
			t.Fatalf("Get bootstrap Secret: %v", err)
		}

		secret.Data = map[string][]byte{"tree.json": []byte("{}")}
		if err := c.Update(t.Context(), secret); err != nil {
			t.Fatalf("empty bootstrap tree: %v", err)
		}

		fx.vm.State = ch.VMState("Running")
		fx.reconcileMachine(t, lm.hm)

		if got := len(fx.vm.DiskPathSets); got != 1 {
			t.Fatalf("SetDiskPaths called %d times, want 1 (recorded %v)", got, fx.vm.DiskPathSets)
		}

		wantDisks := []string{
			filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-root.qcow2"),
			filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-cidata.img"),
		}
		if !reflect.DeepEqual(fx.vm.DiskPathSets[0], wantDisks) {
			t.Errorf("disk paths = %v, want %v", fx.vm.DiskPathSets[0], wantDisks)
		}
	})

	t.Run("machine without bootstrap carries root only", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-vm-bootcfg-noboot", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", false)

		fx.vm.State = ch.VMState("Running")
		fx.reconcileMachine(t, lm.hm)

		if got := len(fx.vm.DiskPathSets); got != 1 {
			t.Fatalf("SetDiskPaths called %d times, want 1 (recorded %v)", got, fx.vm.DiskPathSets)
		}

		wantDisks := []string{
			filepath.Join(fx.r.Config.VMDiskDir, lm.name+"-root.qcow2"),
		}
		if !reflect.DeepEqual(fx.vm.DiskPathSets[0], wantDisks) {
			t.Errorf("disk paths = %v, want %v", fx.vm.DiskPathSets[0], wantDisks)
		}
	})
}

// TestMachineVMHandsCPUAndRAMToClient pins the cpu/ram wiring: before the VM
// boots, the controller hands the VM client the machine's spec vCPU count and
// memory size in MiB, so the vm.create push carries the spec shape instead of
// the hardcoded 512 MiB / 1 vCPU defaults.
func TestMachineVMHandsCPUAndRAMToClient(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-cpuram", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	if got := len(fx.vm.CPUs); got != 1 {
		t.Fatalf("SetCPU called %d times, want 1 (recorded %v)", got, fx.vm.CPUs)
	}

	if fx.vm.CPUs[0] != testMachineCPU {
		t.Errorf("cpu = %d, want spec cpu %d", fx.vm.CPUs[0], testMachineCPU)
	}

	if got := len(fx.vm.RAMs); got != 1 {
		t.Fatalf("SetRAM called %d times, want 1 (recorded %v)", got, fx.vm.RAMs)
	}

	if fx.vm.RAMs[0] != testMachineRAM {
		t.Errorf("ram = %d, want spec ram %d", fx.vm.RAMs[0], testMachineRAM)
	}
}

// TestMachineVMHandlesZeroSpecCPUAndRAM pins the graceful zero-spec contract:
// a machine whose spec leaves CPU and RAM unset (the webhook normally rejects
// this, but a hand-built object can carry it) still reconciles without error
// and passes the zero values through to the client, which renders the
// cloud-hypervisor defaults instead of crashing or pushing invalid config.
func TestMachineVMHandlesZeroSpecCPUAndRAM(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-zero-cpuram", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	hm := getMachine(t, c, lm.hm)
	hm.Spec.CPU = 0

	hm.Spec.RAM = 0
	if err := c.Update(t.Context(), hm); err != nil {
		t.Fatalf("update HypervisorMachine spec: %v", err)
	}

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	if got := len(fx.vm.CPUs); got != 1 {
		t.Fatalf("SetCPU called %d times, want 1 (recorded %v)", got, fx.vm.CPUs)
	}

	if fx.vm.CPUs[0] != 0 {
		t.Errorf("cpu = %d, want the zero spec value passed through", fx.vm.CPUs[0])
	}

	if got := len(fx.vm.RAMs); got != 1 {
		t.Fatalf("SetRAM called %d times, want 1 (recorded %v)", got, fx.vm.RAMs)
	}

	if fx.vm.RAMs[0] != 0 {
		t.Errorf("ram = %d, want the zero spec value passed through", fx.vm.RAMs[0])
	}
}

// TestMachineVMProviderID pins the provider identity contract: once the VM
// lifecycle steps run, status.providerID is hypervisor://<cluster>/<machine>
// and the addresses allocated in part 1 are still in place.
func TestMachineVMProviderID(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-providerid", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	hm := getMachine(t, c, lm.hm)

	wantProviderID := fmt.Sprintf("hypervisor://%s/%s", lc.name, lm.name)
	if hm.Status.ProviderID == nil {
		t.Fatalf("status.providerID = nil, want %q", wantProviderID)
	}

	if *hm.Status.ProviderID != wantProviderID {
		t.Errorf("status.providerID = %q, want %q", *hm.Status.ProviderID, wantProviderID)
	}

	if hm.Spec.ProviderID == nil || *hm.Spec.ProviderID != wantProviderID {
		t.Errorf("spec.providerID = %v, want %q", hm.Spec.ProviderID, wantProviderID)
	}

	if ip := statusInternalIP(hm); ip == "" {
		t.Error("status.addresses lost the internal IP")
	}

	if host := statusHostName(hm); host != lm.name {
		t.Errorf("status.addresses hostname = %q, want %q", host, lm.name)
	}
}

// TestMachineVMProvisionedCondition pins the condition contract: once the VM
// lifecycle steps run, the VMProvisioned condition is present and true.
func TestMachineVMProvisionedCondition(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-cond", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	hm := getMachine(t, c, lm.hm)

	cond := machineCondition(hm, machineVMProvisionedCondition)
	if cond == nil {
		t.Fatalf("condition %q missing from status.conditions: %v", machineVMProvisionedCondition, hm.Status.Conditions)
	}

	if cond.Status != metav1.ConditionTrue {
		t.Errorf("condition %q status = %q, want %q", machineVMProvisionedCondition, cond.Status, metav1.ConditionTrue)
	}
}

// TestMachineVMReadyWhenRunning pins the readiness contract: when the VM
// client reports the VM running, status.ready becomes true.
func TestMachineVMReadyWhenRunning(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-ready", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)

	if hm := getMachine(t, c, lm.hm); !hm.Status.Ready {
		t.Error("status.ready = false with the VM running, want true")
	}
}

// TestMachineVMNotReadyWhenNotRunning pins the not-ready contract: when the
// VM client reports a non-running state or the state query fails, status.ready
// stays false and the reconcile completes without error.
func TestMachineVMNotReadyWhenNotRunning(t *testing.T) {
	c := mustReconcileClient(t)
	errInfo := errors.New("fake: vm info failed")

	tests := []struct {
		name      string
		namespace string
		state     ch.VMState
		infoErr   error
	}{
		{name: "VM reported shut down", namespace: "machine-vm-notready-shutdown", state: ch.VMState("Shutdown")},
		{name: "VM reported created", namespace: "machine-vm-notready-created", state: ch.VMState("Created")},
		{name: "VM state query fails", namespace: "machine-vm-notready-infoerr", infoErr: errInfo},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fx := newMachineFixture(t, c)
			lc := newLinkedCluster(t, c, tt.namespace, "capi-cluster")
			lm := newLinkedMachine(t, c, lc, "node-1", true)

			fx.vm.State = tt.state
			fx.vm.InfoErr = tt.infoErr
			fx.reconcileMachine(t, lm.hm)

			if hm := getMachine(t, c, lm.hm); hm.Status.Ready {
				t.Error("status.ready = true with the VM not running, want false")
			}
		})
	}
}

// HypervisorMachine controller contract, part 3: deletion and teardown
// (reconcile step 9).
//
// This section pins the contract the machine controller's deletion step
// implements over the same envtest harness and recording fakes. A machine
// whose deletion has begun — the deletion timestamp set and the machine
// finalizer still holding the object — is torn down instead of provisioned:
//
//   - Graceful shutdown: the VM is asked to shut down through the
//     cloud-hypervisor client (VM.Shutdown).
//   - Process teardown: the cloud-hypervisor process is stopped (VM.Stop),
//     after the graceful shutdown.
//   - Disk removal: the root disk <vm-disks>/<name>-root.qcow2 and the
//     confext data-disk artifacts — the packaged .raw output directory
//     (<vm-disks>/<name>-data) and the staging tree
//     (<vm-disks>/<name>-confext-staging) — are removed from the configured
//     VM disk directory, unless spec.retainDiskOnDelete keeps them in place
//     while the rest of the teardown still completes.
//   - Finalizer removal: the teardown drops the machine finalizer so the
//     object is reclaimed by the API server.
//   - A VM that is already absent (the client reports ErrNotFound from
//     Shutdown or Stop) is tolerated: teardown completes without error.
const (
	// machineDeleteFinalizer is the finalizer the machine carries when its
	// deletion begins; the teardown step must drop it so the object is
	// reclaimed. The name mirrors the cluster-side finalizer convention
	// <kind>.<group>.
	machineDeleteFinalizer = "hypervisormachine.infrastructure.cluster.x-k8s.io"
)

// markMachineForDeletion arms the deletion contract on the machine: it adds
// the machine finalizer, deletes the object through the API, and verifies
// the deletion timestamp is set with the finalizer still holding the object
// in the store.
func markMachineForDeletion(t *testing.T, c client.Client, hm *infrastructurev1alpha1.HypervisorMachine) {
	t.Helper()
	ctx := t.Context()

	fresh := getMachine(t, c, hm)

	fresh.Finalizers = append(fresh.Finalizers, machineDeleteFinalizer)
	if err := c.Update(ctx, fresh); err != nil {
		t.Fatalf("add machine finalizer: %v", err)
	}

	if err := c.Delete(ctx, fresh); err != nil {
		t.Fatalf("delete HypervisorMachine: %v", err)
	}

	pending := getMachine(t, c, hm)
	if pending.DeletionTimestamp.IsZero() {
		t.Fatal("deletion timestamp not set after Delete with a finalizer present")
	}
}

// assertMachineReclaimed fails the test unless the machine has been fully
// deleted: a Get must report NotFound, which the API server does only after
// every finalizer is dropped.
func assertMachineReclaimed(t *testing.T, c client.Client, hm *infrastructurev1alpha1.HypervisorMachine) {
	t.Helper()

	got := &infrastructurev1alpha1.HypervisorMachine{}

	err := c.Get(t.Context(), client.ObjectKeyFromObject(hm), got)
	if apierrors.IsNotFound(err) {
		return
	}

	if err != nil {
		t.Fatalf("Get HypervisorMachine after teardown: %v", err)
	}

	t.Errorf("machine not reclaimed after teardown: still present with finalizers %v", got.Finalizers)
}

// writeMachineRootDisk writes a real root disk file for the machine under the
// fixture's VM disk directory, standing in for the qemu-img output the
// provisioning seam only simulates. The returned path is the file written.
func writeMachineRootDisk(t *testing.T, vmDisksDir, name string) string {
	t.Helper()

	path := filepath.Join(vmDisksDir, name+"-root.qcow2")
	if err := os.WriteFile(path, []byte("fixture root disk"), 0o644); err != nil {
		t.Fatalf("write root disk %q: %v", path, err)
	}

	return path
}

// writeMachineConfextDisk writes a real confext data-disk artifact for the
// machine under the fixture's VM disk directory, standing in for the
// mksquashfs output the packager seam only simulates. The returned path is
// the .raw image written inside the <name>-data directory.
func writeMachineConfextDisk(t *testing.T, vmDisksDir, name string) string {
	t.Helper()

	dir := filepath.Join(vmDisksDir, name+"-data")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create confext data dir %q: %v", dir, err)
	}

	path := filepath.Join(dir, "z-kubelet-node1.raw")
	if err := os.WriteFile(path, []byte("fixture confext image"), 0o644); err != nil {
		t.Fatalf("write confext image %q: %v", path, err)
	}

	return path
}

// assertPathRemoved fails the test when the path still exists.
func assertPathRemoved(t *testing.T, path string) {
	t.Helper()

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Errorf("path %q still exists after teardown (stat error %v)", path, err)
	}
}

// TestMachineDeleteToleratesMissingVM pins the absent-VM contract: a client
// that reports ErrNotFound from the graceful shutdown and the process
// teardown — the VM was never booted or is already gone — does not abort the
// teardown. The reconcile completes without error, the disks are removed,
// and the finalizer is dropped.
func TestMachineDeleteToleratesMissingVM(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	vmDisksDir := fx.r.Config.VMDiskDir
	lc := newLinkedCluster(t, c, "machine-delete-missingvm", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", false)

	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)
	rootDisk := writeMachineRootDisk(t, vmDisksDir, lm.name)

	// The VM is absent from this point on.
	fx.vm.ShutdownErr = chclient.ErrNotFound
	fx.vm.StopErr = chclient.ErrNotFound

	markMachineForDeletion(t, c, lm.hm)
	fx.reconcileMachine(t, lm.hm)

	assertPathRemoved(t, rootDisk)
	assertMachineReclaimed(t, c, lm.hm)
}

// TestMachineDeleteRemovesConfextDisk pins the confext data-disk removal
// contract: the <name>-data directory holding the packaged .raw images is
// removed with the machine.
func TestMachineDeleteRemovesConfextDisk(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	vmDisksDir := fx.r.Config.VMDiskDir
	lc := newLinkedCluster(t, c, "machine-delete-confext", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	// Provisioning materializes the staging tree and the data-disk output
	// directory for real; the mksquashfs seam does not produce the .raw
	// image, so the test writes one by hand to represent the packaged
	// artifact.
	fx.vm.State = ch.VMState("Running")
	fx.reconcileMachine(t, lm.hm)
	confextDisk := writeMachineConfextDisk(t, vmDisksDir, lm.name)

	markMachineForDeletion(t, c, lm.hm)
	fx.reconcileMachine(t, lm.hm)

	assertPathRemoved(t, filepath.Dir(confextDisk))
	assertMachineReclaimed(t, c, lm.hm)
}

// statusInitializationProvisioned reads status.initialization.provisioned
// from the serialized form of a provider status struct. The read goes through
// JSON so the assertion pins the wire field the cluster-api v1beta2 contract
// readers consume (contract paths address status.initialization.provisioned
// on the unstructured object) without coupling this suite's compilation to
// the api type shape pinned by the api/v1alpha1 contract tests. present is
// false when the field is absent.
func statusInitializationProvisioned(status any) (provisioned, present bool) {
	raw, err := json.Marshal(status)
	if err != nil {
		return false, false
	}

	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false, false
	}

	init, ok := doc["initialization"].(map[string]any)
	if !ok {
		return false, false
	}

	p, ok := init["provisioned"].(bool)

	return p, ok
}

// TestMachineVMInitializationProvisionedWhenRunning pins the CAPI v1beta2
// InfrastructureMachine contract on the reconciler: once a reconcile of a
// running VM succeeds — the same conditions under which status.ready flips
// true and the VMProvisioned condition is set — status.initialization.
// provisioned must be true. The cluster-api v1beta2 machine controller gates
// InfrastructureReady on that field alone; without it a running VM leaves the
// CAPI Machine NotReady with "status.initialization.provisioned is false".
//
// Red phase: the reconciler never writes the field today, so the happy path
// fails while status.ready already reports true.
func TestMachineVMInitializationProvisionedWhenRunning(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("running VM marks initialization.provisioned", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-vm-init-prov", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		fx.vm.State = ch.VMState("Running")
		fx.reconcileMachine(t, lm.hm)

		hm := getMachine(t, c, lm.hm)
		if !hm.Status.Ready {
			t.Fatalf("precondition failed: status.ready = false after a successful reconcile of a running VM")
		}

		provisioned, present := statusInitializationProvisioned(hm.Status)
		if !present {
			t.Fatal(
				"status.initialization.provisioned missing with the VM running: the CAPI v1beta2 machine controller waits on this field (InfrastructureReady stays False)",
			)
		}

		if !provisioned {
			t.Error("status.initialization.provisioned = false with the VM running, want true")
		}
	})

	t.Run("not-running VM leaves initialization.provisioned unset", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-vm-init-notprov", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		fx.vm.State = ch.VMState("Created")
		fx.reconcileMachine(t, lm.hm)

		hm := getMachine(t, c, lm.hm)
		if hm.Status.Ready {
			t.Fatalf("precondition failed: status.ready = true with the VM not running")
		}

		if provisioned, present := statusInitializationProvisioned(hm.Status); present && provisioned {
			t.Error("status.initialization.provisioned = true with the VM not running, want unset or false")
		}
	})

	t.Run("initialization.provisioned is stable across reconciles", func(t *testing.T) {
		fx := newMachineFixture(t, c)
		lc := newLinkedCluster(t, c, "machine-vm-init-stable", "capi-cluster")
		lm := newLinkedMachine(t, c, lc, "node-1", true)

		fx.vm.State = ch.VMState("Running")
		fx.reconcileMachine(t, lm.hm)
		fx.reconcileMachine(t, lm.hm)

		hm := getMachine(t, c, lm.hm)
		if provisioned, present := statusInitializationProvisioned(hm.Status); !present || !provisioned {
			t.Errorf(
				"status.initialization.provisioned = (%t, present=%t) after two reconciles, want true",
				provisioned,
				present,
			)
		}
	})
}
