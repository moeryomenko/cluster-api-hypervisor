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

package controllers

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	"sigs.k8s.io/cluster-api/util/predicates"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

const (
	// configRoleControlPlane and configRoleWorker are the node roles the
	// bootstrap data is rendered for.
	configRoleControlPlane = "control-plane"
	configRoleWorker       = "worker"

	// dataSecretAvailableCondition is the condition type reported once the
	// bootstrap data Secret exists.
	dataSecretAvailableCondition = clusterv1.ConditionType("DataSecretAvailable")

	// configTreeBlobKey is the data key of the rendered tree in the bootstrap
	// data Secret: a JSON object mapping every tree path to its base64-encoded
	// content. Kubernetes Secret data keys cannot contain "/", so the tree
	// paths cannot be stored as literal Secret keys.
	configTreeBlobKey = "tree.json"
)

// encryptionConfig is the static apiserver EncryptionConfiguration rendered
// into the control-plane tree, mirroring the phase-B tree layout. The key is
// a fixed lab secret: the rendered bytes must be deterministic so a repeated
// reconcile leaves the tree unchanged.
const encryptionConfig = `kind: EncryptionConfiguration
apiVersion: apiserver.config.k8s.io/v1
resources:
  - resources:
      - secrets
    providers:
      - aescbc:
          keys:
            - name: key1
              secret: MDEyMzQ1Njc4OWFiY2RlZjAxMjM0NTY3ODlhYmNkZWY=
      - identity: {}
`

// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisorclusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch

// HypervisorConfigReconciler reconciles a HypervisorConfig object: it renders
// the role-split confext tree for the node the config bootstraps and writes it
// as the tree.json blob in the conventional <config>-data Secret. The PKI
// generation and kubeconfig rendering run behind injectable seams, so the
// reconcile contract is testable without generating any RSA key.
type HypervisorConfigReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// BuildTree renders the role-split confext tree for one node: the
	// kubeconfigs map holds the rendered documents keyed by role ("admin",
	// "controller-manager", "scheduler", "kubelet") and encryptionConfig is
	// the control-plane encryption configuration.
	BuildTree func(
		role, cpIP, nodeName string,
		pk pki.ClusterPKI,
		kubeletCert, kubeletKey []byte,
		kubeconfigs map[string][]byte,
		encryptionConfig []byte,
	) (map[string][]byte, error)
	// GenerateClusterPKI generates the cluster-scoped PKI for one cluster;
	// cpIP and cpName are the apiserver certificate SAN inputs.
	GenerateClusterPKI func(cpIP, cpName string) (pki.ClusterPKI, error)
	// GenerateKubeletCert generates one node's kubelet certificate and key
	// signed by the cluster PKI.
	GenerateKubeletCert func(pk pki.ClusterPKI, nodeName string) (certPEM, keyPEM []byte, err error)
	// RenderKubeconfig renders a kubeconfig document from PEM material with
	// the given server URL and user name.
	RenderKubeconfig func(caPEM []byte, serverURL, user string, clientCert, clientKey []byte) ([]byte, error)
}

