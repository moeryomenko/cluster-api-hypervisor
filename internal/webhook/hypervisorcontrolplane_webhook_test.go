// Contract tests for the HypervisorControlPlane defaulting and validation
// webhook.
//
// This file pins the exact behavior the webhook in this package must
// implement. Defaulting is a no-op: every spec field keeps its value,
// including Replicas, whose default of one is applied by the API layer
// (CRD/controller), not by the mutating webhook — a zero-value Replicas must
// stay zero after Default. Validation requires at least one replica (zero or
// negative replicas are rejected, because scaling this single-control-plane
// generation to zero is destructive and unsupported) and a non-empty
// machineTemplate.infrastructureRef; the version field is informational and
// accepts any value, including empty. The same rules apply on create and
// update, where the new object is validated, so scaling a live control plane
// down to zero replicas is rejected too; deletion is always allowed. The
// compile-time pins below force the webhook type to implement the
// controller-runtime defaulter and validator interfaces with the exact method
// signatures this file calls.

package webhook_test

import (
	"reflect"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/webhook"
)

// Compile-time pins: HypervisorControlPlaneWebhook must satisfy the
// controller-runtime defaulter and validator interfaces. The generic
// instantiations below are the same types the deprecated CustomDefaulter and
// CustomValidator aliases resolve to, so the methods take runtime.Object:
//
//	Default(ctx context.Context, obj runtime.Object) error
//	ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
//	ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error)
//	ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
var (
	_ admission.Defaulter[runtime.Object] = &webhook.HypervisorControlPlaneWebhook{}
	_ admission.Validator[runtime.Object] = &webhook.HypervisorControlPlaneWebhook{}
)

// validControlPlane returns a HypervisorControlPlane that satisfies every
// pinned validation rule: at least one replica, a set infrastructureRef, and
// an informational version.
func validControlPlane() *controlplanev1alpha1.HypervisorControlPlane {
	return &controlplanev1alpha1.HypervisorControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-cluster", Namespace: "lab"},
		Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
			Replicas: 1,
			Version:  "v1.36.0",
			MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					Kind:      "HypervisorMachineTemplate",
					Namespace: "lab",
					Name:      "lab-cluster-cp",
				},
			},
		},
	}
}

// withReplicas returns obj with the desired replica count replaced. Callers
// pass a fresh validControlPlane() so table rows never share mutable state.
func withReplicas(obj *controlplanev1alpha1.HypervisorControlPlane, replicas int32) *controlplanev1alpha1.HypervisorControlPlane {
	obj.Spec.Replicas = replicas
	return obj
}

// withVersion returns obj with the informational version replaced.
func withVersion(obj *controlplanev1alpha1.HypervisorControlPlane, version string) *controlplanev1alpha1.HypervisorControlPlane {
	obj.Spec.Version = version
	return obj
}

// withInfrastructureRef returns obj with the machine template
// infrastructureRef replaced.
func withInfrastructureRef(obj *controlplanev1alpha1.HypervisorControlPlane, ref corev1.ObjectReference) *controlplanev1alpha1.HypervisorControlPlane {
	obj.Spec.MachineTemplate.InfrastructureRef = ref
	return obj
}

// TestHypervisorControlPlaneDefaulting pins the mutating webhook as a no-op:
// a zero-value control plane stays zero, a populated control plane keeps
// every field, and the replica count is never defaulted at webhook time (the
// default of one lives in the API layer, so zero stays zero and values above
// one are preserved). Any non-HypervisorControlPlane object is rejected.
func TestHypervisorControlPlaneDefaulting(t *testing.T) {
	tests := []struct {
		name string
		give *controlplanev1alpha1.HypervisorControlPlane
		want controlplanev1alpha1.HypervisorControlPlaneSpec
	}{
		{
			name: "zero-value control plane is not modified",
			give: &controlplanev1alpha1.HypervisorControlPlane{},
			want: controlplanev1alpha1.HypervisorControlPlaneSpec{},
		},
		{
			name: "populated control plane is not modified",
			give: validControlPlane(),
			want: validControlPlane().Spec,
		},
		{
			name: "zero replicas stay zero (no default to one)",
			give: withReplicas(validControlPlane(), 0),
			want: withReplicas(validControlPlane(), 0).Spec,
		},
		{
			name: "replicas above one are preserved",
			give: withReplicas(validControlPlane(), 3),
			want: withReplicas(validControlPlane(), 3).Spec,
		},
	}
	wh := &webhook.HypervisorControlPlaneWebhook{}
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
		err := wh.Default(t.Context(), &controlplanev1alpha1.HypervisorControlPlaneList{})
		if err == nil {
			t.Error("Default on a HypervisorControlPlaneList: want error, got nil")
		}
	})
}

