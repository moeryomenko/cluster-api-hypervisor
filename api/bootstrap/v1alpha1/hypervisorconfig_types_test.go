// Contract tests for the HypervisorConfig bootstrap API type.
//
// Source of truth: spec REQ-002 (API groups, kinds, versions) and REQ-007
// (HypervisorConfig — BootstrapConfig contract); plan task 09 (revision).
// HypervisorConfig belongs to the bootstrap.cluster.x-k8s.io group and lives
// in its own package api/bootstrap/v1alpha1, separate from the infrastructure
// kinds in api/v1alpha1 (controller-gen assigns one +groupName per package,
// so the bootstrap group needs its own package).
// This file is the red phase: it fails to compile until api/bootstrap/v1alpha1
// provides HypervisorConfig and registers it via AddToScheme.

package bootstrapv1alpha1_test

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
)

// configFixedTransitionTime is a fixed, UTC, monotonic-free timestamp. A value
// from time.Now() carries a monotonic clock reading that json round-trips to a
// different metav1.Time, which would break reflect.DeepEqual comparisons. The
// config prefix avoids colliding with the package-level timestamps declared by
// the other contract tests.
var configFixedTransitionTime = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// configContractConditions returns the conditions carried by the populated
// fixture: the DataSecretAvailable condition that REQ-007 assigns to the
// bootstrap contract (spec REQ-007).
func configContractConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               "DataSecretAvailable",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: configFixedTransitionTime},
			Reason:             "BootstrapSecretRendered",
			Message:            "bootstrap secret data is available",
		},
	}
}

// newFullyPopulatedConfig builds a HypervisorConfig with every contract field
// set: all four spec fields, the dataSecretName status pointer, and one
// condition. It is a maximally populated round-trip fixture, not a coherent
// reconciled state.
func newFullyPopulatedConfig() *bootstrapv1alpha1.HypervisorConfig {
	dataSecretName := "lab-cluster-cp-1-bootstrap"
	return &bootstrapv1alpha1.HypervisorConfig{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "bootstrap.cluster.x-k8s.io/v1alpha1",
			Kind:       "HypervisorConfig",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "lab-cluster-cp-1",
			Namespace:   "lab",
			Labels:      map[string]string{"cluster.x-k8s.io/cluster-name": "lab-cluster"},
			Annotations: map[string]string{"k8slabs.io/owner": "contract-tests"},
		},
		Spec: bootstrapv1alpha1.HypervisorConfigSpec{
			ClusterName:  "lab-cluster",
			Role:         "control-plane",
			NodeName:     "cp-1",
			SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample contract-fixture",
		},
		Status: bootstrapv1alpha1.HypervisorConfigStatus{
			Ready:          true,
			DataSecretName: &dataSecretName,
			FailureReason:  "BootstrapFailed",
			FailureMessage: "rendering confext trees failed (contract fixture)",
			Conditions:     configContractConditions(),
		},
	}
}

// TestHypervisorConfigGroupVersionKind verifies that registering the type
// with a scheme resolves to the bootstrap.cluster.x-k8s.io/v1alpha1 group
// version and the HypervisorConfig kind (spec REQ-002). The scheme is built
// from the bootstrap package's own AddToScheme, which must not depend on the
// infrastructure package.
func TestHypervisorConfigGroupVersionKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := bootstrapv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvks, _, err := scheme.ObjectKinds(&bootstrapv1alpha1.HypervisorConfig{})
	if err != nil {
		t.Fatalf("ObjectKinds: %v", err)
	}

	want := schema.GroupVersionKind{
		Group:   "bootstrap.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "HypervisorConfig",
	}
	if len(gvks) != 1 {
		t.Fatalf("ObjectKinds returned %d kinds, want 1: %v", len(gvks), gvks)
	}
	if got := gvks[0]; got != want {
		t.Errorf("GroupVersionKind = %s, want %s", got, want)
	}
}

