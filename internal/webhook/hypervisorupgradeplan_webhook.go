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
	"slices"

	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorupgradeplan,mutating=true,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=hypervisorupgradeplans,verbs=create;update,versions=v1alpha1,name=mhypervisorupgradeplan.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-controlplane-cluster-x-k8s-io-v1alpha1-hypervisorupgradeplan,mutating=false,failurePolicy=fail,sideEffects=None,groups=controlplane.cluster.x-k8s.io,resources=hypervisorupgradeplans,verbs=create;update;delete,versions=v1alpha1,name=vhypervisorupgradeplan.kb.io,admissionReviewVersions=v1

// HypervisorUpgradePlanWebhook implements the defaulting and validating
// admission webhooks for HypervisorUpgradePlan. The uniqueness and cluster
// existence checks run through the manager client the setup wires in.
type HypervisorUpgradePlanWebhook struct {
	Client client.Client
}

// hypervisorUpgradePlanDefaulter adapts the runtime.Object-based Defaulter
// implementation to the concrete HypervisorUpgradePlan type the webhook
// builder infers.
type hypervisorUpgradePlanDefaulter struct {
	*HypervisorUpgradePlanWebhook
}

// hypervisorUpgradePlanValidator adapts the runtime.Object-based Validator
// implementation to the concrete HypervisorUpgradePlan type the webhook
// builder infers.
type hypervisorUpgradePlanValidator struct {
	*HypervisorUpgradePlanWebhook
}

var (
	_ admission.Defaulter[runtime.Object] = &HypervisorUpgradePlanWebhook{}
	_ admission.Validator[runtime.Object] = &HypervisorUpgradePlanWebhook{}
)

// SetupWebhookWithManager registers the HypervisorUpgradePlan mutating and
// validating webhooks with the manager.
func (w *HypervisorUpgradePlanWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &controlplanev1alpha1.HypervisorUpgradePlan{}).
		WithDefaulter(hypervisorUpgradePlanDefaulter{HypervisorUpgradePlanWebhook: w}).
		WithValidator(hypervisorUpgradePlanValidator{HypervisorUpgradePlanWebhook: w}).
		Complete()
}

// Default is a no-op: every HypervisorUpgradePlan spec field keeps its value.
// Any non-HypervisorUpgradePlan object is rejected.
func (w *HypervisorUpgradePlanWebhook) Default(_ context.Context, obj runtime.Object) error {
	if _, ok := obj.(*controlplanev1alpha1.HypervisorUpgradePlan); !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorUpgradePlan but got a %T", obj))
	}

	return nil
}

// ValidateCreate validates a HypervisorUpgradePlan on creation: the versions
// must be well-formed and ordered, the referenced Cluster must exist in the
// plan's namespace, and no other active plan may target the same Cluster.
func (w *HypervisorUpgradePlanWebhook) ValidateCreate(
	ctx context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	plan, ok := obj.(*controlplanev1alpha1.HypervisorUpgradePlan)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorUpgradePlan but got a %T", obj))
	}

	if err := validateHypervisorUpgradePlan(plan); err != nil {
		return nil, err
	}

	if _, err := w.validateClusterExists(ctx, plan); err != nil {
		return nil, err
	}

	return w.validateActivePlanUniqueness(ctx, plan)
}

// ValidateUpdate validates the new HypervisorUpgradePlan on update. The spec
// is immutable: a plan is a one-shot declaration, and changing the target
// version or the steps of an in-flight plan would corrupt the sequencing
// state machine. Re-targeting means deleting and re-creating the plan, which
// also re-runs the preflight against the current cluster state.
func (w *HypervisorUpgradePlanWebhook) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	oldPlan, ok := oldObj.(*controlplanev1alpha1.HypervisorUpgradePlan)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorUpgradePlan but got a %T", oldObj))
	}

	plan, ok := newObj.(*controlplanev1alpha1.HypervisorUpgradePlan)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorUpgradePlan but got a %T", newObj))
	}

	if err := validateHypervisorUpgradePlan(plan); err != nil {
		return nil, err
	}

	if oldPlan.Spec.ClusterName != plan.Spec.ClusterName ||
		oldPlan.Spec.ToVersion != plan.Spec.ToVersion ||
		!slices.Equal(oldPlan.Spec.Steps, plan.Spec.Steps) {
		return nil, apierrors.NewInvalid(
			plan.GroupVersionKind().GroupKind(),
			plan.Name,
			field.ErrorList{field.Forbidden(field.NewPath("spec"), "is immutable; delete and re-create the plan to re-target")},
		)
	}

	return w.validateActivePlanUniqueness(ctx, plan)
}

// ValidateDelete always allows deletion of a HypervisorUpgradePlan regardless
// of its content. Any non-HypervisorUpgradePlan object is rejected.
func (w *HypervisorUpgradePlanWebhook) ValidateDelete(
	_ context.Context,
	obj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := obj.(*controlplanev1alpha1.HypervisorUpgradePlan); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorUpgradePlan but got a %T", obj))
	}

	return nil, nil
}

