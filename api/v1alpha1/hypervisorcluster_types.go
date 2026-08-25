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

// HypervisorClusterSpec defines the desired state of HypervisorCluster.
type HypervisorClusterSpec struct {
	// ClusterName is the name of the Cluster this object belongs to.
	ClusterName string `json:"clusterName,omitempty"`

	// Network defines the cluster's host-side network stack: the bridge, the
	// NAT table, and the static IP pool the Machine controllers allocate from.
	Network HypervisorClusterNetworkSpec `json:"network,omitempty"`
}

// HypervisorClusterNetworkSpec defines the host network configuration the
// cluster controller provisions for this cluster.
type HypervisorClusterNetworkSpec struct {
	// CIDR is the cluster subnet, e.g. "192.168.124.0/24".
	CIDR string `json:"cidr,omitempty"`

	// Gateway is the address served on the bridge, e.g. "192.168.124.1".
	Gateway string `json:"gateway,omitempty"`

	// DNSIP is the address the DNS forwarder listens on, e.g. "192.168.124.1".
	DNSIP string `json:"dnsIP,omitempty"`

	// BridgeName is the Linux bridge the Machine TAPs attach to, e.g. "k8sbr0".
	BridgeName string `json:"bridgeName,omitempty"`

	// NATTable is the nftables table used for NAT and forwarding rules, e.g. "k8slab".
	NATTable string `json:"natTable,omitempty"`
}

// HypervisorClusterStatus defines the observed state of HypervisorCluster.
type HypervisorClusterStatus struct {
	// Ready indicates the host network stack is provisioned and the control
	// plane endpoint is reachable.
	Ready bool `json:"ready"`

	// ControlPlaneEndpoint is the workload cluster's API server endpoint.
	ControlPlaneEndpoint clusterv1.APIEndpoint `json:"controlPlaneEndpoint,omitempty,omitzero"`

	// FailureReason is a machine-readable reason for the last failure.
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage is a human-readable message for the last failure.
	FailureMessage string `json:"failureMessage,omitempty"`

	// Conditions defines current service state of the HypervisorCluster.
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// HypervisorCluster is the Schema for the hypervisorclusters API.
type HypervisorCluster struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorClusterSpec   `json:"spec,omitempty"`
	Status HypervisorClusterStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the cluster.
func (c *HypervisorCluster) GetConditions() []metav1.Condition {
	return c.Status.Conditions
}

// SetConditions sets the status conditions of the cluster.
func (c *HypervisorCluster) SetConditions(conditions []metav1.Condition) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// HypervisorClusterList contains a list of HypervisorCluster.
type HypervisorClusterList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorCluster `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorCluster{}, &HypervisorClusterList{})
}