// TestHypervisorConfigJSONRoundTrip verifies that a fully populated
// HypervisorConfig survives a marshal/unmarshal cycle with all contract fields
// preserved (spec REQ-007), including a nil vs non-nil DataSecretName pointer
// and conditions carrying LastTransitionTime.
func TestHypervisorConfigJSONRoundTrip(t *testing.T) {
	emptyDataSecretName := ""
	tests := []struct {
		name string
		give *bootstrapv1alpha1.HypervisorConfig
	}{
		{name: "fully populated contract fields", give: newFullyPopulatedConfig()},
		{name: "zero value", give: &bootstrapv1alpha1.HypervisorConfig{}},
		{
			name: "dataSecretName pointer to empty string is preserved",
			give: func() *bootstrapv1alpha1.HypervisorConfig {
				c := newFullyPopulatedConfig()
				c.Status.DataSecretName = &emptyDataSecretName
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

			var got bootstrapv1alpha1.HypervisorConfig
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

// TestHypervisorConfigJSONOmitEmptyShape pins the omitempty contract of the
// optional fields: at the zero value the spec omits role, nodeName and
// sshPublicKey while the required clusterName key stays present, and the
// status omits the optional dataSecretName, failureReason, failureMessage and
// conditions keys. A round trip through the same Go type cannot detect a wrong
// json tag (the error round-trips back into the same field), so the raw
// document is inspected directly (spec REQ-007).
func TestHypervisorConfigJSONOmitEmptyShape(t *testing.T) {
	tests := []struct {
		name     string
		give     *bootstrapv1alpha1.HypervisorConfig
		wantRole bool
		wantNode bool
		wantSSH  bool
	}{
		{
			name: "zero value omits optional spec fields",
			give: &bootstrapv1alpha1.HypervisorConfig{},
		},
		{
			name:     "set optional spec fields are serialized",
			give:     newFullyPopulatedConfig(),
			wantRole: true,
			wantNode: true,
			wantSSH:  true,
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

			_, gotRole := spec["role"]
			if gotRole != tt.wantRole {
				t.Errorf("spec.role present = %t, want %t (json: %s)", gotRole, tt.wantRole, raw)
			}
			_, gotNode := spec["nodeName"]
			if gotNode != tt.wantNode {
				t.Errorf("spec.nodeName present = %t, want %t (json: %s)", gotNode, tt.wantNode, raw)
			}
			_, gotSSH := spec["sshPublicKey"]
			if gotSSH != tt.wantSSH {
				t.Errorf("spec.sshPublicKey present = %t, want %t (json: %s)", gotSSH, tt.wantSSH, raw)
			}

			// clusterName is required (spec REQ-007): the key must be present
			// even at the zero value, so the tag cannot carry omitempty.
			if _, ok := spec["clusterName"]; !ok {
				t.Errorf("spec.clusterName missing, want present as required field (json: %s)", raw)
			}
		})
	}

	t.Run("zero value omits optional status keys", func(t *testing.T) {
		raw, err := json.Marshal(&bootstrapv1alpha1.HypervisorConfig{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal into document map: %v", err)
		}
		statusRaw, ok := doc["status"]
		if !ok {
			// The whole status block is omitted; every optional key is
			// necessarily absent.
			return
		}
		var status map[string]json.RawMessage
		if err := json.Unmarshal(statusRaw, &status); err != nil {
			t.Fatalf("Unmarshal status: %v", err)
		}
		for _, key := range []string{"dataSecretName", "failureReason", "failureMessage", "conditions"} {
			if _, present := status[key]; present {
				t.Errorf("status.%s present at zero value, want omitted (json: %s)", key, raw)
			}
		}
	})
}

// TestHypervisorConfigDeepCopyNonAliasing verifies that DeepCopyObject
// returns a fully independent object: mutating the copy's DataSecretName
// pointer or conditions slice must not touch the original.
func TestHypervisorConfigDeepCopyNonAliasing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*bootstrapv1alpha1.HypervisorConfig)
	}{
		{
			name: "status.dataSecretName pointee mutation",
			mutate: func(c *bootstrapv1alpha1.HypervisorConfig) {
				*c.Status.DataSecretName = "other-cluster-cp-2-bootstrap"
			},
		},
		{
			name: "status.conditions append",
			mutate: func(c *bootstrapv1alpha1.HypervisorConfig) {
				c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
					Type:               "RenderedFilesValidated",
					Status:             metav1.ConditionUnknown,
					LastTransitionTime: metav1.Time{Time: configFixedTransitionTime},
					Reason:             "Unknown",
					Message:            "appended condition",
				})
			},
		},
		{
			name: "status.conditions element mutation",
			mutate: func(c *bootstrapv1alpha1.HypervisorConfig) {
				c.Status.Conditions[0].Message = "mutated"
			},
		},
		{
			name: "spec sibling fields",
			mutate: func(c *bootstrapv1alpha1.HypervisorConfig) {
				c.Spec.Role = "worker"
				c.Spec.NodeName = "worker-2"
				c.Spec.SSHPublicKey = "ssh-ed25519 other-fixture"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := newFullyPopulatedConfig()
			obj := original.DeepCopyObject()

			copy, ok := obj.(*bootstrapv1alpha1.HypervisorConfig)
			if !ok {
				t.Fatalf("DeepCopyObject returned %T, want *bootstrapv1alpha1.HypervisorConfig", obj)
			}
			if copy == original {
				t.Fatal("DeepCopyObject returned the original pointer")
			}
			if !reflect.DeepEqual(copy, original) {
				t.Fatalf("DeepCopyObject did not preserve the value:\ncopy:     %#v\noriginal: %#v", copy, original)
			}

			// want is built from literals, so it is independent of the
			// DeepCopyObject implementation under test.
			want := newFullyPopulatedConfig()
			tt.mutate(copy)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("mutating the copy changed the original:\nwant: %#v\ngot:  %#v", want, original)
			}
		})
	}
}

// TestHypervisorConfigConditionsContract verifies the GetConditions and
// SetConditions methods round-trip a []metav1.Condition list, satisfying
// the conditions accessor contract used by the CAPI conditions package.
func TestHypervisorConfigConditionsContract(t *testing.T) {
	t.Run("unset conditions return nil", func(t *testing.T) {
		config := &bootstrapv1alpha1.HypervisorConfig{}
		if got := config.GetConditions(); got != nil {
			t.Errorf("GetConditions() on zero object = %v, want nil", got)
		}
	})

	conditions := configContractConditions()
	second := metav1.Condition{
		Type:               "RenderedFilesValidated",
		Status:             metav1.ConditionFalse,
		LastTransitionTime: metav1.Time{Time: configFixedTransitionTime},
		Reason:             "ValidationFailed",
		Message:            "tree files failed validation",
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
			config := &bootstrapv1alpha1.HypervisorConfig{}
			config.SetConditions(tt.give)

			got := config.GetConditions()
			if !reflect.DeepEqual(got, tt.give) {
				t.Errorf("conditions round trip mismatch:\nwant: %#v\ngot:  %#v", tt.give, got)
			}
		})
	}
}
