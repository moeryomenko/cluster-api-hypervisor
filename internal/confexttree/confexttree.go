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

// Package confexttree renders the role-split confext source trees that the
// bootstrap provider produces for a Machine and the infrastructure provider
// packages into the confext data disk. The trees mirror the phase-B confext
// layout: a control-plane node receives three trees (z-etcd, z-kubernetes-cp,
// z-kubelet-<node>) and a worker receives one (z-kubelet-<node>). The builder
// is a pure renderer: every value in the returned map is either a passed byte
// slice copied verbatim or a fixed text file rendered from the inputs; it
// never generates certificates or keys.
package confexttree

import (
	"fmt"
	"maps"
	"strings"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// etcdConfTemplate is the single-node etcd configuration with the control-plane
// IP and node name interpolated, mirroring the phase-B tree layout (TLS
// disabled for the isolated lab network).
const etcdConfTemplate = `name: '{{NODE_NAME}}'
data-dir: '/var/lib/etcd'
listen-peer-urls: 'http://{{CP_IP}}:2380'
listen-client-urls: 'http://{{CP_IP}}:2379,http://127.0.0.1:2379'
advertise-client-urls: 'http://{{CP_IP}}:2379'
initial-advertise-peer-urls: 'http://{{CP_IP}}:2380'
initial-cluster: '{{NODE_NAME}}=http://{{CP_IP}}:2380'
initial-cluster-token: 'etcd-cluster'
initial-cluster-state: 'new'
# TLS disabled for lab (single-node, isolated network)
#cert-file: '/etc/kubernetes/pki/etcd.pem'
#key-file: '/etc/kubernetes/pki/etcd-key.pem'
client-cert-auth: false
#trusted-ca-file: '/etc/kubernetes/pki/ca.pem'
peer-client-cert-auth: false
`

// cpEnvTemplate is the apiserver environment file with the control-plane IP
// interpolated, mirroring the phase-B tree layout.
const cpEnvTemplate = `KUBE_ADVERTISE_ADDRESS={{CP_IP}}
KUBE_ETCD_SERVERS=http://{{CP_IP}}:2379
`

// BuildControlPlane renders the full control-plane confext tree set for the
// node named nodeName with the static IP cpIP: the z-etcd tree (etcd.conf.yml
// carrying the control-plane IP), the z-kubernetes-cp tree (cp.env carrying
// the IP, the cluster PKI certificate and key material, the three kubeconfigs,
// and the encryption configuration), and the z-kubelet-<node> tree for the
// control-plane node itself (its kubeconfig plus its kubelet certificate and
// key). Tree map keys are exact slash-separated paths under the confext root;
// values are the passed bytes verbatim, except for the rendered etcd.conf.yml,
// cp.env, and extension-release files. An empty control-plane IP or node name
// returns an error.
func BuildControlPlane(
	cpIP, nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey []byte,
	kubeletKubeconfig []byte,
	adminKubeconfig, controllerManagerKubeconfig, schedulerKubeconfig []byte,
	encryptionConfig []byte,
) (map[string][]byte, error) {
	if cpIP == "" {
		return nil, fmt.Errorf("control-plane IP must not be empty")
	}
	if nodeName == "" {
		return nil, fmt.Errorf("node name must not be empty")
	}

	tree := map[string][]byte{
		"z-etcd/etc/etcd/etcd.conf.yml":                           renderEtcdConf(cpIP, nodeName),
		"z-etcd/etc/extension-release.d/extension-release.z-etcd": extensionRelease("z-etcd"),

		"z-kubernetes-cp/etc/kubernetes/cp.env":                                     renderCPEnv(cpIP),
		"z-kubernetes-cp/etc/kubernetes/pki/ca.pem":                                 pk.CA,
		"z-kubernetes-cp/etc/kubernetes/pki/ca-key.pem":                             pk.CAKey,
		"z-kubernetes-cp/etc/kubernetes/pki/kubernetes.pem":                         pk.APIServer,
		"z-kubernetes-cp/etc/kubernetes/pki/kubernetes-key.pem":                     pk.APIServerKey,
		"z-kubernetes-cp/etc/kubernetes/pki/front-proxy-ca.pem":                     pk.FrontProxyCA,
		"z-kubernetes-cp/etc/kubernetes/pki/front-proxy-client.pem":                 pk.FrontProxy,
		"z-kubernetes-cp/etc/kubernetes/pki/front-proxy-client-key.pem":             pk.FrontProxyKey,
		"z-kubernetes-cp/etc/kubernetes/pki/service-account.pem":                    pk.ServiceAccount,
		"z-kubernetes-cp/etc/kubernetes/pki/service-account-key.pem":                pk.ServiceAccountKey,
		"z-kubernetes-cp/etc/kubernetes/admin.kubeconfig":                           adminKubeconfig,
		"z-kubernetes-cp/etc/kubernetes/controller-manager.kubeconfig":              controllerManagerKubeconfig,
		"z-kubernetes-cp/etc/kubernetes/scheduler.kubeconfig":                       schedulerKubeconfig,
		"z-kubernetes-cp/etc/kubernetes/encryption-config.yaml":                     encryptionConfig,
		"z-kubernetes-cp/etc/extension-release.d/extension-release.z-kubernetes-cp": extensionRelease("z-kubernetes-cp"),
	}
	maps.Copy(tree, kubeletTree(nodeName, pk, kubeletCert, kubeletKey, kubeletKubeconfig))

	return tree, nil
}

// BuildWorker renders the z-kubelet-<node> confext tree for the node named
// nodeName: kubelet.conf (the node's kubeconfig), the cluster CA certificate,
// the node's kubelet certificate and key, and the extension-release metadata.
// Tree map keys are exact slash-separated paths under the confext root; values
// are the passed bytes verbatim, except for the rendered extension-release
// file. An empty node name returns an error.
func BuildWorker(
	nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey []byte,
	kubeletKubeconfig []byte,
) (map[string][]byte, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("node name must not be empty")
	}

	return kubeletTree(nodeName, pk, kubeletCert, kubeletKey, kubeletKubeconfig), nil
}

