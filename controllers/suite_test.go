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

// envtest suite (spec section 4, VC-01: all five CRDs install against the
// management apiserver with correct group/kind/version; REQ-002).
//
// This suite is test-first (task 14) and depends on the envtest harness
// contract that task 15 implements in test/helpers/envtest.go. Until that
// file exists this package does not compile; the intended red phase failure
// is "undefined: helpers.StartEnvTest" / "no non-test Go files in
// ...test/helpers".
//
// The harness owns the control-plane lifecycle: it starts envtest with the
// k8s 1.35.x binaries resolved by setup-envtest (KUBEBUILDER_ASSETS), loads
// and installs the five CRDs from config/crd/bases, registers the scheme
// (clientgoscheme plus the three provider api groups), and stops the control
// plane when the test completes. This suite only exercises the contract:
// creating, reading back, and deleting one object of each kind through the
// envtest client is the load/install assertion.
package controllers

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	controllerutil "sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	ch "github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	chclient "github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	confext "github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	config "github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	k8netd "github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	fake "github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
	mac "github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
	"github.com/moeryomenko/cluster-api-hypervisor/test/helpers"
)

// TestEnvtestCRDsAndScheme starts the envtest control plane through the
// harness and proves the load/install contract for every kind shipped by the
// provider set: the CRD is installed (create succeeds), the registered scheme
// round-trips the object (get returns the submitted spec), and the object can
// be deleted (delete then get reports NotFound).
func TestEnvtestCRDsAndScheme(t *testing.T) {
	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}
	if envTest.Env == nil {
		t.Fatalf("helpers.StartEnvTest returned a nil Env")
	}
	if envTest.Client == nil {
		t.Fatalf("helpers.StartEnvTest returned a nil Client")
	}

	const namespace = "envtest-contract"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := envTest.Client.Create(ctx, ns); err != nil {
		t.Fatalf("create test namespace %q: %v", namespace, err)
	}
	t.Cleanup(func() {
		// Best-effort; the harness stops the control plane in its own cleanup.
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = envTest.Client.Delete(cctx, ns)
	})

	cases := []struct {
		name   string
		obj    client.Object
		verify func(client.Object) error
	}{
		{
			name: "HypervisorCluster",
			obj: &infrastructurev1alpha1.HypervisorCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-cluster", Namespace: namespace},
				Spec: infrastructurev1alpha1.HypervisorClusterSpec{
					ClusterName: "sample-cluster",
					Network: infrastructurev1alpha1.HypervisorClusterNetworkSpec{
						CIDR:       "192.168.124.0/24",
						Gateway:    "192.168.124.1",
						DNSIP:      "192.168.124.1",
						BridgeName: "k8sbr0",
						NATTable:   "k8slab",
					},
				},
			},
			verify: func(o client.Object) error {
				got := o.(*infrastructurev1alpha1.HypervisorCluster)
				if got.Spec.Network.CIDR != "192.168.124.0/24" {
					return fmt.Errorf("spec.network.cidr = %q, want %q", got.Spec.Network.CIDR, "192.168.124.0/24")
				}
				return nil
			},
		},
		{
			name: "HypervisorMachine",
			obj: &infrastructurev1alpha1.HypervisorMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-machine", Namespace: namespace},
				Spec: infrastructurev1alpha1.HypervisorMachineSpec{
					ClusterName: "sample-cluster",
					CPU:         2,
					RAM:         4096,
					Disk:        20480,
				},
			},
			verify: func(o client.Object) error {
				got := o.(*infrastructurev1alpha1.HypervisorMachine)
				if got.Spec.CPU != 2 {
					return fmt.Errorf("spec.cpu = %d, want %d", got.Spec.CPU, 2)
				}
				return nil
			},
		},
		{
			name: "HypervisorMachineTemplate",
			obj: &infrastructurev1alpha1.HypervisorMachineTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-machinetemplate", Namespace: namespace},
				Spec: infrastructurev1alpha1.HypervisorMachineTemplateSpec{
					Template: infrastructurev1alpha1.HypervisorMachineTemplateResource{
						Spec: infrastructurev1alpha1.HypervisorMachineSpec{
							ClusterName: "sample-cluster",
							CPU:         4,
							RAM:         8192,
							Disk:        30720,
						},
					},
				},
			},
			verify: func(o client.Object) error {
				got := o.(*infrastructurev1alpha1.HypervisorMachineTemplate)
				if got.Spec.Template.Spec.CPU != 4 {
					return fmt.Errorf("spec.template.spec.cpu = %d, want %d", got.Spec.Template.Spec.CPU, 4)
				}
				return nil
			},
		},
		{
			name: "HypervisorConfig",
			obj: &bootstrapv1alpha1.HypervisorConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-config", Namespace: namespace},
				Spec: bootstrapv1alpha1.HypervisorConfigSpec{
					ClusterName: "sample-cluster",
					Role:        "worker",
					NodeName:    "sample-worker",
				},
			},
			verify: func(o client.Object) error {
				got := o.(*bootstrapv1alpha1.HypervisorConfig)
				if got.Spec.Role != "worker" {
					return fmt.Errorf("spec.role = %q, want %q", got.Spec.Role, "worker")
				}
				return nil
			},
		},
		{
			name: "HypervisorControlPlane",
			obj: &controlplanev1alpha1.HypervisorControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-controlplane", Namespace: namespace},
				Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
					Replicas: 1,
					Version:  "v1.35.4",
					MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
						Spec: controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{
							InfrastructureRef: clusterv1.ContractVersionedObjectReference{
								APIGroup: "infrastructure.cluster.x-k8s.io",
								Kind:     "HypervisorMachineTemplate",
								Name:     "sample-machinetemplate",
							},
						},
					},
				},
			},
			verify: func(o client.Object) error {
				got := o.(*controlplanev1alpha1.HypervisorControlPlane)
				if got.Spec.Replicas != 1 {
					return fmt.Errorf("spec.replicas = %d, want %d", got.Spec.Replicas, 1)
				}
				return nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertCRUD(t, envTest.Client, tc.obj, tc.verify)
		})
	}
}

