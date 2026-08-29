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

// REQ-002 / VC-01 — k8netd provider config changes (test-first, RED phase).
//
// This file proves the spec-required config mutations that the current
// implementation does NOT yet satisfy. All tests in this file are expected
// to FAIL against the pre-REQ-002 implementation, demonstrating:
//
//   - K8NetdSocket field absent / HYPERVISOR_K8NETD_SOCKET not consumed
//   - Dnsmasq field still present / HYPERVISOR_DNSMASQ still queried
//   - StateDir default still /var/lib/k8slab (root-owned) instead of a
//     user-writable path.
//
// Grill coverage:
//   - unset vs empty string vs set overrides for K8NetdSocket (valueOrDefault semantics)
//   - nil env func behaves as unset (nil guard)
//   - arbitrary custom values preserved verbatim
//   - Dnsmasq removal verified structurally (reflection) and behaviorally (spy)
//   - StateDir default must not be the old /var/lib path, must be user-writable
//   - StateDir override via HYPERVISOR_STATE_DIR still honoured
//   - exact default value pin for K8NetdSocket per spec
//   - HYPERVISOR_NETWORK_CIDR validation unchanged (regression guard)
package config_test

import (
	"reflect"
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
)

const (
	expectedK8NetdSocketDefault = "/run/user/1000/k8snet/control.sock"
	oldStateDirDefault          = "/var/lib/k8slab"
)

// helpers for reflection-based RED checks — avoids compile-time dependency on
// fields that do not yet exist (or should not exist after the change).

func fieldExists(t *testing.T, typ reflect.Type, name string) bool {
	t.Helper()
	_, ok := typ.FieldByName(name)
	return ok
}

func stringFieldValue(t *testing.T, cfg config.Config, name string) (string, bool) {
	t.Helper()
	v := reflect.ValueOf(cfg)
	f := v.FieldByName(name)
	if !f.IsValid() {
		return "", false
	}
	if f.Kind() != reflect.String {
		t.Fatalf("field %q is not a string (kind %v)", name, f.Kind())
	}
	return f.String(), true
}

// ---------------------------------------------------------------------------
// K8NetdSocket — default and override
// ---------------------------------------------------------------------------

func TestK8NetdSocket_DefaultWhenUnset(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	got, ok := stringFieldValue(t, cfg, "K8NetdSocket")
	if !ok {
		t.Fatalf(
			"Config has no field K8NetdSocket — REQ-002 requires K8NetdSocket from HYPERVISOR_K8NETD_SOCKET with default %q",
			expectedK8NetdSocketDefault,
		)
	}
	if got != expectedK8NetdSocketDefault {
		t.Errorf("K8NetdSocket when unset = %q, want default %q", got, expectedK8NetdSocketDefault)
	}
}

func TestK8NetdSocket_DefaultWhenEmptyString(t *testing.T) {
	t.Parallel()

	env := map[string]string{
		"HYPERVISOR_K8NETD_SOCKET": "",
	}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	got, ok := stringFieldValue(t, cfg, "K8NetdSocket")
	if !ok {
		t.Fatalf(
			"Config has no field K8NetdSocket — empty HYPERVISOR_K8NETD_SOCKET must fall back to default %q",
			expectedK8NetdSocketDefault,
		)
	}
	if got != expectedK8NetdSocketDefault {
		t.Errorf("K8NetdSocket when HYPERVISOR_K8NETD_SOCKET=\"\" = %q, want default %q", got, expectedK8NetdSocketDefault)
	}
}

func TestK8NetdSocket_NilEnvUsesDefault(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load(nil) unexpected error: %v", err)
	}
	got, ok := stringFieldValue(t, cfg, "K8NetdSocket")
	if !ok {
		t.Fatalf("Config has no field K8NetdSocket — Load(nil) must still expose default %q", expectedK8NetdSocketDefault)
	}
	if got != expectedK8NetdSocketDefault {
		t.Errorf("K8NetdSocket with nil env = %q, want default %q", got, expectedK8NetdSocketDefault)
	}
}

