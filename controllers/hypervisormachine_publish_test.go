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

// HypervisorMachine published-endpoint contract (test-first, RED).
//
// REQ-008 / TASK-013: after the machine's port is attached, the machine
// controller publishes the control-plane endpoints through the k8netd
// PublishPort RPC — exactly two calls for a control-plane machine,
// (port=<machine-name>, vm_port=6443) and (port=<machine-name>, vm_port=22) —
// and records the returned host ports on status.publishedPorts. Worker
// machines publish nothing. Re-reconciles are idempotent: no duplicate or
// changed allocations.
//
// Grill cases covered:
//   - exactly two PublishPort calls with exact param sets after AttachPort
//   - returned host ports recorded in status.publishedPorts
//   - re-reconcile does not duplicate or change allocations
//   - worker machines issue zero PublishPort calls and record nothing
//
// This file is RED: infrastructurev1alpha1.MachinePublishedPort does not
// exist yet ("undefined: v1alpha1.MachinePublishedPort"), so the controllers
// package does not compile until the API type lands; after it lands, every
// assertion fails until the controller publishes ports.

package controllers

import (
	"encoding/json"
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
)

// newLinkedControlPlaneMachine creates the full CAPI linkage for one
// control-plane machine: identical to newLinkedMachine except the owner CAPI
// Machine carries the control-plane role label and the bootstrap config is
// pinned to the control-plane role — the two signals the controller may use
// to tell control-plane machines from workers.
func newLinkedControlPlaneMachine(
	t *testing.T,
	c client.Client,
	lc *linkedCluster,
	name string,
	withBootstrap bool,
) *linkedMachine {
	t.Helper()

	lm := newLinkedMachine(t, c, lc, name, withBootstrap)

	if withBootstrap {
		lm.config.Spec.Role = testConfigRoleControlPlane
		if err := c.Update(t.Context(), lm.config); err != nil {
			t.Fatalf("set bootstrap config role %q: %v", testConfigRoleControlPlane, err)
		}
	}

	fresh := &clusterv1.Machine{}
	key := client.ObjectKey{Namespace: lm.machine.Namespace, Name: lm.machine.Name}
	if err := c.Get(t.Context(), key, fresh); err != nil {
		t.Fatalf("get CAPI Machine %q: %v", lm.machine.Name, err)
	}
	if fresh.Labels == nil {
		fresh.Labels = map[string]string{}
	}
	fresh.Labels[clusterv1.ClusterNameLabel] = lc.name
	fresh.Labels[clusterv1.MachineControlPlaneLabel] = ""
	if err := c.Update(t.Context(), fresh); err != nil {
		t.Fatalf("set control-plane role label on Machine %q: %v", fresh.Name, err)
	}

	return lm
}

// publishPortParams is the decoded wire params of a PublishPort request.
type publishPortParams struct {
	Port   string `json:"port"`
	VMPort int32  `json:"vm_port"`
}

// installPublishRecorder arms a recording PublishPort handler that maps
// vm_port to a fixed host port (6443 -> 26443, 22 -> 20022) and returns a
// snapshot function yielding the captured calls in order.
func installPublishRecorder(t *testing.T, srv *fake.Server) func() []publishPortParams {
	t.Helper()
	var calls []publishPortParams
	srv.Handle("PublishPort", func(params json.RawMessage) (any, *fake.RPCError) {
		var p publishPortParams
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, &fake.RPCError{Code: "invalid_params", Message: err.Error()}
		}
		calls = append(calls, p)
		host := int32(20022)
		if p.VMPort == 6443 {
			host = 26443
		}
		return map[string]int32{"host_port": host}, nil
	})
	return func() []publishPortParams { return calls }
}