// Reconcile renders the bootstrap data Secret for the HypervisorConfig: it
// resolves the owning CAPI Machine and the linked Cluster, derives the node
// role and the control-plane address, generates the cluster PKI once per
// cluster and stores it in the conventional <cluster>-pki Secret, renders the
// per-machine kubelet certificate and the kubeconfigs, builds the role-split
// confext tree, and writes it as the tree.json blob in the <config>-data
// Secret. A missing object is a no-op; an unresolvable link or missing
// address surfaces as a status failure without error; a failing dependency
// surfaces as a status failure and a reconcile error that preserves the
// underlying error.
func (r *HypervisorConfigReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	cfg := &bootstrapv1alpha1.HypervisorConfig{}
	if err := r.Get(ctx, req.NamespacedName, cfg); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get HypervisorConfig %q: %w", req.NamespacedName, err)
	}

	machine, err := r.owningMachine(ctx, cfg)
	if err != nil {
		return ctrl.Result{}, err
	}
	if machine == nil {
		return r.recordFailure(ctx, cfg, "MachineNotFound", "owning Machine not found")
	}

	cluster, err := r.linkedCluster(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		return r.recordFailure(
			ctx,
			cfg,
			"ClusterNotFound",
			fmt.Sprintf("linked Cluster %q not found", machine.Spec.ClusterName),
		)
	}

	role := configRoleWorker
	if cfg.Spec.Role != "" {
		role = cfg.Spec.Role
	} else if _, ok := machine.Labels[clusterv1.MachineControlPlaneLabel]; ok {
		role = configRoleControlPlane
	}

	nodeName := machine.Name
	if cfg.Spec.NodeName != "" {
		nodeName = cfg.Spec.NodeName
	}

	cpIP, cpPort, err := r.controlPlaneAddress(ctx, role, machine, cluster)
	if err != nil {
		return r.recordFailure(ctx, cfg, "AddressNotFound", err.Error())
	}

	pk, generated, err := r.clusterPKI(ctx, cfg, cpIP, nodeName)
	if err != nil {
		return r.recordFailureError(ctx, cfg, "ClusterPKIUnavailable", err)
	}

	kubeletCert, kubeletKey, err := r.GenerateKubeletCert(pk, nodeName)
	if err != nil {
		return r.recordFailureError(
			ctx,
			cfg,
			"KubeletCertGenerationFailed",
			fmt.Errorf("generate kubelet certificate: %w", err),
		)
	}

	serverURL := fmt.Sprintf("https://%s:%d", cpIP, cpPort)
	kubeconfigs, err := r.renderKubeconfigs(role, serverURL, nodeName, pk, kubeletCert, kubeletKey)
	if err != nil {
		return r.recordFailureError(ctx, cfg, "KubeconfigRenderFailed", err)
	}

	tree, err := r.BuildTree(role, cpIP, nodeName, pk, kubeletCert, kubeletKey, kubeconfigs, []byte(encryptionConfig))
	if err != nil {
		return r.recordFailureError(ctx, cfg, "TreeBuildFailed", fmt.Errorf("build confext tree: %w", err))
	}

	blob, err := encodeTreeBlob(tree)
	if err != nil {
		return r.recordFailureError(ctx, cfg, "TreeEncodeFailed", err)
	}

	if generated {
		if err := r.persistPKISecret(ctx, cfg, pk); err != nil {
			return r.recordFailureError(ctx, cfg, "ClusterPKIWriteFailed", err)
		}
	}

	dataSecretName := cfg.Name + "-data"
	if err := r.persistDataSecret(ctx, cfg, dataSecretName, blob); err != nil {
		return r.recordFailureError(ctx, cfg, "DataSecretWriteFailed", err)
	}

	cfg.Status.Ready = true
	cfg.Status.FailureReason = ""
	cfg.Status.FailureMessage = ""
	cfg.Status.DataSecretName = &dataSecretName
	markDataSecretAvailable(cfg, corev1.ConditionTrue, "BootstrapDataRendered", "bootstrap data Secret rendered")
	if err := r.Status().Update(ctx, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("update HypervisorConfig status: %w", err)
	}
	log.Info("rendered bootstrap data Secret", "config", cfg.Name, "secret", dataSecretName, "role", role)

	return ctrl.Result{}, nil
}

// recordFailure records a resolution failure on the config status: not ready
// with failureReason and failureMessage set. Resolution failures are not
// reconcile errors: the controller waits for the link to appear.
func (r *HypervisorConfigReconciler) recordFailure(
	ctx context.Context,
	cfg *bootstrapv1alpha1.HypervisorConfig,
	reason, message string,
) (ctrl.Result, error) {
	cfg.Status.Ready = false
	cfg.Status.FailureReason = reason
	cfg.Status.FailureMessage = message
	markDataSecretAvailable(cfg, corev1.ConditionFalse, reason, message)
	if err := r.Status().Update(ctx, cfg); err != nil {
		return ctrl.Result{}, fmt.Errorf("update HypervisorConfig failure status: %w", err)
	}
	return ctrl.Result{}, nil
}

