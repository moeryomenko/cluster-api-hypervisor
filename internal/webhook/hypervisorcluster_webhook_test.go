// Contract tests for the HypervisorCluster defaulting and validation webhook.
//
// This file pins the exact behavior the webhook in this package must
// implement. Defaulting fills an empty network CIDR with the lab subnet, an
// empty bridge name with the standard lab bridge, and an empty NAT table with
// the standard lab table, while leaving user-supplied values and the optional
// gateway and DNS fields untouched. Validation accepts only parseable IPv4
// CIDRs and an optional gateway that is a plain IPv4 address, applies the
// same rules on create and update, and always permits deletion. The
// compile-time pins below force the webhook type to implement the
// controller-runtime defaulter and validator interfaces with the exact method
// signatures this file calls.

package webhook_test

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	"github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/webhook"
)

// Compile-time pins: HypervisorClusterWebhook must satisfy the
// controller-runtime defaulter and validator interfaces. The generic
// instantiations below are the same types the deprecated CustomDefaulter and
// CustomValidator aliases resolve to, so the methods take runtime.Object:
//
//	Default(ctx context.Context, obj runtime.Object) error
//	ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
//	ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error)
//	ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
var (
	_ admission.Defaulter[runtime.Object] = &webhook.HypervisorClusterWebhook{}
	_ admission.Validator[runtime.Object] = &webhook.HypervisorClusterWebhook{}
)

// validCluster returns a HypervisorCluster that satisfies every pinned
// validation rule: a parseable IPv4 CIDR and a gateway that is a plain IPv4
// address.
func validCluster() *v1alpha1.HypervisorCluster {
	return &v1alpha1.HypervisorCluster{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-cluster", Namespace: "lab"},
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
	}
}

// withCIDR returns obj with the network CIDR replaced. Callers pass a fresh
// validCluster() so table rows never share mutable state.
func withCIDR(obj *v1alpha1.HypervisorCluster, cidr string) *v1alpha1.HypervisorCluster {
	obj.Spec.Network.CIDR = cidr
	return obj
}

// withGateway returns obj with the network gateway replaced.
func withGateway(obj *v1alpha1.HypervisorCluster, gateway string) *v1alpha1.HypervisorCluster {
	obj.Spec.Network.Gateway = gateway
	return obj
}

// TestHypervisorClusterDefaulting pins the mutating webhook defaults: an
// empty CIDR becomes the k8labs lab subnet, an empty bridge name becomes
// k8sbr0, and an empty NAT table becomes k8slab. Values the user already set
// are preserved, and the optional fields that have no default (gateway,
// dnsIP) stay empty.
func TestHypervisorClusterDefaulting(t *testing.T) {
	tests := []struct {
		name string
		give *v1alpha1.HypervisorCluster
		want v1alpha1.HypervisorClusterNetworkSpec
	}{
		{
			name: "empty network gets the lab defaults",
			give: &v1alpha1.HypervisorCluster{},
			want: v1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       "192.168.124.0/24",
				BridgeName: "k8sbr0",
				NATTable:   "k8slab",
			},
		},
		{
			name: "user values are preserved",
			give: &v1alpha1.HypervisorCluster{
				Spec: v1alpha1.HypervisorClusterSpec{Network: v1alpha1.HypervisorClusterNetworkSpec{
					CIDR:       "10.0.0.0/16",
					Gateway:    "10.0.0.1",
					DNSIP:      "10.0.0.1",
					BridgeName: "br-lab",
					NATTable:   "lab-nat",
				}},
			},
			want: v1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       "10.0.0.0/16",
				Gateway:    "10.0.0.1",
				DNSIP:      "10.0.0.1",
				BridgeName: "br-lab",
				NATTable:   "lab-nat",
			},
		},
		{
			name: "only the missing fields are defaulted",
			give: &v1alpha1.HypervisorCluster{
				Spec: v1alpha1.HypervisorClusterSpec{Network: v1alpha1.HypervisorClusterNetworkSpec{
					CIDR: "172.16.0.0/12",
				}},
			},
			want: v1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       "172.16.0.0/12",
				BridgeName: "k8sbr0",
				NATTable:   "k8slab",
			},
		},
		{
			name: "gateway and dnsIP have no defaults",
			give: &v1alpha1.HypervisorCluster{
				Spec: v1alpha1.HypervisorClusterSpec{Network: v1alpha1.HypervisorClusterNetworkSpec{
					BridgeName: "br-x",
					NATTable:   "nat-x",
				}},
			},
			want: v1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       "192.168.124.0/24",
				BridgeName: "br-x",
				NATTable:   "nat-x",
			},
		},
	}
	wh := &webhook.HypervisorClusterWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := wh.Default(t.Context(), tt.give); err != nil {
				t.Fatalf("Default: %v", err)
			}
			if got := tt.give.Spec.Network; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("network after Default = %#v, want %#v", got, tt.want)
			}
		})
	}

	t.Run("wrong object type is rejected", func(t *testing.T) {
		err := wh.Default(t.Context(), &v1alpha1.HypervisorClusterList{})
		if err == nil {
			t.Error("Default on a HypervisorClusterList: want error, got nil")
		}
	})
}

