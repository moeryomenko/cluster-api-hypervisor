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

// Package confext turns rendered node configuration trees into the per-role
// squashfs images (the .raw files) that the confext data disk carries to a
// node and systemd-confext merges at first boot.
package confext

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Runner is the command-execution seam of the packager: Run executes the
// program named name with the given arguments and returns its combined
// output and error.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// Option configures a Packager at construction time.
type Option func(*Packager)

// Packager materializes a rendered tree on disk and packages each confext
// into a .raw squashfs image.
type Packager struct {
	runner Runner
}

// execRunner is the default Runner: it executes the command on the host and
// returns the process combined output.
type execRunner struct{}

// Run executes name with args on the host and returns the combined output.
func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

// NewPackager constructs a packager. Options are applied in order; without
// options the packager executes commands with the host mksquashfs binary via
// os/exec.
func NewPackager(opts ...Option) *Packager {
	p := &Packager{runner: execRunner{}}
	for _, opt := range opts {
		opt(p)
	}

	return p
}

// WithRunner replaces the command runner of the packager.
func WithRunner(r Runner) Option {
	return func(p *Packager) {
		p.runner = r
	}
}

// WriteTree materializes tree under stagingDir, creating the staging
// directory and any intermediate directories as needed. Every key is a
// slash-separated path whose first segment names the confext and whose
// remainder is the file path inside it, so the key
// "z-kubernetes-cp/etc/kubernetes/pki/ca.pem" becomes
// stagingDir/z-kubernetes-cp/etc/kubernetes/pki/ca.pem holding the key's
// bytes exactly. An empty tree, a key with no path separator, or a key that
// begins with a separator is rejected with an error.
func (p *Packager) WriteTree(tree map[string][]byte, stagingDir string) error {
	if len(tree) == 0 {
		return errors.New("confext: cannot write an empty tree")
	}

	for key, content := range tree {
		if !strings.Contains(key, "/") {
			return fmt.Errorf("confext: tree key %q has no path separator", key)
		}

		if strings.HasPrefix(key, "/") {
			return fmt.Errorf("confext: tree key %q begins with a separator", key)
		}

		path := filepath.Join(stagingDir, filepath.FromSlash(key))
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			return fmt.Errorf("confext: create tree directory for %q: %w", key, err)
		}

		if err := os.WriteFile(path, content, 0o644); err != nil {
			return fmt.Errorf("confext: write tree file %q: %w", path, err)
		}
	}

	return nil
}

// BuildRaws runs mksquashfs once per top-level directory of stagingDir — the
// confexts — with exactly the arguments mksquashfs <stagingDir>/<name>
// <outDir>/<name>.raw -noappend -all-root. Non-directory entries are not
// confexts and are ignored. outDir is created when missing. The returned
// paths are the produced <outDir>/<name>.raw files. A stagingDir with no
// confexts is an error, and the first mksquashfs failure aborts the build:
// the error (detectable with errors.Is) is returned and no partial path list
// is produced.
func (p *Packager) BuildRaws(ctx context.Context, stagingDir, outDir string) ([]string, error) {
	entries, err := os.ReadDir(stagingDir)
	if err != nil {
		return nil, fmt.Errorf("confext: read staging dir %q: %w", stagingDir, err)
	}

	var names []string

	for _, entry := range entries {
		if entry.IsDir() {
			names = append(names, entry.Name())
		}
	}

	if len(names) == 0 {
		return nil, fmt.Errorf("confext: no confext trees found under %q", stagingDir)
	}

	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return nil, fmt.Errorf("confext: create output dir %q: %w", outDir, err)
	}

	paths := make([]string, 0, len(names))
	for _, name := range names {
		src := filepath.Join(stagingDir, name)

		dst := filepath.Join(outDir, name+".raw")
		if _, err := p.runner.Run(ctx, "mksquashfs", src, dst, "-noappend", "-all-root"); err != nil {
			return nil, fmt.Errorf("confext: mksquashfs %s -> %s: %w", src, dst, err)
		}

		paths = append(paths, dst)
	}

	return paths, nil
}
