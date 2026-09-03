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
	"net/netip"

	"golang.org/x/mod/semver"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	infrav1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// +kubebuilder:webhook:path=/mutate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster,mutating=true,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters,verbs=create;update,versions=v1alpha1,name=mhypervisorcluster.kb.io,admissionReviewVersions=v1
// +kubebuilder:webhook:path=/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster,mutating=false,failurePolicy=fail,sideEffects=None,groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters,verbs=create;update,versions=v1alpha1,name=vhypervisorcluster.kb.io,admissionReviewVersions=v1

// HypervisorClusterWebhook implements the defaulting and validating admission
// webhooks for HypervisorCluster.
type HypervisorClusterWebhook struct{}

// hypervisorClusterDefaulter adapts the runtime.Object-based Defaulter
// implementation to the concrete HypervisorCluster type the webhook builder
// infers, so the non-deprecated admission.Defaulter interface can be wired
// through ctrl.NewWebhookManagedBy.
type hypervisorClusterDefaulter struct {
	*HypervisorClusterWebhook
}

// hypervisorClusterValidator adapts the runtime.Object-based Validator
// implementation to the concrete HypervisorCluster type the webhook builder
// infers, so the non-deprecated admission.Validator interface can be wired
// through ctrl.NewWebhookManagedBy.
type hypervisorClusterValidator struct {
	*HypervisorClusterWebhook
}

var (
	_ admission.Defaulter[runtime.Object] = &HypervisorClusterWebhook{}
	_ admission.Validator[runtime.Object] = &HypervisorClusterWebhook{}
)

// SetupWebhookWithManager registers the HypervisorCluster mutating and
// validating webhooks with the manager.
func (w *HypervisorClusterWebhook) SetupWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr, &infrav1.HypervisorCluster{}).
		WithDefaulter(hypervisorClusterDefaulter{HypervisorClusterWebhook: w}).
		WithValidator(hypervisorClusterValidator{HypervisorClusterWebhook: w}).
		Complete()
}

// Default sets the HypervisorCluster network defaults: an empty CIDR becomes
// 192.168.124.0/24, an empty bridge name becomes k8sbr0, and an empty NAT
// table becomes k8slab. Values already set by the user are preserved; Gateway
// and DNSIP have no defaults. Any non-HypervisorCluster object is rejected.
func (w *HypervisorClusterWebhook) Default(_ context.Context, obj runtime.Object) error {
	cluster, ok := obj.(*infrav1.HypervisorCluster)
	if !ok {
		return apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorCluster but got a %T", obj))
	}

	if cluster.Spec.Network.CIDR == "" {
		cluster.Spec.Network.CIDR = "192.168.124.0/24"
	}

	if cluster.Spec.Network.BridgeName == "" {
		cluster.Spec.Network.BridgeName = "k8sbr0"
	}

	if cluster.Spec.Network.NATTable == "" {
		cluster.Spec.Network.NATTable = "k8slab"
	}

	return nil
}

// ValidateCreate validates a HypervisorCluster on creation: the network CIDR
// must be a valid IPv4 network and the gateway, when set, must be a valid IP
// address.
func (w *HypervisorClusterWebhook) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cluster, ok := obj.(*infrav1.HypervisorCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorCluster but got a %T", obj))
	}

	return nil, validateHypervisorCluster(cluster)
}

// ValidateUpdate validates the new HypervisorCluster on update. The new
// object is held to the same rules as create, so a previously invalid object
// may be fixed by the update.
func (w *HypervisorClusterWebhook) ValidateUpdate(
	_ context.Context,
	oldObj, newObj runtime.Object,
) (admission.Warnings, error) {
	if _, ok := oldObj.(*infrav1.HypervisorCluster); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorCluster but got a %T", oldObj))
	}

	cluster, ok := newObj.(*infrav1.HypervisorCluster)
	if !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorCluster but got a %T", newObj))
	}

	return nil, validateHypervisorCluster(cluster)
}

