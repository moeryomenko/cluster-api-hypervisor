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
//     spec.infrastructureRef names the concrete HypervisorMachine the
//     controller instantiates from the template named by
//     spec.machineTemplate.spec.infrastructureRef (get-or-create named after
//     the Machine, template spec.template.spec copied, cluster-name label
//     set, controller owner reference to the Machine); and it carries a
//     controller owner reference to the HypervisorControlPlane so the control
//     plane owns its Machines.
//   - Bootstrap wiring: for every Machine the controller invokes NewConfig
//     with the control plane and the Machine name, fills the returned
//     HypervisorConfig's spec.clusterName from the linked Cluster and its
//     spec.nodeName with the Machine name, persists the config in the control
//     plane's namespace under the conventional name <machine-name>-config,
//     and points the Machine's spec.bootstrap.configRef at it (bootstrap
//     group, Kind HypervisorConfig, same name and namespace).
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
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"gopkg.in/yaml.v3"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd/fake"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/mac"
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
// spec.clusterName from the linked Cluster and the spec.nodeName with the
// Machine name before persisting.
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

// recordingCPPKI records every invocation — the control-plane IP the
// controller reserved through k8netd — and returns the canned cluster PKI,
// or the injected error.
type recordingCPPKI struct {
	calls []string
	pk    pki.ClusterPKI
	err   error
}

// gen implements the GeneratePKI seam.
func (s *recordingCPPKI) gen(cpIP string) (pki.ClusterPKI, error) {
	s.calls = append(s.calls, cpIP)
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
	health        *recordingHealthCheck
}

// testReservedCPIP is the non-default address the fixture's fake k8netd
// server answers AllocateIP with; the PKI SAN input must be exactly this
// reservation, never a pinned pool constant.
const testReservedCPIP = "192.168.124.77"

// newControlPlaneFixture builds the reconciler under test over the recording
// seams with the canned cluster PKI fixture bytes.
func newControlPlaneFixture(t *testing.T, c client.Client) *controlPlaneFixture {
	t.Helper()
	return newControlPlaneFixtureWithPKI(t, c, fixtureClusterPKI())
}

