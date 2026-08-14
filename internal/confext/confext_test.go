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

// The confext packager contract (test-first).
//
// This suite pins the behavior of the confext packaging step of the node
// configuration delivery path: the rendered tree files for the node's role
// confexts arrive as a map of slash-separated paths to content, and the
// packager turns them into the per-confext squashfs images (the .raw files)
// that are carried to the node on the confext data disk and merged by
// systemd-confext at first boot. The mksquashfs invocation shape matches the
// role-split packaging script that today produces the same images for the
// lab's phase-B runtime trees.
//
// The contract, in prose:
//
//   - Runner is the command-execution seam: Run(ctx, name, args...) runs an
//     external program and returns its combined output and error. Production
//     uses the host mksquashfs binary via exec.CommandContext; tests inject a
//     recording fake so no external tool is ever invoked.
//   - NewPackager(...Option) *Packager constructs a packager.
//     WithRunner(r Runner) Option replaces the command runner; without it the
//     packager uses the default exec-based runner.
//   - WriteTree(tree map[string][]byte, stagingDir string) error
//     materializes the tree under stagingDir, creating the directory and any
//     intermediate directories as needed. Every key is a slash-separated path
//     whose first segment names the confext and whose remainder is the file
//     path inside it, so the key "z-kubernetes-cp/etc/kubernetes/pki/ca.pem"
//     becomes stagingDir/z-kubernetes-cp/etc/kubernetes/pki/ca.pem holding
//     the key's bytes exactly. An empty tree, a key with no path separator,
//     or a key that begins with a separator is rejected with an error.
//   - BuildRaws(ctx, stagingDir, outDir string) ([]string, error) runs
//     mksquashfs once per top-level directory of stagingDir — the confexts —
//     with exactly these arguments: mksquashfs <stagingDir>/<name>
//     <outDir>/<name>.raw -noappend -all-root. Non-directory entries are not
//     confexts and are ignored. outDir is created when missing. The returned
//     paths are the produced <outDir>/<name>.raw files. A stagingDir with no
//     confexts is an error, and the first mksquashfs failure aborts the
//     build: the error (detectable with errors.Is) is returned and no
//     partial path list is produced.
package confext_test

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
)

// Compile-time pins: the constructor, the runner option, and the two
// packaging steps must exist with exactly these names and signatures.
var (
	_ func(...confext.Option) *confext.Packager                                  = confext.NewPackager
	_ func(confext.Runner) confext.Option                                        = confext.WithRunner
	_ func(*confext.Packager, map[string][]byte, string) error                   = (*confext.Packager).WriteTree
	_ func(*confext.Packager, context.Context, string, string) ([]string, error) = (*confext.Packager).BuildRaws
	_ confext.Runner                                                             = (*fakeRunner)(nil)
)

// recordedCall is one captured command invocation: the program name and the
// exact argument list.
type recordedCall struct {
	name string
	args []string
}

// fakeRunner records every invocation and returns a canned output. When
// failOn is non-zero the failOn-th invocation returns err instead of the
// canned output, letting tests fail a specific mksquashfs call.
type fakeRunner struct {
	calls  []recordedCall
	out    []byte
	err    error
	failOn int
}

// Run implements confext.Runner. The context is accepted and deliberately
// ignored: cancellation propagation is an implementation concern of the
// default exec runner, not part of this contract.
func (f *fakeRunner) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	argsCopy := make([]string, len(args))
	copy(argsCopy, args)
	f.calls = append(f.calls, recordedCall{name: name, args: argsCopy})
	if f.failOn > 0 && len(f.calls) == f.failOn {
		return nil, f.err
	}

	return f.out, nil
}

// sampleTree mirrors the role-split tree set the bootstrap side renders for a
// control-plane node: the etcd config, the control-plane PKI and kubeconfigs,
// the node kubelet config, and the extension-release metadata each confext
// ships. Keys are slash-separated paths whose first segment is the confext
// name.
var sampleTree = map[string][]byte{
	"z-etcd/etc/etcd/etcd.conf.yml":                           []byte("etcd config"),
	"z-etcd/etc/extension-release.d/extension-release.z-etcd": []byte("ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\n"),
	"z-kubernetes-cp/etc/kubernetes/cp.env":                   []byte("cp env"),
	"z-kubernetes-cp/etc/kubernetes/pki/ca.pem":               []byte("ca cert"),
	"z-kubernetes-cp/etc/kubernetes/pki/ca-key.pem":           []byte("ca key"),
	"z-kubelet-node1/etc/kubernetes/kubelet.conf":             []byte("kubelet config"),
}

// makeStagingTree creates a fresh staging dir containing one directory per
// confext name, each with a marker file, and returns the staging dir.
func makeStagingTree(t *testing.T, names ...string) string {
	t.Helper()

	staging := t.TempDir()
	for _, name := range names {
		dir := filepath.Join(staging, name, "etc")
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatalf("MkdirAll(%q) error: %v", dir, err)
		}
		if err := os.WriteFile(filepath.Join(dir, "marker"), []byte(name), 0o644); err != nil {
			t.Fatalf("WriteFile marker error: %v", err)
		}
	}

	return staging
}

