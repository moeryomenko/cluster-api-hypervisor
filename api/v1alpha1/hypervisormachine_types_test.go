// Contract tests for the HypervisorMachine infrastructure API type.
//
// Source of truth: spec REQ-002 (API groups, kinds, versions) and REQ-004
// (HypervisorMachine — InfrastructureMachine contract); plan task 05.
// This file is the red phase: it fails to compile until api/v1alpha1 provides
// HypervisorMachine and registers it in the scheme.

package v1alpha1_test

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
	"time"

	apiequality "k8s.io/apimachinery/pkg/api/equality"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	"github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// machineFixedTransitionTime is a fixed, UTC, monotonic-free timestamp. A
// value from time.Now() carries a monotonic clock reading that json
// round-trips to a different metav1.Time, which would break reflect.DeepEqual
// comparisons. The machine prefix avoids colliding with the package-level
// fixedTransitionTime declared by the HypervisorCluster contract tests.
var machineFixedTransitionTime = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// machineContractConditions returns the conditions carried by the populated
// fixture: the VMProvisioned condition plus the deprecated v1beta1
// BootstrapReady condition with a failing state.
func machineContractConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               "VMProvisioned",
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: machineFixedTransitionTime},
			Reason:             "VMIsRunning",
			Message:            "cloud-hypervisor process is running",
		},
		{
			Type:               string(clusterv1.BootstrapReadyV1Beta1Condition),
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Time{Time: machineFixedTransitionTime},
			Reason:             "BootstrapNotReady",
			Message:            "bootstrap secret not yet available",
		},
	}
}

// newFullyPopulatedMachine builds a HypervisorMachine with every contract
// field set. It is a maximally populated round-trip fixture, not a coherent
// reconciled state.
func newFullyPopulatedMachine() *v1alpha1.HypervisorMachine {
	providerID := "hypervisor://lab-cluster/cp-1"

	return &v1alpha1.HypervisorMachine{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
			Kind:       "HypervisorMachine",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "cp-1",
			Namespace:   "lab",
			Labels:      map[string]string{"cluster.x-k8s.io/cluster-name": "lab-cluster"},
			Annotations: map[string]string{"k8slabs.io/owner": "contract-tests"},
		},
		Spec: v1alpha1.HypervisorMachineSpec{
			ClusterName:        "lab-cluster",
			CPU:                4,
			RAM:                4096,
			Disk:               20480,
			MAC:                "c6:e5:50:1c:ec:01",
			RetainDiskOnDelete: true,
		},
		Status: v1alpha1.HypervisorMachineStatus{
			Ready: true,
			Addresses: []clusterv1.MachineAddress{
				{
					Type:    clusterv1.MachineInternalIP,
					Address: "192.168.124.10",
				},
				{
					Type:    clusterv1.MachineHostName,
					Address: "cp-1",
				},
			},
			ProviderID:     &providerID,
			FailureReason:  "ProvisioningFailed",
			FailureMessage: "cloud-hypervisor failed to start (contract fixture)",
			Conditions:     machineContractConditions(),
		},
	}
}

// TestHypervisorMachineGroupVersionKind verifies that registering the type
// with a scheme resolves to the infrastructure.cluster.x-k8s.io/v1alpha1
// group version and the HypervisorMachine kind (spec REQ-002).
func TestHypervisorMachineGroupVersionKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvks, _, err := scheme.ObjectKinds(&v1alpha1.HypervisorMachine{})
	if err != nil {
		t.Fatalf("ObjectKinds: %v", err)
	}

	want := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "HypervisorMachine",
	}

	if len(gvks) != 1 {
		t.Fatalf("ObjectKinds returned %d kinds, want 1: %v", len(gvks), gvks)
	}

	if got := gvks[0]; got != want {
		t.Errorf("GroupVersionKind = %s, want %s", got, want)
	}
}