// newControlPlaneFixtureWithPKI builds the reconciler under test over the
// recording seams with the given cluster PKI. The composite literal pins the
// exact reconciler shape the implementation must expose: the
// controller-runtime wiring plus the injectable NewConfig, CreateMachine,
// GeneratePKI, K8Netd, and CheckAPIServerHealth dependencies. K8Netd is wired
// to a fake k8netd server whose AllocateIP answers testReservedCPIP, so the
// PKI SAN input flows from the reservation. The readiness tests pass real
// generated PKI so the rendered kubeconfig is parseable; the machine-creation
// suite keeps the canned fixture bytes.
func newControlPlaneFixtureWithPKI(t *testing.T, c client.Client, pk pki.ClusterPKI) *controlPlaneFixture {
	t.Helper()

	newConfig := &recordingNewConfig{}
	createMachine := &recordingCreateMachine{c: c}
	genPKI := &recordingCPPKI{pk: pk}
	health := &recordingHealthCheck{}

	sock := filepath.Join(t.TempDir(), "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New %q: %v", sock, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	srv.SetResult("AllocateIP", testReservedCPIP)

	r := &HypervisorControlPlaneReconciler{
		Client:               c,
		Scheme:               newScheme(),
		Recorder:             record.NewFakeRecorder(16),
		NewConfig:            newConfig.build,
		CreateMachine:        createMachine.create,
		GeneratePKI:          genPKI.gen,
		CheckAPIServerHealth: health.check,
		K8Netd:               k8netd.NewClient(sock),
	}

	return &controlPlaneFixture{r: r, newConfig: newConfig, createMachine: createMachine, genPKI: genPKI, health: health}
}

// linkedControlPlane is the CAPI linkage the machine-creation contract reads:
// the HypervisorControlPlane, the HypervisorMachineTemplate its
// spec.machineTemplate.spec.infrastructureRef names, and the linked CAPI
// Cluster whose controlPlaneRef points back at the control plane.
type linkedControlPlane struct {
	namespace string
	name      string
	cp        *controlplanev1alpha1.HypervisorControlPlane
	template  *infrastructurev1alpha1.HypervisorMachineTemplate
}

// newLinkedControlPlane creates the full CAPI linkage for the control-plane
// controller: the HypervisorMachineTemplate, the HypervisorControlPlane whose
// machineTemplate references it through the v1beta2 nested
// machineTemplate.spec.infrastructureRef contract-versioned shape (with the
// given replicas and template labels), and the Cluster controlPlaneRef link
// back to the control plane. The reference carries no namespace: the
// ContractVersionedObjectReference type has none, so the reconciler resolves
// the template in the control plane's namespace.
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
				Metadata: clusterv1.ObjectMeta{Labels: templateLabels},
				Spec: controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: infrastructurev1alpha1.GroupVersion.Group,
						Kind:     "HypervisorMachineTemplate",
						Name:     tmpl.Name,
					},
				},
			},
		},
	}
	if err := c.Create(ctx, cp); err != nil {
		t.Fatalf("create HypervisorControlPlane: %v", err)
	}

	lc.cluster.Spec.ControlPlaneRef = clusterv1.ContractVersionedObjectReference{
		APIGroup: controlplanev1alpha1.GroupVersion.Group,
		Kind:     "HypervisorControlPlane",
		Name:     cp.Name,
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

// wantMachineOwner fails the test unless the object carries a controller
// owner reference to the given Machine.
func wantMachineOwner(t *testing.T, obj client.Object, machine *clusterv1.Machine) {
	t.Helper()
	for _, ref := range obj.GetOwnerReferences() {
		if ref.Kind == "Machine" && ref.Name == machine.Name && ref.Controller != nil && *ref.Controller {
			return
		}
	}
	t.Errorf("%s %s has no controller owner reference to Machine %s (owner references %+v)",
		obj.GetObjectKind().GroupVersionKind().Kind, obj.GetName(), machine.Name, obj.GetOwnerReferences())
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
		wantRef := clusterv1.ContractVersionedObjectReference{
			APIGroup: infrastructurev1alpha1.GroupVersion.Group,
			Kind:     "HypervisorMachine",
			Name:     m.Name,
		}
		if !reflect.DeepEqual(m.Spec.InfrastructureRef, wantRef) {
			t.Errorf("spec.infrastructureRef = %+v, want the concrete HypervisorMachine reference %+v",
				m.Spec.InfrastructureRef, wantRef)
		}
		hm := &infrastructurev1alpha1.HypervisorMachine{}
		if err := c.Get(t.Context(), client.ObjectKey{Namespace: lc.namespace, Name: m.Name}, hm); err != nil {
			t.Fatalf("Get HypervisorMachine %q: %v", m.Name, err)
		}
		wantSpec := lcp.template.Spec.Template.Spec
		if hm.Spec != wantSpec {
			t.Errorf("HypervisorMachine spec = %+v, want the template spec.template.spec %+v", hm.Spec, wantSpec)
		}
		if got := hm.Labels[clusterv1.ClusterNameLabel]; got != lc.name {
			t.Errorf("HypervisorMachine %s cluster-name label = %q, want %q", hm.Name, got, lc.name)
		}
		wantMachineOwner(t, hm, &m)
		wantControlPlaneOwner(t, &m, lcp.cp)
		if !m.Spec.Bootstrap.ConfigRef.IsDefined() {
			t.Fatal("spec.bootstrap.configRef is unset after reconcile")
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

// TestControlPlaneMachineInfraResolvedThroughMachineTemplateSpec pins the
// v1beta2 reference path: the reconciler resolves the infrastructure template
// THROUGH spec.machineTemplate.spec.infrastructureRef, not through any other
// template in the namespace. Two templates exist; the reference is repointed
// at the second one before the first reconcile, and the created Machine must
// reference a concrete HypervisorMachine cloned from exactly that template —
// its spec matches the referenced template's spec.template.spec and the
// cloned-from annotations record the resolved template. The
// ContractVersionedObjectReference carries no namespace field, so resolution
// in the control plane's namespace is compile-enforced and proven here by the
// clone succeeding from that namespace alone.
func TestControlPlaneMachineInfraResolvedThroughMachineTemplateSpec(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-infra-ref-path", "capi-cluster")
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	// The decoy template from newLinkedControlPlane carries CPU 2; the
	// referenced one carries CPU 8, so matching specs prove which template
	// the reconcile followed.
	referenced := &infrastructurev1alpha1.HypervisorMachineTemplate{
		ObjectMeta: metav1.ObjectMeta{Name: lcp.name + "-machine-template-referenced", Namespace: lc.namespace},
		Spec: infrastructurev1alpha1.HypervisorMachineTemplateSpec{
			Template: infrastructurev1alpha1.HypervisorMachineTemplateResource{
				Spec: infrastructurev1alpha1.HypervisorMachineSpec{
					ClusterName: lc.name,
					CPU:         8,
					RAM:         16384,
					Disk:        40960,
				},
			},
		},
	}
	if err := c.Create(t.Context(), referenced); err != nil {
		t.Fatalf("create referenced HypervisorMachineTemplate: %v", err)
	}

	lcp.cp = updateControlPlaneSpec(t, c, lcp.cp, func(cp *controlplanev1alpha1.HypervisorControlPlane) {
		cp.Spec.MachineTemplate.Spec.InfrastructureRef = clusterv1.ContractVersionedObjectReference{
			APIGroup: infrastructurev1alpha1.GroupVersion.Group,
			Kind:     "HypervisorMachineTemplate",
			Name:     referenced.Name,
		}
	})
	fx.reconcileControlPlane(t, lcp.cp)

	machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 1 {
		t.Fatalf("created %d Machines, want 1 (names %v)", len(machines), machineNamesOf(machines))
	}
	m := machines[0]

	wantRef := clusterv1.ContractVersionedObjectReference{
		APIGroup: infrastructurev1alpha1.GroupVersion.Group,
		Kind:     "HypervisorMachine",
		Name:     m.Name,
	}
	if !reflect.DeepEqual(m.Spec.InfrastructureRef, wantRef) {
		t.Errorf(
			"spec.infrastructureRef = %+v, want the concrete HypervisorMachine reference %+v resolved through machineTemplate.spec.infrastructureRef",
			m.Spec.InfrastructureRef,
			wantRef,
		)
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: lc.namespace, Name: m.Name}, hm); err != nil {
		t.Fatalf("Get HypervisorMachine %q: %v", m.Name, err)
	}
	wantSpec := referenced.Spec.Template.Spec
	if hm.Spec != wantSpec {
		t.Errorf(
			"HypervisorMachine spec = %+v, want the REFERENCED template's spec.template.spec %+v (CPU 8), not another namespace template",
			hm.Spec,
			wantSpec,
		)
	}
	if got := hm.Annotations[clusterv1.TemplateClonedFromNameAnnotation]; got != referenced.Name {
		t.Errorf(
			"HypervisorMachine %s cloned-from annotation = %q, want the referenced template %q",
			hm.Name,
			got,
			referenced.Name,
		)
	}
	wantGroupKind := "HypervisorMachineTemplate." + infrastructurev1alpha1.GroupVersion.Group
	if got := hm.Annotations[clusterv1.TemplateClonedFromGroupKindAnnotation]; got != wantGroupKind {
		t.Errorf("HypervisorMachine %s cloned-from-groupkind annotation = %q, want %q", hm.Name, got, wantGroupKind)
	}
	wantMachineOwner(t, hm, &m)
}

// TestControlPlaneMachineBootstrapRef pins the bootstrap wiring contract:
// every Machine's spec.bootstrap.configRef names the generated
// HypervisorConfig — bootstrap group, Kind HypervisorConfig, the conventional
// <machine-name>-config name in the control plane's namespace — and the
// referenced config exists in the API store, wired to the linked Cluster,
// pinned to the control-plane role, and carries spec.nodeName equal to the
// Machine name.
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
	if !ref.IsDefined() {
		t.Fatal("spec.bootstrap.configRef is unset after reconcile")
	}
	if ref.APIGroup != bootstrapv1alpha1.GroupVersion.Group {
		t.Errorf("configRef apiGroup = %q, want %q", ref.APIGroup, bootstrapv1alpha1.GroupVersion.Group)
	}
	if ref.Kind != "HypervisorConfig" {
		t.Errorf("configRef kind = %q, want HypervisorConfig", ref.Kind)
	}
	wantConfigName := machineConfigName(m.Name)
	if ref.Name != wantConfigName {
		t.Errorf("configRef name = %q, want %q", ref.Name, wantConfigName)
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
	if cfg.Spec.NodeName != m.Name {
		t.Errorf("generated config spec.nodeName = %q, want %q", cfg.Spec.NodeName, m.Name)
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

		if len(fx.genPKI.calls) != 1 {
			t.Errorf("GeneratePKI called %d times, want 1", len(fx.genPKI.calls))
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
		if len(fx.genPKI.calls) != 1 {
			t.Errorf("GeneratePKI called %d times across two reconciles, want 1", len(fx.genPKI.calls))
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

		if len(fx.genPKI.calls) != 1 {
			t.Errorf("GeneratePKI called %d times for two replicas, want 1", len(fx.genPKI.calls))
		}
		if got := countSecretsNamed(t, c, lc.namespace, lc.name+"-pki"); got != 1 {
			t.Errorf("cluster PKI Secrets = %d, want exactly 1", got)
		}
	})
}

// TestControlPlanePKISANInputIsReservedIP pins REQ-006: the PKI SAN input is
// the cp-0 internal IP reserved through k8netd AllocateIP — for the same MAC
// derivation the machine controller uses for <control-plane-name>-0 on the
// cluster's network — never a hardcoded pool address. The fake server answers
// with a non-default address; the test proves the generator received exactly
// that reservation and that the stored apiserver certificate carries it plus
// loopback as its IP SANs.
func TestControlPlanePKISANInputIsReservedIP(t *testing.T) {
	c := mustReconcileClient(t)

	sock := filepath.Join(t.TempDir(), "control.sock")
	srv, err := fake.New(sock)
	if err != nil {
		t.Fatalf("fake.New %q: %v", sock, err)
	}
	t.Cleanup(func() { _ = srv.Close() })
	var allocNetwork, allocMAC string
	srv.Handle("AllocateIP", func(params json.RawMessage) (any, *fake.RPCError) {
		var p map[string]string
		_ = json.Unmarshal(params, &p)
		allocNetwork, allocMAC = p["network"], p["mac"]
		return testReservedCPIP, nil
	})

	lc := newLinkedCluster(t, c, "cp-pki-san-reserved", "capi-cluster")
	machineName := lc.name + "-cp-0"
	pk := mustGenerateClusterPKI(t, testReservedCPIP, machineName)
	fx := newControlPlaneFixtureWithPKI(t, c, pk)
	fx.r.K8Netd = k8netd.NewClient(sock)
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	if len(fx.genPKI.calls) != 1 || fx.genPKI.calls[0] != testReservedCPIP {
		t.Fatalf("GeneratePKI inputs = %v, want exactly [%q] (the k8netd reservation)", fx.genPKI.calls, testReservedCPIP)
	}
	if allocNetwork != lc.name {
		t.Errorf("AllocateIP network = %q, want the HypervisorCluster name %q", allocNetwork, lc.name)
	}
	if wantMAC := mac.Derive(lc.name, lcp.name+"-0"); allocMAC != wantMAC {
		t.Errorf("AllocateIP mac = %q, want the cp-0 derivation %q", allocMAC, wantMAC)
	}

	// The stored apiserver certificate SANs carry the reserved IP and loopback.
	secret := &corev1.Secret{}
	pkiKey := client.ObjectKey{Namespace: lc.namespace, Name: lc.name + "-pki"}
	if err := c.Get(t.Context(), pkiKey, secret); err != nil {
		t.Fatalf("Get cluster PKI Secret %s: %v", pkiKey, err)
	}
	got, err := decodeClusterPKI(secret.Data)
	if err != nil {
		t.Fatalf("decode stored cluster PKI: %v", err)
	}
	block, _ := pem.Decode(got.APIServer)
	if block == nil {
		t.Fatal("stored apiserver certificate is not PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("parse stored apiserver certificate: %v", err)
	}
	sans := make(map[string]bool, len(cert.IPAddresses))
	for _, ip := range cert.IPAddresses {
		sans[ip.String()] = true
	}
	if !sans[testReservedCPIP] || !sans["127.0.0.1"] {
		t.Errorf("apiserver certificate IP SANs = %v, want {%q 127.0.0.1}", sans, testReservedCPIP)
	}
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
				Spec: controlplanev1alpha1.HypervisorControlPlaneMachineTemplateSpec{
					InfrastructureRef: clusterv1.ContractVersionedObjectReference{
						APIGroup: infrastructurev1alpha1.GroupVersion.Group,
						Kind:     "HypervisorMachineTemplate",
						Name:     tmpl.Name,
					},
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

	if len(fx.newConfig.calls) != 0 || len(fx.createMachine.calls) != 0 || len(fx.genPKI.calls) != 0 {
		t.Errorf("missing-cluster reconcile touched the seams: NewConfig %d, CreateMachine %d, GeneratePKI %d",
			len(fx.newConfig.calls), len(fx.createMachine.calls), len(fx.genPKI.calls))
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

// Readiness and kubeconfig contract (test-first, red).
//
// This section pins the control-plane readiness contract: once the first
// control-plane Machine's VM is up, the reconciler polls the workload
// apiserver healthz endpoint through an injectable seam, and only when the
// apiserver is healthy does it write the conventional <cluster>-kubeconfig
// Secret, set status.initialized and status.ready, and report the
// ControlPlaneReady condition.
//
// The contract, in prose:
//
//   - HypervisorControlPlaneReconciler gains one readiness seam,
//     CheckAPIServerHealth, called as
//     CheckAPIServerHealth(ctx, host, port, clientCert, clientKey, caCert) and
//     returning nil exactly when the workload apiserver is healthy. The tests
//     inject a recording fake whose result (healthy, not ready, failing) is
//     chosen per test.
//   - The reconciler resolves the first control-plane Machine (the one with
//     the control-plane role label) and its linked HypervisorMachine (the
//     conventional same-name infrastructure object), reads the InternalIP
//     address and the recorded 6443 allocation from status.publishedPorts,
//     and polls the apiserver at https://127.0.0.1:<hostPort> through the
//     seam with the cluster PKI material as the client certificate and CA.
//     A machine without a recorded allocation requeues without polling —
//     there is no fallback to the VM IP, which has no host route.
//   - Before the VM reports an address the reconciler must not poll, must not
//     write anything, and must requeue: nothing in the committed watch set
//     (control plane, Machines, Clusters) fires on a HypervisorMachine status
//     change, so the requeue is what eventually notices the boot.
//   - When the poll reports healthy, the reconciler renders the admin
//     kubeconfig with the server https://<cp-ip>:6443 and the cluster CA,
//     writes it as the conventional <cluster>-kubeconfig Secret in the
//     control plane's namespace under the CAPI data key "value", and sets
//     status.initialized, status.ready, and the ControlPlaneReady condition.
//   - The poller is bounded: a never-healthy apiserver ends the reconcile in
//     a requeue and the kubeconfig Secret is never written; a failing healthz
//     check behaves the same. In both cases initialized and ready stay false
//     and no ready condition is reported.
//
// Unlike the machine-creation suite, the readiness tests generate real
// cluster PKI through pki.GenerateClusterPKI instead of the canned fixture
// bytes: the rendered kubeconfig must be a parseable document whose CA
// round-trips, which canned bytes cannot satisfy.
const (
	// controlPlaneReadyCondition is the condition type the readiness contract
	// requires once the workload apiserver is healthy.
	controlPlaneReadyCondition = "ControlPlaneReady"

	// kubeconfigSecretDataKey is the data key the conventional
	// <cluster>-kubeconfig Secret carries the rendered document under.
	kubeconfigSecretDataKey = "value"
)

// errAPIServerNotReady is the sentinel the healthz fake returns while the
// workload apiserver is still starting; the poller must treat it as
// not-yet-healthy and keep polling.
var errAPIServerNotReady = errors.New("apiserver not ready yet")

// healthCheckCall captures one invocation of the apiserver healthz seam: the
// poll target endpoint and the cluster PKI material passed as client
// credentials and CA.
type healthCheckCall struct {
	host       string
	port       int32
	clientCert []byte
	clientKey  []byte
	caCert     []byte
}

// recordingHealthCheck records every healthz invocation and returns the
// injected result: nil for a healthy apiserver, errAPIServerNotReady for a
// never-healthy apiserver, or the injected hard error.
type recordingHealthCheck struct {
	calls  []healthCheckCall
	result error
}

// check implements the CheckAPIServerHealth seam.
func (s *recordingHealthCheck) check(
	_ context.Context,
	host string,
	port int32,
	clientCert, clientKey, caCert []byte,
) error {
	s.calls = append(
		s.calls,
		healthCheckCall{host: host, port: port, clientCert: clientCert, clientKey: clientKey, caCert: caCert},
	)
	return s.result
}

// mustGenerateClusterPKI generates real cluster PKI material for the given
// control-plane IP and machine name, failing the test on error. The readiness
// tests need parseable certificates because the kubeconfig the reconciler
// renders must round-trip its CA.
func mustGenerateClusterPKI(t *testing.T, cpIP, cpName string) pki.ClusterPKI {
	t.Helper()
	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI(%q, %q): %v", cpIP, cpName, err)
	}
	return pk
}

// getControlPlane reads the control plane back from the API store so the
// readiness assertions observe the persisted status and conditions.
func getControlPlane(
	t *testing.T,
	c client.Client,
	cp *controlplanev1alpha1.HypervisorControlPlane,
) *controlplanev1alpha1.HypervisorControlPlane {
	t.Helper()
	got := &controlplanev1alpha1.HypervisorControlPlane{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(cp), got); err != nil {
		t.Fatalf("Get HypervisorControlPlane: %v", err)
	}
	return got
}

// wantCPStatus fails the test unless the control plane reports exactly the
// expected initialized and ready flags.
func wantCPStatus(t *testing.T, cp *controlplanev1alpha1.HypervisorControlPlane, initialized, ready bool) {
	t.Helper()
	if cp.Status.Initialized != initialized {
		t.Errorf("status.initialized = %v, want %v", cp.Status.Initialized, initialized)
	}
	if cp.Status.Ready != ready {
		t.Errorf("status.ready = %v, want %v", cp.Status.Ready, ready)
	}
}

// wantControlPlaneReadyCondition fails the test unless the control plane
// carries a ControlPlaneReady condition with exactly the given status.
func wantControlPlaneReadyCondition(
	t *testing.T,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	status metav1.ConditionStatus,
) {
	t.Helper()
	for _, cond := range cp.Status.Conditions {
		if cond.Type != controlPlaneReadyCondition {
			continue
		}
		if cond.Status != status {
			t.Errorf("ControlPlaneReady condition status = %q, want %q", cond.Status, status)
		}
		return
	}
	t.Errorf("control plane has no %s condition (conditions %+v)", controlPlaneReadyCondition, cp.Status.Conditions)
}

// wantControlPlaneReadyNotTrue fails the test unless the control plane has no
// ControlPlaneReady condition reporting True.
func wantControlPlaneReadyNotTrue(t *testing.T, cp *controlplanev1alpha1.HypervisorControlPlane) {
	t.Helper()
	for _, cond := range cp.Status.Conditions {
		if cond.Type == controlPlaneReadyCondition && cond.Status == metav1.ConditionTrue {
			t.Errorf(
				"control plane reports %s=True (conditions %+v), want no ready condition",
				controlPlaneReadyCondition,
				cp.Status.Conditions,
			)
			return
		}
	}
}

// kubeconfigSecretKey returns the conventional key of the
// <cluster-name>-kubeconfig Secret in the given namespace.
func kubeconfigSecretKey(clusterName, namespace string) client.ObjectKey {
	return client.ObjectKey{Namespace: namespace, Name: clusterName + "-kubeconfig"}
}

// wantKubeconfigSecret fails the test unless the conventional kubeconfig
// Secret exists and returns it.
func wantKubeconfigSecret(t *testing.T, c client.Client, key client.ObjectKey) *corev1.Secret {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(t.Context(), key, secret); err != nil {
		t.Fatalf("Get kubeconfig Secret %s: %v", key, err)
	}
	return secret
}

// wantNoKubeconfigSecret fails the test unless no conventional kubeconfig
// Secret exists.
func wantNoKubeconfigSecret(t *testing.T, c client.Client, key client.ObjectKey) {
	t.Helper()
	if err := c.Get(t.Context(), key, &corev1.Secret{}); err == nil {
		t.Errorf("kubeconfig Secret %s exists, want none", key)
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("Get kubeconfig Secret %s: %v", key, err)
	}
}

// kubeconfigYAML is the subset of a rendered kubeconfig document the
// readiness tests read back: the cluster server URL and CA data.
type kubeconfigYAML struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Clusters   []struct {
		Name    string `yaml:"name"`
		Cluster struct {
			Server                   string `yaml:"server"`
			CertificateAuthorityData string `yaml:"certificate-authority-data"`
		} `yaml:"cluster"`
	} `yaml:"clusters"`
}

// parseKubeconfig decodes a rendered kubeconfig document.
func parseKubeconfig(t *testing.T, data []byte) kubeconfigYAML {
	t.Helper()
	doc := kubeconfigYAML{}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("decode kubeconfig document: %v", err)
	}
	return doc
}

// newControlPlaneInfraMachine returns the linked HypervisorMachine for one
// control-plane Machine, the way the infra controller would after the VM
// boots: the conventional same-name infrastructure object owned by the CAPI
// Machine, reporting ip as its InternalIP when ip is non-empty (an empty ip
// leaves the VM not yet up). The control-plane reconciler instantiates the
// object itself since the template-cloning fix, so the helper adopts the
// existing object when present instead of creating a duplicate.
func newControlPlaneInfraMachine(
	t *testing.T,
	c client.Client,
	lcp *linkedControlPlane,
	machineName, ip string,
) *infrastructurev1alpha1.HypervisorMachine {
	t.Helper()
	ctx := t.Context()

	machine := &clusterv1.Machine{}
	if err := c.Get(ctx, client.ObjectKey{Namespace: lcp.namespace, Name: machineName}, machine); err != nil {
		t.Fatalf("Get Machine %q: %v", machineName, err)
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	err := c.Get(ctx, client.ObjectKey{Namespace: lcp.namespace, Name: machineName}, hm)
	switch {
	case apierrors.IsNotFound(err):
		hm = &infrastructurev1alpha1.HypervisorMachine{
			ObjectMeta: metav1.ObjectMeta{
				Name:      machineName,
				Namespace: lcp.namespace,
				OwnerReferences: []metav1.OwnerReference{
					{
						APIVersion: clusterv1.GroupVersion.String(),
						Kind:       "Machine",
						Name:       machine.Name,
						UID:        machine.UID,
					},
				},
			},
			Spec: infrastructurev1alpha1.HypervisorMachineSpec{ClusterName: machine.Spec.ClusterName},
		}
		if err := c.Create(ctx, hm); err != nil {
			t.Fatalf("create HypervisorMachine: %v", err)
		}
	case err != nil:
		t.Fatalf("get HypervisorMachine %q: %v", machineName, err)
	}
	if ip == "" {
		return hm
	}
	hm.Status.Addresses = []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: ip}}
	if err := c.Status().Update(ctx, hm); err != nil {
		t.Fatalf("set HypervisorMachine addresses: %v", err)
	}
	return hm
}

// wantRequeue fails the test unless the reconcile result requests a retry.
func wantRequeue(t *testing.T, res ctrl.Result) {
	t.Helper()
	if res.RequeueAfter <= 0 {
		t.Errorf("Reconcile result %+v does not requeue, want a requeue", res)
	}
}

// newReadinessControlPlane wires a control plane for the readiness contract
// with real generated PKI: the cluster linkage, the template, the control
// plane, and a first reconcile that creates the Machine, its bootstrap
// config, and the cluster PKI Secret. The healthz seam returns healthResult
// on every poll.
func newReadinessControlPlane(
	t *testing.T,
	c client.Client,
	namespace string,
	healthResult error,
) (*controlPlaneFixture, *linkedCluster, *linkedControlPlane, pki.ClusterPKI) {
	t.Helper()
	lc := newLinkedCluster(t, c, namespace, "capi-cluster")
	machineName := lc.name + "-cp-0"
	pk := mustGenerateClusterPKI(t, testCPIP, machineName)
	fx := newControlPlaneFixtureWithPKI(t, c, pk)
	fx.health.result = healthResult
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	return fx, lc, lcp, pk
}

// TestControlPlaneReadinessWritesKubeconfig pins the happy-path readiness
// contract: once the first control-plane Machine's VM reports an address, the
// reconciler polls the apiserver healthz at the machine's IP with the cluster
// PKI material, and on a healthy response writes the conventional
// <cluster>-kubeconfig Secret, sets initialized and ready, and reports the
// ControlPlaneReady condition. A second reconcile converges: no duplicate
// Secret and no error.
func TestControlPlaneReadinessWritesKubeconfig(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp, pk := newReadinessControlPlane(t, c, "cp-readiness-healthy", nil)

	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	// REQ-009: the kubeconfig endpoint comes from the recorded 6443
	// allocation, so the readiness fixture records one.
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{VMPort: 6443, HostPort: 26443})
	fx.reconcileControlPlane(t, lcp.cp)

	if len(fx.health.calls) == 0 {
		t.Fatal("apiserver healthz seam never called")
	}
	call := fx.health.calls[0]
	if call.host != "127.0.0.1" || call.port != 26443 {
		t.Errorf("healthz polled endpoint %s:%d, want 127.0.0.1:26443 (the recorded allocation)", call.host, call.port)
	}
	if !bytes.Equal(call.caCert, pk.CA) {
		t.Errorf("healthz CA does not match the cluster PKI CA")
	}
	if len(call.clientCert) == 0 || len(call.clientKey) == 0 {
		t.Errorf("healthz client material empty (cert %d bytes, key %d bytes)", len(call.clientCert), len(call.clientKey))
	}

	secret := wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	data, ok := secret.Data[kubeconfigSecretDataKey]
	if !ok {
		t.Fatalf("kubeconfig Secret has no %q data key (keys %v)", kubeconfigSecretDataKey, secret.Data)
	}
	doc := parseKubeconfig(t, data)
	wantServer := "https://host.containers.internal:26443"
	if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != wantServer {
		t.Errorf("kubeconfig server = %+v, want %q", doc.Clusters, wantServer)
	}

	// CAPI core's secret cache filters by cluster-name label; the Secret
	// must carry it so the ClusterCache can find the kubeconfig.
	if got := secret.Labels[clusterv1.ClusterNameLabel]; got != lc.name {
		t.Errorf("kubeconfig Secret label %s = %q, want %q", clusterv1.ClusterNameLabel, got, lc.name)
	}

	got := getControlPlane(t, c, lcp.cp)
	wantCPStatus(t, got, true, true)
	wantControlPlaneReadyCondition(t, got, metav1.ConditionTrue)

	// A second reconcile converges: the Secret is not duplicated and the
	// status stays ready.
	fx.reconcileControlPlane(t, lcp.cp)
	if count := countSecretsNamed(t, c, lc.namespace, lc.name+"-kubeconfig"); count != 1 {
		t.Errorf("kubeconfig Secrets = %d after second reconcile, want 1", count)
	}
	got = getControlPlane(t, c, lcp.cp)
	wantCPStatus(t, got, true, true)
}

// TestControlPlaneReadinessRespectsTimeout pins the bounded-poll contract: a
// never-healthy apiserver ends the reconcile in a requeue, the kubeconfig
// Secret is never written, and initialized, ready, and the ControlPlaneReady
// condition all stay unset.
func TestControlPlaneReadinessRespectsTimeout(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp, _ := newReadinessControlPlane(t, c, "cp-readiness-timeout", errAPIServerNotReady)

	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	// The probe targets the recorded published endpoint, so the fixture
	// records the 6443 allocation.
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{VMPort: 6443, HostPort: 26443})

	res, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp.cp)})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	wantRequeue(t, res)

	if len(fx.health.calls) == 0 {
		t.Error("apiserver healthz seam never called")
	} else if call := fx.health.calls[0]; call.host != "127.0.0.1" || call.port != 26443 {
		t.Errorf("healthz polled endpoint %s:%d, want 127.0.0.1:26443 (the recorded allocation)", call.host, call.port)
	}

	wantNoKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	got := getControlPlane(t, c, lcp.cp)
	wantCPStatus(t, got, false, false)
	wantControlPlaneReadyNotTrue(t, got)
}

