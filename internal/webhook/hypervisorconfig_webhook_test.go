// Contract tests for the HypervisorConfig defaulting and validation webhook.
//
// This file pins the exact behavior the webhook in this package must
// implement. Defaulting is a no-op: every spec field keeps its value — the
// cluster name, the role, the node name, and the optional SSH public key are
// all left untouched. Validation requires the role to be exactly one of the
// enum values "control-plane" or "worker" (case-sensitive; empty and any other
// value are rejected), a non-empty node name, and a non-empty cluster name;
// the optional SSH public key is not validated. The same rules apply on create
// and update, where the new object is validated; deletion is always allowed.
// The compile-time pins below force the webhook type to implement the
// controller-runtime defaulter and validator interfaces with the exact method
// signatures this file calls.

package webhook_test

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/webhook"
)

// Compile-time pins: HypervisorConfigWebhook must satisfy the
// controller-runtime defaulter and validator interfaces. The generic
// instantiations below are the same types the deprecated CustomDefaulter and
// CustomValidator aliases resolve to, so the methods take runtime.Object:
//
//	Default(ctx context.Context, obj runtime.Object) error
//	ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
//	ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error)
//	ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
var (
	_ admission.Defaulter[runtime.Object] = &webhook.HypervisorConfigWebhook{}
	_ admission.Validator[runtime.Object] = &webhook.HypervisorConfigWebhook{}
)

// validConfig returns a HypervisorConfig that satisfies every pinned
// validation rule: a role from the enum, a node name, a cluster name, and an
// optional SSH public key.
func validConfig() *bootstrapv1alpha1.HypervisorConfig {
	return &bootstrapv1alpha1.HypervisorConfig{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "lab"},
		Spec: bootstrapv1alpha1.HypervisorConfigSpec{
			ClusterName:  "lab-cluster",
			Role:         "control-plane",
			NodeName:     "cp-1",
			SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample contract-fixture",
		},
	}
}

// withRole returns obj with the role replaced. Callers pass a fresh
// validConfig() so table rows never share mutable state.
func withRole(obj *bootstrapv1alpha1.HypervisorConfig, role string) *bootstrapv1alpha1.HypervisorConfig {
	obj.Spec.Role = role
	return obj
}

// withNodeName returns obj with the node name replaced.
func withNodeName(obj *bootstrapv1alpha1.HypervisorConfig, nodeName string) *bootstrapv1alpha1.HypervisorConfig {
	obj.Spec.NodeName = nodeName
	return obj
}

// withClusterName returns obj with the cluster name replaced.
func withClusterName(obj *bootstrapv1alpha1.HypervisorConfig, clusterName string) *bootstrapv1alpha1.HypervisorConfig {
	obj.Spec.ClusterName = clusterName
	return obj
}

// withSSHPublicKey returns obj with the SSH public key replaced.
func withSSHPublicKey(obj *bootstrapv1alpha1.HypervisorConfig, key string) *bootstrapv1alpha1.HypervisorConfig {
	obj.Spec.SSHPublicKey = key
	return obj
}

// TestHypervisorConfigDefaulting pins the mutating webhook as a no-op: a
// zero-value config stays zero, a populated config keeps every spec field
// including the optional SSH public key. Any non-HypervisorConfig or nil
// object is rejected.
func TestHypervisorConfigDefaulting(t *testing.T) {
	tests := []struct {
		name string
		give *bootstrapv1alpha1.HypervisorConfig
		want bootstrapv1alpha1.HypervisorConfigSpec
	}{
		{
			name: "zero-value config is not modified",
			give: &bootstrapv1alpha1.HypervisorConfig{},
			want: bootstrapv1alpha1.HypervisorConfigSpec{},
		},
		{
			name: "populated config is not modified",
			give: validConfig(),
			want: bootstrapv1alpha1.HypervisorConfigSpec{
				ClusterName:  "lab-cluster",
				Role:         "control-plane",
				NodeName:     "cp-1",
				SSHPublicKey: "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIexample contract-fixture",
			},
		},
		{
			name: "empty sshPublicKey is not filled in",
			give: withSSHPublicKey(validConfig(), ""),
			want: bootstrapv1alpha1.HypervisorConfigSpec{
				ClusterName: "lab-cluster",
				Role:        "control-plane",
				NodeName:    "cp-1",
			},
		},
	}
	wh := &webhook.HypervisorConfigWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := wh.Default(t.Context(), tt.give); err != nil {
				t.Fatalf("Default: %v", err)
			}
			if got := tt.give.Spec; !reflect.DeepEqual(got, tt.want) {
				t.Errorf("spec after Default = %#v, want %#v", got, tt.want)
			}
		})
	}

	t.Run("wrong object type is rejected", func(t *testing.T) {
		err := wh.Default(t.Context(), &bootstrapv1alpha1.HypervisorConfigList{})
		if err == nil {
			t.Error("Default on a HypervisorConfigList: want error, got nil")
		}
	})

	t.Run("nil object is rejected", func(t *testing.T) {
		err := wh.Default(t.Context(), nil)
		if err == nil {
			t.Error("Default on a nil object: want error, got nil")
		}
	})
}

