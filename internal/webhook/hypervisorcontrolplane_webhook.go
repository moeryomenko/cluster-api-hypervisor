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

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorcontrolplane,mutating=true,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes,verbs=create;update,versions=v1alpha1,name=mhypervisorcontrolplane.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorcontrolplane,mutating=false,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=hypervisorcontrolplanes,verbs=create;update,versions=v1alpha1,name=vhypervisorcontrolplane.kb.io,admissionReviewVersions=v1

// HypervisorControlPlaneWebhook implements the defaulting and validating
// admission webhooks for HypervisorControlPlane.
type HypervisorControlPlaneWebhook struct{}

// hypervisorControlPlaneDefaulter adapts the runtime.Object-based Defaulter
// implementation to the concrete HypervisorControlPlane type the webhook
// builder infers, so the non-deprecated admission.Defaulter interface can be
// wired through ctrl.NewWebhookManagedBy.
type hypervisorControlPlaneDefaulter struct {
	*HypervisorControlPlaneWebhook
}

// hypervisorControlPlaneValidator adapts the runtime.Object-based Validator
// implementation to the concrete HypervisorControlPlane type the webhook
// builder infers, so the non-deprecated admission.Validator interface can be
// wired through ctrl.NewWebhookManagedBy.
type hypervisorControlPlaneValidator struct {
	*HypervisorControlPlaneWebhook
}

var (
	_ admission.Defaulter[runtime.Object] = &HypervisorControlPlaneWebhook{}
	_ admission.Validator[runtime.Object] = &HypervisorControlPlaneWebhook{}
)

// SetupWebhookWithManager registers the HypervisorControlPlane mutating and
// validating webhooks with the manager.
func (w *HypervisorControlPlaneWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &controlplanev1alpha1.HypervisorControlPlane{}).
		WithDefaulter(hypervisorControlPlaneDefaulter{HypervisorControlPlaneWebhook: w}).
		WithValidator(hypervisorControlPlaneValidator{HypervisorControlPlaneWebhook: w}).
		Complete()
}

// Default is a no-op: every HypervisorControlPlane spec field keeps its value,
// including Replicas, whose default of one is applied by the API layer (CRD
// default/controller), not by the mutating webhook, so a zero-value Replicas
// stays zero. Any non-HypervisorControlPlane object is rejected.
func (w *HypervisorControlPlaneWebhook) Default(_ context.Context, obj runtime.Object) error {
	if _, ok := obj.(*controlplanev1alpha1.HypervisorControlPlane); !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorControlPlane but got a %T", obj))
	}
	return nil
}

// ValidateCreate validates a HypervisorControlPlane on creation: at least one
// replica is required, and the machine template infrastructureRef must be a
// usable reference with both kind and name set. The informational version
// accepts any value, including empty.
func (w *HypervisorControlPlaneWebhook) ValidateCreate(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	controlPlane, ok := obj.(*controlplanev1alpha1.HypervisorControlPlane)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorControlPlane but got a %T", obj))
	}
	return nil, validateHypervisorControlPlane(controlPlane)
}

// ValidateUpdate validates the new HypervisorControlPlane on update. The new
// object is held to the same rules as create, so a previously invalid object
// may be fixed by the update and scaling a live control plane down to zero
// replicas is rejected.
func (w *HypervisorControlPlaneWebhook) ValidateUpdate(
	_ context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := oldObj.(*controlplanev1alpha1.HypervisorControlPlane); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorControlPlane but got a %T", oldObj))
	}
	controlPlane, ok := newObj.(*controlplanev1alpha1.HypervisorControlPlane)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorControlPlane but got a %T", newObj))
	}
	return nil, validateHypervisorControlPlane(controlPlane)
}

// ValidateDelete always allows deletion of a HypervisorControlPlane regardless
// of its content. Any non-HypervisorControlPlane object is rejected.
func (w *HypervisorControlPlaneWebhook) ValidateDelete(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := obj.(*controlplanev1alpha1.HypervisorControlPlane); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorControlPlane but got a %T", obj))
	}
	return nil, nil
}

// validateHypervisorControlPlane validates the replicas and machine template
// infrastructureRef of a HypervisorControlPlane, returning an Invalid error
// with per-field messages.
func validateHypervisorControlPlane(controlPlane *controlplanev1alpha1.HypervisorControlPlane) error {
	var allErrs field.ErrorList

	fldPath := field.NewPath("spec")

	if controlPlane.Spec.Replicas < 1 {
		allErrs = append(
			allErrs,
			field.Invalid(fldPath.Child("replicas"), controlPlane.Spec.Replicas, "must be greater than or equal to 1"),
		)
	}
	ref := controlPlane.Spec.MachineTemplate.InfrastructureRef
	if ref.Kind == "" {
		allErrs = append(
			allErrs,
			field.Required(fldPath.Child("machineTemplate", "infrastructureRef", "kind"), "must not be empty"),
		)
	}
	if ref.Name == "" {
		allErrs = append(
			allErrs,
			field.Required(fldPath.Child("machineTemplate", "infrastructureRef", "name"), "must not be empty"),
		)
	}
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(controlPlane.GroupVersionKind().GroupKind(), controlPlane.Name, allErrs)
}

// Default delegates to the runtime.Object-based implementation.
func (d hypervisorControlPlaneDefaulter) Default(
	ctx context.Context,
	obj *controlplanev1alpha1.HypervisorControlPlane,
) error {
	return d.HypervisorControlPlaneWebhook.Default(ctx, obj)
}

// ValidateCreate delegates to the runtime.Object-based implementation.
func (v hypervisorControlPlaneValidator) ValidateCreate(
	ctx context.Context,
	obj *controlplanev1alpha1.HypervisorControlPlane,
) (admission.Warnings, error) {
	return v.HypervisorControlPlaneWebhook.ValidateCreate(ctx, obj)
}

// ValidateUpdate delegates to the runtime.Object-based implementation.
func (v hypervisorControlPlaneValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj *controlplanev1alpha1.HypervisorControlPlane,
) (admission.Warnings, error) {
	return v.HypervisorControlPlaneWebhook.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete delegates to the runtime.Object-based implementation.
func (v hypervisorControlPlaneValidator) ValidateDelete(
	ctx context.Context,
	obj *controlplanev1alpha1.HypervisorControlPlane,
) (admission.Warnings, error) {
	return v.HypervisorControlPlaneWebhook.ValidateDelete(ctx, obj)
}