// TestControlPlaneReadinessFailureLeavesKubeconfigAbsent pins the failure
// contract: a failing healthz check is treated like a not-yet-healthy poll —
// the reconcile requeues, the kubeconfig Secret is never written, and
// initialized, ready, and the ready condition stay unset.
func TestControlPlaneReadinessFailureLeavesKubeconfigAbsent(t *testing.T) {
	c := mustReconcileClient(t)
	healthErr := errors.New("fake: apiserver healthz check failed")
	fx, lc, lcp, _ := newReadinessControlPlane(t, c, "cp-readiness-failure", healthErr)

	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	// The probe targets the recorded published endpoint, so the fixture
	// records the 6443 allocation.
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{VMPort: 6443, HostPort: 26443})

	res, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp.cp)})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	wantRequeue(t, res)

	if len(fx.health.calls) == 0 {
		t.Error("apiserver healthz seam never called")
	}

	wantNoKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	got := getControlPlane(t, c, lcp.cp)
	wantCPStatus(t, got, false, false)
	wantControlPlaneReadyNotTrue(t, got)
}

// TestControlPlaneReadinessWaitsForMachineAddresses pins the VM-not-up gate:
// before the linked HypervisorMachine reports an InternalIP address, the
// reconciler must not poll the apiserver, must not write the kubeconfig
// Secret, must leave initialized and ready false, and must requeue so the
// later boot is eventually noticed.
func TestControlPlaneReadinessWaitsForMachineAddresses(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp, _ := newReadinessControlPlane(t, c, "cp-readiness-waiting", nil)

	newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", "")

	res, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcp.cp)})
	if err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
	wantRequeue(t, res)

	if len(fx.health.calls) != 0 {
		t.Errorf("apiserver healthz seam called %d times with no VM address, want 0", len(fx.health.calls))
	}

	wantNoKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	got := getControlPlane(t, c, lcp.cp)
	wantCPStatus(t, got, false, false)
	wantControlPlaneReadyNotTrue(t, got)
}

