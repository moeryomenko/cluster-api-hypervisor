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

// HypervisorConfig controller contract (test-first, red).
//
// This suite pins the contract for the bootstrap reconciler that renders the
// role-split confext tree for one Machine into a Secret the infrastructure
// provider packages into the confext data disk. The reconciler is exercised
// through the committed envtest harness with recording seams standing in for
// the PKI generation and kubeconfig rendering, so no real RSA key is ever
// generated and the rendered material is deterministic.
//
// The contract, in prose:
//
//   - HypervisorConfigReconciler carries the controller-runtime wiring
//     (embedded client.Client, Scheme, Recorder) plus the injectable
//     dependencies: BuildTree (the role-split confext tree builder),
//     GenerateClusterPKI (the cluster-scoped PKI generator), GenerateKubeletCert
//     (the per-machine kubelet certificate generator), and RenderKubeconfig
//     (the kubeconfig renderer). The tests build every dependency over a
//     recording seam and hand the fully constructed reconciler to the
//     controller. The tree builder seam delegates to the real confexttree
//     builders, so the rendered key sets are genuine; the PKI and kubeconfig
//     seams return deterministic canned material.
//   - Reconcile resolves the object, then the owning CAPI Machine (owner
//     reference), then the linked CAPI Cluster (machine.spec.clusterName). A
//     missing object is a no-op; a config with no owning Machine and a config
//     whose Machine's Cluster does not exist are failures, not no-ops:
//     status.failureReason and status.failureMessage are set and the config
//     is not ready, and no Secret is written.
//   - Role detection: with an empty spec.role the role comes from the owning
//     Machine's labels — the standard control-plane label means
//     "control-plane", its absence means "worker"; an explicit spec.role
//     overrides the labels. spec.nodeName defaults to the Machine name.
//   - Control-plane IP: the apiserver address the kubeconfigs and the
//     apiserver certificate SAN use is the linked HypervisorMachine's
//     status.addresses InternalIP for a control-plane node, and the linked
//     HypervisorCluster's status.controlPlaneEndpoint.host for a worker node.
//     A missing address is a failure, not a requeue.
//   - Cluster PKI: the controller generates the cluster-scoped PKI through
//     the injected generator on the first reconcile of the cluster and
//     persists it in the conventional Secret <cluster>-pki whose data keys
//     are exactly the pki.ClusterPKI exported field names; later reconciles
//     read the stored Secret and never regenerate. The stored material feeds
//     the per-machine kubelet certificate generator.
//   - Secret rendering: the controller renders the role-split tree through
//     the injected builder, encodes it as the tree.json blob (a JSON object
//     mapping every tree path to its base64-encoded content — Secret data
//     keys cannot contain "/"), and writes the conventional Secret
//     <config>-data; status.dataSecretName names it and the config is marked
//     ready with the DataSecretAvailable condition true.
//   - Kubeconfigs: the controller renders admin/controller-manager/scheduler
//     kubeconfigs for a control-plane node and kubelet.conf for every node
//     through the injected renderer with the server URL https://<cp-ip>:6443
//     and the KTHW user names.
//   - Idempotency: a second reconcile with unchanged inputs neither creates a
//     new data Secret nor regenerates the cluster PKI — same
//     status.dataSecretName, same Secret set, same tree.json bytes.
//   - Failures: a failing tree build or cluster PKI generation surfaces as a
//     reconcile error that preserves the underlying error, sets
//     status.failureReason/failureMessage, and leaves the config not ready.
package controllers

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confexttree"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// Fixture constants shared by the tests in this file. The IP and port
// constants come from the cluster controller suite's shared fixtures.
const (
	// testConfigRoleControlPlane and testConfigRoleWorker are the exact role
	// strings the spec defines and the controller must branch on.
	testConfigRoleControlPlane = "control-plane"
	testConfigRoleWorker       = "worker"

	// configDataSecretAvailableCondition is the condition type the spec
	// requires once the bootstrap Secret exists.
	configDataSecretAvailableCondition = clusterv1.ConditionType("DataSecretAvailable")

	// testFixtureKubeletCert and testFixtureKubeletKey are the deterministic
	// per-machine kubelet certificate and key the injected generator returns.
	testFixtureKubeletCert = "fixture kubelet cert"
	testFixtureKubeletKey  = "fixture kubelet key"
)

// The reconciler's injectable dependency shapes. The tests build each one
// over a recording seam; the fixture's composite literal pins the exact
// fields of HypervisorConfigReconciler.
type (
	// buildTreeFunc renders the role-split confext tree for one node. The
	// kubeconfigs map holds the rendered kubeconfig documents keyed by role
	// ("admin", "controller-manager", "scheduler", "kubelet"); the
	// encryptionConfig is the control-plane encryption configuration.
	buildTreeFunc func(role, cpIP, nodeName string, pk pki.ClusterPKI, kubeletCert, kubeletKey []byte, kubeconfigs map[string][]byte, encryptionConfig []byte) (map[string][]byte, error)
	// generateClusterPKIFunc generates the cluster-scoped PKI for one
	// cluster; cpIP and cpName are the apiserver certificate SAN inputs.
	generateClusterPKIFunc func(cpIP, cpName string) (pki.ClusterPKI, error)
	// generateKubeletCertFunc generates one node's kubelet certificate and
	// key signed by the cluster PKI.
	generateKubeletCertFunc func(pk pki.ClusterPKI, nodeName string) (certPEM, keyPEM []byte, err error)
	// renderKubeconfigFunc renders a kubeconfig document from PEM material
	// with the given server URL and user name.
	renderKubeconfigFunc func(caPEM []byte, serverURL, user string, clientCert, clientKey []byte) ([]byte, error)
)

