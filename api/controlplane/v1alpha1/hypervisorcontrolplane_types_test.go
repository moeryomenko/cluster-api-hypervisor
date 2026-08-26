// Contract tests for the HypervisorControlPlane control-plane API type.
//
// Source of truth: spec REQ-002 (API groups, kinds, versions) and REQ-010
// (HypervisorControlPlane — ControlPlane contract); plan task 11 (test-first).
// HypervisorControlPlane belongs to the controlplane.cluster.x-k8s.io group and
// lives in its own package api/controlplane/v1alpha1, separate from the
// infrastructure kinds in api/v1alpha1 and the bootstrap kind in
// api/bootstrap/v1alpha1 (controller-gen assigns one +groupName per package, so
// the control-plane group needs its own package).
// This file is the red phase: it fails to compile until api/controlplane/v1alpha1
// provides HypervisorControlPlane and registers it via AddToScheme.
//
// machineTemplate restructure (v1beta2 contract parity): the machine template
// mirrors the CAPI v1beta2 KubeadmControlPlaneMachineTemplate shape
// (cluster-api@v1.13.5 kubeadm_control_plane_types.go lines 484-539) — the
// InfrastructureRef moves under a required Spec value struct and becomes a
// clusterv1.ContractVersionedObjectReference (apiGroup/kind/name, no
// namespace), while Metadata stays on the template. The topology controller
// rejects the old flat spec.machineTemplate.infrastructureRef path with
// "field not declared in schema", so these pins hold the nested wire shape in
// place for both the concrete kind and the ClusterClass template kind.

package controlplanev1alpha1_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
)

// cpFixedTransitionTime is a fixed, UTC, monotonic-free timestamp. A value
// from time.Now() carries a monotonic clock reading that json round-trips to a
// different metav1.Time, which would break reflect.DeepEqual comparisons. The
// cp prefix avoids colliding with the package-level timestamps declared by the
// other contract tests.
var cpFixedTransitionTime = time.Date(2026, 8, 13, 12, 0, 0, 0, time.UTC)

// cpContractConditions returns the conditions carried by the populated
// fixture: the ControlPlaneReady condition that REQ-010 assigns to the
// control-plane contract (spec REQ-010).
func cpContractConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               "ControlPlaneReady",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: cpFixedTransitionTime},
			Reason:             "APIServerHealthy",
			Message:            "apiserver is healthy",
		},
	}
}

// newFullyPopulatedControlPlane builds a HypervisorControlPlane with every
// contract field set: replicas, version, the machine template's nested
// spec.infrastructureRef and metadata, every status counter, the version
// pointer, and one condition. It is a maximally populated round-trip fixture,
// not a coherent reconciled state.
func newFullyPopulatedControlPlane() *controlplanev1alpha1.HypervisorControlPlane {
	version := "v1.32.4"
	return &controlplanev1alpha1.HypervisorControlPlane{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "controlplane.cluster.x-k8s.io/v1alpha1",
			Kind:       "HypervisorControlPlane",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "lab-cluster-cp",
			Namespace:   "lab",
			Labels:      map[string]string{"cluster.x-k8s.io/cluster-name": "lab-cluster"},
			Annotations: map[string]string{"k8slabs.io/owner": "contract-tests"},
		},
		Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
			Replicas: 1,
			Version:  "v1.32.4",
			MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
				Metadata: clusterv1.ObjectMeta{
					Labels:      map[string]string{"cluster.x-k8s.io/cluster-name": "lab-cluster"},
					Annotations: map[string]string{"k8slabs.io/owner": "contract-tests"},
				},
				Spec: controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: "infrastructure.cluster.x-k8s.io",
						Kind:     "HypervisorMachineTemplate",
						Name:     "lab-cluster-cp-template",
					},
				},
			},
		},
		Status: controlplanev1alpha1.HypervisorControlPlaneStatus{
			Ready:               true,
			Initialized:         true,
			Selector:            "cluster.x-k8s.io/control-plane-name=lab-cluster-cp",
			Version:             &version,
			Replicas:            1,
			UpdatedReplicas:     1,
			ReadyReplicas:       1,
			UnavailableReplicas: 1,
			FailureReason:       "ControlPlaneNotReady",
			FailureMessage:      "apiserver healthz not reached (contract fixture)",
			Conditions:          cpContractConditions(),
		},
	}
}