// TestControlPlaneReadinessKubeconfigContent pins the rendered kubeconfig
// content: the document is a Config whose single cluster entry serves the
// recorded 6443 allocation on loopback and whose certificate-authority-data is
// exactly the base64 encoding of the cluster PKI CA.
func TestControlPlaneReadinessKubeconfigContent(t *testing.T) {
	c := mustReconcileClient(t)
	fx, lc, lcp, pk := newReadinessControlPlane(t, c, "cp-readiness-content", nil)

	hm := newControlPlaneInfraMachine(t, c, lcp, lcp.name+"-0", testCPIP)
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{VMPort: 6443, HostPort: 26443})
	fx.reconcileControlPlane(t, lcp.cp)

	secret := wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	data, ok := secret.Data[kubeconfigSecretDataKey]
	if !ok {
		t.Fatalf("kubeconfig Secret has no %q data key (keys %v)", kubeconfigSecretDataKey, secret.Data)
	}
	doc := parseKubeconfig(t, data)
	if doc.APIVersion != "v1" || doc.Kind != "Config" {
		t.Errorf("kubeconfig apiVersion/kind = %q/%q, want v1/Config", doc.APIVersion, doc.Kind)
	}
	if len(doc.Clusters) != 1 {
		t.Fatalf("kubeconfig has %d cluster entries, want 1", len(doc.Clusters))
	}
	wantServer := "https://host.containers.internal:26443"
	if got := doc.Clusters[0].Cluster.Server; got != wantServer {
		t.Errorf("kubeconfig server = %q, want %q", got, wantServer)
	}
	wantCA := base64.StdEncoding.EncodeToString(pk.CA)
	if got := doc.Clusters[0].Cluster.CertificateAuthorityData; got != wantCA {
		t.Errorf("kubeconfig certificate-authority-data does not match the cluster PKI CA")
	}
}

