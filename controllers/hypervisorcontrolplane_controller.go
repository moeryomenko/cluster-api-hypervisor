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
	"bytes"
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

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
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

const (
	// controlPlaneReadyConditionType is the condition type the control plane
	// reports once the workload apiserver is healthy.
	controlPlaneReadyConditionType = clusterv1.ConditionType("ControlPlaneReady")

	// controlPlaneKubeconfigDataKey is the data key the conventional
	// <cluster>-kubeconfig Secret carries the rendered admin kubeconfig under.
	controlPlaneKubeconfigDataKey = "value"

	// controlPlaneKubeconfigUser is the user entry of the rendered admin
	// kubeconfig.
	controlPlaneKubeconfigUser = "admin"

	// controlPlaneAPIServerPort is the port the workload apiserver serves on.
	controlPlaneAPIServerPort = 6443

	// controlPlaneReadinessPollInterval is the delay before the next apiserver
	// healthz poll while the apiserver is not yet healthy.
	controlPlaneReadinessPollInterval = 30 * time.Second
)

// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=get;list;watch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// HypervisorControlPlaneReconciler reconciles a HypervisorControlPlane
// object: it creates the control-plane Machine set (one Machine per replica
// with the control-plane role label), wires every Machine's bootstrap ref to
// a generated HypervisorConfig, and persists the cluster-scoped PKI Secret on
// the first replica. Once the first control-plane Machine's VM is up, it
// polls the workload apiserver and, when healthy, renders the admin
// kubeconfig into the conventional <cluster>-kubeconfig Secret before marking
// the control plane initialized and ready. The config generation, the Machine
// persistence, the PKI generation, and the apiserver healthz poll run behind
// injectable seams, so the reconcile contract is testable without generating
// any RSA key or dialing a real apiserver.
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
	// conventional <cluster>-pki Secret on the first replica. cpIP is the
	// control-plane internal IP reserved through k8netd; it becomes the
	// apiserver certificate IP SAN.
	GeneratePKI func(cpIP string) (pki.ClusterPKI, error)
	// K8Netd is the k8netd JSON-RPC client used to reserve the first
	// control-plane Machine's IP before the cluster PKI is generated. It is
	// injected from main.go via cfg.K8NetdSocket.
	K8Netd *k8netd.Client
	// CheckAPIServerHealth polls the workload apiserver healthz endpoint at
	// https://cpIP:6443 with the cluster PKI material and returns nil exactly
	// when the apiserver is healthy.
	CheckAPIServerHealth func(ctx context.Context, cpIP string, clientCert, clientKey, caCert []byte) error
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
// replica; later reconciles read the existing Secret. Machines beyond the
// desired replica count are deleted, and the replica counters and version are
// reported on the control plane status. Once the first control-plane Machine's
// VM reports an address the reconciler polls the workload apiserver, and when
// healthy renders the admin kubeconfig into the conventional
// <cluster>-kubeconfig Secret and marks the control plane initialized and
// ready; a not-yet-healthy apiserver requeues without error. A missing object
// is a no-op, and a control plane with no linked Cluster is left untouched
// without error. A failing PKI generation or Machine creation surfaces as a
// reconcile error that preserves the underlying error.
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
			r.Recorder.Eventf(
				cp,
				corev1.EventTypeWarning,
				"FailedCreateMachine",
				"failed to create Machine %q: %v",
				machineName,
				err,
			)
			return ctrl.Result{}, fmt.Errorf("create Machine %q: %w", machineName, err)
		}
	}

	if err := r.scaleDownMachines(ctx, cp, cluster, replicas); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.updateScaleStatus(ctx, cp, cluster); err != nil {
		return ctrl.Result{}, err
	}

	log.Info("reconciled control-plane Machines", "controlPlane", cp.Name, "replicas", replicas)

	// Readiness and kubeconfig: once the first control-plane Machine's VM
	// reports an address, poll the workload apiserver and, when healthy,
	// render the admin kubeconfig into the conventional <cluster>-kubeconfig
	// Secret and mark the control plane initialized and ready. The kubeconfig
	// keeps reconciling to the currently recorded published endpoint even
	// after the control plane is ready (REQ-009), so a re-allocation reaches
	// consumers within one reconcile. A VM with no address yet, a missing
	// recorded allocation, or an apiserver that is not yet healthy is not an
	// error: the reconcile requeues and keeps polling.
	res, err := r.reconcileReadiness(ctx, cp, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if res.RequeueAfter > 0 {
		return res, nil
	}

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
// the Secret already exists. The apiserver SAN input is the cp-0 internal IP
// reserved through k8netd (reserveControlPlaneIP), never a pinned address.
// The data keys are exactly the pki.ClusterPKI field names, so a later
// reconcile reads the existing Secret and never regenerates.
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

	cpIP, err := r.reserveControlPlaneIP(ctx, cp, cluster)
	if err != nil {
		return err
	}
	pk, err := r.GeneratePKI(cpIP)
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

// reserveControlPlaneIP reserves the first control-plane Machine's internal
// IP through k8netd before the cluster PKI is generated: the contract
// reserves the control-plane IP before the VM boots so kubeadm/PKI config can
// reference it (REQ-004/REQ-006). The MAC is derived exactly as the machine
// controller derives it for <control-plane-name>-0 — same stable-hash family,
// same cluster and machine names — so the machine controller's later
// AllocateIP for that MAC returns this same reservation (idempotent by MAC).
// The network name is the HypervisorCluster name per the k8netd naming
// contract; a missing infrastructure link is an error so the reconcile
// retries once the link appears.
func (r *HypervisorControlPlaneReconciler) reserveControlPlaneIP(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) (string, error) {
	hc, err := linkedHypervisorCluster(ctx, r.Client, cluster)
	if err != nil {
		return "", err
	}
	if hc == nil {
		return "", fmt.Errorf("reserve control-plane IP: HypervisorCluster for Cluster %q not found", cluster.Name)
	}

	cp0MAC := mac.Derive(cluster.Name, fmt.Sprintf("%s-%d", cp.Name, 0))
	ip, err := r.K8Netd.AllocateIP(ctx, hc.Name, cp0MAC)
	if err != nil {
		return "", fmt.Errorf("reserve control-plane IP for MAC %q on network %q: %w", cp0MAC, hc.Name, err)
	}

	return ip, nil
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

// controlPlaneMachines lists the control-plane Machines of the linked Cluster:
// the Machines in the control plane's namespace carrying the cluster-name and
// control-plane role labels.
func (r *HypervisorControlPlaneReconciler) controlPlaneMachines(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) ([]clusterv1.Machine, error) {
	machines := &clusterv1.MachineList{}
	if err := r.List(ctx, machines, client.InNamespace(cp.Namespace), client.MatchingLabels(map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	})); err != nil {
		return nil, fmt.Errorf("list control-plane Machines in %q: %w", cp.Namespace, err)
	}

	return machines.Items, nil
}

// controlPlaneMachineIndex parses the deterministic replica index from a
// Machine name <control-plane-name>-<index> and reports whether the name
// follows the pattern.
func controlPlaneMachineIndex(machineName, cpName string) (int32, bool) {
	prefix := cpName + "-"
	if !strings.HasPrefix(machineName, prefix) {
		return 0, false
	}
	index, err := strconv.ParseInt(strings.TrimPrefix(machineName, prefix), 10, 32)
	if err != nil {
		return 0, false
	}

	return int32(index), true
}

// scaleDownMachines deletes the surplus control-plane Machines: every Machine
// whose deterministic name <control-plane-name>-<index> carries an index at or
// above the desired replica count. The retained Machines and their bootstrap
// configs are left untouched.
func (r *HypervisorControlPlaneReconciler) scaleDownMachines(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
	replicas int32,
) error {
	machines, err := r.controlPlaneMachines(ctx, cp, cluster)
	if err != nil {
		return err
	}
	for i := range machines {
		machine := &machines[i]
		index, ok := controlPlaneMachineIndex(machine.Name, cp.Name)
		if !ok || index < replicas {
			continue
		}
		if err := r.Delete(ctx, machine); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("delete surplus Machine %q: %w", machine.Name, err)
		}
		r.Recorder.Eventf(cp, corev1.EventTypeNormal, "DeletedMachine", "deleted surplus Machine %q", machine.Name)
	}

	return nil
}

