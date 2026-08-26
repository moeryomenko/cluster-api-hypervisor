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

// HypervisorCluster controller contract (post-k8netd integration).
//
// The host network stack (bridge, dnsmasq, nftables, static-IP IPAM) is owned
// by the k8netd daemon; its provisioning and teardown contract is pinned by
// hypervisorcluster_k8netd_test.go. This suite keeps the contracts that are
// independent of who owns the network:
//
//   - Reconcile resolves the object, then the linked CAPI Cluster (owner
//     reference or clusterName link), then applies the paused gate: an
//     object carrying the standard paused annotation is left untouched with
//     no reconcile actions. A missing object and a missing linked Cluster
//     are both no-ops, not errors.
//   - Control-plane endpoint publication (TASK-011/012): once the linked
//     control plane reports initialized and a control-plane machine holds an
//     internal IP, the endpoint is published as 127.0.0.1:6443 — loopback,
//     reachable via the control-plane VM's per-VM passt forwarding — never
//     the machine's internal IP. An absent or uninitialized control plane
//     leaves the endpoint empty without error.
//
// The reconciler is exercised through the committed envtest harness with a
// fake k8netd server standing in for the daemon, so no real host state is
// ever touched.
package controllers

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
	"github.com/moeryomenko/cluster-api-hypervisor/test/helpers"
)

// Cluster network fixture: the default lab network values the fixtures write
// into the HypervisorCluster spec.
const (
	testBridge    = "k8sbr0"
	testCIDR      = "192.168.124.0/24"
	testGateway   = "192.168.124.1"
	testDNSIP     = "192.168.124.1"
	testNATTable  = "k8slab"
	testPoolStart = "192.168.124.20"
	testCPPort    = 6443
	testCPIP      = "192.168.124.20"
)

var _ func(context.Context, ctrl.Request) (ctrl.Result, error) = (*HypervisorClusterReconciler)(nil).Reconcile

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
// the pinned cluster-api version, resolved through `go list -m`. Both CRDs
// are installed as shipped: v1beta2 is the storage version and the version
// the provider's clusterv1 scheme speaks since the v1beta2 contract
// migration, so no conversion round-trip is involved.
func installCAPICoreCRDs(t *testing.T, cfg *rest.Config) {
	t.Helper()

	dir, err := capiCRDDirectory()
	if err != nil {
		t.Fatalf("resolve cluster-api module directory: %v", err)
	}
	clusterCRD := loadCRD(t, filepath.Join(dir, "cluster.x-k8s.io_clusters.yaml"))
	machineCRD := loadCRD(t, filepath.Join(dir, "cluster.x-k8s.io_machines.yaml"))

	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{
		CRDs: []*apiextensionsv1.CustomResourceDefinition{clusterCRD, machineCRD},
	}); err != nil {
		t.Fatalf("install cluster-api core CRDs: %v", err)
	}
}

// loadCRD decodes one CRD manifest from the cluster-api module cache into its
// typed CustomResourceDefinition.
func loadCRD(t *testing.T, path string) *apiextensionsv1.CustomResourceDefinition {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read CRD manifest %q: %v", path, err)
	}
	crd := &apiextensionsv1.CustomResourceDefinition{}
	if err := yaml.Unmarshal(data, crd); err != nil {
		t.Fatalf("decode CRD manifest %q: %v", path, err)
	}

	return crd
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
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrastructurev1alpha1.GroupVersion.Group,
				Kind:     "HypervisorCluster",
				Name:     name,
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
				Spec: controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: infrastructurev1alpha1.GroupVersion.Group,
						Kind:     "HypervisorMachineTemplate",
						Name:     lc.name + "-machine-template",
					},
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

	lc.cluster.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: controlplanev1alpha1.GroupVersion.Group,
		Kind:     "HypervisorControlPlane",
		Name:     cp.Name,
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
			// spec.bootstrap is required by the v1beta2 Machine API; the
			// dangling reference stands in for "no bootstrap data", which the
			// endpoint contract does not read.
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: clusterv1.ContractVersionedObjectReference{
					APIGroup: bootstrapv1alpha1.GroupVersion.Group,
					Kind:     "HypervisorConfig",
					Name:     hm.Name + "-config",
				},
			},
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrastructurev1alpha1.GroupVersion.Group,
				Kind:     "HypervisorMachine",
				Name:     hm.Name,
			},
		},
	}
	if err := c.Create(ctx, machine); err != nil {
		t.Fatalf("create control-plane Machine: %v", err)
	}
}

// newTestReconciler builds the reconciler under test over a fake k8netd
// server. After the k8netd rewiring (TASK-006) the host-stack seams
// (Net/Nft/Dnsmasq/NewAllocator) are gone; the reconciler uses a k8netd
// client, so the fixture wires a fake k8netd server instead of host fakes.
func newTestReconciler(t *testing.T, c client.Client) *HypervisorClusterReconciler {
	t.Helper()

	sock := filepath.Join(t.TempDir(), "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New %q: %v", sock, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	kc := k8netd.NewClient(sock)
	return &HypervisorClusterReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(16),
		K8Netd:   kc,
	}
}

