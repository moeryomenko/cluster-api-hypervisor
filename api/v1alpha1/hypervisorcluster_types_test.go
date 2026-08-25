// Contract tests for the HypervisorCluster infrastructure API type.
//
// Source of truth: spec REQ-002 (API groups, kinds, versions) and REQ-003
// (HypervisorCluster — InfrastructureCluster contract); plan task 03.
// This file is the red phase: it fails to compile until api/v1alpha1 provides
// HypervisorCluster and the scheme registration in groupversion_info.go.

package v1alpha1_test

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

	"github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// fixedTransitionTime is a fixed, UTC, monotonic-free timestamp. A value from
// time.Now() carries a monotonic clock reading that json round-trips to a
// different metav1.Time, which would break reflect.DeepEqual comparisons.
var fixedTransitionTime = time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)

// contractConditions returns the conditions carried by the populated fixture:
// the standard InfrastructureReady condition plus a failing condition.
func contractConditions() []metav1.Condition {
	return []metav1.Condition{
		{
			Type:               clusterv1.InfrastructureReadyCondition,
			Status:             metav1.ConditionTrue,
			LastTransitionTime: metav1.Time{Time: fixedTransitionTime},
			Reason:             "BridgeAndNATReady",
			Message:            "bridge k8sbr0 and NAT table k8slab are ready",
		},
		{
			Type:               "DNSForwarderReady",
			Status:             metav1.ConditionFalse,
			LastTransitionTime: metav1.Time{Time: fixedTransitionTime},
			Reason:             "DnsmasqNotRunning",
			Message:            "dnsmasq failed to start",
		},
	}
}

// newFullyPopulatedCluster builds a HypervisorCluster with every contract
// field set. It is a maximally populated round-trip fixture, not a coherent
// reconciled state.
func newFullyPopulatedCluster() *v1alpha1.HypervisorCluster {
	return &v1alpha1.HypervisorCluster{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
			Kind:       "HypervisorCluster",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:        "lab-cluster",
			Namespace:   "lab",
			Labels:      map[string]string{"cluster.x-k8s.io/cluster-name": "lab-cluster"},
			Annotations: map[string]string{"k8slabs.io/owner": "contract-tests"},
		},
		Spec: v1alpha1.HypervisorClusterSpec{
			ClusterName: "lab-cluster",
			Network: v1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       "192.168.124.0/24",
				Gateway:    "192.168.124.1",
				DNSIP:      "192.168.124.1",
				BridgeName: "k8sbr0",
				NATTable:   "k8slab",
			},
		},
		Status: v1alpha1.HypervisorClusterStatus{
			Ready: true,
			ControlPlaneEndpoint: clusterv1.APIEndpoint{
				Host: "192.168.124.10",
				Port: 6443,
			},
			FailureReason:  "ProvisioningFailed",
			FailureMessage: "dnsmasq failed to start (contract fixture)",
			Conditions:     contractConditions(),
		},
	}
}

// TestHypervisorClusterGroupVersionKind verifies that registering the type
// with a scheme resolves to the infrastructure.cluster.x-k8s.io/v1alpha1
// group version and the HypervisorCluster kind (spec REQ-002).
func TestHypervisorClusterGroupVersionKind(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme: %v", err)
	}

	gvks, _, err := scheme.ObjectKinds(&v1alpha1.HypervisorCluster{})
	if err != nil {
		t.Fatalf("ObjectKinds: %v", err)
	}

	want := schema.GroupVersionKind{
		Group:   "infrastructure.cluster.x-k8s.io",
		Version: "v1alpha1",
		Kind:    "HypervisorCluster",
	}
	if len(gvks) != 1 {
		t.Fatalf("ObjectKinds returned %d kinds, want 1: %v", len(gvks), gvks)
	}
	if got := gvks[0]; got != want {
		t.Errorf("GroupVersionKind = %s, want %s", got, want)
	}
}

