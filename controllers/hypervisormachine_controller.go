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
	"os"
	"path/filepath"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	"github.com/go-logr/logr"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ipam"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/networking"
)

// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=infrastructure.cluster.x-k8s.io,resources=hypervisormachines/finalizers,verbs=update
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=machines,verbs=get;list;watch
// +kubebuilder:rbac:groups=cluster.x-k8s.io,resources=clusters,verbs=get;list;watch
// +kubebuilder:rbac:groups=bootstrap.cluster.x-k8s.io,resources=hypervisorconfigs,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch

const (
	// confextTreeKey is the bootstrap Secret data key that carries the rendered
	// confext tree: a JSON object mapping each slash-separated tree path to its
	// base64-encoded content. Kubernetes Secret data keys cannot contain "/", so
	// the tree paths cannot be stored as literal Secret keys.
	confextTreeKey = "tree.json"

	// defaultMachineTapPrefix is the TAP name prefix for a machine TAP:
	// <tapPrefix>-<machineName>.
	defaultMachineTapPrefix = "k8s-"

	// vmProvisionedCondition is the condition type reported once the
	// cloud-hypervisor VM backing the machine is provisioned and running.
	vmProvisionedCondition = clusterv1.ConditionType("VMProvisioned")
)

// HypervisorMachineReconciler reconciles a HypervisorMachine object: it
// resolves the owning CAPI Machine and the linked Cluster, ensures the
// machine identity (MAC and static IP), provisions the root disk, packages
// the bootstrap Secret tree into the confext data disk, renders the
// cloud-init CIDATA parts, ensures the machine TAP, boots the VM through the
// cloud-hypervisor client, and reports readiness. Host-side effects run
// behind injectable seams (QemuImg, Confext, RenderCloudInit, NewAllocator,
// DeriveMAC, VM, Net), so the reconcile contract is testable without
// qemu-img, mksquashfs, netlink, or a cloud-hypervisor process.
type HypervisorMachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Config is the provider configuration: BaseImage and VMDiskDir drive
	// the root disk and confext output paths, and SocketDir is the root the
	// per-machine VM socket directory derives from.
	Config config.Config

	// VM drives the machine's cloud-hypervisor VM.
	VM chclient.Client

	// Net orchestrates the machine TAP and the cluster bridge over netlink.
	Net *networking.Manager

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
// desired state. It resolves the object first; a missing object is a no-op.
// When the object is being deleted (the deletion timestamp is set and a
// finalizer still holds it), it tears the machine down instead of
// provisioning it: graceful VM shutdown, process stop, TAP removal, disk
// removal unless the spec retains the disks, static IP release, and finalizer
// removal. On a normal reconcile it resolves the owning CAPI Machine, the
// linked Cluster, and the infrastructure Cluster: a machine with no owning
// Machine, or whose Cluster or infrastructure Cluster is missing, is a no-op,
// not an error, and no dependency is touched. Then it ensures the machine
// identity, provisions the root disk, packages the bootstrap data into
// confext raws, renders the CIDATA parts, and drives the VM lifecycle: the
// machine TAP, the VM boot through the client, the provider ID, the
// provisioning condition, and readiness.
func (r *HypervisorMachineReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := ctrl.LoggerFrom(ctx)

	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := r.Get(ctx, req.NamespacedName, hm); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("get HypervisorMachine %q: %w", req.NamespacedName, err)
	}

	// The deletion branch runs before any owner or cluster resolution: a
	// real CAPI teardown deletes the owning Machine at the same time, so
	// teardown must not depend on the owner link surviving.
	if !hm.DeletionTimestamp.IsZero() && len(hm.Finalizers) > 0 {
		return r.reconcileDelete(ctx, log, hm)
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

	if err := r.reconcileVMLifecycle(ctx, hm, machine, hc); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// reconcileDelete tears the machine's host-side stack down and drops its
// finalizers so the object is reclaimed. The VM is shut down gracefully
// through the client and the cloud-hypervisor process is stopped, in that
// order; a VM that is already absent (the client reports ErrNotFound from
// Shutdown or Stop) is tolerated. The machine TAP is deleted, and the root
// disk and the confext data disk artifacts are removed from the VM disk
// directory unless spec.retainDiskOnDelete keeps them in place. The static IP
// the machine holds is released back to the cluster pool. Every step is
// idempotent, so a repeated delete reconcile adds no further host calls.
func (r *HypervisorMachineReconciler) reconcileDelete(
	ctx context.Context,
	log logr.Logger,
	hm *infrastructurev1alpha1.HypervisorMachine,
) (ctrl.Result, error) {
	if err := r.VM.Shutdown(ctx); err != nil && !errors.Is(err, chclient.ErrNotFound) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to shut down VM for %q: %v", hm.Name, err)
		return ctrl.Result{}, fmt.Errorf("shut down VM for %q: %w", hm.Name, err)
	}
	if err := r.VM.Stop(ctx); err != nil && !errors.Is(err, chclient.ErrNotFound) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to stop VM for %q: %v", hm.Name, err)
		return ctrl.Result{}, fmt.Errorf("stop VM for %q: %w", hm.Name, err)
	}

	tapName := defaultMachineTapPrefix + hm.Name
	if err := r.Net.DeleteTap(tapName); err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to delete TAP %q: %v", tapName, err)
		return ctrl.Result{}, fmt.Errorf("delete TAP %q: %w", tapName, err)
	}

	if !hm.Spec.RetainDiskOnDelete {
		if err := removeMachineDisks(r.Config.VMDiskDir, hm.Name); err != nil {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to remove machine disks: %v", err)
			return ctrl.Result{}, fmt.Errorf("remove disks for %q: %w", hm.Name, err)
		}
	}

	if err := r.releaseMachineIP(ctx, log, hm); err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to release static IP: %v", err)
		return ctrl.Result{}, fmt.Errorf("release static IP for %q: %w", hm.Name, err)
	}

	hm.Finalizers = nil
	if err := r.Update(ctx, hm); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizers from HypervisorMachine %q: %w", hm.Name, err)
	}

	return ctrl.Result{}, nil
}

