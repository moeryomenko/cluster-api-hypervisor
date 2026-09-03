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

// Package templates holds the structural contract tests for the ClusterClass
// and the example Cluster that this directory ships.
//
// The contract is test-first: the two YAML manifests are written by the
// implementation task and must satisfy every invariant below. Until they
// exist every test fails at the file read, which is the intended red phase.
//
// The pinned shapes follow the committed API groups:
//
//   - infrastructure.cluster.x-k8s.io/v1alpha1: HypervisorCluster,
//     HypervisorMachine, HypervisorMachineTemplate, plus the
//     ClusterClass-compatible HypervisorClusterTemplate
//   - bootstrap.cluster.x-k8s.io/v1alpha1: HypervisorConfig plus the
//     ClusterClass-compatible HypervisorConfigTemplate
//   - controlplane.cluster.x-k8s.io/v1alpha1: HypervisorControlPlane plus the
//     ClusterClass-compatible HypervisorControlPlaneTemplate
//   - cluster.x-k8s.io/v1beta2: ClusterClass, Cluster
//
// CAPI core requires every ClusterClass template ref kind to end in
// "Template", so the class references the three *Template kinds; the concrete
// kinds are only ever produced by cloning spec.template of those templates.
// The ControlPlaneClass carries no bootstrap template field: the
// HypervisorConfig for control-plane Machines is generated at runtime by the
// HypervisorControlPlane controller, so the manifest must not invent a
// machineBootstrap key under spec.controlPlane.
package templates

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
	"k8s.io/apimachinery/pkg/runtime/schema"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/yaml"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrav1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

const (
	clusterClassFile   = "clusterclass.yaml"
	exampleClusterFile = "cluster-example.yaml"
)

// committedKinds maps every committed API Kind that a ClusterClass template
// ref may target to the group/version the templates are allowed to reference.
// Every kind ends in "Template" per the CAPI ClusterClass contract; a
// reference to any other kind or to the wrong group/version is dangling.
var committedKinds = map[string]schema.GroupVersion{
	"HypervisorClusterTemplate":      infrav1alpha1.GroupVersion,
	"HypervisorMachineTemplate":      infrav1alpha1.GroupVersion,
	"HypervisorControlPlaneTemplate": controlplanev1alpha1.GroupVersion,
	"HypervisorConfigTemplate":       bootstrapv1alpha1.GroupVersion,
}

func TestClusterClassExistsAndParses(t *testing.T) {
	var cc clusterv1.ClusterClass
	// The file is a multi-document stream: the ClusterClass plus the three
	// template objects its refs point at.
	if !findDocument(t, clusterClassFile, "ClusterClass", clusterv1.GroupVersion.String(), &cc) {
		t.Fatalf(
			"%s must contain a ClusterClass document with apiVersion %s",
			clusterClassFile,
			clusterv1.GroupVersion.String(),
		)
	}

	if cc.Name == "" {
		t.Fatal("metadata.name must be set: the example Cluster references the class by this name")
	}
}

func TestClusterClassInfrastructureRef(t *testing.T) {
	cc := parseClusterClass(t)

	ref := requireTemplateRef(t, cc.Spec.Infrastructure.TemplateRef, "spec.infrastructure.templateRef")
	assertTemplateRefTarget(t, ref, "HypervisorClusterTemplate", infrav1alpha1.GroupVersion)
}

func TestClusterClassControlPlaneClass(t *testing.T) {
	cc := parseClusterClass(t)

	ref := requireTemplateRef(t, cc.Spec.ControlPlane.TemplateRef, "spec.controlPlane.templateRef")
	assertTemplateRefTarget(t, ref, "HypervisorControlPlaneTemplate", controlplanev1alpha1.GroupVersion)

	miRef := requireTemplateRef(
		t,
		cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef,
		"spec.controlPlane.machineInfrastructure.templateRef",
	)
	assertTemplateRefTarget(t, miRef, "HypervisorMachineTemplate", infrav1alpha1.GroupVersion)

	// The ControlPlaneClass carries no bootstrap template field: the
	// HypervisorConfig for control-plane Machines is generated at runtime by
	// the HypervisorControlPlane controller, so the manifest must not invent
	// a machineBootstrap key under spec.controlPlane.
	cpRaw := nestedMap(t, rawClusterClass(t), "spec", "controlPlane")
	if _, ok := cpRaw["machineBootstrap"]; ok {
		t.Error(
			"spec.controlPlane.machineBootstrap is not part of the ClusterClass API: control-plane bootstrap is generated by the HypervisorControlPlane controller",
		)
	}
}

