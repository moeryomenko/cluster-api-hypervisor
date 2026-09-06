// Contract tests for the HypervisorUpgradePlan defaulting and validation
// webhook.
//
// This file pins the exact behavior the webhook in this package must
// implement. Defaulting is a no-op: every spec field keeps its value. Create
// validation requires a non-empty cluster name, v-prefixed semver for the
// target version and every step, strictly increasing steps, and a last step
// equal to the target version; the referenced Cluster must exist in the
// plan's namespace and no other active plan may target the same Cluster.
// Update validation re-runs the structural rules and rejects any spec change
// (the spec is immutable: re-targeting means delete and re-create). Deletion
// is always allowed. The client-backed checks (cluster existence, active-plan
// uniqueness) are skipped when the webhook carries no client, so structural
// cases run with a nil client and the client-backed cases use a fake client.

package webhook_test

import (
	"reflect"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/webhook"
)

// Compile-time pins: HypervisorUpgradePlanWebhook must satisfy the
// controller-runtime defaulter and validator interfaces.
var (
	_ admission.Defaulter[runtime.Object] = &webhook.HypervisorUpgradePlanWebhook{}
	_ admission.Validator[runtime.Object] = &webhook.HypervisorUpgradePlanWebhook{}
)

// validUpgradePlan returns a HypervisorUpgradePlan that satisfies every
// pinned validation rule: a cluster name, a v-prefixed semver target, and
// strictly increasing steps ending at the target.
func validUpgradePlan() *controlplanev1alpha1.HypervisorUpgradePlan {
	return &controlplanev1alpha1.HypervisorUpgradePlan{
		ObjectMeta: metav1.ObjectMeta{Name: "lab-upgrade", Namespace: "lab"},
		Spec: controlplanev1alpha1.HypervisorUpgradePlanSpec{
			ClusterName: "lab",
			ToVersion:   "v1.38.0",
			Steps:       []string{"v1.38.0"},
		},
	}
}

// withSpec returns obj with the spec replaced. Callers pass a fresh
// validUpgradePlan() so table rows never share mutable state.
func withSpec(
	obj *controlplanev1alpha1.HypervisorUpgradePlan,
	spec controlplanev1alpha1.HypervisorUpgradePlanSpec,
) *controlplanev1alpha1.HypervisorUpgradePlan {
	obj.Spec = spec
	return obj
}

// upgradePlanScheme returns a scheme carrying the plan and Cluster types for
// the fake client.
func upgradePlanScheme(t *testing.T) *runtime.Scheme {
	t.Helper()

	scheme := runtime.NewScheme()
	if err := controlplanev1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme controlplane: %v", err)
	}

	if err := clusterv1.AddToScheme(scheme); err != nil {
		t.Fatalf("AddToScheme cluster-api: %v", err)
	}

	return scheme
}

// TestHypervisorUpgradePlanDefaulting pins the mutating webhook as a no-op:
// a populated plan keeps every field, and any non-HypervisorUpgradePlan
// object is rejected.
func TestHypervisorUpgradePlanDefaulting(t *testing.T) {
	tests := []struct {
		name string
		give *controlplanev1alpha1.HypervisorUpgradePlan
		want controlplanev1alpha1.HypervisorUpgradePlanSpec
	}{
		{
			name: "populated plan is not modified",
			give: validUpgradePlan(),
			want: validUpgradePlan().Spec,
		},
		{
			name: "zero-value plan is not modified",
			give: &controlplanev1alpha1.HypervisorUpgradePlan{},
			want: controlplanev1alpha1.HypervisorUpgradePlanSpec{},
		},
	}
	wh := &webhook.HypervisorUpgradePlanWebhook{}

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
		err := wh.Default(t.Context(), &controlplanev1alpha1.HypervisorUpgradePlanList{})
		if err == nil {
			t.Error("Default on a HypervisorUpgradePlanList: want error, got nil")
		}
	})
}