// Scale contract (test-first, red).
//
// This section pins the control-plane scale contract: bumping spec.replicas
// creates the surplus Machines, reducing it deletes the surplus Machines, the
// replica counters in status track the created Machine set, and spec.version
// is propagated to status.version. The reconciler is exercised through the
// same envtest fixture and recording seams as the machine-creation suite; the
// scale assertions read the API store and the control plane status after
// direct Reconcile calls.
//
// The contract, in prose:
//
//   - Scale-up: after a reconcile with replicas 1, bumping spec.replicas to 2
//     and reconciling again creates exactly one additional Machine, named
//     <control-plane-name>-1, carrying the same labels, infrastructure ref,
//     and bootstrap wiring as the first replica (its configRef names the
//     generated <machine-name>-config HypervisorConfig, which exists in the
//     API store). A further reconcile converges: no additional Machine and no
//     additional creation-seam invocation.
//   - Scale-down: after a reconcile with replicas 2, reducing spec.replicas to
//     1 and reconciling again deletes the surplus Machine (the one beyond the
//     desired count, <control-plane-name>-1) so the API store holds exactly
//     the retained Machines. The retained Machine keeps its bootstrap ref and
//     the referenced config still exists, and the scale-down reconcile creates
//     nothing.
//   - Replica counters: status.replicas equals the number of created Machines,
//     status.updatedReplicas equals the same number (every Machine in this lab
//     carries the current template), status.readyReplicas equals the number of
//     Machines whose linked VM is ready (none in this suite, which never boots
//     a VM), and status.unavailableReplicas equals
//     status.replicas - status.readyReplicas. The counters are idempotent
//     across reconciles and re-pin after a scale-down.
//   - Version: status.version is the spec.version value, propagated on
//     reconcile and kept in sync when spec.version changes.
//
// updateControlPlaneSpec applies mutate to a fresh copy of the control plane
// and persists it, returning the updated object for the caller to reconcile
// against.
func updateControlPlaneSpec(
	t *testing.T,
	c client.Client,
	cp *controlplanev1alpha1.HypervisorControlPlane,
	mutate func(*controlplanev1alpha1.HypervisorControlPlane),
) *controlplanev1alpha1.HypervisorControlPlane {
	t.Helper()
	got := getControlPlane(t, c, cp)
	mutate(got)
	if err := c.Update(t.Context(), got); err != nil {
		t.Fatalf("update HypervisorControlPlane %s: %v", client.ObjectKeyFromObject(cp), err)
	}

	return got
}

