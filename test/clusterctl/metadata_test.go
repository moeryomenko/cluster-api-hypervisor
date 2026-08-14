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

// Package clusterctl holds the structural contract tests for the two
// repository-root files that turn this repository into a clusterctl local
// provider repository: metadata.yaml (provider metadata) and clusterctl.yaml
// (the clusterctl configuration template).
//
// The contract is test-first: both files are committed at the repository
// root. Until they exist every test fails at the file read, which is the
// intended red phase.
//
// The pinned shapes follow the clusterctl v1alpha3 API and the local
// repository layout as shipped by the Cluster API module pinned in go.mod
// (sigs.k8s.io/cluster-api v1.13.5):
//
//   - metadata.yaml is a provider repository metadata document with
//     apiVersion clusterctl.cluster.x-k8s.io/v1alpha3, kind Metadata, and a
//     releaseSeries list with exactly one entry {major: 0, minor: 1,
//     contract: v1beta1}.
//   - clusterctl.yaml lists exactly three providers named hypervisor (one
//     InfrastructureProvider, one BootstrapProvider, one
//     ControlPlaneProvider) whose file:// URLs follow the local repository
//     layout {basepath}/{provider-label}/{version}/{components.yaml}, and a
//     variables map declaring overridesFolder.
package clusterctl

import (
	"os"
	"path/filepath"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// metadataDocument is the minimal typed shape of a provider repository
// metadata document (cmd/clusterctl/api/v1alpha3 metadata_type.go).
type metadataDocument struct {
	APIVersion    string                  `yaml:"apiVersion"`
	Kind          string                  `yaml:"kind"`
	ReleaseSeries []metadataReleaseSeries `yaml:"releaseSeries"`
}

// metadataReleaseSeries pins one release series of the provider repository.
type metadataReleaseSeries struct {
	Major    int32  `yaml:"major"`
	Minor    int32  `yaml:"minor"`
	Contract string `yaml:"contract"`
}

const (
	metadataAPIVersion = "clusterctl.cluster.x-k8s.io/v1alpha3"
	metadataKind       = "Metadata"
	metadataMajor      = 0
	metadataMinor      = 1
	metadataContract   = "v1beta1"
)

// readRepoFile reads a committed file from the repository root. The go test
// working directory is the package directory, so the repository root is two
// levels up.
func readRepoFile(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return data
}

// mustMetadataMap parses metadata.yaml into a generic mapping so tests can
// assert key presence without relying on zero values.
func mustMetadataMap(t *testing.T) map[string]any {
	t.Helper()
	var doc map[string]any
	if err := yamlv3.Unmarshal(readRepoFile(t, "metadata.yaml"), &doc); err != nil {
		t.Fatalf("metadata.yaml is not valid YAML: %v", err)
	}
	return doc
}

func TestMetadataFileExists(t *testing.T) {
	if len(readRepoFile(t, "metadata.yaml")) == 0 {
		t.Error("metadata.yaml is empty")
	}
}

func TestMetadataDocument(t *testing.T) {
	doc := mustMetadataMap(t)
	for _, key := range []string{"apiVersion", "kind", "releaseSeries"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("metadata.yaml missing key %q", key)
		}
	}

	var md metadataDocument
	if err := yamlv3.Unmarshal(readRepoFile(t, "metadata.yaml"), &md); err != nil {
		t.Fatalf("metadata.yaml does not decode as metadataDocument: %v", err)
	}
	if md.APIVersion != metadataAPIVersion {
		t.Errorf("apiVersion = %q, want %q", md.APIVersion, metadataAPIVersion)
	}
	if md.Kind != metadataKind {
		t.Errorf("kind = %q, want %q", md.Kind, metadataKind)
	}
}

func TestMetadataReleaseSeries(t *testing.T) {
	doc := mustMetadataMap(t)
	raw, ok := doc["releaseSeries"].([]any)
	if !ok {
		t.Fatalf("releaseSeries must be a list, got %T", doc["releaseSeries"])
	}
	if len(raw) != 1 {
		t.Fatalf("releaseSeries has %d entries, want exactly 1", len(raw))
	}
	entry, ok := raw[0].(map[string]any)
	if !ok {
		t.Fatalf("releaseSeries entry must be a mapping, got %T", raw[0])
	}
	// Key presence is asserted on the raw entry so a missing major key cannot
	// slip through as the zero value 0.
	for _, key := range []string{"major", "minor", "contract"} {
		if _, ok := entry[key]; !ok {
			t.Errorf("releaseSeries entry missing key %q", key)
		}
	}

	var md metadataDocument
	if err := yamlv3.Unmarshal(readRepoFile(t, "metadata.yaml"), &md); err != nil {
		t.Fatalf("metadata.yaml does not decode as metadataDocument: %v", err)
	}
	if len(md.ReleaseSeries) != 1 {
		t.Fatalf("releaseSeries has %d entries, want exactly 1", len(md.ReleaseSeries))
	}
	rs := md.ReleaseSeries[0]
	if rs.Major != metadataMajor || rs.Minor != metadataMinor || rs.Contract != metadataContract {
		t.Errorf("releaseSeries = {%d, %d, %q}, want {%d, %d, %q}",
			rs.Major, rs.Minor, rs.Contract, metadataMajor, metadataMinor, metadataContract)
	}
}
