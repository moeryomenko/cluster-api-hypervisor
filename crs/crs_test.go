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

// Package crs holds the structural contract tests for the ClusterResourceSet
// (CRS) manifests and the populate script shipped in this directory.
//
// The contract is test-first: the implementation task writes the CRS
// manifests, the ordering note, and crs/populate.sh. Until they exist every
// test fails at the file read, which is the intended red phase.
//
// The pinned shapes follow the Cluster API v1beta1 API as shipped by the
// Cluster API module in the go.mod (sigs.k8s.io/cluster-api v1.13.x):
//
//   - group addons.cluster.x-k8s.io, version v1beta1, kind ClusterResourceSet
//   - spec.clusterSelector is a label selector over Clusters in the same
//     namespace. The controller treats an empty selector as matching nothing,
//     so the manifest must pin the workload cluster through the
//     cluster.x-k8s.io/cluster-name label.
//   - spec.resources lists ConfigMap references by name; the controller
//     fetches each ConfigMap from the CRS namespace and applies every string
//     value of its data map as a YAML document.
//   - spec.strategy is optional and defaults to ApplyOnce. The delivery
//     contract here is one-shot application after the workload cluster is
//     provisioned, so Reconcile is not acceptable.
//
// The cluster-ops contract delivered through the CRS is the rbac, cilium,
// coredns, and metrics-server manifest sets, with the apply ordering
// documented: rbac must precede cilium.
//
// The populate script contract is behavioral: given a directory of manifest
// YAML files (first positional argument, or the CRS_MANIFEST_DIR environment
// variable), the script writes a valid ConfigMap YAML stream to stdout whose
// data embeds the manifest content. The fixture under testdata/ exercises
// that contract.
package crs

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
	addonsv1 "sigs.k8s.io/cluster-api/api/addons/v1beta1"
	sigsyaml "sigs.k8s.io/yaml"
)

const (
	// clusterNameLabel is the label the CRS selector must match against: the
	// Cluster object carries it with the workload cluster name.
	clusterNameLabel = "cluster.x-k8s.io/cluster-name"

	// fixtureManifestDir is the populate script fixture shipped under
	// testdata/: a single small manifest that the script must turn into a
	// ConfigMap.
	fixtureManifestDir = "testdata/manifests"

	fixtureMarkerName = "fixture-sa"
	fixtureMarkerKind = "ServiceAccount"
)

// dns1123Subdomain matches Kubernetes object names: lowercase alphanumeric
// plus '-' and '.' between.
var dns1123Subdomain = regexp.MustCompile(`^[a-z0-9]([-a-z0-9.]*[a-z0-9])?$`)

func TestCRSExistsAndParses(t *testing.T) {
	crss := clusterResourceSets(t)
	for _, pc := range crss {
		if pc.crs.APIVersion != addonsv1.GroupVersion.String() {
			t.Errorf("%s: apiVersion must be %s, got %q", pc.file, addonsv1.GroupVersion.String(), pc.crs.APIVersion)
		}
		if pc.crs.Kind != "ClusterResourceSet" {
			t.Errorf("%s: kind must be ClusterResourceSet, got %q", pc.file, pc.crs.Kind)
		}
		if pc.crs.Name == "" {
			t.Errorf("%s: metadata.name must be set", pc.file)
		}
		// The strategy defaults to ApplyOnce when omitted; the delivery
		// contract is one-shot application, so Reconcile is not acceptable.
		switch pc.crs.Spec.Strategy {
		case "", string(addonsv1.ClusterResourceSetStrategyApplyOnce):
		default:
			t.Errorf(
				"%s: spec.strategy must be ApplyOnce (or omitted, which the controller defaults to ApplyOnce), got %q",
				pc.file,
				pc.crs.Spec.Strategy,
			)
		}
	}
}

func TestCRSSelector(t *testing.T) {
	for _, pc := range clusterResourceSets(t) {
		labels := pc.crs.Spec.ClusterSelector.MatchLabels
		if labels == nil {
			t.Errorf(
				"%s: spec.clusterSelector.matchLabels must select the workload cluster via the %q label; an empty selector matches no Cluster",
				pc.file,
				clusterNameLabel,
			)
			continue
		}
		value, ok := labels[clusterNameLabel]
		if !ok {
			t.Errorf(
				"%s: spec.clusterSelector.matchLabels must include %q so the set applies only to the workload cluster carrying that label",
				pc.file,
				clusterNameLabel,
			)
			continue
		}
		if value == "" {
			t.Errorf("%s: spec.clusterSelector.matchLabels[%q] must have a non-empty value", pc.file, clusterNameLabel)
		}
	}
}

