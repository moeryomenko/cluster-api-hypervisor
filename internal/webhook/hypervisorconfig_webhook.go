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

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig,mutating=true,failurePolicy=fail,sideEffects=None,groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=create;update,versions=v1alpha1,name=mhypervisorconfig.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-bootstrap-cluster-x-k8s-io-v1alpha1-hypervisorconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=create;update,versions=v1alpha1,name=vhypervisorconfig.kb.io,admissionReviewVersions=v1

// HypervisorConfigWebhook implements the defaulting and validating admission
// webhooks for HypervisorConfig.
type HypervisorConfigWebhook struct{}

// hypervisorConfigDefaulter adapts the runtime.Object-based Defaulter
// implementation to the concrete HypervisorConfig type the webhook builder
// infers, so the non-deprecated admission.Defaulter interface can be wired
// through ctrl.NewWebhookManagedBy.
type hypervisorConfigDefaulter struct {
	*HypervisorConfigWebhook
}

// hypervisorConfigValidator adapts the runtime.Object-based Validator
// implementation to the concrete HypervisorConfig type the webhook builder
// infers, so the non-deprecated admission.Validator interface can be wired
// through ctrl.NewWebhookManagedBy.
type hypervisorConfigValidator struct {
	*HypervisorConfigWebhook
}

var (
	_ admission.Defaulter[runtime.Object] = &HypervisorConfigWebhook{}
	_ admission.Validator[runtime.Object] = &HypervisorConfigWebhook{}
)

// SetupWebhookWithManager registers the HypervisorConfig mutating and
// validating webhooks with the manager.
func (w *HypervisorConfigWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &bootstrapv1alpha1.HypervisorConfig{}).
		WithDefaulter(hypervisorConfigDefaulter{HypervisorConfigWebhook: w}).
		WithValidator(hypervisorConfigValidator{HypervisorConfigWebhook: w}).
		Complete()
}

// Default fills spec.nodeName with metadata.name when it is empty: a
// HypervisorConfig is per-machine and named after its Machine, so an unset
// node name defaults to the object name. Every other spec field keeps its
// value, including the optional SSH public key. Any non-HypervisorConfig
// object is rejected.
func (w *HypervisorConfigWebhook) Default(_ context.Context, obj runtime.Object) error {
	config, ok := obj.(*bootstrapv1alpha1.HypervisorConfig)
	if !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorConfig but got a %T", obj))
	}

	if config.Spec.NodeName == "" {
		config.Spec.NodeName = config.Name
	}

	return nil
}

// ValidateCreate validates a HypervisorConfig on creation: the role must be
// exactly "control-plane" or "worker", and the node and cluster names must be
// non-empty. The optional SSH public key is not validated.
func (w *HypervisorConfigWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	config, ok := obj.(*bootstrapv1alpha1.HypervisorConfig)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorConfig but got a %T", obj))
	}

	return nil, validateHypervisorConfig(config)
}

// ValidateUpdate validates the new HypervisorConfig on update. The new object
// is held to the same rules as create, so a previously invalid object may be
// fixed by the update.
func (w *HypervisorConfigWebhook) ValidateUpdate(
	_ context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := oldObj.(*bootstrapv1alpha1.HypervisorConfig); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorConfig but got a %T", oldObj))
	}

	config, ok := newObj.(*bootstrapv1alpha1.HypervisorConfig)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorConfig but got a %T", newObj))
	}

	return nil, validateHypervisorConfig(config)
}

// ValidateDelete always allows deletion of a HypervisorConfig regardless of
// its content. Any non-HypervisorConfig object is rejected.
func (w *HypervisorConfigWebhook) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if _, ok := obj.(*bootstrapv1alpha1.HypervisorConfig); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorConfig but got a %T", obj))
	}

	return nil, nil
}

// validateHypervisorConfig validates the role, node name, and cluster name of
// a HypervisorConfig, returning an Invalid error with per-field messages.
func validateHypervisorConfig(config *bootstrapv1alpha1.HypervisorConfig) error {
	var allErrs field.ErrorList

	fldPath := field.NewPath("spec")

	if config.Spec.Role != "control-plane" && config.Spec.Role != "worker" {
		allErrs = append(
			allErrs,
			field.NotSupported(fldPath.Child("role"), config.Spec.Role, []string{"control-plane", "worker"}),
		)
	}

	if config.Spec.NodeName == "" {
		allErrs = append(
			allErrs,
			field.Required(fldPath.Child("nodeName"), "must not be empty"),
		)
	}

	if config.Spec.ClusterName == "" {
		allErrs = append(
			allErrs,
			field.Required(fldPath.Child("clusterName"), "must not be empty"),
		)
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(config.GroupVersionKind().GroupKind(), config.Name, allErrs)
}

// Default delegates to the runtime.Object-based implementation.
func (d hypervisorConfigDefaulter) Default(ctx context.Context, obj *bootstrapv1alpha1.HypervisorConfig) error {
	return d.HypervisorConfigWebhook.Default(ctx, obj)
}

// ValidateCreate delegates to the runtime.Object-based implementation.
func (v hypervisorConfigValidator) ValidateCreate(
	ctx context.Context,
	obj *bootstrapv1alpha1.HypervisorConfig,
) (admission.Warnings, error) {
	return v.HypervisorConfigWebhook.ValidateCreate(ctx, obj)
}

// ValidateUpdate delegates to the runtime.Object-based implementation.
func (v hypervisorConfigValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj *bootstrapv1alpha1.HypervisorConfig,
) (admission.Warnings, error) {
	return v.HypervisorConfigWebhook.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete delegates to the runtime.Object-based implementation.
func (v hypervisorConfigValidator) ValidateDelete(
	ctx context.Context,
	obj *bootstrapv1alpha1.HypervisorConfig,
) (admission.Warnings, error) {
	return v.HypervisorConfigWebhook.ValidateDelete(ctx, obj)
}