// TestHypervisorClusterValidateCreate pins the create admission rules: a
// parseable IPv4 CIDR and an optional gateway that is a plain IPv4 address.
// Anything else is rejected with an error.
func TestHypervisorClusterValidateCreate(t *testing.T) {
	tests := []struct {
		name    string
		give    runtime.Object
		wantErr bool
	}{
		{name: "valid cluster", give: validCluster(), wantErr: false},
		{name: "alternate valid cidr", give: withCIDR(validCluster(), "10.0.0.0/16"), wantErr: false},
		{name: "empty gateway is optional", give: withGateway(validCluster(), ""), wantErr: false},
		{name: "cidr is not a cidr", give: withCIDR(validCluster(), "not-a-cidr"), wantErr: true},
		{name: "cidr prefix out of range", give: withCIDR(validCluster(), "10.0.0.0/99"), wantErr: true},
		{name: "cidr octet out of range", give: withCIDR(validCluster(), "256.0.0.0/24"), wantErr: true},
		{name: "cidr missing prefix", give: withCIDR(validCluster(), "10.0.0.0"), wantErr: true},
		{name: "gateway is not an ip", give: withGateway(validCluster(), "gateway"), wantErr: true},
		{name: "gateway is a cidr not an ip", give: withGateway(validCluster(), "10.0.0.0/24"), wantErr: true},
		{name: "gateway octet out of range", give: withGateway(validCluster(), "999.1.1.1"), wantErr: true},
		{
			name:    "invalid cidr and gateway combined",
			give:    withGateway(withCIDR(validCluster(), "not-a-cidr"), "gateway"),
			wantErr: true,
		},
		{name: "wrong object type", give: &v1alpha1.HypervisorClusterList{}, wantErr: true},
		{name: "nil object", give: nil, wantErr: true},
	}
	wh := &webhook.HypervisorClusterWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := wh.ValidateCreate(t.Context(), tt.give)
			if tt.wantErr {
				if err == nil {
					t.Error("ValidateCreate: want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateCreate: unexpected error: %v", err)
			}
			if len(warnings) != 0 {
				t.Errorf("ValidateCreate: unexpected warnings: %v", warnings)
			}
		})
	}
}

// TestHypervisorClusterValidateUpdate pins the update admission rules: the
// new object is held to the same CIDR and gateway rules as create, while a
// previously invalid object may be fixed by the update.
func TestHypervisorClusterValidateUpdate(t *testing.T) {
	tests := []struct {
		name    string
		oldObj  runtime.Object
		newObj  runtime.Object
		wantErr bool
	}{
		{name: "valid to valid", oldObj: validCluster(), newObj: validCluster(), wantErr: false},
		{
			name:    "valid to invalid cidr",
			oldObj:  validCluster(),
			newObj:  withCIDR(validCluster(), "not-a-cidr"),
			wantErr: true,
		},
		{
			name:    "valid to bad gateway",
			oldObj:  validCluster(),
			newObj:  withGateway(validCluster(), "10.0.0.0/24"),
			wantErr: true,
		},
		{name: "valid to empty gateway", oldObj: validCluster(), newObj: withGateway(validCluster(), ""), wantErr: false},
		{
			name:    "invalid old can be fixed",
			oldObj:  withCIDR(validCluster(), "not-a-cidr"),
			newObj:  validCluster(),
			wantErr: false,
		},
		{
			name:    "invalid to invalid",
			oldObj:  withCIDR(validCluster(), "not-a-cidr"),
			newObj:  withGateway(validCluster(), "gateway"),
			wantErr: true,
		},
		{name: "wrong new object type", oldObj: validCluster(), newObj: &v1alpha1.HypervisorClusterList{}, wantErr: true},
		{name: "wrong old object type", oldObj: &v1alpha1.HypervisorClusterList{}, newObj: validCluster(), wantErr: true},
	}
	wh := &webhook.HypervisorClusterWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := wh.ValidateUpdate(t.Context(), tt.oldObj, tt.newObj)
			if tt.wantErr {
				if err == nil {
					t.Error("ValidateUpdate: want error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("ValidateUpdate: unexpected error: %v", err)
			}
			if len(warnings) != 0 {
				t.Errorf("ValidateUpdate: unexpected warnings: %v", warnings)
			}
		})
	}
}

// TestHypervisorClusterValidateDelete pins that deletion is always allowed,
// even for objects whose content would fail create validation.
func TestHypervisorClusterValidateDelete(t *testing.T) {
	tests := []struct {
		name string
		give runtime.Object
	}{
		{name: "valid cluster", give: validCluster()},
		{name: "invalid content still deletable", give: withCIDR(validCluster(), "not-a-cidr")},
	}
	wh := &webhook.HypervisorClusterWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			warnings, err := wh.ValidateDelete(t.Context(), tt.give)
			if err != nil {
				t.Fatalf("ValidateDelete: unexpected error: %v", err)
			}
			if len(warnings) != 0 {
				t.Errorf("ValidateDelete: unexpected warnings: %v", warnings)
			}
		})
	}

	t.Run("wrong object type is rejected", func(t *testing.T) {
		_, err := wh.ValidateDelete(t.Context(), &v1alpha1.HypervisorClusterList{})
		if err == nil {
			t.Error("ValidateDelete on a HypervisorClusterList: want error, got nil")
		}
	})
}
