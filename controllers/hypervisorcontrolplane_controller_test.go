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

// HypervisorControlPlane controller contract, machine-creation portion
// (test-first, red).
//
// This suite pins the machine-creation contract for the control-plane
// reconciler: it creates one Machine per replica carrying the control-plane
// role label, wires each Machine's bootstrap ref to a generated
// HypervisorConfig, creates the cluster-scoped PKI Secret on the first
// replica, propagates the machineTemplate metadata labels, and names Machines
// deterministically. The reconciler is exercised through the committed envtest
// harness with recording seams standing in for the config generation, the
// Machine persistence, and the PKI generation, so the API-store side effects
// are real envtest objects while every injectable dependency is recorded.
//
// The contract, in prose:
//
//   - HypervisorControlPlaneReconciler carries the controller-runtime wiring
//     (embedded client.Client, Scheme, Recorder) plus three injectable
//     dependencies: NewConfig (builds the per-machine bootstrap
//     HypervisorConfig), CreateMachine (persists the per-replica CAPI
//     Machine), and GeneratePKI (produces the cluster-scoped PKI material
//     stored on the first replica). The tests build every dependency over a
//     recording seam and hand the fully constructed reconciler to the
//     controller. The CreateMachine seam delegates to the real envtest client,
//     so the persisted Machines are genuine API objects, while NewConfig and
//     GeneratePKI return deterministic canned objects.
//   - Reconcile resolves the object, then the linked CAPI Cluster: the
//     Cluster whose spec.controlPlaneRef names this HypervisorControlPlane. A
//     missing object is a no-op; a control plane with no linked Cluster is
//     left untouched — no Machine, no HypervisorConfig, no PKI Secret, no
//     seam invocation — and reconcile returns no error.
//   - Desired replica count is spec.replicas, treated as one when unset.
//     For each replica the controller reconciles the deterministic Machine
//     name <control-plane-name>-<index> (index starting at zero); a Machine
//     that already exists is skipped, so a second reconcile creates nothing.
//   - Each Machine carries the standard cluster-name label (the linked
//     Cluster's name) and the control-plane role label with its conventional
//     empty value, plus every label from spec.machineTemplate.metadata; its
//     spec.clusterName is the linked Cluster's name; its
//     spec.infrastructureRef is exactly the
//     spec.machineTemplate.infrastructureRef from the control plane; and it
//     carries a controller owner reference to the HypervisorControlPlane so
//     the control plane owns its Machines.
//   - Bootstrap wiring: for every Machine the controller invokes NewConfig
//     with the control plane and the Machine name, fills the returned
//     HypervisorConfig's spec.clusterName from the linked Cluster, persists
//     the config in the control plane's namespace under the conventional name
//     <machine-name>-config, and points the Machine's
//     spec.bootstrap.configRef at it (bootstrap group, Kind
//     HypervisorConfig, same name and namespace).
//   - Cluster PKI: the controller invokes GeneratePKI once on the first
//     replica and stores the material in the conventional <cluster>-pki
//     Secret in the control plane's namespace, whose data keys are exactly
//     the pki.ClusterPKI exported field names. A later reconcile reads the
//     existing Secret and never regenerates, and multiple replicas still
//     generate the PKI exactly once.
//   - Failures: a failing Machine creation or cluster PKI generation surfaces
//     as a reconcile error that preserves the underlying error, and the
//     failed artifact is not persisted.
package controllers

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// Compile-time pin for the reconciler shape the implementation must expose:
// the Reconcile signature. The three injectable dependencies are pinned by
// the composite literal in newControlPlaneFixture: the field type must accept
// the seam method. Until the reconciler type exists the package does not
// compile — that is the intended red phase.
var _ func(context.Context, ctrl.Request) (ctrl.Result, error) = (*HypervisorControlPlaneReconciler)(nil).Reconcile

// newConfigCall captures one invocation of the config generation seam: the
// control plane and the Machine name the config is generated for.
type newConfigCall struct {
	cp          *controlplanev1alpha1.HypervisorControlPlane
	machineName string
}

