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
	"fmt"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ipam"
)

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

// confextTreeKey is the bootstrap Secret data key that carries the rendered
// confext tree: a JSON object mapping each slash-separated tree path to its
// base64-encoded content. Kubernetes Secret data keys cannot contain "/", so
// the tree paths cannot be stored as literal Secret keys.
const confextTreeKey = "tree.json"

// HypervisorMachineReconciler reconciles a HypervisorMachine object: it
// resolves the owning CAPI Machine and the linked Cluster, ensures the
// machine identity (MAC and static IP), provisions the root disk, packages
// the bootstrap Secret tree into the confext data disk, and renders the
// cloud-init CIDATA parts. Host-side effects run behind injectable seams
// (QemuImg, Confext, RenderCloudInit, NewAllocator, DeriveMAC, VM), so the
// reconcile contract is testable without qemu-img, mksquashfs, or a
// cloud-hypervisor process.
type HypervisorMachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Config is the provider configuration: BaseImage and VMDiskDir drive
	// the root disk and confext output paths.
	Config config.Config

	// VM drives the machine's cloud-hypervisor VM.
	VM chclient.Client

	// QemuImg executes the qemu-img binary: Run(ctx, name, args...).
	QemuImg func(ctx context.Context, name string, args ...string) ([]byte, error)

	// Confext packages the bootstrap Secret tree into .raw images.
	Confext *confext.Packager

	// RenderCloudInit renders the three CIDATA parts for one machine.
	RenderCloudInit func(data cloudinit.Data) (map[string][]byte, error)

	// NewAllocator constructs the per-cluster static IP allocator.
	NewAllocator func(clusterCIDR, gateway, poolStart, poolEnd string) (*ipam.Allocator, error)

	// DeriveMAC derives the default machine MAC address.
	DeriveMAC func(clusterName, machineName string) string
}

// Reconcile moves the current state of the HypervisorMachine towards the
// desired state. It resolves the object, the owning CAPI Machine, and the
// linked Cluster first: a missing object, a machine with no owning Machine,
// and a machine whose Cluster or infrastructure Cluster is missing are all
// no-ops, not errors, and no dependency is touched. On a normal reconcile it
// ensures the machine identity, provisions the root disk, packages the
// bootstrap data into confext raws, and renders the CIDATA parts. The VM
// lifecycle and deletion steps are out of this phase's scope.
func (r *HypervisorMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := r.Get(ctx, req.NamespacedName, hm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get HypervisorMachine %q: %w", req.NamespacedName, err)
	}

	machine, ok, err := r.getOwnerMachine(ctx, hm)
	if err != nil {
		return ctrl.Result{}, err
	}
	if !ok {
		log.Info("no owning Machine, waiting for the owner link")
		return ctrl.Result{}, nil
	}

	cluster, err := r.getLinkedCluster(ctx, machine)
	if err != nil {
		return ctrl.Result{}, err
	}
	if cluster == nil {
		log.Info("linked Cluster not found, waiting for the Cluster link")
		return ctrl.Result{}, nil
	}

	hc, err := r.getLinkedHypervisorCluster(ctx, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hc == nil {
		log.Info("linked HypervisorCluster not found, waiting for the infrastructure link")
		return ctrl.Result{}, nil
	}

	ip, err := r.reconcileIdentity(ctx, hm, machine, hc)
	if err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRootDisk(ctx, hm); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileConfextDataDisk(ctx, hm, machine); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileCIDATA(ctx, hm, machine, hc, ip); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// getOwnerMachine resolves the CAPI Machine that owns hm through the owner
// references. A machine without an owning Machine is reported as (nil, false,
// nil): the controller waits for the owner link instead of erroring.
func (r *HypervisorMachineReconciler) getOwnerMachine(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
) (*clusterv1.Machine, bool, error) {
	for _, ref := range hm.OwnerReferences {
		if ref.Kind != "Machine" {
			continue
		}
		gv, err := schema.ParseGroupVersion(ref.APIVersion)
		if err != nil || gv.Group != clusterv1.GroupVersion.Group {
			continue
		}
		machine := &clusterv1.Machine{}
		key := client.ObjectKey{Namespace: hm.Namespace, Name: ref.Name}
		if err := r.Get(ctx, key, machine); err != nil {
			if apierrors.IsNotFound(err) {
				continue
			}
			return nil, false, fmt.Errorf("get owner Machine %q: %w", key, err)
		}
		return machine, true, nil
	}

	return nil, false, nil
}

// getLinkedCluster resolves the CAPI Cluster of the owning Machine through
// its clusterName link. A missing Cluster is reported as (nil, nil): the
// controller waits for the link instead of erroring.
func (r *HypervisorMachineReconciler) getLinkedCluster(
	ctx context.Context,
	machine *clusterv1.Machine,
) (*clusterv1.Cluster, error) {
	if machine.Spec.ClusterName == "" {
		return nil, nil
	}
	cluster := &clusterv1.Cluster{}
	key := client.ObjectKey{Namespace: machine.Namespace, Name: machine.Spec.ClusterName}
	if err := r.Get(ctx, key, cluster); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get linked Cluster %q: %w", key, err)
	}
	return cluster, nil
}

// getLinkedHypervisorCluster resolves the HypervisorCluster the CAPI Cluster
// points at through its infrastructure reference; it carries the network
// config the machine allocates from. A missing infrastructure reference or a
// missing HypervisorCluster is reported as (nil, nil): the controller waits
// for the link instead of erroring.
func (r *HypervisorMachineReconciler) getLinkedHypervisorCluster(
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
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, hc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get linked HypervisorCluster %q: %w", key, err)
	}
	return hc, nil
}

