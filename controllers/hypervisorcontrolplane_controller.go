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

package controllers

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;create
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// HypervisorControlPlaneReconciler reconciles a HypervisorControlPlane
// object: it creates the control-plane Machine set (one Machine per replica
// with the control-plane role label), wires every Machine's bootstrap ref to
// a generated HypervisorConfig, and persists the cluster-scoped PKI Secret on
// the first replica. The config generation, the Machine persistence, and the
// PKI generation run behind injectable seams, so the reconcile contract is
// testable without generating any RSA key.
type HypervisorControlPlaneReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// NewConfig builds the per-machine bootstrap HypervisorConfig for the
	// Machine named machineName.
	NewConfig func(cp *controlplanev1alpha1.HypervisorControlPlane, machineName string) *bootstrapv1alpha1.HypervisorConfig
	// CreateMachine persists the per-replica CAPI Machine.
	CreateMachine func(ctx context.Context, machine *clusterv1.Machine) (client.Object, error)
	// GeneratePKI produces the cluster-scoped PKI material stored in the
	// conventional <cluster>-pki Secret on the first replica.
	GeneratePKI func() (pki.ClusterPKI, error)
}

// Reconcile moves the current state of the control-plane Machine set towards
// the desired state: it resolves the linked CAPI Cluster (the Cluster whose
// spec.controlPlaneRef names this HypervisorControlPlane), then for every
// replica index creates the deterministic Machine <control-plane-name>-<index>
// when it does not exist yet. Each Machine carries the cluster-name and
// control-plane role labels plus the machineTemplate metadata labels, its
// bootstrap ref points at a generated HypervisorConfig persisted in the
// control plane's namespace, and it is owned by the control plane. The
// cluster-scoped PKI Secret is generated and persisted once on the first
// replica; later reconciles read the existing Secret. A missing object is a
// no-op, and a control plane with no linked Cluster is left untouched without
// error. A failing PKI generation or Machine creation surfaces as a reconcile
// error that preserves the underlying error.
func (r *HypervisorControlPlaneReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	cp := &controlplanev1alpha1.HypervisorControlPlane{}
	if err := r.Get(ctx, req.NamespacedName, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get HypervisorControlPlane %q: %w", req.NamespacedName, err)
	}

	cluster, err := r.linkedCluster(ctx, cp)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("linked Cluster not found, waiting for the Cluster controlPlaneRef")
		return ctrl.Result{}, nil
	}

	replicas := cp.Spec.Replicas
	if replicas < 1 {
		replicas = 1
	}

	for i := int32(0); i < replicas; i++ {
		machineName := fmt.Sprintf("%s-%d", cp.Name, i)

		existing := &clusterv1.Machine{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: machineName}, existing); err == nil {
			continue
		} else if !apierrors.IsNotFound(err) {
			return ctrl.Result{}, fmt.Errorf("get Machine %q: %w", machineName, err)
		}

		if err := r.ensureClusterPKISecret(ctx, cp, cluster); err != nil {
			r.Recorder.Eventf(cp, corev1.EventTypeWarning, "FailedClusterPKI", "failed to ensure cluster PKI Secret: %v", err)
			return ctrl.Result{}, err
		}

		cfg := r.NewConfig(cp, machineName)
		if cfg.Namespace == "" {
			cfg.Namespace = cp.Namespace
		}
		cfg.Spec.ClusterName = cluster.Name
		if err := r.Create(ctx, cfg); err != nil && !apierrors.IsAlreadyExists(err) {
			return ctrl.Result{}, fmt.Errorf("create HypervisorConfig %q: %w", client.ObjectKeyFromObject(cfg), err)
		}

		machine, err := r.machineFor(cp, cluster, machineName, cfg)
		if err != nil {
			return ctrl.Result{}, err
		}
		if _, err := r.CreateMachine(ctx, machine); err != nil {
			r.Recorder.Eventf(cp, corev1.EventTypeWarning, "FailedCreateMachine", "failed to create Machine %q: %v", machineName, err)
			return ctrl.Result{}, fmt.Errorf("create Machine %q: %w", machineName, err)
		}
	}

	log.Info("reconciled control-plane Machines", "controlPlane", cp.Name, "replicas", replicas)

	return ctrl.Result{}, nil
}