// TestHypervisorMachineJSONRoundTrip verifies that a fully populated
// HypervisorMachine survives a marshal/unmarshal cycle with all contract
// fields preserved (spec REQ-004), including a nil vs non-nil ProviderID
// pointer and conditions carrying LastTransitionTime.
func TestHypervisorMachineJSONRoundTrip(t *testing.T) {
	emptyProviderID := ""

	tests := []struct {
		name string
		give *v1alpha1.HypervisorMachine
	}{
		{name: "fully populated contract fields", give: newFullyPopulatedMachine()},
		{name: "zero value", give: &v1alpha1.HypervisorMachine{}},
		{
			name: "providerID pointer to empty string is preserved",
			give: func() *v1alpha1.HypervisorMachine {
				m := newFullyPopulatedMachine()
				m.Status.ProviderID = &emptyProviderID

				return m
			}(),
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.give)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var got v1alpha1.HypervisorMachine
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

// TestHypervisorMachineJSONFieldPresence pins the omitempty contract of the
// optional spec fields: MAC and RetainDiskOnDelete are absent from the
// serialized form at their defaults and present once set (spec REQ-004).
func TestHypervisorMachineJSONFieldPresence(t *testing.T) {
	tests := []struct {
		name       string
		give       *v1alpha1.HypervisorMachine
		wantMAC    bool
		wantRetain bool
	}{
		{
			name:       "defaults are omitted",
			give:       &v1alpha1.HypervisorMachine{},
			wantMAC:    false,
			wantRetain: false,
		},
		{
			name: "overrides are serialized",
			give: func() *v1alpha1.HypervisorMachine {
				m := newFullyPopulatedMachine()
				m.Spec.MAC = "c6:e5:50:1c:ec:ff"
				m.Spec.RetainDiskOnDelete = true

				return m
			}(),
			wantMAC:    true,
			wantRetain: true,
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

			_, gotMAC := spec["mac"]
			if gotMAC != tt.wantMAC {
				t.Errorf("spec.mac present = %t, want %t (json: %s)", gotMAC, tt.wantMAC, raw)
			}

			_, gotRetain := spec["retainDiskOnDelete"]
			if gotRetain != tt.wantRetain {
				t.Errorf("spec.retainDiskOnDelete present = %t, want %t (json: %s)", gotRetain, tt.wantRetain, raw)
			}
		})
	}
}

// TestHypervisorMachineDeepCopyNonAliasing verifies that DeepCopyObject
// returns a fully independent object: mutating the copy's addresses slice,
// conditions slice, or provider ID pointer must not touch the original.
func TestHypervisorMachineDeepCopyNonAliasing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.HypervisorMachine)
	}{
		{
			name: "status.addresses append",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.Addresses = append(m.Status.Addresses, clusterv1.MachineAddress{
					Type:    clusterv1.MachineExternalIP,
					Address: "10.0.0.10",
				})
			},
		},
		{
			name: "status.addresses element mutation",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.Addresses[0].Address = "10.0.0.10"
			},
		},
		{
			name: "status.conditions append",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.Conditions = append(m.Status.Conditions, metav1.Condition{
					Type:               "DiskProvisioned",
					Status:             metav1.ConditionUnknown,
					LastTransitionTime: metav1.Time{Time: machineFixedTransitionTime},
					Reason:             "Unknown",
					Message:            "appended condition",
				})
			},
		},
		{
			name: "status.conditions element mutation",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				m.Status.Conditions[0].Message = "mutated"
			},
		},
		{
			name: "status.providerID pointee mutation",
			mutate: func(m *v1alpha1.HypervisorMachine) {
				*m.Status.ProviderID = "hypervisor://other-cluster/other-machine"
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := newFullyPopulatedMachine()
			obj := original.DeepCopyObject()

			copy, ok := obj.(*v1alpha1.HypervisorMachine)
			if !ok {
				t.Fatalf("DeepCopyObject returned %T, want *v1alpha1.HypervisorMachine", obj)
			}

			if copy == original {
				t.Fatal("DeepCopyObject returned the original pointer")
			}

			if !reflect.DeepEqual(copy, original) {
				t.Fatalf("DeepCopyObject did not preserve the value:\ncopy:     %#v\noriginal: %#v", copy, original)
			}

			// want is built from literals, so it is independent of the
			// DeepCopyObject implementation under test.
			want := newFullyPopulatedMachine()

			tt.mutate(copy)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("mutating the copy changed the original:\nwant: %#v\ngot:  %#v", want, original)
			}
		})
	}
}