// recordingNewConfig records every invocation and returns a config named
// <machine-name>-config in the control plane's namespace with the
// control-plane role pinned. The controller is responsible for filling the
// spec.clusterName from the linked Cluster before persisting.
type recordingNewConfig struct {
	calls []newConfigCall
}

// build implements the NewConfig seam.
func (s *recordingNewConfig) build(
	cp *controlplanev1alpha1.HypervisorControlPlane,
	machineName string,
) *bootstrapv1alpha1.HypervisorConfig {
	s.calls = append(s.calls, newConfigCall{cp: cp, machineName: machineName})

	return &bootstrapv1alpha1.HypervisorConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineConfigName(machineName),
			Namespace: cp.Namespace,
		},
		Spec: bootstrapv1alpha1.HypervisorConfigSpec{
			Role: testConfigRoleControlPlane,
		},
	}
}

// recordingCreateMachine records every invocation and, when no error is
// injected, delegates to the real envtest client so the Machine is persisted
// exactly as the controller built it.
type recordingCreateMachine struct {
	calls []*clusterv1.Machine
	c     client.Client
	err   error
}

// create implements the CreateMachine seam.
func (s *recordingCreateMachine) create(ctx context.Context, machine *clusterv1.Machine) (client.Object, error) {
	s.calls = append(s.calls, machine.DeepCopy())
	if s.err != nil {
		return nil, s.err
	}
	if err := s.c.Create(ctx, machine); err != nil {
		return nil, err
	}

	return machine, nil
}

// recordingCPPKI records every invocation and returns the canned cluster PKI,
// or the injected error.
type recordingCPPKI struct {
	calls int
	pk    pki.ClusterPKI
	err   error
}

// gen implements the GeneratePKI seam.
func (s *recordingCPPKI) gen() (pki.ClusterPKI, error) {
	s.calls++
	if s.err != nil {
		return pki.ClusterPKI{}, s.err
	}

	return s.pk, nil
}

// controlPlaneFixture bundles the reconciler under test with every recording
// seam.
type controlPlaneFixture struct {
	r             *HypervisorControlPlaneReconciler
	newConfig     *recordingNewConfig
	createMachine *recordingCreateMachine
	genPKI        *recordingCPPKI
}

// newControlPlaneFixture builds the reconciler under test over the recording
// seams. The composite literal pins the exact reconciler shape the
// implementation must expose: the controller-runtime wiring plus the
// injectable NewConfig, CreateMachine, and GeneratePKI dependencies.
func newControlPlaneFixture(t *testing.T, c client.Client) *controlPlaneFixture {
	t.Helper()

	newConfig := &recordingNewConfig{}
	createMachine := &recordingCreateMachine{c: c}
	genPKI := &recordingCPPKI{pk: fixtureClusterPKI()}

	r := &HypervisorControlPlaneReconciler{
		Client:        c,
		Scheme:        newScheme(),
		Recorder:      record.NewFakeRecorder(16),
		NewConfig:     newConfig.build,
		CreateMachine: createMachine.create,
		GeneratePKI:   genPKI.gen,
	}

	return &controlPlaneFixture{r: r, newConfig: newConfig, createMachine: createMachine, genPKI: genPKI}
}

// linkedControlPlane is the CAPI linkage the machine-creation contract reads:
// the HypervisorControlPlane, the HypervisorMachineTemplate its
// spec.machineTemplate.infrastructureRef names, and the linked CAPI Cluster
// whose controlPlaneRef points back at the control plane.
type linkedControlPlane struct {
	namespace string
	name      string
	cp        *controlplanev1alpha1.HypervisorControlPlane
	template  *infrastructurev1alpha1.HypervisorMachineTemplate
}

