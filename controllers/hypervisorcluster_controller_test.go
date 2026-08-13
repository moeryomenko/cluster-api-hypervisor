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

// HypervisorCluster controller contract (test-first, red).
//
// This suite pins the contract for the HypervisorCluster reconciler that
// provisions and tears down the host network stack for one cluster: the lab
// bridge, the dnsmasq DNS forwarder, and the nftables NAT table. The
// reconciler is exercised through the committed envtest harness with
// recording fakes standing in for every host-side effect, so no real
// netlink, nft, or dnsmasq state is ever touched.
//
// The contract, in prose:
//
//   - HypervisorClusterReconciler carries the controller-runtime wiring
//     (embedded client.Client, Scheme, Recorder) plus the injectable host
//     network stack: Net (the bridge/TAP orchestrator), Nft (the NAT table
//     manager), Dnsmasq (the DNS forwarder manager), and NewAllocator, the
//     per-cluster static-IP allocator constructor. The tests build every
//     dependency over a recording seam and hand the fully constructed
//     managers to the reconciler, so the controller never touches the host.
//   - Reconcile resolves the object, then the linked CAPI Cluster (owner
//     reference or clusterName link), then applies the paused gate: an
//     object carrying the standard paused annotation is left untouched with
//     no reconcile actions. A missing object and a missing linked Cluster
//     are both no-ops, not errors.
//   - Normal reconcile ensures the bridge exists, starts dnsmasq, applies
//     the NAT ruleset, and constructs the IPAM allocator from the cluster's
//     network config. When every step succeeds the object is marked ready
//     with the InfrastructureReady condition true. When the linked control
//     plane reports initialized and a control-plane machine holds a static
//     IP, the control-plane endpoint is published with that IP on port
//     6443; an absent or uninitialized control plane leaves the endpoint
//     empty without error.
//   - Delete reconcile (deletion timestamp set, finalizer present) stops
//     dnsmasq, deletes the NAT table, and removes the bridge, then drops
//     the finalizer so the object is reclaimed. Teardown is idempotent: a
//     later reconcile of the missing object adds no further calls.
//   - Every dependency failure surfaces as a reconcile error that preserves
//     the underlying error, aborts the provisioning sequence at the failing
//     step, and leaves the object not ready.
package controllers

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/dnsmasq"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ipam"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/networking"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/nft"
	"github.com/moeryomenko/cluster-api-hypervisor/test/helpers"
)

// Cluster network fixture: the default lab network, plus the documented
// default static-IP pool bounds the allocator is constructed with.
const (
	testBridge    = "k8sbr0"
	testCIDR      = "192.168.124.0/24"
	testGateway   = "192.168.124.1"
	testDNSIP     = "192.168.124.1"
	testNATTable  = "k8slab"
	testPoolStart = "192.168.124.20"
	testPoolEnd   = "192.168.124.200"
	testCPPort    = 6443
	testCPIP      = "192.168.124.20"
)

// Compile-time pins for the injected seams and for the reconciler shape the
// implementation must expose: the four dependencies, the Reconcile
// signature, and the fake seam implementations. Until the reconciler type
// exists the package does not compile — that is the intended red phase.
var (
	_ func(networking.LinkOps) *networking.Manager                  = networking.NewManager
	_ func(string, string, nft.Runner) *nft.Manager                 = nft.NewManager
	_ func(dnsmasq.Config, dnsmasq.Runner, string) *dnsmasq.Manager = dnsmasq.NewManager
	_ func(string, string, string, string) (*ipam.Allocator, error) = ipam.NewAllocator
	_ networking.LinkOps                                            = (*recordingLinkOps)(nil)
	_ nft.Runner                                                    = (*recordingNftRunner)(nil)
	_ dnsmasq.Runner                                                = (*recordingDnsmasqRunner)(nil)
	_ func(context.Context, ctrl.Request) (ctrl.Result, error)      = (*HypervisorClusterReconciler)(nil).Reconcile
)

// fakeLink is the recording seam's view of a kernel link: kind and master
// bridge only, which is all the contract uses.
type fakeLink struct {
	kind   string
	master string
}

// linkOpCall records one invocation of the link ops seam.
type linkOpCall struct {
	op     string
	kind   string
	name   string
	master string
}

// Link operations recorded by the fake.
const (
	opByName    = "by-name"
	opAdd       = "add"
	opSetMaster = "set-master"
	opDel       = "del"
)

// recordingLinkOps is an in-memory LinkOps. It records every call in
// invocation order, simulates kernel link state, and can fail an operation
// with an injected error (see failBy, keyed by the op constants).
type recordingLinkOps struct {
	links  map[string]fakeLink
	calls  []linkOpCall
	failBy map[string]error
}

