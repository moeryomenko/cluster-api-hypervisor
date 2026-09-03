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
	"slices"
	"time"

	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorupgradeplans,verbs=get;list;watch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorupgradeplans/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorupgradeplans/finalizers,verbs=update
// +kubebuilder:rbac:groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machinedeployments,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch

const (
	// upgradePlanRequeueInterval is the polling interval while a plan waits
	// for a phase of the current step to converge.
	upgradePlanRequeueInterval = 15 * time.Second

	// upgradePlanFinalizer holds the plan until its MachineDeployments are
	// unpaused, so deleting an in-flight plan never freezes the workers at
	// the old version forever.
	upgradePlanFinalizer = "upgradeplan.controlplane.cluster.x-k8s.io"

	// upgradePlanProgressingCondition is the condition type the plan reports
	// while an upgrade is in flight.
	upgradePlanProgressingCondition = "Progressing"

	// upgradePlanReadyCondition is the condition type the plan reports once
	// every step completed.
	upgradePlanReadyCondition = "Ready"
)

// HypervisorUpgradePlanReconciler reconciles a HypervisorUpgradePlan: it
// validates the plan against the cluster's current state (preflight), then
// sequences every version step control plane first and workers second. The
// control plane phase pauses the cluster's MachineDeployments (the CAPI
// pause annotation) and patches the Cluster's topology version, so the
// topology controller propagates the version to the control plane object and
// the provider's replace-in-place rolling takes over; the workers phase
// unpauses the MachineDeployments and waits for the CAPI MachineDeployment
// machinery to roll every worker Machine to the step version. A failing
// preflight or step moves the plan to the terminal Failed phase; recovery is
// explicit (fix the cause and re-apply — machines already at the right
// version are recognized, not re-replaced) or a reverse plan built on the
// retained etcd snapshots.
type HypervisorUpgradePlanReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// Reconcile advances the upgrade plan through its state machine. A missing
// plan is a no-op; a terminal plan is never touched again; a deleting plan
// unpauses its cluster's MachineDeployments before its finalizer is dropped.
func (r *HypervisorUpgradePlanReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	plan := &controlplanev1alpha1.HypervisorUpgradePlan{}
	if err := r.Get(ctx, req.NamespacedName, plan); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}

		return ctrl.Result{}, fmt.Errorf("get HypervisorUpgradePlan %q: %w", req.NamespacedName, err)
	}

	if !plan.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, plan)
	}

	if plan.Status.Phase.Terminal() {
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(plan, upgradePlanFinalizer) {
		controllerutil.AddFinalizer(plan, upgradePlanFinalizer)

		if err := r.Update(ctx, plan); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer to HypervisorUpgradePlan %q: %w", plan.Name, err)
		}

		return ctrl.Result{}, nil
	}

	cluster, err := r.targetCluster(ctx, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cluster == nil {
		return r.failPlan(ctx, plan, "ClusterNotFound",
			fmt.Sprintf("Cluster %q not found in namespace %q", plan.Spec.ClusterName, plan.Namespace))
	}

	// Initialize the plan status on the first execution: record the version
	// the cluster currently runs and resolve the step list.
	if plan.Status.Phase == "" || plan.Status.Phase == controlplanev1alpha1.UpgradePlanPhasePending {
		return r.beginPlan(ctx, plan, cluster)
	}

	steps := resolveUpgradeSteps(plan)
	if plan.Status.CurrentStep >= len(steps) {
		return r.completePlan(ctx, plan)
	}

	step := steps[plan.Status.CurrentStep]

	switch plan.Status.Phase {
	case controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane:
		return r.reconcileControlPlaneStep(ctx, plan, cluster, step)
	case controlplanev1alpha1.UpgradePlanPhaseRollingWorkers:
		return r.reconcileWorkersStep(ctx, plan, cluster, step)
	default:
		return ctrl.Result{}, fmt.Errorf(
			"HypervisorUpgradePlan %q is in unexpected phase %q", plan.Name, plan.Status.Phase,
		)
	}
}