// machineReplicaCounts counts the created control-plane Machines and how many
// of them are ready: a Machine counts as ready when its linked
// HypervisorMachine reports the VM ready.
func (r *HypervisorControlPlaneReconciler) machineReplicaCounts(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) (created, ready int32, err error) {
	machines, err := r.controlPlaneMachines(ctx, cp, cluster)
	if err != nil {
		return 0, 0, err
	}
	created = int32(len(machines))
	for i := range machines {
		hm := &infrastructurev1alpha1.HypervisorMachine{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: machines[i].Name}, hm); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return 0, 0, fmt.Errorf("get HypervisorMachine %q: %w", machines[i].Name, err)
		}
		if hm.Status.Ready {
			ready++
		}
	}

	return created, ready, nil
}

// updateScaleStatus reports the control-plane replica counters and version on
// the control plane status: status.replicas and status.updatedReplicas equal
// the created Machine count, status.readyReplicas equals the count of Machines
// whose linked VM is ready, status.unavailableReplicas equals the difference,
// and status.version mirrors spec.version. The write is idempotent across
// reconciles and re-pins after a scale-down.
func (r *HypervisorControlPlaneReconciler) updateScaleStatus(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) error {
	created, ready, err := r.machineReplicaCounts(ctx, cp, cluster)
	if err != nil {
		return err
	}

	version := cp.Spec.Version
	cp.Status.Replicas = created
	cp.Status.UpdatedReplicas = created
	cp.Status.ReadyReplicas = ready
	cp.Status.UnavailableReplicas = created - ready
	cp.Status.Version = &version

	if err := r.Status().Update(ctx, cp); err != nil {
		return fmt.Errorf("update HypervisorControlPlane scale status: %w", err)
	}

	return nil
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

// reconcileReadiness advances the control-plane readiness contract: it
// resolves the first control-plane Machine and its linked HypervisorMachine,
// and once the VM reports an InternalIP it polls the workload apiserver
// healthz endpoint through the CheckAPIServerHealth seam with the cluster PKI
// material. A healthy poll renders the admin kubeconfig into the conventional
// <cluster>-kubeconfig Secret — with the server URL taken from the 6443
// allocation recorded on the machine's status.publishedPorts (REQ-009); a
// machine without a recorded allocation has no endpoint to render and requeues
// rather than falling back to a static port — and marks the control plane
// initialized and ready. After readiness, the kubeconfig keeps reconciling to
// the currently recorded allocation so a changed endpoint updates the existing
// Secret in place. A VM with no address yet, or an apiserver that is not yet
// healthy, is not an error: the reconcile requeues after
// controlPlaneReadinessPollInterval so the later boot is eventually noticed.
func (r *HypervisorControlPlaneReconciler) reconcileReadiness(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	machine, err := r.firstControlPlaneMachine(ctx, cp, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		log.Info("control-plane Machine not created yet, waiting", "controlPlane", cp.Name)
		return ctrl.Result{RequeueAfter: controlPlaneReadinessPollInterval}, nil
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: cp.Namespace, Name: machine.Name}, hm); err != nil {
		if apierrors.IsNotFound(err) {
			log.Info("linked HypervisorMachine not found, waiting for the VM", "machine", machine.Name)
			return ctrl.Result{RequeueAfter: controlPlaneReadinessPollInterval}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get HypervisorMachine %q: %w", machine.Name, err)
	}

	cpIP := internalIPAddress(hm.Status.Addresses)
	if cpIP == "" {
		log.Info("control-plane VM has no InternalIP address yet, waiting", "machine", machine.Name)
		return ctrl.Result{RequeueAfter: controlPlaneReadinessPollInterval}, nil
	}

	pk, err := r.clusterPKI(ctx, cp, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if !cp.Status.Ready {
		if r.CheckAPIServerHealth == nil {
			log.Info("apiserver healthz seam not wired, waiting", "controlPlane", cp.Name)
			return ctrl.Result{RequeueAfter: controlPlaneReadinessPollInterval}, nil
		}
		if err := r.CheckAPIServerHealth(ctx, cpIP, pk.CA, pk.CAKey, pk.CA); err != nil {
			log.Info("workload apiserver not healthy yet, waiting", "ip", cpIP, "error", err)
			return ctrl.Result{RequeueAfter: controlPlaneReadinessPollInterval}, nil
		}
	}

	apiHostPort, ok := publishedHostPort(hm.Status.PublishedPorts, controlPlaneAPIServerPort)
	if !ok {
		log.Info("control-plane VM has no recorded apiserver port allocation yet, waiting", "machine", machine.Name)
		return ctrl.Result{RequeueAfter: controlPlaneReadinessPollInterval}, nil
	}

	serverURL := fmt.Sprintf("https://%s:%d", "127.0.0.1", apiHostPort)
	if err := r.ensureKubeconfigSecret(ctx, cp, cluster, serverURL, pk); err != nil {
		return ctrl.Result{}, err
	}

	if !cp.Status.Ready {
		cp.Status.Initialized = true
		cp.Status.Ready = true
		markControlPlaneReady(cp, corev1.ConditionTrue, "ControlPlaneReady", "control plane apiserver is healthy")
		if err := r.Status().Update(ctx, cp); err != nil {
			return ctrl.Result{}, fmt.Errorf("update HypervisorControlPlane readiness status: %w", err)
		}
		log.Info("control plane initialized and ready", "controlPlane", cp.Name, "server", serverURL)
	}

	return ctrl.Result{}, nil
}

// firstControlPlaneMachine resolves the first control-plane Machine of the
// linked Cluster: the lexically first Machine carrying the cluster-name and
// control-plane role labels. It returns nil while no such Machine exists.
func (r *HypervisorControlPlaneReconciler) firstControlPlaneMachine(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) (*clusterv1.Machine, error) {
	machines, err := r.controlPlaneMachines(ctx, cp, cluster)
	if err != nil {
		return nil, err
	}
	if len(machines) == 0 {
		return nil, nil
	}
	sort.Slice(machines, func(i, j int) bool {
		return machines[i].Name < machines[j].Name
	})

	return &machines[0], nil
}

// internalIPAddress returns the first InternalIP machine address, or "" when
// the machine reports no such address yet.
func internalIPAddress(addresses []clusterv1.MachineAddress) string {
	for _, addr := range addresses {
		if addr.Type == clusterv1.MachineInternalIP {
			return addr.Address
		}
	}
	return ""
}

// clusterPKI reads the cluster-scoped PKI material from the conventional
// <cluster>-pki Secret in the control plane's namespace.
func (r *HypervisorControlPlaneReconciler) clusterPKI(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
) (pki.ClusterPKI, error) {
	key := client.ObjectKey{Namespace: cp.Namespace, Name: cluster.Name + "-pki"}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, key, secret); err != nil {
		return pki.ClusterPKI{}, fmt.Errorf("get cluster PKI Secret %q: %w", key, err)
	}
	pk, err := decodeClusterPKI(secret.Data)
	if err != nil {
		return pki.ClusterPKI{}, fmt.Errorf("read stored cluster PKI Secret %q: %w", key, err)
	}

	return pk, nil
}

// ensureKubeconfigSecret reconciles the conventional <cluster>-kubeconfig
// Secret to the current server URL: it creates the Secret in the control
// plane's namespace under the "value" data key when absent, and updates the
// existing Secret's data value in place when the rendered document changes —
// write-once semantics become reconcile-to-current, so a re-allocation of the
// published apiserver port reaches consumers within one reconcile (REQ-009).
func (r *HypervisorControlPlaneReconciler) ensureKubeconfigSecret(
	ctx context.Context,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	cluster *clusterv1.Cluster,
	serverURL string,
	pk pki.ClusterPKI,
) error {
	key := client.ObjectKey{Namespace: cp.Namespace, Name: cluster.Name + "-kubeconfig"}
	data, err := pki.RenderKubeconfig(pk.CA, serverURL, controlPlaneKubeconfigUser, pk.CA, pk.CAKey)
	if err != nil {
		return fmt.Errorf("render admin kubeconfig: %w", err)
	}

	secret := &corev1.Secret{}
	switch err := r.Get(ctx, key, secret); {
	case apierrors.IsNotFound(err):
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: key.Name, Namespace: key.Namespace},
			Data:       map[string][]byte{controlPlaneKubeconfigDataKey: data},
		}
		if err := r.Create(ctx, secret); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("create kubeconfig Secret %q: %w", key, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get kubeconfig Secret %q: %w", key, err)
	}

	if bytes.Equal(secret.Data[controlPlaneKubeconfigDataKey], data) {
		return nil
	}
	if secret.Data == nil {
		secret.Data = map[string][]byte{}
	}
	secret.Data[controlPlaneKubeconfigDataKey] = data
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("update kubeconfig Secret %q: %w", key, err)
	}

	return nil
}

// markControlPlaneReady upserts the ControlPlaneReady condition on the control
// plane status with the given status, preserving any other conditions.
func markControlPlaneReady(
	cp *controlplanev1alpha1.HypervisorControlPlane,
	status corev1.ConditionStatus,
	reason, message string,
) {
	for i := range cp.Status.Conditions {
		if cp.Status.Conditions[i].Type != controlPlaneReadyConditionType {
			continue
		}
		if cp.Status.Conditions[i].Status == status {
			return
		}
		cp.Status.Conditions[i].Status = status
		cp.Status.Conditions[i].Reason = reason
		cp.Status.Conditions[i].Message = message
		cp.Status.Conditions[i].LastTransitionTime = metav1.Now()
		return
	}
	cp.Status.Conditions = append(cp.Status.Conditions, clusterv1.Condition{
		Type:               controlPlaneReadyConditionType,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}
