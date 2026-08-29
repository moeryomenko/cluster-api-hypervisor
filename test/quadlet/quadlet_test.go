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
	"regexp"
	"strings"
	"testing"
)

const (
	unitRelPath       = "deploy/cluster-api-hypervisor.container"
	unitInstallSuffix = ".config/containers/systemd/cluster-api-hypervisor.container"

	k8netdUnit  = "k8netd.service"
	capishimPod = "capishim-pod.service"

	labRootDefault      = ".local/state/k8slab"
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

// TestUnitCoreDirectives covers REQ-007: Image=localhost/..., Network=host,
// KVM passthrough via PodmanArgs --device /dev/kvm (podman 6.x quadlet has
// no Device= key), seccomp=unconfined via PodmanArgs (CH v48's internal
// filter SIGSYS-kills its API thread under container profiles; we disable
// podman's default profile at the container boundary instead), no privileged
// mode, no added capabilities.
func TestUnitCoreDirectives(t *testing.T) {
	u := mustLoadUnit(t)

	if got := u.values("Container", "Image"); len(got) != 1 || got[0] != "localhost/cluster-api-hypervisor:dev" {
		t.Errorf("[Container] Image = %v, want exactly [localhost/cluster-api-hypervisor:dev]", got)
	}

	var networkHost bool
	for _, v := range u.values("Container", "Network") {
		if v == "host" {
			networkHost = true
		}
	}
	if !networkHost {
		t.Errorf("[Container] Network = %v, want host among values", u.values("Container", "Network"))
	}

	var kvmDevice bool
	for _, v := range u.values("Container", "PodmanArgs") {
		if strings.Contains(v, "--device /dev/kvm") {
			kvmDevice = true
		}
	}
	if !kvmDevice {
		t.Errorf("[Container] PodmanArgs = %v, want --device /dev/kvm among values", u.values("Container", "PodmanArgs"))
	}

	var seccompUnconfined bool
	for _, v := range u.values("Container", "PodmanArgs") {
		if strings.Contains(v, "--security-opt seccomp=unconfined") {
			seccompUnconfined = true
		}
	}
	if !seccompUnconfined {
		t.Errorf(
			"[Container] PodmanArgs = %v, want --security-opt seccomp=unconfined among values",
			u.values("Container", "PodmanArgs"),
		)
	}

	for _, v := range u.values("Container", "PodmanArgs") {
		if strings.Contains(v, "--privileged") {
			t.Errorf("[Container] PodmanArgs = %q grants privileged mode; REQ-007 forbids it", v)
		}
	}

	// Keys are lowercased at parse time, so this catches addcapability= too.
	if caps := u.values("Container", "AddCapability"); len(caps) != 0 {
		t.Errorf("[Container] AddCapability = %v; REQ-007 forbids added capabilities", caps)
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
	}
	for _, flag := range flags {
		if !strings.Contains(execLines[0], flag) {
			t.Errorf("single Exec= line %q misses flag %s", execLines[0], strings.TrimSuffix(flag, "="))
		}
	}
}

// TestUnitMounts covers REQ-007 mount clauses: lab build dir at /build,
// k8netd runtime dir and ch socket dir at their absolute host paths, the
// capishim webhook-cert subtree at --webhook-cert-dir, the capishim
// hypervisor kubeconfig read-only at --kubeconfig, and the capishim pki
// subtree read-only at the identical in-container path (the kubeconfig
// references pki material by absolute host path).
func TestUnitMounts(t *testing.T) {
	u := mustLoadUnit(t)
	mounts := u.mounts()

	findTarget := func(target string) *bindMount {
		for i, m := range mounts {
			if m.target == target {
				return &mounts[i]
			}
		}
		return nil
	}
	findSourceContaining := func(sub string) *bindMount {
		for i, m := range mounts {
			if strings.Contains(m.source, sub) {
				return &mounts[i]
			}
		}
		return nil
	}

	build := findTarget("/build")
	if build == nil {
		t.Fatalf("no Mount targets /build; want the lab build dir (base image, firmware, vm-disks)")
	}
	if !strings.HasSuffix(build.source, "/build") {
		t.Errorf("/build mount source = %q, want the lab build dir path ending in /build", build.source)
	}

	runtimeDir := "/run/user/1000/k8snet"
	k8netdMount := findTarget(runtimeDir)
	if k8netdMount == nil {
		t.Fatalf("no Mount targets %s; want the k8netd runtime dir at its absolute path", runtimeDir)
	}
	if k8netdMount.source != runtimeDir {
		t.Errorf("k8netd runtime mount source = %q, want identical paths (%s)", k8netdMount.source, runtimeDir)
	}

	socketDir := "/tmp/ch-capi"
	chMount := findTarget(socketDir)
	if chMount == nil {
		t.Fatalf("no Mount targets %s; want the cloud-hypervisor socket dir at its absolute path", socketDir)
	}
	if chMount.source != socketDir {
		t.Errorf("ch socket mount source = %q, want identical paths (%s)", chMount.source, socketDir)
	}

	webhookCerts := findSourceContaining("webhook-certs/hypervisor")
	if webhookCerts == nil {
		t.Fatalf("no Mount sources <capishim-state>/webhook-certs/hypervisor; mounts = %+v", mounts)
	}
	wantCertTarget := u.execFlagValue("--webhook-cert-dir")
	if wantCertTarget == "" {
		t.Fatal("Exec= carries no --webhook-cert-dir; cannot tie the webhook cert mount to its consumer")
	}
	if webhookCerts.target != wantCertTarget {
		t.Errorf("webhook cert mount target = %q, want the --webhook-cert-dir value %q", webhookCerts.target, wantCertTarget)
	}

	kubeconfig := findSourceContaining("kubeconfigs/hypervisor.kubeconfig")
	if kubeconfig == nil {
		t.Fatalf("no Mount sources <capishim-state>/kubeconfigs/hypervisor.kubeconfig; mounts = %+v", mounts)
	}
	if !kubeconfig.readonly {
		t.Errorf("hypervisor.kubeconfig mount (source %q) is not read-only; REQ-007 requires ro", kubeconfig.source)
	}
	wantKubeconfigTarget := u.execFlagValue("--kubeconfig")
	if wantKubeconfigTarget == "" {
		t.Fatal("Exec= carries no --kubeconfig; cannot tie the kubeconfig mount to its consumer")
	}
	if kubeconfig.target != wantKubeconfigTarget {
		t.Errorf("kubeconfig mount target = %q, want the --kubeconfig value %q", kubeconfig.target, wantKubeconfigTarget)
	}

	pki := findSourceContaining("capishim/pki")
	if pki == nil {
		t.Fatalf(
			"no Mount sources <capishim-state>/pki; the mgmt kubeconfig references PKI material by absolute host path; mounts = %+v",
			mounts,
		)
	}
	if !pki.readonly {
		t.Errorf("pki mount (source %q) is not read-only; REQ-007 requires ro", pki.source)
	}
	if pki.source != pki.target {
		t.Errorf("pki mount source %q != target %q; want the same-path bind mount pattern", pki.source, pki.target)
	}
}

// TestUnitEnvironmentSurface covers REQ-007: the Environment block carries
// the full HYPERVISOR_* surface of docs/install-contract.md section 3.
func TestUnitEnvironmentSurface(t *testing.T) {
	u := mustLoadUnit(t)

	env := map[string]string{}
	for _, line := range u.values("Container", "Environment") {
		if key, value, ok := strings.Cut(line, "="); ok {
			env[key] = value
		}
	}

	surface := []string{
		"HYPERVISOR_BASE_IMAGE",
		"HYPERVISOR_FIRMWARE",
		"HYPERVISOR_VM_DISKS_DIR",
		"HYPERVISOR_SOCKET_DIR",
		"HYPERVISOR_STATE_DIR",
		"HYPERVISOR_CH_BINARY",
		"HYPERVISOR_QEMU_IMG",
		"HYPERVISOR_K8NETD_SOCKET",
		"HYPERVISOR_NETWORK_CIDR",
	}
	for _, key := range surface {
		value, ok := env[key]
		switch {
		case !ok:
			t.Errorf("Environment missing %s; the HYPERVISOR_* surface must be explicit", key)
		case strings.TrimSpace(value) == "":
			t.Errorf("Environment %s is empty; every HYPERVISOR_* value must be stated", key)
		}
	}
}

// TestUnitOrderingAndRestart covers REQ-007 ordering clauses: After=/Wants=
// on k8netd.service and the capishim pod unit, Restart=always, and the
// disabled start rate limit ([Unit]; current systemd ignores these keys
// under [Service]) that lets the provider outlive long capishim setup
// windows.
func TestUnitOrderingAndRestart(t *testing.T) {
	u := mustLoadUnit(t)

	for _, directive := range []string{"After", "Wants"} {
		values := u.values("Unit", directive)
		joined := strings.Join(values, " ")
		for _, unit := range []string{k8netdUnit, capishimPod} {
			if !strings.Contains(joined, unit) {
				t.Errorf("[Unit] %s = %v, want %s included", directive, values, unit)
			}
		}
	}

	if got := u.values("Service", "Restart"); len(got) == 0 || got[0] != "always" {
		t.Errorf("[Service] Restart = %v, want always", got)
	}

	if got := u.values("Unit", "StartLimitIntervalSec"); len(got) == 0 || got[0] != "0" {
		t.Errorf("[Unit] StartLimitIntervalSec = %v, want 0 (rate limiting disabled)", got)
	}
	if got := u.values("Unit", "StartLimitBurst"); len(got) == 0 || got[0] != "0" {
		t.Errorf("[Unit] StartLimitBurst = %v, want 0 (rate limiting disabled)", got)
	}
}

// TestUnitHeaderPathVariables covers REQ-007: the two host-path roots (lab
// build/state root and capishim state root) are documented in the unit
// header comments, with defaults matching the k8labs layout.
func TestUnitHeaderPathVariables(t *testing.T) {
	u := mustLoadUnit(t)

	if len(u.headerComments) == 0 {
		t.Fatalf("%s has no header comments; the two path roots must be documented there", unitRelPath)
	}

	// A documented variable looks like NAME=<default> or prose naming the
	// default; both must state the k8labs-layout default path.
	varAssign := regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*\s*[=:]\s*\S*`)
	documented := func(defaultPath string) bool {
		for _, comment := range u.headerComments {
			lower := strings.ToLower(comment)
			if !strings.Contains(lower, defaultPath) {
				continue
			}
			if strings.Contains(lower, "default") || varAssign.MatchString(comment) {
				return true
			}
		}
		return false
	}

	if !documented(labRootDefault) {
		t.Errorf("header comments do not document the lab build/state root variable with its %s default", labRootDefault)
	}
	if !documented(capishimRootDefault) {
		t.Errorf("header comments do not document the capishim state root variable with its %s default", capishimRootDefault)
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
