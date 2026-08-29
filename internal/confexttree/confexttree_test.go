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

// The role-split confext tree builder contract (test-first).
//
// This suite pins the rendered role-split confext tree that the bootstrap
// provider produces for a Machine and that the infrastructure provider
// packages into the confext data disk. The tree mirrors the phase-B confext
// layout: a control-plane node receives three trees (z-etcd,
// z-kubernetes-cp, z-kubelet-<node>) and a worker receives one
// (z-kubelet-<node>). Everything is hand-rolled: no kubeadm anywhere.
//
// The contract, in prose:
//
//   - BuildControlPlane(cpIP, nodeName string, pk pki.ClusterPKI,
//     kubeletCert, kubeletKey, kubeletKubeconfig, adminKubeconfig,
//     controllerManagerKubeconfig, schedulerKubeconfig, encryptionConfig
//     []byte) (map[string][]byte, error) renders the full control-plane tree
//     set: the z-etcd tree (etcd.conf.yml carrying the control-plane IP), the
//     z-kubernetes-cp tree (cp.env carrying the IP, the cluster PKI
//     certificate and key material, the three kubeconfigs, and the encryption
//     configuration), and the z-kubelet-<node> tree for the control-plane node
//     itself (its kubeconfig plus its kubelet certificate and key). Tree map
//     keys are exact slash-separated paths under the confext root; values are
//     the passed bytes verbatim.
//   - BuildWorker(nodeName string, pk pki.ClusterPKI, kubeletCert,
//     kubeletKey, kubeletKubeconfig []byte) (map[string][]byte, error)
//     renders the z-kubelet-<node> tree only, with the same layout.
//   - Every tree carries etc/extension-release.d/extension-release.z-<tree>
//     whose content is exactly
//     "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-<tree>\n".
//   - The builder is a pure renderer: it copies the kubelet certificate and
//     key and the kubeconfig bytes into the tree and never generates
//     certificates or keys itself.
//   - An empty control-plane IP and an empty node name are rejected with an
//     error.
package confexttree_test

import (
	"bytes"
	"strings"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/confexttree"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// Compile-time pins: the exported functions must exist with exactly these
// names and signatures.
var (
	_ func(string, string, string, pki.ClusterPKI, []byte, []byte, []byte, []byte, []byte, []byte, []byte) (map[string][]byte, error) = confexttree.BuildControlPlane
	_ func(string, string, string, pki.ClusterPKI, []byte, []byte, []byte) (map[string][]byte, error)                                 = confexttree.BuildWorker
)

const (
	testCPIP    = "192.168.124.10"
	testCluster = "cl1"
	testCPName  = "cp1"
	testWorker  = "worker1"
)

var (
	testKubeletKubeconf = []byte("kubelet kubeconfig bytes")
	testAdminKubeconfig = []byte("admin kubeconfig bytes")
	testCMKubeconfig    = []byte("controller-manager kubeconfig bytes")
	testSchedKubeconfig = []byte("scheduler kubeconfig bytes")
	testEncryptionCfg   = []byte("encryption-config bytes")
)

// mustPKI generates the cluster PKI for the test, failing on any error.
func mustPKI(t *testing.T, cpIP, cpName string) pki.ClusterPKI {
	t.Helper()

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI(%q, %q) error: %v", cpIP, cpName, err)
	}

	return pk
}

// mustKubeletCert generates one node's kubelet certificate and key, failing
// on any error.
func mustKubeletCert(t *testing.T, pk pki.ClusterPKI, nodeName string) ([]byte, []byte) {
	t.Helper()

	cert, key, err := pki.GenerateKubeletCert(pk, nodeName)
	if err != nil {
		t.Fatalf("GenerateKubeletCert(%q) error: %v", nodeName, err)
	}

	return cert, key
}

// keysOf returns the tree keys in map iteration order.
func keysOf(tree map[string][]byte) []string {
	keys := make([]string, 0, len(tree))
	for key := range tree {
		keys = append(keys, key)
	}

	return keys
}

// mustTreeValue returns the content of one tree key, failing when the key is
// missing.
func mustTreeValue(t *testing.T, tree map[string][]byte, key string) []byte {
	t.Helper()

	content, ok := tree[key]
	if !ok {
		t.Fatalf("tree is missing key %q (tree keys: %v)", key, keysOf(tree))
	}

	return content
}

// assertTreeBytes fails when the tree content for key differs from want.
func assertTreeBytes(t *testing.T, tree map[string][]byte, key string, want []byte) {
	t.Helper()

	got := mustTreeValue(t, tree, key)
	if !bytes.Equal(got, want) {
		t.Errorf("tree[%q] differs from the expected bytes", key)
	}
}