// reconcileDelete unpauses the cluster's MachineDeployments and drops the
// finalizer, so a deleted plan can never leave the workers frozen at the old
// version.
func (r *HypervisorUpgradePlanReconciler) reconcileDelete(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
) (ctrl.Result, error) {
	cluster, err := r.targetCluster(ctx, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cluster != nil {
		if err := r.setMachineDeploymentsPaused(ctx, cluster, false); err != nil {
			return ctrl.Result{}, err
		}
	}

	controllerutil.RemoveFinalizer(plan, upgradePlanFinalizer)

	if err := r.Update(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer from HypervisorUpgradePlan %q: %w", plan.Name, err)
	}

	return ctrl.Result{}, nil
}

// beginPlan runs the preflight and initializes the plan status: every step
// version must be registered in the infrastructure cluster's image map, the
// control plane must have exactly one replica (the single-member etcd design
// only supports replace-in-place of one Machine), and the target version must
// be strictly greater than the version the cluster currently runs. On success
// the plan enters the control plane phase of the first step.
func (r *HypervisorUpgradePlanReconciler) beginPlan(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
	cluster *clusterv1.Cluster,
) (ctrl.Result, error) {
	hc, err := linkedHypervisorCluster(ctx, r.Client, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if hc == nil {
		return r.failPlan(ctx, plan, "InfrastructureNotFound",
			fmt.Sprintf("HypervisorCluster for Cluster %q not found", cluster.Name))
	}

	cp, err := r.controlPlane(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cp == nil {
		return r.failPlan(ctx, plan, "ControlPlaneNotFound",
			fmt.Sprintf("HypervisorControlPlane for Cluster %q not found", cluster.Name))
	}

	steps := resolveUpgradeSteps(plan)

	if replicas := max(cp.Spec.Replicas, 1); replicas != 1 {
		return r.failPlan(ctx, plan, "UnsupportedReplicas",
			fmt.Sprintf(
				"upgrades require exactly one control plane replica (per-node single-member etcd), but %s has %d",
				cp.Name, replicas,
			))
	}

	current := currentClusterVersion(cluster, cp)
	if current != "" && plan.Spec.ToVersion != "" {
		if err := checkVersionAdvance(current, plan.Spec.ToVersion); err != nil {
			return r.failPlan(ctx, plan, "VersionNotAdvancing", err.Error())
		}
	}

	missing := missingImageVersions(hc, steps)
	if len(missing) > 0 {
		return r.failPlan(ctx, plan, "ImageNotRegistered",
			fmt.Sprintf(
				"no base image registered on HypervisorCluster %q for versions %v; bake and register them before upgrading",
				hc.Name, missing,
			))
	}

	stepStatuses := make([]controlplanev1alpha1.UpgradeStepStatus, len(steps))
	for i, step := range steps {
		stepStatuses[i] = controlplanev1alpha1.UpgradeStepStatus{
			Version: step,
			Phase:   controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane,
		}
	}

	plan.Status.FromVersion = current
	plan.Status.CurrentStep = 0
	plan.Status.Steps = stepStatuses
	plan.Status.FailureReason = ""
	plan.Status.FailureMessage = ""
	plan.Status.Phase = controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:    upgradePlanProgressingCondition,
		Status:  metav1.ConditionTrue,
		Reason:  "UpgradeStarted",
		Message: fmt.Sprintf("upgrading from %s to %s", current, plan.Spec.ToVersion),
	})

	if err := r.Status().Update(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("initialize HypervisorUpgradePlan status: %w", err)
	}

	r.Recorder.Eventf(plan, "Normal", "UpgradeStarted", "upgrading Cluster %q from %s to %s",
		cluster.Name, current, plan.Spec.ToVersion)

	return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
}

// reconcileControlPlaneStep advances the control plane phase of the current
// step: it pauses the cluster's MachineDeployments, patches the Cluster's
// topology version to the step version, and waits until the control plane
// object, all its Machines, and the apiserver report the step version and
// readiness.
func (r *HypervisorUpgradePlanReconciler) reconcileControlPlaneStep(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
	cluster *clusterv1.Cluster,
	step string,
) (ctrl.Result, error) {
	if err := r.setMachineDeploymentsPaused(ctx, cluster, true); err != nil {
		return ctrl.Result{}, err
	}

	if cluster.Spec.Topology.Version != step {
		return r.patchTopologyVersion(ctx, plan, cluster, step)
	}

	cp, err := r.controlPlane(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cp == nil {
		return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
	}

	machines, err := r.controlPlaneMachines(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	replicas := max(cp.Spec.Replicas, 1)
	versionMatched := cp.Spec.Version == step
	ready := cp.Status.Ready && cp.Status.ReadyReplicas >= replicas

	for i := range machines {
		if machines[i].Spec.Version != step {
			versionMatched = false
			break
		}
	}

	if !versionMatched || !ready || len(machines) < int(replicas) {
		return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
	}

	r.markStepPhase(plan, step, controlplanev1alpha1.UpgradePlanPhaseRollingWorkers)

	plan.Status.Phase = controlplanev1alpha1.UpgradePlanPhaseRollingWorkers
	if err := r.Status().Update(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("advance HypervisorUpgradePlan to the workers phase: %w", err)
	}

	r.Recorder.Eventf(plan, "Normal", "ControlPlaneUpgraded",
		"control plane of Cluster %q is running %s", cluster.Name, step)

	return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
}

// reconcileWorkersStep advances the workers phase of the current step: it
// unpauses the cluster's MachineDeployments and waits until every
// MachineDeployment reports its template at the step version and all its
// replicas ready and up to date. When the last step completes the plan moves
// to the terminal Completed phase.
func (r *HypervisorUpgradePlanReconciler) reconcileWorkersStep(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
	cluster *clusterv1.Cluster,
	step string,
) (ctrl.Result, error) {
	if err := r.setMachineDeploymentsPaused(ctx, cluster, false); err != nil {
		return ctrl.Result{}, err
	}

	mds, err := r.machineDeployments(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}

	for i := range mds {
		md := &mds[i]
		if md.Spec.Template.Spec.Version != step {
			return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
		}

		if md.Spec.Replicas == nil {
			return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
		}

		if md.Status.UpToDateReplicas == nil || md.Status.ReadyReplicas == nil {
			return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
		}

		if *md.Status.UpToDateReplicas < *md.Spec.Replicas || *md.Status.ReadyReplicas < *md.Spec.Replicas {
			return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
		}
	}

	r.markStepPhase(plan, step, controlplanev1alpha1.UpgradePlanPhaseCompleted)

	steps := resolveUpgradeSteps(plan)
	if plan.Status.CurrentStep+1 >= len(steps) {
		return r.completePlan(ctx, plan)
	}

	plan.Status.CurrentStep++

	plan.Status.Phase = controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane
	if err := r.Status().Update(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("advance HypervisorUpgradePlan to step %d: %w", plan.Status.CurrentStep, err)
	}

	r.Recorder.Eventf(plan, "Normal", "StepCompleted",
		"workers of Cluster %q are running %s; continuing to the next step", cluster.Name, step)

	return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
}

// completePlan marks the plan Completed and unpauses the MachineDeployments
// one last time.
func (r *HypervisorUpgradePlanReconciler) completePlan(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
) (ctrl.Result, error) {
	cluster, err := r.targetCluster(ctx, plan)
	if err != nil {
		return ctrl.Result{}, err
	}

	if cluster != nil {
		if err := r.setMachineDeploymentsPaused(ctx, cluster, false); err != nil {
			return ctrl.Result{}, err
		}
	}

	plan.Status.Phase = controlplanev1alpha1.UpgradePlanPhaseCompleted
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:    upgradePlanProgressingCondition,
		Status:  metav1.ConditionFalse,
		Reason:  "UpgradeCompleted",
		Message: "every step completed",
	})
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:    upgradePlanReadyCondition,
		Status:  metav1.ConditionTrue,
		Reason:  "UpgradeCompleted",
		Message: fmt.Sprintf("cluster is running %s", plan.Spec.ToVersion),
	})

	if err := r.Status().Update(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("complete HypervisorUpgradePlan: %w", err)
	}

	r.Recorder.Eventf(plan, "Normal", "UpgradeCompleted", "Cluster %q is running %s",
		plan.Spec.ClusterName, plan.Spec.ToVersion)

	return ctrl.Result{}, nil
}