// TestHypervisorConfigValidateCreate pins the create admission rules: the role
// must be exactly "control-plane" or "worker", the node name must be
// non-empty, the cluster name must be non-empty, and the optional SSH public
// key is ignored. Anything else is rejected with an error.
func TestHypervisorConfigValidateCreate(t *testing.T) {
	tests := []struct {
		name    string
		give    runtime.Object
		wantErr bool
	}{
		{name: "valid config", give: validConfig(), wantErr: false},
		{name: "worker role is accepted", give: withRole(validConfig(), "worker"), wantErr: false},
		{name: "empty sshPublicKey is optional", give: withSSHPublicKey(validConfig(), ""), wantErr: false},
		{name: "empty role is rejected", give: withRole(validConfig(), ""), wantErr: true},
		{name: "master role is rejected", give: withRole(validConfig(), "master"), wantErr: true},
		{name: "node role is rejected", give: withRole(validConfig(), "node"), wantErr: true},
		{name: "mixed-case role is rejected", give: withRole(validConfig(), "Control-Plane"), wantErr: true},
		{name: "uppercase role is rejected", give: withRole(validConfig(), "WORKER"), wantErr: true},
		{name: "role with surrounding whitespace is rejected", give: withRole(validConfig(), " control-plane "), wantErr: true},
		{name: "empty nodeName is rejected", give: withNodeName(validConfig(), ""), wantErr: true},
		{name: "empty clusterName is rejected", give: withClusterName(validConfig(), ""), wantErr: true},
		{name: "multiple invalid fields are rejected", give: withNodeName(withRole(validConfig(), ""), ""), wantErr: true},
		{name: "wrong object type", give: &bootstrapv1alpha1.HypervisorConfigList{}, wantErr: true},
		{name: "nil object", give: nil, wantErr: true},
	}
	wh := &webhook.HypervisorConfigWebhook{}
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

// TestHypervisorConfigValidateUpdate pins the update admission rules: the new
// object is held to the same role, node name, and cluster name rules as
// create, while a previously invalid object may be fixed by the update.
func TestHypervisorConfigValidateUpdate(t *testing.T) {
	tests := []struct {
		name    string
		oldObj  runtime.Object
		newObj  runtime.Object
		wantErr bool
	}{
		{name: "valid to valid", oldObj: validConfig(), newObj: validConfig(), wantErr: false},
		{name: "valid to worker role", oldObj: validConfig(), newObj: withRole(validConfig(), "worker"), wantErr: false},
		{name: "valid to empty role", oldObj: validConfig(), newObj: withRole(validConfig(), ""), wantErr: true},
		{name: "valid to invalid role", oldObj: validConfig(), newObj: withRole(validConfig(), "master"), wantErr: true},
		{name: "valid to empty nodeName", oldObj: validConfig(), newObj: withNodeName(validConfig(), ""), wantErr: true},
		{name: "valid to empty clusterName", oldObj: validConfig(), newObj: withClusterName(validConfig(), ""), wantErr: true},
		{name: "invalid old can be fixed", oldObj: withRole(validConfig(), "master"), newObj: validConfig(), wantErr: false},
		{name: "invalid to invalid", oldObj: withRole(validConfig(), "master"), newObj: withNodeName(validConfig(), ""), wantErr: true},
		{name: "wrong new object type", oldObj: validConfig(), newObj: &bootstrapv1alpha1.HypervisorConfigList{}, wantErr: true},
		{name: "wrong old object type", oldObj: &bootstrapv1alpha1.HypervisorConfigList{}, newObj: validConfig(), wantErr: true},
		{name: "nil new object", oldObj: validConfig(), newObj: nil, wantErr: true},
		{name: "nil old object", oldObj: nil, newObj: validConfig(), wantErr: true},
	}
	wh := &webhook.HypervisorConfigWebhook{}
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

// TestHypervisorConfigValidateDelete pins that deletion is always allowed,
// even for objects whose content would fail create validation and for
// zero-value configs. Any non-HypervisorConfig or nil object is rejected.
func TestHypervisorConfigValidateDelete(t *testing.T) {
	tests := []struct {
		name string
		give runtime.Object
	}{
		{name: "valid config", give: validConfig()},
		{name: "invalid content still deletable", give: withRole(validConfig(), "master")},
		{name: "zero-value config deletable", give: &bootstrapv1alpha1.HypervisorConfig{}},
	}
	wh := &webhook.HypervisorConfigWebhook{}
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
		_, err := wh.ValidateDelete(t.Context(), &bootstrapv1alpha1.HypervisorConfigList{})
		if err == nil {
			t.Error("ValidateDelete on a HypervisorConfigList: want error, got nil")
		}
	})

	t.Run("nil object is rejected", func(t *testing.T) {
		_, err := wh.ValidateDelete(t.Context(), nil)
		if err == nil {
			t.Error("ValidateDelete on a nil object: want error, got nil")
		}
	})
}
