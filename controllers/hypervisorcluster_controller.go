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

// Package controllers implements the provider's Kubernetes controllers. The
// HypervisorCluster reconciler owns the host network stack of one cluster:
// the lab bridge, the dnsmasq DNS forwarder, and the nftables NAT table, all
// behind injectable seams so the reconcile contract is testable without host
// privileges.
package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/cluster-api/util/annotations"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/dnsmasq"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ipam"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/networking"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/nft"
)

const (
	// hypervisorClusterFinalizer protects the host network stack of a cluster
	// until the controller has torn it down.
	hypervisorClusterFinalizer = "hypervisorcluster.infrastructure.cluster.x-k8s.io"

	// defaultPoolStart and defaultPoolEnd bound the static IP pool handed to
	// the per-cluster allocator.
	defaultPoolStart = "192.168.124.20"
	defaultPoolEnd   = "192.168.124.200"

	// defaultBridgeName is the effective bridge name when the spec leaves it
	// empty.
	defaultBridgeName = "k8sbr0"

	// defaultControlPlanePort is the workload API server port published in
	// status.controlPlaneEndpoint.
	defaultControlPlanePort = 6443
)

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters;clusters/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines;machines/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes,verbs=get;list;watch

// HypervisorClusterReconciler reconciles a HypervisorCluster object.
type HypervisorClusterReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Net orchestrates the cluster bridge and machine TAPs over netlink.
	Net *networking.Manager
	// Nft owns the cluster NAT table.
	Nft *nft.Manager
	// Dnsmasq owns the cluster DNS forwarder subprocess.
	Dnsmasq *dnsmasq.Manager
	// NewAllocator constructs the per-cluster static IP allocator from the
	// cluster CIDR, gateway, and pool bounds.
	NewAllocator func(clusterCIDR, gateway, poolStart, poolEnd string) (*ipam.Allocator, error)
}

// Reconcile moves the current state of the HypervisorCluster towards the
// desired state: it resolves the linked CAPI Cluster, applies the paused
// gate, provisions the host network stack on normal reconciles, and tears the
// stack down on deletion.
func (r *HypervisorClusterReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := r.Get(ctx, req.NamespacedName, hc); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get HypervisorCluster %q: %w", req.NamespacedName, err)
	}

	cluster, err := r.getLinkedCluster(ctx, hc)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("linked Cluster not found, waiting for the Cluster link")
		return ctrl.Result{}, nil
	}

	if annotations.HasPaused(cluster) || annotations.HasPaused(hc) {
		log.Info("HypervisorCluster or linked Cluster is paused, skipping reconcile")
		return ctrl.Result{}, nil
	}

	if !hc.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, hc)
	}

	if !controllerutil.ContainsFinalizer(hc, hypervisorClusterFinalizer) {
		controllerutil.AddFinalizer(hc, hypervisorClusterFinalizer)
		if err := r.Update(ctx, hc); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to HypervisorCluster: %w", err)
		}
	}

	if err := r.reconcileNormal(ctx, hc, cluster); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileNormal provisions the host network stack in order — bridge,
// dnsmasq, NAT ruleset, then the IPAM allocator — and reports the object ready
// with the InfrastructureReady condition true once every step succeeded.
func (r *HypervisorClusterReconciler) reconcileNormal(
	ctx context.Context,
	hc *infrastructurev1alpha1.HypervisorCluster,
	cluster *clusterv1.Cluster,
) error {
	network := &hc.Spec.Network

	bridgeName := effectiveBridgeName(network)
	if err := r.Net.EnsureBridge(bridgeName); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedProvision", "failed to ensure bridge %q: %v", bridgeName, err)
		return fmt.Errorf("ensure bridge %q: %w", bridgeName, err)
	}

	if err := r.Dnsmasq.Start(ctx); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedProvision", "failed to start dnsmasq: %v", err)
		return fmt.Errorf("start dnsmasq: %w", err)
	}

	if err := r.Nft.Apply(ctx); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedProvision", "failed to apply NAT ruleset: %v", err)
		return fmt.Errorf("apply NAT ruleset: %w", err)
	}

	if _, err := r.NewAllocator(network.CIDR, network.Gateway, defaultPoolStart, defaultPoolEnd); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedProvision", "failed to construct ipam allocator: %v", err)
		return fmt.Errorf("construct ipam allocator: %w", err)
	}

	hc.Status.Ready = true
	markInfrastructureReady(hc)

	if err := r.reconcileControlPlaneEndpoint(ctx, cluster, hc); err != nil {
		return err
	}

	if err := r.Status().Update(ctx, hc); err != nil {
		return fmt.Errorf("update HypervisorCluster status: %w", err)
	}

	return nil
}