// reconcileIdentity ensures the machine identity: the MAC address (derived
// through the seam when spec.mac is empty, the spec value otherwise) and the
// static IP allocated from the cluster pool. The allocator is constructed
// fresh per reconcile and seeded from the addresses already recorded in the
// cluster's machine status, so a provider restart never hands an address an
// existing machine holds. The allocated IP is recorded in status.addresses
// together with the machine hostname and returned to the caller.
func (r *HypervisorMachineReconciler) reconcileIdentity(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
	hc *infrastructurev1alpha1.HypervisorCluster,
) (string, error) {
	if hm.Spec.MAC == "" {
		// The derived address is consumed by the VM lifecycle step in a
		// later phase; running the derivation here pins the identity input.
		_ = r.DeriveMAC(machine.Spec.ClusterName, machine.Name)
	}

	allocator, err := r.NewAllocator(hc.Spec.Network.CIDR, hc.Spec.Network.Gateway, defaultPoolStart, defaultPoolEnd)
	if err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to construct ipam allocator: %v", err)
		return "", fmt.Errorf("construct ipam allocator: %w", err)
	}

	if err := r.reassertClusterAddresses(ctx, allocator, machine); err != nil {
		return "", err
	}

	key := client.ObjectKeyFromObject(hm).String()
	ip, err := r.machineAddress(allocator, key, hm)
	if err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to allocate static IP: %v", err)
		return "", fmt.Errorf("allocate static IP for %q: %w", machine.Name, err)
	}

	if err := r.recordAddresses(ctx, hm, machine, ip); err != nil {
		return "", err
	}

	return ip, nil
}

// reassertClusterAddresses seeds the fresh per-reconcile allocator with the
// addresses already recorded in the status of the cluster's machines, so a
// new machine never receives an address an existing machine holds.
func (r *HypervisorMachineReconciler) reassertClusterAddresses(
	ctx context.Context,
	allocator *ipam.Allocator,
	machine *clusterv1.Machine,
) error {
	machines := &infrastructurev1alpha1.HypervisorMachineList{}
	if err := r.List(ctx, machines, client.InNamespace(machine.Namespace)); err != nil {
		return fmt.Errorf("list HypervisorMachines: %w", err)
	}
	for i := range machines.Items {
		hm := &machines.Items[i]
		if hm.Spec.ClusterName != machine.Spec.ClusterName {
			continue
		}
		ip := machineInternalIPAddress(hm)
		if ip == "" {
			continue
		}
		key := client.ObjectKeyFromObject(hm).String()
		if err := allocator.Reserve(key, ip); err != nil {
			return fmt.Errorf("re-assert address %q for %q: %w", ip, key, err)
		}
	}
	return nil
}