// failPlan moves the plan to the terminal Failed phase. The
// MachineDeployments are deliberately left paused: freezing the workers at
// the old version is the safe state, and the plan's delete path (or the next
// plan) unpauses them.
func (r *HypervisorUpgradePlanReconciler) failPlan(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
	reason, message string,
) (ctrl.Result, error) {
	plan.Status.Phase = controlplanev1alpha1.UpgradePlanPhaseFailed
	plan.Status.FailureReason = reason
	plan.Status.FailureMessage = message
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:    upgradePlanProgressingCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
	meta.SetStatusCondition(&plan.Status.Conditions, metav1.Condition{
		Type:    upgradePlanReadyCondition,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})

	if err := r.Status().Update(ctx, plan); err != nil {
		return ctrl.Result{}, fmt.Errorf("fail HypervisorUpgradePlan: %w", err)
	}

	r.Recorder.Eventf(plan, "Warning", reason, "%s", message)

	return ctrl.Result{}, nil
}

// markStepPhase records the phase a step reached in the plan status.
func (r *HypervisorUpgradePlanReconciler) markStepPhase(
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
	version string,
	phase controlplanev1alpha1.HypervisorUpgradePlanPhase,
) {
	for i := range plan.Status.Steps {
		if plan.Status.Steps[i].Version == version {
			plan.Status.Steps[i].Phase = phase
			return
		}
	}
}

