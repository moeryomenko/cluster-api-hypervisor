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
)

// HypervisorClusterTemplateSpec defines the desired state of
// HypervisorClusterTemplate.
type HypervisorClusterTemplateSpec struct {
	// Template is the specification of the desired behavior of the
	// HypervisorCluster created from this template.
	Template HypervisorClusterTemplateResource `json:"template"`
}

// HypervisorClusterTemplateResource describes the data needed to create a
// HypervisorCluster from a template.
type HypervisorClusterTemplateResource struct {
	// ObjectMeta is the standard object metadata of the cluster created from
	// this template.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the specification of the desired behavior of the cluster.
	Spec HypervisorClusterSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorClusterTemplate is the Schema for the hypervisorclustertemplates
// API. It is the ClusterClass-compatible template kind the topology
// controller clones into a concrete HypervisorCluster.
type HypervisorClusterTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HypervisorClusterTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorClusterTemplateList contains a list of HypervisorClusterTemplate.
type HypervisorClusterTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorClusterTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorClusterTemplate{}, &HypervisorClusterTemplateList{})
}