// reconcileDelete tears the host network stack down — dnsmasq first, then the
// NAT table, then the bridge — and drops the finalizer so the object is
// reclaimed. Every step is idempotent, so a repeated delete reconcile adds no
// further host calls.
func (r *HypervisorClusterReconciler) reconcileDelete(
	ctx context.Context,
	hc *infrastructurev1alpha1.HypervisorCluster,
) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(hc, hypervisorClusterFinalizer) {
		return ctrl.Result{}, nil
	}

	if err := r.Dnsmasq.Stop(ctx); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedTeardown", "failed to stop dnsmasq: %v", err)
		return ctrl.Result{}, fmt.Errorf("stop dnsmasq: %w", err)
	}

	if err := r.Nft.Delete(ctx); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedTeardown", "failed to delete NAT table: %v", err)
		return ctrl.Result{}, fmt.Errorf("delete NAT table: %w", err)
	}

	if err := r.Net.DeleteBridge(effectiveBridgeName(&hc.Spec.Network)); err != nil {
		r.Recorder.Eventf(hc, corev1.EventTypeWarning, "FailedTeardown", "failed to delete bridge: %v", err)
		return ctrl.Result{}, fmt.Errorf("delete bridge: %w", err)
	}

	controllerutil.RemoveFinalizer(hc, hypervisorClusterFinalizer)
	if err := r.Update(ctx, hc); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from HypervisorCluster: %w", err)
	}

	return ctrl.Result{}, nil
}

// reconcileControlPlaneEndpoint publishes status.controlPlaneEndpoint when the
// linked control plane reports initialized and a control-plane machine holds a
// static internal IP: the host is the first such IP and the port is 6443. An
// absent or uninitialized control plane leaves the endpoint untouched without
// error.
func (r *HypervisorClusterReconciler) reconcileControlPlaneEndpoint(
	ctx context.Context,
	cluster *clusterv1.Cluster,
	hc *infrastructurev1alpha1.HypervisorCluster,
) error {
	if cluster.Spec.ControlPlaneRef == nil {
		return nil
	}

	cpRef := cluster.Spec.ControlPlaneRef
	namespace := cpRef.Namespace
	if namespace == "" {
		namespace = cluster.Namespace
	}

	cp := &controlplanev1alpha1.HypervisorControlPlane{}
	key := client.ObjectKey{Namespace: namespace, Name: cpRef.Name}
	if err := r.Get(ctx, key, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get control plane %q: %w", key, err)
	}
	if !cp.Status.Initialized {
		return nil
	}

	machines := &clusterv1.MachineList{}
	selector := client.MatchingLabels{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}
	if err := r.List(ctx, machines, client.InNamespace(cluster.Namespace), selector); err != nil {
		return fmt.Errorf("list control-plane machines: %w", err)
	}

	for i := range machines.Items {
		ip, ok, err := r.machineInternalIP(ctx, &machines.Items[i])
		if err != nil {
			return err
		}
		if !ok {
			continue
		}
		hc.Status.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: ip, Port: defaultControlPlanePort}
		return nil
	}

	return nil
}

// machineInternalIP returns the static internal IP of the HypervisorMachine
// backing the given CAPI Machine, when the machine holds one.
func (r *HypervisorClusterReconciler) machineInternalIP(
	ctx context.Context,
	machine *clusterv1.Machine,
) (string, bool, error) {
	ref := machine.Spec.InfrastructureRef
	if ref.Kind != "HypervisorMachine" || ref.Name == "" {
		return "", false, nil
	}

	// Infrastructure references are namespaced to the machine by CAPI
	// convention; the reference's namespace may be dropped by the API
	// round-trip, so fall back to the machine's own namespace.
	namespace := ref.Namespace
	if namespace == "" {
		namespace = machine.Namespace
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, hm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get infrastructure machine %q: %w", key, err)
	}

	for _, addr := range hm.Status.Addresses {
		if addr.Type == clusterv1.MachineInternalIP && addr.Address != "" {
			return addr.Address, true, nil
		}
	}

	return "", false, nil
}