// newLinkedControlPlane creates the full CAPI linkage for the control-plane
// controller: the HypervisorMachineTemplate, the HypervisorControlPlane whose
// machineTemplate references it (with the given replicas and template
// labels), and the Cluster controlPlaneRef link back to the control plane.
func newLinkedControlPlane(
	t *testing.T,
	c client.Client,
	lc *linkedCluster,
	name string,
	replicas int32,
	templateLabels map[string]string,
) *linkedControlPlane {
	t.Helper()
	ctx := t.Context()

	tmpl := &infrastructurev1alpha1.HypervisorMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: name + "-machine-template", Namespace: lc.namespace},
		Spec: infrastructurev1alpha1.HypervisorMachineTemplateSpec{
			Template: infrastructurev1alpha1.HypervisorMachineTemplateResource{
				Spec: infrastructurev1alpha1.HypervisorMachineSpec{
					ClusterName: lc.name,
					CPU:         2,
					RAM:         4096,
					Disk:        20480,
				},
			},
		},
	}
	if err := c.Create(ctx, tmpl); err != nil {
		t.Fatalf("create HypervisorMachineTemplate: %v", err)
	}

	cp := &controlplanev1alpha1.HypervisorControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: lc.namespace},
		Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
			Replicas: replicas,
			Version:  "v1.35.4",
			MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: infrastructurev1alpha1.GroupVersion.String(),
					Kind:       "HypervisorMachineTemplate",
					Name:       tmpl.Name,
					Namespace:  lc.namespace,
				},
				Metadata: clusterv1.ObjectMeta{Labels: templateLabels},
			},
		},
	}
	if err := c.Create(ctx, cp); err != nil {
		t.Fatalf("create HypervisorControlPlane: %v", err)
	}

	lc.cluster.Spec.ControlPlaneRef = &corev1.ObjectReference{
		APIVersion: controlplanev1alpha1.GroupVersion.String(),
		Kind:       "HypervisorControlPlane",
		Name:       cp.Name,
		Namespace:  lc.namespace,
	}
	if err := c.Update(ctx, lc.cluster); err != nil {
		t.Fatalf("link control plane ref on Cluster: %v", err)
	}

	return &linkedControlPlane{namespace: lc.namespace, name: name, cp: cp, template: tmpl}
}

// reconcileControlPlane runs one reconcile of the control plane and fails the
// test on any error.
func (fx *controlPlaneFixture) reconcileControlPlane(t *testing.T, cp *controlplanev1alpha1.HypervisorControlPlane) {
	t.Helper()
	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
}

// machineConfigName returns the conventional name of the HypervisorConfig the
// control-plane reconciler generates for the Machine named machineName.
func machineConfigName(machineName string) string {
	return machineName + "-config"
}

// listControlPlaneMachines lists the Machines the control-plane reconciler
// manages: the cluster-name label plus the control-plane role label.
func listControlPlaneMachines(t *testing.T, c client.Client, namespace, clusterName string) []clusterv1.Machine {
	t.Helper()
	list := &clusterv1.MachineList{}
	if err := c.List(t.Context(), list, client.InNamespace(namespace), client.MatchingLabels(map[string]string{
		clusterv1.ClusterNameLabel:         clusterName,
		clusterv1.MachineControlPlaneLabel: "",
	})); err != nil {
		t.Fatalf("List control-plane Machines in %q: %v", namespace, err)
	}

	return list.Items
}

// machineNamesOf returns the Machine names in list order.
func machineNamesOf(machines []clusterv1.Machine) []string {
	names := make([]string, 0, len(machines))
	for i := range machines {
		names = append(names, machines[i].Name)
	}

	return names
}

// sameStringSet reports whether a and b contain the same strings, ignoring
// order and multiplicity.
func sameStringSet(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	seen := make(map[string]int, len(a))
	for _, s := range a {
		seen[s]++
	}
	for _, s := range b {
		seen[s]--
		if seen[s] < 0 {
			return false
		}
	}

	return true
}