// assertCRUD proves one kind satisfies the load/install contract: create
// succeeds (CRD installed and reachable), get returns the submitted object
// through the registered scheme, delete succeeds, and a subsequent get
// reports NotFound.
func assertCRUD(t *testing.T, c client.Client, obj client.Object, verify func(client.Object) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	key := client.ObjectKeyFromObject(obj)

	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("Create %s: %v", key, err)
	}

	got := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	if err := verify(got); err != nil {
		t.Fatalf("Get %s round-trip: %v", key, err)
	}

	if err := c.Delete(ctx, obj); err != nil {
		t.Fatalf("Delete %s: %v", key, err)
	}

	gone := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, key, gone); !apierrors.IsNotFound(err) {
		t.Fatalf("Get %s after Delete: want NotFound, got %v", key, err)
	}
}

// ---------------------------------------------------------------------------
// VC-07 (spec K8NETD-INT-001): envtest suite wiring — the controllers boot
// inside a manager against the envtest control plane with a fake k8netd
// JSON-RPC server, and the full CRUD reconcile flows pass end to end.
//
// Hermeticity contract (REQ-009): no root, no host network, no real k8netd
// socket. The fake server listens on a t.TempDir() Unix socket; the VM is a
// chclient.FakeClient; qemu-img/mksquashfs run through recording exec seams;
// cloud-init rendering goes through the recording renderer. Nothing here may
// touch netlink, nftables, dnsmasq, or a real k8netd daemon.
//
// Fake injection point: startEnvtestK8netdSuite constructs both reconcilers
// with k8netd.NewClient(fakeServer.SocketPath()) — the same seam main.go
// wires from cfg.K8NetdSocket — and registers per-method handlers on the
// fake that record every contract call into one ordered op log shared with
// the VM wrapper, so cross-seam ordering (port lifecycle before VM start,
// VM stop before port teardown) is assertable.
// ---------------------------------------------------------------------------

// opLog records ordered operation names across the k8netd and VM seams.
type opLog struct {
	mu  sync.Mutex
	ops []string
}

func (l *opLog) add(op string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.ops = append(l.ops, op)
}

func (l *opLog) snapshot() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make([]string, len(l.ops))
	copy(out, l.ops)
	return out
}

// firstIndex returns the index of the first occurrence of op at or after
// from, or -1 when absent.
func (l *opLog) firstIndex(op string, from int) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	for i := from; i < len(l.ops); i++ {
		if l.ops[i] == op {
			return i
		}
	}
	return -1
}

// opLoggingVM wraps the fake VM client and appends every operation to the
// shared ordered log, so VM calls interleave with k8netd calls in one trace.
type opLoggingVM struct {
	inner *chclient.FakeClient
	log   *opLog
}