// wantMachineGone fails the test unless no Machine with the given name exists
// in the namespace.
func wantMachineGone(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &clusterv1.Machine{}); err == nil {
		t.Errorf("Machine %s/%s exists, want it deleted", namespace, name)
	} else if !apierrors.IsNotFound(err) {
		t.Fatalf("Get Machine %s/%s: %v", namespace, name, err)
	}
}

// wantBootstrapConfigExists fails the test unless the HypervisorConfig with
// the given name exists in the namespace.
func wantBootstrapConfigExists(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()
	if err := c.Get(t.Context(), client.ObjectKey{Namespace: namespace, Name: name}, &bootstrapv1alpha1.HypervisorConfig{}); err != nil {
		t.Errorf("HypervisorConfig %s/%s missing: %v", namespace, name, err)
	}
}

// wantControlPlaneScaleStatus fails the test unless the control plane reports
// the pinned scale counters for a created Machine set: status.replicas and
// status.updatedReplicas equal created, status.readyReplicas equals ready
// (the number of Machines whose linked VM is ready), and
// status.unavailableReplicas equals created - ready.
func wantControlPlaneScaleStatus(t *testing.T, cp *controlplanev1alpha1.HypervisorControlPlane, created, ready int32) {
	t.Helper()
	if cp.Status.Replicas != created {
		t.Errorf("status.replicas = %d, want %d", cp.Status.Replicas, created)
	}
	if cp.Status.UpdatedReplicas != created {
		t.Errorf("status.updatedReplicas = %d, want %d", cp.Status.UpdatedReplicas, created)
	}
	if cp.Status.ReadyReplicas != ready {
		t.Errorf("status.readyReplicas = %d, want %d", cp.Status.ReadyReplicas, ready)
	}
	if want := created - ready; cp.Status.UnavailableReplicas != want {
		t.Errorf("status.unavailableReplicas = %d, want %d", cp.Status.UnavailableReplicas, want)
	}
}