// wantMachineLabels fails the test unless the Machine carries the cluster
// name label, the control-plane role label with its conventional empty value,
// and every label from the template metadata.
func wantMachineLabels(t *testing.T, machine *clusterv1.Machine, clusterName string, templateLabels map[string]string) {
	t.Helper()
	if got := machine.Labels[clusterv1.ClusterNameLabel]; got != clusterName {
		t.Errorf("Machine %s cluster-name label = %q, want %q", machine.Name, got, clusterName)
	}
	if got, ok := machine.Labels[clusterv1.MachineControlPlaneLabel]; !ok || got != "" {
		t.Errorf("Machine %s control-plane label = %q (present %v), want the conventional empty value", machine.Name, got, ok)
	}
	for key, want := range templateLabels {
		if got := machine.Labels[key]; got != want {
			t.Errorf("Machine %s label %q = %q, want %q", machine.Name, key, got, want)
		}
	}
}

// wantControlPlaneOwner fails the test unless the Machine carries a
// controller owner reference to the HypervisorControlPlane.
func wantControlPlaneOwner(t *testing.T, machine *clusterv1.Machine, cp *controlplanev1alpha1.HypervisorControlPlane) {
	t.Helper()
	for _, ref := range machine.OwnerReferences {
		if ref.Kind == "HypervisorControlPlane" && ref.Name == cp.Name && ref.Controller != nil && *ref.Controller {
			return
		}
	}
	t.Errorf("Machine %s has no controller owner reference to HypervisorControlPlane %s (owner references %+v)",
		machine.Name, cp.Name, machine.OwnerReferences)
}

// TestControlPlaneMachineCreatedPerReplica pins the replica contract: one
// Machine is created per replica, each carrying the control-plane role label
// and the cluster linkage, and an unset replicas field behaves as one. A
// second reconcile creates nothing — the reconciler converges on the desired
// count instead of duplicating.
func TestControlPlaneMachineCreatedPerReplica(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("one replica creates exactly one machine", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-machines-one", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

		fx.reconcileControlPlane(t, lcp.cp)

		machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
		if len(machines) != 1 {
			t.Fatalf("created %d Machines, want 1 (names %v)", len(machines), machineNamesOf(machines))
		}
		m := machines[0]
		if m.Name != lcp.name+"-0" {
			t.Errorf("Machine name = %q, want %q", m.Name, lcp.name+"-0")
		}
		wantMachineLabels(t, &m, lc.name, nil)
		if m.Spec.ClusterName != lc.name {
			t.Errorf("spec.clusterName = %q, want %q", m.Spec.ClusterName, lc.name)
		}
		if !reflect.DeepEqual(m.Spec.InfrastructureRef, lcp.cp.Spec.MachineTemplate.InfrastructureRef) {
			t.Errorf("spec.infrastructureRef = %+v, want the machineTemplate infrastructureRef %+v",
				m.Spec.InfrastructureRef, lcp.cp.Spec.MachineTemplate.InfrastructureRef)
		}
		wantControlPlaneOwner(t, &m, lcp.cp)
		if m.Spec.Bootstrap.ConfigRef == nil {
			t.Fatal("spec.bootstrap.configRef is nil after reconcile")
		}

		if len(fx.createMachine.calls) != 1 {
			t.Errorf("CreateMachine called %d times, want 1", len(fx.createMachine.calls))
		}

		// A second reconcile converges: the Machine already exists, so the
		// creation seam is not invoked again and no duplicate appears.
		fx.reconcileControlPlane(t, lcp.cp)
		if len(fx.createMachine.calls) != 1 {
			t.Errorf("CreateMachine called %d times across two reconciles, want 1", len(fx.createMachine.calls))
		}
		if got := listControlPlaneMachines(t, c, lc.namespace, lc.name); len(got) != 1 {
			t.Errorf("reconcile duplicated Machines: %d after second reconcile, want 1", len(got))
		}
	})

	t.Run("unset replicas defaults to one machine", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-machines-default", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 0, nil)

		fx.reconcileControlPlane(t, lcp.cp)

		if machines := listControlPlaneMachines(t, c, lc.namespace, lc.name); len(machines) != 1 {
			t.Errorf("created %d Machines with unset replicas, want 1 (names %v)", len(machines), machineNamesOf(machines))
		}
	})

	t.Run("two replicas create two machines", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-machines-two", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 2, nil)

		fx.reconcileControlPlane(t, lcp.cp)

		machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
		if len(machines) != 2 {
			t.Fatalf("created %d Machines, want 2 (names %v)", len(machines), machineNamesOf(machines))
		}
		if len(fx.createMachine.calls) != 2 {
			t.Errorf("CreateMachine called %d times, want 2", len(fx.createMachine.calls))
		}
		for i := range machines {
			wantMachineLabels(t, &machines[i], lc.name, nil)
		}
	})
}