// Compile-time pin for the reconciler shape the implementation must expose:
// the Reconcile signature. Until the reconciler type exists the package does
// not compile — that is the intended red phase.
var _ func(context.Context, ctrl.Request) (ctrl.Result, error) = (*HypervisorConfigReconciler)(nil).Reconcile

// fixtureClusterPKI returns the deterministic cluster PKI the injected
// generator hands back: fixed bytes the tree builder copies verbatim. No
// certificate is ever generated, so the suite stays fast and deterministic.
func fixtureClusterPKI() pki.ClusterPKI {
	return pki.ClusterPKI{
		CA:                []byte("fixture ca cert"),
		CAKey:             []byte("fixture ca key"),
		FrontProxyCA:      []byte("fixture front-proxy ca cert"),
		FrontProxyCAKey:   []byte("fixture front-proxy ca key"),
		APIServer:         []byte("fixture apiserver cert"),
		APIServerKey:      []byte("fixture apiserver key"),
		FrontProxy:        []byte("fixture front-proxy client cert"),
		FrontProxyKey:     []byte("fixture front-proxy client key"),
		ServiceAccount:    []byte("fixture service-account cert"),
		ServiceAccountKey: []byte("fixture service-account key"),
	}
}

// fixturePKISecretData maps the pki.ClusterPKI exported fields to the data
// keys the conventional cluster PKI Secret must carry.
func fixturePKISecretData() map[string][]byte {
	pk := fixtureClusterPKI()
	return map[string][]byte{
		"CA":                pk.CA,
		"CAKey":             pk.CAKey,
		"FrontProxyCA":      pk.FrontProxyCA,
		"FrontProxyCAKey":   pk.FrontProxyCAKey,
		"APIServer":         pk.APIServer,
		"APIServerKey":      pk.APIServerKey,
		"FrontProxy":        pk.FrontProxy,
		"FrontProxyKey":     pk.FrontProxyKey,
		"ServiceAccount":    pk.ServiceAccount,
		"ServiceAccountKey": pk.ServiceAccountKey,
	}
}

// buildTreeCall captures one invocation of the tree builder seam.
type buildTreeCall struct {
	role             string
	cpIP             string
	nodeName         string
	pk               pki.ClusterPKI
	kubeletCert      []byte
	kubeletKey       []byte
	kubeconfigs      map[string][]byte
	encryptionConfig []byte
}

// recordingBuildTree records every invocation and, when no error is injected,
// delegates to the real confexttree builders so the rendered tree is genuine:
// the control-plane builder for the control-plane role, the worker builder
// otherwise.
type recordingBuildTree struct {
	calls []buildTreeCall
	err   error
}

// build implements the BuildTree seam.
func (b *recordingBuildTree) build(role, cpIP, nodeName string, pk pki.ClusterPKI, kubeletCert, kubeletKey []byte, kubeconfigs map[string][]byte, encryptionConfig []byte) (map[string][]byte, error) {
	b.calls = append(b.calls, buildTreeCall{role: role, cpIP: cpIP, nodeName: nodeName, pk: pk, kubeletCert: kubeletCert, kubeletKey: kubeletKey, kubeconfigs: kubeconfigs, encryptionConfig: encryptionConfig})
	if b.err != nil {
		return nil, b.err
	}
	if role == testConfigRoleControlPlane {
		return confexttree.BuildControlPlane(cpIP, nodeName, pk, kubeletCert, kubeletKey, kubeconfigs["kubelet"], kubeconfigs["admin"], kubeconfigs["controller-manager"], kubeconfigs["scheduler"], encryptionConfig)
	}
	return confexttree.BuildWorker(nodeName, pk, kubeletCert, kubeletKey, kubeconfigs["kubelet"])
}

// genPKICall captures one invocation of the cluster PKI generator seam: the
// control-plane IP and node name the apiserver certificate SAN is built for.
type genPKICall struct {
	cpIP   string
	cpName string
}

// recordingGenPKI records every invocation and returns the canned cluster
// PKI, or the injected error.
type recordingGenPKI struct {
	calls []genPKICall
	pk    pki.ClusterPKI
	err   error
}

// gen implements the GenerateClusterPKI seam.
func (g *recordingGenPKI) gen(cpIP, cpName string) (pki.ClusterPKI, error) {
	g.calls = append(g.calls, genPKICall{cpIP: cpIP, cpName: cpName})
	if g.err != nil {
		return pki.ClusterPKI{}, g.err
	}
	return g.pk, nil
}

// genKubeletCall captures one invocation of the kubelet certificate seam: the
// cluster PKI material passed and the node name.
type genKubeletCall struct {
	pk       pki.ClusterPKI
	nodeName string
}

// recordingGenKubeletCert records every invocation and returns the canned
// kubelet certificate and key, or the injected error.
type recordingGenKubeletCert struct {
	calls []genKubeletCall
	cert  []byte
	key   []byte
	err   error
}

// gen implements the GenerateKubeletCert seam.
func (g *recordingGenKubeletCert) gen(pk pki.ClusterPKI, nodeName string) ([]byte, []byte, error) {
	g.calls = append(g.calls, genKubeletCall{pk: pk, nodeName: nodeName})
	if g.err != nil {
		return nil, nil, g.err
	}
	return g.cert, g.key, nil
}

// renderKubeconfigCall captures one invocation of the kubeconfig renderer
// seam: the server URL and user name the controller passed.
type renderKubeconfigCall struct {
	serverURL  string
	user       string
	clientCert []byte
	clientKey  []byte
}

// recordingRenderKubeconfig records every invocation and returns a minimal
// kubeconfig document whose server line carries the exact URL the controller
// passed, so tree-content assertions observe the URL end to end. The CA bytes
// are accepted and deliberately ignored: the renderer's PEM parsing is the
// pki package's own contract.
type recordingRenderKubeconfig struct {
	calls []renderKubeconfigCall
	err   error
}