// assertExactKeySet fails when tree does not contain exactly the given keys.
func assertExactKeySet(t *testing.T, tree map[string][]byte, want ...string) {
	t.Helper()

	if len(tree) != len(want) {
		t.Fatalf("tree has %d keys, want %d (tree keys: %v)", len(tree), len(want), keysOf(tree))
	}
	for _, key := range want {
		if _, ok := tree[key]; !ok {
			t.Errorf("tree is missing key %q (tree keys: %v)", key, keysOf(tree))
		}
	}
}

// TestBuildControlPlaneFullKeySet pins the exact control-plane tree: the
// z-etcd and z-kubernetes-cp trees plus the control-plane node's own
// z-kubelet-<node> tree, and no other keys.
func TestBuildControlPlaneFullKeySet(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	kubeletCert, kubeletKey := mustKubeletCert(t, pk, testCPName)

	tree, err := confexttree.BuildControlPlane(
		testCluster, testCPIP, testCPName, pk,
		kubeletCert, kubeletKey, testKubeletKubeconf,
		testAdminKubeconfig, testCMKubeconfig, testSchedKubeconfig, testEncryptionCfg,
	)
	if err != nil {
		t.Fatalf("BuildControlPlane error: %v", err)
	}

	assertExactKeySet(t, tree,
		"z-etcd/etc/etcd/etcd.conf.yml",
		"z-etcd/etc/extension-release.d/extension-release.z-etcd",
		"z-kubernetes-cp/etc/kubernetes/cp.env",
		"z-kubernetes-cp/etc/kubernetes/pki/ca.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/ca-key.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/kubernetes.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/kubernetes-key.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/front-proxy-ca.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/front-proxy-client.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/front-proxy-client-key.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/service-account.pem",
		"z-kubernetes-cp/etc/kubernetes/pki/service-account-key.pem",
		"z-kubernetes-cp/etc/kubernetes/admin.kubeconfig",
		"z-kubernetes-cp/etc/kubernetes/controller-manager.kubeconfig",
		"z-kubernetes-cp/etc/kubernetes/scheduler.kubeconfig",
		"z-kubernetes-cp/etc/kubernetes/encryption-config.yaml",
		"z-kubernetes-cp/etc/extension-release.d/extension-release.z-kubernetes-cp",
		"z-kubernetes-cp/etc/k8s-service-nft.sh",
		"z-kubelet-cp1/etc/kubernetes/kubelet.conf",
		"z-kubelet-cp1/etc/kubernetes/provider-id.env",
		"z-kubelet-cp1/etc/kubernetes/pki/ca.pem",
		"z-kubelet-cp1/etc/kubernetes/pki/cp1.pem",
		"z-kubelet-cp1/etc/kubernetes/pki/cp1-key.pem",
		"z-kubelet-cp1/etc/extension-release.d/extension-release.z-kubelet-cp1",
	)
}

// TestBuildControlPlanePKIValues pins the certificate and key material: every
// z-kubernetes-cp pki file equals the corresponding ClusterPKI field, and the
// control-plane node's kubelet files equal the passed kubelet certificate and
// key.
func TestBuildControlPlanePKIValues(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	kubeletCert, kubeletKey := mustKubeletCert(t, pk, testCPName)

	tree, err := confexttree.BuildControlPlane(
		testCluster, testCPIP, testCPName, pk,
		kubeletCert, kubeletKey, testKubeletKubeconf,
		testAdminKubeconfig, testCMKubeconfig, testSchedKubeconfig, testEncryptionCfg,
	)
	if err != nil {
		t.Fatalf("BuildControlPlane error: %v", err)
	}

	t.Run("cluster CA certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/ca.pem", pk.CA)
	})
	t.Run("cluster CA key", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/ca-key.pem", pk.CAKey)
	})
	t.Run("apiserver certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/kubernetes.pem", pk.APIServer)
	})
	t.Run("apiserver key", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/kubernetes-key.pem", pk.APIServerKey)
	})
	t.Run("front-proxy CA certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/front-proxy-ca.pem", pk.FrontProxyCA)
	})
	t.Run("front-proxy client certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/front-proxy-client.pem", pk.FrontProxy)
	})
	t.Run("front-proxy client key", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/front-proxy-client-key.pem", pk.FrontProxyKey)
	})
	t.Run("service-account certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/service-account.pem", pk.ServiceAccount)
	})
	t.Run("service-account key", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/pki/service-account-key.pem", pk.ServiceAccountKey)
	})
	t.Run("control-plane kubelet certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-cp1/etc/kubernetes/pki/cp1.pem", kubeletCert)
	})
	t.Run("control-plane kubelet key", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-cp1/etc/kubernetes/pki/cp1-key.pem", kubeletKey)
	})
}

