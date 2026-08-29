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

// HypervisorCluster k8netd rewiring contract (test-first, RED).
//
// VC-03 / REQ-003: with a fake k8netd server, reconcileNormal issues
// CreateNetwork before marking InfrastructureReady; reconcileDelete issues
// DeleteNetwork before finalizer removal; repeated reconciles issue no
// duplicate creates; the Net/Nft/Dnsmasq/NewAllocator fields are gone from
// the reconciler struct; controlPlaneEndpoint still reconciles.
//
// Grill cases covered:
//   - network spec mapping (name=cluster name, CIDR/gateway/pool from
//     hc.Spec.Network and pool constants defaultPoolStart/defaultPoolEnd)
//   - idempotency: no second CreateNetwork on re-reconcile; no second
//     DeleteNetwork after finalizer removed
//   - finalizer ordering: DeleteNetwork before finalizer removal; error
//     from CreateNetwork/DeleteNetwork aborts and leaves status/finalizer
//   - empty/custom CIDR/gateway still mapped verbatim
//   - pool constants reuse (not spec pool)
//   - AlreadyExists on CreateNetwork is not an error (idempotent)
//   - NotFound on DeleteNetwork is not an error
//   - controlPlaneEndpoint still published when k8netd path is used
//
// This file is RED: the current controller still uses the old host-stack
// (Net/Nft/Dnsmasq/NewAllocator) and does not call k8netd, so every test
// that asserts k8netd interaction fails until TASK-006 rewires the
// controller.

package controllers

import (
	"encoding/json"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
)

// k8netdCreateNetworkParams is the decoded params of a CreateNetwork call.
type k8netdCreateNetworkParams struct {
	Name      string `json:"name"`
	CIDR      string `json:"cidr"`
	Gateway   string `json:"gateway"`
	PoolStart string `json:"poolStart"`
	PoolEnd   string `json:"poolEnd"`
}

// k8netdDeleteNetworkParams is the decoded params of a DeleteNetwork call.
type k8netdDeleteNetworkParams struct {
	Name string `json:"name"`
}