// render implements the RenderKubeconfig seam.
func (r *recordingRenderKubeconfig) render(caPEM []byte, serverURL, user string, clientCert, clientKey []byte) ([]byte, error) {
	r.calls = append(r.calls, renderKubeconfigCall{serverURL: serverURL, user: user, clientCert: clientCert, clientKey: clientKey})
	if r.err != nil {
		return nil, r.err
	}
	return []byte("server: " + serverURL + "\nuser: " + user + "\n"), nil
}

// configFixture bundles the reconciler under test with every recording seam.
type configFixture struct {
	r       *HypervisorConfigReconciler
	build   *recordingBuildTree
	genPKI  *recordingGenPKI
	genCert *recordingGenKubeletCert
	render  *recordingRenderKubeconfig
}

// newConfigFixture builds the reconciler under test over the recording seams.
// The composite literal pins the exact reconciler shape the implementation
// must expose: the controller-runtime wiring plus the injectable BuildTree,
// GenerateClusterPKI, GenerateKubeletCert, and RenderKubeconfig dependencies.
func newConfigFixture(t *testing.T, c client.Client) *configFixture {
	t.Helper()

	build := &recordingBuildTree{}
	genPKI := &recordingGenPKI{pk: fixtureClusterPKI()}
	genCert := &recordingGenKubeletCert{cert: []byte(testFixtureKubeletCert), key: []byte(testFixtureKubeletKey)}
	render := &recordingRenderKubeconfig{}

	r := &HypervisorConfigReconciler{
		Client:              c,
		Scheme:              newScheme(),
		Recorder:            record.NewFakeRecorder(16),
		BuildTree:           build.build,
		GenerateClusterPKI:  genPKI.gen,
		GenerateKubeletCert: genCert.gen,
		RenderKubeconfig:    render.render,
	}

	return &configFixture{r: r, build: build, genPKI: genPKI, genCert: genCert, render: render}
}

// linkedConfig is the CAPI linkage the bootstrap reconciler resolves: the
// owning CAPI Machine, the HypervisorConfig bootstrap object, and the linked
// HypervisorMachine infrastructure object. The cluster objects come from the
// shared linkedCluster fixture.
type linkedConfig struct {
	namespace string
	name      string // the HypervisorConfig name
	machine   *clusterv1.Machine
	cfg       *bootstrapv1alpha1.HypervisorConfig
	hm        *infrastructurev1alpha1.HypervisorMachine
}

// newLinkedConfig creates the full CAPI linkage for one machine: a
// HypervisorConfig owned by the CAPI Machine, whose bootstrap config ref
// points back at the config and whose infrastructure ref points at the
// HypervisorMachine. clusterName is the cluster the machine belongs to (the
// linked cluster's name in every happy-path test; a ghost name for the
// missing-cluster case). controlPlane controls whether the Machine carries
// the standard control-plane label, while spec.role stays empty so the role
// is derived from the label; machineIP, when non-empty, is recorded as the
// HypervisorMachine's InternalIP. mutate, when non-nil, adjusts the config
// before it is created.
func newLinkedConfig(t *testing.T, c client.Client, lc *linkedCluster, clusterName, machineName string, controlPlane bool, machineIP string, mutate func(*bootstrapv1alpha1.HypervisorConfig)) *linkedConfig {
	t.Helper()
	ctx := t.Context()

	labels := map[string]string{clusterv1.ClusterNameLabel: clusterName}
	if controlPlane {
		labels[clusterv1.MachineControlPlaneLabel] = ""
	}
	machine := &clusterv1.Machine{
		ObjectMeta: metav1.ObjectMeta{Name: machineName, Namespace: lc.namespace, Labels: labels},
		Spec: clusterv1.MachineSpec{
			ClusterName: clusterName,
			Bootstrap:   clusterv1.Bootstrap{},
			InfrastructureRef: corev1.ObjectReference{
				APIVersion: infrastructurev1alpha1.GroupVersion.String(),
				Kind:       "HypervisorMachine",
				Name:       machineName,
				Namespace:  lc.namespace,
			},
		},
	}
	if err := c.Create(ctx, machine); err != nil {
		t.Fatalf("create CAPI Machine: %v", err)
	}

	cfg := &bootstrapv1alpha1.HypervisorConfig{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName + "-config",
			Namespace: lc.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       machine.Name,
					UID:        machine.UID,
				},
			},
		},
		Spec: bootstrapv1alpha1.HypervisorConfigSpec{
			ClusterName: clusterName,
		},
	}
	if mutate != nil {
		mutate(cfg)
	}
	if err := c.Create(ctx, cfg); err != nil {
		t.Fatalf("create HypervisorConfig: %v", err)
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{
		ObjectMeta: metav1.ObjectMeta{
			Name:      machineName,
			Namespace: lc.namespace,
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Machine",
					Name:       machine.Name,
					UID:        machine.UID,
				},
			},
		},
		Spec: infrastructurev1alpha1.HypervisorMachineSpec{ClusterName: clusterName},
	}
	if err := c.Create(ctx, hm); err != nil {
		t.Fatalf("create HypervisorMachine: %v", err)
	}
	if machineIP != "" {
		hm.Status.Addresses = []clusterv1.MachineAddress{{Type: clusterv1.MachineInternalIP, Address: machineIP}}
		if err := c.Status().Update(ctx, hm); err != nil {
			t.Fatalf("set HypervisorMachine addresses: %v", err)
		}
	}

	return &linkedConfig{namespace: lc.namespace, name: cfg.Name, machine: machine, cfg: cfg, hm: hm}
}

