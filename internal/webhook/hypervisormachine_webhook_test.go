// Contract tests for the HypervisorMachine defaulting and validation webhook.
//
// This file pins the exact behavior the webhook in this package must
// implement. Defaulting is a no-op: every spec field keeps its value,
// including RetainDiskOnDelete whose Go zero value (false) is already the
// documented default, so the mutating webhook must not alter any field.
// Validation requires a positive CPU, RAM, and disk; the optional MAC, when
// set, must be a well-formed MAC address belonging to the lab family whose
// prefix is c6:e5:50:1c:ec (an empty MAC is allowed because the controller
// derives one from a stable hash of the cluster and machine names). The same
// rules apply on create and update, where the new object is validated;
// deletion is always allowed. The compile-time pins below force the webhook
// type to implement the controller-runtime defaulter and validator interfaces
// with the exact method signatures this file calls.

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

// Compile-time pins: HypervisorMachineWebhook must satisfy the
// controller-runtime defaulter and validator interfaces. The generic
// instantiations below are the same types the deprecated CustomDefaulter and
// CustomValidator aliases resolve to, so the methods take runtime.Object:
//
//	Default(ctx context.Context, obj runtime.Object) error
//	ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
//	ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error)
//	ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
var (
	_ admission.Defaulter[runtime.Object] = &webhook.HypervisorMachineWebhook{}
	_ admission.Validator[runtime.Object] = &webhook.HypervisorMachineWebhook{}
)

// validMachine returns a HypervisorMachine that satisfies every pinned
// validation rule: positive CPU, RAM, and disk, and an empty MAC that the
// controller is allowed to derive.
func validMachine() *v1alpha1.HypervisorMachine {
	return &v1alpha1.HypervisorMachine{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "lab"},
		Spec: v1alpha1.HypervisorMachineSpec{
			ClusterName: "lab-cluster",
			CPU:         2,
			RAM:         2048,
			Disk:        20480,
		},
	}
}

// withMAC returns obj with the MAC override replaced. Callers pass a fresh
// validMachine() so table rows never share mutable state.
func withMAC(obj *v1alpha1.HypervisorMachine, mac string) *v1alpha1.HypervisorMachine {
	obj.Spec.MAC = mac
	return obj
}

// withCPU returns obj with the CPU count replaced.
func withCPU(obj *v1alpha1.HypervisorMachine, cpu int32) *v1alpha1.HypervisorMachine {
	obj.Spec.CPU = cpu
	return obj
}

// withRAM returns obj with the RAM size replaced.
func withRAM(obj *v1alpha1.HypervisorMachine, ram int32) *v1alpha1.HypervisorMachine {
	obj.Spec.RAM = ram
	return obj
}

// withDisk returns obj with the disk size replaced.
func withDisk(obj *v1alpha1.HypervisorMachine, disk int32) *v1alpha1.HypervisorMachine {
	obj.Spec.Disk = disk
	return obj
}

// TestHypervisorMachineDefaulting pins the mutating webhook as a no-op: a
// zero-value machine stays zero, a populated machine keeps every field,
// RetainDiskOnDelete true is not flipped back to its default, and a set MAC
// override is preserved. Any non-HypervisorMachine object is rejected.
func TestHypervisorMachineDefaulting(t *testing.T) {
	tests := []struct {
		name string
		give *v1alpha1.HypervisorMachine
		want v1alpha1.HypervisorMachineSpec
	}{
		{
			name: "zero-value machine is not modified",
			give: &v1alpha1.HypervisorMachine{},
			want: v1alpha1.HypervisorMachineSpec{},
		},
		{
			name: "populated machine is not modified",
			give: &v1alpha1.HypervisorMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "lab"},
				Spec: v1alpha1.HypervisorMachineSpec{
					ClusterName:        "lab-cluster",
					CPU:                2,
					RAM:                2048,
					Disk:               20480,
					MAC:                "c6:e5:50:1c:ec:01",
					RetainDiskOnDelete: false,
				},
			},
			want: v1alpha1.HypervisorMachineSpec{
				ClusterName:        "lab-cluster",
				CPU:                2,
				RAM:                2048,
				Disk:               20480,
				MAC:                "c6:e5:50:1c:ec:01",
				RetainDiskOnDelete: false,
			},
		},
		{
			name: "retainDiskOnDelete true is preserved",
			give: &v1alpha1.HypervisorMachine{
				Spec: v1alpha1.HypervisorMachineSpec{
					CPU:                4,
					RAM:                4096,
					Disk:               20480,
					RetainDiskOnDelete: true,
				},
			},
			want: v1alpha1.HypervisorMachineSpec{
				CPU:                4,
				RAM:                4096,
				Disk:               20480,
				RetainDiskOnDelete: true,
			},
		},
	}
	wh := &webhook.HypervisorMachineWebhook{}

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
		err := wh.Default(t.Context(), &v1alpha1.HypervisorMachineList{})
		if err == nil {
			t.Error("Default on a HypervisorMachineList: want error, got nil")
		}
	})
}