// machineAddress returns the static IP of hm: the address already recorded in
// status when the machine holds one (re-asserted through Reserve so the key
// keeps it), otherwise the first free address of the pool.
func (r *HypervisorMachineReconciler) machineAddress(
	allocator *ipam.Allocator,
	key string,
	hm *infrastructurev1alpha1.HypervisorMachine,
) (string, error) {
	if ip := machineInternalIPAddress(hm); ip != "" {
		if err := allocator.Reserve(key, ip); err != nil {
			return "", fmt.Errorf("re-assert address %q for %q: %w", ip, key, err)
		}
		return ip, nil
	}
	return allocator.Allocate(key)
}

// recordAddresses writes the allocated static IP and the machine hostname
// into status.addresses.
func (r *HypervisorMachineReconciler) recordAddresses(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
	ip string,
) error {
	hm.Status.Addresses = []clusterv1.MachineAddress{
		{Type: clusterv1.MachineInternalIP, Address: ip},
		{Type: clusterv1.MachineHostName, Address: machine.Name},
	}
	if err := r.Status().Update(ctx, hm); err != nil {
		return fmt.Errorf("update HypervisorMachine status: %w", err)
	}
	return nil
}

// machineInternalIPAddress returns the internal IP recorded in the status of
// hm, or the empty string when none is recorded.
func machineInternalIPAddress(hm *infrastructurev1alpha1.HypervisorMachine) string {
	for _, addr := range hm.Status.Addresses {
		if addr.Type == clusterv1.MachineInternalIP {
			return addr.Address
		}
	}
	return ""
}

// reconcileRootDisk ensures <vm-disks>/<name>-root.qcow2 exists at the spec
// size: qemu-img info checks the current disk, and a disk that is absent or
// at the wrong size is converted from the base image and resized.
func (r *HypervisorMachineReconciler) reconcileRootDisk(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
) error {
	diskPath := filepath.Join(r.Config.VMDiskDir, hm.Name+"-root.qcow2")

	wantSize := int64(hm.Spec.Disk) * 1024 * 1024
	size, err := r.rootDiskSize(ctx, diskPath)
	if err == nil && size == wantSize {
		return nil
	}

	if _, err := r.QemuImg(ctx, "qemu-img", "convert", "-O", "qcow2", r.Config.BaseImage, diskPath); err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to convert root disk %q: %v", diskPath, err)
		return fmt.Errorf("convert root disk %q: %w", diskPath, err)
	}
	if _, err := r.QemuImg(ctx, "qemu-img", "resize", diskPath, fmt.Sprintf("%dM", hm.Spec.Disk)); err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to resize root disk %q: %v", diskPath, err)
		return fmt.Errorf("resize root disk %q: %w", diskPath, err)
	}

	return nil
}

// rootDiskSize reports the virtual size in bytes of the disk at diskPath,
// from qemu-img info. An absent or unreadable disk is reported as an error.
func (r *HypervisorMachineReconciler) rootDiskSize(ctx context.Context, diskPath string) (int64, error) {
	out, err := r.QemuImg(ctx, "qemu-img", "info", diskPath)
	if err != nil {
		return 0, err
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("parse qemu-img info for %q: %w", diskPath, err)
	}
	return info.VirtualSize, nil
}

// reconcileConfextDataDisk reads the bootstrap Secret named by the linked
// bootstrap config's status, decodes the tree.json blob into the confext
// tree, materializes the tree through the confext packager, and packages each
// confext into a .raw squashfs image under the configured VM disk directory.
// A machine with no bootstrap data skips packaging without error.
func (r *HypervisorMachineReconciler) reconcileConfextDataDisk(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
) error {
	secretName, err := r.bootstrapDataSecretName(ctx, machine)
	if err != nil || secretName == "" {
		return err
	}

	secret := &corev1.Secret{}
	secretKey := client.ObjectKey{Namespace: machine.Namespace, Name: secretName}
	if err := r.Get(ctx, secretKey, secret); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get bootstrap Secret %q: %w", secretKey, err)
	}

	tree, err := decodeConfextTree(secret)
	if err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to decode confext tree: %v", err)
		return fmt.Errorf("decode confext tree for %q: %w", machine.Name, err)
	}
	if len(tree) == 0 {
		return nil
	}

	stagingDir := filepath.Join(r.Config.VMDiskDir, machine.Name+"-confext-staging")
	outDir := filepath.Join(r.Config.VMDiskDir, machine.Name+"-data")

	if err := r.Confext.WriteTree(tree, stagingDir); err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to materialize confext tree: %v", err)
		return fmt.Errorf("materialize confext tree for %q: %w", machine.Name, err)
	}
	if _, err := r.Confext.BuildRaws(ctx, stagingDir, outDir); err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to build confext raws: %v", err)
		return fmt.Errorf("build confext raws for %q: %w", machine.Name, err)
	}

	return nil
}