// getLinkedCluster resolves the CAPI Cluster this object belongs to, through
// the owner reference first and the spec.clusterName link as a fallback. A
// missing Cluster is reported as (nil, nil): the controller waits for the
// link instead of erroring.
func (r *HypervisorClusterReconciler) getLinkedCluster(
	ctx context.Context,
	hc *infrastructurev1alpha1.HypervisorCluster,
) (*clusterv1.Cluster, error) {
	for _, ref := range hc.OwnerReferences {
		if ref.Kind != "Cluster" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != clusterv1.GroupVersion.Group {
			continue
		}
		cluster := &clusterv1.Cluster{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: hc.Namespace, Name: ref.Name}, cluster); err != nil {
			if apierrors.IsNotFound(err) {
				break
			}
			return nil, fmt.Errorf("get owner Cluster %q: %w", ref.Name, err)
		}
		return cluster, nil
	}

	if hc.Spec.ClusterName == "" {
		return nil, nil
	}
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: hc.Namespace, Name: hc.Spec.ClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get linked Cluster %q: %w", hc.Spec.ClusterName, err)
	}
	return cluster, nil
}

// SetupWithManager sets up the controller with the Manager, watching the
// primary HypervisorCluster kind and the CAPI Cluster and HypervisorMachine
// objects that drive cluster-level reconciles. The optional controller
// options are applied to the underlying controller in order, so a caller can
// tune e.g. the maximum concurrent reconciles.
func (r *HypervisorClusterReconciler) SetupWithManager(mgr ctrl.Manager, opts ...controller.Options) error {
	log := ctrl.Log.WithName("hypervisorcluster-controller")

	builder := ctrl.NewControllerManagedBy(mgr)
	for _, options := range opts {
		builder = builder.WithOptions(options)
	}

	return builder.
		For(&infrastructurev1alpha1.HypervisorCluster{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(mgr.GetScheme(), log, "")).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToHypervisorCluster),
		).
		Watches(
			&infrastructurev1alpha1.HypervisorMachine{},
			handler.EnqueueRequestsFromMapFunc(r.machineToHypervisorCluster),
		).
		Complete(r)
}

// clusterToHypervisorCluster maps a CAPI Cluster event to its linked
// HypervisorCluster through the infrastructure reference.
func (r *HypervisorClusterReconciler) clusterToHypervisorCluster(
	_ context.Context,
	obj client.Object,
) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}

	ref := cluster.Spec.InfrastructureRef
	if ref == nil || ref.Kind != "HypervisorCluster" || ref.Name == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name},
	}}
}

// machineToHypervisorCluster maps a HypervisorMachine event to the
// HypervisorCluster of its cluster, so a machine gaining its static IP
// re-reconciles the cluster control-plane endpoint.
func (r *HypervisorClusterReconciler) machineToHypervisorCluster(
	_ context.Context,
	obj client.Object,
) []reconcile.Request {
	machine, ok := obj.(*infrastructurev1alpha1.HypervisorMachine)
	if !ok || machine.Spec.ClusterName == "" {
		return nil
	}

	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.ClusterName},
	}}
}

// markInfrastructureReady upserts the InfrastructureReady condition as true on
// the cluster status, preserving any other conditions.
func markInfrastructureReady(hc *infrastructurev1alpha1.HypervisorCluster) {
	for i := range hc.Status.Conditions {
		if hc.Status.Conditions[i].Type != clusterv1.InfrastructureReadyCondition {
			continue
		}
		if hc.Status.Conditions[i].Status == corev1.ConditionTrue {
			return
		}
		hc.Status.Conditions[i].Status = corev1.ConditionTrue
		hc.Status.Conditions[i].LastTransitionTime = metav1.Now()
		return
	}

	hc.Status.Conditions = append(hc.Status.Conditions, clusterv1.Condition{
		Type:               clusterv1.InfrastructureReadyCondition,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
	})
}

// effectiveBridgeName returns the bridge name to ensure, defaulting to k8sbr0
// when the spec leaves it empty.
func effectiveBridgeName(network *infrastructurev1alpha1.HypervisorClusterNetworkSpec) string {
	if network.BridgeName == "" {
		return defaultBridgeName
	}
	return network.BridgeName
}