// TestControlPlaneMachineBootstrapRef pins the bootstrap wiring contract:
// every Machine's spec.bootstrap.configRef names the generated
// HypervisorConfig — bootstrap group, Kind HypervisorConfig, the conventional
// <machine-name>-config name in the control plane's namespace — and the
// referenced config exists in the API store, wired to the linked Cluster and
// pinned to the control-plane role.
func TestControlPlaneMachineBootstrapRef(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-bootstrap-ref", "capi-cluster")
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 1 {
		t.Fatalf("created %d Machines, want 1 (names %v)", len(machines), machineNamesOf(machines))
	}
	m := machines[0]

	ref := m.Spec.Bootstrap.ConfigRef
	if ref == nil {
		t.Fatal("spec.bootstrap.configRef is nil after reconcile")
	}
	if ref.APIVersion != bootstrapv1alpha1.GroupVersion.String() {
		t.Errorf("configRef apiVersion = %q, want %q", ref.APIVersion, bootstrapv1alpha1.GroupVersion.String())
	}
	if ref.Kind != "HypervisorConfig" {
		t.Errorf("configRef kind = %q, want HypervisorConfig", ref.Kind)
	}
	wantConfigName := machineConfigName(m.Name)
	if ref.Name != wantConfigName {
		t.Errorf("configRef name = %q, want %q", ref.Name, wantConfigName)
	}
	if ref.Namespace != lc.namespace {
		t.Errorf("configRef namespace = %q, want %q", ref.Namespace, lc.namespace)
	}

	cfg := &bootstrapv1alpha1.HypervisorConfig{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: lc.namespace, Name: wantConfigName}, cfg); err != nil {
		t.Fatalf("Get generated HypervisorConfig %q: %v", wantConfigName, err)
	}
	if cfg.Spec.ClusterName != lc.name {
		t.Errorf("generated config spec.clusterName = %q, want %q", cfg.Spec.ClusterName, lc.name)
	}
	if cfg.Spec.Role != testConfigRoleControlPlane {
		t.Errorf("generated config spec.role = %q, want %q", cfg.Spec.Role, testConfigRoleControlPlane)
	}

	if len(fx.newConfig.calls) != 1 {
		t.Fatalf("NewConfig called %d times, want 1", len(fx.newConfig.calls))
	}
	if call := fx.newConfig.calls[0]; call.machineName != lcp.name+"-0" {
		t.Errorf("NewConfig called with machine name %q, want %q", call.machineName, lcp.name+"-0")
	}
}