// reconcileConfig runs one reconcile of the config and fails the test on any
// error.
func (fx *configFixture) reconcileConfig(t *testing.T, cfg *bootstrapv1alpha1.HypervisorConfig) {
	t.Helper()
	if _, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cfg)}); err != nil {
		t.Fatalf("Reconcile error: %v", err)
	}
}

// getConfig reads the config back from the API store.
func getConfig(t *testing.T, c client.Client, cfg *bootstrapv1alpha1.HypervisorConfig) *bootstrapv1alpha1.HypervisorConfig {
	t.Helper()
	got := &bootstrapv1alpha1.HypervisorConfig{}
	if err := c.Get(t.Context(), client.ObjectKeyFromObject(cfg), got); err != nil {
		t.Fatalf("Get HypervisorConfig: %v", err)
	}
	return got
}

// configDataSecretKey returns the object key of the rendered data Secret from
// the config's status, failing when status.dataSecretName is not set.
func configDataSecretKey(t *testing.T, c client.Client, cfg *bootstrapv1alpha1.HypervisorConfig) client.ObjectKey {
	t.Helper()
	got := getConfig(t, c, cfg)
	if got.Status.DataSecretName == nil || *got.Status.DataSecretName == "" {
		t.Fatalf("status.dataSecretName not set after reconcile (status %+v)", got.Status)
	}
	return client.ObjectKey{Namespace: cfg.Namespace, Name: *got.Status.DataSecretName}
}

// dataSecretBlob returns the raw tree.json bytes of the named Secret.
func dataSecretBlob(t *testing.T, c client.Client, key client.ObjectKey) []byte {
	t.Helper()
	secret := &corev1.Secret{}
	if err := c.Get(t.Context(), key, secret); err != nil {
		t.Fatalf("Get Secret %s: %v", key, err)
	}
	blob, ok := secret.Data["tree.json"]
	if !ok {
		t.Fatalf("Secret %s has no tree.json key (have %v)", key, secret.Data)
	}
	return blob
}

// configSecretTree reads the data Secret named by the config status and
// decodes its tree.json blob back into the path-to-content map.
func configSecretTree(t *testing.T, c client.Client, key client.ObjectKey) map[string][]byte {
	t.Helper()
	blob := dataSecretBlob(t, c, key)
	encoded := map[string]string{}
	if err := json.Unmarshal(blob, &encoded); err != nil {
		t.Fatalf("decode tree.json blob: %v", err)
	}
	tree := make(map[string][]byte, len(encoded))
	for path, content := range encoded {
		raw, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			t.Fatalf("decode tree.json entry %q: %v", path, err)
		}
		tree[path] = raw
	}
	return tree
}

// configCondition returns the status condition with the given type, or nil.
func configCondition(cfg *bootstrapv1alpha1.HypervisorConfig, t clusterv1.ConditionType) *clusterv1.Condition {
	for i := range cfg.Status.Conditions {
		if cfg.Status.Conditions[i].Type == t {
			return &cfg.Status.Conditions[i]
		}
	}
	return nil
}

// assertConfigFailure fails the test unless the config reports a failure: not
// ready with both failureReason and failureMessage set.
func assertConfigFailure(t *testing.T, cfg *bootstrapv1alpha1.HypervisorConfig) {
	t.Helper()
	if cfg.Status.Ready {
		t.Error("status.ready = true after failed reconcile, want false")
	}
	if cfg.Status.FailureReason == "" {
		t.Error("status.failureReason empty after failed reconcile")
	}
	if cfg.Status.FailureMessage == "" {
		t.Error("status.failureMessage empty after failed reconcile")
	}
}

// countSecrets returns the number of Secrets in the namespace.
func countSecrets(t *testing.T, c client.Client, namespace string) int {
	t.Helper()
	list := &corev1.SecretList{}
	if err := c.List(t.Context(), list, client.InNamespace(namespace)); err != nil {
		t.Fatalf("List Secrets in %q: %v", namespace, err)
	}
	return len(list.Items)
}

// countSecretsNamed returns the number of Secrets with the given name in the
// namespace.
func countSecretsNamed(t *testing.T, c client.Client, namespace, name string) int {
	t.Helper()
	list := &corev1.SecretList{}
	if err := c.List(t.Context(), list, client.InNamespace(namespace)); err != nil {
		t.Fatalf("List Secrets in %q: %v", namespace, err)
	}
	n := 0
	for _, s := range list.Items {
		if s.Name == name {
			n++
		}
	}
	return n
}

// setClusterEndpoint publishes the workload cluster's control-plane endpoint
// on the HypervisorCluster status, as the cluster controller does once the
// control plane reports initialized.
func setClusterEndpoint(t *testing.T, c client.Client, lc *linkedCluster, host string, port int32) {
	t.Helper()
	lc.hc.Status.ControlPlaneEndpoint = clusterv1.APIEndpoint{Host: host, Port: port}
	if err := c.Status().Update(t.Context(), lc.hc); err != nil {
		t.Fatalf("set HypervisorCluster controlPlaneEndpoint: %v", err)
	}
}

// controlPlaneTreeKeys returns the exact control-plane tree file set for the
// node named nodeName: the z-etcd, z-kubernetes-cp, and z-kubelet-<node> sets
// from the phase-B confext layout.
func controlPlaneTreeKeys(nodeName string) []string {
	return []string{
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
		"z-kubelet-" + nodeName + "/etc/kubernetes/kubelet.conf",
		"z-kubelet-" + nodeName + "/etc/kubernetes/pki/ca.pem",
		"z-kubelet-" + nodeName + "/etc/kubernetes/pki/" + nodeName + ".pem",
		"z-kubelet-" + nodeName + "/etc/kubernetes/pki/" + nodeName + "-key.pem",
		"z-kubelet-" + nodeName + "/etc/extension-release.d/extension-release.z-kubelet-" + nodeName,
	}
}

