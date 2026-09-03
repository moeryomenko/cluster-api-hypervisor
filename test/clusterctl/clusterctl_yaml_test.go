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

package clusterctl

import (
	"net/url"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	yamlv3 "gopkg.in/yaml.v3"
)

// clusterctlConfig is the minimal typed shape of the clusterctl
// configuration file (cmd/clusterctl/client/config providers_client.go
// configProvider).
type clusterctlConfig struct {
	Providers []clusterctlProvider `yaml:"providers"`
}

// clusterctlProvider pins one entry of the providers list.
type clusterctlProvider struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
	Type string `yaml:"type"`
}

// providerTypeSpec pins, for one clusterctl provider type, the manifest label
// prefix and the components file name required by the local repository
// layout. The label prefix mirrors clusterctlv1.ManifestLabel
// (cmd/clusterctl/api/v1alpha3 labels.go).
type providerTypeSpec struct {
	typeName       string
	labelPrefix    string
	componentsFile string
}

const (
	providerName    = "hypervisor"
	providerVersion = "v0.1.0"
)

var providerTypes = []providerTypeSpec{
	{typeName: "InfrastructureProvider", labelPrefix: "infrastructure", componentsFile: "infrastructure-components.yaml"},
	{typeName: "BootstrapProvider", labelPrefix: "bootstrap", componentsFile: "bootstrap-components.yaml"},
	{typeName: "ControlPlaneProvider", labelPrefix: "control-plane", componentsFile: "control-plane-components.yaml"},
}

// mustClusterctlMap parses clusterctl.yaml into a generic mapping so tests
// can assert key presence without relying on zero values.
func mustClusterctlMap(t *testing.T) map[string]any {
	t.Helper()

	var doc map[string]any
	if err := yamlv3.Unmarshal(readRepoFile(t, "clusterctl.yaml"), &doc); err != nil {
		t.Fatalf("clusterctl.yaml is not valid YAML: %v", err)
	}

	return doc
}

// mustClusterctlConfig decodes clusterctl.yaml into the typed shape.
func mustClusterctlConfig(t *testing.T) clusterctlConfig {
	t.Helper()

	var cfg clusterctlConfig
	if err := yamlv3.Unmarshal(readRepoFile(t, "clusterctl.yaml"), &cfg); err != nil {
		t.Fatalf("clusterctl.yaml does not decode as clusterctlConfig: %v", err)
	}

	return cfg
}

// providerOfType returns the provider entry with the given type.
func providerOfType(t *testing.T, cfg clusterctlConfig, typeName string) clusterctlProvider {
	t.Helper()

	for _, p := range cfg.Providers {
		if p.Type == typeName {
			return p
		}
	}

	t.Fatalf("no provider of type %q", typeName)

	return clusterctlProvider{}
}

// typeKeys returns the sorted keys of a count map for stable failure output.
func typeKeys(m map[string]int) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}

	sort.Strings(keys)

	return keys
}

func TestClusterctlFileExists(t *testing.T) {
	if len(readRepoFile(t, "clusterctl.yaml")) == 0 {
		t.Error("clusterctl.yaml is empty")
	}
}

func TestClusterctlDocument(t *testing.T) {
	doc := mustClusterctlMap(t)
	// overridesFolder must be a top-level key: clusterctl reads it with the
	// flat viper key "overridesFolder" (repository/overrides.go), which only
	// resolves top-level keys, never a nested variables map.
	for _, key := range []string{"providers", "overridesFolder"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("clusterctl.yaml missing key %q", key)
		}
	}
}

func TestClusterctlProviders(t *testing.T) {
	doc := mustClusterctlMap(t)

	raw, ok := doc["providers"].([]any)
	if !ok {
		t.Fatalf("providers must be a list, got %T", doc["providers"])
	}

	if len(raw) != 3 {
		t.Fatalf("providers has %d entries, want exactly 3", len(raw))
	}

	for i, entry := range raw {
		prov, ok := entry.(map[string]any)
		if !ok {
			t.Errorf("provider %d must be a mapping, got %T", i, entry)
			continue
		}

		for _, key := range []string{"name", "url", "type"} {
			if _, ok := prov[key]; !ok {
				t.Errorf("provider %d missing key %q", i, key)
			}
		}
	}

	cfg := mustClusterctlConfig(t)
	for i, p := range cfg.Providers {
		if p.Name != providerName {
			t.Errorf("provider %d name = %q, want %q", i, p.Name, providerName)
		}
	}
}

func TestClusterctlProviderTypes(t *testing.T) {
	cfg := mustClusterctlConfig(t)

	got := map[string]int{}
	for _, p := range cfg.Providers {
		got[p.Type]++
	}

	want := map[string]int{}
	for _, tt := range providerTypes {
		want[tt.typeName] = 1
	}

	if len(got) != len(want) {
		t.Errorf("provider types = %v, want %v", typeKeys(got), typeKeys(want))
	}

	for _, tt := range providerTypes {
		if got[tt.typeName] != 1 {
			t.Errorf("provider type %q count = %d, want 1", tt.typeName, got[tt.typeName])
		}
	}
}

func TestClusterctlProviderURLs(t *testing.T) {
	cfg := mustClusterctlConfig(t)
	for _, tt := range providerTypes {
		t.Run(tt.typeName, func(t *testing.T) {
			p := providerOfType(t, cfg, tt.typeName)

			u, err := url.Parse(p.URL)
			if err != nil {
				t.Fatalf("parse url %q: %v", p.URL, err)
			}

			if u.Scheme != "file" {
				t.Errorf("url %q scheme = %q, want %q", p.URL, u.Scheme, "file")
			}

			if !filepath.IsAbs(u.Path) {
				t.Errorf("url %q path %q must be absolute", p.URL, u.Path)
			}

			// The local repository layout splits the path as
			// {basepath}/{provider-label}/{version}/{components.yaml}
			// (cmd/clusterctl/client/repository/repository_local.go).
			segs := strings.Split(u.Path, "/")
			if len(segs) < 3 {
				t.Fatalf("url %q path must be {basepath}/{provider-label}/{version}/{components.yaml}", p.URL)
			}

			wantLabel := tt.labelPrefix + "-" + providerName
			if got := segs[len(segs)-3]; got != wantLabel {
				t.Errorf("url %q provider-label segment = %q, want %q", p.URL, got, wantLabel)
			}

			if got := segs[len(segs)-2]; got != providerVersion {
				t.Errorf("url %q version segment = %q, want %q", p.URL, got, providerVersion)
			}

			if got := segs[len(segs)-1]; got != tt.componentsFile {
				t.Errorf("url %q components segment = %q, want %q", p.URL, got, tt.componentsFile)
			}
		})
	}
}

func TestClusterctlOverridesFolder(t *testing.T) {
	doc := mustClusterctlMap(t)

	value, ok := doc["overridesFolder"]
	if !ok {
		t.Fatal("clusterctl.yaml missing top-level key \"overridesFolder\"")
	}

	folder, ok := value.(string)
	if !ok {
		t.Fatalf("overridesFolder must be a string, got %T", value)
	}
	// overrides.go only honors a non-empty value, so an empty declaration
	// would silently fall back to the default overrides directory.
	if strings.TrimSpace(folder) == "" {
		t.Error("overridesFolder must not be empty")
	}
}