// TestHypervisorClusterJSONRoundTrip verifies that a fully populated
// HypervisorCluster survives a marshal/unmarshal cycle with all contract
// fields preserved (spec REQ-003).
func TestHypervisorClusterJSONRoundTrip(t *testing.T) {
	tests := []struct {
		name string
		give *v1alpha1.HypervisorCluster
	}{
		{name: "fully populated contract fields", give: newFullyPopulatedCluster()},
		{name: "zero value", give: &v1alpha1.HypervisorCluster{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw, err := json.Marshal(tt.give)
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}

			var got v1alpha1.HypervisorCluster
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

// TestHypervisorClusterDeepCopyNonAliasing verifies that DeepCopyObject
// returns a fully independent object: mutating the copy's network spec,
// conditions slice, or control plane endpoint must not touch the original.
func TestHypervisorClusterDeepCopyNonAliasing(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*v1alpha1.HypervisorCluster)
	}{
		{
			name: "spec.network.cidr",
			mutate: func(c *v1alpha1.HypervisorCluster) {
				c.Spec.Network.CIDR = "10.0.0.0/8"
			},
		},
		{
			name: "spec.network sibling fields",
			mutate: func(c *v1alpha1.HypervisorCluster) {
				c.Spec.Network.Gateway = "10.0.0.1"
				c.Spec.Network.DNSIP = "10.0.0.2"
				c.Spec.Network.BridgeName = "br1"
				c.Spec.Network.NATTable = "nat1"
			},
		},
		{
			name: "status.conditions append",
			mutate: func(c *v1alpha1.HypervisorCluster) {
				c.Status.Conditions = append(c.Status.Conditions, metav1.Condition{
					Type:               "MachinePoolReady",
					Status:             metav1.ConditionUnknown,
					LastTransitionTime: metav1.Time{Time: fixedTransitionTime},
					Reason:             "Unknown",
					Message:            "appended condition",
				})
			},
		},
		{
			name: "status.conditions element mutation",
			mutate: func(c *v1alpha1.HypervisorCluster) {
				c.Status.Conditions[0].Message = "mutated"
			},
		},
		{
			name: "status.controlPlaneEndpoint",
			mutate: func(c *v1alpha1.HypervisorCluster) {
				c.Status.ControlPlaneEndpoint.Host = "10.0.0.10"
				c.Status.ControlPlaneEndpoint.Port = 8443
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			original := newFullyPopulatedCluster()
			obj := original.DeepCopyObject()

			copy, ok := obj.(*v1alpha1.HypervisorCluster)
			if !ok {
				t.Fatalf("DeepCopyObject returned %T, want *v1alpha1.HypervisorCluster", obj)
			}
			if copy == original {
				t.Fatal("DeepCopyObject returned the original pointer")
			}
			if !reflect.DeepEqual(copy, original) {
				t.Fatalf("DeepCopyObject did not preserve the value:\ncopy:     %#v\noriginal: %#v", copy, original)
			}

			// want is built from literals, so it is independent of the
			// DeepCopyObject implementation under test.
			want := newFullyPopulatedCluster()
			tt.mutate(copy)

			if !reflect.DeepEqual(original, want) {
				t.Errorf("mutating the copy changed the original:\nwant: %#v\ngot:  %#v", want, original)
			}
		})
	}
}

// TestHypervisorClusterConditionsContract verifies the GetConditions and
// SetConditions methods round-trip a []metav1.Condition list, satisfying
// the conditions accessor contract used by the CAPI conditions package.
func TestHypervisorClusterConditionsContract(t *testing.T) {
	t.Run("unset conditions return nil", func(t *testing.T) {
		cluster := &v1alpha1.HypervisorCluster{}
		if got := cluster.GetConditions(); got != nil {
			t.Errorf("GetConditions() on zero object = %v, want nil", got)
		}
	})

	conditions := contractConditions()
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
			cluster := &v1alpha1.HypervisorCluster{}
			cluster.SetConditions(tt.give)

			got := cluster.GetConditions()
			if !reflect.DeepEqual(got, tt.give) {
				t.Errorf("conditions round trip mismatch:\nwant: %#v\ngot:  %#v", tt.give, got)
			}
		})
	}
}
