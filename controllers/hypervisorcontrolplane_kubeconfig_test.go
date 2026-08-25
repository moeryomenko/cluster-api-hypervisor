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

// HypervisorControlPlane kubeconfig-from-allocation contract (test-first,
// RED).
//
// REQ-009 / VC-07 / TASK-013: ensureKubeconfigSecret renders the server URL
// as https://127.0.0.1:<hostPort> using the 6443 allocation recorded by
// REQ-008 on the cluster's control-plane machines' HypervisorMachine status.
// Write-once semantics become reconcile-to-current: when the recorded
// allocation changes, the existing Secret's data.value is updated in place.
// Worker machines never participate in endpoint resolution.
//
// Grill cases covered:
//   - server URL carries the recorded 6443 host port, not a static 6443
//   - allocation change reconciles the existing Secret within one reconcile,
//     even though the control plane already reports ready
//   - no recorded allocation yet: requeue, no Secret, not ready
//   - a worker machine's published ports never win endpoint resolution
//
// This file is RED: infrastructurev1alpha1.MachinePublishedPort does not
// exist yet ("undefined: v1alpha1.MachinePublishedPort"), so the controllers
// package does not compile until the API type lands; after it lands, every
// assertion fails until ensureKubeconfigSecret reads the allocation and
// reconciles to current.

package controllers

import (
	"testing"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// setHMPublishedPorts records published endpoints on a HypervisorMachine's
// status, standing in for what the machine controller writes after the
// k8netd PublishPort calls.
func setHMPublishedPorts(
	t *testing.T,
	c client.Client,
	hm *infrastructurev1alpha1.HypervisorMachine,
	ports ...infrastructurev1alpha1.MachinePublishedPort,
) {
	t.Helper()
	key := client.ObjectKeyFromObject(hm)
	fresh := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), key, fresh); err != nil {
		t.Fatalf("Get HypervisorMachine %s: %v", key, err)
	}
	fresh.Status.PublishedPorts = ports
	if err := c.Status().Update(t.Context(), fresh); err != nil {
		t.Fatalf("set HypervisorMachine %s publishedPorts: %v", key, err)
	}
}

// newKubeconfigAllocationFixture wires the readiness path up to (but not
// including) the final healthy reconcile: linked cluster and control plane,
// real generated PKI, a healthy healthz seam, one completed reconcile that
// created the Machine, bootstrap config, and PKI Secret. The caller creates
// the cp-0 infrastructure machine with its addresses and published ports and
// drives the remaining reconciles.
func newKubeconfigAllocationFixture(
	t *testing.T,
	c client.Client,
	namespace string,
) (*controlPlaneFixture, *linkedCluster, *linkedControlPlane) {
	t.Helper()
	lc := newLinkedCluster(t, c, namespace, "capi-cluster")
	machineName := lc.name + "-cp-0"
	pk := mustGenerateClusterPKI(t, testCPIP, machineName)
	fx := newControlPlaneFixtureWithPKI(t, c, pk)
	fx.health.result = nil
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	return fx, lc, lcp
}

// TestControlPlaneKubeconfigServerURLFromPublishedAllocation pins REQ-009:
// the rendered kubeconfig server URL is https://127.0.0.1:<hostPort> taken
// from the cp machine's recorded 6443 allocation — not the static
// https://127.0.0.1:6443 of the pre-allocation flow.
func TestControlPlaneKubeconfigServerURLFromPublishedAllocation(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp := newKubeconfigAllocationFixture(t, c, "cp-kubeconfig-allocation")

	const allocatedHostPort = int32(26443)
	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{
		VMPort:   6443,
		HostPort: allocatedHostPort,
	})

	fx.reconcileControlPlane(t, lcp.cp)

	secret := wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	data, ok := secret.Data[kubeconfigSecretDataKey]
	if !ok {
		t.Fatalf("kubeconfig Secret has no %q data key (keys %v)", kubeconfigSecretDataKey, secret.Data)
	}
	doc := parseKubeconfig(t, data)
	wantServer := "https://127.0.0.1:26443"
	if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != wantServer {
		t.Errorf("kubeconfig server = %+v, want %q (from the recorded 6443 allocation)", doc.Clusters, wantServer)
	}
}

