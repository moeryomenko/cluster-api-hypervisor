// Toolchain contract test (spec REQ-001, repository layout and tooling).
//
// Red phase: the repository has no go.mod, no tools/go.mod, and no Makefile,
// so `go test .` fails at the go command level with "go: cannot find main
// module". To observe the per-element failure messages before a module
// exists, run the assertions directly in GOPATH mode:
//
//	GO111MODULE=off go test .
//
// The subprocess checks (`go tool <name>`) force GO111MODULE=on and
// GOPROXY=off so they are deterministic, offline, and fail fast.

package main

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// Toolchain contract values (source of truth: scaffold task).
const (
	wantModulePath = "github.com/moeryomenko/cluster-api-hypervisor"
	wantGoVersion  = "1.26.0"
)

// requiredTools are the Go tool directives that tools/go.mod must declare
// and that must be resolvable as `go tool <name>`.
var requiredTools = []string{
	"controller-gen",
	"golangci-lint",
	"gotestsum",
	"golines",
	"gofumpt",
	"goimports",
	"setup-envtest",
}

// requiredMakeTargets are the Makefile targets the scaffold must define.
var requiredMakeTargets = []string{
	"build", "test", "lint", "fmt", "vet", "tidy",
	"generate", "generate-check", "check", "clean", "help", "image",
}

func TestToolchainContract(t *testing.T) {
	t.Run("go.mod declares module path and go directive", func(t *testing.T) {
		raw, err := os.ReadFile("go.mod")
		if err != nil {
			t.Fatalf("go.mod: missing module directive: %v", err)
		}

		var modulePath, goVersion string
		goDirective := regexp.MustCompile(`^go ([0-9]+(?:\.[0-9]+)*)$`)
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			switch {
			case strings.HasPrefix(line, "module "):
				modulePath = strings.TrimPrefix(line, "module ")
			case goDirective.MatchString(line):
				goVersion = goDirective.FindStringSubmatch(line)[1]
			}
		}

		if modulePath == "" {
			t.Errorf("go.mod: missing module directive (want %q)", wantModulePath)
		} else if modulePath != wantModulePath {
			t.Errorf("go.mod: module directive = %q, want %q", modulePath, wantModulePath)
		}

		if goVersion == "" {
			t.Errorf("go.mod: missing go directive (want %q)", wantGoVersion)
		} else if goVersion != wantGoVersion {
			t.Errorf("go.mod: go directive = %q, want %q", goVersion, wantGoVersion)
		}
	})

	t.Run("required tools resolve via go tool", func(t *testing.T) {
		for _, name := range requiredTools {
			t.Run(name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
				defer cancel()

				cmd := exec.CommandContext(ctx, "go", "tool", name)
				cmd.Env = append(os.Environ(), "GO111MODULE=on", "GOPROXY=off")
				out, err := cmd.CombinedOutput()
				if err != nil {
					t.Fatalf("go tool %s: not resolvable: %v: %s",
						name, err, strings.TrimSpace(string(out)))
				}
			})
		}
	})

	t.Run("Makefile defines required targets", func(t *testing.T) {
		raw, err := os.ReadFile("Makefile")
		if err != nil {
			t.Fatalf("Makefile: missing (cannot read Makefile: %v); required targets: %s",
				err, strings.Join(requiredMakeTargets, ", "))
		}

		present := make(map[string]bool)
		targetRE := regexp.MustCompile(`^([A-Za-z0-9_][A-Za-z0-9_.-]*):`)
		for _, line := range strings.Split(string(raw), "\n") {
			line = strings.TrimSpace(line)
			if m := targetRE.FindStringSubmatch(line); m != nil {
				present[m[1]] = true
			}
		}

		for _, target := range requiredMakeTargets {
			if !present[target] {
				t.Errorf("Makefile: missing target %q", target)
			}
		}
	})
}
