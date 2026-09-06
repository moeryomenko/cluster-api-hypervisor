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

// Contract tests for the z-etcd-restore confext tree.
//
// BuildEtcdRestore renders the tree a replacement control-plane Machine
// carries: the captured snapshot staged verbatim under etc/, the restore
// script, the etcd.service drop-in that runs it before etcd starts, and the
// extension-release marker. An empty snapshot is rejected: the tree must
// never render without one, or the drop-in would silently boot an empty
// control plane.

package confexttree_test

import (
	"slices"
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/confexttree"
)

// TestBuildEtcdRestore pins the rendered tree: four entries under the
// z-etcd-restore tree name, the snapshot carried verbatim, a restore script
// that no-ops without a staged snapshot and refuses a populated data dir,
// and a drop-in wiring the script into etcd.service startup.
func TestBuildEtcdRestore(t *testing.T) {
	snapshot := []byte{0x53, 0x51, 0x4c, 0x69, 0x74, 0x65, 0x00, 0x01, 0x02}

	tree, err := confexttree.BuildEtcdRestore(snapshot)
	if err != nil {
		t.Fatalf("BuildEtcdRestore: %v", err)
	}

	if len(tree) != 4 {
		t.Fatalf("BuildEtcdRestore rendered %d entries, want 4", len(tree))
	}

	for path := range tree {
		if !strings.HasPrefix(path, "z-etcd-restore/") {
			t.Errorf("tree path %q is outside the z-etcd-restore tree", path)
		}
	}

	if got := tree["z-etcd-restore/etc/k8slab/etcd-restore/snapshot.db"]; !slices.Equal(got, snapshot) {
		t.Errorf("staged snapshot = %v, want the capture bytes verbatim", got)
	}

	script := string(tree["z-etcd-restore/etc/k8slab/etcd-restore.sh"])
	for _, want := range []string{
		"etcdctl snapshot restore",
		"[ -f \"$SNAP\" ] || exit 0",
		"rmdir \"$DATA_DIR\"",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("restore script misses %q", want)
		}
	}

	dropIn := string(tree["z-etcd-restore/etc/systemd/system/etcd.service.d/10-restore.conf"])
	if !strings.Contains(dropIn, "ExecStartPre=/etc/k8slab/etcd-restore.sh") {
		t.Errorf("drop-in = %q, want an ExecStartPre running the restore script", dropIn)
	}
}

// TestBuildEtcdRestoreEmptySnapshot pins the guard: an empty (or nil)
// snapshot is rejected so the tree never renders without one.
func TestBuildEtcdRestoreEmptySnapshot(t *testing.T) {
	tests := []struct {
		name string
		give []byte
	}{
		{name: "nil snapshot", give: nil},
		{name: "empty snapshot", give: []byte{}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := confexttree.BuildEtcdRestore(tt.give); err == nil {
				t.Error("BuildEtcdRestore: want error, got nil")
			}
		})
	}
}