func (v *opLoggingVM) SetNetConfig(netConfig string) {
	v.inner.SetNetConfig(netConfig)
}

func (v *opLoggingVM) SetFirmware(firmware string) {
	v.inner.SetFirmware(firmware)
}

func (v *opLoggingVM) SetDiskPaths(paths []string) {
	v.inner.SetDiskPaths(paths)
}

func (v *opLoggingVM) SetCPU(cpu int32) {
	v.inner.SetCPU(cpu)
}

func (v *opLoggingVM) SetRAM(ramMiB int32) {
	v.inner.SetRAM(ramMiB)
}

func (v *opLoggingVM) EnsureRunning(ctx context.Context) error {
	v.log.add("VM.EnsureRunning")
	return v.inner.EnsureRunning(ctx)
}

func (v *opLoggingVM) Shutdown(ctx context.Context) error {
	v.log.add("VM.Shutdown")
	return v.inner.Shutdown(ctx)
}

func (v *opLoggingVM) Stop(ctx context.Context) error {
	v.log.add("VM.Stop")
	return v.inner.Stop(ctx)
}

func (v *opLoggingVM) Info(ctx context.Context) (ch.VMState, error) {
	v.log.add("VM.Info")
	return v.inner.Info(ctx)
}

// registerK8netdOpHandlers wires every k8netd contract method on the fake
// server to record its call in ops and succeed; AllocateIP returns the pool
// start address so identity provisioning completes.
func registerK8netdOpHandlers(srv *fake.Server, ops *opLog) {
	for _, method := range []string{
		"CreateNetwork", "DeleteNetwork", "CreatePort", "DeletePort", "AttachPort", "DetachPort",
	} {
		srv.Handle(method, func(json.RawMessage) (any, *fake.RPCError) {
			ops.add(method)
			return nil, nil
		})
	}
	srv.Handle("AllocateIP", func(json.RawMessage) (any, *fake.RPCError) {
		ops.add("AllocateIP")
		return testPoolStart, nil
	})
}

// envtestK8netdSuite is the booted controller suite: the direct envtest
// client for object CRUD and assertions, the fake k8netd server, and the
// ordered operation log.
type envtestK8netdSuite struct {
	client    client.Client
	k8netdSrv *fake.Server
	vm        *chclient.FakeClient
	ops       *opLog
}