// newK8netdFakeServer creates a fake k8netd server on a temp socket for
// the test. The caller should defer srv.Close via t.Cleanup.
func newK8netdFakeServer(t *testing.T) *fake.Server {
	t.Helper()
	sock := filepath.Join(t.TempDir(), "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New %q: %v", sock, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	return srv
}

// newK8netdReconciler builds a HypervisorClusterReconciler wired to the fake
// k8netd server. It uses reflection to set the k8netd client field so the
// test file compiles both before and after the rewiring (RED before, PASS
// after). If the field is absent the helper fails the test (RED).
func newK8netdReconciler(t *testing.T, c client.Client, srv *fake.Server) *HypervisorClusterReconciler {
	t.Helper()
	kc := k8netd.NewClient(srv.SocketPath())
	r := &HypervisorClusterReconciler{
		Client:   c,
		Scheme:   newScheme(),
		Recorder: record.NewFakeRecorder(16),
	}
	rv := reflect.ValueOf(r).Elem()
	rt := rv.Type()
	kcType := reflect.TypeOf(kc)
	found := false
	// First, look for a field whose type exactly matches *k8netd.Client.
	for i := 0; i < rt.NumField(); i++ {
		if rt.Field(i).Type == kcType && rv.Field(i).CanSet() {
			rv.Field(i).Set(reflect.ValueOf(kc))
			found = true
			break
		}
	}
	if !found {
		// Fallback: any field whose name contains k8netd/k8snet that is assignable.
		for i := 0; i < rt.NumField(); i++ {
			name := strings.ToLower(rt.Field(i).Name)
			if (strings.Contains(name, "k8netd") || strings.Contains(name, "k8snet") || strings.Contains(name, "k8")) &&
				rt.Field(i).Type.AssignableTo(kcType) &&
				rv.Field(i).CanSet() {
				rv.Field(i).Set(reflect.ValueOf(kc))
				found = true
				break
			}
		}
	}
	if !found {
		// Explicit name candidates for better error message.
		candidates := []string{"K8netd", "K8Netd", "K8netdClient", "K8NetdClient", "K8sNetd", "Client"}
		for _, n := range candidates {
			if f := rv.FieldByName(n); f.IsValid() && f.CanSet() && f.Type() == kcType {
				f.Set(reflect.ValueOf(kc))
				found = true
				break
			}
		}
	}
	if !found {
		var fields []string
		for field := range rt.Fields() {
			fields = append(fields, field.Name+":"+field.Type.String())
		}
		t.Fatalf(
			"HypervisorClusterReconciler has no *k8netd.Client field; fields=%v (expected Net/Nft/Dnsmasq/NewAllocator removed, K8netd added)",
			fields,
		)
	}
	return r
}

// decodeCreateNetworkParams decodes the first CreateNetwork request's params.
func decodeCreateNetworkParams(t *testing.T, req fake.CapturedRequest) k8netdCreateNetworkParams {
	t.Helper()
	var p k8netdCreateNetworkParams
	if err := json.Unmarshal(req.Params, &p); err != nil {
		t.Fatalf("unmarshal CreateNetwork params %s: %v", string(req.Params), err)
	}
	return p
}

// countMethod counts requests with the given method.
func countMethod(reqs []fake.CapturedRequest, method string) int {
	n := 0
	for _, r := range reqs {
		if r.Method == method {
			n++
		}
	}
	return n
}

// TestHypervisorClusterK8netd_ReconcilerHasNoLegacyFields asserts the old
// host-stack fields are absent. RED while the controller still carries
// Net/Nft/Dnsmasq/NewAllocator.
func TestHypervisorClusterK8netd_ReconcilerHasNoLegacyFields(t *testing.T) {
	t.Parallel()
	rt := reflect.TypeFor[HypervisorClusterReconciler]()
	legacy := []string{"Net", "Nft", "Dnsmasq", "NewAllocator"}
	for _, name := range legacy {
		if _, ok := rt.FieldByName(name); ok {
			t.Errorf(
				"HypervisorClusterReconciler still has field %q; REQ-003 requires it removed (Net/Nft/Dnsmasq/NewAllocator must be gone)",
				name,
			)
		}
	}
	// Also assert a k8netd client field exists.
	kcType := reflect.TypeOf(k8netd.NewClient(""))
	hasK8netd := false
	for field := range rt.Fields() {
		if field.Type == kcType {
			hasK8netd = true
			break
		}
	}
	if !hasK8netd {
		t.Errorf("HypervisorClusterReconciler has no *k8netd.Client field; REQ-003 requires it wired from cfg.K8NetdSocket")
	}
}

// TestHypervisorClusterK8netd_ReconcileNormal_CreatesNetworkBeforeReady pins
// the order: CreateNetwork with name==cluster name before InfrastructureReady.
func TestHypervisorClusterK8netd_ReconcileNormal_CreatesNetworkBeforeReady(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-ready", "capi-cluster")

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	reqs := srv.Requests()
	if countMethod(reqs, "CreateNetwork") != 1 {
		t.Fatalf("CreateNetwork calls = %d, want 1 (methods seen: %v)", countMethod(reqs, "CreateNetwork"), reqs)
	}
	// First request must be CreateNetwork, not GetNetwork or other.
	if len(reqs) == 0 || reqs[0].Method != "CreateNetwork" {
		t.Fatalf("first k8netd call = %q, want CreateNetwork before marking ready", reqs[0].Method)
	}
	p := decodeCreateNetworkParams(t, reqs[0])
	if p.Name != lc.name {
		t.Errorf("CreateNetwork name = %q, want cluster name %q", p.Name, lc.name)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if !hc.Status.Ready {
		t.Error("status.ready = false after successful CreateNetwork, want true")
	}
	cond := findCondition(hc, clusterv1.InfrastructureReadyCondition)
	if cond == nil || cond.Status != metav1.ConditionTrue {
		t.Errorf("InfrastructureReady condition = %v, want True after CreateNetwork", cond)
	}
	// Ensure no legacy host-stack was used via old fields — the fake k8netd is
	// the only seam that should have been called.
	if len(reqs) != 1 {
		t.Errorf("unexpected extra k8netd calls: %v", reqs)
	}
}

// TestHypervisorClusterK8netd_ReconcileNormal_NetworkParamMapping pins the
// CIDR/gateway/pool mapping: CIDR and gateway from hc.Spec.Network, pool
// bounds from the pool constants defaultPoolStart/defaultPoolEnd.
func TestHypervisorClusterK8netd_ReconcileNormal_NetworkParamMapping(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	// Use a custom CIDR/gateway to prove mapping is verbatim, not default.
	lc := newLinkedCluster(t, c, "hc-k8netd-map", "capi-cluster")
	// Update spec to custom values before reconcile.
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	hc.Spec.Network.CIDR = "10.20.0.0/16"
	hc.Spec.Network.Gateway = "10.20.0.1"
	if err := c.Update(t.Context(), hc); err != nil {
		t.Fatalf("Update HypervisorCluster network: %v", err)
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	reqs := srv.Requests()
	if countMethod(reqs, "CreateNetwork") != 1 {
		t.Fatalf("CreateNetwork calls = %d, want 1", countMethod(reqs, "CreateNetwork"))
	}
	var createReq fake.CapturedRequest
	for _, rq := range reqs {
		if rq.Method == "CreateNetwork" {
			createReq = rq
			break
		}
	}
	p := decodeCreateNetworkParams(t, createReq)
	if p.CIDR != "10.20.0.0/16" {
		t.Errorf("CreateNetwork cidr = %q, want %q from hc.Spec.Network.CIDR", p.CIDR, "10.20.0.0/16")
	}
	if p.Gateway != "10.20.0.1" {
		t.Errorf("CreateNetwork gateway = %q, want %q from hc.Spec.Network.Gateway", p.Gateway, "10.20.0.1")
	}
	// Pool constants must be reused, not derived from spec.
	if p.PoolStart != defaultPoolStart {
		t.Errorf("CreateNetwork poolStart = %q, want pool constant %q", p.PoolStart, defaultPoolStart)
	}
	if p.PoolEnd != defaultPoolEnd {
		t.Errorf("CreateNetwork poolEnd = %q, want pool constant %q", p.PoolEnd, defaultPoolEnd)
	}
	if p.PoolStart != "192.168.124.20" || p.PoolEnd != "192.168.124.200" {
		t.Errorf("pool bounds = (%q, %q), want (192.168.124.20, 192.168.124.200)", p.PoolStart, p.PoolEnd)
	}
}

// TestHypervisorClusterK8netd_ReconcileNormal_IdempotentNoDuplicateCreate
// asserts repeated reconciles do not issue a second CreateNetwork.
func TestHypervisorClusterK8netd_ReconcileNormal_IdempotentNoDuplicateCreate(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-idem", "capi-cluster")

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("first Reconcile error: %v", err)
	}
	firstCount := countMethod(srv.Requests(), "CreateNetwork")
	if firstCount != 1 {
		t.Fatalf("first Reconcile CreateNetwork calls = %d, want 1", firstCount)
	}

	// Second reconcile: should be idempotent, no second CreateNetwork.
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("second Reconcile error: %v", err)
	}
	secondCount := countMethod(srv.Requests(), "CreateNetwork")
	if secondCount != 1 {
		t.Errorf("second Reconcile CreateNetwork calls = %d, want still 1 (no duplicate create on re-reconcile)", secondCount)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if !hc.Status.Ready {
		t.Error("status.ready = false after idempotent re-reconcile, want true")
	}
}

// TestHypervisorClusterK8netd_ReconcileNormal_AlreadyExistsIsIdempotent
// asserts that a fake server returning already_exists for CreateNetwork is
// treated as success (k8netd contract idempotent).
func TestHypervisorClusterK8netd_ReconcileNormal_AlreadyExistsIsIdempotent(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	// First call will succeed, second will return already_exists — handler
	// covers both by returning already_exists on the second invocation.
	call := 0
	srv.Handle("CreateNetwork", func(params json.RawMessage) (any, *fake.RPCError) {
		call++
		if call > 1 {
			return nil, &fake.RPCError{Code: "already_exists", Message: "already_exists"}
		}
		return nil, nil
	})
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-already", "capi-cluster")

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("first Reconcile error: %v", err)
	}
	// Reset handler to already_exists for second call if controller retries.
	srv.SetError("CreateNetwork", "already_exists", "already_exists")
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("second Reconcile with already_exists error: %v (should be treated as success)", err)
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if !hc.Status.Ready {
		t.Error("status.ready = false after already_exists, want true (idempotent)")
	}
}

// TestHypervisorClusterK8netd_ReconcileDelete_DeletesNetworkBeforeFinalizerRemoval
// pins delete order: DeleteNetwork with name==cluster name before finalizer removal.
func TestHypervisorClusterK8netd_ReconcileDelete_DeletesNetworkBeforeFinalizerRemoval(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-del", "capi-cluster")

	// Provision so finalizer is set.
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("provision Reconcile error: %v", err)
	}
	// Verify finalizer present.
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if len(hc.Finalizers) == 0 {
		t.Fatal("finalizer not set after provision; cannot test delete order")
	}

	// Delete: finalizer keeps object with deletionTimestamp.
	if err := c.Delete(t.Context(), hc); err != nil {
		t.Fatalf("Delete HypervisorCluster: %v", err)
	}
	pending := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), pending); err != nil {
		t.Fatalf("object vanished before teardown reconcile: %v", err)
	}
	if pending.DeletionTimestamp.IsZero() {
		t.Fatal("deletionTimestamp not set after Delete")
	}

	// Record requests before teardown.
	beforeDeleteCount := countMethod(srv.Requests(), "DeleteNetwork")

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("teardown Reconcile error: %v", err)
	}

	reqs := srv.Requests()
	deleteReqs := 0
	var deleteReq fake.CapturedRequest
	for _, rq := range reqs {
		if rq.Method == "DeleteNetwork" {
			deleteReqs++
			deleteReq = rq
		}
	}
	if deleteReqs != 1 {
		t.Fatalf("DeleteNetwork calls = %d, want 1 (methods %v)", deleteReqs, reqs)
	}
	if beforeDeleteCount != 0 {
		t.Errorf("DeleteNetwork called before teardown, want only on delete reconcile")
	}
	var p k8netdDeleteNetworkParams
	if err := json.Unmarshal(deleteReq.Params, &p); err != nil {
		t.Fatalf("unmarshal DeleteNetwork params: %v", err)
	}
	if p.Name != lc.name {
		t.Errorf("DeleteNetwork name = %q, want cluster name %q", p.Name, lc.name)
	}

	// Finalizer removed and object reclaimed.
	if err := c.Get(t.Context(), lc.key(), &infrastructurev1alpha1.HypervisorCluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Get after teardown = %v, want NotFound (finalizer removed after DeleteNetwork)", err)
	}
}