// patchTopologyVersion patches the Cluster's topology version to the step
// version. The topology controller then propagates the version to the control
// plane object and the (paused) MachineDeployments.
func (r *HypervisorUpgradePlanReconciler) patchTopologyVersion(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
	cluster *clusterv1.Cluster,
	step string,
) (ctrl.Result, error) {
	if cluster.Spec.Topology.ClassRef.Name == "" {
		return r.failPlan(ctx, plan, "TopologyMissing",
			fmt.Sprintf("Cluster %q carries no topology; upgrades are only supported for ClusterClass clusters", cluster.Name))
	}

	patch := client.MergeFrom(cluster.DeepCopy())

	cluster.Spec.Topology.Version = step
	if err := r.Patch(ctx, cluster, patch); err != nil {
		return ctrl.Result{}, fmt.Errorf("patch topology version of Cluster %q to %s: %w", cluster.Name, step, err)
	}

	r.Recorder.Eventf(plan, "Normal", "TopologyVersionPatched",
		"Cluster %q topology version patched to %s", cluster.Name, step)

	return ctrl.Result{RequeueAfter: upgradePlanRequeueInterval}, nil
}

// setMachineDeploymentsPaused applies or removes the CAPI pause annotation on
// every MachineDeployment of the cluster. Pausing is what keeps the workers
// at the old version while the control plane replaces its Machines.
func (r *HypervisorUpgradePlanReconciler) setMachineDeploymentsPaused(
	ctx context.Context,
	cluster *clusterv1.Cluster,
	paused bool,
) error {
	mds, err := r.machineDeployments(ctx, cluster)
	if err != nil {
		return err
	}

	for i := range mds {
		md := &mds[i]

		_, hasPause := md.Annotations[clusterv1.PausedAnnotation]
		if paused && hasPause {
			continue
		}

		if !paused && !hasPause {
			continue
		}

		patch := client.MergeFrom(md.DeepCopy())
		if paused {
			if md.Annotations == nil {
				md.Annotations = map[string]string{}
			}

			md.Annotations[clusterv1.PausedAnnotation] = ""
		} else {
			delete(md.Annotations, clusterv1.PausedAnnotation)
		}

		if err := r.Patch(ctx, md, patch); err != nil {
			return fmt.Errorf("set paused=%t on MachineDeployment %q: %w", paused, md.Name, err)
		}
	}

	return nil
}