// LinkByName implements networking.LinkOps: it records the call and returns
// the link, or ErrLinkNotFound when no such link exists.
func (f *recordingLinkOps) LinkByName(name string) (networking.Link, error) {
	f.calls = append(f.calls, linkOpCall{op: opByName, name: name})
	if err := f.fail(opByName); err != nil {
		return networking.Link{}, err
	}
	l, ok := f.links[name]
	if !ok {
		return networking.Link{}, networking.ErrLinkNotFound
	}

	return networking.Link{Name: name, Kind: l.kind, Master: l.master}, nil
}

// LinkAdd implements networking.LinkOps: it records the call and creates the
// link. Creating a link that already exists fails, as a correct manager never
// double-creates.
func (f *recordingLinkOps) LinkAdd(kind, name string) error {
	f.calls = append(f.calls, linkOpCall{op: opAdd, kind: kind, name: name})
	if err := f.fail(opAdd); err != nil {
		return err
	}
	if _, ok := f.links[name]; ok {
		return fmt.Errorf("fake: link %q already exists", name)
	}
	f.links[name] = fakeLink{kind: kind}

	return nil
}

// LinkSetMaster implements networking.LinkOps: it records the call and
// enslaves the named link to the named master bridge.
func (f *recordingLinkOps) LinkSetMaster(name, master string) error {
	f.calls = append(f.calls, linkOpCall{op: opSetMaster, name: name, master: master})
	if err := f.fail(opSetMaster); err != nil {
		return err
	}
	l, ok := f.links[name]
	if !ok {
		return networking.ErrLinkNotFound
	}
	l.master = master
	f.links[name] = l

	return nil
}

// LinkDel implements networking.LinkOps: it records the call and removes the
// link. Deleting a missing link returns ErrLinkNotFound so the manager can
// treat deletion as idempotent.
func (f *recordingLinkOps) LinkDel(name string) error {
	f.calls = append(f.calls, linkOpCall{op: opDel, name: name})
	if err := f.fail(opDel); err != nil {
		return err
	}
	if _, ok := f.links[name]; !ok {
		return networking.ErrLinkNotFound
	}
	delete(f.links, name)

	return nil
}

// fail returns the injected error for the operation key, if any.
func (f *recordingLinkOps) fail(key string) error {
	return f.failBy[key]
}

// newRecordingLinkOps builds an empty fake with a live link table.
func newRecordingLinkOps() *recordingLinkOps {
	return &recordingLinkOps{
		links:  make(map[string]fakeLink),
		failBy: make(map[string]error),
	}
}

// wantLinkCalls asserts the recorded link-op log matches the expected calls
// exactly, in order.
func wantLinkCalls(t *testing.T, f *recordingLinkOps, want ...linkOpCall) {
	t.Helper()
	if len(f.calls) != len(want) {
		t.Fatalf("link call log = %v, want %v", f.calls, want)
	}
	for i := range want {
		if f.calls[i] != want[i] {
			t.Fatalf("link call %d = %v, want %v (full log %v)", i, f.calls[i], want[i], f.calls)
		}
	}
}

// recordedNftCall records one invocation of the nft runner seam: the binary,
// the exact arguments, and the bytes read from stdin (nil when none).
type recordedNftCall struct {
	name string
	args []string
	in   []byte
}

// recordingNftRunner records every nft invocation. When err is set every
// invocation returns it, standing in for a rejected ruleset or a failed
// delete.
type recordingNftRunner struct {
	calls []recordedNftCall
	err   error
}

// Run implements nft.Runner. The context is accepted and ignored:
// cancellation propagation is the default exec runner's concern, not this
// contract's.
func (f *recordingNftRunner) Run(_ context.Context, name string, args []string, stdin io.Reader) ([]byte, error) {
	argsCopy := append([]string(nil), args...)
	var in []byte
	if stdin != nil {
		in, _ = io.ReadAll(stdin)
	}
	f.calls = append(f.calls, recordedNftCall{name: name, args: argsCopy, in: in})
	if f.err != nil {
		return nil, f.err
	}

	return nil, nil
}

// argLog returns the argument list of every recorded invocation.
func (f *recordingNftRunner) argLog() [][]string {
	args := make([][]string, 0, len(f.calls))
	for _, c := range f.calls {
		args = append(args, c.args)
	}

	return args
}

// recordingDnsmasqRunner records start/stop invocations. When startErr is
// set a Start returns it, standing in for a failed subprocess launch.
type recordingDnsmasqRunner struct {
	calls    []string
	startErr error
}

