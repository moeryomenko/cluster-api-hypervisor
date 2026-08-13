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

package bootstrapv1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
)

// HypervisorConfigSpec defines the desired state of HypervisorConfig.
type HypervisorConfigSpec struct {
	// ClusterName is the name of the Cluster this object belongs to.
	ClusterName string `json:"clusterName"`

	// Role is the node role the bootstrap data is rendered for, either
	// "control-plane" or "worker". Defaults to the role derived from the
	// owning Machine's labels.
	Role string `json:"role,omitempty"`

	// NodeName is the name of the node the bootstrap data is rendered for.
	// Defaults to the owning Machine's name.
	NodeName string `json:"nodeName,omitempty"`

	// SSHPublicKey is an optional SSH public key added to the node's
	// authorized_keys. When empty the cluster-level key is used.
	SSHPublicKey string `json:"sshPublicKey,omitempty"`
}

// HypervisorConfigStatus defines the observed state of HypervisorConfig.
type HypervisorConfigStatus struct {
	// Ready indicates the bootstrap data Secret has been rendered.
	Ready bool `json:"ready,omitempty"`

	// DataSecretName is the name of the Secret that stores the rendered
	// bootstrap data for the node.
	DataSecretName *string `json:"dataSecretName,omitempty"`

	// FailureReason is a machine-readable reason for the last failure.
	FailureReason string `json:"failureReason,omitempty"`

	// FailureMessage is a human-readable message for the last failure.
	FailureMessage string `json:"failureMessage,omitempty"`

	// Conditions defines current service state of the HypervisorConfig.
	Conditions clusterv1.Conditions `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// HypervisorConfig is the Schema for the hypervisorconfigs API.
type HypervisorConfig struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   HypervisorConfigSpec   `json:"spec,omitempty"`
	Status HypervisorConfigStatus `json:"status,omitempty"`
}

// GetConditions returns the status conditions of the config.
func (c *HypervisorConfig) GetConditions() clusterv1.Conditions {
	return c.Status.Conditions
}

// SetConditions sets the status conditions of the config.
func (c *HypervisorConfig) SetConditions(conditions clusterv1.Conditions) {
	c.Status.Conditions = conditions
}

// +kubebuilder:object:root=true

// HypervisorConfigList contains a list of HypervisorConfig.
type HypervisorConfigList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorConfig `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorConfig{}, &HypervisorConfigList{})
}
