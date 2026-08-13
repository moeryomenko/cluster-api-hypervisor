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

// HypervisorMachineTemplateSpec defines the desired state of
// HypervisorMachineTemplate.
type HypervisorMachineTemplateSpec struct {
	// Template describes the machines that will be created from this template.
	Template HypervisorMachineTemplateResource `json:"template"`
}

// HypervisorMachineTemplateResource describes the data needed to create a
// HypervisorMachine from a template.
type HypervisorMachineTemplateResource struct {
	// ObjectMeta is the standard object metadata of the machines created from
	// this template.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the specification of the desired behavior of the machine.
	Spec HypervisorMachineSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// HypervisorMachineTemplate is the Schema for the hypervisormachinetemplates
// API.
type HypervisorMachineTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HypervisorMachineTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorMachineTemplateList contains a list of HypervisorMachineTemplate.
type HypervisorMachineTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorMachineTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorMachineTemplate{}, &HypervisorMachineTemplateList{})
}