// startEnvtestK8netdSuite boots one envtest control plane (k8s 1.35.x
// binaries via the committed harness), installs the CAPI core CRDs the
// controllers read, starts a manager running the cluster and machine
// controllers wired to a fake k8netd server on a temp-dir socket, and stops
// everything on cleanup.
func startEnvtestK8netdSuite(t *testing.T) *envtestK8netdSuite {
	t.Helper()

	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}
	installCAPICoreCRDs(t, envTest.Env.Config)

	c, err := client.New(envTest.Env.Config, client.Options{Scheme: newScheme()})
	if err != nil {
		t.Fatalf("create suite client: %v", err)
	}

	srv := newK8netdFakeServer(t)
	ops := &opLog{}
	registerK8netdOpHandlers(srv, ops)

	vm := &chclient.FakeClient{State: ch.VMState("Running")}
	recVM := &opLoggingVM{inner: vm, log: ops}
	qemu := newRecordingExecRunner()
	pack := newRecordingExecRunner()
	derive := &recordingMACDerive{}
	render := &recordingCIDATARender{}

	clusterRec := &HypervisorClusterReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(1024),
		K8Netd:   k8netd.NewClient(srv.SocketPath()),
	}
	machineRec := &HypervisorMachineReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(1024),
		Config: config.Config{
			BaseImage: testBaseImage,
			VMDiskDir: t.TempDir(),
			SocketDir: t.TempDir(),
		},
		NewVMClient:     func(string, string) chclient.Client { return recVM },
		K8Netd:          k8netd.NewClient(srv.SocketPath()),
		QemuImg:         qemu.Run,
		Confext:         confext.NewPackager(confext.WithRunner(pack)),
		RenderCloudInit: render.render,
		DeriveMAC:       derive.derive,
	}

	mgr, err := ctrl.NewManager(envTest.Env.Config, ctrl.Options{
		Scheme:  newScheme(),
		Metrics: metricsserver.Options{BindAddress: "0"},
	})
	if err != nil {
		t.Fatalf("new manager: %v", err)
	}
	if err := clusterRec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup cluster controller: %v", err)
	}
	if err := machineRec.SetupWithManager(mgr); err != nil {
		t.Fatalf("setup machine controller: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	startErr := make(chan error, 1)
	go func() { startErr <- mgr.Start(ctx) }()
	select {
	case <-mgr.Elected():
	case err := <-startErr:
		cancel()
		t.Fatalf("manager stopped before election: %v", err)
	case <-time.After(60 * time.Second):
		cancel()
		t.Fatal("manager did not become elected within 60s")
	}
	t.Cleanup(func() {
		cancel()
		select {
		case err := <-startErr:
			if err != nil && !errors.Is(err, context.Canceled) {
				t.Errorf("manager start error: %v", err)
			}
		case <-time.After(10 * time.Second):
			t.Error("manager did not stop within 10s of cancel")
		}
	})

	return &envtestK8netdSuite{client: c, k8netdSrv: srv, vm: vm, ops: ops}
}

// eventuallyF polls cond until it holds or the timeout elapses, failing with
// desc when it never does.
func eventuallyF(t *testing.T, timeout time.Duration, desc string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("timed out after %s waiting for %s", timeout, desc)
}

// firstRequestOf returns the first captured request of method.
func firstRequestOf(t *testing.T, srv *fake.Server, method string) fake.CapturedRequest {
	t.Helper()
	for _, req := range srv.Requests() {
		if req.Method == method {
			return req
		}
	}
	t.Fatalf("no %q request captured; captured: %v", method, srv.Requests())
	return fake.CapturedRequest{}
}

// armMachineFinalizer adds the machine delete finalizer, retrying on
// conflict because the running controller updates the status concurrently.
func armMachineFinalizer(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	for attempt := 0; attempt < 20; attempt++ {
		hm := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), key, hm); err != nil {
			t.Fatalf("get HypervisorMachine to arm finalizer: %v", err)
		}
		if controllerutil.ContainsFinalizer(hm, machineDeleteFinalizer) {
			return
		}
		hm.Finalizers = append(hm.Finalizers, machineDeleteFinalizer)
		err := c.Update(t.Context(), hm)
		if err == nil {
			return
		}
		if apierrors.IsConflict(err) {
			time.Sleep(200 * time.Millisecond)
			continue
		}
		t.Fatalf("arm machine finalizer: %v", err)
	}
	t.Fatal("could not arm machine finalizer after 20 attempts (persistent conflict)")
}