func TestClusterClassWorkerClass(t *testing.T) {
	cc := parseClusterClass(t)

	classes := cc.Spec.Workers.MachineDeployments
	if len(classes) == 0 {
		t.Fatal("spec.workers.machineDeployments must define at least one worker MachineDeployment class")
	}

	for i := range classes {
		path := fmt.Sprintf("spec.workers.machineDeployments[%d]", i)
		if classes[i].Class == "" {
			t.Errorf("%s: class name must be set", path)
		}

		boot := requireTemplateRef(t, classes[i].Bootstrap.TemplateRef, path+".template.bootstrap.templateRef")
		assertTemplateRefTarget(t, boot, "HypervisorConfigTemplate", bootstrapv1alpha1.GroupVersion)
		infra := requireTemplateRef(t, classes[i].Infrastructure.TemplateRef, path+".template.infrastructure.templateRef")
		assertTemplateRefTarget(t, infra, "HypervisorMachineTemplate", infrav1alpha1.GroupVersion)
	}
}

func TestExampleClusterTopology(t *testing.T) {
	cluster := parseExampleCluster(t)

	topo := cluster.Spec.Topology
	// Topology is a value type in v1beta2: an absent spec.topology decodes to
	// the zero Topology, whose empty classRef.name the next check rejects.
	if topo.ClassRef.Name == "" {
		t.Fatal("spec.topology.classRef.name must name the ClusterClass")
	}

	if topo.Version == "" {
		t.Fatal("spec.topology.version must be set")
	}

	if topo.ControlPlane.Replicas == nil {
		t.Fatal("spec.topology.controlPlane.replicas must be set")
	}

	if *topo.ControlPlane.Replicas != 1 {
		t.Fatalf(
			"spec.topology.controlPlane.replicas must be 1 (single control-plane lab), got %d",
			*topo.ControlPlane.Replicas,
		)
	}

	if len(topo.Workers.MachineDeployments) == 0 {
		t.Fatal("spec.topology.workers.machineDeployments must list at least one worker MachineDeployment")
	}

	for i, md := range topo.Workers.MachineDeployments {
		path := fmt.Sprintf("spec.topology.workers.machineDeployments[%d]", i)
		if md.Name == "" {
			t.Errorf("%s: name must be set", path)
		}

		if md.Class == "" {
			t.Errorf("%s: class must reference a MachineDeployment class from the ClusterClass", path)
		}

		if md.Replicas == nil || *md.Replicas < 1 {
			t.Errorf("%s: replicas must be set to at least 1", path)
		}
	}

	// Cross-file resolution: the topology class and every worker class must
	// exist in the ClusterClass.
	cc := parseClusterClass(t)
	if topo.ClassRef.Name != cc.Name {
		t.Errorf("spec.topology.classRef.name %q does not match the ClusterClass name %q", topo.ClassRef.Name, cc.Name)
	}

	workerClasses := make(map[string]bool, len(cc.Spec.Workers.MachineDeployments))
	for _, class := range cc.Spec.Workers.MachineDeployments {
		workerClasses[class.Class] = true
	}

	for _, md := range topo.Workers.MachineDeployments {
		if !workerClasses[md.Class] {
			t.Errorf("machineDeployment class %q is not defined in the ClusterClass", md.Class)
		}
	}
}

