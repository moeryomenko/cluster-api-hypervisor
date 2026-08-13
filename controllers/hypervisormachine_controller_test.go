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
//     dependencies: Config (the provider paths), VM (the cloud-hypervisor
//     client), QemuImg (the qemu-img exec func), Confext (the confext
//     packager), RenderCloudInit (the CIDATA renderer), NewAllocator (the
//     per-cluster static-IP allocator constructor), and DeriveMAC (the MAC
//     derivation func). The tests build every dependency over a recording
//     seam and hand the fully constructed reconciler to the controller.
//   - Reconcile resolves the object, then the owning CAPI Machine (owner
//     reference), then the linked Cluster. A missing object, a machine with
//     no owning Machine, and a machine whose Cluster is missing are all
//     no-ops, not errors, and no dependency is touched.
//   - Identity: when spec.mac is empty the controller derives the MAC
//     through the injected derivation seam with the cluster and machine
//     names; a spec.mac override is used as-is and derivation is skipped.
//     The static IP comes from the cluster pool: the controller constructs
//     the allocator from the linked cluster's network config and the
//     documented pool bounds, re-asserts the addresses already recorded in
//     cluster machine status, allocates the first free address for the
//     machine, and records it in status.addresses together with the
//     hostname. A second reconcile keeps the same address, and a controller
//     restart (a fresh allocator) does not hand an address held by another
//     machine to a new machine.
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
	"strconv"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/networking"
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

// recordingExecRunner records every command invocation in call order and
// simulates the side effects the machine controller's exec seams depend on.
// It is used two ways: as the qemu-img exec func and as the command runner
// of the confext packager. For qemu-img the runner simulates a small virtual
// disk store: "convert" creates the destination disk, "resize" sets its
// virtual size, and "info" reports the stored size as qemu-img JSON (or
// fails when the disk is absent). For mksquashfs it records the call and
// returns the canned output, so packaging proceeds without a real
// squashfs-tools binary.
type recordingExecRunner struct {
	calls []recordedExecCall
	out   []byte
	err   error
	disks map[string]int64 // disk path -> virtual size in bytes
}

// newRecordingExecRunner builds an empty runner with a live disk store.
func newRecordingExecRunner() *recordingExecRunner {
	return &recordingExecRunner{disks: make(map[string]int64)}
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
	default:
		return nil, fmt.Errorf("recordingExecRunner: unexpected binary %q", name)
	}
}

