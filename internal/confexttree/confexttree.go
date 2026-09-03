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
	clusterName, cpIP, nodeName string,
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
		"z-kubernetes-cp/" + k8sServiceNftPath:                                      renderServiceNftScript("127.0.0.1:6443"),
	}
	maps.Copy(tree, kubeletTree(clusterName, nodeName, pk, kubeletCert, kubeletKey, kubeletKubeconfig))

	return tree, nil
}

// BuildWorker renders the z-kubelet-<node> confext tree for the node named
// nodeName: kubelet.conf (the node's kubeconfig), the cluster CA certificate,
// the node's kubelet certificate and key, and the extension-release metadata.
// Tree map keys are exact slash-separated paths under the confext root; values
// are the passed bytes verbatim, except for the rendered extension-release
// file. An empty node name returns an error.
func BuildWorker(
	clusterName, cpIP, nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey []byte,
	kubeletKubeconfig []byte,
) (map[string][]byte, error) {
	if nodeName == "" {
		return nil, fmt.Errorf("node name must not be empty")
	}

	if cpIP == "" {
		return nil, fmt.Errorf("control-plane IP must not be empty")
	}

	if clusterName == "" {
		return nil, fmt.Errorf("cluster name must not be empty")
	}

	tree := kubeletTree(clusterName, nodeName, pk, kubeletCert, kubeletKey, kubeletKubeconfig)
	tree["z-kubelet-"+nodeName+"/"+k8sServiceNftPath] = renderServiceNftScript(cpIP + ":6443")

	return tree, nil
}

// kubeletTree renders the z-kubelet-<node> tree: the node's kubelet kubeconfig
// at kubelet.conf, the cluster CA certificate, the node's kubelet certificate
// and key, and the extension-release metadata for the tree.
func kubeletTree(
	clusterName, nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey, kubeletKubeconfig []byte,
) map[string][]byte {
	treeName := "z-kubelet-" + nodeName

	return map[string][]byte{
		treeName + "/etc/kubernetes/kubelet.conf":                           kubeletKubeconfig,
		treeName + "/etc/kubernetes/pki/ca.pem":                             pk.CA,
		treeName + "/etc/kubernetes/pki/" + nodeName + ".pem":               kubeletCert,
		treeName + "/etc/kubernetes/pki/" + nodeName + "-key.pem":           kubeletKey,
		treeName + "/etc/kubernetes/provider-id.env":                        renderProviderIDEnv(clusterName, nodeName),
		treeName + "/etc/extension-release.d/extension-release." + treeName: extensionRelease(treeName),
	}
}

// renderProviderIDEnv renders the /etc/kubernetes/provider-id.env file that
// the baked kubelet unit reads via EnvironmentFile and passes to
// --provider-id. The Cluster API machine controller matches a Machine to its
// Node by spec.providerID; without it the NodeHealthy condition never flips
// and the Machine never becomes Ready. An env file (read at unit start)
// avoids systemd drop-in caching that would otherwise leave the flag off on a
// freshly-merged confext.
func renderProviderIDEnv(clusterName, nodeName string) []byte {
	return []byte(fmt.Sprintf("PROVIDER_ID=hypervisor://%s/%s\n", clusterName, nodeName))
}

// k8sServiceNftPath is the path (inside a confext tree) of the script that
// installs the kubernetes Service-IP DNAT for the apiserver. The baked
// k8slab-stack.service (which runs after cloud-final, once the confexts are
// merged into /etc) executes it before starting the Kubernetes services.
const k8sServiceNftPath = "etc/k8s-service-nft.sh"

// renderServiceNftScript renders a POSIX script that DNATs the kubernetes
// Service clusterIP apiserver endpoint (10.96.0.1:443) to the workload
// apiserver. On the control plane target is 127.0.0.1:6443 (its own
// apiserver); on a worker it is <cpIP>:6443. Cilium's config-init runs in the
// host network namespace and dials the apiserver through the in-cluster
// KUBERNETES_SERVICE_HOST (10.96.0.1) before its own datapath exists, so the
// Service-IP route must be in place before kubelet schedules any pod. Unlike
// the earlier /12 dev lo route (which conflicted with Cilium's BPF Service
// load-balancer), this narrow rule DNATs only the apiserver endpoint and is
// safe alongside Cilium's kube-proxy-replacement.
func renderServiceNftScript(target string) []byte {
	return []byte(fmt.Sprintf(`#!/bin/sh
# Install the kubernetes Service-IP DNAT for the apiserver: route
# 10.96.0.1:443 to the workload apiserver at %s. Runs from k8slab-stack
# before the Kubernetes services start. Narrow: only the apiserver endpoint,
# so Cilium's BPF Service load-balancer handles every other Service IP.
set -e
nft add table ip k8sfix 2>/dev/null || true
nft add chain ip k8sfix output "{ type nat hook output priority 0; }" 2>/dev/null || true
nft add rule ip k8sfix output ip daddr 10.96.0.1 tcp dport 443 dnat to %s 2>/dev/null || true
`, target, target))
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