func TestTemplateRefsResolveToCommittedKinds(t *testing.T) {
	cc := parseClusterClass(t)

	type namedRef struct {
		path string
		ref  clusterv1.ClusterClassTemplateReference
	}

	var refs []namedRef

	add := func(path string, ref clusterv1.ClusterClassTemplateReference) {
		if ref.IsDefined() {
			refs = append(refs, namedRef{path: path, ref: ref})
		}
	}
	add("spec.infrastructure.templateRef", cc.Spec.Infrastructure.TemplateRef)
	add("spec.controlPlane.templateRef", cc.Spec.ControlPlane.TemplateRef)
	add("spec.controlPlane.machineInfrastructure.templateRef", cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef)

	for i := range cc.Spec.Workers.MachineDeployments {
		path := fmt.Sprintf("spec.workers.machineDeployments[%d]", i)
		add(path+".template.bootstrap.templateRef", cc.Spec.Workers.MachineDeployments[i].Bootstrap.TemplateRef)
		add(path+".template.infrastructure.templateRef", cc.Spec.Workers.MachineDeployments[i].Infrastructure.TemplateRef)
	}

	if len(refs) == 0 {
		t.Fatal("the ClusterClass must contain at least one template reference")
	}

	for _, nr := range refs {
		gv, ok := committedKinds[nr.ref.Kind]
		if !ok {
			t.Errorf(
				"%s: kind %q is not one of the committed kinds (HypervisorClusterTemplate, HypervisorMachineTemplate, HypervisorControlPlaneTemplate, HypervisorConfigTemplate)",
				nr.path,
				nr.ref.Kind,
			)

			continue
		}

		if nr.ref.APIVersion != gv.String() {
			t.Errorf(
				"%s: apiVersion %q does not match the committed group/version %q for kind %s",
				nr.path,
				nr.ref.APIVersion,
				gv.String(),
				nr.ref.Kind,
			)
		}

		if nr.ref.Name == "" {
			t.Errorf("%s: name must be set (no dangling references)", nr.path)
		}
	}
}

func readTemplate(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("template %s must exist in templates/: %v", path, err)
	}

	return data
}

func parseClusterClass(t *testing.T) clusterv1.ClusterClass {
	t.Helper()

	var cc clusterv1.ClusterClass
	if !findDocument(t, clusterClassFile, "ClusterClass", clusterv1.GroupVersion.String(), &cc) {
		t.Fatalf(
			"%s must contain a ClusterClass document with apiVersion %s",
			clusterClassFile,
			clusterv1.GroupVersion.String(),
		)
	}

	return cc
}

func parseExampleCluster(t *testing.T) clusterv1.Cluster {
	t.Helper()

	var cluster clusterv1.Cluster

	found := false

	for _, doc := range splitYAMLDocuments(t, exampleClusterFile) {
		if err := yaml.Unmarshal(doc, &cluster); err != nil {
			t.Fatalf("%s contains an invalid YAML document: %v", exampleClusterFile, err)
		}

		if cluster.Kind == "Cluster" && cluster.APIVersion == clusterv1.GroupVersion.String() {
			found = true
			break
		}
	}

	if !found {
		t.Fatalf("%s must contain a Cluster document with apiVersion %s", exampleClusterFile, clusterv1.GroupVersion.String())
	}

	return cluster
}

func rawClusterClass(t *testing.T) map[string]any {
	t.Helper()

	var m map[string]any
	if !findDocument(t, clusterClassFile, "ClusterClass", clusterv1.GroupVersion.String(), &m) {
		t.Fatalf(
			"%s must contain a ClusterClass document with apiVersion %s",
			clusterClassFile,
			clusterv1.GroupVersion.String(),
		)
	}

	return m
}

func nestedMap(t *testing.T, m map[string]any, path ...string) map[string]any {
	t.Helper()

	cur := m
	for _, key := range path {
		next, ok := cur[key]
		if !ok {
			t.Fatalf("missing key %q in %v", key, path)
		}

		nm, ok := next.(map[string]any)
		if !ok {
			t.Fatalf("key %q in %v is not a mapping", key, path)
		}

		cur = nm
	}

	return cur
}

// splitYAMLDocuments decodes every top-level document of a (possibly
// multi-document) YAML stream and returns each document re-encoded as its own
// byte slice.
func splitYAMLDocuments(t *testing.T, path string) [][]byte {
	t.Helper()
	dec := yamlv3.NewDecoder(bytes.NewReader(readTemplate(t, path)))

	var docs [][]byte

	for {
		var doc any

		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}

		if err != nil {
			t.Fatalf("%s contains an invalid YAML document: %v", path, err)
		}

		if doc == nil {
			continue
		}

		raw, err := yamlv3.Marshal(doc)
		if err != nil {
			t.Fatalf("%s: re-encoding YAML document: %v", path, err)
		}

		docs = append(docs, raw)
	}
}

