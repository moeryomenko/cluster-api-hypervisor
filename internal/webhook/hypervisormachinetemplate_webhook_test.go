// Contract tests for the HypervisorMachineTemplate defaulting and validation
// webhook.
//
// This file pins the exact behavior the webhook in this package must
// implement. Defaulting is a no-op: no field of spec.template.spec, no entry
// of spec.template.metadata, and no object metadata is altered. Validation
// enforces template immutability: on update, any change between the old and
// the new object in spec.template.spec — cluster name, cpu, ram, disk, mac,
// or retainDiskOnDelete — is rejected, while objects whose spec values are
// identical are accepted. The metadata of the template resource (labels and
// annotations) is mutable, so metadata-only updates pass. Creation and
// deletion are always allowed regardless of content. The compile-time pins
// below force the webhook type to implement the controller-runtime defaulter
// and validator interfaces with the exact method signatures this file calls.

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

// Compile-time pins: HypervisorMachineTemplateWebhook must satisfy the
// controller-runtime defaulter and validator interfaces. The generic
// instantiations below are the same types the deprecated CustomDefaulter and
// CustomValidator aliases resolve to, so the methods take runtime.Object:
//
//	Default(ctx context.Context, obj runtime.Object) error
//	ValidateCreate(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
//	ValidateUpdate(ctx context.Context, oldObj, newObj runtime.Object) (admission.Warnings, error)
//	ValidateDelete(ctx context.Context, obj runtime.Object) (admission.Warnings, error)
var (
	_ admission.Defaulter[runtime.Object] = &webhook.HypervisorMachineTemplateWebhook{}
	_ admission.Validator[runtime.Object] = &webhook.HypervisorMachineTemplateWebhook{}
)

// validTemplate returns a HypervisorMachineTemplate whose template spec
// carries every field the immutability contract covers.
func validTemplate() *v1alpha1.HypervisorMachineTemplate {
	return &v1alpha1.HypervisorMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "lab"},
		Spec: v1alpha1.HypervisorMachineTemplateSpec{
			Template: v1alpha1.HypervisorMachineTemplateResource{
				ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "lab"},
				Spec: v1alpha1.HypervisorMachineSpec{
					ClusterName: "lab-cluster",
					CPU:         2,
					RAM:         2048,
					Disk:        20480,
				},
			},
		},
	}
}

// fullyPopulatedTemplate returns a valid template with the optional spec
// fields set (mac, retainDiskOnDelete) and template metadata attached.
func fullyPopulatedTemplate() *v1alpha1.HypervisorMachineTemplate {
	return withTemplateMetadata(
		withTemplateRetainDiskOnDelete(withTemplateMAC(validTemplate(), "c6:e5:50:1c:ec:01"), true),
		map[string]string{"role": "control-plane"},
		map[string]string{"note": "k8slab"},
	)
}

// withTemplateClusterName returns obj with the template spec cluster name
// replaced. Callers pass a fresh validTemplate() so table rows never share
// mutable state.
func withTemplateClusterName(obj *v1alpha1.HypervisorMachineTemplate, name string) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.Spec.ClusterName = name
	return obj
}

// withTemplateCPU returns obj with the template spec CPU count replaced.
func withTemplateCPU(obj *v1alpha1.HypervisorMachineTemplate, cpu int32) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.Spec.CPU = cpu
	return obj
}

// withTemplateRAM returns obj with the template spec RAM size replaced.
func withTemplateRAM(obj *v1alpha1.HypervisorMachineTemplate, ram int32) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.Spec.RAM = ram
	return obj
}

// withTemplateDisk returns obj with the template spec disk size replaced.
func withTemplateDisk(obj *v1alpha1.HypervisorMachineTemplate, disk int32) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.Spec.Disk = disk
	return obj
}

// withTemplateMAC returns obj with the template spec MAC override replaced.
func withTemplateMAC(obj *v1alpha1.HypervisorMachineTemplate, mac string) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.Spec.MAC = mac
	return obj
}

// withTemplateRetainDiskOnDelete returns obj with the template spec
// retainDiskOnDelete flag replaced.
func withTemplateRetainDiskOnDelete(
	obj *v1alpha1.HypervisorMachineTemplate,
	retain bool,
) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.Spec.RetainDiskOnDelete = retain
	return obj
}