// targetCluster resolves the Cluster the plan upgrades, or nil when it does
// not exist (yet).
func (r *HypervisorUpgradePlanReconciler) targetCluster(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
) (*clusterv1.Cluster, error) {
	cluster := &clusterv1.Cluster{}

	key := types.NamespacedName{Namespace: plan.Namespace, Name: plan.Spec.ClusterName}
	if err := r.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get Cluster %q: %w", key, err)
	}

	return cluster, nil
}

// controlPlane resolves the HypervisorControlPlane the Cluster's
// controlPlaneRef names, or nil when the reference is not defined or does not
// resolve.
func (r *HypervisorUpgradePlanReconciler) controlPlane(
	ctx context.Context,
	cluster *clusterv1.Cluster,
) (*controlplanev1alpha1.HypervisorControlPlane, error) {
	ref := cluster.Spec.ControlPlaneRef
	if !ref.IsDefined() || ref.Kind != "HypervisorControlPlane" || ref.Name == "" {
		return nil, nil
	}

	cp := &controlplanev1alpha1.HypervisorControlPlane{}

	key := client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, cp); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get HypervisorControlPlane %q: %w", key, err)
	}

	return cp, nil
}

// controlPlaneMachines lists the control-plane Machines of the cluster.
func (r *HypervisorUpgradePlanReconciler) controlPlaneMachines(
	ctx context.Context,
	cluster *clusterv1.Cluster,
) ([]clusterv1.Machine, error) {
	machines := &clusterv1.MachineList{}
	if err := r.List(ctx, machines, client.InNamespace(cluster.Namespace), client.MatchingLabels(map[string]string{
		clusterv1.ClusterNameLabel:         cluster.Name,
		clusterv1.MachineControlPlaneLabel: "",
	})); err != nil {
		return nil, fmt.Errorf("list control-plane Machines of Cluster %q: %w", cluster.Name, err)
	}

	return machines.Items, nil
}

// machineDeployments lists the MachineDeployments of the cluster.
func (r *HypervisorUpgradePlanReconciler) machineDeployments(
	ctx context.Context,
	cluster *clusterv1.Cluster,
) ([]clusterv1.MachineDeployment, error) {
	mds := &clusterv1.MachineDeploymentList{}
	if err := r.List(ctx, mds, client.InNamespace(cluster.Namespace), client.MatchingLabels(map[string]string{
		clusterv1.ClusterNameLabel: cluster.Name,
	})); err != nil {
		return nil, fmt.Errorf("list MachineDeployments of Cluster %q: %w", cluster.Name, err)
	}

	return mds.Items, nil
}

// resolveUpgradeSteps returns the ordered version list of the plan: the
// declared steps, or the single target version when no steps are declared.
func resolveUpgradeSteps(plan *controlplanev1alpha1.HypervisorUpgradePlan) []string {
	if len(plan.Spec.Steps) > 0 {
		return slices.Clone(plan.Spec.Steps)
	}

	return []string{plan.Spec.ToVersion}
}

// currentClusterVersion reports the version the cluster currently runs: the
// control plane's spec version when set, otherwise the topology version.
func currentClusterVersion(cluster *clusterv1.Cluster, cp *controlplanev1alpha1.HypervisorControlPlane) string {
	if cp.Spec.Version != "" {
		return cp.Spec.Version
	}

	return cluster.Spec.Topology.Version
}

