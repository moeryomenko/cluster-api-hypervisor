/*
Copyright 2026 The cluster-api-hypervisor Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package webhook

import (
	"context"
	"fmt"
	"reflect"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachinetemplate,mutating=true,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachinetemplates,verbs=create;update,versions=v1alpha1,name=mhypervisormachinetemplate.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachinetemplate,mutating=false,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachinetemplates,verbs=create;update,versions=v1alpha1,name=vhypervisormachinetemplate.kb.io,admissionReviewVersions=v1

// HypervisorMachineTemplateWebhook implements the defaulting and validating
// admission webhooks for HypervisorMachineTemplate.
type HypervisorMachineTemplateWebhook struct{}

// hypervisorMachineTemplateDefaulter adapts the runtime.Object-based Defaulter
// implementation to the concrete HypervisorMachineTemplate type the webhook
// builder infers, so the non-deprecated admission.Defaulter interface can be
// wired through ctrl.NewWebhookManagedBy.
type hypervisorMachineTemplateDefaulter struct {
	*HypervisorMachineTemplateWebhook
}

// hypervisorMachineTemplateValidator adapts the runtime.Object-based Validator
// implementation to the concrete HypervisorMachineTemplate type the webhook
// builder infers, so the non-deprecated admission.Validator interface can be
// wired through ctrl.NewWebhookManagedBy.
type hypervisorMachineTemplateValidator struct {
	*HypervisorMachineTemplateWebhook
}

var (
	_ admission.Defaulter[runtime.Object] = &HypervisorMachineTemplateWebhook{}
	_ admission.Validator[runtime.Object] = &HypervisorMachineTemplateWebhook{}
)

// SetupWebhookWithManager registers the HypervisorMachineTemplate mutating and
// validating webhooks with the manager.
func (w *HypervisorMachineTemplateWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.HypervisorMachineTemplate{}).
		WithDefaulter(hypervisorMachineTemplateDefaulter{HypervisorMachineTemplateWebhook: w}).
		WithValidator(hypervisorMachineTemplateValidator{HypervisorMachineTemplateWebhook: w}).
		Complete()
}

// Default is a no-op: templates carry no defaultable fields, so every
// spec.template.spec value keeps its setting. Any non-HypervisorMachineTemplate
// object is rejected.
func (w *HypervisorMachineTemplateWebhook) Default(_ context.Context, obj runtime.Object) error {
	if _, ok := obj.(*infrav1.HypervisorMachineTemplate); !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachineTemplate but got a %T", obj))
	}

	return nil
}

// ValidateCreate always allows creation of a HypervisorMachineTemplate
// regardless of its content; validation is limited to immutability on update.
// Any non-HypervisorMachineTemplate object is rejected.
func (w *HypervisorMachineTemplateWebhook) ValidateCreate(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := obj.(*infrav1.HypervisorMachineTemplate); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachineTemplate but got a %T", obj))
	}

	return nil, nil
}

// ValidateUpdate enforces template immutability: any change between the old and
// the new object in spec.template.spec is rejected, while objects whose spec
// values are identical are accepted. Template metadata and the object's own
// metadata are mutable. Any non-HypervisorMachineTemplate or nil old/new object
// is rejected.
func (w *HypervisorMachineTemplateWebhook) ValidateUpdate(
	_ context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	oldTemplate, ok := oldObj.(*infrav1.HypervisorMachineTemplate)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachineTemplate but got a %T", oldObj))
	}

	newTemplate, ok := newObj.(*infrav1.HypervisorMachineTemplate)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachineTemplate but got a %T", newObj))
	}

	if !reflect.DeepEqual(oldTemplate.Spec.Template.Spec, newTemplate.Spec.Template.Spec) {
		allErrs := field.ErrorList{
			field.Invalid(
				field.NewPath("spec", "template", "spec"),
				newTemplate.Spec.Template.Spec,
				"field is immutable",
			),
		}

		return nil, apierrors.NewInvalid(
			newTemplate.GroupVersionKind().GroupKind(),
			newTemplate.Name,
			allErrs,
		)
	}

	return nil, nil
}

// ValidateDelete always allows deletion of a HypervisorMachineTemplate
// regardless of its content. Any non-HypervisorMachineTemplate object is
// rejected.
func (w *HypervisorMachineTemplateWebhook) ValidateDelete(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := obj.(*infrav1.HypervisorMachineTemplate); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachineTemplate but got a %T", obj))
	}

	return nil, nil
}

// Default delegates to the runtime.Object-based implementation.
func (d hypervisorMachineTemplateDefaulter) Default(ctx context.Context, obj *infrav1.HypervisorMachineTemplate) error {
	return d.HypervisorMachineTemplateWebhook.Default(ctx, obj)
}

// ValidateCreate delegates to the runtime.Object-based implementation.
func (v hypervisorMachineTemplateValidator) ValidateCreate(
	ctx context.Context,
	obj *infrav1.HypervisorMachineTemplate,
) (admission.Warnings, error) {
	return v.HypervisorMachineTemplateWebhook.ValidateCreate(ctx, obj)
}

// ValidateUpdate delegates to the runtime.Object-based implementation.
func (v hypervisorMachineTemplateValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj *infrav1.HypervisorMachineTemplate,
) (admission.Warnings, error) {
	return v.HypervisorMachineTemplateWebhook.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete delegates to the runtime.Object-based implementation.
func (v hypervisorMachineTemplateValidator) ValidateDelete(
	ctx context.Context,
	obj *infrav1.HypervisorMachineTemplate,
) (admission.Warnings, error) {
	return v.HypervisorMachineTemplateWebhook.ValidateDelete(ctx, obj)
}