// recordFailureError records a dependency failure on the config status and
// returns the reconcile error, preserving the underlying error so callers can
// match it with errors.Is.
func (r *HypervisorConfigReconciler) recordFailureError(
	ctx context.Context,
	cfg *bootstrapv1alpha1.HypervisorConfig,
	reason string,
	err error,
) (ctrl.Result, error) {
	if _, statusErr := r.recordFailure(ctx, cfg, reason, err.Error()); statusErr != nil {
		return ctrl.Result{}, errors.Join(err, fmt.Errorf("record config failure status: %w", statusErr))
	}
	return ctrl.Result{}, err
}

// owningMachine resolves the CAPI Machine that owns the config, through the
// owner reference of Kind Machine in the cluster-api group. A config with no
// owning Machine resolves to nil.
func (r *HypervisorConfigReconciler) owningMachine(
	ctx context.Context,
	cfg *bootstrapv1alpha1.HypervisorConfig,
) (*clusterv1.Machine, error) {
	for _, ref := range cfg.OwnerReferences {
		if ref.Kind != "Machine" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != clusterv1.GroupVersion.Group {
			continue
		}
		machine := &clusterv1.Machine{}
		if err := r.Get(ctx, client.ObjectKey{Namespace: cfg.Namespace, Name: ref.Name}, machine); err != nil {
			if apierrors.IsNotFound(err) {
				return nil, nil
			}
			return nil, fmt.Errorf("get owner Machine %q: %w", ref.Name, err)
		}
		return machine, nil
	}
	return nil, nil
}

// linkedCluster resolves the CAPI Cluster the owning Machine belongs to,
// through machine.spec.clusterName in the machine's namespace. A missing
// Cluster resolves to nil.
func (r *HypervisorConfigReconciler) linkedCluster(
	ctx context.Context,
	machine *clusterv1.Machine,
) (*clusterv1.Cluster, error) {
	if machine.Spec.ClusterName == "" {
		return nil, nil
	}
	cluster := &clusterv1.Cluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.ClusterName}, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get linked Cluster %q: %w", machine.Spec.ClusterName, err)
	}
	return cluster, nil
}

// controlPlaneAddress resolves the API server address the kubeconfigs and the
// apiserver certificate SAN are built for: the linked HypervisorMachine's
// internal IP for a control-plane node, and the linked HypervisorCluster's
// control-plane endpoint for a worker node. A missing address is an error:
// the node cannot be bootstrapped without it.
func (r *HypervisorConfigReconciler) controlPlaneAddress(
	ctx context.Context,
	role string,
	machine *clusterv1.Machine,
	cluster *clusterv1.Cluster,
) (host string, port int32, err error) {
	if role == configRoleControlPlane {
		ip, ok, err := r.machineInternalIP(ctx, machine)
		if err != nil {
			return "", 0, err
		}
		if !ok {
			return "", 0, fmt.Errorf("control-plane machine %q has no internal IP", machine.Name)
		}
		return ip, defaultControlPlanePort, nil
	}

	hc, err := r.linkedHypervisorCluster(ctx, cluster)
	if err != nil {
		return "", 0, err
	}
	if hc == nil || hc.Status.ControlPlaneEndpoint.Host == "" {
		return "", 0, fmt.Errorf("Cluster %q has no control-plane endpoint", cluster.Name)
	}
	port = hc.Status.ControlPlaneEndpoint.Port
	if port == 0 {
		port = defaultControlPlanePort
	}
	return hc.Status.ControlPlaneEndpoint.Host, port, nil
}