// qemuImg simulates the qemu-img subcommands the machine controller issues.
// The disk path is the last argument of each invocation.
func (f *recordingExecRunner) qemuImg(args []string) ([]byte, error) {
	switch args[0] {
	case "convert":
		f.disks[args[len(args)-1]] = 0
		return f.out, nil
	case "resize":
		disk := args[len(args)-2]
		sizeMiB, err := strconv.ParseInt(strings.TrimSuffix(args[len(args)-1], "M"), 10, 64)
		if err != nil {
			return nil, err
		}
		f.disks[disk] = sizeMiB * 1024 * 1024
		return f.out, nil
	case "info":
		disk := args[len(args)-1]
		size, ok := f.disks[disk]
		if !ok {
			return nil, fmt.Errorf("qemu-img: %s: No such file or directory", disk)
		}
		return []byte(fmt.Sprintf(`{"virtual-size": %d, "format": "qcow2", "filename": %q}`, size, disk)), nil
	default:
		return nil, fmt.Errorf("recordingExecRunner: unexpected qemu-img subcommand %q", args[0])
	}
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
// withBootstrap is false the Machine is left without a bootstrap config ref,
// so the controller's confext step has no Secret to read.
func newLinkedMachine(t *testing.T, c client.Client, lc *linkedCluster, name string, withBootstrap bool) *linkedMachine {
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
		lm.config.Status.DataSecretName = stringPtr(name + "-data")
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
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: infrastructurev1alpha1.GroupVersion.String(),
				Kind:       "HypervisorMachine",
				Name:       name,
				Namespace:  lc.namespace,
			},
		},
	}
	if withBootstrap {
		lm.machine.Spec.Bootstrap.ConfigRef = &corev1.ObjectReference{
			APIVersion: bootstrapv1alpha1.GroupVersion.String(),
			Kind:       "HypervisorConfig",
			Name:       lm.config.Name,
			Namespace:  lc.namespace,
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
	alloc  *recordingAllocator
	vm     *chclient.FakeClient
	net    *recordingLinkOps
}

// newMachineFixture builds the reconciler under test over the recording
// seams. The composite literal pins the exact reconciler shape the
// implementation must expose: the controller-runtime wiring plus the
// injectable Config, Net, VM, QemuImg, Confext, RenderCloudInit,
// NewAllocator, and DeriveMAC dependencies.
func newMachineFixture(t *testing.T, c client.Client) *machineFixture {
	t.Helper()

	qemu := newRecordingExecRunner()
	pack := newRecordingExecRunner()
	derive := &recordingMACDerive{}
	render := &recordingCIDATARender{}
	alloc := &recordingAllocator{}
	vm := &chclient.FakeClient{}
	netOps := newRecordingLinkOps()
	// The cluster controller ensures the bridge before the machine
	// controller's TAP step; the suite seeds it as pre-existing host state.
	netOps.links[testBridge] = fakeLink{kind: "bridge"}

	r := &HypervisorMachineReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(16),
		Config: config.Config{
			BaseImage: testBaseImage,
			VMDiskDir: t.TempDir(),
			SocketDir: testSocketDir,
		},
		VM:              vm,
		Net:             networking.NewManager(netOps),
		QemuImg:         qemu.Run,
		Confext:         confext.NewPackager(confext.WithRunner(pack)),
		RenderCloudInit: render.render,
		NewAllocator:    alloc.alloc,
		DeriveMAC:       derive.derive,
	}

	return &machineFixture{r: r, qemu: qemu, pack: pack, derive: derive, render: render, alloc: alloc, vm: vm, net: netOps}
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

// stringPtr returns a pointer to s.
func stringPtr(s string) *string {
	return &s
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
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: infrastructurev1alpha1.GroupVersion.String(),
					Kind:       "HypervisorMachine",
					Name:       "clusterless",
					Namespace:  lc.namespace,
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

// TestMachineIdentityStaticIPAllocatedAndStable pins the static IP
// allocation contract: the first reconcile allocates the first free address
// of the cluster pool, records it in status.addresses alongside the
// hostname, and a second reconcile keeps the same address. The allocator is
// constructed once per reconcile from the cluster network config and the
// documented pool bounds.
func TestMachineIdentityStaticIPAllocatedAndStable(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-ip", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)
	key := client.ObjectKeyFromObject(lm.hm)

	ip := ""
	for i := 0; i < 2; i++ {
		if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
			t.Fatalf("Reconcile %d error: %v", i+1, err)
		}

		hm := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), key, hm); err != nil {
			t.Fatalf("Get HypervisorMachine: %v", err)
		}
		got := statusInternalIP(hm)
		if got == "" {
			t.Fatalf("reconcile %d recorded no internal IP in status", i+1)
		}
		if i == 0 {
			ip = got
			if ip != testPoolStart {
				t.Errorf("first allocated IP = %q, want the pool start %q", ip, testPoolStart)
			}
			if host := statusHostName(hm); host != lm.name {
				t.Errorf("status hostname = %q, want %q", host, lm.name)
			}
		} else if got != ip {
			t.Errorf("reconcile %d changed the allocated IP from %q to %q", i+1, ip, got)
		}
	}

	// The allocator is constructed once per reconcile, from the cluster's
	// CIDR, gateway, and the documented default pool bounds.
	if fx.alloc.calls != 2 {
		t.Errorf("allocator constructed %d times, want 2", fx.alloc.calls)
	}
	if fx.alloc.cidr != testCIDR || fx.alloc.gateway != testGateway ||
		fx.alloc.start != testPoolStart || fx.alloc.end != testPoolEnd {
		t.Errorf("allocator args = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
			fx.alloc.cidr, fx.alloc.gateway, fx.alloc.start, fx.alloc.end,
			testCIDR, testGateway, testPoolStart, testPoolEnd)
	}

	// The VM lifecycle runs on every reconcile: the first reconcile boots
	// the VM through the client and re-checks its state, and the second
	// reconcile only re-checks the state (the boot is skipped once the
	// provider ID is recorded).
	wantVMCalls := []string{"EnsureRunning", "Info", "Info"}
	if !reflect.DeepEqual(fx.vm.Calls, wantVMCalls) {
		t.Errorf("VM client calls = %v, want %v", fx.vm.Calls, wantVMCalls)
	}
}