// TestHypervisorUpgradePlanValidateCreateStructure pins the structural create
// rules with a clientless webhook (cluster existence and uniqueness are
// skipped without a client): cluster name required, v-prefixed semver for the
// target and every step, strictly increasing steps, last step equal to the
// target.
func TestHypervisorUpgradePlanValidateCreateStructure(t *testing.T) {
	base := controlplanev1alpha1.HypervisorUpgradePlanSpec{
		ClusterName: "lab",
		ToVersion:   "v1.39.0",
		Steps:       []string{"v1.38.0", "v1.39.0"},
	}
	tests := []struct {
		name    string
		give    controlplanev1alpha1.HypervisorUpgradePlanSpec
		wantErr bool
	}{
		{name: "multi-step plan is accepted", give: base, wantErr: false},
		{
			name: "single target without steps is accepted",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.38.0",
			},
			wantErr: false,
		},
		{
			name: "empty cluster name is rejected",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ToVersion: "v1.38.0",
			},
			wantErr: true,
		},
		{
			name: "non-semver target is rejected",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "1.38.0",
			},
			wantErr: true,
		},
		{
			name: "non-semver step is rejected",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.39.0",
				Steps:       []string{"v1.38", "soon"},
			},
			wantErr: true,
		},
		{
			name: "non-increasing steps are rejected",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.39.0",
				Steps:       []string{"v1.39.0", "v1.38.0"},
			},
			wantErr: true,
		},
		{
			name: "duplicate steps are rejected",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.38.0",
				Steps:       []string{"v1.38.0", "v1.38.0"},
			},
			wantErr: true,
		},
		{
			name: "last step must equal the target",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.39.0",
				Steps:       []string{"v1.38.0"},
			},
			wantErr: true,
		},
	}
	wh := &webhook.HypervisorUpgradePlanWebhook{}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := wh.ValidateCreate(t.Context(), withSpec(validUpgradePlan(), tt.give))
			if (err != nil) != tt.wantErr {
				t.Errorf("ValidateCreate error = %v, wantErr %v", err, tt.wantErr)
			}
		})
	}

	t.Run("wrong object type is rejected", func(t *testing.T) {
		_, err := wh.ValidateCreate(t.Context(), &controlplanev1alpha1.HypervisorUpgradePlanList{})
		if err == nil {
			t.Error("ValidateCreate on a HypervisorUpgradePlanList: want error, got nil")
		}
	})
}

// TestHypervisorUpgradePlanValidateCreateClient pins the client-backed create
// rules against a fake client: a missing Cluster is rejected, and a second
// active plan for the same Cluster conflicts while a terminal plan does not
// block.
func TestHypervisorUpgradePlanValidateCreateClient(t *testing.T) {
	newCluster := func() *clusterv1.Cluster {
		return &clusterv1.Cluster{
			ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "lab"},
		}
	}

	t.Run("missing cluster is rejected", func(t *testing.T) {
		wh := &webhook.HypervisorUpgradePlanWebhook{
			Client: fake.NewClientBuilder().WithScheme(upgradePlanScheme(t)).Build(),
		}

		_, err := wh.ValidateCreate(t.Context(), validUpgradePlan())
		if err == nil {
			t.Error("ValidateCreate with no Cluster: want error, got nil")
		}
	})

	t.Run("second active plan conflicts", func(t *testing.T) {
		existing := validUpgradePlan()
		existing.Name = "lab-upgrade-first"
		wh := &webhook.HypervisorUpgradePlanWebhook{
			Client: fake.NewClientBuilder().WithScheme(upgradePlanScheme(t)).
				WithObjects(newCluster(), existing).Build(),
		}

		_, err := wh.ValidateCreate(t.Context(), validUpgradePlan())
		if err == nil {
			t.Error("ValidateCreate with an active plan: want conflict, got nil")
		}
	})

	t.Run("terminal plan does not block", func(t *testing.T) {
		existing := validUpgradePlan()
		existing.Name = "lab-upgrade-done"
		existing.Status.Phase = controlplanev1alpha1.UpgradePlanPhaseCompleted

		wh := &webhook.HypervisorUpgradePlanWebhook{
			Client: fake.NewClientBuilder().WithScheme(upgradePlanScheme(t)).
				WithObjects(newCluster(), existing).Build(),
		}
		if _, err := wh.ValidateCreate(t.Context(), validUpgradePlan()); err != nil {
			t.Errorf("ValidateCreate with only a terminal plan: want nil, got %v", err)
		}
	})

	t.Run("plan for another cluster does not block", func(t *testing.T) {
		existing := validUpgradePlan()
		existing.Name = "other-upgrade"
		existing.Spec.ClusterName = "other"
		other := newCluster()
		other.Name = "other"

		wh := &webhook.HypervisorUpgradePlanWebhook{
			Client: fake.NewClientBuilder().WithScheme(upgradePlanScheme(t)).
				WithObjects(newCluster(), other, existing).Build(),
		}
		if _, err := wh.ValidateCreate(t.Context(), validUpgradePlan()); err != nil {
			t.Errorf("ValidateCreate with a plan for another cluster: want nil, got %v", err)
		}
	})
}