// TestBuildControlPlaneConfigAndKubeconfigValues pins the rendered text
// material: etcd.conf.yml and cp.env carry the control-plane IP, and every
// kubeconfig and the encryption configuration equal the passed bytes
// verbatim.
func TestBuildControlPlaneConfigAndKubeconfigValues(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	kubeletCert, kubeletKey := mustKubeletCert(t, pk, testCPName)

	tree, err := confexttree.BuildControlPlane(
		testCluster, testCPIP, testCPName, pk,
		kubeletCert, kubeletKey, testKubeletKubeconf,
		testAdminKubeconfig, testCMKubeconfig, testSchedKubeconfig, testEncryptionCfg,
	)
	if err != nil {
		t.Fatalf("BuildControlPlane error: %v", err)
	}

	t.Run("etcd config carries the control-plane IP", func(t *testing.T) {
		content := mustTreeValue(t, tree, "z-etcd/etc/etcd/etcd.conf.yml")
		if !bytes.Contains(content, []byte(testCPIP)) {
			t.Errorf("etcd.conf.yml does not contain the control-plane IP %q", testCPIP)
		}
	})
	t.Run("cp.env carries the control-plane IP", func(t *testing.T) {
		content := mustTreeValue(t, tree, "z-kubernetes-cp/etc/kubernetes/cp.env")
		if !bytes.Contains(content, []byte(testCPIP)) {
			t.Errorf("cp.env does not contain the control-plane IP %q", testCPIP)
		}
	})
	t.Run("admin kubeconfig", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/admin.kubeconfig", testAdminKubeconfig)
	})
	t.Run("controller-manager kubeconfig", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/controller-manager.kubeconfig", testCMKubeconfig)
	})
	t.Run("scheduler kubeconfig", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/scheduler.kubeconfig", testSchedKubeconfig)
	})
	t.Run("encryption configuration", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubernetes-cp/etc/kubernetes/encryption-config.yaml", testEncryptionCfg)
	})
	t.Run("control-plane kubelet kubeconfig", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-cp1/etc/kubernetes/kubelet.conf", testKubeletKubeconf)
	})
}

// TestExtensionReleaseMetadata pins the systemd-confext release metadata of
// every tree: exactly
// "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-<tree>\n".
func TestExtensionReleaseMetadata(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	cpKubeletCert, cpKubeletKey := mustKubeletCert(t, pk, testCPName)
	workerKubeletCert, workerKubeletKey := mustKubeletCert(t, pk, testWorker)

	cpTree, err := confexttree.BuildControlPlane(
		testCluster, testCPIP, testCPName, pk,
		cpKubeletCert, cpKubeletKey, testKubeletKubeconf,
		testAdminKubeconfig, testCMKubeconfig, testSchedKubeconfig, testEncryptionCfg,
	)
	if err != nil {
		t.Fatalf("BuildControlPlane error: %v", err)
	}
	workerTree, err := confexttree.BuildWorker(
		testCluster,
		testCPIP,
		testWorker,
		pk,
		workerKubeletCert,
		workerKubeletKey,
		testKubeletKubeconf,
	)
	if err != nil {
		t.Fatalf("BuildWorker error: %v", err)
	}

	metadata := []struct {
		name string
		tree map[string][]byte
		key  string
		want string
	}{
		{
			name: "z-etcd",
			tree: cpTree,
			key:  "z-etcd/etc/extension-release.d/extension-release.z-etcd",
			want: "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-etcd\n",
		},
		{
			name: "z-kubernetes-cp",
			tree: cpTree,
			key:  "z-kubernetes-cp/etc/extension-release.d/extension-release.z-kubernetes-cp",
			want: "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-kubernetes-cp\n",
		},
		{
			name: "z-kubelet-cp1",
			tree: cpTree,
			key:  "z-kubelet-cp1/etc/extension-release.d/extension-release.z-kubelet-cp1",
			want: "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-kubelet-cp1\n",
		},
		{
			name: "z-kubelet-worker1",
			tree: workerTree,
			key:  "z-kubelet-worker1/etc/extension-release.d/extension-release.z-kubelet-worker1",
			want: "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-kubelet-worker1\n",
		},
	}
	for _, tt := range metadata {
		t.Run(tt.name, func(t *testing.T) {
			content := mustTreeValue(t, tt.tree, tt.key)
			if !bytes.Equal(content, []byte(tt.want)) {
				t.Errorf("extension-release content = %q, want %q", content, tt.want)
			}
		})
	}
}