// TestMachineIdentityStaticIPNoCollisionAcrossRestart pins the
// status-driven re-assertion contract. The allocator is in-memory per
// reconcile, so after a provider restart the pool bookkeeping is lost; the
// controller must re-assert the addresses already recorded in machine status
// and hand a new machine a free address. Without that, a new machine would
// collide with an existing one.
func TestMachineIdentityStaticIPNoCollisionAcrossRestart(t *testing.T) {
	c := mustReconcileClient(t)
	lc := newLinkedCluster(t, c, "machine-ip-restart", "capi-cluster")

	first := newMachineFixture(t, c)
	lm1 := newLinkedMachine(t, c, lc, "node-1", true)
	lm2 := newLinkedMachine(t, c, lc, "node-2", true)

	for _, lm := range []*linkedMachine{lm1, lm2} {
		if _, err := first.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm.hm)}); err != nil {
			t.Fatalf("first reconcile of %q error: %v", lm.name, err)
		}
	}

	// A fresh controller over the same API state, as after a provider
	// restart: the in-memory allocator starts empty.
	second := newMachineFixture(t, c)
	lm3 := newLinkedMachine(t, c, lc, "node-3", true)

	// Re-reconciling an existing machine must keep its recorded address.
	if _, err := second.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm1.hm)}); err != nil {
		t.Fatalf("restart reconcile of %q error: %v", lm1.name, err)
	}
	// A brand-new machine must receive an address no existing machine holds.
	if _, err := second.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lm3.hm)}); err != nil {
		t.Fatalf("restart reconcile of %q error: %v", lm3.name, err)
	}

	ips := make(map[string]string)
	for _, lm := range []*linkedMachine{lm1, lm2, lm3} {
		hm := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), client.ObjectKeyFromObject(lm.hm), hm); err != nil {
			t.Fatalf("Get HypervisorMachine %q: %v", lm.name, err)
		}
		ips[lm.name] = statusInternalIP(hm)
	}
	if ips["node-1"] != testPoolStart {
		t.Errorf("node-1 address = %q, want %q", ips["node-1"], testPoolStart)
	}
	if ips["node-2"] == "" || ips["node-3"] == "" {
		t.Fatalf("node-2 or node-3 hold no address: %v", ips)
	}

	seen := make(map[string]string)
	for name, ip := range ips {
		if other, ok := seen[ip]; ok {
			t.Errorf("address %q shared by %q and %q", ip, other, name)
		}
		seen[ip] = name
	}
}

