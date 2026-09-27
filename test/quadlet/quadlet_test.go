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

// Package quadlet holds the contract tests for the committed provider
// quadlet artifact (spec REQ-007, VC-08 install clauses). The tests parse
// deploy/cluster-api-hypervisor.container into INI-style directive maps
// instead of whole-file string diffs, and exercise the Makefile
// install-quadlet target via make -n dry runs.
package quadlet

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	unitRelPath       = "deploy/cluster-api-hypervisor.container"
	unitInstallSuffix = ".config/containers/systemd/cluster-api-hypervisor.container"

	k8netdUnit  = "k8netd.service"
	capishimPod = "capishim-pod.service"

	capishimRootDefault = ".local/share/capishim"
)

// quadletUnit is a parsed .container file: sections keyed by lowercase name,
// directive values keyed by lowercase key. List-type directives (After=,
// Wants=, Mount=, Environment=, Exec=) accumulate across repeated lines,
// matching systemd unit semantics.
type quadletUnit struct {
	sections map[string]map[string][]string
	// headerComments holds comment lines appearing before the first section.
	headerComments []string
}

// parseUnit parses INI-style quadlet content. Directive keys are normalized
// to lowercase because systemd setting names are case-insensitive; this also
// blocks case-variant smuggling of forbidden directives.
func parseUnit(data string) *quadletUnit {
	u := &quadletUnit{sections: make(map[string]map[string][]string)}
	current := ""

	for line := range strings.SplitSeq(data, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			if current == "" && line != "" {
				u.headerComments = append(u.headerComments, line)
			}

			continue
		}

		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			current = strings.ToLower(strings.Trim(line, "[]"))
			if _, ok := u.sections[current]; !ok {
				u.sections[current] = make(map[string][]string)
			}

			continue
		}

		key, value, ok := strings.Cut(line, "=")
		if !ok || current == "" {
			continue
		}

		key = strings.ToLower(strings.TrimSpace(key))
		u.sections[current][key] = append(u.sections[current][key], strings.TrimSpace(value))
	}

	return u
}

// values returns every value recorded for key in section, or nil.
func (u *quadletUnit) values(section, key string) []string {
	return u.sections[strings.ToLower(section)][strings.ToLower(key)]
}

// bindMount is the parsed shape of one Mount=type=bind,... directive.
type bindMount struct {
	source   string
	target   string
	readonly bool
}

// parseBindMount parses a podman bind-mount option list. Accepts both the
// bare "ro" flag and ro=true / readonly=true spellings.
func parseBindMount(value string) bindMount {
	m := bindMount{}

	for part := range strings.SplitSeq(value, ",") {
		part = strings.TrimSpace(part)
		switch {
		case strings.EqualFold(part, "ro"):
			m.readonly = true
		case strings.EqualFold(part, "rw"):
		default:
			key, val, ok := strings.Cut(part, "=")
			if !ok {
				continue
			}

			switch strings.ToLower(key) {
			case "source", "src":
				m.source = val
			case "target", "dst", "destination":
				m.target = val
			case "ro", "readonly":
				m.readonly = val == "true" || val == "1"
			}
		}
	}

	return m
}

// readRepoFile reads a committed file from the repository root. The go test
// working directory is the package directory, so the repository root is two
// levels up (same convention as test/clusterctl).
func readRepoFile(t *testing.T, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}

	return string(data)
}

// mustLoadUnit reads and parses the committed provider quadlet. All unit
// content tests funnel through here so a missing artifact fails each check
// with the same actionable message.
func mustLoadUnit(t *testing.T) *quadletUnit {
	t.Helper()
	return parseUnit(readRepoFile(t, unitRelPath))
}

// mounts returns every Mount= directive of the [Container] section as parsed
// bind mounts.
func (u *quadletUnit) mounts() []bindMount {
	var out []bindMount
	for _, v := range u.values("Container", "Mount") {
		out = append(out, parseBindMount(v))
	}

	return out
}

// execFlagValue returns the value assigned to flag across all Exec=
// directives (e.g. "--kubeconfig"), or "" when the flag is absent. Flags
// share a single space-separated Exec= line (podman 6.x quadlet is
// last-wins on repeated Exec=), so each directive is tokenized.
func (u *quadletUnit) execFlagValue(flag string) string {
	for _, v := range u.values("Container", "Exec") {
		for token := range strings.FieldsSeq(v) {
			if assigned, ok := strings.CutPrefix(token, flag+"="); ok {
				return assigned
			}
		}
	}

	return ""
}