// ValidateDelete always allows deletion of a HypervisorCluster regardless of
// its content. Any non-HypervisorCluster object is rejected.
func (w *HypervisorClusterWebhook) ValidateDelete(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	if _, ok := obj.(*infrav1.HypervisorCluster); !ok {
		return nil, apierrors.NewBadRequest(fmt.Sprintf("expected a HypervisorCluster but got a %T", obj))
	}

	return nil, nil
}

// validateHypervisorCluster validates the network CIDR and gateway of a
// HypervisorCluster, returning an Invalid error with per-field messages.
func validateHypervisorCluster(cluster *infrav1.HypervisorCluster) error {
	var allErrs field.ErrorList

	fldPath := field.NewPath("spec", "network")

	if prefix, err := netip.ParsePrefix(cluster.Spec.Network.CIDR); err != nil || !prefix.Addr().Is4() {
		allErrs = append(
			allErrs,
			field.Invalid(fldPath.Child("cidr"), cluster.Spec.Network.CIDR, "must be a valid IPv4 CIDR"),
		)
	}

	if cluster.Spec.Network.Gateway != "" {
		if _, err := netip.ParseAddr(cluster.Spec.Network.Gateway); err != nil {
			allErrs = append(
				allErrs,
				field.Invalid(fldPath.Child("gateway"), cluster.Spec.Network.Gateway, "must be a valid IP address"),
			)
		}
	}

	allErrs = append(allErrs, validateClusterImages(cluster, field.NewPath("spec", "images"))...)

	if len(allErrs) == 0 {
		return nil
	}

	return apierrors.NewInvalid(cluster.GroupVersionKind().GroupKind(), cluster.Name, allErrs)
}

// validateClusterImages validates the version-to-image map of a cluster:
// v-prefixed semver versions, unique versions, and non-empty host paths.
func validateClusterImages(cluster *infrav1.HypervisorCluster, fldPath *field.Path) field.ErrorList {
	var allErrs field.ErrorList

	seen := map[string]bool{}

	for i, image := range cluster.Spec.Images {
		imagePath := fldPath.Index(i)
		if !semver.IsValid(image.Version) {
			allErrs = append(
				allErrs,
				field.Invalid(imagePath.Child("version"), image.Version, "must be v-prefixed semver (e.g. v1.38.0)"),
			)
		}

		if seen[image.Version] {
			allErrs = append(allErrs, field.Duplicate(imagePath.Child("version"), image.Version))
		}

		seen[image.Version] = true
		if image.Path == "" {
			allErrs = append(allErrs, field.Required(imagePath.Child("path"), "must not be empty"))
		}
	}

	return allErrs
}

// Default delegates to the runtime.Object-based implementation.
func (d hypervisorClusterDefaulter) Default(ctx context.Context, obj *infrav1.HypervisorCluster) error {
	return d.HypervisorClusterWebhook.Default(ctx, obj)
}

// ValidateCreate delegates to the runtime.Object-based implementation.
func (v hypervisorClusterValidator) ValidateCreate(
	ctx context.Context,
	obj *infrav1.HypervisorCluster,
) (admission.Warnings, error) {
	return v.HypervisorClusterWebhook.ValidateCreate(ctx, obj)
}

// ValidateUpdate delegates to the runtime.Object-based implementation.
func (v hypervisorClusterValidator) ValidateUpdate(
	ctx context.Context,
	oldObj, newObj *infrav1.HypervisorCluster,
) (admission.Warnings, error) {
	return v.HypervisorClusterWebhook.ValidateUpdate(ctx, oldObj, newObj)
}

// ValidateDelete delegates to the runtime.Object-based implementation.
func (v hypervisorClusterValidator) ValidateDelete(
	ctx context.Context,
	obj *infrav1.HypervisorCluster,
) (admission.Warnings, error) {
	return v.HypervisorClusterWebhook.ValidateDelete(ctx, obj)
}
