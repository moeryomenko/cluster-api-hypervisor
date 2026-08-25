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
)

// HypervisorConfigTemplateSpec defines the desired state of
// HypervisorConfigTemplate.
type HypervisorConfigTemplateSpec struct {
	// Template is the specification of the desired behavior of the
	// HypervisorConfig created from this template.
	Template HypervisorConfigResource `json:"template"`
}

// HypervisorConfigResource describes the data needed to create a
// HypervisorConfig from a template.
type HypervisorConfigResource struct {
	// ObjectMeta is the standard object metadata of the configs created from
	// this template.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the specification of the desired behavior of the config.
	Spec HypervisorConfigSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// HypervisorConfigTemplate is the Schema for the hypervisorconfigtemplates
// API. It is the ClusterClass-compatible template kind the MachineDeployment
// controller clones into a concrete HypervisorConfig per worker Machine;
// nodeName stays unset here and defaults to each owning Machine's name.
type HypervisorConfigTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HypervisorConfigTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorConfigTemplateList contains a list of HypervisorConfigTemplate.
type HypervisorConfigTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorConfigTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorConfigTemplate{}, &HypervisorConfigTemplateList{})
}