// TestMachineDisksRootDiskQemuImgArgs pins the root disk provisioning
// contract: the first reconcile converts the configured base image into the
// machine root disk and resizes it to the spec size, with the exact qemu-img
// argument shapes.
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

		for i := 0; i < 2; i++ {
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

		for i := 0; i < 2; i++ {
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

	if d.IP != ip {
		t.Errorf("render IP = %q, want the allocated %q", d.IP, ip)
	}
	if d.Hostname != lm.name {
		t.Errorf("render hostname = %q, want %q", d.Hostname, lm.name)
	}
	if d.Gateway != testGateway || d.DNS != testDNSIP {
		t.Errorf("render gateway/dns = (%q, %q), want (%q, %q)", d.Gateway, d.DNS, testGateway, testDNSIP)
	}
	if d.InstanceID == "" {
		t.Error("render instance id is empty")
	}
	if d.SSHPublicKey != testSSHPublicKey {
		t.Errorf("render ssh public key = %q, want %q", d.SSHPublicKey, testSSHPublicKey)
	}

	// The real renderer emits a network-config that addresses the node with
	// the allocated IP, alongside the user-data and meta-data parts.
	parts, err := cloudinit.Render(d)
	if err != nil {
		t.Fatalf("cloudinit.Render: %v", err)
	}
	if networkConfig := string(parts["network-config"]); !strings.Contains(networkConfig, ip+"/24") {
		t.Errorf("network-config does not address %s/24:\n%s", ip, networkConfig)
	}
	for _, part := range []string{"user-data", "meta-data", "network-config"} {
		if len(parts[part]) == 0 {
			t.Errorf("rendered part %q is empty", part)
		}
	}
}

// HypervisorMachine controller contract, part 2: the VM lifecycle (reconcile
// steps 6-8).
//
// This section pins the contract the machine controller's lifecycle steps
// implement over the same envtest harness and recording fakes:
//
//   - HypervisorMachineReconciler gains a Net field, a *networking.Manager
//     over the recording LinkOps seam, used to ensure the machine TAP; the
//     fixture constructs it with a fake LinkOps and seeds the cluster bridge
//     as pre-existing host state (the cluster controller ensures the bridge
//     before the machine controller's TAP step).
//   - Step 6 ensures the TAP: Net.EnsureTap is called with the bridge name
//     from the linked HypervisorCluster's network spec (default k8sbr0) and
//     the machine TAP name k8s-<machine>. An absent TAP is created with kind
//     tuntap and enslaved to the bridge, and an already-enslaved TAP is left
//     alone on the next reconcile.
//   - Step 7 boots the VM through the injected chclient.Client: the fake's
//     call log records one EnsureRunning call followed by an Info state
//     check. The per-machine socket directory derives from the config's
//     socket root as <SocketDir>/<cluster>/<machine>; the fixture pins the
//     documented default socket root /tmp/ch-capi. The socket path itself is
//     not observable through the fake's call log because the client seam
//     exposes no path, so this section pins the root the implementation
//     derives the per-machine directory from. The client's EnsureRunning is
//     idempotent by contract (a no-op when the VM is already running), so
//     the controller has no reason to pre-check the state.
//   - Step 8 reports readiness: status.ready is true exactly when Info
//     reports the VM running, and stays false when the VM is in a
//     non-running state or the state query fails; both leave the reconcile
//     without error.
//   - The lifecycle steps set status.providerID to
//     hypervisor://<cluster>/<machine>, set the VMProvisioned condition once
//     the VM runs, and leave the addresses allocated in part 1 in place.
const (
	// testSocketDir is the provider's socket root the fixture config pins;
	// the VM lifecycle step derives the per-machine socket directory
	// <SocketDir>/<cluster>/<machine> from it.
	testSocketDir = "/tmp/ch-capi"

	// machineTapPrefix is the TAP name prefix the lifecycle step uses for a
	// machine's TAP: <tapPrefix>-<machine>.
	machineTapPrefix = "k8s-"

	// machineVMProvisionedCondition is the condition type the VM lifecycle
	// step reports once the cloud-hypervisor VM is provisioned.
	machineVMProvisionedCondition = clusterv1.ConditionType("VMProvisioned")
)

// Compile-time pin for the TAP seam the VM lifecycle step drives: the
// reconciler's Net field is a networking.Manager over the LinkOps seam, and
// the TAP step drives it through EnsureTap.
var _ func(*networking.Manager, string, string) error = (*networking.Manager).EnsureTap

// reconcileMachine runs one reconcile of the machine and fails the test on
// any error.
func (fx *machineFixture) reconcileMachine(t *testing.T, hm *infrastructurev1alpha1.HypervisorMachine) {
	t.Helper()
	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(hm)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
}

// getMachine reads the machine back from the API store.
func getMachine(t *testing.T, c client.Client, hm *infrastructurev1alpha1.HypervisorMachine) *infrastructurev1alpha1.HypervisorMachine {
	t.Helper()
	got := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(hm), got); err != nil {
		t.Fatalf("Get HypervisorMachine: %v", err)
	}

	return got
}