// TestUnitArtifactExists covers REQ-007 clause 1: CAPH commits
// deploy/cluster-api-hypervisor.container.
func TestUnitArtifactExists(t *testing.T) {
	data := readRepoFile(t, unitRelPath)
	if strings.TrimSpace(data) == "" {
		t.Fatalf("%s exists but is empty", unitRelPath)
	}
}

// TestUnitCoreDirectives ensures the manager container has no direct
// host-lifecycle privilege or device access.
func TestUnitCoreDirectives(t *testing.T) {
	u := mustLoadUnit(t)

	if got := u.values("Container", "Image"); len(got) != 1 || got[0] != "localhost/cluster-api-hypervisor:dev" {
		t.Errorf("[Container] Image = %v, want exactly [localhost/cluster-api-hypervisor:dev]", got)
	}

	if got := u.values("Container", "Network"); len(got) != 1 || got[0] != "host" {
		t.Errorf("[Container] Network = %v, want exactly [host]", got)
	}

	for _, value := range u.values("Container", "PodmanArgs") {
		if strings.Contains(value, "--device") || strings.Contains(value, "--privileged") ||
			strings.Contains(value, "seccomp=unconfined") {
			t.Errorf("[Container] PodmanArgs = %q exposes a prohibited host-lifecycle surface", value)
		}
	}

	if caps := u.values("Container", "AddCapability"); len(caps) != 0 {
		t.Errorf("[Container] AddCapability = %v; manager must not add capabilities", caps)
	}
}

// TestUnitSingleExecLine covers REQ-007: the manager flags ride exactly ONE
// Exec= directive. podman 6.x quadlet treats repeated Exec= as last-wins,
// so multiple lines silently drop every flag except the last and the
// manager exits via GetConfigOrDie.
func TestUnitSingleExecLine(t *testing.T) {
	u := mustLoadUnit(t)

	execLines := u.values("Container", "Exec")
	if len(execLines) != 1 {
		t.Fatalf(
			"[Container] Exec = %v, want exactly one directive (podman 6.x quadlet is last-wins on repeated Exec=)",
			execLines,
		)
	}

	flags := []string{
		"--kubeconfig=",
		"--webhook-cert-dir=",
		"--webhook-port=",
		"--health-addr=",
		"--hypervisorcluster-concurrency=",
		"--hypervisormachine-concurrency=",
		"--hypervisorconfig-concurrency=",
		"--hypervisorcontrolplane-concurrency=",
		"--agent-address=",
		"--agent-server-name=",
		"--agent-client-cert-dir=",
		"--agent-ca=",
	}
	for _, flag := range flags {
		if !strings.Contains(execLines[0], flag) {
			t.Errorf("single Exec= line %q misses flag %s", execLines[0], strings.TrimSuffix(flag, "="))
		}
	}
}

// TestUnitMounts admits only manager inputs and makes every credential mount
// read-only. Host lifecycle data belongs exclusively to the HostAgent.
func TestUnitMounts(t *testing.T) {
	u := mustLoadUnit(t)
	mounts := u.mounts()

	findTarget := func(target string) *bindMount {
		for i, mount := range mounts {
			if mount.target == target {
				return &mounts[i]
			}
		}

		return nil
	}

	for _, required := range []struct {
		target string
		flag   string
	}{
		{"/tmp/k8s-webhook-server/serving-certs", "--webhook-cert-dir"},
		{"/etc/kubernetes/mgmt/hypervisor.kubeconfig", "--kubeconfig"},
		{"/tls/agent-client", "--agent-client-cert-dir"},
		{"/tls/agent-ca/ca.crt", "--agent-ca"},
	} {
		mount := findTarget(required.target)
		if mount == nil {
			t.Errorf("no Mount targets %s", required.target)
			continue
		}

		if !mount.readonly {
			t.Errorf("Mount target %s is not read-only", required.target)
		}

		if got := u.execFlagValue(required.flag); got != required.target {
			t.Errorf("%s = %q, want %q", required.flag, got, required.target)
		}
	}

	for _, mount := range mounts {
		for _, prohibited := range []string{"/build", "/state", "/tmp/ch-capi", "/run/user", "/dev/kvm", "k8netd"} {
			if strings.Contains(mount.source, prohibited) || strings.Contains(mount.target, prohibited) {
				t.Errorf("Mount %+v exposes prohibited manager host-lifecycle surface %q", mount, prohibited)
			}
		}
	}
}

func TestUnitHasNoDirectLifecycleEnvironment(t *testing.T) {
	u := mustLoadUnit(t)
	for _, line := range u.values("Container", "Environment") {
		key, _, _ := strings.Cut(line, "=")
		if strings.HasPrefix(key, "HYPERVISOR_") {
			t.Errorf("Environment %q exposes direct host-lifecycle configuration", key)
		}
	}
}