// Start implements dnsmasq.Runner.
func (f *recordingDnsmasqRunner) Start(_ context.Context, name string, _ []string, _, _ io.Writer) error {
	f.calls = append(f.calls, "start:"+name)
	if f.startErr != nil {
		return f.startErr
	}

	return nil
}

// Stop implements dnsmasq.Runner.
func (f *recordingDnsmasqRunner) Stop(context.Context) error {
	f.calls = append(f.calls, "stop")

	return nil
}

// recordingAllocator records every constructor invocation and either builds
// a real per-cluster allocator or, when err is set, returns the injected
// error.
type recordingAllocator struct {
	calls                     int
	cidr, gateway, start, end string
	err                       error
}

// alloc is the injected NewAllocator implementation.
func (a *recordingAllocator) alloc(clusterCIDR, gateway, poolStart, poolEnd string) (*ipam.Allocator, error) {
	a.calls++
	a.cidr, a.gateway, a.start, a.end = clusterCIDR, gateway, poolStart, poolEnd
	if a.err != nil {
		return nil, a.err
	}

	return ipam.NewAllocator(clusterCIDR, gateway, poolStart, poolEnd)
}

// newScheme registers every group the suite touches: the core client-go
// types, the three provider groups, and the cluster-api core types the
// reconciler reads (Cluster, Machine).
func newScheme() *runtime.Scheme {
	s := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(s)
	_ = infrastructurev1alpha1.AddToScheme(s)
	_ = bootstrapv1alpha1.AddToScheme(s)
	_ = controlplanev1alpha1.AddToScheme(s)
	_ = clusterv1.AddToScheme(s)

	return s
}

// mustReconcileClient starts the envtest control plane through the committed
// harness, installs the two cluster-api core CRDs the endpoint contract
// depends on (Cluster, Machine) from the module cache, and returns a client
// with the extended scheme above.
func mustReconcileClient(t *testing.T) client.Client {
	t.Helper()

	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}
	installCAPICoreCRDs(t, envTest.Env.Config)

	c, err := client.New(envTest.Env.Config, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatalf("create test client: %v", err)
	}

	return c
}

// installCAPICoreCRDs installs the cluster-api core CRDs the endpoint
// contract reads (Cluster and Machine). The provider CRDs are already
// installed by the harness; the cluster-api CRDs ship in the module cache of
// the pinned cluster-api version, resolved through `go list -m`.
func installCAPICoreCRDs(t *testing.T, cfg *rest.Config) {
	t.Helper()

	dir, err := capiCRDDirectory()
	if err != nil {
		t.Fatalf("resolve cluster-api module directory: %v", err)
	}
	paths := []string{
		filepath.Join(dir, "cluster.x-k8s.io_clusters.yaml"),
		filepath.Join(dir, "cluster.x-k8s.io_machines.yaml"),
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{Paths: paths}); err != nil {
		t.Fatalf("install cluster-api core CRDs: %v", err)
	}
}

// capiCRDDirectory resolves the CRD manifest directory of the pinned
// sigs.k8s.io/cluster-api module.
func capiCRDDirectory() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api").Output()
	if err != nil {
		return "", fmt.Errorf("go list -m sigs.k8s.io/cluster-api: %w", err)
	}

	return filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases"), nil
}

// linkedCluster is the minimal CAPI linkage the reconciler contract reads: a
// Cluster whose infrastructureRef points at the HypervisorCluster, whose
// owner reference links the HypervisorCluster back, and whose name matches
// the HypervisorCluster's clusterName.
type linkedCluster struct {
	namespace string
	name      string
	hc        *infrastructurev1alpha1.HypervisorCluster
	cluster   *clusterv1.Cluster
}

// key returns the object key of the HypervisorCluster.
func (l *linkedCluster) key() client.ObjectKey {
	return client.ObjectKey{Namespace: l.namespace, Name: l.name}
}