// TestControlPlanePKISecretCreated pins the cluster PKI contract: the first
// replica invokes the PKI generator exactly once and stores the material in
// the conventional <cluster>-pki Secret whose data keys are exactly the
// pki.ClusterPKI exported field names. A second reconcile reads the existing
// Secret and never regenerates, and multiple replicas still generate the PKI
// once per cluster.
func TestControlPlanePKISecretCreated(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("pki secret created once on the first replica", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-pki", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

		fx.reconcileControlPlane(t, lcp.cp)

		if fx.genPKI.calls != 1 {
			t.Errorf("GeneratePKI called %d times, want 1", fx.genPKI.calls)
		}

		pkiKey := client.ObjectKey{Namespace: lc.namespace, Name: lc.name + "-pki"}
		secret := &corev1.Secret{}
		if err := c.Get(t.Context(), pkiKey, secret); err != nil {
			t.Fatalf("Get cluster PKI Secret %s: %v", pkiKey, err)
		}
		wantData := fixturePKISecretData()
		if len(secret.Data) != len(wantData) {
			t.Errorf("cluster PKI Secret has %d keys, want %d: %v", len(secret.Data), len(wantData), secret.Data)
		}
		for key, want := range wantData {
			if got, ok := secret.Data[key]; !ok || !bytes.Equal(got, want) {
				t.Errorf("cluster PKI Secret data[%q] = %q (present %v), want %q", key, got, ok, want)
			}
		}

		// A second reconcile does not regenerate or duplicate the Secret.
		fx.reconcileControlPlane(t, lcp.cp)
		if fx.genPKI.calls != 1 {
			t.Errorf("GeneratePKI called %d times across two reconciles, want 1", fx.genPKI.calls)
		}
		if got := countSecretsNamed(t, c, lc.namespace, lc.name+"-pki"); got != 1 {
			t.Errorf("cluster PKI Secrets = %d, want exactly 1", got)
		}
	})

	t.Run("multiple replicas share one cluster PKI", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-pki-multi", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 2, nil)

		fx.reconcileControlPlane(t, lcp.cp)

		if fx.genPKI.calls != 1 {
			t.Errorf("GeneratePKI called %d times for two replicas, want 1", fx.genPKI.calls)
		}
		if got := countSecretsNamed(t, c, lc.namespace, lc.name+"-pki"); got != 1 {
			t.Errorf("cluster PKI Secrets = %d, want exactly 1", got)
		}
	})
}

// TestControlPlaneMachineLabels pins the label propagation contract: every
// label from spec.machineTemplate.metadata is present on the created Machines
// alongside the standard cluster-name and control-plane role labels.
func TestControlPlaneMachineLabels(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-labels", "capi-cluster")
	templateLabels := map[string]string{"lab-node": "cp-1", "team": "k8labs"}
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, templateLabels)

	fx.reconcileControlPlane(t, lcp.cp)

	machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 1 {
		t.Fatalf("created %d Machines, want 1 (names %v)", len(machines), machineNamesOf(machines))
	}
	wantMachineLabels(t, &machines[0], lc.name, templateLabels)
}

// TestControlPlaneMachineNamePattern pins the naming contract: Machine names
// follow the deterministic <control-plane-name>-<index> pattern with the
// index starting at zero, so the replica set for two replicas is exactly the
// names <control-plane-name>-0 and <control-plane-name>-1.
func TestControlPlaneMachineNamePattern(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-name-pattern", "capi-cluster")
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 2, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 2 {
		t.Fatalf("created %d Machines, want 2 (names %v)", len(machines), machineNamesOf(machines))
	}
	want := []string{lcp.name + "-0", lcp.name + "-1"}
	if !sameStringSet(machineNamesOf(machines), want) {
		t.Errorf("Machine names = %v, want %v", machineNamesOf(machines), want)
	}
}