// workerTreeKeys returns the exact worker tree file set for the node named
// nodeName: the z-kubelet-<node> set only.
func workerTreeKeys(nodeName string) []string {
	return []string{
		"z-kubelet-" + nodeName + "/etc/kubernetes/kubelet.conf",
		"z-kubelet-" + nodeName + "/etc/kubernetes/pki/ca.pem",
		"z-kubelet-" + nodeName + "/etc/kubernetes/pki/" + nodeName + ".pem",
		"z-kubelet-" + nodeName + "/etc/kubernetes/pki/" + nodeName + "-key.pem",
		"z-kubelet-" + nodeName + "/etc/extension-release.d/extension-release.z-kubelet-" + nodeName,
	}
}

// wantTreeKeys fails the test unless the tree has exactly the expected keys.
func wantTreeKeys(t *testing.T, tree map[string][]byte, want []string) {
	t.Helper()
	if len(tree) != len(want) {
		t.Errorf("tree has %d keys, want %d (tree keys %v)", len(tree), len(want), treeKeysOf(tree))
	}
	for _, key := range want {
		if _, ok := tree[key]; !ok {
			t.Errorf("tree missing key %q", key)
		}
	}
}

// treeKeysOf returns the tree keys in map iteration order.
func treeKeysOf(tree map[string][]byte) []string {
	keys := make([]string, 0, len(tree))
	for key := range tree {
		keys = append(keys, key)
	}
	return keys
}

// wantRenderCalls fails the test unless the recorded renderer invocations
// cover exactly the expected server URL and user pairs, in any order.
func wantRenderCalls(t *testing.T, calls []renderKubeconfigCall, wantURL string, wantUsers ...string) {
	t.Helper()
	if len(calls) != len(wantUsers) {
		t.Fatalf("RenderKubeconfig called %d times, want %d (calls %+v)", len(calls), len(wantUsers), calls)
	}
	seen := make(map[string]bool, len(calls))
	for _, call := range calls {
		if call.serverURL != wantURL {
			t.Errorf("RenderKubeconfig server URL = %q, want %q", call.serverURL, wantURL)
		}
		seen[call.user] = true
	}
	for _, wantUser := range wantUsers {
		if !seen[wantUser] {
			t.Errorf("RenderKubeconfig never called with user %q (calls %+v)", wantUser, calls)
		}
	}
}

// TestConfigRoleDetection pins the role contract: with an empty spec.role the
// role comes from the owning Machine's labels — the standard control-plane
// label means the control-plane tree, its absence the kubelet-only worker
// tree — and an explicit spec.role overrides the labels. The decoded Secret
// tree is the authoritative observable, and the recorded BuildTree call pins
// the exact role string the controller resolved.
func TestConfigRoleDetection(t *testing.T) {
	c := mustReconcileClient(t)
	lc := newLinkedCluster(t, c, "config-role", "capi-cluster")

	t.Run("control-plane machine renders the control-plane tree", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)
		fx.reconcileConfig(t, lcfg.cfg)

		tree := configSecretTree(t, c, configDataSecretKey(t, c, lcfg.cfg))
		wantTreeKeys(t, tree, controlPlaneTreeKeys("cp-1"))

		if len(fx.build.calls) != 1 {
			t.Fatalf("BuildTree called %d times, want 1", len(fx.build.calls))
		}
		call := fx.build.calls[0]
		if call.role != testConfigRoleControlPlane || call.cpIP != testCPIP || call.nodeName != "cp-1" {
			t.Errorf("BuildTree call = role %q cpIP %q nodeName %q, want role %q cpIP %q nodeName %q",
				call.role, call.cpIP, call.nodeName, testConfigRoleControlPlane, testCPIP, "cp-1")
		}
	})

	t.Run("worker machine renders the kubelet-only tree", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		setClusterEndpoint(t, c, lc, testCPIP, testCPPort)
		lcfg := newLinkedConfig(t, c, lc, lc.name, "worker-1", false, "", nil)
		fx.reconcileConfig(t, lcfg.cfg)

		tree := configSecretTree(t, c, configDataSecretKey(t, c, lcfg.cfg))
		wantTreeKeys(t, tree, workerTreeKeys("worker-1"))
		for key := range tree {
			if strings.HasPrefix(key, "z-etcd") || strings.HasPrefix(key, "z-kubernetes-cp") {
				t.Errorf("worker tree leaks control-plane key %q", key)
			}
		}

		if len(fx.build.calls) != 1 || fx.build.calls[0].role != testConfigRoleWorker {
			t.Errorf("BuildTree role = %q, want %q (calls %d)", roleOfBuildCalls(fx.build), testConfigRoleWorker, len(fx.build.calls))
		}
	})

	t.Run("explicit spec.role overrides the machine labels", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		setClusterEndpoint(t, c, lc, testCPIP, testCPPort)
		// The Machine carries the control-plane label but the config pins the
		// worker role; the rendered tree must be the worker tree.
		lcfg := newLinkedConfig(t, c, lc, lc.name, "labeled-cp", true, testCPIP, func(cfg *bootstrapv1alpha1.HypervisorConfig) {
			cfg.Spec.Role = testConfigRoleWorker
		})
		fx.reconcileConfig(t, lcfg.cfg)

		tree := configSecretTree(t, c, configDataSecretKey(t, c, lcfg.cfg))
		wantTreeKeys(t, tree, workerTreeKeys("labeled-cp"))
		if len(fx.build.calls) != 1 || fx.build.calls[0].role != testConfigRoleWorker {
			t.Errorf("BuildTree role = %q, want the explicit worker role (calls %d)", roleOfBuildCalls(fx.build), len(fx.build.calls))
		}
	})

	t.Run("explicit spec.nodeName names the kubelet tree", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-2", true, testCPIP, func(cfg *bootstrapv1alpha1.HypervisorConfig) {
			cfg.Spec.NodeName = "custom-node"
		})
		fx.reconcileConfig(t, lcfg.cfg)

		tree := configSecretTree(t, c, configDataSecretKey(t, c, lcfg.cfg))
		wantTreeKeys(t, tree, controlPlaneTreeKeys("custom-node"))
		if len(fx.build.calls) != 1 || fx.build.calls[0].nodeName != "custom-node" {
			t.Errorf("BuildTree nodeName = %q, want custom-node", nodeNameOfBuildCalls(fx.build))
		}
	})
}