// TestHypervisorMachineConditionsContract verifies the GetConditions and
// SetConditions methods round-trip a []metav1.Condition list, satisfying
// the conditions accessor contract used by the CAPI conditions package.
func TestHypervisorMachineConditionsContract(t *testing.T) {
	t.Run("unset conditions return nil", func(t *testing.T) {
		machine := &v1alpha1.HypervisorMachine{}
		if got := machine.GetConditions(); got != nil {
			t.Errorf("GetConditions() on zero object = %v, want nil", got)
		}
	})

	conditions := machineContractConditions()

	tests := []struct {
		name string
		give []metav1.Condition
	}{
		{name: "single condition", give: conditions[:1]},
		{name: "multiple conditions preserve order", give: conditions},
		{name: "nil conditions", give: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			machine := &v1alpha1.HypervisorMachine{}
			machine.SetConditions(tt.give)

			got := machine.GetConditions()
			if !reflect.DeepEqual(got, tt.give) {
				t.Errorf("conditions round trip mismatch:\nwant: %#v\ngot:  %#v", tt.give, got)
			}
		})
	}
}

// TestHypervisorMachineProviderIDFormat pins the providerID format convention
// hypervisor://<cluster>/<machine> (spec REQ-004). The controller sets the
// value, so the test asserts the scheme prefix only, not the full value.
func TestHypervisorMachineProviderIDFormat(t *testing.T) {
	tests := []struct {
		name       string
		providerID string
		wantPrefix bool
	}{
		{
			name:       "canonical format carries the hypervisor scheme",
			providerID: "hypervisor://lab-cluster/cp-1",
			wantPrefix: true,
		},
		{
			name:       "empty providerID is not accepted",
			providerID: "",
			wantPrefix: false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := strings.HasPrefix(tt.providerID, "hypervisor://"); got != tt.wantPrefix {
				t.Errorf("HasPrefix(%q, \"hypervisor://\") = %t, want %t", tt.providerID, got, tt.wantPrefix)
			}
		})
	}
}

// TestHypervisorMachineStatusInitializationContract pins the CAPI v1beta2
// InfrastructureMachine contract field: HypervisorMachineStatus exposes
// Initialization *InitializationStatus with a Provisioned bool, serialized
// under status.initialization.provisioned. The cluster-api v1beta2 machine
// controller gates InfrastructureReady on exactly that path
// (contract.InfrastructureMachine().Provisioned reads
// status.initialization.provisioned), so a running VM that only reports
// status.ready=true leaves the CAPI Machine waiting forever on
// "status.initialization.provisioned is false".
//
// Red phase: this test fails to compile until api/v1alpha1 provides
// InitializationStatus and wires it into HypervisorMachineStatus.
func TestHypervisorMachineStatusInitializationContract(t *testing.T) {
	t.Run("provisioned serializes under status.initialization.provisioned", func(t *testing.T) {
		status := v1alpha1.HypervisorMachineStatus{
			Ready:          true,
			Initialization: &v1alpha1.InitializationStatus{Provisioned: true},
		}

		raw, err := json.Marshal(status)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal into document map: %v", err)
		}

		var init map[string]json.RawMessage
		if err := json.Unmarshal(doc["initialization"], &init); err != nil {
			t.Fatalf("Unmarshal initialization: %v", err)
		}

		var provisioned bool
		if err := json.Unmarshal(init["provisioned"], &provisioned); err != nil {
			t.Fatalf("Unmarshal provisioned: %v", err)
		}

		if !provisioned {
			t.Errorf("status.initialization.provisioned = %t, want true (json: %s)", provisioned, raw)
		}
	})

	t.Run("unset initialization is omitted", func(t *testing.T) {
		raw, err := json.Marshal(v1alpha1.HypervisorMachineStatus{})
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var doc map[string]json.RawMessage
		if err := json.Unmarshal(raw, &doc); err != nil {
			t.Fatalf("Unmarshal into document map: %v", err)
		}

		if _, ok := doc["initialization"]; ok {
			t.Errorf("zero value serialized initialization: %s", raw)
		}
	})

	t.Run("round trip preserves provisioned", func(t *testing.T) {
		m := newFullyPopulatedMachine()
		m.Status.Initialization = &v1alpha1.InitializationStatus{Provisioned: true}

		raw, err := json.Marshal(m)
		if err != nil {
			t.Fatalf("Marshal: %v", err)
		}

		var got v1alpha1.HypervisorMachine
		if err := json.Unmarshal(raw, &got); err != nil {
			t.Fatalf("Unmarshal(%s): %v", raw, err)
		}

		if got.Status.Initialization == nil || !got.Status.Initialization.Provisioned {
			t.Errorf("round trip lost status.initialization.provisioned (json: %s)", raw)
		}
	})
}