// newLinkedCluster creates the namespace, the CAPI Cluster, and the
// HypervisorCluster, wired exactly as CAPI links them: the Cluster's
// infrastructureRef names the HypervisorCluster, and the HypervisorCluster
// carries the Cluster owner reference plus the clusterName link.
func newLinkedCluster(t *testing.T, c client.Client, namespace, name string) *linkedCluster {
	t.Helper()
	ctx := t.Context()

	if err := c.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace %q: %v", namespace, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Delete(cleanupCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	})

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: &corev1.ObjectReference{
				APIVersion: infrastructurev1alpha1.GroupVersion.String(),
				Kind:       "HypervisorCluster",
				Name:       name,
				Namespace:  namespace,
			},
		},
	}
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatalf("create Cluster %q: %v", name, err)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Cluster",
					Name:       cluster.Name,
					UID:        cluster.UID,
				},
			},
		},
		Spec: infrastructurev1alpha1.HypervisorClusterSpec{
			ClusterName: name,
			Network: infrastructurev1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       testCIDR,
				Gateway:    testGateway,
				DNSIP:      testDNSIP,
				BridgeName: testBridge,
				NATTable:   testNATTable,
			},
		},
	}
	if err := c.Create(ctx, hc); err != nil {
		t.Fatalf("create HypervisorCluster %q: %v", name, err)
	}

	return &linkedCluster{namespace: namespace, name: name, hc: hc, cluster: cluster}
}

// newControlPlane creates a HypervisorControlPlane linked through the
// Cluster's controlPlaneRef, with the given initialized status. Status is
// written through the status subresource, so the object is created and then
// patched.
func newControlPlane(t *testing.T, c client.Client, lc *linkedCluster, initialized bool) {
	t.Helper()
	ctx := t.Context()

	cp := &controlplanev1alpha1.HypervisorControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: lc.name + "-cp", Namespace: lc.namespace},
		Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
			Replicas: 1,
			MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: infrastructurev1alpha1.GroupVersion.String(),
					Kind:       "HypervisorMachineTemplate",
					Name:       lc.name + "-machine-template",
					Namespace:  lc.namespace,
				},
			},
		},
	}
	if err := c.Create(ctx, cp); err != nil {
		t.Fatalf("create HypervisorControlPlane: %v", err)
	}
	cp.Status.Initialized = initialized
	if err := c.Status().Update(ctx, cp); err != nil {
		t.Fatalf("set HypervisorControlPlane initialized status: %v", err)
	}

	lc.cluster.Spec.ControlPlaneRef = &corev1.ObjectReference{
		APIVersion: controlplanev1alpha1.GroupVersion.String(),
		Kind:       "HypervisorControlPlane",
		Name:       cp.Name,
		Namespace:  lc.namespace,
	}
	if err := c.Update(ctx, lc.cluster); err != nil {
		t.Fatalf("link control plane ref on Cluster: %v", err)
	}
}

// newControlPlaneMachine creates one control-plane machine: a CAPI Machine
// carrying the control-plane and cluster labels, backed by a
// HypervisorMachine whose status records the given static IP.
func newControlPlaneMachine(t *testing.T, c client.Client, lc *linkedCluster, ip string) {
	t.Helper()
	ctx := t.Context()

	hm := &infrastructurev1alpha1.HypervisorMachine{
		ObjectMeta: metav1.ObjectMeta{Name: lc.name + "-cp-0", Namespace: lc.namespace},
		Spec: infrastructurev1alpha1.HypervisorMachineSpec{
			ClusterName: lc.name,
		},
	}
	if err := c.Create(ctx, hm); err != nil {
		t.Fatalf("create HypervisorMachine: %v", err)
	}
	hm.Status.Addresses = []clusterv1.MachineAddress{
		{Type: clusterv1.MachineInternalIP, Address: ip},
	}
	if err := c.Status().Update(ctx, hm); err != nil {
		t.Fatalf("set HypervisorMachine addresses: %v", err)
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      lc.name + "-cp-0",
			Namespace: lc.namespace,
			Labels: map[string]string{
				clusterv1.ClusterNameLabel:         lc.name,
				clusterv1.MachineControlPlaneLabel: "",
			},
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: lc.name,
			Bootstrap:   clusterv1.Bootstrap{},
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: infrastructurev1alpha1.GroupVersion.String(),
				Kind:       "HypervisorMachine",
				Name:       hm.Name,
				Namespace:  lc.namespace,
			},
		},
	}
	if err := c.Create(ctx, machine); err != nil {
		t.Fatalf("create control-plane Machine: %v", err)
	}
}

// newTestReconciler builds the reconciler under test over the recording
// seams: a networking manager over the fake link ops, an nft manager over
// the fake runner, a dnsmasq manager over the fake runner writing its
// rendered config into a fresh temp dir, and the injected allocator
// constructor. This composite literal pins the exact reconciler shape the
// implementation must expose.
func newTestReconciler(t *testing.T, c client.Client, ops *recordingLinkOps, dnsRunner *recordingDnsmasqRunner, nftRunner *recordingNftRunner, allocator *recordingAllocator) *HypervisorClusterReconciler {
	t.Helper()

	return &HypervisorClusterReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(16),
		Net:      networking.NewManager(ops),
		Nft:      nft.NewManager(testBridge, testNATTable, nftRunner),
		Dnsmasq: dnsmasq.NewManager(dnsmasq.Config{
			BridgeName:    testBridge,
			ListenAddress: testDNSIP,
			Upstream:      []string{"1.1.1.1"},
		}, dnsRunner, t.TempDir()),
		NewAllocator: allocator.alloc,
	}
}