// roleOfBuildCalls returns the role of the first recorded BuildTree call, or
// the empty string when none was recorded.
func roleOfBuildCalls(b *recordingBuildTree) string {
	if len(b.calls) == 0 {
		return ""
	}
	return b.calls[0].role
}

// nodeNameOfBuildCalls returns the node name of the first recorded BuildTree
// call, or the empty string when none was recorded.
func nodeNameOfBuildCalls(b *recordingBuildTree) string {
	if len(b.calls) == 0 {
		return ""
	}
	return b.calls[0].nodeName
}

// TestConfigSecretWrittenWithTreeBlob pins the Secret rendering contract: the
// controller writes one Secret whose single data key is tree.json, a JSON
// object mapping every tree path to its base64-encoded content, and whose
// decoded content matches the role tree — the control-plane IP interpolated
// into the etcd config, the fixture PKI bytes carried verbatim, and the
// extension-release metadata. status.dataSecretName names the conventional
// <config>-data Secret.
func TestConfigSecretWrittenWithTreeBlob(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newConfigFixture(t, c)
	lc := newLinkedCluster(t, c, "config-secret", "capi-cluster")
	lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)
	fx.reconcileConfig(t, lcfg.cfg)

	cfg := getConfig(t, c, lcfg.cfg)
	if cfg.Status.DataSecretName == nil || *cfg.Status.DataSecretName == "" {
		t.Fatalf("status.dataSecretName not set (status %+v)", cfg.Status)
	}
	wantSecretName := lcfg.name + "-data"
	if *cfg.Status.DataSecretName != wantSecretName {
		t.Errorf("status.dataSecretName = %q, want %q", *cfg.Status.DataSecretName, wantSecretName)
	}

	key := client.ObjectKey{Namespace: lc.namespace, Name: *cfg.Status.DataSecretName}
	secret := &corev1.Secret{}
	if err := c.Get(t.Context(), key, secret); err != nil {
		t.Fatalf("Get data Secret %s: %v", key, err)
	}
	if len(secret.Data) != 1 {
		t.Errorf("data Secret has %d keys, want exactly one (tree.json): %v", len(secret.Data), secret.Data)
	}
	if _, ok := secret.Data["tree.json"]; !ok {
		t.Fatalf("data Secret missing tree.json key (have %v)", secret.Data)
	}

	tree := configSecretTree(t, c, key)
	wantTreeKeys(t, tree, controlPlaneTreeKeys("cp-1"))

	etcdConf := string(tree["z-etcd/etc/etcd/etcd.conf.yml"])
	if !strings.Contains(etcdConf, testCPIP) {
		t.Errorf("etcd.conf.yml does not carry the control-plane IP %q", testCPIP)
	}
	if !strings.Contains(etcdConf, "cp-1") {
		t.Error("etcd.conf.yml does not carry the node name cp-1")
	}

	extRel := string(tree["z-etcd/etc/extension-release.d/extension-release.z-etcd"])
	wantExtRel := "ID=fedora\nVERSION_ID=44\nKERNEL_VERSION=7.1\nEXTENSION=z-etcd\n"
	if extRel != wantExtRel {
		t.Errorf("z-etcd extension-release = %q, want %q", extRel, wantExtRel)
	}

	if got := tree["z-kubernetes-cp/etc/kubernetes/pki/ca.pem"]; !bytes.Equal(got, fixtureClusterPKI().CA) {
		t.Errorf("tree ca.pem = %q, want the injected cluster PKI CA", got)
	}
}

// TestConfigReadyAndDataSecretAvailable pins the readiness contract: after a
// successful render the config is ready and carries the DataSecretAvailable
// condition true.
func TestConfigReadyAndDataSecretAvailable(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newConfigFixture(t, c)
	lc := newLinkedCluster(t, c, "config-ready", "capi-cluster")
	setClusterEndpoint(t, c, lc, testCPIP, testCPPort)
	lcfg := newLinkedConfig(t, c, lc, lc.name, "worker-1", false, "", nil)
	fx.reconcileConfig(t, lcfg.cfg)

	cfg := getConfig(t, c, lcfg.cfg)
	if !cfg.Status.Ready {
		t.Error("status.ready = false after successful reconcile, want true")
	}
	cond := configCondition(cfg, configDataSecretAvailableCondition)
	if cond == nil {
		t.Fatalf("condition %q missing from status.conditions: %v", configDataSecretAvailableCondition, cfg.Status.Conditions)
	}
	if cond.Status != corev1.ConditionTrue {
		t.Errorf("condition %q status = %q, want %q", configDataSecretAvailableCondition, cond.Status, corev1.ConditionTrue)
	}
}