// machineInternalIP returns the static internal IP of the HypervisorMachine
// backing the given CAPI Machine, when the machine holds one.
func (r *HypervisorConfigReconciler) machineInternalIP(
	ctx context.Context,
	machine *clusterv1.Machine,
) (string, bool, error) {
	ref := machine.Spec.InfrastructureRef
	if ref.Kind != "HypervisorMachine" || ref.Name == "" {
		return "", false, nil
	}

	// Infrastructure references are namespaced to the machine by CAPI
	// convention; the reference's namespace may be dropped by the API
	// round-trip, so fall back to the machine's own namespace.
	namespace := ref.Namespace
	if namespace == "" {
		namespace = machine.Namespace
	}

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, hm); err != nil {
		if apierrors.IsNotFound(err) {
			return "", false, nil
		}
		return "", false, fmt.Errorf("get infrastructure machine %q: %w", key, err)
	}

	for _, addr := range hm.Status.Addresses {
		if addr.Type == clusterv1.MachineInternalIP && addr.Address != "" {
			return addr.Address, true, nil
		}
	}
	return "", false, nil
}

// linkedHypervisorCluster resolves the HypervisorCluster of the CAPI Cluster
// through its infrastructure reference, or nil when the reference is absent
// or missing.
func (r *HypervisorConfigReconciler) linkedHypervisorCluster(
	ctx context.Context,
	cluster *clusterv1.Cluster,
) (*infrastructurev1alpha1.HypervisorCluster, error) {
	ref := cluster.Spec.InfrastructureRef
	if ref == nil || ref.Kind != "HypervisorCluster" || ref.Name == "" {
		return nil, nil
	}
	namespace := ref.Namespace
	if namespace == "" {
		namespace = cluster.Namespace
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	if err := r.Get(ctx, client.ObjectKey{Namespace: namespace, Name: ref.Name}, hc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get infrastructure cluster %q: %w", ref.Name, err)
	}
	return hc, nil
}

// clusterPKI returns the cluster-scoped PKI for the config's cluster: the
// stored <cluster>-pki Secret when it exists, or a freshly generated PKI
// otherwise. The returned generated flag reports whether the PKI was newly
// generated and still needs persisting.
func (r *HypervisorConfigReconciler) clusterPKI(
	ctx context.Context,
	cfg *bootstrapv1alpha1.HypervisorConfig,
	cpIP, cpName string,
) (pki.ClusterPKI, bool, error) {
	pkiKey := client.ObjectKey{Namespace: cfg.Namespace, Name: cfg.Spec.ClusterName + "-pki"}
	secret := &corev1.Secret{}
	if err := r.Get(ctx, pkiKey, secret); err == nil {
		pk, err := decodeClusterPKI(secret.Data)
		if err != nil {
			return pki.ClusterPKI{}, false, fmt.Errorf("read stored cluster PKI Secret %q: %w", pkiKey, err)
		}
		return pk, false, nil
	} else if !apierrors.IsNotFound(err) {
		return pki.ClusterPKI{}, false, fmt.Errorf("get cluster PKI Secret %q: %w", pkiKey, err)
	}

	pk, err := r.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		return pki.ClusterPKI{}, false, fmt.Errorf("generate cluster PKI: %w", err)
	}
	return pk, true, nil
}

// persistPKISecret stores the cluster PKI in the conventional <cluster>-pki
// Secret whose data keys are exactly the pki.ClusterPKI field names.
func (r *HypervisorConfigReconciler) persistPKISecret(
	ctx context.Context,
	cfg *bootstrapv1alpha1.HypervisorConfig,
	pk pki.ClusterPKI,
) error {
	secret := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: cfg.Spec.ClusterName + "-pki", Namespace: cfg.Namespace},
		Data:       clusterPKISecretData(pk),
	}
	if err := r.Create(ctx, secret); err != nil {
		return fmt.Errorf("create cluster PKI Secret %q: %w", client.ObjectKeyFromObject(secret), err)
	}
	return nil
}