func requireTemplateRef(
	t *testing.T,
	ref clusterv1.ClusterClassTemplateReference,
	path string,
) clusterv1.ClusterClassTemplateReference {
	t.Helper()

	if !ref.IsDefined() {
		t.Fatalf("%s must be set", path)
	}

	if ref.Name == "" {
		t.Fatalf("%s must name a template object", path)
	}

	return ref
}

func assertTemplateRefTarget(
	t *testing.T,
	ref clusterv1.ClusterClassTemplateReference,
	kind string,
	gv schema.GroupVersion,
) {
	t.Helper()

	if ref.Kind != kind {
		t.Errorf("ref %s must have kind %s, got %q", ref.Name, kind, ref.Kind)
	}

	if ref.APIVersion != gv.String() {
		t.Errorf("ref %s must have apiVersion %s, got %q", ref.Name, gv.String(), ref.APIVersion)
	}
}

// clusterTemplateFile is the clusterctl default-flavor template under test: a
// multi-document stream whose first document is the ClusterClass
// hypervisor-cluster-template and whose second document is a topology Cluster
// parameterized with clusterctl-style ${VARIABLE} markers.
const clusterTemplateFile = "cluster-template.yaml"

func TestClusterTemplateDocumentOrder(t *testing.T) {
	docs := splitYAMLDocuments(t, clusterTemplateFile)

	// The stream is order-pinned: the ClusterClass first, then the three
	// template objects its refs point at, then the topology Cluster. Every
	// kind must end in "Template" per the CAPI ClusterClass contract except
	// the ClusterClass and Cluster documents themselves.
	want := []struct{ apiVersion, kind string }{
		{clusterv1.GroupVersion.String(), "ClusterClass"},
		{infrav1alpha1.GroupVersion.String(), "HypervisorClusterTemplate"},
		{controlplanev1alpha1.GroupVersion.String(), "HypervisorControlPlaneTemplate"},
		{bootstrapv1alpha1.GroupVersion.String(), "HypervisorConfigTemplate"},
		{clusterv1.GroupVersion.String(), "Cluster"},
	}
	if len(docs) != len(want) {
		t.Fatalf(
			"%s must contain exactly %d YAML documents (%s), got %d",
			clusterTemplateFile,
			len(want),
			"one ClusterClass, the three referenced templates, one Cluster",
			len(docs),
		)
	}

	type docMeta struct {
		APIVersion string `json:"apiVersion"`
		Kind       string `json:"kind"`
	}

	for i, doc := range docs {
		var meta docMeta
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("document %d of %s must parse: %v", i, clusterTemplateFile, err)
		}

		if meta.APIVersion != want[i].apiVersion || meta.Kind != want[i].kind {
			t.Errorf(
				"document %d of %s must be %s %s, got %s %s",
				i,
				clusterTemplateFile,
				want[i].apiVersion,
				want[i].kind,
				meta.APIVersion,
				meta.Kind,
			)
		}
	}
}

func TestClusterTemplateClusterClassInfrastructureRef(t *testing.T) {
	cc := parseClusterTemplateClusterClass(t)

	ref := requireTemplateRef(t, cc.Spec.Infrastructure.TemplateRef, "spec.infrastructure.templateRef")
	assertTemplateRefTarget(t, ref, "HypervisorClusterTemplate", infrav1alpha1.GroupVersion)
}

func TestClusterTemplateClusterClassControlPlaneClass(t *testing.T) {
	cc := parseClusterTemplateClusterClass(t)

	ref := requireTemplateRef(t, cc.Spec.ControlPlane.TemplateRef, "spec.controlPlane.templateRef")
	assertTemplateRefTarget(t, ref, "HypervisorControlPlaneTemplate", controlplanev1alpha1.GroupVersion)

	miRef := requireTemplateRef(
		t,
		cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef,
		"spec.controlPlane.machineInfrastructure.templateRef",
	)
	assertTemplateRefTarget(t, miRef, "HypervisorMachineTemplate", infrav1alpha1.GroupVersion)

	// The ControlPlaneClass carries no bootstrap template field; a
	// machineBootstrap key would be dropped by the API server.
	cpRaw := nestedMap(t, rawClusterTemplateClusterClass(t), "spec", "controlPlane")
	if _, ok := cpRaw["machineBootstrap"]; ok {
		t.Error(
			"spec.controlPlane.machineBootstrap is not part of the ClusterClass API: control-plane bootstrap is generated by the HypervisorControlPlane controller",
		)
	}
}