// linkedCluster resolves the CAPI Cluster whose spec.controlPlaneRef names
// this HypervisorControlPlane, or nil when no such Cluster exists. Clusters
// are matched in the control plane's namespace by the reference name, kind,
// and namespace.
func (r *HypervisorControlPlaneReconciler) linkedCluster(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
) (*clusterv1.Cluster, error) {
	clusters := &clusterv1.ClusterList{}
	if err := r.List(ctx, clusters, client.InNamespace(cp.Namespace)); err != nil {
		return nil, fmt.Errorf("list Clusters in %q: %w", cp.Namespace, err)
	}
	for i := range clusters.Items {
		cluster := &clusters.Items[i]
		ref := cluster.Spec.ControlPlaneRef
		if ref == nil || ref.Name != cp.Name {
			continue
		}
		if ref.Kind != "" && ref.Kind != "HypervisorControlPlane" {
			continue
		}
		if ref.Namespace != "" && ref.Namespace != cp.Namespace {
			continue
		}
		return cluster, nil
	}
	return nil, nil
}

// ensureClusterPKISecret generates and persists the cluster-scoped PKI in the
// conventional <cluster>-pki Secret in the control plane's namespace, unless
// the Secret already exists. The data keys are exactly the pki.ClusterPKI
// field names, so a later reconcile reads the existing Secret and never
// regenerates.
func (r *HypervisorControlPlaneReconciler) ensureClusterPKISecret(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) error {
	key := client.ObjectKey{Namespace: cp.Namespace, Name: cluster.Name + "-pki"}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err == nil {
		return nil
	} else if !apierrors.IsNotFound(err) {
		return fmt.Errorf("get cluster PKI Secret %q: %w", key, err)
	}

	pk, err := r.GeneratePKI()
	if err != nil {
		return fmt.Errorf("generate cluster PKI: %w", err)
	}
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
		Data:       clusterPKISecretData(pk),
	}
	if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
		return fmt.Errorf("create cluster PKI Secret %q: %w", key, err)
	}

	return nil
}

// machineFor builds the CAPI Machine for one replica: the deterministic name
// <control-plane-name>-<index>, the cluster-name and control-plane role
// labels plus the machineTemplate metadata labels, the linked Cluster's name,
// the machineTemplate infrastructureRef, a bootstrap configRef naming the
// generated HypervisorConfig, and a controller owner reference to the control
// plane.
func (r *HypervisorControlPlaneReconciler) machineFor(
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
	machineName string,
	cfg *bootstrapv1alpha1.HypervisorConfig,
) (*clusterv1.Machine, error) {
	labels := map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	}
	for key, value := range cp.Spec.MachineTemplate.Metadata.Labels {
		labels[key] = value
	}

	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: cp.Namespace,
			Labels:    labels,
		},
		Spec: clusterv1.MachineSpec{
			ClusterName: cluster.Name,
			Bootstrap: clusterv1.Bootstrap{
				ConfigRef: &corev1.ObjectReference{
					APIVersion: bootstrapv1alpha1.GroupVersion.String(),
					Kind:       "HypervisorConfig",
					Name:       cfg.Name,
					Namespace:  cfg.Namespace,
				},
			},
			InfrastructureRef: cp.Spec.MachineTemplate.InfrastructureRef,
		},
	}

	if err := controllerutil.SetControllerReference(cp, machine, r.Scheme); err != nil {
		return nil, fmt.Errorf("set controller owner reference on Machine %q: %w", machineName, err)
	}

	return machine, nil
}

// SetupWithManager sets up the controller with the Manager, watching the
// primary HypervisorControlPlane kind, the Machines it owns, and the CAPI
// Cluster objects that link to control planes. The optional controller
// options are applied to the underlying controller in order, so a caller can
// tune e.g. the maximum concurrent reconciles.
func (r *HypervisorControlPlaneReconciler) SetupWithManager(mgr ctrl.Manager, opts ...controller.Options) error {
	log := ctrl.Log.WithName("hypervisorcontrolplane-controller")

	builder := ctrl.NewControllerManagedBy(mgr)
	for _, options := range opts {
		builder = builder.WithOptions(options)
	}

	return builder.
		For(&controlplanev1alpha1.HypervisorControlPlane{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(mgr.GetScheme(), log, "")).
		Owns(&clusterv1.Machine{}).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToHypervisorControlPlane),
		).
		Complete(r)
}

// clusterToHypervisorControlPlane maps a CAPI Cluster event to the control
// plane its spec.controlPlaneRef names.
func (r *HypervisorControlPlaneReconciler) clusterToHypervisorControlPlane(
	_ context.Context,
	obj client.Object,
) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}

	ref := cluster.Spec.ControlPlaneRef
	if ref == nil || ref.Kind != "HypervisorControlPlane" || ref.Name == "" {
		return nil
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = cluster.Namespace
	}

	return []reconcile.Request{{
		NamespacedName: client.ObjectKey{Namespace: namespace, Name: ref.Name},
	}}
}