// TestConfigIdempotentReconcile pins the idempotency contract: a second
// reconcile with unchanged inputs neither creates a new data Secret nor
// changes the rendered bytes. status.dataSecretName stays the same and the
// namespace Secret count does not grow.
func TestConfigIdempotentReconcile(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newConfigFixture(t, c)
	lc := newLinkedCluster(t, c, "config-idem", "capi-cluster")
	lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)
	fx.reconcileConfig(t, lcfg.cfg)

	first := getConfig(t, c, lcfg.cfg)
	if first.Status.DataSecretName == nil {
		t.Fatal("status.dataSecretName not set after first reconcile")
	}
	firstKey := *first.Status.DataSecretName
	firstBlob := dataSecretBlob(t, c, client.ObjectKey{Namespace: lc.namespace, Name: firstKey})
	firstCount := countSecrets(t, c, lc.namespace)
	if firstCount == 0 {
		t.Fatal("no Secret after first reconcile")
	}

	fx.reconcileConfig(t, lcfg.cfg)

	second := getConfig(t, c, lcfg.cfg)
	if second.Status.DataSecretName == nil || *second.Status.DataSecretName != firstKey {
		t.Errorf("status.dataSecretName changed across reconciles: %q -> %v", firstKey, second.Status.DataSecretName)
	}
	if got := countSecrets(t, c, lc.namespace); got != firstCount {
		t.Errorf("Secret count changed across reconciles: %d -> %d", firstCount, got)
	}
	if got := dataSecretBlob(t, c, client.ObjectKey{Namespace: lc.namespace, Name: firstKey}); !bytes.Equal(got, firstBlob) {
		t.Error("tree.json blob changed across reconciles with unchanged inputs")
	}
}

// TestConfigClusterPKISecretCreatedOnce pins the cluster PKI persistence
// contract: the first reconcile generates the cluster-scoped PKI through the
// injected generator, called with the resolved control-plane IP and the node
// name (the apiserver SAN inputs), and stores it in the conventional
// <cluster>-pki Secret whose data keys are exactly the pki.ClusterPKI field
// names. The stored material feeds the per-machine kubelet certificate. A
// second reconcile reads the stored Secret and never regenerates.
func TestConfigClusterPKISecretCreatedOnce(t *testing.T) {
	c := mustReconcileClient(t)
	fx := newConfigFixture(t, c)
	lc := newLinkedCluster(t, c, "config-pki", "capi-cluster")
	lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)
	fx.reconcileConfig(t, lcfg.cfg)

	if len(fx.genPKI.calls) != 1 {
		t.Fatalf("GenerateClusterPKI called %d times, want 1", len(fx.genPKI.calls))
	}
	if call := fx.genPKI.calls[0]; call.cpIP != testCPIP || call.cpName != "cp-1" {
		t.Errorf("GenerateClusterPKI(%q, %q), want (%q, %q)", call.cpIP, call.cpName, testCPIP, "cp-1")
	}

	pkiKey := client.ObjectKey{Namespace: lc.namespace, Name: lc.name + "-pki"}
	secret := &corev1.Secret{}
	if err := c.Get(t.Context(), pkiKey, secret); err != nil {
		t.Fatalf("Get cluster PKI Secret %s: %v", pkiKey, err)
	}
	wantData := fixturePKISecretData()
	if len(secret.Data) != len(wantData) {
		t.Errorf("cluster PKI Secret has %d keys, want %d: %v", len(secret.Data), len(wantData), secret.Data)
	}
	for key, want := range wantData {
		if got, ok := secret.Data[key]; !ok || !bytes.Equal(got, want) {
			t.Errorf("cluster PKI Secret data[%q] = %q (present %v), want %q", key, got, ok, want)
		}
	}

	if len(fx.genCert.calls) != 1 || fx.genCert.calls[0].nodeName != "cp-1" {
		t.Errorf("GenerateKubeletCert calls = %+v, want one call for node cp-1", fx.genCert.calls)
	}
	if !reflect.DeepEqual(fx.genCert.calls[0].pk, fixtureClusterPKI()) {
		t.Error("GenerateKubeletCert did not receive the stored cluster PKI")
	}

	fx.reconcileConfig(t, lcfg.cfg)

	if len(fx.genPKI.calls) != 1 {
		t.Errorf("GenerateClusterPKI called %d times across two reconciles, want 1", len(fx.genPKI.calls))
	}
	if got := countSecretsNamed(t, c, lc.namespace, lc.name+"-pki"); got != 1 {
		t.Errorf("cluster PKI Secrets = %d, want exactly 1", got)
	}
}

