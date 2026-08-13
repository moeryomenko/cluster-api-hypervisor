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
	"net"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// hypervisorMachineMACPrefix is the first five octets of the lab MAC family.
// When spec.mac is set it must belong to this family; the controller derives
// the address from a stable hash when the field is left empty.
const hypervisorMachineMACPrefix = "c6:e5:50:1c:ec"

// +kubebuilder:webhook:path=/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine,mutating=true,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=create;update,versions=v1alpha1,name=mhypervisormachine.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisormachine,mutating=false,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=create;update,versions=v1alpha1,name=vhypervisormachine.kb.io,admissionReviewVersions=v1

// HypervisorMachineWebhook implements the defaulting and validating admission
// webhooks for HypervisorMachine.
type HypervisorMachineWebhook struct{}

// hypervisorMachineDefaulter adapts the runtime.Object-based Defaulter
// implementation to the concrete HypervisorMachine type the webhook builder
// infers, so the non-deprecated admission.Defaulter interface can be wired
// through ctrl.NewWebhookManagedBy.
type hypervisorMachineDefaulter struct {
	*HypervisorMachineWebhook
}

// hypervisorMachineValidator adapts the runtime.Object-based Validator
// implementation to the concrete HypervisorMachine type the webhook builder
// infers, so the non-deprecated admission.Validator interface can be wired
// through ctrl.NewWebhookManagedBy.
type hypervisorMachineValidator struct {
	*HypervisorMachineWebhook
}

var (
	_ admission.Defaulter[runtime.Object] = &HypervisorMachineWebhook{}
	_ admission.Validator[runtime.Object] = &HypervisorMachineWebhook{}
)

// SetupWebhookWithManager registers the HypervisorMachine mutating and
// validating webhooks with the manager.
func (w *HypervisorMachineWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.HypervisorMachine{}).
		WithDefaulter(hypervisorMachineDefaulter{HypervisorMachineWebhook: w}).
		WithValidator(hypervisorMachineValidator{HypervisorMachineWebhook: w}).
		Complete()
}

// Default is a no-op: every HypervisorMachine spec field keeps its value,
// including RetainDiskOnDelete whose Go zero value (false) is already the
// documented default. Any non-HypervisorMachine object is rejected.
func (w *HypervisorMachineWebhook) Default(_ context.Context, obj runtime.Object) error {
	if _, ok := obj.(*infrav1.HypervisorMachine); !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachine but got a %T", obj))
	}
	return nil
}

// ValidateCreate validates a HypervisorMachine on creation: CPU, RAM, and disk
// must be positive, and the optional MAC, when set, must be a well-formed MAC
// address belonging to the c6:e5:50:1c:ec family.
func (w *HypervisorMachineWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	machine, ok := obj.(*infrav1.HypervisorMachine)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachine but got a %T", obj))
	}
	return nil, validateHypervisorMachine(machine)
}

// ValidateUpdate validates the new HypervisorMachine on update. The new
// object is held to the same rules as create, so a previously invalid object
// may be fixed by the update.
func (w *HypervisorMachineWebhook) ValidateUpdate(
	_ context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := oldObj.(*infrav1.HypervisorMachine); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachine but got a %T", oldObj))
	}
	machine, ok := newObj.(*infrav1.HypervisorMachine)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachine but got a %T", newObj))
	}
	return nil, validateHypervisorMachine(machine)
}

// ValidateDelete always allows deletion of a HypervisorMachine regardless of
// its content. Any non-HypervisorMachine object is rejected.
func (w *HypervisorMachineWebhook) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if _, ok := obj.(*infrav1.HypervisorMachine); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorMachine but got a %T", obj))
	}
	return nil, nil
}

// validateHypervisorMachine validates the CPU, RAM, disk, and MAC of a
// HypervisorMachine, returning an Invalid error with per-field messages.
func validateHypervisorMachine(machine *infrav1.HypervisorMachine) error {
	var allErrs field.ErrorList

	fldPath := field.NewPath("spec")

	if machine.Spec.CPU <= 0 {
		allErrs = append(
			allErrs,
			field.Invalid(fldPath.Child("cpu"), machine.Spec.CPU, "must be greater than 0"),
		)
	}
	if machine.Spec.RAM <= 0 {
		allErrs = append(
			allErrs,
			field.Invalid(fldPath.Child("ram"), machine.Spec.RAM, "must be greater than 0"),
		)
	}
	if machine.Spec.Disk <= 0 {
		allErrs = append(
			allErrs,
			field.Invalid(fldPath.Child("disk"), machine.Spec.Disk, "must be greater than 0"),
		)
	}
	if machine.Spec.MAC != "" {
		parsed, err := net.ParseMAC(machine.Spec.MAC)
		if err != nil {
			allErrs = append(
				allErrs,
				field.Invalid(fldPath.Child("mac"), machine.Spec.MAC, "must be a valid MAC address"),
			)
		} else if !strings.HasPrefix(strings.ToLower(parsed.String()), hypervisorMachineMACPrefix) {
			allErrs = append(
				allErrs,
				field.Invalid(fldPath.Child("mac"), machine.Spec.MAC, "must belong to the c6:e5:50:1c:ec MAC family"),
			)
		}
	}
	if len(allErrs) == 0 {
		return nil
	}
	return apierrors.NewInvalid(machine.GroupVersionKind().GroupKind(), machine.Name, allErrs)
}

// Default delegates to the runtime.Object-based implementation.
func (d hypervisorMachineDefaulter) Default(ctx context.Context, obj *infrav1.HypervisorMachine) error {
	return d.HypervisorMachineWebhook.Default(ctx, obj)
}

// ValidateCreate delegates to the runtime.Object-based implementation.
func (v hypervisorMachineValidator) ValidateCreate(
	ctx context.Context,
	obj *infrav1.HypervisorMachine,
) (admission.Warnings, error) {
	return v.HypervisorMachineWebhook.ValidateCreate(ctx, obj)
}

// ValidateUpdate delegates to the runtime.Object-based implementation.
func (v hypervisorMachineValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj *infrav1.HypervisorMachine,
) (admission.Warnings, error) {
	return v.HypervisorMachineWebhook.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete delegates to the runtime.Object-based implementation.
func (v hypervisorMachineValidator) ValidateDelete(
	ctx context.Context,
	obj *infrav1.HypervisorMachine,
) (admission.Warnings, error) {
	return v.HypervisorMachineWebhook.ValidateDelete(ctx, obj)
}