// TestWriteTreeMaterializesFiles pins the staging write: every tree key is
// written at its full slash path under the staging dir (which is created when
// missing, along with intermediate directories) with the exact bytes.
func TestWriteTreeMaterializesFiles(t *testing.T) {
	p := confext.NewPackager()
	staging := filepath.Join(t.TempDir(), "trees") // does not exist yet

	if err := p.WriteTree(sampleTree, staging); err != nil {
		t.Fatalf("WriteTree error: %v", err)
	}

	for key, want := range sampleTree {
		path := filepath.Join(staging, filepath.FromSlash(key))
		got, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("ReadFile(%q) error: %v", path, err)
			continue
		}
		if !bytes.Equal(got, want) {
			t.Errorf("content of %q = %q, want %q", path, got, want)
		}
	}
}

// TestWriteTreeEmptyTree pins the empty-tree rejection: a tree with no files
// has no confext roots and is an error.
func TestWriteTreeEmptyTree(t *testing.T) {
	p := confext.NewPackager()

	if err := p.WriteTree(map[string][]byte{}, t.TempDir()); err == nil {
		t.Error("WriteTree with an empty tree succeeded, want an error")
	}
}

// TestWriteTreeRejectsInvalidKeys pins the tree-layout validation: every key
// must be a slash-separated path with a non-empty confext name segment.
func TestWriteTreeRejectsInvalidKeys(t *testing.T) {
	tests := []struct {
		name string
		key  string
	}{
		{name: "key without a path separator", key: "z-etcd"},
		{name: "key with a leading separator", key: "/etc/kubernetes/kubelet.conf"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			p := confext.NewPackager()
			if err := p.WriteTree(map[string][]byte{tt.key: []byte("content")}, t.TempDir()); err == nil {
				t.Errorf("WriteTree with key %q succeeded, want an error", tt.key)
			}
		})
	}
}

// TestBuildRawsInvokesMksquashfsPerConfext pins the exact mksquashfs
// invocations: one per top-level directory of the staging dir, with the
// source tree, the .raw destination, and the -noappend -all-root flags in
// that order. Non-directory entries are not confexts and produce no
// invocation.
func TestBuildRawsInvokesMksquashfsPerConfext(t *testing.T) {
	staging := makeStagingTree(t, "z-etcd", "z-kubernetes-cp", "z-kubelet-node1")
	if err := os.WriteFile(filepath.Join(staging, "not-a-tree"), []byte("x"), 0o644); err != nil {
		t.Fatalf("WriteFile stray entry error: %v", err)
	}

	out := filepath.Join(t.TempDir(), "out")
	runner := &fakeRunner{out: []byte("squashed")}
	p := confext.NewPackager(confext.WithRunner(runner))

	if _, err := p.BuildRaws(t.Context(), staging, out); err != nil {
		t.Fatalf("BuildRaws error: %v", err)
	}

	want := []recordedCall{
		{
			name: "mksquashfs",
			args: []string{filepath.Join(staging, "z-etcd"), filepath.Join(out, "z-etcd.raw"), "-noappend", "-all-root"},
		},
		{
			name: "mksquashfs",
			args: []string{
				filepath.Join(staging, "z-kubernetes-cp"),
				filepath.Join(out, "z-kubernetes-cp.raw"),
				"-noappend",
				"-all-root",
			},
		},
		{
			name: "mksquashfs",
			args: []string{
				filepath.Join(staging, "z-kubelet-node1"),
				filepath.Join(out, "z-kubelet-node1.raw"),
				"-noappend",
				"-all-root",
			},
		},
	}
	// Invocation order is not part of the contract; compare as multisets
	// keyed by the source tree path.
	sort.Slice(runner.calls, func(i, j int) bool { return runner.calls[i].args[0] < runner.calls[j].args[0] })
	sort.Slice(want, func(i, j int) bool { return want[i].args[0] < want[j].args[0] })
	if !reflect.DeepEqual(runner.calls, want) {
		t.Errorf("mksquashfs invocations = %+v, want %+v", runner.calls, want)
	}
}

// TestBuildRawsReturnsRawPaths pins the output naming: each confext becomes
// <outDir>/<name>.raw, and the returned paths name exactly those images.
func TestBuildRawsReturnsRawPaths(t *testing.T) {
	staging := makeStagingTree(t, "z-etcd", "z-kubernetes-cp", "z-kubelet-node1")
	out := t.TempDir()
	p := confext.NewPackager(confext.WithRunner(&fakeRunner{out: []byte("squashed")}))

	got, err := p.BuildRaws(t.Context(), staging, out)
	if err != nil {
		t.Fatalf("BuildRaws error: %v", err)
	}

	want := []string{
		filepath.Join(out, "z-etcd.raw"),
		filepath.Join(out, "z-kubernetes-cp.raw"),
		filepath.Join(out, "z-kubelet-node1.raw"),
	}
	// The path list order is not part of the contract; compare as multisets.
	sort.Strings(got)
	sort.Strings(want)
	if !reflect.DeepEqual(got, want) {
		t.Errorf("BuildRaws paths = %v, want %v", got, want)
	}
}