// TestHypervisorControlPlaneGroupVersionKind verifies that registering the
// type with a scheme resolves to the controlplane.cluster.x-k8s.io/v1alpha1
// group version and the HypervisorControlPlane kind (spec REQ-002). The scheme
// is built from the control-plane package's own AddToScheme, which must not
// depend on the infrastructure or bootstrap packages.
func TestHypervisorControlPlaneGroupVersionKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := controlplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvks, _, err := scheme.ObjectKinds(&controlplanev1alpha1.HypervisorControlPlane{})
	if err != nil {
		t.Fatalf("ObjectKinds: %v", err)
	}

	want := schema.GroupVersionKind{
		Group:   "controlplane.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "HypervisorControlPlane",
	}
	if len(gvks) != 1 {
		t.Fatalf("ObjectKinds returned %d kinds, want 1: %v", len(gvks), gvks)
	}
	if got := gvks[0]; got != want {
		t.Errorf("GroupVersionKind = %s, want %s", got, want)
	}
}

// TestHypervisorControlPlaneJSONRoundTrip verifies that a fully populated
// HypervisorControlPlane survives a marshal/unmarshal cycle with all contract
// fields preserved (spec REQ-010), including a nil vs non-nil Version pointer,
// the machine template's infrastructureRef and metadata, and conditions
// carrying LastTransitionTime.
func TestHypervisorControlPlaneJSONRoundTrip(t *testing.T) {
	emptyVersion := ""
	tests := []struct {
		name string
		give *controlplanev1alpha1.HypervisorControlPlane
	}{
		{name: "fully populated contract fields", give: newFullyPopulatedControlPlane()},
		{name: "zero value", give: &controlplanev1alpha1.HypervisorControlPlane{}},
		{
			name: "version pointer to empty string is preserved",
			give: func() *controlplanev1alpha1.HypervisorControlPlane {
				c := newFullyPopulatedControlPlane()
				c.Status.Version = &emptyVersion
				return c
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.give)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var got controlplanev1alpha1.HypervisorControlPlane
			if err := json.Unmarshal(raw, &got); err != nil {
				t.Fatalf("Unmarshal(%s): %v", raw, err)
			}

			// reflect.DeepEqual cannot be used here: metav1.Time.UnmarshalJSON
			// converts the parsed timestamp to time.Local, and DeepEqual on
			// time.Time compares the *Location pointers, so a UTC fixture is
			// never deeply equal to the unmarshaled value. apiequality
			// compares metav1.Time via t.UTC() == t.UTC(), which ignores the
			// Location and makes the round trip independent of the host TZ.
			if !apiequality.Semantic.DeepEqual(&got, tt.give) {
				t.Errorf("round trip mismatch:\nwant: %#v\ngot:  %#v", tt.give, &got)
			}
		})
	}
}

// TestHypervisorControlPlaneJSONOmitEmptyShape pins the omitempty contract of
// the optional spec fields: at the zero value the spec omits replicas and
// version while the required machineTemplate key stays present (as an empty
// object), and a populated spec serializes both keys. A round trip through the
// same Go type cannot detect a wrong json tag (the error round-trips back into
// the same field), so the raw document is inspected directly (spec REQ-010).
func TestHypervisorControlPlaneJSONOmitEmptyShape(t *testing.T) {
	tests := []struct {
		name         string
		give         *controlplanev1alpha1.HypervisorControlPlane
		wantReplicas bool
		wantVersion  bool
	}{
		{
			name: "zero value omits optional spec fields",
			give: &controlplanev1alpha1.HypervisorControlPlane{},
		},
		{
			name:         "set optional spec fields are serialized",
			give:         newFullyPopulatedControlPlane(),
			wantReplicas: true,
			wantVersion:  true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.give)
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

			_, gotReplicas := spec["replicas"]
			if gotReplicas != tt.wantReplicas {
				t.Errorf("spec.replicas present = %t, want %t (json: %s)", gotReplicas, tt.wantReplicas, raw)
			}
			_, gotVersion := spec["version"]
			if gotVersion != tt.wantVersion {
				t.Errorf("spec.version present = %t, want %t (json: %s)", gotVersion, tt.wantVersion, raw)
			}

			// machineTemplate is required (spec REQ-010): the key must be
			// present even at the zero value, so the tag cannot carry
			// omitempty.
			if _, ok := spec["machineTemplate"]; !ok {
				t.Errorf("spec.machineTemplate missing, want present as required field (json: %s)", raw)
			}
		})
	}
}