func TestCRSResources(t *testing.T) {
	for _, pc := range clusterResourceSets(t) {
		if len(pc.crs.Spec.Resources) == 0 {
			t.Errorf(
				"%s: spec.resources must reference at least one ConfigMap carrying the rbac, cilium, coredns, and metrics-server manifests",
				pc.file,
			)
			continue
		}
		for i, res := range pc.crs.Spec.Resources {
			path := fmt.Sprintf("%s: spec.resources[%d]", pc.file, i)
			if res.Kind != string(addonsv1.ConfigMapClusterResourceSetResourceKind) {
				t.Errorf(
					"%s: kind must be ConfigMap (the cluster-ops manifests are delivered as ConfigMaps, not Secrets), got %q",
					path,
					res.Kind,
				)
			}
			if res.Name == "" {
				t.Errorf("%s: name must reference a ConfigMap emitted by crs/populate.sh", path)
				continue
			}
			if !dns1123Subdomain.MatchString(res.Name) {
				t.Errorf("%s: name %q must be a valid Kubernetes object name (DNS-1123 subdomain)", path, res.Name)
			}
		}
	}
}

func TestCRSOrderingNote(t *testing.T) {
	notes, err := filepath.Glob("*.md")
	if err != nil {
		t.Fatalf("glob *.md: %v", err)
	}
	if len(notes) == 0 {
		t.Fatal("crs/ must ship a markdown note (e.g. README.md) documenting the apply ordering")
	}
	var text strings.Builder
	for _, note := range notes {
		raw, err := os.ReadFile(note)
		if err != nil {
			t.Errorf("reading %s: %v", note, err)
			continue
		}
		text.Write(raw)
	}
	lower := strings.ToLower(text.String())
	for _, component := range []string{"rbac", "cilium"} {
		if !strings.Contains(lower, component) {
			t.Errorf(
				"the ordering note must mention %q: the rbac manifests must be applied before the cilium manifests",
				component,
			)
		}
	}
	if !strings.Contains(lower, "before") && !strings.Contains(lower, "then") && !strings.Contains(lower, "order") {
		t.Error("the ordering note must state that rbac precedes cilium (e.g. \"rbac before cilium\")")
	}
}

func TestPopulateScriptContract(t *testing.T) {
	const script = "populate.sh"
	info, err := os.Stat(script)
	if err != nil {
		t.Fatalf(
			"crs/%s must exist: it turns the k8labs manifest directories into ConfigMaps referenced by the ClusterResourceSet",
			script,
		)
	}
	if info.Mode().Perm()&0o111 == 0 {
		t.Fatalf("crs/%s must be executable", script)
	}

	out := runPopulate(t, script, fixtureManifestDir)
	configMaps := configMapsFromStream(t, out)
	if len(configMaps) == 0 {
		t.Fatal("populate.sh must emit at least one ConfigMap document")
	}

	var data strings.Builder
	for _, cm := range configMaps {
		if cm.APIVersion != "v1" {
			t.Errorf("emitted ConfigMap must have apiVersion v1, got %q", cm.APIVersion)
		}
		if cm.Metadata.Name == "" {
			t.Error("emitted ConfigMap must have metadata.name")
		}
		for _, value := range cm.Data {
			data.WriteString(value)
		}
	}

	// The ConfigMap data must embed the fixture manifest: the ClusterResourceSet
	// controller applies every string value of the data map as a YAML document.
	// The fixture is a ServiceAccount named fixture-sa.
	if !strings.Contains(data.String(), fixtureMarkerName) || !strings.Contains(data.String(), fixtureMarkerKind) {
		t.Errorf(
			"emitted ConfigMap data must embed the fixture manifest content (kind %s named %s)",
			fixtureMarkerKind,
			fixtureMarkerName,
		)
	}
}

// parsedCRS pairs a parsed ClusterResourceSet with the manifest file it came
// from so failure messages stay actionable.
type parsedCRS struct {
	file string
	crs  addonsv1.ClusterResourceSet
}