// machineCondition returns the status condition with the given type, or nil.
func machineCondition(hm *infrastructurev1alpha1.HypervisorMachine, t clusterv1.ConditionType) *clusterv1.Condition {
	for i := range hm.Status.Conditions {
		if hm.Status.Conditions[i].Type == t {
			return &hm.Status.Conditions[i]
		}
	}

	return nil
}

// TestMachineVMTapCreatedAndEnslaved pins the TAP step contract: the
// controller drives the networking manager with the bridge name from the
// linked HypervisorCluster's network spec and the machine TAP name
// k8s-<machine>. The absent TAP is created with kind tuntap and enslaved to
// the bridge, in that order, and a second reconcile leaves an already
// enslaved TAP alone.
func TestMachineVMTapCreatedAndEnslaved(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newMachineFixture(t, c)
	lc := newLinkedCluster(t, c, "machine-vm-tap", "capi-cluster")
	lm := newLinkedMachine(t, c, lc, "node-1", true)

	fx.reconcileMachine(t, lm.hm)

	wantTap := machineTapPrefix + lm.name
	wantLinkCalls(t, fx.net,
		linkOpCall{op: opByName, name: wantTap},
		linkOpCall{op: opAdd, kind: "tuntap", name: wantTap},
		linkOpCall{op: opSetMaster, name: wantTap, master: testBridge},
	)

	// The fake's kernel state confirms the enslavement, not just the call
	// log.
	tap, err := fx.net.LinkByName(wantTap)
	if err != nil {
		t.Fatalf("LinkByName(%q) error: %v", wantTap, err)
	}
	if tap.Kind != "tuntap" || tap.Master != testBridge {
		t.Errorf("TAP = %+v, want kind tuntap enslaved to %q", tap, testBridge)
	}

	t.Run("second reconcile does not recreate an enslaved tap", func(t *testing.T) {
		fx.reconcileMachine(t, lm.hm)

		// The second reconcile finds the TAP already mastered to the bridge
		// and stops after the lookup.
		wantLinkCalls(t, fx.net,
			linkOpCall{op: opByName, name: wantTap},
			linkOpCall{op: opAdd, kind: "tuntap", name: wantTap},
			linkOpCall{op: opSetMaster, name: wantTap, master: testBridge},
			linkOpCall{op: opByName, name: wantTap},
		)
	})
}

// TestMachineVMBootedViaClient pins the boot step contract: the controller
// drives the injected chclient.Client with a single EnsureRunning call
// followed by an Info state check, and the fixture config pins the socket
// root the per-machine socket directory /tmp/ch-capi/<cluster>/<machine>
// derives from.
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

	// The per-machine socket directory is <SocketDir>/<cluster>/<machine>;
	// the fake client seam exposes no path, so the fixture pins the socket
	// root the implementation derives it from.
	if got := fx.r.Config.SocketDir; got != testSocketDir {
		t.Errorf("Config.SocketDir = %q, want %q", got, testSocketDir)
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
	if cond.Status != corev1.ConditionTrue {
		t.Errorf("condition %q status = %q, want %q", machineVMProvisionedCondition, cond.Status, corev1.ConditionTrue)
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