// TestHypervisorControlPlaneDeepCopyNonAliasing verifies that DeepCopyObject
// returns a fully independent object: mutating the copy's InfrastructureRef
// fields, the Metadata labels map, the Version pointer, or the conditions
// slice must not touch the original.
func TestHypervisorControlPlaneDeepCopyNonAliasing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*controlplanev1alpha1.HypervisorControlPlane)
	}{
		{
			name: "spec.machineTemplate.spec.infrastructureRef field mutation",
			mutate: func(c *controlplanev1alpha1.HypervisorControlPlane) {
				c.Spec.MachineTemplate.Spec.InfrastructureRef.Name = "other-cp-template"
			},
		},
		{
			name: "spec.machineTemplate.metadata labels map mutation",
			mutate: func(c *controlplanev1alpha1.HypervisorControlPlane) {
				c.Spec.MachineTemplate.Metadata.Labels["mutated"] = "yes"
			},
		},
		{
			name: "status.version pointee mutation",
			mutate: func(c *controlplanev1alpha1.HypervisorControlPlane) {
				*c.Status.Version = "v1.33.0"
			},
		},
		{
			name: "status.conditions append",
			mutate: func(c *controlplanev1alpha1.HypervisorControlPlane) {
				c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
					Type:               "MachinesCreated",
					Status:             metav1.ConditionUnknown,
					LastTransitionTime: metav1.Time{Time: cpFixedTransitionTime},
					Reason:             "Unknown",
					Message:            "appended condition",
				})
			},
		},
		{
			name: "status.conditions element mutation",
			mutate: func(c *controlplanev1alpha1.HypervisorControlPlane) {
				c.Status.Conditions[0].Message = "mutated"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := newFullyPopulatedControlPlane()
			obj := original.DeepCopyObject()

			copy, ok := obj.(*controlplanev1alpha1.HypervisorControlPlane)
			if !ok {
				t.Fatalf("DeepCopyObject returned %T, want *controlplanev1alpha1.HypervisorControlPlane", obj)
			}
			if copy == original {
				t.Fatal("DeepCopyObject returned the original pointer")
			}
			if !reflect.DeepEqual(copy, original) {
				t.Fatalf("DeepCopyObject did not preserve the value:\ncopy:     %#v\noriginal: %#v", copy, original)
			}

			// want is built from literals, so it is independent of the
			// DeepCopyObject implementation under test.
			want := newFullyPopulatedControlPlane()
			tt.mutate(copy)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("mutating the copy changed the original:\nwant: %#v\ngot:  %#v", want, original)
			}
		})
	}
}

// TestHypervisorControlPlaneConditionsContract verifies the GetConditions and
// SetConditions methods round-trip a []metav1.Condition list, satisfying
// the conditions accessor contract used by the CAPI conditions package.
func TestHypervisorControlPlaneConditionsContract(t *testing.T) {
	t.Run("unset conditions return nil", func(t *testing.T) {
		cp := &controlplanev1alpha1.HypervisorControlPlane{}
		if got := cp.GetConditions(); got != nil {
			t.Errorf("GetConditions() on zero object = %v, want nil", got)
		}
	})

	conditions := cpContractConditions()
	second := metav1.Condition{
		Type:               "MachinesCreated",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Time{Time: cpFixedTransitionTime},
		Reason:             "MachinesNotCreated",
		Message:            "no control-plane machines exist",
	}
	tests := []struct {
		name string
		give []metav1.Condition
	}{
		{name: "single condition", give: conditions[:1]},
		{name: "multiple conditions preserve order", give: []metav1.Condition{conditions[0], second}},
		{name: "nil conditions", give: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cp := &controlplanev1alpha1.HypervisorControlPlane{}
			cp.SetConditions(tt.give)

			got := cp.GetConditions()
			if !reflect.DeepEqual(got, tt.give) {
				t.Errorf("conditions round trip mismatch:\nwant: %#v\ngot:  %#v", tt.give, got)
			}
		})
	}
}