// checkVersionAdvance reports whether the target version advances on current.
func checkVersionAdvance(current, target string) error {
	comparison, err := compareSemver(current, target)
	if err != nil {
		return err
	}

	if comparison <= 0 {
		return fmt.Errorf("target version %q must be strictly greater than the current version %q", target, current)
	}

	return nil
}

// missingImageVersions returns the step versions without a registered base
// image on the infrastructure cluster.
func missingImageVersions(hc *infrastructurev1alpha1.HypervisorCluster, steps []string) []string {
	registered := make(map[string]bool, len(hc.Spec.Images))
	for _, image := range hc.Spec.Images {
		registered[image.Version] = true
	}

	var missing []string

	for _, step := range steps {
		if !registered[step] {
			missing = append(missing, step)
		}
	}

	return missing
}

// compareSemver compares two v-prefixed semver versions.
func compareSemver(a, b string) (int, error) {
	if !semver.IsValid(a) || !semver.IsValid(b) {
		return 0, fmt.Errorf("versions must be v-prefixed semver: %q vs %q", a, b)
	}

	return semver.Compare(a, b), nil
}

// SetupWithManager sets up the plan controller: it watches plans, the
// Clusters a plan targets, the control planes whose versions the plan gates
// on, and the Machines and MachineDeployments whose versions it tracks. The
// optional controller options are applied to the underlying controller in
// order.
func (r *HypervisorUpgradePlanReconciler) SetupWithManager(mgr ctrl.Manager, opts ...controller.Options) error {
	builder := ctrl.NewControllerManagedBy(mgr)
	for _, options := range opts {
		builder = builder.WithOptions(options)
	}

	return builder.
		For(&controlplanev1alpha1.HypervisorUpgradePlan{}).
		Watches(
			&clusterv1.Cluster{},
			handler.EnqueueRequestsFromMapFunc(r.clusterToPlans),
		).
		Watches(
			&controlplanev1alpha1.HypervisorControlPlane{},
			handler.EnqueueRequestsFromMapFunc(r.objectClusterToPlans),
		).
		Watches(
			&clusterv1.MachineDeployment{},
			handler.EnqueueRequestsFromMapFunc(r.objectClusterToPlans),
		).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.objectClusterToPlans),
		).
		Complete(r)
}

// clusterToPlans maps a Cluster event to the plans targeting it.
func (r *HypervisorUpgradePlanReconciler) clusterToPlans(
	_ context.Context,
	obj client.Object,
) []reconcile.Request {
	cluster, ok := obj.(*clusterv1.Cluster)
	if !ok {
		return nil
	}

	return r.planRequests(cluster.Namespace, cluster.Name)
}

// objectClusterToPlans maps an object carrying the cluster-name label to the
// plans targeting that cluster.
func (r *HypervisorUpgradePlanReconciler) objectClusterToPlans(
	_ context.Context,
	obj client.Object,
) []reconcile.Request {
	clusterName := obj.GetLabels()[clusterv1.ClusterNameLabel]
	if clusterName == "" {
		return nil
	}

	return r.planRequests(obj.GetNamespace(), clusterName)
}

// planRequests lists the plans targeting the named cluster.
func (r *HypervisorUpgradePlanReconciler) planRequests(
	namespace, clusterName string,
) []reconcile.Request {
	plans := &controlplanev1alpha1.HypervisorUpgradePlanList{}
	if err := r.List(context.Background(), plans, client.InNamespace(namespace)); err != nil {
		return nil
	}

	requests := make([]reconcile.Request, 0, len(plans.Items))
	for i := range plans.Items {
		if plans.Items[i].Spec.ClusterName != clusterName {
			continue
		}

		requests = append(requests, reconcile.Request{
			NamespacedName: client.ObjectKeyFromObject(&plans.Items[i]),
		})
	}

	return requests
}