// findCondition returns the condition with the given type, or nil.
func findCondition(hc *infrastructurev1alpha1.HypervisorCluster, t string) *metav1.Condition {
	for i := range hc.Status.Conditions {
		if hc.Status.Conditions[i].Type == t {
			return &hc.Status.Conditions[i]
		}
	}

	return nil
}

// TestReconcilePausedClusterIsUntouched pins the paused gate: a
// HypervisorCluster carrying the standard paused annotation triggers no
// reconcile actions — no finalizer, no status change — and no error.
func TestReconcilePausedClusterIsUntouched(t *testing.T) {
	c := mustReconcileClient(t)
	r := newTestReconciler(t, c)

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

// TestReconcileMissingClusterIsUntouched pins the linkage contract: an
// object with no linked CAPI Cluster is left alone — not provisioned, not
// marked ready, no finalizer — and reconcile returns no error.
func TestReconcileMissingClusterIsUntouched(t *testing.T) {
	c := mustReconcileClient(t)
	r := newTestReconciler(t, c)

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
	r := newTestReconciler(t, c)

	key := client.ObjectKey{Namespace: "hc-missing-obj", Name: "does-not-exist"}
	res, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want empty", res)
	}
}

// TASK-011 VC-06 REQ-006 — endpoint + PKI: reconcileControlPlaneEndpoint
// publishes 127.0.0.1:6443, not the VM's internal IP.
//
// Grill-me: reserved IP is dynamic (not hardcoded .20); endpoint must be
// loopback even when the machine's InternalIP differs; port must be 6443;
// uninitialized control plane still leaves endpoint empty; second reconcile
// converges.
// RED: current impl publishes the machine's internal IP (e.g. 192.168.124.77),
// so the host assertion fails.
func TestReconcileControlPlaneEndpointPublishesLoopback(t *testing.T) {
	c := mustReconcileClient(t)
	// Use non-default reserved IP to prove not hardcoded .20.
	const reservedIP = "192.168.124.77"
	const wantHost = "127.0.0.1"
	const wantPort = 6443

	r := newTestReconciler(t, c)

	lc := newLinkedCluster(t, c, "hc-endpoint-loopback", "capi-cluster")
	newControlPlane(t, c, lc, true)
	newControlPlaneMachine(t, c, lc, reservedIP)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if hc.Status.ControlPlaneEndpoint.Host != wantHost {
		t.Errorf(
			"controlPlaneEndpoint.host = %q, want %q (REQ-006 VC-06: must be loopback, not %s)",
			hc.Status.ControlPlaneEndpoint.Host,
			wantHost,
			reservedIP,
		)
	}
	if hc.Status.ControlPlaneEndpoint.Port != wantPort {
		t.Errorf("controlPlaneEndpoint.port = %d, want %d", hc.Status.ControlPlaneEndpoint.Port, wantPort)
	}
	// Prove not still publishing the reserved IP.
	if hc.Status.ControlPlaneEndpoint.Host == reservedIP {
		t.Errorf("controlPlaneEndpoint still publishes reserved IP %q, want loopback", reservedIP)
	}
	// Prove not hardcoded to old default testCPIP.
	if hc.Status.ControlPlaneEndpoint.Host == testCPIP && wantHost != testCPIP {
		t.Logf("endpoint is old default %q, want loopback", testCPIP)
	}
	// Second reconcile converges: still loopback.
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("second Reconcile error: %v", err)
	}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster after second reconcile: %v", err)
	}
	if hc.Status.ControlPlaneEndpoint.Host != wantHost || hc.Status.ControlPlaneEndpoint.Port != wantPort {
		t.Errorf(
			"after second reconcile endpoint = %s:%d, want %s:%d",
			hc.Status.ControlPlaneEndpoint.Host,
			hc.Status.ControlPlaneEndpoint.Port,
			wantHost,
			wantPort,
		)
	}
}

// TestReconcileControlPlaneEndpointLoopbackWithDynamicIPs proves the
// loopback endpoint does not vary with the reserved IP AllocateIP returns.
func TestReconcileControlPlaneEndpointLoopbackWithDynamicIPs(t *testing.T) {
	for _, reservedIP := range []string{"192.168.124.50", "192.168.124.90", "192.168.124.200"} {
		t.Run(reservedIP, func(t *testing.T) {
			c := mustReconcileClient(t)
			ns := "hc-endpoint-loopback-" + reservedIP[len(reservedIP)-2:]

			r := newTestReconciler(t, c)
			lc := newLinkedCluster(t, c, ns, "capi-cluster")
			newControlPlane(t, c, lc, true)
			newControlPlaneMachine(t, c, lc, reservedIP)
			if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
				t.Fatalf("Reconcile error: %v", err)
			}
			hc := &infrastructurev1alpha1.HypervisorCluster{}
			if err := c.Get(t.Context(), lc.key(), hc); err != nil {
				t.Fatalf("Get HypervisorCluster: %v", err)
			}
			if hc.Status.ControlPlaneEndpoint.Host != "127.0.0.1" {
				t.Errorf("reserved %s: endpoint host = %q, want 127.0.0.1", reservedIP, hc.Status.ControlPlaneEndpoint.Host)
			}
			if hc.Status.ControlPlaneEndpoint.Port != 6443 {
				t.Errorf("reserved %s: endpoint port = %d, want 6443", reservedIP, hc.Status.ControlPlaneEndpoint.Port)
			}
		})
	}
}