// withTemplateMetadata returns obj with the template resource metadata
// (labels and annotations) replaced.
func withTemplateMetadata(
	obj *v1alpha1.HypervisorMachineTemplate,
	labels, annotations map[string]string,
) *v1alpha1.HypervisorMachineTemplate {
	obj.Spec.Template.ObjectMeta.Labels = labels
	obj.Spec.Template.ObjectMeta.Annotations = annotations
	return obj
}

// withObjectLabels returns obj with the template object's own labels
// replaced.
func withObjectLabels(
	obj *v1alpha1.HypervisorMachineTemplate,
	labels map[string]string,
) *v1alpha1.HypervisorMachineTemplate {
	obj.ObjectMeta.Labels = labels
	return obj
}

// TestHypervisorMachineTemplateDefaulting pins the mutating webhook as a
// no-op: a zero-value template stays zero, a populated template keeps every
// spec field, template metadata and object metadata are preserved. Any
// non-HypervisorMachineTemplate object is rejected.
func TestHypervisorMachineTemplateDefaulting(t *testing.T) {
	tests := []struct {
		name string
		give *v1alpha1.HypervisorMachineTemplate
		want v1alpha1.HypervisorMachineTemplateSpec
	}{
		{
			name: "zero-value template is not modified",
			give: &v1alpha1.HypervisorMachineTemplate{},
			want: v1alpha1.HypervisorMachineTemplateSpec{},
		},
		{
			name: "populated template is not modified",
			give: validTemplate(),
			want: v1alpha1.HypervisorMachineTemplateSpec{
				Template: v1alpha1.HypervisorMachineTemplateResource{
					ObjectMeta: metav1.ObjectMeta{Name: "cp-1", Namespace: "lab"},
					Spec: v1alpha1.HypervisorMachineSpec{
						ClusterName: "lab-cluster",
						CPU:         2,
						RAM:         2048,
						Disk:        20480,
					},
				},
			},
		},
		{
			name: "optional fields and metadata are preserved",
			give: fullyPopulatedTemplate(),
			want: v1alpha1.HypervisorMachineTemplateSpec{
				Template: v1alpha1.HypervisorMachineTemplateResource{
					ObjectMeta: metav1.ObjectMeta{
						Name:        "cp-1",
						Namespace:   "lab",
						Labels:      map[string]string{"role": "control-plane"},
						Annotations: map[string]string{"note": "k8slab"},
					},
					Spec: v1alpha1.HypervisorMachineSpec{
						ClusterName:        "lab-cluster",
						CPU:                2,
						RAM:                2048,
						Disk:               20480,
						MAC:                "c6:e5:50:1c:ec:01",
						RetainDiskOnDelete: true,
					},
				},
			},
		},
	}
	wh := &webhook.HypervisorMachineTemplateWebhook{}
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
		err := wh.Default(t.Context(), &v1alpha1.HypervisorMachineTemplateList{})
		if err == nil {
			t.Error("Default on a HypervisorMachineTemplateList: want error, got nil")
		}
	})

	t.Run("nil object is rejected", func(t *testing.T) {
		err := wh.Default(t.Context(), nil)
		if err == nil {
			t.Error("Default on a nil object: want error, got nil")
		}
	})
}