// releaseMachineIP frees the static IP the machine holds back to the cluster
// pool. The per-reconcile allocator is constructed from the linked cluster's
// network config, seeded from the addresses recorded in the status of the
// cluster's surviving machines, and the machine's address is released, so the
// freed address is the next one handed out. The release is best-effort: when
// the owning Machine or the linked Cluster is already gone (as during a real
// CAPI teardown) the address is freed automatically once the object is
// reclaimed, because the fresh allocator of the next reconcile seeds only
// from surviving machines.
func (r *HypervisorMachineReconciler) releaseMachineIP(
	ctx context.Context,
	log logr.Logger,
	hm *infrastructurev1alpha1.HypervisorMachine,
) error {
	machine, ok, err := r.getOwnerMachine(ctx, hm)
	if err != nil {
		return err
	}
	if !ok {
		log.Info("no owning Machine, skipping static IP release during teardown")
		return nil
	}

	cluster, err := r.getLinkedCluster(ctx, machine)
	if err != nil {
		return err
	}
	if cluster == nil {
		log.Info("linked Cluster not found, skipping static IP release during teardown")
		return nil
	}

	hc, err := r.getLinkedHypervisorCluster(ctx, cluster)
	if err != nil {
		return err
	}
	if hc == nil {
		log.Info("linked HypervisorCluster not found, skipping static IP release during teardown")
		return nil
	}

	allocator, err := r.NewAllocator(hc.Spec.Network.CIDR, hc.Spec.Network.Gateway, defaultPoolStart, defaultPoolEnd)
	if err != nil {
		return fmt.Errorf("construct ipam allocator: %w", err)
	}

	if err := r.reassertClusterAddresses(ctx, allocator, machine); err != nil {
		return err
	}

	allocator.Release(client.ObjectKeyFromObject(hm).String())

	return nil
}