// TestConfigFailureSurfaces pins the failure contract: a config that cannot
// be resolved or rendered is left not ready with status.failureReason and
// status.failureMessage set, and no data Secret exists. A failing tree build
// or cluster PKI generation additionally surfaces as a reconcile error that
// preserves the underlying error.
func TestConfigFailureSurfaces(t *testing.T) {
	c := mustReconcileClient(t)

	t.Run("missing owning machine", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lc := newLinkedCluster(t, c, "cfg-fail-owner", "capi-cluster")
		cfg := &bootstrapv1alpha1.HypervisorConfig{
			ObjectMeta: metav1.ObjectMeta{Name: "orphan-config", Namespace: lc.namespace},
			Spec: bootstrapv1alpha1.HypervisorConfigSpec{
				ClusterName: lc.name,
			},
		}
		if err := c.Create(t.Context(), cfg); err != nil {
			t.Fatalf("create orphan HypervisorConfig: %v", err)
		}

		_, _ = fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})

		assertConfigFailure(t, getConfig(t, c, cfg))
		if got := countSecrets(t, c, lc.namespace); got != 0 {
			t.Errorf("missing-machine reconcile created %d Secrets, want 0", got)
		}
		assertSeamsUntouched(t, fx)
	})

	t.Run("missing linked cluster", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lc := newLinkedCluster(t, c, "cfg-fail-cluster", "capi-cluster")
		lcfg := newLinkedConfig(t, c, lc, "ghost-cluster", "ghost-worker", false, "", nil)

		_, _ = fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcfg.cfg)})

		assertConfigFailure(t, getConfig(t, c, lcfg.cfg))
		if got := countSecrets(t, c, lc.namespace); got != 0 {
			t.Errorf("missing-cluster reconcile created %d Secrets, want 0", got)
		}
		assertSeamsUntouched(t, fx)
	})

	t.Run("control-plane machine without an internal IP", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lc := newLinkedCluster(t, c, "cfg-fail-ip", "capi-cluster")
		lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, "", nil)

		_, _ = fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcfg.cfg)})

		assertConfigFailure(t, getConfig(t, c, lcfg.cfg))
	})

	t.Run("worker without a control-plane endpoint", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lc := newLinkedCluster(t, c, "cfg-fail-endpoint", "capi-cluster")
		lcfg := newLinkedConfig(t, c, lc, lc.name, "worker-1", false, "", nil)

		_, _ = fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcfg.cfg)})

		assertConfigFailure(t, getConfig(t, c, lcfg.cfg))
	})

	t.Run("tree build failure", func(t *testing.T) {
		errBuild := errors.New("fake: tree build denied")
		fx := newConfigFixture(t, c)
		fx.build.err = errBuild
		lc := newLinkedCluster(t, c, "cfg-fail-build", "capi-cluster")
		lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcfg.cfg)})
		if err == nil {
			t.Fatal("Reconcile succeeded with a failing tree build, want an error")
		}
		if !errors.Is(err, errBuild) {
			t.Errorf("Reconcile error %v does not wrap %v", err, errBuild)
		}

		assertConfigFailure(t, getConfig(t, c, lcfg.cfg))
		if got := countSecrets(t, c, lc.namespace); got != 0 {
			t.Errorf("failed reconcile created %d Secrets, want 0", got)
		}
	})

	t.Run("cluster PKI generation failure", func(t *testing.T) {
		errPKI := errors.New("fake: pki generation denied")
		fx := newConfigFixture(t, c)
		fx.genPKI.err = errPKI
		lc := newLinkedCluster(t, c, "cfg-fail-pki", "capi-cluster")
		lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)

		_, err := fx.r.Reconcile(t.Context(), ctrl.Request{NamespacedName: client.ObjectKeyFromObject(lcfg.cfg)})
		if err == nil {
			t.Fatal("Reconcile succeeded with a failing PKI generator, want an error")
		}
		if !errors.Is(err, errPKI) {
			t.Errorf("Reconcile error %v does not wrap %v", err, errPKI)
		}

		assertConfigFailure(t, getConfig(t, c, lcfg.cfg))
		if got := countSecrets(t, c, lc.namespace); got != 0 {
			t.Errorf("failed reconcile created %d Secrets, want 0", got)
		}
	})
}

// assertSeamsUntouched fails the test unless the resolution failure happened
// before any rendering seam ran.
func assertSeamsUntouched(t *testing.T, fx *configFixture) {
	t.Helper()
	if len(fx.build.calls) != 0 || len(fx.genPKI.calls) != 0 || len(fx.genCert.calls) != 0 || len(fx.render.calls) != 0 {
		t.Errorf("resolution failure still touched the rendering seams: build %d, genPKI %d, genCert %d, render %d",
			len(fx.build.calls), len(fx.genPKI.calls), len(fx.genCert.calls), len(fx.render.calls))
	}
}

// TestConfigKubeconfigServerURL pins the kubeconfig contract: every rendered
// kubeconfig — admin, controller-manager, scheduler, and kubelet — embeds the
// server URL https://<cp-ip>:6443, and the renderer is invoked once per
// kubeconfig with the KTHW user names. A control-plane node's address is its
// own InternalIP; a worker's is the cluster control-plane endpoint.
func TestConfigKubeconfigServerURL(t *testing.T) {
	c := mustReconcileClient(t)
	lc := newLinkedCluster(t, c, "config-kubeurl", "capi-cluster")

	t.Run("control-plane kubeconfigs point at the control-plane IP", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		lcfg := newLinkedConfig(t, c, lc, lc.name, "cp-1", true, testCPIP, nil)
		fx.reconcileConfig(t, lcfg.cfg)

		wantURL := fmt.Sprintf("https://%s:%d", testCPIP, testCPPort)
		tree := configSecretTree(t, c, configDataSecretKey(t, c, lcfg.cfg))
		for _, path := range []string{
			"z-kubernetes-cp/etc/kubernetes/admin.kubeconfig",
			"z-kubernetes-cp/etc/kubernetes/controller-manager.kubeconfig",
			"z-kubernetes-cp/etc/kubernetes/scheduler.kubeconfig",
			"z-kubelet-cp-1/etc/kubernetes/kubelet.conf",
		} {
			if content := string(tree[path]); !strings.Contains(content, wantURL) {
				t.Errorf("%s does not contain %q: %q", path, wantURL, content)
			}
		}

		wantRenderCalls(t, fx.render.calls, wantURL,
			"admin", "system:kube-controller-manager", "system:kube-scheduler", "system:node:cp-1")
	})

	t.Run("worker kubelet.conf points at the cluster endpoint", func(t *testing.T) {
		fx := newConfigFixture(t, c)
		setClusterEndpoint(t, c, lc, testCPIP, testCPPort)
		lcfg := newLinkedConfig(t, c, lc, lc.name, "worker-1", false, "", nil)
		fx.reconcileConfig(t, lcfg.cfg)

		wantURL := fmt.Sprintf("https://%s:%d", testCPIP, testCPPort)
		tree := configSecretTree(t, c, configDataSecretKey(t, c, lcfg.cfg))
		content := string(tree["z-kubelet-worker-1/etc/kubernetes/kubelet.conf"])
		if !strings.Contains(content, wantURL) {
			t.Errorf("worker kubelet.conf does not contain %q: %q", wantURL, content)
		}

		wantRenderCalls(t, fx.render.calls, wantURL, "system:node:worker-1")
	})
}