// TestBuildRawsCreatesOutputDir pins that BuildRaws creates the output
// directory, including nested paths, when it does not exist yet.
func TestBuildRawsCreatesOutputDir(t *testing.T) {
	staging := makeStagingTree(t, "z-etcd")
	out := filepath.Join(t.TempDir(), "deep", "nested", "out")
	p := confext.NewPackager(confext.WithRunner(&fakeRunner{out: []byte("squashed")}))

	if _, err := p.BuildRaws(t.Context(), staging, out); err != nil {
		t.Fatalf("BuildRaws error: %v", err)
	}

	if fi, err := os.Stat(out); err != nil || !fi.IsDir() {
		t.Errorf("output dir %q not created (stat error: %v)", out, err)
	}
}

// TestBuildRawsPropagatesExecError pins the exec-failure contract: a
// mksquashfs failure surfaces as an error from BuildRaws, detectable with
// errors.Is against the runner's error.
func TestBuildRawsPropagatesExecError(t *testing.T) {
	staging := makeStagingTree(t, "z-etcd", "z-kubernetes-cp")
	boom := errors.New("mksquashfs exploded")
	p := confext.NewPackager(confext.WithRunner(&fakeRunner{err: boom, failOn: 1}))

	paths, err := p.BuildRaws(t.Context(), staging, t.TempDir())
	if err == nil {
		t.Fatal("BuildRaws succeeded, want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("BuildRaws error %v does not wrap the runner error %v", err, boom)
	}
	if paths != nil {
		t.Errorf("BuildRaws returned paths %v on error, want nil", paths)
	}
}

// TestBuildRawsStopsAtFirstFailureNoPartialPaths pins the abort semantics:
// when a later mksquashfs call fails, the build stops, the error is surfaced
// (errors.Is), no partial path list is returned, and no further invocations
// happen.
func TestBuildRawsStopsAtFirstFailureNoPartialPaths(t *testing.T) {
	staging := makeStagingTree(t, "z-etcd", "z-kubernetes-cp", "z-kubelet-node1")
	boom := errors.New("mksquashfs exploded")
	runner := &fakeRunner{err: boom, failOn: 2}
	p := confext.NewPackager(confext.WithRunner(runner))

	paths, err := p.BuildRaws(t.Context(), staging, t.TempDir())
	if err == nil {
		t.Fatal("BuildRaws succeeded, want an error")
	}
	if !errors.Is(err, boom) {
		t.Errorf("BuildRaws error %v does not wrap the runner error %v", err, boom)
	}
	if paths != nil {
		t.Errorf("BuildRaws returned partial paths %v on failure, want nil", paths)
	}
	if len(runner.calls) != 2 {
		t.Errorf("mksquashfs called %d times, want 2 (stop at the first failure)", len(runner.calls))
	}
}

// TestBuildRawsNoConfextsReturnsError pins that a staging dir without any
// confext directories is an error: there is nothing to package.
func TestBuildRawsNoConfextsReturnsError(t *testing.T) {
	t.Run("empty staging dir", func(t *testing.T) {
		runner := &fakeRunner{}
		p := confext.NewPackager(confext.WithRunner(runner))

		paths, err := p.BuildRaws(t.Context(), t.TempDir(), t.TempDir())
		if err == nil {
			t.Error("BuildRaws with an empty staging dir succeeded, want an error")
		}
		if paths != nil {
			t.Errorf("BuildRaws returned paths %v, want nil", paths)
		}
		if len(runner.calls) != 0 {
			t.Errorf("mksquashfs called %d times with no confexts, want 0", len(runner.calls))
		}
	})

	t.Run("only stray files", func(t *testing.T) {
		staging := t.TempDir()
		if err := os.WriteFile(filepath.Join(staging, "not-a-tree"), []byte("x"), 0o644); err != nil {
			t.Fatalf("WriteFile stray entry error: %v", err)
		}
		p := confext.NewPackager(confext.WithRunner(&fakeRunner{}))

		if _, err := p.BuildRaws(t.Context(), staging, t.TempDir()); err == nil {
			t.Error("BuildRaws with only stray files succeeded, want an error")
		}
	})

	t.Run("missing staging dir", func(t *testing.T) {
		staging := filepath.Join(t.TempDir(), "does-not-exist")
		p := confext.NewPackager(confext.WithRunner(&fakeRunner{}))

		if _, err := p.BuildRaws(t.Context(), staging, t.TempDir()); err == nil {
			t.Error("BuildRaws with a missing staging dir succeeded, want an error")
		}
	})
}