// TestHypervisorControlPlaneMachineTemplateShapePinned pins the Go shape of
// the machine template against the CAPI v1beta2 contract
// (cluster-api@v1.13.5 KubeadmControlPlaneMachineTemplate, lines 484-502):
// the InfrastructureRef lives under a required Spec value struct typed
// HypervisorControlPlaneMachineTemplateSpec and is a
// clusterv1.ContractVersionedObjectReference, the flat
// spec.machineTemplate.infrastructureRef path is gone, and Metadata stays on
// the template. The json tags are pinned too because a round trip through the
// same Go type cannot detect a wrong tag.
func TestHypervisorControlPlaneMachineTemplateShapePinned(t *testing.T) {
	machineTemplateType := reflect.TypeOf(controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{})
	specType := reflect.TypeOf(controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{})
	contractRefType := reflect.TypeOf(clusterv1.ContractVersionedObjectReference{})

	tests := []struct {
		name       string
		structType reflect.Type
		fieldName  string
		wantType   reflect.Type // nil pins the field as absent
		wantTag    string       // checked only when wantType is non-nil
	}{
		{
			name:       "machineTemplate has no flat infrastructureRef field",
			structType: machineTemplateType,
			fieldName:  "InfrastructureRef",
			wantType:   nil,
		},
		{
			name:       "machineTemplate carries Spec",
			structType: machineTemplateType,
			fieldName:  "Spec",
			wantType:   specType,
			wantTag:    "spec,omitempty,omitzero",
		},
		{
			name:       "machineTemplate keeps Metadata",
			structType: machineTemplateType,
			fieldName:  "Metadata",
			wantType:   reflect.TypeOf(clusterv1.ObjectMeta{}),
			wantTag:    "metadata,omitempty,omitzero",
		},
		{
			name:       "spec carries InfrastructureRef as a contract-versioned reference",
			structType: specType,
			fieldName:  "InfrastructureRef",
			wantType:   contractRefType,
			wantTag:    "infrastructureRef,omitempty,omitzero",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			structField, ok := tt.structType.FieldByName(tt.fieldName)
			if tt.wantType == nil {
				if ok {
					t.Fatalf("%s unexpectedly has field %s; the v1beta2 contract nests it under Spec", tt.structType, tt.fieldName)
				}
				return
			}
			if !ok {
				t.Fatalf("%s has no field %s", tt.structType, tt.fieldName)
			}
			if structField.Type != tt.wantType {
				t.Errorf("%s.%s type = %s, want %s", tt.structType, tt.fieldName, structField.Type, tt.wantType)
			}
			if got := structField.Tag.Get("json"); got != tt.wantTag {
				t.Errorf("%s.%s json tag = %q, want %q", tt.structType, tt.fieldName, got, tt.wantTag)
			}
		})
	}

	t.Run("spec.infrastructureRef carries no namespace field", func(t *testing.T) {
		if _, ok := contractRefType.FieldByName("Namespace"); ok {
			t.Error("ContractVersionedObjectReference has a Namespace field; the v1beta2 contract resolves references in the owning object's namespace")
		}
	})

	t.Run("spec.machineTemplate stays a required key", func(t *testing.T) {
		specField, ok := reflect.TypeOf(controlplanev1alpha1.HypervisorControlPlaneSpec{}).FieldByName("MachineTemplate")
		if !ok {
			t.Fatal("HypervisorControlPlaneSpec has no MachineTemplate field")
		}
		if got, want := specField.Tag.Get("json"), "machineTemplate"; got != want {
			t.Errorf("spec.machineTemplate json tag = %q, want %q (required, no omitempty)", got, want)
		}
	})
}