// findCondition returns the condition with the given type, or nil.
func findCondition(hc *infrastructurev1alpha1.HypervisorCluster, t clusterv1.ConditionType) *clusterv1.Condition {
	for i := range hc.Status.Conditions {
		if hc.Status.Conditions[i].Type == t {
			return &hc.Status.Conditions[i]
		}
	}

	return nil
}

// TestReconcileProvisionsNetworkStack pins the normal reconcile contract:
// the bridge is ensured, dnsmasq is started, the NAT ruleset is applied, and
// the allocator is constructed from the cluster's network config. The object
// also gains a finalizer so deletion is intercepted.
func TestReconcileProvisionsNetworkStack(t *testing.T) {
	c := mustReconcileClient(t)
	ops := newRecordingLinkOps()
	dnsRunner := &recordingDnsmasqRunner{}
	nftRunner := &recordingNftRunner{}
	allocator := &recordingAllocator{}
	r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

	lc := newLinkedCluster(t, c, "hc-provision", "capi-cluster")

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want empty (no requeue)", res)
	}

	// The exact provisioning operations: bridge lookup then create, one
	// dnsmasq start, one nft apply reading the ruleset from stdin.
	wantLinkCalls(t, ops,
		linkOpCall{op: opByName, name: testBridge},
		linkOpCall{op: opAdd, kind: "bridge", name: testBridge},
	)
	wantNft := [][]string{{"-f", "-"}}
	if !reflect.DeepEqual(nftRunner.argLog(), wantNft) {
		t.Errorf("nft invocations = %v, want %v", nftRunner.argLog(), wantNft)
	}
	wantStart := []string{"start:dnsmasq"}
	if !reflect.DeepEqual(dnsRunner.calls, wantStart) {
		t.Errorf("dnsmasq invocations = %v, want %v", dnsRunner.calls, wantStart)
	}

	// The allocator is constructed once, from the cluster's CIDR, gateway,
	// and the documented default pool bounds.
	if allocator.calls != 1 {
		t.Errorf("allocator constructed %d times, want 1", allocator.calls)
	}
	if allocator.cidr != testCIDR || allocator.gateway != testGateway ||
		allocator.start != testPoolStart || allocator.end != testPoolEnd {
		t.Errorf("allocator args = (%q, %q, %q, %q), want (%q, %q, %q, %q)",
			allocator.cidr, allocator.gateway, allocator.start, allocator.end,
			testCIDR, testGateway, testPoolStart, testPoolEnd)
	}

	// A finalizer guards the object so deletion is intercepted later.
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if len(hc.Finalizers) != 1 {
		t.Errorf("finalizers = %v, want exactly one", hc.Finalizers)
	}
}

// TestReconcileMarksReadyAndCondition pins the status contract: once the
// network stack is provisioned the object reports ready with the
// InfrastructureReady condition true, and a second reconcile re-ensures the
// stack without error (the bridge create is not repeated) while leaving the
// status stable.
func TestReconcileMarksReadyAndCondition(t *testing.T) {
	c := mustReconcileClient(t)
	ops := newRecordingLinkOps()
	dnsRunner := &recordingDnsmasqRunner{}
	nftRunner := &recordingNftRunner{}
	allocator := &recordingAllocator{}
	r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

	lc := newLinkedCluster(t, c, "hc-ready", "capi-cluster")

	for i := 0; i < 2; i++ {
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
			t.Fatalf("Reconcile %d error: %v", i+1, err)
		}
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if !hc.Status.Ready {
		t.Error("status.ready = false, want true")
	}
	if cond := findCondition(hc, clusterv1.InfrastructureReadyCondition); cond == nil || cond.Status != corev1.ConditionTrue {
		t.Errorf("InfrastructureReady condition = %v, want True", cond)
	}

	// The second reconcile re-ensures the stack: the bridge already exists,
	// so the create is not repeated, and dnsmasq and nft are applied again
	// without error.
	wantLinkCalls(t, ops,
		linkOpCall{op: opByName, name: testBridge},
		linkOpCall{op: opAdd, kind: "bridge", name: testBridge},
		linkOpCall{op: opByName, name: testBridge},
	)
	wantNft := [][]string{{"-f", "-"}, {"-f", "-"}}
	if !reflect.DeepEqual(nftRunner.argLog(), wantNft) {
		t.Errorf("nft invocations = %v, want %v", nftRunner.argLog(), wantNft)
	}
	wantStart := []string{"start:dnsmasq", "start:dnsmasq"}
	if !reflect.DeepEqual(dnsRunner.calls, wantStart) {
		t.Errorf("dnsmasq invocations = %v, want %v", dnsRunner.calls, wantStart)
	}
}