// decodeConfextTree decodes the tree.json blob of the bootstrap Secret into
// the path-to-content map the confext packager consumes: every slash-separated
// tree path maps to the base64-decoded file content. A Secret without the
// tree.json key yields an empty tree — the machine has no bootstrap data.
// A malformed blob (invalid JSON or a non-base64 value) is surfaced as an
// error.
func decodeConfextTree(secret *corev1.Secret) (map[string][]byte, error) {
	blob, ok := secret.Data[confextTreeKey]
	if !ok {
		return nil, nil
	}

	var encoded map[string]string
	if err := json.Unmarshal(blob, &encoded); err != nil {
		return nil, fmt.Errorf("unmarshal %s: %w", confextTreeKey, err)
	}

	tree := make(map[string][]byte, len(encoded))
	for path, content := range encoded {
		decoded, err := base64.StdEncoding.DecodeString(content)
		if err != nil {
			return nil, fmt.Errorf("decode tree path %q: %w", path, err)
		}
		tree[path] = decoded
	}

	return tree, nil
}

// reconcileCIDATA renders the three cloud-init parts for the machine through
// the injected renderer with the allocated IP, the machine hostname, the
// cluster gateway and DNS, and the SSH public key of the linked bootstrap
// config. A machine with no linked bootstrap config has no key to inject and
// skips rendering without error.
func (r *HypervisorMachineReconciler) reconcileCIDATA(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
	hc *infrastructurev1alpha1.HypervisorCluster,
	ip string,
) error {
	ref := machine.Spec.Bootstrap.ConfigRef
	if ref == nil || ref.Kind != "HypervisorConfig" || ref.Name == "" {
		return nil
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = machine.Namespace
	}
	config := &bootstrapv1alpha1.HypervisorConfig{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get bootstrap config %q: %w", key, err)
	}

	parts, err := r.RenderCloudInit(cloudinit.Data{
		InstanceID:   machine.Name,
		Hostname:     machine.Name,
		SSHPublicKey: config.Spec.SSHPublicKey,
		IP:           ip,
		Gateway:      hc.Spec.Network.Gateway,
		DNS:          hc.Spec.Network.DNSIP,
	})
	if err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to render cloud-init data: %v", err)
		return fmt.Errorf("render cloud-init data for %q: %w", machine.Name, err)
	}
	// The rendered parts feed the CIDATA disk of the VM boot step in a later
	// phase; the render itself is the identity contract pinned here.
	_ = parts

	return nil
}

// bootstrapDataSecretName resolves the name of the bootstrap Secret of the
// machine: the owning Machine's bootstrap config reference names a
// HypervisorConfig, and the config's status carries the rendered Secret name.
// A machine with no bootstrap data reports an empty name without error.
func (r *HypervisorMachineReconciler) bootstrapDataSecretName(
	ctx context.Context,
	machine *clusterv1.Machine,
) (string, error) {
	ref := machine.Spec.Bootstrap.ConfigRef
	if ref == nil || ref.Kind != "HypervisorConfig" || ref.Name == "" {
		return "", nil
	}

	namespace := ref.Namespace
	if namespace == "" {
		namespace = machine.Namespace
	}
	config := &bootstrapv1alpha1.HypervisorConfig{}
	key := client.ObjectKey{Namespace: namespace, Name: ref.Name}
	if err := r.Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			return "", nil
		}
		return "", fmt.Errorf("get bootstrap config %q: %w", key, err)
	}
	if config.Status.DataSecretName == nil {
		return "", nil
	}
	return *config.Status.DataSecretName, nil
}

// SetupWithManager sets up the controller with the Manager, watching the
// primary HypervisorMachine kind.
func (r *HypervisorMachineReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&infrastructurev1alpha1.HypervisorMachine{}).
		Complete(r)
}