func TestClusterTemplateClusterClassWorkerClass(t *testing.T) {
	cc := parseClusterTemplateClusterClass(t)

	classes := cc.Spec.Workers.MachineDeployments
	if len(classes) == 0 {
		t.Fatal("spec.workers.machineDeployments must define at least one worker MachineDeployment class")
	}

	for i := range classes {
		path := fmt.Sprintf("spec.workers.machineDeployments[%d]", i)
		if classes[i].Class == "" {
			t.Errorf("%s: class name must be set", path)
		}

		boot := requireTemplateRef(t, classes[i].Bootstrap.TemplateRef, path+".template.bootstrap.templateRef")
		assertTemplateRefTarget(t, boot, "HypervisorConfigTemplate", bootstrapv1alpha1.GroupVersion)
		infra := requireTemplateRef(t, classes[i].Infrastructure.TemplateRef, path+".template.infrastructure.templateRef")
		assertTemplateRefTarget(t, infra, "HypervisorMachineTemplate", infrav1alpha1.GroupVersion)
	}
}

func TestClusterTemplateRefsResolveToCommittedKinds(t *testing.T) {
	cc := parseClusterTemplateClusterClass(t)

	type namedRef struct {
		path string
		ref  clusterv1.ClusterClassTemplateReference
	}

	var refs []namedRef

	add := func(path string, ref clusterv1.ClusterClassTemplateReference) {
		if ref.IsDefined() {
			refs = append(refs, namedRef{path: path, ref: ref})
		}
	}
	add("spec.infrastructure.templateRef", cc.Spec.Infrastructure.TemplateRef)
	add("spec.controlPlane.templateRef", cc.Spec.ControlPlane.TemplateRef)
	add("spec.controlPlane.machineInfrastructure.templateRef", cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef)

	for i := range cc.Spec.Workers.MachineDeployments {
		path := fmt.Sprintf("spec.workers.machineDeployments[%d]", i)
		add(path+".template.bootstrap.templateRef", cc.Spec.Workers.MachineDeployments[i].Bootstrap.TemplateRef)
		add(path+".template.infrastructure.templateRef", cc.Spec.Workers.MachineDeployments[i].Infrastructure.TemplateRef)
	}

	if len(refs) == 0 {
		t.Fatalf("the ClusterClass in %s must contain at least one template reference", clusterTemplateFile)
	}

	for _, nr := range refs {
		gv, ok := committedKinds[nr.ref.Kind]
		if !ok {
			t.Errorf(
				"%s: kind %q is not one of the committed kinds (HypervisorClusterTemplate, HypervisorMachineTemplate, HypervisorControlPlaneTemplate, HypervisorConfigTemplate)",
				nr.path,
				nr.ref.Kind,
			)

			continue
		}

		if nr.ref.APIVersion != gv.String() {
			t.Errorf(
				"%s: apiVersion %q does not match the committed group/version %q for kind %s",
				nr.path,
				nr.ref.APIVersion,
				gv.String(),
				nr.ref.Kind,
			)
		}

		if nr.ref.Name == "" {
			t.Errorf("%s: name must be set (no dangling references)", nr.path)
		}
	}
}

// namedTemplateRef pairs a ClusterClass template reference with the path it
// lives at, so resolution assertions can report precise locations.
type namedTemplateRef struct {
	path string
	ref  clusterv1.ClusterClassTemplateReference
}