// wantControlPlaneVersion fails the test unless the control plane reports the
// given status.version value.
func wantControlPlaneVersion(t *testing.T, cp *controlplanev1alpha1.HypervisorControlPlane, want string) {
	t.Helper()
	if cp.Status.Version == nil {
		t.Errorf("status.version is nil, want %q", want)
		return
	}
	if *cp.Status.Version != want {
		t.Errorf("status.version = %q, want %q", *cp.Status.Version, want)
	}
}

// TestControlPlaneScaleUp pins the scale-up contract: bumping spec.replicas
// from 1 to 2 creates exactly one additional Machine with the deterministic
// name pattern and the same bootstrap wiring as the first replica, and a
// further reconcile converges instead of duplicating.
func TestControlPlaneScaleUp(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-scale-up", "capi-cluster")
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	if machines := listControlPlaneMachines(t, c, lc.namespace, lc.name); len(machines) != 1 {
		t.Fatalf("created %d Machines before scale-up, want 1 (names %v)", len(machines), machineNamesOf(machines))
	}

	lcp.cp = updateControlPlaneSpec(t, c, lcp.cp, func(cp *controlplanev1alpha1.HypervisorControlPlane) {
		cp.Spec.Replicas = 2
	})
	fx.reconcileControlPlane(t, lcp.cp)

	machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 2 {
		t.Fatalf("created %d Machines after scale-up, want 2 (names %v)", len(machines), machineNamesOf(machines))
	}
	want := []string{lcp.name + "-0", lcp.name + "-1"}
	if !sameStringSet(machineNamesOf(machines), want) {
		t.Errorf("Machine names after scale-up = %v, want %v", machineNamesOf(machines), want)
	}

	var second *clusterv1.Machine
	for i := range machines {
		m := &machines[i]
		wantMachineLabels(t, m, lc.name, nil)
		if m.Name != lcp.name+"-1" {
			continue
		}
		second = m
	}
	if second == nil {
		t.Fatal("scale-up did not create Machine " + lcp.name + "-1")
	}
	if second.Spec.Bootstrap.ConfigRef == (clusterv1.ContractVersionedObjectReference{}) {
		t.Fatal("scale-up Machine spec.bootstrap.configRef is unset")
	}
	ref := second.Spec.Bootstrap.ConfigRef
	wantConfigName := machineConfigName(second.Name)
	if ref.Name != wantConfigName || ref.Kind != "HypervisorConfig" {
		t.Errorf("scale-up Machine configRef = %+v, want HypervisorConfig %q", ref, wantConfigName)
	}
	wantBootstrapConfigExists(t, c, lc.namespace, wantConfigName)

	if len(fx.createMachine.calls) != 2 {
		t.Errorf("CreateMachine called %d times across scale-up, want 2", len(fx.createMachine.calls))
	}

	// A further reconcile converges: no duplicate Machine, no extra creation.
	fx.reconcileControlPlane(t, lcp.cp)
	if got := listControlPlaneMachines(t, c, lc.namespace, lc.name); len(got) != 2 {
		t.Errorf("reconcile after scale-up produced %d Machines, want 2 (names %v)", len(got), machineNamesOf(got))
	}
	if len(fx.createMachine.calls) != 2 {
		t.Errorf("CreateMachine called %d times across three reconciles, want 2", len(fx.createMachine.calls))
	}
}

// TestControlPlaneScaleDown pins the scale-down contract: reducing
// spec.replicas from 2 to 1 deletes the surplus Machine so the API store
// holds exactly the retained set, the retained Machine keeps its bootstrap
// ref and config, and the scale-down reconcile creates nothing.
func TestControlPlaneScaleDown(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-scale-down", "capi-cluster")
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 2, nil)

	fx.reconcileControlPlane(t, lcp.cp)

	machines := listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 2 {
		t.Fatalf("created %d Machines before scale-down, want 2 (names %v)", len(machines), machineNamesOf(machines))
	}

	lcp.cp = updateControlPlaneSpec(t, c, lcp.cp, func(cp *controlplanev1alpha1.HypervisorControlPlane) {
		cp.Spec.Replicas = 1
	})
	fx.reconcileControlPlane(t, lcp.cp)

	wantMachineGone(t, c, lc.namespace, lcp.name+"-1")

	machines = listControlPlaneMachines(t, c, lc.namespace, lc.name)
	if len(machines) != 1 {
		t.Fatalf("%d Machines after scale-down, want 1 (names %v)", len(machines), machineNamesOf(machines))
	}
	retained := machines[0]
	if retained.Name != lcp.name+"-0" {
		t.Errorf("retained Machine = %q, want %q", retained.Name, lcp.name+"-0")
	}
	if retained.Spec.Bootstrap.ConfigRef == (clusterv1.ContractVersionedObjectReference{}) {
		t.Fatal("retained Machine spec.bootstrap.configRef is unset after scale-down")
	}
	ref := retained.Spec.Bootstrap.ConfigRef
	if ref.Name != machineConfigName(retained.Name) || ref.Kind != "HypervisorConfig" {
		t.Errorf(
			"retained Machine configRef = %+v, want HypervisorConfig %q",
			ref,
			machineConfigName(retained.Name),
		)
	}
	wantBootstrapConfigExists(t, c, lc.namespace, machineConfigName(retained.Name))

	// The scale-down reconcile creates nothing: the retained Machine already
	// exists and the surplus one is deleted, so the creation seam is not
	// invoked again.
	if len(fx.createMachine.calls) != 2 {
		t.Errorf("CreateMachine called %d times across scale-down, want 2", len(fx.createMachine.calls))
	}
}

// TestControlPlaneScaleCounters pins the replica counter contract at the
// creation level: status.replicas and status.updatedReplicas equal the number
// of created Machines, status.readyReplicas is zero because no VM in this
// suite ever boots, and status.unavailableReplicas equals the difference. The
// counters are idempotent across reconciles and re-pin after a scale-down.
func TestControlPlaneScaleCounters(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("counters track the created machines", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-scale-counters", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 3, nil)

		fx.reconcileControlPlane(t, lcp.cp)

		wantControlPlaneScaleStatus(t, getControlPlane(t, c, lcp.cp), 3, 0)

		// A second reconcile converges: the counters are written again with
		// the same values.
		fx.reconcileControlPlane(t, lcp.cp)
		wantControlPlaneScaleStatus(t, getControlPlane(t, c, lcp.cp), 3, 0)
	})

	t.Run("counters re-pin after scale-down", func(t *testing.T) {
		fx := newControlPlaneFixture(t, c)
		lc := newLinkedCluster(t, c, "cp-scale-counters-down", "capi-cluster")
		lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 2, nil)

		fx.reconcileControlPlane(t, lcp.cp)
		wantControlPlaneScaleStatus(t, getControlPlane(t, c, lcp.cp), 2, 0)

		lcp.cp = updateControlPlaneSpec(t, c, lcp.cp, func(cp *controlplanev1alpha1.HypervisorControlPlane) {
			cp.Spec.Replicas = 1
		})
		fx.reconcileControlPlane(t, lcp.cp)

		wantControlPlaneScaleStatus(t, getControlPlane(t, c, lcp.cp), 1, 0)
	})
}