// kubeletTree renders the z-kubelet-<node> tree: the node's kubelet kubeconfig
// at kubelet.conf, the cluster CA certificate, the node's kubelet certificate
// and key, and the extension-release metadata for the tree.
func kubeletTree(
	nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey, kubeletKubeconfig []byte,
) map[string][]byte {
	treeName := "z-kubelet-" + nodeName
	return map[string][]byte{
		treeName + "/etc/kubernetes/kubelet.conf":                           kubeletKubeconfig,
		treeName + "/etc/kubernetes/pki/ca.pem":                             pk.CA,
		treeName + "/etc/kubernetes/pki/" + nodeName + ".pem":               kubeletCert,
		treeName + "/etc/kubernetes/pki/" + nodeName + "-key.pem":           kubeletKey,
		treeName + "/etc/extension-release.d/extension-release." + treeName: extensionRelease(treeName),
	}
}

// renderEtcdConf renders the etcd configuration with the control-plane IP and
// node name interpolated.
func renderEtcdConf(cpIP, nodeName string) []byte {
	replacer := strings.NewReplacer("{{CP_IP}}", cpIP, "{{NODE_NAME}}", nodeName)
	return []byte(replacer.Replace(etcdConfTemplate))
}

// renderCPEnv renders the apiserver environment file with the control-plane IP
// interpolated.
func renderCPEnv(cpIP string) []byte {
	return []byte(strings.ReplaceAll(cpEnvTemplate, "{{CP_IP}}", cpIP))
}

// extensionRelease renders the systemd-confext release metadata for the tree
// named treeName: exactly
// "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=<treeName>\n".
func extensionRelease(treeName string) []byte {
	return []byte("ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=" + treeName + "\n")
}