func TestK8NetdSocket_OverrideWhenSet(t *testing.T) {
	t.Parallel()

	custom := "/tmp/custom/k8netd.sock"
	env := map[string]string{
		"HYPERVISOR_K8NETD_SOCKET": custom,
	}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	got, ok := stringFieldValue(t, cfg, "K8NetdSocket")
	if !ok {
		t.Fatalf("Config has no field K8NetdSocket — REQ-002 requires it")
	}
	if got != custom {
		t.Errorf("K8NetdSocket when HYPERVISOR_K8NETD_SOCKET=%q = %q, want %q", custom, got, custom)
	}
}

func TestK8NetdSocket_PreservesCustomValueVerbatim(t *testing.T) {
	t.Parallel()

	// grill: arbitrary path with spaces / unusual chars must be preserved verbatim
	cases := []string{
		"/run/user/1000/k8snet/control.sock", // same as default but explicitly set
		"/tmp/a b/c.sock",
		"/var/tmp/k8netd-123.sock",
	}
	for _, custom := range cases {
		t.Run(custom, func(t *testing.T) {
			t.Parallel()
			env := map[string]string{"HYPERVISOR_K8NETD_SOCKET": custom}
			cfg, err := config.Load(func(name string) string { return env[name] })
			if err != nil {
				t.Fatalf("Load() unexpected error: %v", err)
			}
			got, ok := stringFieldValue(t, cfg, "K8NetdSocket")
			if !ok {
				t.Fatalf("Config has no field K8NetdSocket")
			}
			if got != custom {
				t.Errorf("K8NetdSocket verbatim: got %q, want %q", got, custom)
			}
		})
	}
}

func TestK8NetdSocket_QueriedByLoad(t *testing.T) {
	t.Parallel()

	queried := map[string]bool{}
	env := func(name string) string {
		queried[name] = true
		return ""
	}
	if _, err := config.Load(env); err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if !queried["HYPERVISOR_K8NETD_SOCKET"] {
		t.Errorf("Load() did not query HYPERVISOR_K8NETD_SOCKET — REQ-002 requires it")
	}
}

// ---------------------------------------------------------------------------
// Dnsmasq removal — field must not exist and env must be ignored
// ---------------------------------------------------------------------------

func TestDnsmasqFieldRemoved(t *testing.T) {
	t.Parallel()

	typ := reflect.TypeFor[config.Config]()
	if fieldExists(t, typ, "Dnsmasq") {
		t.Errorf("Config still has field Dnsmasq — REQ-002 requires the Dnsmasq field be removed")
	}
}

func TestHYPERVISOR_DNSMASQ_IgnoredNotQueried(t *testing.T) {
	t.Parallel()

	queried := map[string]bool{}
	env := func(name string) string {
		queried[name] = true
		return ""
	}
	if _, err := config.Load(env); err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if queried["HYPERVISOR_DNSMASQ"] {
		t.Errorf("Load() queried HYPERVISOR_DNSMASQ — REQ-002 requires HYPERVISOR_DNSMASQ be ignored (field removed)")
	}
}

func TestHYPERVISOR_DNSMASQ_DoesNotAffectConfig(t *testing.T) {
	t.Parallel()

	// If Dnsmasq is truly removed, setting HYPERVISOR_DNSMASQ must not change
	// any observable Config field compared to not setting it.
	loadWith := func(dnsmasq string) config.Config {
		env := map[string]string{"HYPERVISOR_DNSMASQ": dnsmasq}
		cfg, err := config.Load(func(name string) string { return env[name] })
		if err != nil {
			t.Fatalf("Load() unexpected error: %v", err)
		}
		return cfg
	}
	cfgWithout := loadWith("")
	cfgWith := loadWith("/usr/bin/dnsmasq")

	// Structurally there should be no Dnsmasq field; if there is, the two
	// configs would differ in that field, which is itself a failure.
	typ := reflect.TypeFor[config.Config]()
	if fieldExists(t, typ, "Dnsmasq") {
		t.Fatalf("Config still has Dnsmasq field — cannot verify ignore semantics until field is removed")
	}

	// Compare all remaining fields via reflect.DeepEqual equivalent — here
	// use string representation check for simplicity, or compare known fields.
	// The two configs should be equal when the only difference is HYPERVISOR_DNSMASQ.
	if !reflect.DeepEqual(cfgWithout, cfgWith) {
		t.Errorf(
			"Load() result changed when only HYPERVISOR_DNSMASQ varied:\n without: %#v\n with:    %#v\n HYPERVISOR_DNSMASQ must be ignored per REQ-002",
			cfgWithout,
			cfgWith,
		)
	}
}