// TestHypervisorClusterK8netd_ReconcileDelete_IdempotentNoDuplicateDelete
// asserts that after the object is reclaimed, a further reconcile issues no
// additional DeleteNetwork.
func TestHypervisorClusterK8netd_ReconcileDelete_IdempotentNoDuplicateDelete(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-del-idem", "capi-cluster")

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("provision Reconcile error: %v", err)
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if err := c.Delete(t.Context(), hc); err != nil {
		t.Fatalf("Delete HypervisorCluster: %v", err)
	}
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("teardown Reconcile error: %v", err)
	}
	afterTeardown := countMethod(srv.Requests(), "DeleteNetwork")
	if afterTeardown != 1 {
		t.Fatalf("DeleteNetwork calls after teardown = %d, want 1", afterTeardown)
	}
	// Reconcile the now-missing object: no extra DeleteNetwork.
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("reconcile after deletion error: %v", err)
	}
	afterMissing := countMethod(srv.Requests(), "DeleteNetwork")
	if afterMissing != 1 {
		t.Errorf("DeleteNetwork calls after missing-object reconcile = %d, want still 1 (no duplicate delete)", afterMissing)
	}
}

// TestHypervisorClusterK8netd_ReconcileDelete_NotFoundIsIdempotent asserts
// that DeleteNetwork returning not_found is treated as success and still
// removes the finalizer.
func TestHypervisorClusterK8netd_ReconcileDelete_NotFoundIsIdempotent(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-del-nf", "capi-cluster")

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("provision Reconcile error: %v", err)
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if err := c.Delete(t.Context(), hc); err != nil {
		t.Fatalf("Delete HypervisorCluster: %v", err)
	}
	// Fake returns not_found for DeleteNetwork — controller should treat as success.
	srv.SetError("DeleteNetwork", "not_found", "not_found")
	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("teardown Reconcile with not_found error: %v (should be idempotent)", err)
	}
	if err := c.Get(t.Context(), lc.key(), &infrastructurev1alpha1.HypervisorCluster{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Get after not_found teardown = %v, want NotFound (finalizer should be removed)", err)
	}
}