// removeMachineDisks removes the machine's root disk and the confext data
// disk artifacts — the packaged .raw output directory and the staging tree —
// from the VM disk directory. Missing artifacts are tolerated so teardown is
// idempotent.
func removeMachineDisks(vmDisksDir, name string) error {
	rootDisk := filepath.Join(vmDisksDir, name+"-root.qcow2")
	if err := os.Remove(rootDisk); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove root disk %q: %w", rootDisk, err)
	}

	dataDir := filepath.Join(vmDisksDir, name+"-data")
	if err := os.RemoveAll(dataDir); err != nil {
		return fmt.Errorf("remove confext data dir %q: %w", dataDir, err)
	}

	stagingDir := filepath.Join(vmDisksDir, name+"-confext-staging")
	if err := os.RemoveAll(stagingDir); err != nil {
		return fmt.Errorf("remove confext staging dir %q: %w", stagingDir, err)
	}

	return nil
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

// reconcileVMLifecycle drives the machine's VM (reconcile steps 6-8). On the
// reconcile that first provisions the machine (no provider ID yet) it ensures
// the machine TAP enslaved to the cluster bridge, boots the VM through the
// injected client, and records the provider ID; on every reconcile it asks
// the client for the VM state and reports the VMProvisioned condition and
// readiness once the VM reports running. A VM that is not running yet, or
// whose state query fails, is left not ready without error.
func (r *HypervisorMachineReconciler) reconcileVMLifecycle(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
	hc *infrastructurev1alpha1.HypervisorCluster,
) error {
	if hm.Status.ProviderID == nil {
		tapName := defaultMachineTapPrefix + hm.Name
		if err := r.Net.EnsureTap(effectiveBridgeName(&hc.Spec.Network), tapName); err != nil {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to ensure TAP %q: %v", tapName, err)
			return fmt.Errorf("ensure TAP %q: %w", tapName, err)
		}

		if err := r.VM.EnsureRunning(ctx); err != nil {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to boot VM for %q: %v", machine.Name, err)
			return fmt.Errorf("boot VM for %q: %w", machine.Name, err)
		}

		providerID := fmt.Sprintf("hypervisor://%s/%s", hm.Spec.ClusterName, hm.Name)
		hm.Status.ProviderID = &providerID
	}

	state, err := r.VM.Info(ctx)
	if err == nil && state == ch.VMState("Running") {
		markVMProvisioned(hm)
		hm.Status.Ready = true
	}

	if err := r.Status().Update(ctx, hm); err != nil {
		return fmt.Errorf("update HypervisorMachine status: %w", err)
	}

	return nil
}

// markVMProvisioned upserts the VMProvisioned condition as true on the
// machine status, preserving any other conditions.
func markVMProvisioned(hm *infrastructurev1alpha1.HypervisorMachine) {
	for i := range hm.Status.Conditions {
		if hm.Status.Conditions[i].Type != vmProvisionedCondition {
			continue
		}
		if hm.Status.Conditions[i].Status == corev1.ConditionTrue {
			return
		}
		hm.Status.Conditions[i].Status = corev1.ConditionTrue
		hm.Status.Conditions[i].LastTransitionTime = metav1.Now()
		return
	}

	hm.Status.Conditions = append(hm.Status.Conditions, clusterv1.Condition{
		Type:               vmProvisionedCondition,
		Status:             corev1.ConditionTrue,
		LastTransitionTime: metav1.Now(),
	})
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
// primary HypervisorMachine kind. The optional controller options are applied
// to the underlying controller in order, so a caller can tune e.g. the
// maximum concurrent reconciles.
func (r *HypervisorMachineReconciler) SetupWithManager(mgr ctrl.Manager, opts ...controller.Options) error {
	builder := ctrl.NewControllerManagedBy(mgr)
	for _, options := range opts {
		builder = builder.WithOptions(options)
	}

	return builder.
		For(&infrastructurev1alpha1.HypervisorMachine{}).
		Complete(r)
}