// validateHypervisorUpgradePlan validates the version fields of a plan:
// v-prefixed semver for the target version and every step, strictly
// increasing steps, and a final step that equals the target version.
func validateHypervisorUpgradePlan(plan *controlplanev1alpha1.HypervisorUpgradePlan) error {
	var allErrs field.ErrorList

	fldPath := field.NewPath("spec")

	if plan.Spec.ClusterName == "" {
		allErrs = append(allErrs, field.Required(fldPath.Child("clusterName"), "must not be empty"))
	}

	if !validVersion(plan.Spec.ToVersion) {
		allErrs = append(allErrs, field.Invalid(fldPath.Child("toVersion"), plan.Spec.ToVersion, notSemverMessage))
	}

	previous := ""

	for i, step := range plan.Spec.Steps {
		stepPath := fldPath.Child("steps").Index(i)
		if !validVersion(step) {
			allErrs = append(allErrs, field.Invalid(stepPath, step, notSemverMessage))
			continue
		}

		if previous != "" && semver.Compare(step, previous) <= 0 {
			allErrs = append(allErrs, field.Invalid(stepPath, step, "must be strictly greater than the previous step"))
		}

		previous = step
	}

	if len(plan.Spec.Steps) > 0 && len(allErrs) == 0 &&
		semver.Compare(plan.Spec.Steps[len(plan.Spec.Steps)-1], plan.Spec.ToVersion) != 0 {
		allErrs = append(allErrs, field.Invalid(
			fldPath.Child("steps"),
			plan.Spec.Steps,
			"the last step must equal spec.toVersion",
		))
	}

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(plan.GroupVersionKind().GroupKind(), plan.Name, allErrs)
}

// validateActivePlanUniqueness rejects a plan when another active
// (non-terminal) plan targets the same Cluster in the same namespace. The
// object being updated skips itself by name.
func (w *HypervisorUpgradePlanWebhook) validateActivePlanUniqueness(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
) (admission.Warnings, error) {
	if w.Client == nil || plan.Spec.ClusterName == "" {
		return nil, nil
	}

	plans := &controlplanev1alpha1.HypervisorUpgradePlanList{}
	if err := w.Client.List(ctx, plans, client.InNamespace(plan.Namespace)); err != nil {
		return nil, fmt.Errorf("list HypervisorUpgradePlans in %q: %w", plan.Namespace, err)
	}

	for i := range plans.Items {
		other := &plans.Items[i]
		if other.Name == plan.Name || other.Spec.ClusterName != plan.Spec.ClusterName {
			continue
		}

		if other.Status.Phase.Terminal() {
			continue
		}

		return nil, apierrors.NewConflict(
			schema.GroupResource{Group: controlplanev1alpha1.GroupVersion.Group, Resource: "hypervisorupgradeplans"},
			plan.Name,
			fmt.Errorf("plan %q is already upgrading Cluster %q", other.Name, plan.Spec.ClusterName),
		)
	}

	return nil, nil
}

// validateClusterExists rejects a plan whose target Cluster does not exist in
// the plan's namespace.
func (w *HypervisorUpgradePlanWebhook) validateClusterExists(
	ctx context.Context,
	plan *controlplanev1alpha1.HypervisorUpgradePlan,
) (admission.Warnings, error) {
	if w.Client == nil || plan.Spec.ClusterName == "" {
		return nil, nil
	}

	cluster := &clusterv1.Cluster{}

	key := client.ObjectKey{Namespace: plan.Namespace, Name: plan.Spec.ClusterName}
	if err := w.Client.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, apierrors.NewInvalid(
				plan.GroupVersionKind().GroupKind(),
				plan.Name,
				field.ErrorList{field.Invalid(
					field.NewPath("spec", "clusterName"),
					plan.Spec.ClusterName,
					fmt.Sprintf("Cluster %q not found in namespace %q", plan.Spec.ClusterName, plan.Namespace),
				)},
			)
		}

		return nil, fmt.Errorf("get Cluster %q: %w", key, err)
	}

	return nil, nil
}

// notSemverMessage is the shared message for a version field that is not
// v-prefixed semver.
const notSemverMessage = "must be v-prefixed semver (e.g. v1.38.0)"

// validVersion reports whether version is v-prefixed valid semver.
func validVersion(version string) bool {
	return semver.IsValid(version)
}

// Default delegates to the runtime.Object-based implementation.
func (d hypervisorUpgradePlanDefaulter) Default(
	ctx context.Context,
	obj *controlplanev1alpha1.HypervisorUpgradePlan,
) error {
	return d.HypervisorUpgradePlanWebhook.Default(ctx, obj)
}

// ValidateCreate delegates to the runtime.Object-based implementation.
func (v hypervisorUpgradePlanValidator) ValidateCreate(
	ctx context.Context,
	obj *controlplanev1alpha1.HypervisorUpgradePlan,
) (admission.Warnings, error) {
	return v.HypervisorUpgradePlanWebhook.ValidateCreate(ctx, obj)
}

// ValidateUpdate delegates to the runtime.Object-based implementation.
func (v hypervisorUpgradePlanValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj *controlplanev1alpha1.HypervisorUpgradePlan,
) (admission.Warnings, error) {
	return v.HypervisorUpgradePlanWebhook.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete delegates to the runtime.Object-based implementation.
func (v hypervisorUpgradePlanValidator) ValidateDelete(
	ctx context.Context,
	obj *controlplanev1alpha1.HypervisorUpgradePlan,
) (admission.Warnings, error) {
	return v.HypervisorUpgradePlanWebhook.ValidateDelete(ctx, obj)
}