// TestControlPlaneScaleVersion pins the version propagation contract:
// status.version carries the spec.version value after reconcile and follows
// spec.version when it changes.
func TestControlPlaneScaleVersion(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newControlPlaneFixture(t, c)
	lc := newLinkedCluster(t, c, "cp-scale-version", "capi-cluster")
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)

	fx.reconcileControlPlane(t, lcp.cp)
	wantControlPlaneVersion(t, getControlPlane(t, c, lcp.cp), "v1.35.4")

	lcp.cp = updateControlPlaneSpec(t, c, lcp.cp, func(cp *controlplanev1alpha1.HypervisorControlPlane) {
		cp.Spec.Version = "v1.36.0"
	})
	fx.reconcileControlPlane(t, lcp.cp)
	wantControlPlaneVersion(t, getControlPlane(t, c, lcp.cp), "v1.36.0")
}

// TASK-011 VC-06 REQ-006 — endpoint + PKI: kubeconfig and readiness use loopback.
//
// Grill-me: reserved IP is dynamic (not hardcoded .20); rendered kubeconfig must
// be loopback at the recorded 6443 allocation (REQ-009) even when the VM's
// InternalIP differs; the healthz seam polls the same published loopback
// endpoint (TASK-021 P3: the VM IP has no host route); second reconcile
// converges without duplicating the Secret.
// RED: current impl renders https://<internal-IP>:6443, so the server assertion fails.
func TestControlPlaneKubeconfigServerIsLoopback(t *testing.T) {
	c := mustReconcileClient(t)
	// Use a non-default reserved IP to prove not hardcoded .20.
	const reservedIP = "192.168.124.77"
	const wantServer = "https://host.containers.internal:26443"
	lc := newLinkedCluster(t, c, "cp-kubeconfig-loopback", "capi-cluster")
	machineName := lc.name + "-cp-0"
	pk := mustGenerateClusterPKI(t, reservedIP, machineName)
	fx := newControlPlaneFixtureWithPKI(t, c, pk)
	fx.health.result = nil
	lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)
	// First reconcile creates Machine + PKI Secret.
	fx.reconcileControlPlane(t, lcp.cp)
	// VM boots with the reserved internal IP (simulating AllocateIP result).
	hm := newControlPlaneInfraMachine(t, c, lcp, machineName, reservedIP)
	setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{VMPort: 6443, HostPort: 26443})
	fx.reconcileControlPlane(t, lcp.cp)

	if len(fx.health.calls) == 0 {
		t.Fatal("healthz seam never called")
	}
	// Health check must be polled through the published loopback endpoint,
	// never the VM internal IP (no host route into the k8netd L2 segment).
	if call := fx.health.calls[0]; call.host != "127.0.0.1" || call.port != 26443 {
		t.Errorf(
			"healthz polled endpoint %s:%d, want 127.0.0.1:26443 (published allocation, not the VM IP)",
			call.host,
			call.port,
		)
	}

	secret := wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	data, ok := secret.Data[kubeconfigSecretDataKey]
	if !ok {
		t.Fatalf("kubeconfig Secret missing %q key", kubeconfigSecretDataKey)
	}
	doc := parseKubeconfig(t, data)
	if len(doc.Clusters) != 1 || doc.Clusters[0].Cluster.Server != wantServer {
		t.Errorf(
			"kubeconfig server = %q, want %q (REQ-006 VC-06: must be loopback)",
			doc.Clusters[0].Cluster.Server,
			wantServer,
		)
	}
	// Prove not hardcoded to old default: server must not be https://192.168.124.20:6443
	if doc.Clusters[0].Cluster.Server == fmt.Sprintf("https://%s:%d", testCPIP, testCPPort) {
		t.Errorf("kubeconfig server is still the old default %q, want loopback", doc.Clusters[0].Cluster.Server)
	}
	// Second reconcile converges: no duplicate Secret, still loopback.
	fx.reconcileControlPlane(t, lcp.cp)
	secret = wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
	data = secret.Data[kubeconfigSecretDataKey]
	doc = parseKubeconfig(t, data)
	if doc.Clusters[0].Cluster.Server != wantServer {
		t.Errorf("after second reconcile kubeconfig server = %q, want %q", doc.Clusters[0].Cluster.Server, wantServer)
	}
	if count := countSecretsNamed(t, c, lc.namespace, lc.name+"-kubeconfig"); count != 1 {
		t.Errorf("kubeconfig Secrets = %d after second reconcile, want 1", count)
	}
}

// TestControlPlaneKubeconfigLoopbackWithDifferentReservedIPs ensures the
// loopback contract holds regardless of which reserved IP AllocateIP returns.
func TestControlPlaneKubeconfigLoopbackWithDifferentReservedIPs(t *testing.T) {
	for _, reservedIP := range []string{"192.168.124.50", "192.168.124.90"} {
		t.Run(reservedIP, func(t *testing.T) {
			c := mustReconcileClient(t)
			ns := "cp-kubeconfig-loopback-" + reservedIP[len(reservedIP)-2:]
			lc := newLinkedCluster(t, c, ns, "capi-cluster")
			machineName := lc.name + "-cp-0"
			pk := mustGenerateClusterPKI(t, reservedIP, machineName)
			fx := newControlPlaneFixtureWithPKI(t, c, pk)
			lcp := newLinkedControlPlane(t, c, lc, lc.name+"-cp", 1, nil)
			fx.reconcileControlPlane(t, lcp.cp)
			hm := newControlPlaneInfraMachine(t, c, lcp, machineName, reservedIP)
			setHMPublishedPorts(t, c, hm, infrastructurev1alpha1.MachinePublishedPort{VMPort: 6443, HostPort: 26443})
			fx.reconcileControlPlane(t, lcp.cp)
			secret := wantKubeconfigSecret(t, c, kubeconfigSecretKey(lc.name, lc.namespace))
			doc := parseKubeconfig(t, secret.Data[kubeconfigSecretDataKey])
			if doc.Clusters[0].Cluster.Server != "https://host.containers.internal:26443" {
				t.Errorf(
					"reserved %s: kubeconfig server = %q, want https://host.containers.internal:26443",
					reservedIP,
					doc.Clusters[0].Cluster.Server,
				)
			}
		})
	}
}