// TestReconcileControlPlaneEndpoint pins the endpoint contract: once the
// linked control plane reports initialized and a control-plane machine holds
// a static IP, the control-plane endpoint is published with that IP on port
// 6443. An uninitialized control plane and an absent control plane both
// leave the endpoint empty without error.
func TestReconcileControlPlaneEndpoint(t *testing.T) {
	c := mustReconcileClient(t)

	tests := []struct {
		name        string
		namespace   string
		withCP      bool
		initialized bool
		withMachine bool
		wantHost    string
	}{
		{
			name:        "initialized control plane",
			namespace:   "hc-endpoint-init",
			withCP:      true,
			initialized: true,
			withMachine: true,
			wantHost:    testCPIP,
		},
		{
			name:        "uninitialized control plane",
			namespace:   "hc-endpoint-notinit",
			withCP:      true,
			initialized: false,
			withMachine: true,
			wantHost:    "",
		},
		{
			name:      "no control plane",
			namespace: "hc-endpoint-nocp",
			wantHost:  "",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newRecordingLinkOps()
			dnsRunner := &recordingDnsmasqRunner{}
			nftRunner := &recordingNftRunner{}
			allocator := &recordingAllocator{}
			r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

			lc := newLinkedCluster(t, c, tt.namespace, "capi-cluster")
			if tt.withCP {
				newControlPlane(t, c, lc, tt.initialized)
			}
			if tt.withMachine {
				newControlPlaneMachine(t, c, lc, testCPIP)
			}

			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
				t.Fatalf("Reconcile error: %v", err)
			}

			hc := &infrastructurev1alpha1.HypervisorCluster{}
			if err := c.Get(t.Context(), lc.key(), hc); err != nil {
				t.Fatalf("Get HypervisorCluster: %v", err)
			}
			if hc.Status.ControlPlaneEndpoint.Host != tt.wantHost {
				t.Errorf("controlPlaneEndpoint.host = %q, want %q", hc.Status.ControlPlaneEndpoint.Host, tt.wantHost)
			}
			if tt.wantHost != "" && hc.Status.ControlPlaneEndpoint.Port != testCPPort {
				t.Errorf("controlPlaneEndpoint.port = %d, want %d", hc.Status.ControlPlaneEndpoint.Port, testCPPort)
			}

			// The network stack is up regardless of the control plane state.
			if !hc.Status.Ready {
				t.Error("status.ready = false, want true")
			}
		})
	}
}

// TestReconcileDeleteTearsDownStack pins the delete reconcile contract:
// deleting the object with the finalizer set stops dnsmasq, deletes the NAT
// table, removes the bridge, and drops the finalizer so the object is
// reclaimed. A later reconcile of the missing object is a no-op.
func TestReconcileDeleteTearsDownStack(t *testing.T) {
	c := mustReconcileClient(t)
	ops := newRecordingLinkOps()
	dnsRunner := &recordingDnsmasqRunner{}
	nftRunner := &recordingNftRunner{}
	allocator := &recordingAllocator{}
	r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

	lc := newLinkedCluster(t, c, "hc-teardown", "capi-cluster")

	// Provision so the finalizer is set.
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("provision reconcile error: %v", err)
	}

	// Delete the object: the finalizer keeps it around with a deletion
	// timestamp until the controller tears the stack down.
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if err := c.Delete(t.Context(), hc); err != nil {
		t.Fatalf("Delete HypervisorCluster: %v", err)
	}
	pending := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), pending); err != nil {
		t.Fatalf("object vanished before teardown reconcile: %v", err)
	}
	if pending.DeletionTimestamp.IsZero() {
		t.Fatal("deletion timestamp not set after Delete")
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("teardown reconcile error: %v", err)
	}

	// The finalizer is dropped and the object is reclaimed.
	if err := c.Get(t.Context(), lc.key(), &infrastructurev1alpha1.HypervisorCluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Get after teardown = %v, want NotFound", err)
	}

	// The full call log: provisioning on the first pass, then teardown
	// (dnsmasq stop, NAT delete, bridge delete) on the second.
	wantLinkCalls(t, ops,
		linkOpCall{op: opByName, name: testBridge},
		linkOpCall{op: opAdd, kind: "bridge", name: testBridge},
		linkOpCall{op: opDel, name: testBridge},
	)
	wantNft := [][]string{{"-f", "-"}, {"delete", "table", "inet", testNATTable}}
	if !reflect.DeepEqual(nftRunner.argLog(), wantNft) {
		t.Errorf("nft invocations = %v, want %v", nftRunner.argLog(), wantNft)
	}
	wantCalls := []string{"start:dnsmasq", "stop"}
	if !reflect.DeepEqual(dnsRunner.calls, wantCalls) {
		t.Errorf("dnsmasq invocations = %v, want %v", dnsRunner.calls, wantCalls)
	}

	// Teardown is idempotent: reconciling the now-missing object adds no
	// further calls.
	before := len(ops.calls) + len(nftRunner.calls) + len(dnsRunner.calls)
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("reconcile after deletion error: %v", err)
	}
	after := len(ops.calls) + len(nftRunner.calls) + len(dnsRunner.calls)
	if after != before {
		t.Errorf("teardown not idempotent: %d calls before, %d after missing-object reconcile", before, after)
	}
}