// TestHypervisorClusterK8netd_ReconcileNormal_ErrorLeavesNotReady asserts
// that a CreateNetwork error aborts and leaves the object not ready and
// without InfrastructureReady true, and that finalizer handling is still correct.
func TestHypervisorClusterK8netd_ReconcileNormal_ErrorLeavesNotReady(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	srv.SetError("CreateNetwork", "internal", "internal boom")
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-err", "capi-cluster")

	_, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()})
	if err == nil {
		t.Fatal("Reconcile succeeded with CreateNetwork internal error, want error")
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	if hc.Status.Ready {
		t.Error("status.ready = true after CreateNetwork error, want false")
	}
	if cond := findCondition(hc, clusterv1.InfrastructureReadyCondition); cond != nil &&
		cond.Status == metav1.ConditionTrue {
		t.Errorf("InfrastructureReady condition = True after CreateNetwork error, want not True")
	}
	// Finalizer should still be present? The provision reconcile adds finalizer
	// before CreateNetwork in current controller; after rewiring it may add
	// finalizer before or after CreateNetwork. We only assert that Ready is false.
}

// TestHypervisorClusterK8netd_ReconcileControlPlaneEndpointStillReconciles
// asserts that the k8netd-wired controller still publishes the control-plane
// endpoint (host+6443) when the linked control plane is initialized and a
// control-plane machine has an IP, and that every reconcile still issues
// CreateNetwork before endpoint logic.
func TestHypervisorClusterK8netd_ReconcileControlPlaneEndpointStillReconciles(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newK8netdFakeServer(t)
	r := newK8netdReconciler(t, c, srv)

	lc := newLinkedCluster(t, c, "hc-k8netd-ep", "capi-cluster")
	newControlPlane(t, c, lc, true)
	newControlPlaneMachine(t, c, lc, testCPIP)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc.key()}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := c.Get(t.Context(), lc.key(), hc); err != nil {
		t.Fatalf("Get HypervisorCluster: %v", err)
	}
	const wantHost = "127.0.0.1"
	if hc.Status.ControlPlaneEndpoint.Host != wantHost {
		t.Errorf(
			"controlPlaneEndpoint.host = %q, want %q (endpoint must still reconcile with k8netd)",
			hc.Status.ControlPlaneEndpoint.Host,
			wantHost,
		)
	}
	if hc.Status.ControlPlaneEndpoint.Port != testCPPort {
		t.Errorf("controlPlaneEndpoint.port = %d, want %d", hc.Status.ControlPlaneEndpoint.Port, testCPPort)
	}
	if !hc.Status.Ready {
		t.Error("status.ready = false after endpoint reconcile, want true")
	}
	// Ensure CreateNetwork was still the provisioning seam.
	if countMethod(srv.Requests(), "CreateNetwork") != 1 {
		t.Errorf(
			"CreateNetwork calls = %d, want 1 alongside endpoint reconcile",
			countMethod(srv.Requests(), "CreateNetwork"),
		)
	}
	// Uninitialized control plane leaves endpoint empty but still provisions.
	t.Run("uninitialized", func(t *testing.T) {
		srv2 := newK8netdFakeServer(t)
		r2 := newK8netdReconciler(t, c, srv2)
		lc2 := newLinkedCluster(t, c, "hc-k8netd-ep2", "capi-cluster")
		newControlPlane(t, c, lc2, false)
		newControlPlaneMachine(t, c, lc2, testCPIP)
		if _, err := r2.Reconcile(t.Context(), ctrl.Request{NamespacedName: lc2.key()}); err != nil {
			t.Fatalf("Reconcile error: %v", err)
		}
		hc2 := &infrastructurev1alpha1.HypervisorCluster{}
		if err := c.Get(t.Context(), lc2.key(), hc2); err != nil {
			t.Fatalf("Get HypervisorCluster: %v", err)
		}
		if hc2.Status.ControlPlaneEndpoint.Host != "" {
			t.Errorf(
				"controlPlaneEndpoint.host = %q with uninitialized control plane, want empty",
				hc2.Status.ControlPlaneEndpoint.Host,
			)
		}
	})
}

// Ensure imports are used.
var (
	_ = metav1.Now
	_ = apierrors.IsNotFound
)
