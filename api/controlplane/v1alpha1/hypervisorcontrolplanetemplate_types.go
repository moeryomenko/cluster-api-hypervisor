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

// HypervisorControlPlaneTemplateSpec defines the desired state of
// HypervisorControlPlaneTemplate.
type HypervisorControlPlaneTemplateSpec struct {
	// Template is the specification of the desired behavior of the
	// HypervisorControlPlane created from this template.
	Template HypervisorControlPlaneResource `json:"template"`
}

// HypervisorControlPlaneResource describes the data needed to create a
// HypervisorControlPlane from a template.
type HypervisorControlPlaneResource struct {
	// ObjectMeta is the standard object metadata of the control planes
	// created from this template.
	// +optional
	ObjectMeta metav1.ObjectMeta `json:"metadata,omitempty"`

	// Spec is the specification of the desired behavior of the control plane.
	Spec HypervisorControlPlaneSpec `json:"spec"`
}

// +kubebuilder:object:root=true

// HypervisorControlPlaneTemplate is the Schema for the
// hypervisorcontrolplanetemplates API. It is the ClusterClass-compatible
// template kind the topology controller clones into a concrete
// HypervisorControlPlane. Replicas and version stay topology-controlled: they
// are intentionally left unset here and applied by the Cluster topology.
type HypervisorControlPlaneTemplate struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec HypervisorControlPlaneTemplateSpec `json:"spec,omitempty"`
}

// +kubebuilder:object:root=true

// HypervisorControlPlaneTemplateList contains a list of
// HypervisorControlPlaneTemplate.
type HypervisorControlPlaneTemplateList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []HypervisorControlPlaneTemplate `json:"items"`
}

func init() {
	SchemeBuilder.Register(&HypervisorControlPlaneTemplate{}, &HypervisorControlPlaneTemplateList{})
}