// TestHypervisorMachineValidateCreate pins the create admission rules: CPU,
// RAM, and disk must be positive, and the optional MAC, when set, must be a
// well-formed MAC address whose first five octets are c6:e5:50:1c:ec. An
// empty MAC is allowed. Anything else is rejected with an error.
func TestHypervisorMachineValidateCreate(t *testing.T) {
	tests := []struct {
		name    string
		give    runtime.Object
		wantErr bool
	}{
		{name: "valid machine", give: validMachine(), wantErr: false},
		{name: "empty MAC is allowed", give: validMachine(), wantErr: false},
		{name: "family MAC is allowed", give: withMAC(validMachine(), "c6:e5:50:1c:ec:01"), wantErr: false},
		{name: "family MAC last octet is arbitrary", give: withMAC(validMachine(), "c6:e5:50:1c:ec:ff"), wantErr: false},
		{name: "malformed MAC is rejected", give: withMAC(validMachine(), "not-a-mac"), wantErr: true},
		{name: "too short MAC is rejected", give: withMAC(validMachine(), "c6:e5:50:1c:ec"), wantErr: true},
		{name: "non-hex MAC is rejected", give: withMAC(validMachine(), "zz:zz:zz:zz:zz:zz"), wantErr: true},
		{name: "out-of-family MAC is rejected", give: withMAC(validMachine(), "00:11:22:33:44:55"), wantErr: true},
		{name: "zero MAC is rejected", give: withMAC(validMachine(), "00:00:00:00:00:00"), wantErr: true},
		{name: "family prefix boundary is rejected", give: withMAC(validMachine(), "c6:e5:50:1c:ed:01"), wantErr: true},
		{name: "zero cpu is rejected", give: withCPU(validMachine(), 0), wantErr: true},
		{name: "negative cpu is rejected", give: withCPU(validMachine(), -1), wantErr: true},
		{name: "zero ram is rejected", give: withRAM(validMachine(), 0), wantErr: true},
		{name: "negative ram is rejected", give: withRAM(validMachine(), -1), wantErr: true},
		{name: "zero disk is rejected", give: withDisk(validMachine(), 0), wantErr: true},
		{name: "negative disk is rejected", give: withDisk(validMachine(), -1), wantErr: true},
		{name: "wrong object type", give: &v1alpha1.HypervisorMachineList{}, wantErr: true},
		{name: "nil object", give: nil, wantErr: true},
	}
	wh := &webhook.HypervisorMachineWebhook{}

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

// TestHypervisorMachineValidateUpdate pins the update admission rules: the
// new object is held to the same MAC and resource rules as create, while a
// previously invalid object may be fixed by the update.
func TestHypervisorMachineValidateUpdate(t *testing.T) {
	tests := []struct {
		name    string
		oldObj  runtime.Object
		newObj  runtime.Object
		wantErr bool
	}{
		{name: "valid to valid", oldObj: validMachine(), newObj: validMachine(), wantErr: false},
		{name: "valid to malformed mac", oldObj: validMachine(), newObj: withMAC(validMachine(), "not-a-mac"), wantErr: true},
		{
			name:    "valid to out-of-family mac",
			oldObj:  validMachine(),
			newObj:  withMAC(validMachine(), "00:11:22:33:44:55"),
			wantErr: true,
		},
		{name: "valid to zero cpu", oldObj: validMachine(), newObj: withCPU(validMachine(), 0), wantErr: true},
		{name: "valid to zero ram", oldObj: validMachine(), newObj: withRAM(validMachine(), 0), wantErr: true},
		{name: "valid to zero disk", oldObj: validMachine(), newObj: withDisk(validMachine(), 0), wantErr: true},
		{
			name:    "invalid old can be fixed",
			oldObj:  withMAC(validMachine(), "not-a-mac"),
			newObj:  validMachine(),
			wantErr: false,
		},
		{name: "wrong new object type", oldObj: validMachine(), newObj: &v1alpha1.HypervisorMachineList{}, wantErr: true},
		{name: "wrong old object type", oldObj: &v1alpha1.HypervisorMachineList{}, newObj: validMachine(), wantErr: true},
	}
	wh := &webhook.HypervisorMachineWebhook{}

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

// TestHypervisorMachineValidateDelete pins that deletion is always allowed,
// even for objects whose content would fail create validation.
func TestHypervisorMachineValidateDelete(t *testing.T) {
	tests := []struct {
		name string
		give runtime.Object
	}{
		{name: "valid machine", give: validMachine()},
		{name: "invalid content still deletable", give: withMAC(validMachine(), "not-a-mac")},
	}
	wh := &webhook.HypervisorMachineWebhook{}

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
		_, err := wh.ValidateDelete(t.Context(), &v1alpha1.HypervisorMachineList{})
		if err == nil {
			t.Error("ValidateDelete on a HypervisorMachineList: want error, got nil")
		}
	})
}