// TestHypervisorMachineTemplateValidateCreate pins that creation is always
// allowed: a zero-value template and a fully populated template are both
// accepted, because validation is limited to immutability on update. Anything
// that is not a HypervisorMachineTemplate is rejected.
func TestHypervisorMachineTemplateValidateCreate(t *testing.T) {
	tests := []struct {
		name    string
		give    runtime.Object
		wantErr bool
	}{
		{name: "valid template", give: validTemplate(), wantErr: false},
		{name: "zero-value template is accepted", give: &v1alpha1.HypervisorMachineTemplate{}, wantErr: false},
		{name: "fully populated template is accepted", give: fullyPopulatedTemplate(), wantErr: false},
		{name: "wrong object type", give: &v1alpha1.HypervisorMachineTemplateList{}, wantErr: true},
		{name: "nil object", give: nil, wantErr: true},
	}
	wh := &webhook.HypervisorMachineTemplateWebhook{}
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

// TestHypervisorMachineTemplateValidateUpdate pins the immutability contract:
// objects whose spec.template.spec values are identical are accepted, any
// single-field change to the template spec is rejected, and changes confined
// to the template metadata or to the object's own metadata are accepted.
// Non-HypervisorMachineTemplate or nil old/new objects are rejected.
func TestHypervisorMachineTemplateValidateUpdate(t *testing.T) {
	tests := []struct {
		name    string
		oldObj  runtime.Object
		newObj  runtime.Object
		wantErr bool
	}{
		{name: "identical templates", oldObj: validTemplate(), newObj: validTemplate(), wantErr: false},
		{
			name:    "identical zero-value templates",
			oldObj:  &v1alpha1.HypervisorMachineTemplate{},
			newObj:  &v1alpha1.HypervisorMachineTemplate{},
			wantErr: false,
		},
		{
			name:   "template metadata may change",
			oldObj: validTemplate(),
			newObj: withTemplateMetadata(
				validTemplate(),
				map[string]string{"role": "control-plane"},
				map[string]string{"note": "v2"},
			),
			wantErr: false,
		},
		{
			name: "template metadata may be removed",
			oldObj: withTemplateMetadata(
				validTemplate(),
				map[string]string{"role": "control-plane"},
				map[string]string{"note": "v1"},
			),
			newObj:  validTemplate(),
			wantErr: false,
		},
		{
			name:    "template object metadata may change",
			oldObj:  validTemplate(),
			newObj:  withObjectLabels(validTemplate(), map[string]string{"tier": "control-plane"}),
			wantErr: false,
		},
		{
			name:    "clusterName change is rejected",
			oldObj:  validTemplate(),
			newObj:  withTemplateClusterName(validTemplate(), "other-cluster"),
			wantErr: true,
		},
		{name: "cpu change is rejected", oldObj: validTemplate(), newObj: withTemplateCPU(validTemplate(), 4), wantErr: true},
		{
			name:    "ram change is rejected",
			oldObj:  validTemplate(),
			newObj:  withTemplateRAM(validTemplate(), 4096),
			wantErr: true,
		},
		{
			name:    "disk change is rejected",
			oldObj:  validTemplate(),
			newObj:  withTemplateDisk(validTemplate(), 30720),
			wantErr: true,
		},
		{
			name:    "mac change is rejected",
			oldObj:  withTemplateMAC(validTemplate(), "c6:e5:50:1c:ec:01"),
			newObj:  withTemplateMAC(validTemplate(), "c6:e5:50:1c:ec:02"),
			wantErr: true,
		},
		{
			name:    "retainDiskOnDelete false to true is rejected",
			oldObj:  validTemplate(),
			newObj:  withTemplateRetainDiskOnDelete(validTemplate(), true),
			wantErr: true,
		},
		{
			name:    "retainDiskOnDelete true to false is rejected",
			oldObj:  withTemplateRetainDiskOnDelete(validTemplate(), true),
			newObj:  validTemplate(),
			wantErr: true,
		},
		{
			name:    "multiple spec changes are rejected",
			oldObj:  validTemplate(),
			newObj:  withTemplateRAM(withTemplateCPU(validTemplate(), 8), 8192),
			wantErr: true,
		},
		{
			name:    "wrong new object type",
			oldObj:  validTemplate(),
			newObj:  &v1alpha1.HypervisorMachineTemplateList{},
			wantErr: true,
		},
		{
			name:    "wrong old object type",
			oldObj:  &v1alpha1.HypervisorMachineTemplateList{},
			newObj:  validTemplate(),
			wantErr: true,
		},
		{name: "nil new object", oldObj: validTemplate(), newObj: nil, wantErr: true},
		{name: "nil old object", oldObj: nil, newObj: validTemplate(), wantErr: true},
	}
	wh := &webhook.HypervisorMachineTemplateWebhook{}
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

// TestHypervisorMachineTemplateValidateDelete pins that deletion is always
// allowed, even for zero-value templates. Any non-HypervisorMachineTemplate
// or nil object is rejected.
func TestHypervisorMachineTemplateValidateDelete(t *testing.T) {
	tests := []struct {
		name string
		give runtime.Object
	}{
		{name: "valid template", give: validTemplate()},
		{name: "zero-value template", give: &v1alpha1.HypervisorMachineTemplate{}},
	}
	wh := &webhook.HypervisorMachineTemplateWebhook{}
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
		_, err := wh.ValidateDelete(t.Context(), &v1alpha1.HypervisorMachineTemplateList{})
		if err == nil {
			t.Error("ValidateDelete on a HypervisorMachineTemplateList: want error, got nil")
		}
	})

	t.Run("nil object is rejected", func(t *testing.T) {
		_, err := wh.ValidateDelete(t.Context(), nil)
		if err == nil {
			t.Error("ValidateDelete on a nil object: want error, got nil")
		}
	})
}