func TestUnitOrderingAndRestart(t *testing.T) {
	u := mustLoadUnit(t)

	for _, directive := range []string{"After", "Wants"} {
		joined := strings.Join(u.values("Unit", directive), " ")
		if !strings.Contains(joined, capishimPod) {
			t.Errorf("[Unit] %s must include %s", directive, capishimPod)
		}

		if strings.Contains(joined, k8netdUnit) {
			t.Errorf("[Unit] %s must not depend on %s", directive, k8netdUnit)
		}
	}

	if got := u.values("Service", "Restart"); len(got) == 0 || got[0] != "always" {
		t.Errorf("[Service] Restart = %v, want always", got)
	}

	if got := u.values("Unit", "StartLimitIntervalSec"); len(got) == 0 || got[0] != "0" {
		t.Errorf("[Unit] StartLimitIntervalSec = %v, want 0", got)
	}

	if got := u.values("Unit", "StartLimitBurst"); len(got) == 0 || got[0] != "0" {
		t.Errorf("[Unit] StartLimitBurst = %v, want 0", got)
	}
}

func TestUnitHeaderDocumentsManagerInputs(t *testing.T) {
	u := mustLoadUnit(t)
	if len(u.headerComments) == 0 {
		t.Fatalf("%s has no header comments", unitRelPath)
	}

	header := strings.ToLower(strings.Join(u.headerComments, "\n"))
	for _, required := range []string{
		capishimRootDefault,
		"hostagent",
		"mTLS",
		"do not add host kvm",
	} {
		if !strings.Contains(header, strings.ToLower(required)) {
			t.Errorf("header comments must document %q", required)
		}
	}

	for _, prohibited := range []string{"k8netd", "cloud hypervisor", "artifact", "state mounts"} {
		if !strings.Contains(header, prohibited) {
			t.Errorf("header comments must prohibit direct %s access", prohibited)
		}
	}
}

// runMakeDryRun shells out to make -n install-quadlet in the repository root
// and returns the combined output. Dry-run mode never executes recipes, so
// the check is safe regardless of host state.
func runMakeDryRun(t *testing.T) string {
	t.Helper()

	cmd := exec.Command("make", "-n", "install-quadlet")
	cmd.Dir = filepath.Join("..", "..")
	cmd.Env = os.Environ()

	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("make -n install-quadlet failed: %v\noutput:\n%s", err, out)
	}

	return string(out)
}

// TestMakefileInstallQuadletTarget covers REQ-007 / VC-08: the Makefile
// provides install-quadlet and installs the unit into the user quadlet
// directory under the committed artifact's name.
func TestMakefileInstallQuadletTarget(t *testing.T) {
	out := runMakeDryRun(t)
	if strings.TrimSpace(out) == "" {
		t.Fatal("make -n install-quadlet printed no recipe; the target must install the unit")
	}

	if !strings.Contains(out, unitInstallSuffix) {
		t.Errorf("install-quadlet recipe does not name %s; output:\n%s", unitInstallSuffix, out)
	}
}

// TestMakefileInstallQuadletIdempotent covers REQ-007: running
// install-quadlet twice is idempotent. Verified at dry-run level: the recipe
// is deterministic across invocations, creates the destination directory
// non-destructively, and copies with an overwrite-safe command.
func TestMakefileInstallQuadletIdempotent(t *testing.T) {
	first := runMakeDryRun(t)

	second := runMakeDryRun(t)
	if first != second {
		t.Errorf("install-quadlet recipe is not deterministic between dry runs;\nfirst:\n%s\nsecond:\n%s", first, second)
	}

	hasDirCreation := strings.Contains(first, "mkdir -p") ||
		strings.Contains(first, "install -D") ||
		strings.Contains(first, "install -d")
	if !hasDirCreation {
		t.Errorf(
			"recipe neither mkdir -p's the destination nor uses install -D/-d; re-runs are not guaranteed safe:\n%s",
			first,
		)
	}

	copyCmdIdx := -1

	for i, word := range strings.Fields(first) {
		if word == "install" || word == "cp" {
			copyCmdIdx = i
			break
		}
	}

	if copyCmdIdx < 0 {
		t.Errorf("recipe uses neither install nor cp to place the unit; output:\n%s", first)
		return
	}

	if !strings.Contains(first[copyCmdIdx:], unitInstallSuffix) {
		t.Errorf("copy command does not write %s; output:\n%s", unitInstallSuffix, first)
	}
}
