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

package controlplanev1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// HypervisorUpgradePlanPhase is the lifecycle phase of a HypervisorUpgradePlan.
type HypervisorUpgradePlanPhase string

const (
	// UpgradePlanPhasePending is the phase of a plan that was created but not
	// yet validated against the cluster's current state.
	UpgradePlanPhasePending HypervisorUpgradePlanPhase = "Pending"

	// UpgradePlanPhaseRollingControlPlane is the phase of a plan whose current
	// step is replacing the control plane Machines at the step version.
	UpgradePlanPhaseRollingControlPlane HypervisorUpgradePlanPhase = "RollingControlPlane"

	// UpgradePlanPhaseRollingWorkers is the phase of a plan whose current step
	// is rolling the worker Machines to the step version (the MachineDeployments
	// were unpaused after the control plane finished the step).
	UpgradePlanPhaseRollingWorkers HypervisorUpgradePlanPhase = "RollingWorkers"

	// UpgradePlanPhaseCompleted is the terminal phase of a plan that upgraded
	// the cluster through every step.
	UpgradePlanPhaseCompleted HypervisorUpgradePlanPhase = "Completed"

	// UpgradePlanPhaseFailed is the terminal phase of a plan whose preflight
	// or an upgrade step failed. Recovery is explicit: fix the cause and
	// re-apply the plan (idempotent resume) or apply a reverse plan.
	UpgradePlanPhaseFailed HypervisorUpgradePlanPhase = "Failed"
)

// Terminal reports whether the phase ends the plan lifecycle.
func (p HypervisorUpgradePlanPhase) Terminal() bool {
	return p == UpgradePlanPhaseCompleted || p == UpgradePlanPhaseFailed
}

// UpgradeStepStatus reports the observed state of one version step of the
// plan.
type UpgradeStepStatus struct {
	// Version is the Kubernetes version this step upgrades the cluster to.
	// +required
	Version string `json:"version"`

	// Phase is the phase the step reached: RollingControlPlane,
	// RollingWorkers, or Completed.
	// +required
	Phase HypervisorUpgradePlanPhase `json:"phase"`
}

// HypervisorUpgradePlanSpec defines the desired state of a
// HypervisorUpgradePlan.
type HypervisorUpgradePlanSpec struct {
	// ClusterName is the name of the Cluster the plan upgrades. The plan
	// resolves the Cluster in its own namespace.
	// +required
	// +kubebuilder:validation:MinLength=1
	ClusterName string `json:"clusterName"`

	// ToVersion is the target Kubernetes version of the upgrade, v-prefixed
	// semver (e.g. "v1.38.0"). It must be strictly greater than the version
	// the cluster currently runs.
	// +required
	// +kubebuilder:validation:MinLength=1
	ToVersion string `json:"toVersion"`

	// Steps lists the ordered intermediate Kubernetes versions the upgrade
	// passes through before ToVersion, for multi-minor jumps that must respect
	// kubelet/apiserver skew. Each entry must be strictly greater than the
	// previous one and the last entry must equal ToVersion. When empty the
	// plan upgrades directly to ToVersion in one step.
	// +optional
	Steps []string `json:"steps,omitempty"`
}

// HypervisorUpgradePlanStatus defines the observed state of a
// HypervisorUpgradePlan.
type HypervisorUpgradePlanStatus struct {
	// Phase is the lifecycle phase of the plan.
	// +optional
	Phase HypervisorUpgradePlanPhase `json:"phase,omitempty"`

	// FromVersion is the Kubernetes version the cluster ran when the plan
	// started executing.
	// +optional
	FromVersion string `json:"fromVersion,omitempty"`

	// CurrentStep is the index into the resolved steps (0-based) the plan is
	// currently executing.
	// +optional
	CurrentStep int `json:"currentStep,omitempty"`

	// Steps reports the observed state of every resolved step.
	// +optional
	Steps []UpgradeStepStatus `json:"steps,omitempty"`

	// FailureReason is a machine-readable reason for the failure that moved
	// the plan to Failed.
	// +optional
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage is a human-readable message for the failure that moved
	// the plan to Failed.
	// +optional
	FailureMessage string `json:"failureMessage,omitempty"`

	// Conditions defines current service state of the plan.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Cluster",type="string",JSONPath=".spec.clusterName",description="Cluster the plan upgrades"
// +kubebuilder:printcolumn:name="To",type="string",JSONPath=".spec.toVersion",description="Target Kubernetes version"
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Plan lifecycle phase"

// HypervisorUpgradePlan is the Schema for the hypervisorupgradeplans API. It
// declaratively plans and executes a Kubernetes version upgrade of a lab
// cluster: the plan controller sequences each version step control-plane
// first (through the CAPI topology and the provider's replace-in-place
// control plane rolling) and then workers (by unpausing the cluster's
// MachineDeployments), while the control plane controller snapshots and
// restores etcd around every control plane Machine replacement.
type HypervisorUpgradePlan struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorUpgradePlanSpec   `json:"spec,omitempty"`
	Status HypervisorUpgradePlanStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the plan.
func (p *HypervisorUpgradePlan) GetConditions() []metav1.Condition {
	return p.Status.Conditions
}

// SetConditions sets the status conditions of the plan.
func (p *HypervisorUpgradePlan) SetConditions(conditions []metav1.Condition) {
	p.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// HypervisorUpgradePlanList contains a list of HypervisorUpgradePlan.
type HypervisorUpgradePlanList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorUpgradePlan `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorUpgradePlan{}, &HypervisorUpgradePlanList{})
}
