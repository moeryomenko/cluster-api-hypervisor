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

// Contract tests for the HypervisorUpgradePlan reconciler helpers.
//
// This file pins the pure sequencing helpers the state machine builds on:
// step resolution (declared steps win, otherwise the single target version),
// current-version reporting (control plane spec wins, topology is the
// fallback), version advancement (strictly greater v-prefixed semver),
// image-map coverage (every step needs a registered base image), and step
// phase bookkeeping. The reconciler's cluster interactions (pause, patch,
// watches) are covered by the envtest suite, not here.

package controllers

import (
	"slices"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"

	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// TestResolveUpgradeSteps pins step resolution: declared steps are returned
// in order, and a plan without steps resolves to its single target version.
func TestResolveUpgradeSteps(t *testing.T) {
	tests := []struct {
		name string
		give controlplanev1alpha1.HypervisorUpgradePlanSpec
		want []string
	}{
		{
			name: "declared steps win in order",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.39.0",
				Steps:       []string{"v1.38.0", "v1.39.0"},
			},
			want: []string{"v1.38.0", "v1.39.0"},
		},
		{
			name: "no steps resolves to the target version",
			give: controlplanev1alpha1.HypervisorUpgradePlanSpec{
				ClusterName: "lab",
				ToVersion:   "v1.38.0",
			},
			want: []string{"v1.38.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			plan := &controlplanev1alpha1.HypervisorUpgradePlan{Spec: tt.give}
			if got := resolveUpgradeSteps(plan); !slices.Equal(got, tt.want) {
				t.Errorf("resolveUpgradeSteps = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCurrentClusterVersion pins the version precedence: the control plane
// spec version wins when set, otherwise the topology version is reported.
func TestCurrentClusterVersion(t *testing.T) {
	tests := []struct {
		name     string
		giveCP   string
		giveTopo string
		want     string
	}{
		{name: "control plane version wins", giveCP: "v1.37.0", giveTopo: "v1.36.0", want: "v1.37.0"},
		{name: "topology version is the fallback", giveCP: "", giveTopo: "v1.36.0", want: "v1.36.0"},
		{name: "empty when neither is set", giveCP: "", giveTopo: "", want: ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cluster := &clusterv1.Cluster{}
			cluster.Spec.Topology.Version = tt.giveTopo
			cp := &controlplanev1alpha1.HypervisorControlPlane{}

			cp.Spec.Version = tt.giveCP
			if got := currentClusterVersion(cluster, cp); got != tt.want {
				t.Errorf("currentClusterVersion = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestCheckVersionAdvance pins the advancement rule: the target must be
// strictly greater v-prefixed semver than the current version; equal,
// older, and malformed versions are rejected.
func TestCheckVersionAdvance(t *testing.T) {
	tests := []struct {
		name    string
		giveCur string
		giveTgt string
		wantErr bool
	}{
		{name: "patch advance is allowed", giveCur: "v1.37.0", giveTgt: "v1.37.1", wantErr: false},
		{name: "minor advance is allowed", giveCur: "v1.37.3", giveTgt: "v1.38.0", wantErr: false},
		{name: "equal versions are rejected", giveCur: "v1.37.0", giveTgt: "v1.37.0", wantErr: true},
		{name: "downgrade is rejected", giveCur: "v1.38.0", giveTgt: "v1.37.9", wantErr: true},
		{name: "malformed current is rejected", giveCur: "1.37.0", giveTgt: "v1.38.0", wantErr: true},
		{name: "malformed target is rejected", giveCur: "v1.37.0", giveTgt: "not-a-version", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := checkVersionAdvance(tt.giveCur, tt.giveTgt)
			if (err != nil) != tt.wantErr {
				t.Errorf("checkVersionAdvance(%q, %q) error = %v, wantErr %v",
					tt.giveCur, tt.giveTgt, err, tt.wantErr)
			}
		})
	}
}

// TestMissingImageVersions pins image-map coverage: every step needs a
// registered base image, and only the uncovered steps are reported.
func TestMissingImageVersions(t *testing.T) {
	clusterWithImages := func(versions ...string) *infrastructurev1alpha1.HypervisorCluster {
		hc := &infrastructurev1alpha1.HypervisorCluster{}
		for _, version := range versions {
			hc.Spec.Images = append(hc.Spec.Images, infrastructurev1alpha1.ImageRef{
				Version: version,
				Path:    "/var/lib/hypervisor/images/base-" + version + ".qcow2",
			})
		}

		return hc
	}

	tests := []struct {
		name  string
		give  *infrastructurev1alpha1.HypervisorCluster
		steps []string
		want  []string
	}{
		{
			name:  "all steps covered",
			give:  clusterWithImages("v1.38.0", "v1.39.0"),
			steps: []string{"v1.38.0", "v1.39.0"},
			want:  nil,
		},
		{
			name:  "uncovered steps are reported",
			give:  clusterWithImages("v1.38.0"),
			steps: []string{"v1.38.0", "v1.39.0"},
			want:  []string{"v1.39.0"},
		},
		{
			name:  "empty image map misses everything",
			give:  clusterWithImages(),
			steps: []string{"v1.38.0"},
			want:  []string{"v1.38.0"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := missingImageVersions(tt.give, tt.steps); !slices.Equal(got, tt.want) {
				t.Errorf("missingImageVersions = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestCompareSemver pins the comparison helper: ordering follows semver, and
// non-v-prefixed versions are rejected.
func TestCompareSemver(t *testing.T) {
	tests := []struct {
		name    string
		giveA   string
		giveB   string
		want    int
		wantErr bool
	}{
		{name: "less than", giveA: "v1.37.0", giveB: "v1.38.0", want: -1},
		{name: "equal", giveA: "v1.38.0", giveB: "v1.38.0", want: 0},
		{name: "greater than", giveA: "v1.39.0", giveB: "v1.38.9", want: 1},
		{name: "malformed is rejected", giveA: "v1.38.0", giveB: "not-a-version", wantErr: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := compareSemver(tt.giveA, tt.giveB)
			if (err != nil) != tt.wantErr {
				t.Fatalf("compareSemver(%q, %q) error = %v, wantErr %v",
					tt.giveA, tt.giveB, err, tt.wantErr)
			}

			if err == nil && got != tt.want {
				t.Errorf("compareSemver(%q, %q) = %d, want %d", tt.giveA, tt.giveB, got, tt.want)
			}
		})
	}
}

// TestMarkStepPhase pins the step bookkeeping: the matching step records the
// new phase, other steps are untouched, and an unknown version is a no-op.
func TestMarkStepPhase(t *testing.T) {
	newPlan := func() *controlplanev1alpha1.HypervisorUpgradePlan {
		return &controlplanev1alpha1.HypervisorUpgradePlan{
			ObjectMeta: metav1.ObjectMeta{Name: "lab-upgrade", Namespace: "lab"},
			Status: controlplanev1alpha1.HypervisorUpgradePlanStatus{
				Steps: []controlplanev1alpha1.UpgradeStepStatus{
					{Version: "v1.38.0", Phase: controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane},
					{Version: "v1.39.0", Phase: controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane},
				},
			},
		}
	}

	t.Run("matching step records the new phase", func(t *testing.T) {
		plan := newPlan()
		r := &HypervisorUpgradePlanReconciler{}
		r.markStepPhase(plan, "v1.38.0", controlplanev1alpha1.UpgradePlanPhaseRollingWorkers)

		if plan.Status.Steps[0].Phase != controlplanev1alpha1.UpgradePlanPhaseRollingWorkers {
			t.Errorf("step 0 phase = %q, want %q",
				plan.Status.Steps[0].Phase, controlplanev1alpha1.UpgradePlanPhaseRollingWorkers)
		}

		if plan.Status.Steps[1].Phase != controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane {
			t.Errorf("step 1 phase = %q, want untouched %q",
				plan.Status.Steps[1].Phase, controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane)
		}
	})

	t.Run("unknown version is a no-op", func(t *testing.T) {
		plan := newPlan()
		r := &HypervisorUpgradePlanReconciler{}
		r.markStepPhase(plan, "v1.40.0", controlplanev1alpha1.UpgradePlanPhaseCompleted)

		for _, step := range plan.Status.Steps {
			if step.Phase != controlplanev1alpha1.UpgradePlanPhaseRollingControlPlane {
				t.Errorf("step %s phase = %q, want untouched", step.Version, step.Phase)
			}
		}
	})
}