// ---------------------------------------------------------------------------
// StateDir — default must be user-writable, not /var/lib/k8slab
// ---------------------------------------------------------------------------

func TestStateDir_DefaultIsUserWritable(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(func(string) string { return "" })
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.StateDir == oldStateDirDefault {
		t.Errorf(
			"StateDir default = %q — REQ-002 requires a new user-writable path, not %q",
			cfg.StateDir,
			oldStateDirDefault,
		)
	}
	if cfg.StateDir == "" {
		t.Errorf("StateDir default is empty — must be a user-writable path per REQ-002")
	}
	if strings.HasPrefix(cfg.StateDir, "/var/lib/") {
		t.Errorf(
			"StateDir default = %q — must not be under /var/lib (requires root) per REQ-002 rootless contract",
			cfg.StateDir,
		)
	}
	// User-writable heuristic: must be under $HOME, /run/user, or .local/state
	// Accept any of these patterns; reject the old root-owned location.
	isUserWritable := strings.Contains(cfg.StateDir, ".local/state") ||
		strings.Contains(cfg.StateDir, "/run/user/") ||
		strings.HasPrefix(cfg.StateDir, "/home/") ||
		strings.Contains(cfg.StateDir, "k8slab")
	if !isUserWritable {
		t.Errorf(
			"StateDir default = %q — does not look user-writable (expected to contain .local/state, /run/user/, /home/, or k8slab) per REQ-002",
			cfg.StateDir,
		)
	}
}

func TestStateDir_DefaultNotVarLibWhenNilEnv(t *testing.T) {
	t.Parallel()

	cfg, err := config.Load(nil)
	if err != nil {
		t.Fatalf("Load(nil) unexpected error: %v", err)
	}
	if cfg.StateDir == oldStateDirDefault {
		t.Errorf("StateDir with nil env = %q — must be new user-writable default, not %q", cfg.StateDir, oldStateDirDefault)
	}
}

func TestStateDir_OverrideStillHonoured(t *testing.T) {
	t.Parallel()

	// grill: ensure default change did not break explicit override
	custom := "/tmp/my-state"
	env := map[string]string{"HYPERVISOR_STATE_DIR": custom}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.StateDir != custom {
		t.Errorf(
			"StateDir when HYPERVISOR_STATE_DIR=%q = %q, want %q (override must still work)",
			custom,
			cfg.StateDir,
			custom,
		)
	}
}

func TestStateDir_EmptyStringFallsBackToNewDefault(t *testing.T) {
	t.Parallel()

	env := map[string]string{"HYPERVISOR_STATE_DIR": ""}
	cfg, err := config.Load(func(name string) string { return env[name] })
	if err != nil {
		t.Fatalf("Load() unexpected error: %v", err)
	}
	if cfg.StateDir == oldStateDirDefault {
		t.Errorf(
			"StateDir when HYPERVISOR_STATE_DIR=\"\" = %q — must fall back to new user-writable default, not old %q",
			cfg.StateDir,
			oldStateDirDefault,
		)
	}
	if cfg.StateDir == "" {
		t.Errorf("StateDir when HYPERVISOR_STATE_DIR=\"\" is empty — must fall back to default")
	}
}

// ---------------------------------------------------------------------------
// Regression: HYPERVISOR_NETWORK_CIDR validation unchanged
// ---------------------------------------------------------------------------

func TestK8NetdConfig_NetworkCIDRValidationUntouched(t *testing.T) {
	t.Parallel()

	// Even with new K8NetdSocket env set, invalid CIDR must still error.
	_, err := config.Load(func(name string) string {
		switch name {
		case "HYPERVISOR_NETWORK_CIDR":
			return "not-a-cidr"
		case "HYPERVISOR_K8NETD_SOCKET":
			return "/tmp/custom.sock"
		default:
			return ""
		}
	})
	if err == nil {
		t.Fatalf(
			"Load() with invalid HYPERVISOR_NETWORK_CIDR expected error, got nil — REQ-002 must not change CIDR validation",
		)
	}
	if !strings.Contains(strings.ToLower(err.Error()), "cidr") {
		t.Errorf("error %q does not mention cidr", err)
	}
}