// classRefs collects every template reference of a ClusterClass with the
// path it lives at, so resolution assertions can report precise locations.
func classRefs(cc clusterv1.ClusterClass) []namedTemplateRef {
	refs := []namedTemplateRef{
		{path: "spec.infrastructure.templateRef", ref: cc.Spec.Infrastructure.TemplateRef},
		{path: "spec.controlPlane.templateRef", ref: cc.Spec.ControlPlane.TemplateRef},
		{
			path: "spec.controlPlane.machineInfrastructure.templateRef",
			ref:  cc.Spec.ControlPlane.MachineInfrastructure.TemplateRef,
		},
	}
	for i := range cc.Spec.Workers.MachineDeployments {
		path := fmt.Sprintf("spec.workers.machineDeployments[%d]", i)
		refs = append(
			refs,
			namedTemplateRef{
				path: path + ".template.bootstrap.templateRef",
				ref:  cc.Spec.Workers.MachineDeployments[i].Bootstrap.TemplateRef,
			},
			namedTemplateRef{
				path: path + ".template.infrastructure.templateRef",
				ref:  cc.Spec.Workers.MachineDeployments[i].Infrastructure.TemplateRef,
			},
		)
	}

	return refs
}

// TestClusterClassRefsResolveToCommittedObjects verifies that every template
// reference of clusterclass.yaml names a template object committed in the same
// file, so applying the file leaves no dangling reference for CAPI to trip
// over at reconcile time.
func TestClusterClassRefsResolveToCommittedObjects(t *testing.T) {
	assertRefsResolveToObjects(t, clusterClassFile, parseClusterClass(t))
}

// TestClusterTemplateRefsResolveToCommittedObjects is the cluster-template.yaml
// twin of TestClusterClassRefsResolveToCommittedObjects.
func TestClusterTemplateRefsResolveToCommittedObjects(t *testing.T) {
	assertRefsResolveToObjects(t, clusterTemplateFile, parseClusterTemplateClusterClass(t))
}