// renderKubeconfigs renders the kubeconfigs for the node through the injected
// renderer: the kubelet kubeconfig for every node, plus the admin,
// controller-manager, and scheduler kubeconfigs for a control-plane node. The
// server URL embeds the resolved control-plane address and the users are the
// KTHW user names. The cluster PKI ships no dedicated admin or component
// client certificate, so the control-plane kubeconfigs reuse the cluster CA
// keypair; the exact client material is deliberately unpinned by the contract.
func (r *HypervisorConfigReconciler) renderKubeconfigs(
	role, serverURL, nodeName string,
	pk pki.ClusterPKI,
	kubeletCert, kubeletKey []byte,
) (map[string][]byte, error) {
	render := func(user string, clientCert, clientKey []byte) ([]byte, error) {
		out, err := r.RenderKubeconfig(pk.CA, serverURL, user, clientCert, clientKey)
		if err != nil {
			return nil, fmt.Errorf("render %s kubeconfig: %w", user, err)
		}
		return out, nil
	}

	kubeconfigs := make(map[string][]byte, 4)
	kubelet, err := render("system:node:"+nodeName, kubeletCert, kubeletKey)
	if err != nil {
		return nil, err
	}
	kubeconfigs["kubelet"] = kubelet

	if role != configRoleControlPlane {
		return kubeconfigs, nil
	}

	for _, kc := range []struct {
		name string
		user string
	}{
		{"admin", "admin"},
		{"controller-manager", "system:kube-controller-manager"},
		{"scheduler", "system:kube-scheduler"},
	} {
		out, err := render(kc.user, pk.CA, pk.CAKey)
		if err != nil {
			return nil, err
		}
		kubeconfigs[kc.name] = out
	}

	return kubeconfigs, nil
}

