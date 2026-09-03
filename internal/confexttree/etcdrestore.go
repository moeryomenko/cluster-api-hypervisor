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

package confexttree

import "fmt"

// etcdRestoreTreeName is the confext tree name of the etcd snapshot restore
// extension a replacement control-plane Machine carries.
const etcdRestoreTreeName = "z-etcd-restore"

// etcdRestoreScriptPath is the path (inside the confext tree) of the restore
// script. Everything a confext carries merges under /etc, so both the script
// and the staged snapshot live below etc/ (the k8s-service-nft.sh precedent).
const etcdRestoreScriptPath = "etc/k8slab/etcd-restore.sh"

// etcdRestoreSnapshotPath is the path (inside the confext tree) the captured
// etcd snapshot is staged at on the node.
const etcdRestoreSnapshotPath = "etc/k8slab/etcd-restore/snapshot.db"

// etcdRestoreDropInPath is the path (inside the confext tree) of the
// etcd.service drop-in that runs the restore script before etcd starts.
const etcdRestoreDropInPath = "etc/systemd/system/etcd.service.d/10-restore.conf"

// etcdRestoreScriptTemplate is the restore script an ExecStartPre of
// etcd.service runs: a machine without a staged snapshot or with an existing
// (non-empty) etcd data dir is a no-op, so only a fresh replacement Machine
// with a staged snapshot restores anything. etcdctl snapshot restore requires
// the target data dir to not exist; a fresh Machine has none, and an empty
// /var/lib/etcd left behind by a half-started etcd is removed with rmdir,
// which never deletes a populated dir.
const etcdRestoreScriptTemplate = `#!/bin/sh
# Restore the etcd snapshot captured before this control-plane Machine was
# replaced. Runs from the etcd.service ExecStartPre drop-in: exit 0 without
# a staged snapshot, and refuse to touch a populated /var/lib/etcd.
set -e
SNAP=/etc/k8slab/etcd-restore/snapshot.db
DATA_DIR=/var/lib/etcd
[ -f "$SNAP" ] || exit 0
if [ -d "$DATA_DIR" ]; then
    rmdir "$DATA_DIR" 2>/dev/null || exit 0
fi
etcdctl snapshot restore --data-dir "$DATA_DIR" "$SNAP"
`

// etcdRestoreDropIn is the etcd.service drop-in content: the restore script
// runs before etcd's own ExecStart, in the same cgroup and environment.
const etcdRestoreDropIn = `[Service]
ExecStartPre=/etc/k8slab/etcd-restore.sh
`

// BuildEtcdRestore renders the z-etcd-restore confext tree for a replacement
// control-plane Machine: the captured etcd snapshot staged under etc/, the
// restore script, and the etcd.service drop-in that runs it before etcd
// starts. Tree map keys are exact slash-separated paths under the confext
// root; the snapshot bytes are carried verbatim. Empty snapshot bytes are
// rejected: the tree must never render without a snapshot, or the drop-in
// would silently boot an empty control plane.
func BuildEtcdRestore(snapshot []byte) (map[string][]byte, error) {
	if len(snapshot) == 0 {
		return nil, fmt.Errorf("etcd restore snapshot must not be empty")
	}

	return map[string][]byte{
		etcdRestoreTreeName + "/" + etcdRestoreSnapshotPath: snapshot,
		etcdRestoreTreeName + "/" + etcdRestoreScriptPath: []byte(
			etcdRestoreScriptTemplate,
		),
		etcdRestoreTreeName + "/" + etcdRestoreDropInPath: []byte(etcdRestoreDropIn),
		etcdRestoreTreeName + "/etc/extension-release.d/extension-release." + etcdRestoreTreeName: extensionRelease(
			etcdRestoreTreeName,
		),
	}, nil
}
