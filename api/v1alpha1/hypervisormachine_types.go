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

package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
)

// HypervisorMachineSpec defines the desired state of HypervisorMachine.
type HypervisorMachineSpec struct {
	// ClusterName is the name of the Cluster this object belongs to.
	ClusterName string `json:"clusterName,omitempty"`

	// CPU is the number of vCPUs allocated to the VM.
	CPU int32 `json:"cpu,omitempty"`

	// RAM is the VM memory in MiB.
	RAM int32 `json:"ram,omitempty"`

	// Disk is the root disk size in MiB.
	Disk int32 `json:"disk,omitempty"`

	// MAC is an optional override for the VM MAC address. When empty the
	// controller derives the address from a stable hash of cluster/machine
	// name.
	MAC string `json:"mac,omitempty"`

	// ProviderID is the infrastructure provider ID in the form
	// hypervisor://<cluster>/<machine>. It is set by the controller once the
	// VM has booted; the Cluster API machine controller reads it from
	// spec.providerID per the infrastructure contract.
	// +optional
	ProviderID *string `json:"providerID,omitempty"`

	// RetainDiskOnDelete determines whether the root disk survives VM
	// deletion. Defaults to false: the disk is removed with the VM.
	RetainDiskOnDelete bool `json:"retainDiskOnDelete,omitempty"`
}

// MachinePublishedPort records one host-published endpoint of the machine's
// VM: the VM-side port and the host port k8netd allocated for it through the
// PublishPort RPC.
type MachinePublishedPort struct {
	// VMPort is the port inside the VM.
	VMPort int32 `json:"vmPort"`

	// HostPort is the host port k8netd allocated for VMPort.
	HostPort int32 `json:"hostPort"`
}

// InitializationStatus provides observations of the provider object
// initialization process. It is the shared shape the cluster-api v1beta2
// contract readers address under status.initialization.*:
// contract.InfrastructureMachine().Provisioned and
// contract.InfrastructureCluster().Provisioned both read
// status.initialization.provisioned.
// NOTE: fields in this struct are part of the Cluster API contract and are
// used to orchestrate initial provisioning.
type InitializationStatus struct {
	// Provisioned is true when the infrastructure provider reports the object
	// fully provisioned. The value is never updated after provisioning is
	// completed.
	// NOTE: this field is part of the Cluster API contract and it is used to
	// orchestrate provisioning.
	// +optional
	Provisioned bool `json:"provisioned,omitempty"`
}

// HypervisorMachineStatus defines the observed state of HypervisorMachine.
type HypervisorMachineStatus struct {
	// Ready indicates the VM process is running.
	Ready bool `json:"ready"`

	// Version is the Kubernetes version the VM boots, resolved from the
	// owning Machine's spec.version. Empty for Machines without a version.
	// +optional
	Version string `json:"version,omitempty"`

	// Image is the host path of the base image the VM's root disk was
	// converted from.
	// +optional
	Image string `json:"image,omitempty"`

	// Initialization provides observations of the HypervisorMachine
	// initialization process.
	// NOTE: this field is part of the Cluster API contract and it is used to
	// orchestrate initial provisioning.
	// +optional
	Initialization *InitializationStatus `json:"initialization,omitempty,omitzero"`

	// Addresses is the list of addresses for the VM: the allocated static IP
	// and the machine hostname.
	Addresses []clusterv1.MachineAddress `json:"addresses,omitempty"`

	// PublishedPorts lists the host-published endpoints of the machine
	// recorded through the k8netd PublishPort RPC. Control-plane machines
	// publish the apiserver (6443) and SSH (22); worker machines publish
	// nothing.
	PublishedPorts []MachinePublishedPort `json:"publishedPorts,omitempty"`

	// ProviderID is the infrastructure provider ID in the form
	// hypervisor://<cluster>/<machine>.
	ProviderID *string `json:"providerID,omitempty"`

	// FailureReason is a machine-readable reason for the last failure.
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage is a human-readable message for the last failure.
	FailureMessage string `json:"failureMessage,omitempty"`

	// Conditions defines current service state of the HypervisorMachine.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// HypervisorMachine is the Schema for the hypervisormachines API.
type HypervisorMachine struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorMachineSpec   `json:"spec,omitempty"`
	Status HypervisorMachineStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the machine.
func (m *HypervisorMachine) GetConditions() []metav1.Condition {
	return m.Status.Conditions
}

// SetConditions sets the status conditions of the machine.
func (m *HypervisorMachine) SetConditions(conditions []metav1.Condition) {
	m.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// HypervisorMachineList contains a list of HypervisorMachine.
type HypervisorMachineList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorMachine `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorMachine{}, &HypervisorMachineList{})
}