// persistDataSecret writes the tree.json blob as the single data key of the
// Secret named name, creating it when absent and updating it in place when it
// already exists so repeated reconciles never grow the Secret set.
func (r *HypervisorConfigReconciler) persistDataSecret(
	ctx context.Context,
	cfg *bootstrapv1alpha1.HypervisorConfig,
	name string,
	blob []byte,
) error {
	key := client.ObjectKey{Namespace: cfg.Namespace, Name: name}
	secret := &corev1.Secret{}
	err := r.Get(ctx, key, secret)
	switch {
	case apierrors.IsNotFound(err):
		secret = &corev1.Secret{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: cfg.Namespace},
			Data:       map[string][]byte{configTreeBlobKey: blob},
		}
		if err := r.Create(ctx, secret); err != nil {
			return fmt.Errorf("create data Secret %q: %w", key, err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("get data Secret %q: %w", key, err)
	}

	secret.Data = map[string][]byte{configTreeBlobKey: blob}
	if err := r.Update(ctx, secret); err != nil {
		return fmt.Errorf("update data Secret %q: %w", key, err)
	}
	return nil
}

// encodeTreeBlob encodes the rendered tree as the tree.json blob: a JSON
// object mapping every tree path to its base64-encoded content.
func encodeTreeBlob(tree map[string][]byte) ([]byte, error) {
	encoded := make(map[string]string, len(tree))
	for path, content := range tree {
		encoded[path] = base64.StdEncoding.EncodeToString(content)
	}
	blob, err := json.Marshal(encoded)
	if err != nil {
		return nil, fmt.Errorf("marshal tree blob: %w", err)
	}
	return blob, nil
}

// clusterPKISecretData maps the pki.ClusterPKI exported fields to the data
// keys of the conventional cluster PKI Secret.
func clusterPKISecretData(pk pki.ClusterPKI) map[string][]byte {
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

// decodeClusterPKI reconstructs the pki.ClusterPKI from a cluster PKI Secret
// whose data keys are the pki.ClusterPKI field names. A missing field is an
// error: the stored Secret cannot be trusted.
func decodeClusterPKI(data map[string][]byte) (pki.ClusterPKI, error) {
	pk := pki.ClusterPKI{}
	fields := []struct {
		key string
		dst *[]byte
	}{
		{"CA", &pk.CA},
		{"CAKey", &pk.CAKey},
		{"FrontProxyCA", &pk.FrontProxyCA},
		{"FrontProxyCAKey", &pk.FrontProxyCAKey},
		{"APIServer", &pk.APIServer},
		{"APIServerKey", &pk.APIServerKey},
		{"FrontProxy", &pk.FrontProxy},
		{"FrontProxyKey", &pk.FrontProxyKey},
		{"ServiceAccount", &pk.ServiceAccount},
		{"ServiceAccountKey", &pk.ServiceAccountKey},
	}
	var missing []string
	for _, field := range fields {
		value, ok := data[field.key]
		if !ok {
			missing = append(missing, field.key)
			continue
		}
		*field.dst = value
	}
	if len(missing) > 0 {
		return pki.ClusterPKI{}, fmt.Errorf("cluster PKI Secret missing data keys %v", missing)
	}
	return pk, nil
}

// markDataSecretAvailable upserts the DataSecretAvailable condition on the
// config status with the given status, reason, and message.
func markDataSecretAvailable(
	cfg *bootstrapv1alpha1.HypervisorConfig,
	status corev1.ConditionStatus,
	reason, message string,
) {
	for i := range cfg.Status.Conditions {
		if cfg.Status.Conditions[i].Type != dataSecretAvailableCondition {
			continue
		}
		if cfg.Status.Conditions[i].Status == status && cfg.Status.Conditions[i].Reason == reason {
			cfg.Status.Conditions[i].Message = message
			return
		}
		cfg.Status.Conditions[i].Status = status
		cfg.Status.Conditions[i].Reason = reason
		cfg.Status.Conditions[i].Message = message
		cfg.Status.Conditions[i].LastTransitionTime = metav1.Now()
		return
	}
	cfg.Status.Conditions = append(cfg.Status.Conditions, clusterv1.Condition{
		Type:               dataSecretAvailableCondition,
		Status:             status,
		Reason:             reason,
		Message:            message,
		LastTransitionTime: metav1.Now(),
	})
}

// SetupWithManager sets up the controller with the Manager, watching the
// primary HypervisorConfig kind and the CAPI Machine objects that drive
// config reconciles. The optional controller options are applied to the
// underlying controller in order, so a caller can tune e.g. the maximum
// concurrent reconciles.
func (r *HypervisorConfigReconciler) SetupWithManager(mgr ctrl.Manager, opts ...controller.Options) error {
	log := ctrl.Log.WithName("hypervisorconfig-controller")

	builder := ctrl.NewControllerManagedBy(mgr)
	for _, options := range opts {
		builder = builder.WithOptions(options)
	}

	return builder.
		For(&bootstrapv1alpha1.HypervisorConfig{}).
		WithEventFilter(predicates.ResourceNotPausedAndHasFilterLabel(mgr.GetScheme(), log, "")).
		Watches(
			&clusterv1.Machine{},
			handler.EnqueueRequestsFromMapFunc(r.machineToHypervisorConfig),
		).
		Complete(r)
}

// machineToHypervisorConfig maps a CAPI Machine event to the configs it
// bootstraps: the bootstrap ConfigRef when it names a HypervisorConfig, and
// the configs that carry the Machine as their owner reference as a fallback.
func (r *HypervisorConfigReconciler) machineToHypervisorConfig(
	ctx context.Context,
	obj client.Object,
) []reconcile.Request {
	machine, ok := obj.(*clusterv1.Machine)
	if !ok {
		return nil
	}

	if ref := machine.Spec.Bootstrap.ConfigRef; ref != nil && ref.Kind == "HypervisorConfig" && ref.Name != "" {
		namespace := ref.Namespace
		if namespace == "" {
			namespace = machine.Namespace
		}
		return []reconcile.Request{{NamespacedName: client.ObjectKey{Namespace: namespace, Name: ref.Name}}}
	}

	configs := &bootstrapv1alpha1.HypervisorConfigList{}
	if err := r.List(ctx, configs, client.InNamespace(machine.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for i := range configs.Items {
		for _, ref := range configs.Items[i].OwnerReferences {
			if ref.Kind == "Machine" && ref.Name == machine.Name {
				requests = append(requests, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(&configs.Items[i])})
				break
			}
		}
	}
	return requests
}