// TestReconcileControlPlaneEndpointLoopbackWhenNoMachine ensures that even
// with the loopback contract, an absent or uninitialized control plane still
// leaves the endpoint empty (no regression of the paused/not-ready gate).
func TestReconcileControlPlaneEndpointLoopbackGate(t *testing.T) {
	c := mustReconcileClient(t)
	t.Run("uninitialized control plane leaves endpoint empty", func(t *testing.T) {

		r := newTestReconciler(t, c)
		lc := newLinkedCluster(t, c, "hc-endpoint-gate-notinit", "capi-cluster")
		newControlPlane(t, c, lc, false)
		newControlPlaneMachine(t, c, lc, "192.168.124.77")
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
		hc := &infrastructurev1alpha1.HypervisorCluster{}
		if err := c.Get(t.Context(), lc.key(), hc); err != nil {
			t.Fatalf("Get HypervisorCluster: %v", err)
		}
		if hc.Status.ControlPlaneEndpoint.Host != "" {
			t.Errorf("uninitialized: endpoint host = %q, want empty", hc.Status.ControlPlaneEndpoint.Host)
		}
	})
	t.Run("no machine leaves endpoint empty", func(t *testing.T) {

		r := newTestReconciler(t, c)
		lc := newLinkedCluster(t, c, "hc-endpoint-gate-nomachine", "capi-cluster")
		newControlPlane(t, c, lc, true)
		if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
		hc := &infrastructurev1alpha1.HypervisorCluster{}
		if err := c.Get(t.Context(), lc.key(), hc); err != nil {
			t.Fatalf("Get HypervisorCluster: %v", err)
		}
		if hc.Status.ControlPlaneEndpoint.Host != "" {
			t.Errorf("no machine: endpoint host = %q, want empty", hc.Status.ControlPlaneEndpoint.Host)
		}
	})
}

// TestReconcileFindsClusterByInfrastructureRefWithoutOwnerRefOrClusterName
// pins the reverse-lookup fallback of getLinkedCluster: a HypervisorCluster
// created by the topology controller carries neither a Cluster owner
// reference nor spec.clusterName (observed live on k8labs-vxtxl), yet a
// Cluster exists in the same namespace whose spec.infrastructureRef names it.
// Reconcile must resolve that Cluster through the back-reference and
// provision: status.ready true, InfrastructureReady condition true, and —
// per the CAPI v1beta2 InfrastructureCluster contract —
// status.initialization.provisioned true. Today getLinkedCluster returns nil
// for this shape, so reconcile no-ops forever with "linked Cluster not found"
// and the network is never provisioned.
//
// Red phase: every provisioning assertion fails today because the linked
// Cluster is not found.
func TestReconcileFindsClusterByInfrastructureRefWithoutOwnerRefOrClusterName(t *testing.T) {
	c := mustReconcileClient(t)
	r := newTestReconciler(t, c)

	const namespace = "hc-reverse-lookup"
	const name = "k8labs-vxtxl"
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
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrastructurev1alpha1.GroupVersion.Group,
				Kind:     "HypervisorCluster",
				Name:     name,
			},
		},
	}
	if err := c.Create(ctx, cluster); err != nil {
		t.Fatalf("create Cluster %q: %v", name, err)
	}

	// Topology-controller shape: no OwnerReferences and an empty
	// spec.clusterName; only the Cluster's infrastructureRef links the two.
	hc := &infrastructurev1alpha1.HypervisorCluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: infrastructurev1alpha1.HypervisorClusterSpec{
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
		t.Fatalf("create HypervisorCluster: %v", err)
	}

	key := client.ObjectKey{Namespace: namespace, Name: name}
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	got := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if !got.Status.Ready {
		t.Error("status.ready = false: Reconcile did not find the linked Cluster through the infrastructureRef back-reference, want provisioned")
	}
	if cond := findCondition(got, clusterv1.InfrastructureReadyCondition); cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("InfrastructureReady condition = %+v, want present and True", cond)
	}
	if provisioned, present := statusInitializationProvisioned(got.Status); !present || !provisioned {
		t.Errorf("status.initialization.provisioned = (%t, present=%t), want true (CAPI v1beta2 cluster controller gate)", provisioned, present)
	}

	// Second reconcile converges: provisioning stays idempotent through the
	// reverse lookup.
	if _, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile error: %v", err)
	}
	got2 := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(ctx, key, got2); err != nil {
		t.Fatalf("Get HypervisorCluster after second reconcile: %v", err)
	}
	if !got2.Status.Ready {
		t.Error("status.ready flipped to false on the second reconcile")
	}
}