// TestHypervisorControlPlaneValidateCreate pins the create admission rules:
// at least one replica is required (zero, which is what an unset field yields,
// and negative values are rejected), the machine template infrastructureRef
// must be non-empty, and the informational version accepts any value.
// Anything else is rejected with an error.
func TestHypervisorControlPlaneValidateCreate(t *testing.T) {
	tests := []struct {
		name    string
		give    runtime.Object
		wantErr bool
	}{
		{name: "valid control plane with version set", give: validControlPlane(), wantErr: false},
		{name: "more than one replica is accepted", give: withReplicas(validControlPlane(), 2), wantErr: false},
		{name: "empty version is accepted", give: withVersion(validControlPlane(), ""), wantErr: false},
		{name: "unset replicas are rejected", give: withReplicas(validControlPlane(), 0), wantErr: true},
		{name: "negative replicas are rejected", give: withReplicas(validControlPlane(), -1), wantErr: true},
		{name: "zero infrastructureRef is rejected", give: withInfrastructureRef(validControlPlane(), corev1.ObjectReference{}), wantErr: true},
		{name: "wrong object type", give: &controlplanev1alpha1.HypervisorControlPlaneList{}, wantErr: true},
		{name: "nil object", give: nil, wantErr: true},
	}
	wh := &webhook.HypervisorControlPlaneWebhook{}
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

// TestHypervisorControlPlaneValidateUpdate pins the update admission rules:
// the new object is held to the same replica and infrastructureRef rules as
// create, so scaling an existing control plane down to zero replicas is
// rejected, while a previously invalid object may be fixed by the update.
func TestHypervisorControlPlaneValidateUpdate(t *testing.T) {
	tests := []struct {
		name    string
		oldObj  runtime.Object
		newObj  runtime.Object
		wantErr bool
	}{
		{name: "valid to valid", oldObj: validControlPlane(), newObj: validControlPlane(), wantErr: false},
		{name: "valid to zero replicas is rejected", oldObj: validControlPlane(), newObj: withReplicas(validControlPlane(), 0), wantErr: true},
		{name: "valid to negative replicas is rejected", oldObj: validControlPlane(), newObj: withReplicas(validControlPlane(), -1), wantErr: true},
		{name: "valid to zero infrastructureRef is rejected", oldObj: validControlPlane(), newObj: withInfrastructureRef(validControlPlane(), corev1.ObjectReference{}), wantErr: true},
		{name: "invalid old can be fixed", oldObj: withReplicas(validControlPlane(), 0), newObj: validControlPlane(), wantErr: false},
		{name: "wrong new object type", oldObj: validControlPlane(), newObj: &controlplanev1alpha1.HypervisorControlPlaneList{}, wantErr: true},
		{name: "wrong old object type", oldObj: &controlplanev1alpha1.HypervisorControlPlaneList{}, newObj: validControlPlane(), wantErr: true},
	}
	wh := &webhook.HypervisorControlPlaneWebhook{}
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

// TestHypervisorControlPlaneValidateDelete pins that deletion is always
// allowed, even for objects whose content would fail create validation.
func TestHypervisorControlPlaneValidateDelete(t *testing.T) {
	tests := []struct {
		name string
		give runtime.Object
	}{
		{name: "valid control plane", give: validControlPlane()},
		{name: "invalid content still deletable", give: withReplicas(validControlPlane(), 0)},
	}
	wh := &webhook.HypervisorControlPlaneWebhook{}
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
		_, err := wh.ValidateDelete(t.Context(), &controlplanev1alpha1.HypervisorControlPlaneList{})
		if err == nil {
			t.Error("ValidateDelete on a HypervisorControlPlaneList: want error, got nil")
		}
	})
}