// TestHypervisorMachineK8netd_ControlPlanePublishesTwoPortsAfterAttach pins
// the REQ-008 publication contract for control-plane machines: after the
// port is attached, exactly two PublishPort calls are issued —
// (port=<machine-name>, vm_port=6443) and (port=<machine-name>, vm_port=22),
// each params object carrying exactly those two keys — and the returned host
// ports land on status.publishedPorts.
func TestHypervisorMachineK8netd_ControlPlanePublishesTwoPortsAfterAttach(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newMachineK8netdFakeServer(t)
	r, srv, vm := newMachineK8netdReconciler(t, c, srv)
	vm.State = ch.VMState("Running")
	publishCalls := installPublishRecorder(t, srv)

	lc := newLinkedCluster(t, c, "machine-publish-cp", "capi-cluster")
	lm := newLinkedControlPlaneMachine(t, c, lc, "cp-node-1", true)
	key := client.ObjectKeyFromObject(lm.hm)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if got := len(publishCalls()); got != 2 {
		t.Fatalf("recorder saw %d PublishPort invocations, want 2; requests=%v", got, srv.Requests())
	}

	reqs := srv.Requests()
	if got := countMachineK8netdMethod(reqs, "PublishPort"); got != 2 {
		t.Fatalf("PublishPort calls = %d, want exactly 2 (requests=%v)", got, reqs)
	}

	// Publication happens after the port is attached.
	attachIdx := methodIndex(reqs, "AttachPort")
	if attachIdx == -1 {
		t.Fatalf("AttachPort not called; requests=%v", reqs)
	}
	for i, req := range reqs {
		if req.Method != "PublishPort" {
			continue
		}
		if i < attachIdx {
			t.Errorf("PublishPort@%d fired before AttachPort@%d; want publication after attach", i, attachIdx)
		}
	}

	// Exact param sets: one 6443 call and one 22 call, both naming the
	// machine's port, each carrying exactly the two canonical keys.
	wantByVMPort := map[int32]bool{6443: false, 22: false}
	for _, req := range reqs {
		if req.Method != "PublishPort" {
			continue
		}
		var keys map[string]json.RawMessage
		if err := json.Unmarshal(req.Params, &keys); err != nil {
			t.Fatalf("unmarshal PublishPort params %s: %v", string(req.Params), err)
		}
		if len(keys) != 2 {
			t.Errorf("PublishPort params carry %d keys (%s), want exactly port and vm_port", len(keys), string(req.Params))
		}
		var p publishPortParams
		if err := json.Unmarshal(req.Params, &p); err != nil {
			t.Fatalf("unmarshal typed PublishPort params %s: %v", string(req.Params), err)
		}
		if p.Port != lm.name {
			t.Errorf("PublishPort port = %q, want machine name %q", p.Port, lm.name)
		}
		if _, known := wantByVMPort[p.VMPort]; !known {
			t.Errorf("PublishPort vm_port = %d, want only 6443 and 22; requests=%v", p.VMPort, reqs)
			continue
		}
		wantByVMPort[p.VMPort] = true
	}
	for vmPort, seen := range wantByVMPort {
		if !seen {
			t.Errorf("no PublishPort call for vm_port %d; requests=%v", vmPort, reqs)
		}
	}

	// The returned host ports are recorded on the status.
	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), key, hm); err != nil {
		t.Fatalf("Get HypervisorMachine: %v", err)
	}
	got := map[int32]int32{}
	for _, pp := range hm.Status.PublishedPorts {
		got[pp.VMPort] = pp.HostPort
	}
	want := map[int32]int32{6443: 26443, 22: 20022}
	if len(got) != len(want) {
		t.Fatalf("status.publishedPorts = %+v, want entries for exactly %v", hm.Status.PublishedPorts, want)
	}
	for vmPort, wantHost := range want {
		if gotHost, ok := got[vmPort]; !ok || gotHost != wantHost {
			t.Errorf("status.publishedPorts[%d] = %d (present %v), want %d", vmPort, gotHost, ok, wantHost)
		}
	}
}

// TestHypervisorMachineK8netd_ControlPlanePublishIdempotentAcrossReconciles
// pins the idempotency clause of REQ-008: a re-reconcile neither duplicates
// the PublishPort calls nor changes the recorded allocations.
func TestHypervisorMachineK8netd_ControlPlanePublishIdempotentAcrossReconciles(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newMachineK8netdFakeServer(t)
	r, srv, vm := newMachineK8netdReconciler(t, c, srv)
	vm.State = ch.VMState("Running")
	publishCalls := installPublishRecorder(t, srv)

	lc := newLinkedCluster(t, c, "machine-publish-idem", "capi-cluster")
	lm := newLinkedControlPlaneMachine(t, c, lc, "cp-node-1", true)
	key := client.ObjectKeyFromObject(lm.hm)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first Reconcile error: %v", err)
	}
	firstStatus := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), key, firstStatus); err != nil {
		t.Fatalf("Get after first reconcile: %v", err)
	}

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second Reconcile error: %v", err)
	}

	if got := countMachineK8netdMethod(srv.Requests(), "PublishPort"); got != 2 {
		t.Errorf("PublishPort calls across two reconciles = %d, want still 2 (idempotent)", got)
	}
	if got := len(publishCalls()); got != 2 {
		t.Errorf("recorder saw %d PublishPort invocations across two reconciles, want 2", got)
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), key, hm); err != nil {
		t.Fatalf("Get after second reconcile: %v", err)
	}
	if len(hm.Status.PublishedPorts) != len(firstStatus.Status.PublishedPorts) {
		t.Errorf(
			"status.publishedPorts changed across reconciles: %d -> %d entries",
			len(firstStatus.Status.PublishedPorts), len(hm.Status.PublishedPorts),
		)
	}
	for _, before := range firstStatus.Status.PublishedPorts {
		found := false
		for _, after := range hm.Status.PublishedPorts {
			if after.VMPort == before.VMPort && after.HostPort == before.HostPort {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("allocation for vm_port %d changed or vanished across reconciles: %+v -> %+v",
				before.VMPort, firstStatus.Status.PublishedPorts, hm.Status.PublishedPorts)
		}
	}
}

// TestHypervisorMachineK8netd_WorkerMachinePublishesNothing pins the worker
// exclusion of REQ-008: a worker machine's reconcile issues zero PublishPort
// calls and records no published ports on its status.
func TestHypervisorMachineK8netd_WorkerMachinePublishesNothing(t *testing.T) {
	c := mustReconcileClient(t)
	srv := newMachineK8netdFakeServer(t)
	r, srv, vm := newMachineK8netdReconciler(t, c, srv)
	vm.State = ch.VMState("Running")

	lc := newLinkedCluster(t, c, "machine-publish-worker", "capi-cluster")
	// newLinkedMachine wires the worker role: no control-plane label, no
	// control-plane bootstrap role.
	lm := newLinkedMachine(t, c, lc, "node-1", true)
	key := client.ObjectKeyFromObject(lm.hm)

	if _, err := r.Reconcile(t.Context(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}

	if got := countMachineK8netdMethod(srv.Requests(), "PublishPort"); got != 0 {
		t.Errorf("worker machine issued %d PublishPort calls, want 0; requests=%v", got, srv.Requests())
	}
	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), key, hm); err != nil {
		t.Fatalf("Get HypervisorMachine: %v", err)
	}
	if len(hm.Status.PublishedPorts) != 0 {
		t.Errorf("worker status.publishedPorts = %+v, want empty", hm.Status.PublishedPorts)
	}
}
