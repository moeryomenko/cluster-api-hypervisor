// Contract tests for the HypervisorMachineTemplate infrastructure API type.
//
// Source of truth: spec REQ-002 (API groups, kinds, versions) and REQ-005
// (HypervisorMachineTemplate — immutable template contract); plan task 07.
// This file is the red phase: it fails to compile until api/v1alpha1 provides
// HypervisorMachineTemplate and registers it in the scheme.

package v1alpha1_test

import (
	"encoding/json"
	"reflect"
	"testing"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// newFullyPopulatedMachineTemplate builds a HypervisorMachineTemplate with
// every contract field set: the metadata reference types (labels and
// annotations maps) exercised by the deepcopy test, and a template spec that
// mirrors the full HypervisorMachineSpec under spec.template.spec. It is a
// maximally populated round-trip fixture, not a coherent reconciled state.
func newFullyPopulatedMachineTemplate() *v1alpha1.HypervisorMachineTemplate {
	return &v1alpha1.HypervisorMachineTemplate{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
			Kind:       "HypervisorMachineTemplate",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "lab-cluster-worker-template",
			Namespace:   "lab",
			Labels:      map[string]string{"cluster.x-k8s.io/cluster-name": "lab-cluster"},
			Annotations: map[string]string{"k8slabs.io/owner": "contract-tests"},
		},
		Spec: v1alpha1.HypervisorMachineTemplateSpec{
			Template: v1alpha1.HypervisorMachineTemplateResource{
				Spec: v1alpha1.HypervisorMachineSpec{
					ClusterName:        "lab-cluster",
					CPU:                4,
					RAM:                4096,
					Disk:               20480,
					MAC:                "c6:e5:50:1c:ec:01",
					RetainDiskOnDelete: true,
				},
			},
		},
	}
}

// TestHypervisorMachineTemplateGroupVersionKind verifies that registering the
// type with a scheme resolves to the infrastructure.cluster.x-k8s.io/v1alpha1
// group version and the HypervisorMachineTemplate kind (spec REQ-002).
func TestHypervisorMachineTemplateGroupVersionKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvks, _, err := scheme.ObjectKinds(&v1alpha1.HypervisorMachineTemplate{})
	if err != nil {
		t.Fatalf("ObjectKinds: %v", err)
	}

	want := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "HypervisorMachineTemplate",
	}
	if len(gvks) != 1 {
		t.Fatalf("ObjectKinds returned %d kinds, want 1: %v", len(gvks), gvks)
	}
	if got := gvks[0]; got != want {
		t.Errorf("GroupVersionKind = %s, want %s", got, want)
	}
}

// TestHypervisorMachineTemplateGroupVersionMetadata pins the type-level group
// metadata declared by the package: GroupVersion resolves to the
// infrastructure.cluster.x-k8s.io/v1alpha1 group and WithKind composes the
// HypervisorMachineTemplate GroupVersionKind (spec REQ-002). Unlike the scheme
// registration test it needs no object: it locks the package constants every
// object in the group shares.
func TestHypervisorMachineTemplateGroupVersionMetadata(t *testing.T) {
	wantGroupVersion := schema.GroupVersion{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
	}
	if got := v1alpha1.GroupVersion; got != wantGroupVersion {
		t.Errorf("GroupVersion = %s, want %s", got, wantGroupVersion)
	}

	wantGVK := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "HypervisorMachineTemplate",
	}
	if got := v1alpha1.GroupVersion.WithKind("HypervisorMachineTemplate"); got != wantGVK {
		t.Errorf("GroupVersion.WithKind = %s, want %s", got, wantGVK)
	}
}

// TestHypervisorMachineTemplateJSONRoundTrip verifies that a fully populated
// HypervisorMachineTemplate survives a marshal/unmarshal cycle with all
// contract fields preserved: the template spec nests a full
// HypervisorMachineSpec under spec.template.spec (spec REQ-005).
func TestHypervisorMachineTemplateJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		give *v1alpha1.HypervisorMachineTemplate
	}{
		{name: "fully populated contract fields", give: newFullyPopulatedMachineTemplate()},
		{name: "zero value", give: &v1alpha1.HypervisorMachineTemplate{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.give)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var got v1alpha1.HypervisorMachineTemplate
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", raw, err)
			}

			if !apiequality.Semantic.DeepEqual(&got, tt.give) {
				t.Errorf("round trip mismatch:\nwant: %#v\ngot:  %#v", tt.give, &got)
			}
		})
	}
}