// TestBuildWorkerKeySetAndValues pins the worker tree: exactly the
// z-kubelet-<node> keys (kubelet.conf, the cluster CA, the node's kubelet
// certificate and key, and the extension-release metadata), with the
// kubeconfig and material equal to the passed bytes and no control-plane
// trees.
func TestBuildWorkerKeySetAndValues(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	kubeletCert, kubeletKey := mustKubeletCert(t, pk, testWorker)

	tree, err := confexttree.BuildWorker(
		testCluster,
		testCPIP,
		testWorker,
		pk,
		kubeletCert,
		kubeletKey,
		testKubeletKubeconf,
	)
	if err != nil {
		t.Fatalf("BuildWorker error: %v", err)
	}

	assertExactKeySet(t, tree,
		"z-kubelet-worker1/etc/kubernetes/kubelet.conf",
		"z-kubelet-worker1/etc/kubernetes/provider-id.env",
		"z-kubelet-worker1/etc/kubernetes/pki/ca.pem",
		"z-kubelet-worker1/etc/kubernetes/pki/worker1.pem",
		"z-kubelet-worker1/etc/kubernetes/pki/worker1-key.pem",
		"z-kubelet-worker1/etc/extension-release.d/extension-release.z-kubelet-worker1",
		"z-kubelet-worker1/etc/k8s-service-nft.sh",
	)

	t.Run("kubelet kubeconfig", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-worker1/etc/kubernetes/kubelet.conf", testKubeletKubeconf)
	})
	t.Run("cluster CA certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-worker1/etc/kubernetes/pki/ca.pem", pk.CA)
	})
	t.Run("kubelet certificate", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-worker1/etc/kubernetes/pki/worker1.pem", kubeletCert)
	})
	t.Run("kubelet key", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-worker1/etc/kubernetes/pki/worker1-key.pem", kubeletKey)
	})
	t.Run("extension-release metadata", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-worker1/etc/extension-release.d/extension-release.z-kubelet-worker1",
			[]byte("ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-kubelet-worker1\n"))
	})
	t.Run("provider-id env", func(t *testing.T) {
		assertTreeBytes(t, tree, "z-kubelet-worker1/etc/kubernetes/provider-id.env",
			[]byte("PROVIDER_ID=hypervisor://"+testCluster+"/"+testWorker+"\n"))
	})
	t.Run("apiserver nft DNAT targets the control-plane IP", func(t *testing.T) {
		raw := tree["z-kubelet-worker1/etc/k8s-service-nft.sh"]
		script := string(raw)
		want := "dnat to " + testCPIP + ":6443"
		if !strings.Contains(script, want) {
			t.Errorf("worker nft script lacks %q: %q", want, script)
		}
		if strings.Contains(script, "10.96.0.0/12 dev lo") {
			t.Errorf("worker nft script must not route the whole Service CIDR to lo (Cilium BPF LB conflict): %q", script)
		}
	})
}

// TestBuildControlPlaneRejectsEmptyInputs pins input validation: an empty
// control-plane IP and an empty node name are rejected.
func TestBuildControlPlaneRejectsEmptyInputs(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	kubeletCert, kubeletKey := mustKubeletCert(t, pk, testCPName)

	tests := []struct {
		name     string
		cpIP     string
		nodeName string
	}{
		{name: "empty control-plane IP", cpIP: "", nodeName: testCPName},
		{name: "empty node name", cpIP: testCPIP, nodeName: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := confexttree.BuildControlPlane(
				testCluster, tt.cpIP, tt.nodeName, pk,
				kubeletCert, kubeletKey, testKubeletKubeconf,
				testAdminKubeconfig, testCMKubeconfig, testSchedKubeconfig, testEncryptionCfg,
			)
			if err == nil {
				t.Errorf("BuildControlPlane(%q, %q, ...) succeeded, want an error", tt.cpIP, tt.nodeName)
			}
		})
	}
}

// TestBuildWorkerRejectsEmptyNodeName pins input validation: an empty node
// name is rejected.
func TestBuildWorkerRejectsEmptyNodeName(t *testing.T) {
	pk := mustPKI(t, testCPIP, testCPName)
	kubeletCert, kubeletKey := mustKubeletCert(t, pk, testWorker)

	if _, err := confexttree.BuildWorker(testCluster, "", "", pk, kubeletCert, kubeletKey, testKubeletKubeconf); err == nil {
		t.Error("BuildWorker with an empty node name succeeded, want an error")
	}
}