// TestReconcilePausedClusterIsUntouched pins the paused gate: a
// HypervisorCluster carrying the standard paused annotation triggers no
// reconcile actions — no stack operations, no allocator construction, no
// finalizer, no status change — and no error.
func TestReconcilePausedClusterIsUntouched(t *testing.T) {
	c := mustReconcileClient(t)
	ops := newRecordingLinkOps()
	dnsRunner := &recordingDnsmasqRunner{}
	nftRunner := &recordingNftRunner{}
	allocator := &recordingAllocator{}
	r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

	lc := newLinkedCluster(t, c, "hc-paused", "capi-cluster")
	lc.hc.Annotations = map[string]string{clusterv1.PausedAnnotation: ""}
	if err := c.Update(t.Context(), lc.hc); err != nil {
		t.Fatalf("annotate HypervisorCluster as paused: %v", err)
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want empty", res)
	}

	if len(ops.calls) != 0 || len(dnsRunner.calls) != 0 || len(nftRunner.calls) != 0 {
		t.Errorf("paused reconcile touched the stack: link %v, dnsmasq %v, nft %v",
			ops.calls, dnsRunner.calls, nftRunner.calls)
	}
	if allocator.calls != 0 {
		t.Errorf("paused reconcile constructed the allocator %d times, want 0", allocator.calls)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if len(hc.Finalizers) != 0 {
		t.Errorf("paused reconcile added finalizers %v, want none", hc.Finalizers)
	}
	if hc.Status.Ready {
		t.Error("paused reconcile marked the object ready")
	}
}

// TestReconcileDependencyFailureSurfaces pins the failure contract for the
// three stack steps: a failed step surfaces as a reconcile error that
// preserves the underlying error and aborts the sequence at that step, and
// the object is left not ready.
func TestReconcileDependencyFailureSurfaces(t *testing.T) {
	c := mustReconcileClient(t)

	errBridge := errors.New("fake: bridge create denied")
	errDnsmasq := errors.New("fake: dnsmasq start denied")
	errNft := errors.New("fake: nft apply denied")

	tests := []struct {
		name      string
		namespace string
		failBy    map[string]error
		wantErr   error
		wantLinks []linkOpCall
		wantDns   []string
		wantNft   [][]string
	}{
		{
			name:      "bridge ensure fails",
			namespace: "hc-fail-bridge",
			failBy:    map[string]error{opAdd: errBridge},
			wantErr:   errBridge,
			wantLinks: []linkOpCall{
				{op: opByName, name: testBridge},
				{op: opAdd, kind: "bridge", name: testBridge},
			},
		},
		{
			name:      "dnsmasq start fails",
			namespace: "hc-fail-dnsmasq",
			wantErr:   errDnsmasq,
			wantLinks: []linkOpCall{
				{op: opByName, name: testBridge},
				{op: opAdd, kind: "bridge", name: testBridge},
			},
			wantDns: []string{"start:dnsmasq"},
		},
		{
			name:      "nft apply fails",
			namespace: "hc-fail-nft",
			wantErr:   errNft,
			wantLinks: []linkOpCall{
				{op: opByName, name: testBridge},
				{op: opAdd, kind: "bridge", name: testBridge},
			},
			wantDns: []string{"start:dnsmasq"},
			wantNft: [][]string{{"-f", "-"}},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ops := newRecordingLinkOps()
			for k, v := range tt.failBy {
				ops.failBy[k] = v
			}
			dnsRunner := &recordingDnsmasqRunner{}
			if tt.wantErr == errDnsmasq {
				dnsRunner.startErr = errDnsmasq
			}
			nftRunner := &recordingNftRunner{}
			if tt.wantErr == errNft {
				nftRunner.err = errNft
			}
			allocator := &recordingAllocator{}
			r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

			lc := newLinkedCluster(t, c, tt.namespace, "capi-cluster")

			_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()})
			if err == nil {
				t.Fatal("Reconcile succeeded, want an error")
			}
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Reconcile error %v does not wrap %v", err, tt.wantErr)
			}

			// The failure aborts the provisioning sequence at the failing
			// step; the steps before it ran, the steps after it did not.
			wantLinkCalls(t, ops, tt.wantLinks...)
			if !reflect.DeepEqual(dnsRunner.calls, tt.wantDns) {
				t.Errorf("dnsmasq invocations = %v, want %v", dnsRunner.calls, tt.wantDns)
			}
			if !reflect.DeepEqual(nftRunner.argLog(), tt.wantNft) {
				t.Errorf("nft invocations = %v, want %v", nftRunner.argLog(), tt.wantNft)
			}

			// The object is not reported ready.
			hc := &infrastructurev1alpha1.HypervisorCluster{}
			if err := c.Get(t.Context(), lc.key(), hc); err != nil {
				t.Fatalf("Get HypervisorCluster: %v", err)
			}
			if hc.Status.Ready {
				t.Error("status.ready = true after failed reconcile, want false")
			}
		})
	}
}