// assertRefsResolveToObjects indexes every document of path by kind/name and
// fails for any ClusterClass template ref whose target object is missing from
// the same stream. HypervisorMachineTemplate refs are exempt: the
// machine-shape templates carry lab-specific cpu/ram/disk sizing, so they are
// intentionally supplied by the consuming repository rather than shipped here.
func assertRefsResolveToObjects(t *testing.T, path string, cc clusterv1.ClusterClass) {
	t.Helper()

	objects := make(map[string]bool)

	for _, doc := range splitYAMLDocuments(t, path) {
		var meta struct {
			Kind     string `json:"kind"`
			Metadata struct {
				Name string `json:"name"`
			} `json:"metadata"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("%s contains an invalid YAML document: %v", path, err)
		}

		if meta.Kind == "" {
			continue
		}

		objects[meta.Kind+"/"+meta.Metadata.Name] = true
	}

	for _, nr := range classRefs(cc) {
		if !nr.ref.IsDefined() {
			t.Errorf("%s: %s must be set", path, nr.path)
			continue
		}

		if nr.ref.Kind == "HypervisorMachineTemplate" {
			continue
		}

		if !objects[nr.ref.Kind+"/"+nr.ref.Name] {
			t.Errorf(
				"%s: %s points at %s/%q which is not committed in this file",
				path,
				nr.path,
				nr.ref.Kind,
				nr.ref.Name,
			)
		}
	}
}

// TestControlPlaneTemplatesLeaveScalingToTopology pins the topology-cloning
// contract on both files: the HypervisorControlPlaneTemplate resource must not
// carry replicas or version, because CAPI copies spec.template.spec verbatim
// into the instantiated HypervisorControlPlane and both fields are owned by
// the Cluster topology.
func TestControlPlaneTemplatesLeaveScalingToTopology(t *testing.T) {
	for _, path := range []string{clusterClassFile, clusterTemplateFile} {
		var raw map[string]any
		if !findDocument(t, path, "HypervisorControlPlaneTemplate", "", &raw) {
			t.Fatalf("%s must contain a HypervisorControlPlaneTemplate document", path)
		}

		template := nestedMap(t, raw, "spec", "template")

		spec, ok := template["spec"].(map[string]any)
		if !ok {
			t.Fatalf("%s: spec.template.spec of the HypervisorControlPlaneTemplate must be a mapping", path)
		}

		for _, key := range []string{"replicas", "version"} {
			if _, ok := spec[key]; ok {
				t.Errorf(
					"%s: spec.template.spec.%s must stay unset on the HypervisorControlPlaneTemplate: the Cluster topology controls it",
					path,
					key,
				)
			}
		}
	}
}

func TestClusterTemplateClusterTopology(t *testing.T) {
	cluster := parseClusterTemplateCluster(t)

	topo := nestedMap(t, cluster, "spec", "topology")

	classRef, ok := topo["classRef"].(map[string]any)
	if !ok {
		t.Fatalf("spec.topology.classRef must be a mapping, got %T", topo["classRef"])
	}

	class, ok := classRef["name"].(string)
	if !ok {
		t.Fatalf("spec.topology.classRef.name must be a string, got %T", classRef["name"])
	}

	if class != "hypervisor-cluster-template" {
		t.Errorf("spec.topology.classRef.name must be %q, got %q", "hypervisor-cluster-template", class)
	}

	// Cross-file resolution: the template Cluster, the example Cluster, and
	// the committed ClusterClass must all agree on the class name so the
	// default-flavor template and the example stay twins.
	cc := parseClusterTemplateClusterClass(t)
	if class != cc.Name {
		t.Errorf("spec.topology.classRef.name %q does not match the ClusterClass name %q", class, cc.Name)
	}

	example := parseExampleCluster(t)
	// Topology is a value type in v1beta2; an absent spec.topology yields an
	// empty classRef.name, which the comparison below rejects.
	if class != example.Spec.Topology.ClassRef.Name {
		t.Errorf(
			"spec.topology.classRef.name %q does not match the example Cluster class %q",
			class,
			example.Spec.Topology.ClassRef.Name,
		)
	}

	nestedMap(t, topo, "controlPlane")
	// The first worker MachineDeployment must exist; its replicas marker is
	// pinned by TestClusterTemplateVariableMarkers.
	workers := nestedMap(t, topo, "workers")

	mds, ok := workers["machineDeployments"].([]any)
	if !ok {
		t.Fatal("spec.topology.workers.machineDeployments must be a list")
	}

	if len(mds) == 0 {
		t.Fatal("spec.topology.workers.machineDeployments must list at least one MachineDeployment")
	}
}

func TestClusterTemplateVariableMarkers(t *testing.T) {
	cluster := parseClusterTemplateCluster(t)

	tests := []struct {
		name string
		path string
		want string
	}{
		{name: "cluster name", path: "metadata.name", want: "${CLUSTER_NAME}"},
		{name: "namespace", path: "metadata.namespace", want: "${NAMESPACE}"},
		{name: "kubernetes version", path: "spec.topology.version", want: "${KUBERNETES_VERSION}"},
		{name: "control plane replicas", path: "spec.topology.controlPlane.replicas", want: "${CONTROL_PLANE_MACHINE_COUNT}"},
		{
			name: "worker replicas",
			path: "spec.topology.workers.machineDeployments[0].replicas",
			want: "${WORKER_MACHINE_COUNT}",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			raw := mapPathValue(t, cluster, tt.path)

			got, ok := raw.(string)
			if !ok {
				t.Fatalf("%s must hold the literal marker %s as a string, got %T", tt.path, tt.want, raw)
			}

			if got != tt.want {
				t.Errorf("%s must equal the literal marker %s, got %q", tt.path, tt.want, got)
			}
		})
	}
}

func TestClusterTemplateDefaultsDocumentedByExample(t *testing.T) {
	// The template substitutes ${VARIABLE} markers; the example Cluster is the
	// fallback documentation for what those markers default to.
	example := parseExampleCluster(t)
	if example.Name != "k8labs" {
		t.Errorf("example metadata.name must document the default %q, got %q", "k8labs", example.Name)
	}

	if example.Namespace != "default" {
		t.Errorf("example metadata.namespace must document the default %q, got %q", "default", example.Namespace)
	}

	if example.Spec.Topology.ClassRef.Name == "" {
		t.Fatal("the example Cluster must carry spec.topology to document the template defaults")
	}

	topo := example.Spec.Topology
	if topo.Version != "v1.32.13" {
		t.Errorf("example spec.topology.version must document the default %q, got %q", "v1.32.13", topo.Version)
	}

	if topo.ControlPlane.Replicas == nil || *topo.ControlPlane.Replicas != 1 {
		t.Errorf("example spec.topology.controlPlane.replicas must document the default 1")
	}

	if len(topo.Workers.MachineDeployments) == 0 {
		t.Fatal("the example Cluster must list a worker MachineDeployment to document the worker default")
	}

	md := topo.Workers.MachineDeployments[0]
	if md.Replicas == nil || *md.Replicas != 3 {
		t.Errorf("example spec.topology.workers.machineDeployments[0].replicas must document the default 3")
	}
}

// findDocument finds the first document of path with the given kind,
// unmarshalling it into out. When apiVersion is non-empty the document must
// also carry it. It reports false when no such document exists.
func findDocument(t *testing.T, path, kind, apiVersion string, out any) bool {
	t.Helper()

	for _, doc := range splitYAMLDocuments(t, path) {
		var meta struct {
			APIVersion string `json:"apiVersion"`
			Kind       string `json:"kind"`
		}
		if err := yaml.Unmarshal(doc, &meta); err != nil {
			t.Fatalf("%s contains an invalid YAML document: %v", path, err)
		}

		if meta.Kind != kind || (apiVersion != "" && meta.APIVersion != apiVersion) {
			continue
		}

		if err := yaml.Unmarshal(doc, out); err != nil {
			t.Fatalf("%s document %s must parse: %v", path, kind, err)
		}

		return true
	}

	return false
}

func parseClusterTemplateClusterClass(t *testing.T) clusterv1.ClusterClass {
	t.Helper()

	var cc clusterv1.ClusterClass
	if !findDocument(t, clusterTemplateFile, "ClusterClass", clusterv1.GroupVersion.String(), &cc) {
		t.Fatalf(
			"%s must contain a ClusterClass document with apiVersion %s",
			clusterTemplateFile,
			clusterv1.GroupVersion.String(),
		)
	}

	return cc
}

// parseClusterTemplateCluster returns the template's Cluster document as a
// generic map: the ${VARIABLE} markers stand in for replica counts, so the
// document cannot unmarshal into clusterv1.Cluster while the markers exist.
func parseClusterTemplateCluster(t *testing.T) map[string]any {
	t.Helper()

	var m map[string]any
	if !findDocument(t, clusterTemplateFile, "Cluster", clusterv1.GroupVersion.String(), &m) {
		t.Fatalf(
			"%s must contain a Cluster document with apiVersion %s",
			clusterTemplateFile,
			clusterv1.GroupVersion.String(),
		)
	}

	return m
}

func rawClusterTemplateClusterClass(t *testing.T) map[string]any {
	t.Helper()

	var m map[string]any
	if !findDocument(t, clusterTemplateFile, "ClusterClass", clusterv1.GroupVersion.String(), &m) {
		t.Fatalf(
			"%s must contain a ClusterClass document with apiVersion %s",
			clusterTemplateFile,
			clusterv1.GroupVersion.String(),
		)
	}

	return m
}

// mapPathValue resolves a dotted path such as
// "spec.topology.workers.machineDeployments[0].replicas" through a generic
// YAML document and returns the value at that exact location.
func mapPathValue(t *testing.T, root map[string]any, path string) any {
	t.Helper()

	var cur any = root

	for seg := range strings.SplitSeq(path, ".") {
		key := seg
		index := -1

		if i := strings.IndexByte(seg, '['); i >= 0 {
			key = seg[:i]
			if !strings.HasSuffix(seg, "]") {
				t.Fatalf("path %q: segment %q must end in ]", path, seg)
			}

			n, err := strconv.Atoi(seg[i+1 : len(seg)-1])
			if err != nil {
				t.Fatalf("path %q: segment %q has a non-numeric list index", path, seg)
			}

			index = n
		}

		m, ok := cur.(map[string]any)
		if !ok {
			t.Fatalf("path %q: %v is not a mapping before segment %q", path, cur, seg)
		}

		if index >= 0 {
			list, ok := m[key].([]any)
			if !ok {
				t.Fatalf("path %q: key %q is not a list", path, key)
			}

			if index >= len(list) {
				t.Fatalf("path %q: index %d out of range for key %q (len %d)", path, index, key, len(list))
			}

			cur = list[index]

			continue
		}

		v, ok := m[key]
		if !ok {
			t.Fatalf("path %q: missing key %q", path, key)
		}

		cur = v
	}

	return cur
}