// TestControlPlaneKubeconfigUpdatesInPlaceWhenAllocationChanges pins the
// reconcile-to-current clause of REQ-009 / VC-07: when the recorded 6443
// allocation changes after the control plane is already ready, the next
// reconcile updates the EXISTING Secret's data.value in place — no duplicate
// Secret, stale URL gone.
func TestControlPlaneKubeconfigUpdatesInPlaceWhenAllocationChanges(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp := newKubeconfigAllocationFixture(t, c, "cp-kubeconfig-reconcile")

	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{
		VMPort:   6443,
		HostPort: 26443,
	})
	fx.reconcileControlPlane(t, lcp.cp)

	secretKey := kubeconfigSecretKey(lc.name, lc.namespace)
	first := wantKubeconfigSecret(t, c, secretKey)
	firstDoc := parseKubeconfig(t, first.Data[kubeconfigSecretDataKey])
	if len(firstDoc.Clusters) != 1 || firstDoc.Clusters[0].Cluster.Server != "https://127.0.0.1:26443" {
		t.Fatalf("initial kubeconfig server = %+v, want https://127.0.0.1:26443", firstDoc.Clusters)
	}

	// Force a re-allocation: the machine controller records a new 6443 host
	// port (e.g. after k8netd state loss and republish).
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{
		VMPort:   6443,
		HostPort: 26555,
	})
	fx.reconcileControlPlane(t, lcp.cp)

	if got := countSecretsNamed(t, c, lc.namespace, lc.name+"-kubeconfig"); got != 1 {
		t.Fatalf("kubeconfig Secrets after re-allocation = %d, want exactly 1 (updated in place)", got)
	}
	second := wantKubeconfigSecret(t, c, secretKey)
	secondDoc := parseKubeconfig(t, second.Data[kubeconfigSecretDataKey])
	wantServer := "https://127.0.0.1:26555"
	if len(secondDoc.Clusters) != 1 || secondDoc.Clusters[0].Cluster.Server != wantServer {
		t.Errorf("kubeconfig server after re-allocation = %+v, want %q (reconciled to current)", secondDoc.Clusters, wantServer)
	}
}

// TestControlPlaneKubeconfigWaitsForPublishedAllocation pins the missing-
// allocation gate: a cp machine that reports an address but has no recorded
// 6443 allocation yet cannot yield an endpoint — the reconcile requeues
// without polling the apiserver (TASK-021 P3: no fallback to the VM IP), no
// kubeconfig Secret is written, and the control plane stays not-ready rather
// than falling back to the colliding static 6443.
func TestControlPlaneKubeconfigWaitsForPublishedAllocation(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp := newKubeconfigAllocationFixture(t, c, "cp-kubeconfig-waiting")

	newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)

	res, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp.cp)})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	wantRequeue(t, res)

	if got := len(fx.health.calls); got != 0 {
		t.Errorf("apiserver healthz seam called %d times with no recorded allocation, want 0", got)
	}

	wantNoKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	got := getControlPlane(t, c, lcp.cp)
	wantCPStatus(t, got, false, false)
}

// TestControlPlaneHealthProbeUsesAllocatedHostPort pins the TASK-021 P3 fix:
// the readiness probe dials https://127.0.0.1:<hostPort> — exactly the 6443
// allocation recorded on the cp machine's status.publishedPorts, like the
// kubeconfig server URL — never the VM internal IP and never a static port.
func TestControlPlaneHealthProbeUsesAllocatedHostPort(t *testing.T) {
	c := mustReconcileClient(t)
	fx, _, lcp := newKubeconfigAllocationFixture(t, c, "cp-probe-allocation")

	const allocatedHostPort = int32(30123)
	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{
		VMPort:   6443,
		HostPort: allocatedHostPort,
	})

	fx.reconcileControlPlane(t, lcp.cp)

	if len(fx.health.calls) == 0 {
		t.Fatal("apiserver healthz seam never called")
	}
	call := fx.health.calls[0]
	if call.host != "127.0.0.1" || call.port != allocatedHostPort {
		t.Errorf("healthz polled endpoint %s:%d, want 127.0.0.1:%d (the recorded allocation)", call.host, call.port, allocatedHostPort)
	}
	if call.host == testCPIP {
		t.Errorf("healthz polled the VM internal IP %q, which has no host route", call.host)
	}
}

// TestControlPlaneEndpointResolutionIgnoresWorkerMachines pins the worker
// exclusion of REQ-009: a worker machine of the same cluster carrying its own
// published 6443 allocation never wins endpoint resolution — the kubeconfig
// renders from the control-plane machine's allocation only.
func TestControlPlaneEndpointResolutionIgnoresWorkerMachines(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp := newKubeconfigAllocationFixture(t, c, "cp-kubeconfig-workers")

	// A worker machine of the same cluster with a published 6443 allocation
	// that must be ignored. Only the control-plane role label differs from
	// the cp machine.
	lm := newLinkedMachine(t, c, lc, "worker-0", false)
	workerMachineKey := client.ObjectKey{Namespace: lm.machine.Namespace, Name: lm.machine.Name}
	workerMachine := &clusterv1.Machine{}
	if err := c.Get(t.Context(), workerMachineKey, workerMachine); err != nil {
		t.Fatalf("get worker Machine: %v", err)
	}
	if workerMachine.Labels == nil {
		workerMachine.Labels = map[string]string{}
	}
	workerMachine.Labels[clusterv1.ClusterNameLabel] = lc.name
	if err := c.Update(t.Context(), workerMachine); err != nil {
		t.Fatalf("label worker Machine: %v", err)
	}
	setHMPublishedPorts(t, c, lm.hm, infrastructurev1alpha1.MachinePublishedPort{
		VMPort:   6443,
		HostPort: 39999,
	})

	const cpAllocatedHostPort = int32(26443)
	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{
		VMPort:   6443,
		HostPort: cpAllocatedHostPort,
	})

	fx.reconcileControlPlane(t, lcp.cp)

	secret := wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	doc := parseKubeconfig(t, secret.Data[kubeconfigSecretDataKey])
	wantServer := "https://127.0.0.1:26443"
	if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != wantServer {
		t.Errorf("kubeconfig server = %+v, want %q (worker allocation 39999 must be ignored)", doc.Clusters, wantServer)
	}
}
