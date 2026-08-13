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

// envtest suite (spec section 4, VC-01: all five CRDs install against the
// management apiserver with correct group/kind/version; REQ-002).
//
// This suite is test-first (task 14) and depends on the envtest harness
// contract that task 15 implements in test/helpers/envtest.go. Until that
// file exists this package does not compile; the intended red phase failure
// is "undefined: helpers.StartEnvTest" / "no non-test Go files in
// ...test/helpers".
//
// The harness owns the control-plane lifecycle: it starts envtest with the
// k8s 1.35.x binaries resolved by setup-envtest (KUBEBUILDER_ASSETS), loads
// and installs the five CRDs from config/crd/bases, registers the scheme
// (clientgoscheme plus the three provider api groups), and stops the control
// plane when the test completes. This suite only exercises the contract:
// creating, reading back, and deleting one object of each kind through the
// envtest client is the load/install assertion.
package controllers

import (
	"context"
	"fmt"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/test/helpers"
)

// TestEnvtestCRDsAndScheme starts the envtest control plane through the
// harness and proves the load/install contract for every kind shipped by the
// provider set: the CRD is installed (create succeeds), the registered scheme
// round-trips the object (get returns the submitted spec), and the object can
// be deleted (delete then get reports NotFound).
func TestEnvtestCRDsAndScheme(t *testing.T) {
	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}
	if envTest.Env == nil {
		t.Fatalf("helpers.StartEnvTest returned a nil Env")
	}
	if envTest.Client == nil {
		t.Fatalf("helpers.StartEnvTest returned a nil Client")
	}

	const namespace = "envtest-contract"

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}
	if err := envTest.Client.Create(ctx, ns); err != nil {
		t.Fatalf("create test namespace %q: %v", namespace, err)
	}
	t.Cleanup(func() {
		// Best-effort; the harness stops the control plane in its own cleanup.
		cctx, ccancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer ccancel()
		_ = envTest.Client.Delete(cctx, ns)
	})

	cases := []struct {
		name   string
		obj    client.Object
		verify func(client.Object) error
	}{
		{
			name: "HypervisorCluster",
			obj: &infrastructurev1alpha1.HypervisorCluster{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-cluster", Namespace: namespace},
				Spec: infrastructurev1alpha1.HypervisorClusterSpec{
					ClusterName: "sample-cluster",
					Network: infrastructurev1alpha1.HypervisorClusterNetworkSpec{
						CIDR:       "192.168.124.0/24",
						Gateway:    "192.168.124.1",
						DNSIP:      "192.168.124.1",
						BridgeName: "k8sbr0",
						NATTable:   "k8slab",
					},
				},
			},
			verify: func(o client.Object) error {
				got := o.(*infrastructurev1alpha1.HypervisorCluster)
				if got.Spec.Network.CIDR != "192.168.124.0/24" {
					return fmt.Errorf("spec.network.cidr = %q, want %q", got.Spec.Network.CIDR, "192.168.124.0/24")
				}
				return nil
			},
		},
		{
			name: "HypervisorMachine",
			obj: &infrastructurev1alpha1.HypervisorMachine{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-machine", Namespace: namespace},
				Spec: infrastructurev1alpha1.HypervisorMachineSpec{
					ClusterName: "sample-cluster",
					CPU:         2,
					RAM:         4096,
					Disk:        20480,
				},
			},
			verify: func(o client.Object) error {
				got := o.(*infrastructurev1alpha1.HypervisorMachine)
				if got.Spec.CPU != 2 {
					return fmt.Errorf("spec.cpu = %d, want %d", got.Spec.CPU, 2)
				}
				return nil
			},
		},
		{
			name: "HypervisorMachineTemplate",
			obj: &infrastructurev1alpha1.HypervisorMachineTemplate{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-machinetemplate", Namespace: namespace},
				Spec: infrastructurev1alpha1.HypervisorMachineTemplateSpec{
					Template: infrastructurev1alpha1.HypervisorMachineTemplateResource{
						Spec: infrastructurev1alpha1.HypervisorMachineSpec{
							ClusterName: "sample-cluster",
							CPU:         4,
							RAM:         8192,
							Disk:        30720,
						},
					},
				},
			},
			verify: func(o client.Object) error {
				got := o.(*infrastructurev1alpha1.HypervisorMachineTemplate)
				if got.Spec.Template.Spec.CPU != 4 {
					return fmt.Errorf("spec.template.spec.cpu = %d, want %d", got.Spec.Template.Spec.CPU, 4)
				}
				return nil
			},
		},
		{
			name: "HypervisorConfig",
			obj: &bootstrapv1alpha1.HypervisorConfig{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-config", Namespace: namespace},
				Spec: bootstrapv1alpha1.HypervisorConfigSpec{
					ClusterName: "sample-cluster",
					Role:        "worker",
					NodeName:    "sample-worker",
				},
			},
			verify: func(o client.Object) error {
				got := o.(*bootstrapv1alpha1.HypervisorConfig)
				if got.Spec.Role != "worker" {
					return fmt.Errorf("spec.role = %q, want %q", got.Spec.Role, "worker")
				}
				return nil
			},
		},
		{
			name: "HypervisorControlPlane",
			obj: &controlplanev1alpha1.HypervisorControlPlane{
				ObjectMeta: metav1.ObjectMeta{Name: "sample-controlplane", Namespace: namespace},
				Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
					Replicas: 1,
					Version:  "v1.35.4",
					MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
						InfrastructureRef: corev1.ObjectReference{
							APIVersion: "infrastructure.cluster.x-k8s.io/v1alpha1",
							Kind:       "HypervisorMachineTemplate",
							Name:       "sample-machinetemplate",
							Namespace:  namespace,
						},
					},
				},
			},
			verify: func(o client.Object) error {
				got := o.(*controlplanev1alpha1.HypervisorControlPlane)
				if got.Spec.Replicas != 1 {
					return fmt.Errorf("spec.replicas = %d, want %d", got.Spec.Replicas, 1)
				}
				return nil
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assertCRUD(t, envTest.Client, tc.obj, tc.verify)
		})
	}
}

// assertCRUD proves one kind satisfies the load/install contract: create
// succeeds (CRD installed and reachable), get returns the submitted object
// through the registered scheme, delete succeeds, and a subsequent get
// reports NotFound.
func assertCRUD(t *testing.T, c client.Client, obj client.Object, verify func(client.Object) error) {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	key := client.ObjectKeyFromObject(obj)

	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("Create %s: %v", key, err)
	}

	got := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, key, got); err != nil {
		t.Fatalf("Get %s: %v", key, err)
	}
	if err := verify(got); err != nil {
		t.Fatalf("Get %s round-trip: %v", key, err)
	}

	if err := c.Delete(ctx, obj); err != nil {
		t.Fatalf("Delete %s: %v", key, err)
	}

	gone := obj.DeepCopyObject().(client.Object)
	if err := c.Get(ctx, key, gone); !apierrors.IsNotFound(err) {
		t.Fatalf("Get %s after Delete: want NotFound, got %v", key, err)
	}
}