// TestControlPlaneMachineMissingLinkedCluster pins the linkage gate: a
// HypervisorControlPlane with no CAPI Cluster whose controlPlaneRef names it
// is left untouched — no Machine, no HypervisorConfig, no PKI Secret, no seam
// invocation — and reconcile returns no error.
func TestControlPlaneMachineMissingLinkedCluster(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)

	const namespace = "cp-missing-cluster"
	if err := c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}}); err != nil {
		t.Fatalf("create namespace %q: %v", namespace, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Delete(cleanupCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: namespace}})
	})

	tmpl := &infrastructurev1alpha1.HypervisorMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-cp-machine-template", Namespace: namespace},
		Spec: infrastructurev1alpha1.HypervisorMachineTemplateSpec{
			Template: infrastructurev1alpha1.HypervisorMachineTemplateResource{
				Spec: infrastructurev1alpha1.HypervisorMachineSpec{
					ClusterName: "ghost-cluster",
					CPU:         2,
					RAM:         4096,
					Disk:        20480,
				},
			},
		},
	}
	if err := c.Create(t.Context(), tmpl); err != nil {
		t.Fatalf("create HypervisorMachineTemplate: %v", err)
	}

	cp := &controlplanev1alpha1.HypervisorControlPlane{
		ObjectMeta: metav1.ObjectMeta{Name: "orphan-cp", Namespace: namespace},
		Spec: controlplanev1alpha1.HypervisorControlPlaneSpec{
			Replicas: 1,
			MachineTemplate: controlplanev1alpha1.HypervisorControlPlaneMachineTemplate{
				InfrastructureRef: corev1.ObjectReference{
					APIVersion: infrastructurev1alpha1.GroupVersion.String(),
					Kind:       "HypervisorMachineTemplate",
					Name:       tmpl.Name,
					Namespace:  namespace,
				},
			},
		},
	}
	if err := c.Create(t.Context(), cp); err != nil {
		t.Fatalf("create HypervisorControlPlane: %v", err)
	}

	res, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cp)})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	if res != (ctrl.Result{}) {
		t.Errorf("Reconcile result = %+v, want empty", res)
	}

	if len(fx.newConfig.calls) != 0 || len(fx.createMachine.calls) != 0 || fx.genPKI.calls != 0 {
		t.Errorf("missing-cluster reconcile touched the seams: NewConfig %d, CreateMachine %d, GeneratePKI %d",
			len(fx.newConfig.calls), len(fx.createMachine.calls), fx.genPKI.calls)
	}
	if machines := listControlPlaneMachines(t, c, namespace, "ghost-cluster"); len(machines) != 0 {
		t.Errorf("missing-cluster reconcile created %d Machines, want 0", len(machines))
	}
	configs := &bootstrapv1alpha1.HypervisorConfigList{}
	if err := c.List(t.Context(), configs, client.InNamespace(namespace)); err != nil {
		t.Fatalf("List HypervisorConfigs: %v", err)
	}
	if len(configs.Items) != 0 {
		t.Errorf("missing-cluster reconcile created %d HypervisorConfigs, want 0", len(configs.Items))
	}
	if got := countSecrets(t, c, namespace); got != 0 {
		t.Errorf("missing-cluster reconcile created %d Secrets, want 0", got)
	}
}

// TestControlPlaneMachineCreationFailure pins the failure contract: a failing
// Machine creation or cluster PKI generation surfaces as a reconcile error
// that preserves the underlying error, and the failed artifact is not
// persisted.
func TestControlPlaneMachineCreationFailure(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("machine creation failure surfaces", func(t *testing.T) {
		errCreate := errors.New("fake: machine creation denied")
		fx := newControlPlaneFixture(t, c)
		fx.createMachine.err = errCreate
		lc := newLinkedCluster(t, c, "cp-fail-create", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp.cp)})
		if err == nil {
			t.Fatal("Reconcile succeeded with a failing Machine creation, want an error")
		}
		if !errors.Is(err, errCreate) {
			t.Errorf("Reconcile error %v does not wrap %v", err, errCreate)
		}
		if len(fx.createMachine.calls) != 1 {
			t.Errorf("CreateMachine called %d times, want 1", len(fx.createMachine.calls))
		}
		if machines := listControlPlaneMachines(t, c, lc.namespace, lc.name); len(machines) != 0 {
			t.Errorf("failed reconcile persisted %d Machines, want 0", len(machines))
		}
	})

	t.Run("cluster PKI generation failure surfaces", func(t *testing.T) {
		errPKI := errors.New("fake: pki generation denied")
		fx := newControlPlaneFixture(t, c)
		fx.genPKI.err = errPKI
		lc := newLinkedCluster(t, c, "cp-fail-pki", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp.cp)})
		if err == nil {
			t.Fatal("Reconcile succeeded with a failing PKI generator, want an error")
		}
		if !errors.Is(err, errPKI) {
			t.Errorf("Reconcile error %v does not wrap %v", err, errPKI)
		}
		if got := countSecretsNamed(t, c, lc.namespace, lc.name+"-pki"); got != 0 {
			t.Errorf("failed reconcile created %d cluster PKI Secrets, want 0", got)
		}
	})
}