// TestEnvtestSuiteControllersWithFakeK8Netd proves VC-07: the envtest suite
// boots both controllers against the fake k8netd server and the full CRUD
// reconcile flows pass — cluster provision/delete through CreateNetwork/
// DeleteNetwork, machine provision/delete through the port lifecycle, with
// ordering pinned across seams.
func TestEnvtestSuiteControllersWithFakeK8Netd(t *testing.T) {
	s := startEnvtestK8netdSuite(t)

	// Hermeticity guard: the fake must never sit on the real default control
	// socket path from REQ-002.
	if s.k8netdSrv.SocketPath() == "/run/user/1000/k8snet/control.sock" {
		t.Fatal("suite fake k8netd socket collides with the real default control socket path")
	}

	lc := newLinkedCluster(t, s.client, "envtest-suite-ns", "suite-cluster")

	t.Run("cluster reconcile issues CreateNetwork before InfrastructureReady", func(t *testing.T) {
		eventuallyF(t, 90*time.Second, "CreateNetwork captured and HypervisorCluster ready", func() bool {
			if countMethod(s.k8netdSrv.Requests(), "CreateNetwork") == 0 {
				return false
			}
			hc := &infrastructurev1alpha1.HypervisorCluster{}
			if err := s.client.Get(t.Context(), lc.key(), hc); err != nil {
				return false
			}
			cond := findCondition(hc, clusterv1.InfrastructureReadyCondition)
			return hc.Status.Ready && cond != nil && cond.Status == metav1.ConditionTrue
		})

		reqs := s.k8netdSrv.Requests()
		if reqs[0].Method != "CreateNetwork" {
			t.Errorf("first k8netd call = %q, want CreateNetwork before anything else", reqs[0].Method)
		}
		p := decodeCreateNetworkParams(t, firstRequestOf(t, s.k8netdSrv, "CreateNetwork"))
		if p.Name != lc.name {
			t.Errorf("CreateNetwork name = %q, want cluster name %q", p.Name, lc.name)
		}
		if p.CIDR != testCIDR {
			t.Errorf("CreateNetwork cidr = %q, want %q", p.CIDR, testCIDR)
		}
		if p.Gateway != testGateway {
			t.Errorf("CreateNetwork gateway = %q, want %q", p.Gateway, testGateway)
		}
		if p.PoolStart != defaultPoolStart || p.PoolEnd != defaultPoolEnd {
			t.Errorf("CreateNetwork pool = (%q, %q), want constants (%q, %q)",
				p.PoolStart, p.PoolEnd, defaultPoolStart, defaultPoolEnd)
		}
	})

	t.Run("re-reconcile does not duplicate CreateNetwork", func(t *testing.T) {
		before := countMethod(s.k8netdSrv.Requests(), "CreateNetwork")
		if before == 0 {
			t.Fatal("no CreateNetwork captured; cannot test idempotency")
		}

		// Poke an extra reconcile through a watched field update.
		hc := &infrastructurev1alpha1.HypervisorCluster{}
		if err := s.client.Get(t.Context(), lc.key(), hc); err != nil {
			t.Fatalf("get HypervisorCluster: %v", err)
		}
		if hc.Annotations == nil {
			hc.Annotations = map[string]string{}
		}
		hc.Annotations["suite.cluster.x-k8s.io/rerun"] = "1"
		if err := s.client.Update(t.Context(), hc); err != nil {
			t.Fatalf("poke re-reconcile: %v", err)
		}

		// Grace window: the count must stay put while the extra reconcile lands.
		deadline := time.Now().Add(3 * time.Second)
		for time.Now().Before(deadline) {
			if got := countMethod(s.k8netdSrv.Requests(), "CreateNetwork"); got != before {
				t.Fatalf("CreateNetwork calls after re-reconcile = %d, want still %d", got, before)
			}
			time.Sleep(100 * time.Millisecond)
		}
	})

	var lm *linkedMachine
	t.Run("machine reconcile issues port lifecycle before VM start and publishes IP", func(t *testing.T) {
		lm = newLinkedMachine(t, s.client, lc, "suite-machine", true)
		armMachineFinalizer(t, s.client, client.ObjectKeyFromObject(lm.hm))

		key := client.ObjectKeyFromObject(lm.hm)
		eventuallyF(t, 120*time.Second, "machine provisioned: port lifecycle + VM start + status IP", func() bool {
			ops := s.ops.snapshot()
			hasPort := false
			for _, op := range ops {
				if op == "AllocateIP" {
					hasPort = true
					break
				}
			}
			if !hasPort {
				return false
			}
			hm := &infrastructurev1alpha1.HypervisorMachine{}
			if err := s.client.Get(t.Context(), key, hm); err != nil {
				return false
			}
			ip := ""
			for _, addr := range hm.Status.Addresses {
				if addr.Type == clusterv1.MachineInternalIP {
					ip = addr.Address
				}
			}
			cond := findMachineCondition(hm, vmProvisionedCondition)
			return ip == testPoolStart && hm.Status.ProviderID != nil &&
				hm.Status.Ready && cond != nil && cond.Status == metav1.ConditionTrue
		})

		// Cross-seam order: CreatePort -> AttachPort -> AllocateIP -> VM start.
		ops := s.ops.snapshot()
		idx := func(op string) int {
			for i, o := range ops {
				if o == op {
					return i
				}
			}
			return -1
		}
		cp, ap, ai, vr := idx("CreatePort"), idx("AttachPort"), idx("AllocateIP"), idx("VM.EnsureRunning")
		if cp < 0 || ap < 0 || ai < 0 || vr < 0 {
			t.Fatalf("incomplete op trace: %v", ops)
		}
		if !(cp < ap && ap < ai && ai < vr) {
			t.Errorf("op order = [CreatePort:%d AttachPort:%d AllocateIP:%d EnsureRunning:%d], want strictly increasing",
				cp, ap, ai, vr)
		}

		// AttachPort params: port == machine name, network == cluster name,
		// mac == derived MAC.
		type attachParams struct {
			Port    string `json:"port"`
			Network string `json:"network"`
			MAC     string `json:"mac"`
		}
		var attach attachParams
		attachReq := firstRequestOf(t, s.k8netdSrv, "AttachPort")
		if err := json.Unmarshal(attachReq.Params, &attach); err != nil {
			t.Fatalf("unmarshal AttachPort params: %v", err)
		}
		wantAttachMAC := mac.Derive(lc.name, lm.name)
		if attach.Port != lm.name || attach.Network != lc.name || attach.MAC != wantAttachMAC {
			t.Errorf("AttachPort = (%q, %q, %q), want (%q, %q, %q)", attach.Port, attach.Network, attach.MAC, lm.name, lc.name, wantAttachMAC)
		}

		// AllocateIP params: network == cluster name, mac == derived MAC.
		type allocParams struct {
			Network string `json:"network"`
			MAC     string `json:"mac"`
		}
		var alloc allocParams
		allocReq := firstRequestOf(t, s.k8netdSrv, "AllocateIP")
		if err := json.Unmarshal(allocReq.Params, &alloc); err != nil {
			t.Fatalf("unmarshal AllocateIP params: %v", err)
		}
		wantMAC := mac.Derive(lc.name, lm.name)
		if alloc.Network != lc.name || alloc.MAC != wantMAC {
			t.Errorf("AllocateIP = (%q, %q), want (%q, %q)", alloc.Network, alloc.MAC, lc.name, wantMAC)
		}
	})

	t.Run("machine delete stops VM then detaches and deletes port", func(t *testing.T) {
		if lm == nil {
			t.Fatal("machine provision subtest did not run; cannot test deletion")
		}
		mark := len(s.ops.snapshot())

		if err := s.client.Delete(t.Context(), lm.hm); err != nil {
			t.Fatalf("delete HypervisorMachine: %v", err)
		}
		key := client.ObjectKeyFromObject(lm.hm)
		eventuallyF(t, 90*time.Second, "HypervisorMachine reclaimed after teardown", func() bool {
			err := s.client.Get(t.Context(), key, &infrastructurev1alpha1.HypervisorMachine{})
			return apierrors.IsNotFound(err)
		})

		// After the mark: VM shutdown, then stop, then DetachPort, DeletePort.
		sd := s.ops.firstIndex("VM.Shutdown", mark)
		st := s.ops.firstIndex("VM.Stop", mark)
		dp := s.ops.firstIndex("DetachPort", mark)
		dpo := s.ops.firstIndex("DeletePort", mark)
		if sd < 0 || st < 0 || dp < 0 || dpo < 0 {
			t.Fatalf("teardown ops missing after mark %d: trace %v", mark, s.ops.snapshot())
		}
		if !(st > sd && dp > st && dpo > dp) {
			t.Errorf("teardown order after mark %d = [Shutdown:%d Stop:%d DetachPort:%d DeletePort:%d], want Stop>Shutdown, DetachPort>Stop, DeletePort>DetachPort",
				mark, sd, st, dp, dpo)
		}
	})

	t.Run("cluster delete issues DeleteNetwork before finalizer removal", func(t *testing.T) {
		before := countMethod(s.k8netdSrv.Requests(), "DeleteNetwork")
		mark := len(s.ops.snapshot())

		hc := &infrastructurev1alpha1.HypervisorCluster{}
		if err := s.client.Get(t.Context(), lc.key(), hc); err != nil {
			t.Fatalf("Get HypervisorCluster: %v", err)
		}
		if len(hc.Finalizers) == 0 {
			t.Fatal("cluster finalizer not set; cannot exercise delete flow")
		}
		if err := s.client.Delete(t.Context(), hc); err != nil {
			t.Fatalf("Delete HypervisorCluster: %v", err)
		}
		eventuallyF(t, 90*time.Second, "HypervisorCluster reclaimed", func() bool {
			err := s.client.Get(t.Context(), lc.key(), &infrastructurev1alpha1.HypervisorCluster{})
			return apierrors.IsNotFound(err)
		})

		reqs := s.k8netdSrv.Requests()
		after := countMethod(reqs, "DeleteNetwork")
		if after != before+1 {
			t.Fatalf("DeleteNetwork calls = %d, want %d (exactly one teardown call)", after, before+1)
		}
		if s.ops.firstIndex("DeleteNetwork", mark) < 0 {
			t.Errorf("DeleteNetwork not recorded after delete mark %d", mark)
		}
	})
}

// findMachineCondition returns the condition of the given type on the
// machine status, or nil when absent.
func findMachineCondition(hm *infrastructurev1alpha1.HypervisorMachine, t string) *metav1.Condition {
	for i := range hm.Status.Conditions {
		if hm.Status.Conditions[i].Type == t {
			return &hm.Status.Conditions[i]
		}
	}
	return nil
}