// clusterResourceSets parses every ClusterResourceSet document found in the
// committed YAML files of this directory. Failing to find any manifest is the
// red phase of this suite.
func clusterResourceSets(t *testing.T) []parsedCRS {
	t.Helper()
	var found []parsedCRS
	for _, file := range yamlFiles(t) {
		raw, err := os.ReadFile(file)
		if err != nil {
			t.Errorf("reading %s: %v", file, err)
			continue
		}
		for _, doc := range splitYAML(t, raw) {
			var crs addonsv1.ClusterResourceSet
			if err := sigsyaml.Unmarshal(doc, &crs); err != nil {
				t.Errorf("%s: parsing ClusterResourceSet document: %v", file, err)
				continue
			}
			if crs.Kind == "ClusterResourceSet" {
				found = append(found, parsedCRS{file: file, crs: crs})
			}
		}
	}
	if len(found) == 0 {
		t.Fatal(
			"crs/ must ship at least one ClusterResourceSet manifest (kind ClusterResourceSet, group addons.cluster.x-k8s.io, version v1beta1)",
		)
	}
	return found
}

// yamlFiles lists the committed YAML manifests in this directory. testdata is
// excluded: the go tool ignores it, and the populate script fixture must not
// be parsed as a ClusterResourceSet.
func yamlFiles(t *testing.T) []string {
	t.Helper()
	var files []string
	for _, pattern := range []string{"*.yaml", "*.yml"} {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			t.Fatalf("glob %q: %v", pattern, err)
		}
		files = append(files, matches...)
	}
	return files
}

// splitYAML decodes every top-level document of a (possibly multi-document)
// YAML stream and returns each document re-encoded as its own byte slice.
func splitYAML(t *testing.T, raw []byte) [][]byte {
	t.Helper()
	dec := yamlv3.NewDecoder(bytes.NewReader(raw))
	var docs [][]byte
	for {
		var doc any
		err := dec.Decode(&doc)
		if errors.Is(err, io.EOF) {
			return docs
		}
		if err != nil {
			t.Fatalf("invalid YAML document: %v", err)
		}
		if doc == nil {
			continue
		}
		rawDoc, err := yamlv3.Marshal(doc)
		if err != nil {
			t.Fatalf("re-encoding YAML document: %v", err)
		}
		docs = append(docs, rawDoc)
	}
}

// runPopulate executes crs/populate.sh against the fixture manifest
// directory. The pinned interface is the source directory as the first
// positional argument; the CRS_MANIFEST_DIR environment variable is accepted
// as an alternative interface for scripts that prefer configuration.
func runPopulate(t *testing.T, script, sourceDir string) []byte {
	t.Helper()
	// The leading "./" forces a lookup relative to the package directory; a
	// bare name would go through $PATH instead.
	scriptPath := "./" + script
	argOut, argErr := exec.Command(scriptPath, sourceDir).CombinedOutput()
	if argErr == nil {
		return argOut
	}
	cmd := exec.Command(scriptPath)
	cmd.Env = append(os.Environ(), "CRS_MANIFEST_DIR="+sourceDir)
	envOut, envErr := cmd.CombinedOutput()
	if envErr != nil {
		t.Fatalf(
			"populate.sh must accept the source manifest directory as its first argument (failed: %v) or via CRS_MANIFEST_DIR (failed: %v); output: %s",
			argErr,
			envErr,
			envOut,
		)
	}
	return envOut
}

// emittedConfigMap is the minimal shape a ConfigMap emitted by populate.sh
// must carry: core v1 apiVersion, a name, and string data.
type emittedConfigMap struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Data map[string]string `yaml:"data"`
}

// configMapsFromStream collects every ConfigMap document from a YAML stream.
func configMapsFromStream(t *testing.T, raw []byte) []emittedConfigMap {
	t.Helper()
	var configMaps []emittedConfigMap
	for _, doc := range splitYAML(t, raw) {
		var cm emittedConfigMap
		if err := sigsyaml.Unmarshal(doc, &cm); err != nil {
			t.Errorf("parsing populate.sh output document: %v", err)
			continue
		}
		if cm.Kind != "ConfigMap" {
			t.Errorf("populate.sh must emit ConfigMap documents, got kind %q", cm.Kind)
			continue
		}
		configMaps = append(configMaps, cm)
	}
	return configMaps
}