// TestHypervisorUpgradePlanValidateUpdate pins spec immutability: any change
// to cluster name, target version, or steps is rejected, while a status-only
// change (the object the controller writes back) is accepted. Deletion is
// always allowed.
func TestHypervisorUpgradePlanValidateUpdate(t *testing.T) {
	wh := &webhook.HypervisorUpgradePlanWebhook{}

	t.Run("identical spec is accepted", func(t *testing.T) {
		if _, err := wh.ValidateUpdate(t.Context(), validUpgradePlan(), validUpgradePlan()); err != nil {
			t.Errorf("ValidateUpdate identical: want nil, got %v", err)
		}
	})

	t.Run("retargeting the version is rejected", func(t *testing.T) {
		old := validUpgradePlan()
		give := validUpgradePlan()
		give.Spec.ToVersion = "v1.39.0"

		give.Spec.Steps = []string{"v1.39.0"}
		if _, err := wh.ValidateUpdate(t.Context(), old, give); err == nil {
			t.Error("ValidateUpdate retarget: want error, got nil")
		}
	})

	t.Run("changing steps is rejected", func(t *testing.T) {
		old := validUpgradePlan()
		give := validUpgradePlan()

		give.Spec.Steps = []string{"v1.37.0", "v1.38.0"}
		if _, err := wh.ValidateUpdate(t.Context(), old, give); err == nil {
			t.Error("ValidateUpdate steps change: want error, got nil")
		}
	})

	t.Run("changing the cluster is rejected", func(t *testing.T) {
		old := validUpgradePlan()
		give := validUpgradePlan()

		give.Spec.ClusterName = "other"
		if _, err := wh.ValidateUpdate(t.Context(), old, give); err == nil {
			t.Error("ValidateUpdate cluster change: want error, got nil")
		}
	})

	t.Run("structurally invalid new spec is rejected", func(t *testing.T) {
		old := validUpgradePlan()
		give := validUpgradePlan()

		give.Spec.ToVersion = "soon"
		if _, err := wh.ValidateUpdate(t.Context(), old, give); err == nil {
			t.Error("ValidateUpdate invalid spec: want error, got nil")
		}
	})

	t.Run("wrong object type is rejected", func(t *testing.T) {
		_, err := wh.ValidateUpdate(
			t.Context(),
			&controlplanev1alpha1.HypervisorUpgradePlanList{},
			validUpgradePlan(),
		)
		if err == nil {
			t.Error("ValidateUpdate on a HypervisorUpgradePlanList: want error, got nil")
		}
	})

	t.Run("deletion is always allowed", func(t *testing.T) {
		if _, err := wh.ValidateDelete(t.Context(), validUpgradePlan()); err != nil {
			t.Errorf("ValidateDelete: want nil, got %v", err)
		}
	})

	t.Run("delete of wrong object type is rejected", func(t *testing.T) {
		_, err := wh.ValidateDelete(t.Context(), &controlplanev1alpha1.HypervisorUpgradePlanList{})
		if err == nil {
			t.Error("ValidateDelete on a HypervisorUpgradePlanList: want error, got nil")
		}
	})
}

// TestHypervisorUpgradePlanUniquenessSkipsSelf pins that the update path does
// not conflict with the plan itself.
func TestHypervisorUpgradePlanUniquenessSkipsSelf(t *testing.T) {
	existing := validUpgradePlan()

	wh := &webhook.HypervisorUpgradePlanWebhook{
		Client: fake.NewClientBuilder().WithScheme(upgradePlanScheme(t)).
			WithObjects(
				&clusterv1.Cluster{ObjectMeta: metav1.ObjectMeta{Name: "lab", Namespace: "lab"}},
				existing,
			).Build(),
	}
	if _, err := wh.ValidateUpdate(t.Context(), validUpgradePlan(), validUpgradePlan()); err != nil {
		t.Errorf("ValidateUpdate self: want nil, got %v", err)
	}
}