// TestHypervisorControlPlaneMachineTemplateWireShape pins the serialized
// document: at the zero value the required machineTemplate key serializes as
// an empty object — no infrastructureRef at the old flat path and no spec key
// (the omitzero value structs drop) — and a populated reference serializes
// under machineTemplate.spec.infrastructureRef with the contract-versioned
// apiGroup/kind/name keys, never corev1's apiVersion or a namespace.
func TestHypervisorControlPlaneMachineTemplateWireShape(t *testing.T) {
	t.Run("zero-value machineTemplate serializes without the old infrastructureRef path", func(t *testing.T) {
		raw, err := json.Marshal(&controlplanev1alpha1.HypervisorControlPlane{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal into document map: %v", err)
		}
		specRaw, ok := doc["spec"]
		if !ok {
			t.Fatalf("spec missing from document (json: %s)", raw)
		}
		var spec map[string]json.RawMessage
		if err := json.Unmarshal(specRaw, &spec); err != nil {
			t.Fatalf("Unmarshal spec: %v", err)
		}
		mtRaw, ok := spec["machineTemplate"]
		if !ok {
			t.Fatalf("spec.machineTemplate missing, want the required key present (json: %s)", raw)
		}
		if string(mtRaw) != "{}" {
			t.Errorf("zero-value spec.machineTemplate = %s, want {} (metadata and spec dropped by omitzero, no infrastructureRef at the old path)", mtRaw)
		}

		var machineTemplateDoc map[string]json.RawMessage
		if err := json.Unmarshal(mtRaw, &machineTemplateDoc); err != nil {
			t.Fatalf("Unmarshal machineTemplate: %v", err)
		}
		if ref, ok := machineTemplateDoc["infrastructureRef"]; ok {
			t.Errorf("old flat path machineTemplate.infrastructureRef serialized as %s, want absent", ref)
		}
	})

	t.Run("populated reference serializes under machineTemplate.spec with apiGroup", func(t *testing.T) {
		raw, err := json.Marshal(newFullyPopulatedControlPlane())
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc struct {
			Spec struct {
				MachineTemplate struct {
					InfrastructureRef json.RawMessage `json:"infrastructureRef"`
					Spec              struct {
						InfrastructureRef map[string]json.RawMessage `json:"infrastructureRef"`
					} `json:"spec"`
				} `json:"machineTemplate"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}

		if ref := doc.Spec.MachineTemplate.InfrastructureRef; len(ref) != 0 {
			t.Errorf("old flat path machineTemplate.infrastructureRef serialized as %s, want absent", ref)
		}

		ref := doc.Spec.MachineTemplate.Spec.InfrastructureRef
		if ref == nil {
			t.Fatalf("machineTemplate.spec.infrastructureRef missing (json: %s)", raw)
		}
		wantKeys := map[string]string{
			"apiGroup": "infrastructure.cluster.x-k8s.io",
			"kind":     "HypervisorMachineTemplate",
			"name":     "lab-cluster-cp-template",
		}
		for key, want := range wantKeys {
			var got string
			if err := json.Unmarshal(ref[key], &got); err != nil {
				t.Fatalf("machineTemplate.spec.infrastructureRef.%s: %v", key, err)
			}
			if got != want {
				t.Errorf("machineTemplate.spec.infrastructureRef.%s = %q, want %q", key, got, want)
			}
		}
		for _, banned := range []string{"apiVersion", "namespace"} {
			if val, ok := ref[banned]; ok {
				t.Errorf("machineTemplate.spec.infrastructureRef.%s = %s, want absent (contract-versioned references carry apiGroup/kind/name only)", banned, val)
			}
		}
	})
}

// TestHypervisorControlPlaneTemplateMachineTemplateParity pins that the
// ClusterClass template kind carries the same nested machine template shape:
// HypervisorControlPlaneTemplateSpec's inner template spec exposes the same
// MachineTemplate type as the concrete kind, so the topology controller can
// clone template.spec.template.spec into a HypervisorControlPlane without
// hitting the "field not declared in schema" rejection on either side.
func TestHypervisorControlPlaneTemplateMachineTemplateParity(t *testing.T) {
	tmpl := &controlplanev1alpha1.HypervisorControlPlaneTemplate{
		Spec: controlplanev1alpha1.HypervisorControlPlaneTemplateSpec{
			Template: controlplanev1alpha1.HypervisorControlPlaneResource{
				Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
					MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
						Metadata: clusterv1.ObjectMeta{Labels: map[string]string{"pool": "cp"}},
						Spec: controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{
							InfrastructureRef: clusterv1.ContractVersionedObjectReference{
								APIGroup: "infrastructure.cluster.x-k8s.io",
								Kind:     "HypervisorMachineTemplate",
								Name:     "lab-cp-machine-template",
							},
						},
					},
				},
			},
		},
	}

	t.Run("inner template reuses the concrete kind's machine template type", func(t *testing.T) {
		got := reflect.TypeOf(tmpl.Spec.Template.Spec.MachineTemplate)
		want := reflect.TypeOf(controlplanev1alpha1.HypervisorControlPlane{}.Spec.MachineTemplate)
		if got != want {
			t.Errorf("template.spec.template.spec.machineTemplate type = %s, want the concrete %s", got, want)
		}
	})

	t.Run("inner template serializes the nested wire path", func(t *testing.T) {
		raw, err := json.Marshal(tmpl)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc struct {
			Spec struct {
				Template struct {
					Spec struct {
						MachineTemplate struct {
							Spec struct {
								InfrastructureRef map[string]json.RawMessage `json:"infrastructureRef"`
							} `json:"spec"`
							InfrastructureRef json.RawMessage `json:"infrastructureRef"`
						} `json:"machineTemplate"`
					} `json:"spec"`
				} `json:"template"`
			} `json:"spec"`
		}
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal: %v", err)
		}

		if ref := doc.Spec.Template.Spec.MachineTemplate.InfrastructureRef; len(ref) != 0 {
			t.Errorf("template kind old flat path machineTemplate.infrastructureRef serialized as %s, want absent", ref)
		}
		ref := doc.Spec.Template.Spec.MachineTemplate.Spec.InfrastructureRef
		if ref == nil {
			t.Fatalf("template.spec.template.spec.machineTemplate.spec.infrastructureRef missing (json: %s)", raw)
		}
		var name string
		if err := json.Unmarshal(ref["name"], &name); err != nil || name != "lab-cp-machine-template" {
			t.Errorf("template kind infrastructureRef.name = %q (err %v), want lab-cp-machine-template", name, err)
		}
	})
}