// TestHypervisorMachineTemplateJSONShape pins the serialized contract shape of
// the template: the machine spec fields nest under spec.template.spec and
// never appear at the spec root. A round trip through the same Go type cannot
// detect a wrong json tag (the error round-trips back into the same field), so
// the raw document is inspected directly (spec REQ-005).
func TestHypervisorMachineTemplateJSONShape(t *testing.T) {
	raw, err := json.Marshal(newFullyPopulatedMachineTemplate())
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("Unmarshal into document map: %v", err)
	}
	var spec map[string]json.RawMessage
	if err := json.Unmarshal(doc["spec"], &spec); err != nil {
		t.Fatalf("Unmarshal spec: %v", err)
	}

	// machineSpecFields are the JSON field names of HypervisorMachineSpec.
	machineSpecFields := []string{"clusterName", "cpu", "ram", "disk", "mac", "retainDiskOnDelete"}

	t.Run("machine spec fields nest under spec.template.spec", func(t *testing.T) {
		var template map[string]json.RawMessage
		if err := json.Unmarshal(spec["template"], &template); err != nil {
			t.Fatalf("Unmarshal spec.template: %v", err)
		}
		var templateSpec map[string]json.RawMessage
		if err := json.Unmarshal(template["spec"], &templateSpec); err != nil {
			t.Fatalf("Unmarshal spec.template.spec: %v", err)
		}

		for _, field := range machineSpecFields {
			if _, ok := templateSpec[field]; !ok {
				t.Errorf("spec.template.spec.%s missing in %s", field, raw)
			}
		}
	})

	t.Run("machine spec fields do not leak to the spec root", func(t *testing.T) {
		for _, field := range machineSpecFields {
			if _, ok := spec[field]; ok {
				t.Errorf("spec.%s present at template spec root, want only spec.template: %s", field, raw)
			}
		}
	})
}

// TestHypervisorMachineTemplateDeepCopyNonAliasing verifies that
// DeepCopyObject returns a fully independent object: mutating the copy's
// template spec or its metadata reference types must not touch the original.
// The template spec holds scalars only, so the ObjectMeta maps are the only
// surface where a shallow deepcopy could alias (spec REQ-002 deepcopy).
func TestHypervisorMachineTemplateDeepCopyNonAliasing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.HypervisorMachineTemplate)
	}{
		{
			name: "spec.template.spec.clusterName",
			mutate: func(m *v1alpha1.HypervisorMachineTemplate) {
				m.Spec.Template.Spec.ClusterName = "other-cluster"
			},
		},
		{
			name: "spec.template.spec sibling fields",
			mutate: func(m *v1alpha1.HypervisorMachineTemplate) {
				m.Spec.Template.Spec.CPU = 8
				m.Spec.Template.Spec.RAM = 8192
				m.Spec.Template.Spec.Disk = 40960
				m.Spec.Template.Spec.MAC = "c6:e5:50:1c:ec:ff"
				m.Spec.Template.Spec.RetainDiskOnDelete = false
			},
		},
		{
			name: "objectMeta.labels element mutation",
			mutate: func(m *v1alpha1.HypervisorMachineTemplate) {
				m.ObjectMeta.Labels["cluster.x-k8s.io/cluster-name"] = "other-cluster"
			},
		},
		{
			name: "objectMeta.annotations element mutation",
			mutate: func(m *v1alpha1.HypervisorMachineTemplate) {
				m.ObjectMeta.Annotations["k8slabs.io/owner"] = "mutated"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := newFullyPopulatedMachineTemplate()
			obj := original.DeepCopyObject()

			copy, ok := obj.(*v1alpha1.HypervisorMachineTemplate)
			if !ok {
				t.Fatalf("DeepCopyObject returned %T, want *v1alpha1.HypervisorMachineTemplate", obj)
			}
			if copy == original {
				t.Fatal("DeepCopyObject returned the original pointer")
			}
			if !reflect.DeepEqual(copy, original) {
				t.Fatalf("DeepCopyObject did not preserve the value:\ncopy:     %#v\noriginal: %#v", copy, original)
			}

			// want is built from literals, so it is independent of the
			// DeepCopyObject implementation under test.
			want := newFullyPopulatedMachineTemplate()
			tt.mutate(copy)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("mutating the copy changed the original:\nwant: %#v\ngot:  %#v", want, original)
			}
		})
	}
}