// TestReconcileAllocatorFailureSurfaces pins the allocator failure contract:
// a failing allocator constructor surfaces as a reconcile error preserving
// the underlying error, and the object is left not ready.
func TestReconcileAllocatorFailureSurfaces(t *testing.T) {
	c := mustReconcileClient(t)
	errAllocator := errors.New("fake: allocator denied")
	allocator := &recordingAllocator{err: errAllocator}
	r := newTestReconciler(t, c, newRecordingLinkOps(), &recordingDnsmasqRunner{}, &recordingNftRunner{}, allocator)

	lc := newLinkedCluster(t, c, "hc-fail-alloc", "capi-cluster")

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()})
	if err == nil {
		t.Fatal("Reconcile succeeded, want an error")
	}
	if !errors.Is(err, errAllocator) {
		t.Errorf("Reconcile error %v does not wrap %v", err, errAllocator)
	}
	if allocator.calls != 1 {
		t.Errorf("allocator constructed %d times, want 1", allocator.calls)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if hc.Status.Ready {
		t.Error("status.ready = true after failed reconcile, want false")
	}
}

// TestReconcileMissingClusterIsUntouched pins the linkage contract: an
// object with no linked CAPI Cluster is left alone — not provisioned, not
// marked ready, no finalizer — and reconcile returns no error.
func TestReconcileMissingClusterIsUntouched(t *testing.T) {
	c := mustReconcileClient(t)
	ops := newRecordingLinkOps()
	dnsRunner := &recordingDnsmasqRunner{}
	nftRunner := &recordingNftRunner{}
	allocator := &recordingAllocator{}
	r := newTestReconciler(t, c, ops, dnsRunner, nftRunner, allocator)

	const namespace = "hc-missing"
	key := client.ObjectKey{Namespace: namespace, Name: "capi-cluster"}
	if err := c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace %q: %v", namespace, err)
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Spec: infrastructurev1alpha1.HypervisorClusterSpec{
			ClusterName: key.Name,
			Network: infrastructurev1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       testCIDR,
				Gateway:    testGateway,
				DNSIP:      testDNSIP,
				BridgeName: testBridge,
				NATTable:   testNATTable,
			},
		},
	}
	if err := c.Create(t.Context(), hc); err != nil {
		t.Fatalf("create HypervisorCluster: %v", err)
	}

	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want empty", res)
	}

	got := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), key, got); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if got.Status.Ready {
		t.Error("status.ready = true without a linked Cluster, want false")
	}
	if len(got.Finalizers) != 0 {
		t.Errorf("finalizers = %v without a linked Cluster, want none", got.Finalizers)
	}
}

// TestReconcileMissingObjectIsNoop pins the NotFound branch: reconciling an
// object key that does not exist returns no error and no requeue.
func TestReconcileMissingObjectIsNoop(t *testing.T) {
	c := mustReconcileClient(t)
	r := newTestReconciler(t, c, newRecordingLinkOps(), &recordingDnsmasqRunner{}, &recordingNftRunner{}, &recordingAllocator{})

	key := client.ObjectKey{Namespace: "hc-missing-obj", Name: "does-not-exist"}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want empty", res)
	}
}
