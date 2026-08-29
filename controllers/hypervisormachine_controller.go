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
	"strings"
	"sync"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/tools/record"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/chclient"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/cloudinit"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/config"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
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

	// vmProvisionedCondition is the condition type reported once the
	// cloud-hypervisor VM backing the machine is provisioned and running.
	vmProvisionedCondition = "VMProvisioned"

	// controlPlaneSSHPort is the VM-side SSH port the machine controller
	// publishes for control-plane machines next to the apiserver port
	// (controlPlaneAPIServerPort).
	controlPlaneSSHPort int32 = 22

	// cidataDiskSize is the size of the pre-created CIDATA disk image in
	// bytes (4 MiB, matching the create-cloudinit.sh precedent of 8192
	// 512-byte sectors). The image file must exist before mkdosfs runs:
	// the real binary refuses to format a missing target.
	cidataDiskSize = 4 * 1024 * 1024
)

// HypervisorMachineReconciler reconciles a HypervisorMachine object: it
// resolves the owning CAPI Machine and the linked Cluster, ensures the
// machine identity (MAC and k8netd-allocated IP), provisions the root disk,
// packages the bootstrap Secret tree into the confext data disk, renders the
// cloud-init CIDATA parts (DHCP), and drives the VM lifecycle through the
// cloud-hypervisor client. The k8netd port is created and attached before the
// VM starts, and detached/deleted after the VM stops on deletion.
type HypervisorMachineReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Config is the provider configuration: BaseImage and VMDiskDir drive
	// the root disk and confext output paths, and SocketDir is the root the
	// per-machine VM socket directory derives from (<SocketDir>/<machine>,
	// one socket tree per VM).
	Config config.Config

	// NewVMClient constructs the cloud-hypervisor client bound to one
	// machine's own socket directory: socketDir is <SocketDir>/<machine>
	// (holding that machine's api.sock) and binary is the cloud-hypervisor
	// executable. Production wiring leaves it nil; the reconciler then
	// constructs chclient.NewVMClient(socketDir, binary). The reconciler
	// caches the built client per machine name for the provider process
	// lifetime and calls the factory at most once per machine, so every
	// reconcile reuses one Manager instance and Manager.Start's started-state
	// check prevents respawning the VMM; reconcileDelete evicts the entry
	// after Stop. Tests override it to capture construction and hand back
	// fakes.
	NewVMClient func(socketDir, binary string) chclient.Client

	// K8Netd is the k8netd JSON-RPC client used to create ports and allocate
	// IPs. It is injected from main.go via cfg.K8NetdSocket.
	K8Netd *k8netd.Client

	// QemuImg executes the qemu-img binary: Run(ctx, name, args...).
	QemuImg func(ctx context.Context, name string, args ...string) ([]byte, error)

	// Confext packages the bootstrap Secret tree into .raw images.
	Confext *confext.Packager

	// RenderCloudInit renders the three CIDATA parts for one machine.
	RenderCloudInit func(data cloudinit.Data) (map[string][]byte, error)

	// DeriveMAC derives the default machine MAC address.
	DeriveMAC func(clusterName, machineName string) string

	// vmClients caches one VM client per machine name for the provider
	// process lifetime (map[string]chclient.Client). The cache is what makes
	// VMM spawns idempotent across reconciles: a reused ch.Manager sees its
	// own started state in Start and returns the existing socket instead of
	// respawning the process. Keyed by machine name, matching the per-machine
	// socket directory derivation <SocketDir>/<machine>.
	vmClients sync.Map
}

// Reconcile moves the current state of the HypervisorMachine towards the
// desired state. It resolves the object first; a missing object is a no-op.
// When the object is being deleted (the deletion timestamp is set and a
// finalizer still holds it), it tears the machine down instead of
// provisioning it: graceful VM shutdown, process stop, DetachPort/DeletePort,
// disk removal unless the spec retains the disks, and finalizer removal. On a
// normal reconcile it resolves the owning CAPI Machine, the linked Cluster,
// and the infrastructure Cluster: a machine with no owning Machine, or whose
// Cluster or infrastructure Cluster is missing, is a no-op, not an error, and
// no dependency is touched. Then it ensures the machine identity via k8netd,
// provisions the root disk, packages the bootstrap data into confext raws,
// renders the CIDATA parts (DHCP), and drives the VM lifecycle.
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
		return r.reconcileDelete(ctx, hm)
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

	hc, err := linkedHypervisorCluster(ctx, r.Client, cluster)
	if err != nil {
		return ctrl.Result{}, err
	}
	if hc == nil {
		log.Info("linked HypervisorCluster not found, waiting for the infrastructure link")
		return ctrl.Result{}, nil
	}

	// The MAC is derived once per reconcile and shared by the identity step
	// (k8netd AllocateIP) and the VM lifecycle step (vhost-user net config),
	// so both stages address the same NIC.
	mac := r.effectiveMAC(hm, machine)

	if _, err := r.reconcileIdentity(ctx, hm, machine, hc, mac); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileRootDisk(ctx, hm); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileConfextDataDisk(ctx, hm, machine); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileCIDATA(ctx, hm, machine); err != nil {
		return ctrl.Result{}, err
	}

	if err := r.reconcileVMLifecycle(ctx, hm, machine, mac); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// vmClientFor returns the process-lifetime VM client for hm: the first call
// for a machine name constructs it through the NewVMClient factory seam (or
// the default chclient.NewVMClient) bound to <SocketDir>/<machine-name> and
// caches it; every later call returns that same instance. One socket tree per
// VM keeps concurrent machines off each other's api.sock, and because
// reconciles reuse one Manager instance, Manager.Start's started-state check
// prevents respawning the cloud-hypervisor process on every reconcile.
func (r *HypervisorMachineReconciler) vmClientFor(
	hm *infrastructurev1alpha1.HypervisorMachine,
) chclient.Client {
	if cached, ok := r.vmClients.Load(hm.Name); ok {
		return cached.(chclient.Client)
	}
	socketDir := filepath.Join(r.Config.SocketDir, hm.Name)
	var vm chclient.Client
	if r.NewVMClient != nil {
		vm = r.NewVMClient(socketDir, r.Config.CHBinary)
	} else {
		vm = chclient.NewVMClient(socketDir, r.Config.CHBinary)
	}
	// LoadOrStore closes the concurrent-workers race: when two workers build
	// a client for the same machine at once, the loser's instance is
	// discarded and both callers share the winner's.
	actual, _ := r.vmClients.LoadOrStore(hm.Name, vm)
	return actual.(chclient.Client)
}

// reconcileDelete tears the machine's host-side stack down and drops its
// finalizers so the object is reclaimed. The VM is shut down gracefully
// through the machine's own (cached) client and the cloud-hypervisor process
// is stopped, in that order; a VM that is already absent (the client reports
// ErrNotFound from Shutdown or Stop) is tolerated. Once Stop succeeds, the
// cached client entry is removed so a later re-create of a machine with the
// same name starts from a fresh manager instead of one whose Start refuses to
// run after Stop; a failed Stop keeps the entry so the retry reuses the same
// manager. After the VM stops, the k8netd port is
// detached and deleted in order (both idempotent via NotFound). The root disk
// and the confext data disk artifacts are removed from the VM disk directory
// unless spec.retainDiskOnDelete keeps them in place. Every step is idempotent,
// so a repeated delete reconcile adds no further host calls.
func (r *HypervisorMachineReconciler) reconcileDelete(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
) (ctrl.Result, error) {
	vm := r.vmClientFor(hm)
	if err := vm.Shutdown(ctx); err != nil && !errors.Is(err, chclient.ErrNotFound) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to shut down VM for %q: %v", hm.Name, err)
		return ctrl.Result{}, fmt.Errorf("shut down VM for %q: %w", hm.Name, err)
	}
	if err := vm.Stop(ctx); err != nil && !errors.Is(err, chclient.ErrNotFound) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to stop VM for %q: %v", hm.Name, err)
		return ctrl.Result{}, fmt.Errorf("stop VM for %q: %w", hm.Name, err)
	}

	// Teardown succeeded: drop the cached client so a later re-create of a
	// machine with this name builds a fresh manager.
	r.vmClients.Delete(hm.Name)

	// After VM stop, detach and delete the k8netd port in order. Both are
	// idempotent: NotFound is treated as success.
	portName := hm.Name
	if err := r.K8Netd.DetachPort(ctx, portName); err != nil && !errors.Is(err, k8netd.ErrNotFound) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to detach port %q: %v", portName, err)
		return ctrl.Result{}, fmt.Errorf("detach port %q: %w", portName, err)
	}
	if err := r.K8Netd.DeletePort(ctx, portName); err != nil && !errors.Is(err, k8netd.ErrNotFound) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to delete port %q: %v", portName, err)
		return ctrl.Result{}, fmt.Errorf("delete port %q: %w", portName, err)
	}

	if !hm.Spec.RetainDiskOnDelete {
		if err := removeMachineDisks(r.Config.VMDiskDir, hm.Name); err != nil {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedTeardown", "failed to remove machine disks: %v", err)
			return ctrl.Result{}, fmt.Errorf("remove disks for %q: %w", hm.Name, err)
		}
	}

	hm.Finalizers = nil
	if err := r.Update(ctx, hm); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizers from HypervisorMachine %q: %w", hm.Name, err)
	}

	return ctrl.Result{}, nil
}

// removeMachineDisks removes the machine's root disk, the CIDATA disk and its
// rendered-parts staging tree, and the confext data disk artifacts — the
// packaged .raw output directory and the staging tree — from the VM disk
// directory. Missing artifacts are tolerated so teardown is idempotent.
func removeMachineDisks(vmDisksDir, name string) error {
	rootDisk := filepath.Join(vmDisksDir, name+"-root.qcow2")
	if err := os.Remove(rootDisk); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove root disk %q: %w", rootDisk, err)
	}

	cidataDisk := filepath.Join(vmDisksDir, name+"-cidata.img")
	if err := os.Remove(cidataDisk); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove CIDATA disk %q: %w", cidataDisk, err)
	}

	cidataPartsDir := filepath.Join(vmDisksDir, name+"-cidata-parts")
	if err := os.RemoveAll(cidataPartsDir); err != nil {
		return fmt.Errorf("remove CIDATA parts dir %q: %w", cidataPartsDir, err)
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
// references first, then through the Machine.spec.infrastructureRef
// back-reference: an externally created HypervisorMachine may lack the owner
// reference while the owning Machine still names it. A machine without an
// owning Machine is reported as (nil, false, nil): the controller waits for
// the owner link instead of erroring.
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

	// Reverse lookup: list the Machines of the namespace and return the one
	// whose infrastructureRef names this HypervisorMachine. No match means no
	// owner link yet.
	machines := &clusterv1.MachineList{}
	if err := r.List(ctx, machines, client.InNamespace(hm.Namespace)); err != nil {
		return nil, false, fmt.Errorf("list Machines in %q: %w", hm.Namespace, err)
	}
	for i := range machines.Items {
		ref := machines.Items[i].Spec.InfrastructureRef
		if !ref.IsDefined() || ref.Kind != "HypervisorMachine" || ref.Name != hm.Name {
			continue
		}
		return &machines.Items[i], true, nil
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

// linkedHypervisorCluster resolves the HypervisorCluster the CAPI Cluster
// points at through its infrastructure reference; it carries the network
// config. A missing infrastructure reference or a missing HypervisorCluster is
// reported as (nil, nil): callers wait for the link instead of erroring.
func linkedHypervisorCluster(
	ctx context.Context,
	c client.Client,
	cluster *clusterv1.Cluster,
) (*infrastructurev1alpha1.HypervisorCluster, error) {
	ref := cluster.Spec.InfrastructureRef
	if !ref.IsDefined() || ref.Kind != "HypervisorCluster" || ref.Name == "" {
		return nil, nil
	}
	hc := &infrastructurev1alpha1.HypervisorCluster{}
	key := client.ObjectKey{Namespace: cluster.Namespace, Name: ref.Name}
	if err := c.Get(ctx, key, hc); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("get linked HypervisorCluster %q: %w", key, err)
	}
	return hc, nil
}

// k8netdNetworkName returns the k8netd network name for the cluster. Per
// plan assumption 1, the network name equals the HypervisorCluster name.
func k8netdNetworkName(hc *infrastructurev1alpha1.HypervisorCluster) string {
	return hc.Name
}

// effectiveMAC returns the MAC address for the machine: the spec value when
// set, otherwise the derived address via the seam.
func (r *HypervisorMachineReconciler) effectiveMAC(
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
) string {
	if hm.Spec.MAC != "" {
		return hm.Spec.MAC
	}
	return r.DeriveMAC(machine.Spec.ClusterName, machine.Name)
}

// reconcileIdentity ensures the machine identity: the MAC address (derived
// once per reconcile in Reconcile and passed in) and the k8netd port/IP. It
// creates the port, attaches it to the cluster network, publishes the
// control-plane endpoints for a control-plane machine (REQ-008), and
// allocates an IP for the MAC, in order. AlreadyExists on create/attach is
// treated as success for idempotency. The allocated IP is recorded in
// status.addresses together with the machine hostname and returned; the
// published allocations are recorded on status.publishedPorts in the same
// status update.
func (r *HypervisorMachineReconciler) reconcileIdentity(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
	hc *infrastructurev1alpha1.HypervisorCluster,
	mac string,
) (string, error) {
	network := k8netdNetworkName(hc)
	port := hm.Name

	if err := r.K8Netd.CreatePort(ctx, port); err != nil {
		if errors.Is(err, k8netd.ErrAlreadyExists) {
			// Idempotent: port already exists.
		} else {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to create port %q: %v", port, err)
			return "", fmt.Errorf("create port %q: %w", port, err)
		}
	}

	if err := r.K8Netd.AttachPort(ctx, port, network, mac); err != nil {
		if errors.Is(err, k8netd.ErrAlreadyExists) {
			// Idempotent: already attached.
		} else {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to attach port %q to network %q: %v", port, network, err)
			return "", fmt.Errorf("attach port %q to network %q: %w", port, network, err)
		}
	}

	// With the port attached, a control-plane machine publishes its endpoints
	// through k8netd right away; the recorded allocations ride the status
	// update of recordAddresses below.
	if err := r.publishControlPlaneEndpoints(ctx, hm, machine); err != nil {
		return "", err
	}

	ip, err := r.K8Netd.AllocateIP(ctx, network, mac)
	if err != nil {
		r.Recorder.Eventf(
			hm,
			corev1.EventTypeWarning,
			"FailedProvision",
			"failed to allocate IP for %q: %v",
			machine.Name,
			err,
		)
		return "", fmt.Errorf("allocate IP for %q: %w", machine.Name, err)
	}

	if err := r.recordAddresses(ctx, hm, machine, ip); err != nil {
		return "", err
	}

	return ip, nil
}

// publishControlPlaneEndpoints publishes the control-plane endpoints of a
// control-plane machine through the k8netd PublishPort RPC — the apiserver
// port and SSH — immediately after the machine's port is attached, and
// records the returned host ports on status.publishedPorts. Worker machines
// publish nothing (REQ-008). Publication is idempotent: vmPorts already
// recorded are not re-published, so repeated reconciles issue no duplicate
// allocations; a failed call surfaces as a reconcile error so the existing
// retry path re-runs it.
func (r *HypervisorMachineReconciler) publishControlPlaneEndpoints(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
) error {
	if !isControlPlaneMachine(machine) {
		return nil
	}

	for _, vmPort := range []int32{controlPlaneAPIServerPort, controlPlaneSSHPort} {
		if _, ok := publishedHostPort(hm.Status.PublishedPorts, vmPort); ok {
			continue
		}
		hostPort, err := r.K8Netd.PublishPort(ctx, hm.Name, vmPort)
		if err != nil {
			r.Recorder.Eventf(
				hm,
				corev1.EventTypeWarning,
				"FailedProvision",
				"failed to publish vm_port %d for %q: %v",
				vmPort,
				hm.Name,
				err,
			)
			return fmt.Errorf("publish vm_port %d for %q: %w", vmPort, hm.Name, err)
		}
		hm.Status.PublishedPorts = upsertPublishedPort(hm.Status.PublishedPorts, infrastructurev1alpha1.MachinePublishedPort{
			VMPort:   vmPort,
			HostPort: hostPort,
		})
	}

	return nil
}

// isControlPlaneMachine reports whether the owning CAPI Machine carries the
// control-plane role label — the signal the control-plane controller labels
// its Machines with.
func isControlPlaneMachine(machine *clusterv1.Machine) bool {
	_, ok := machine.Labels[clusterv1.MachineControlPlaneLabel]
	return ok
}

// publishedHostPort returns the recorded host port for vmPort in ports, and
// whether such an entry exists.
func publishedHostPort(ports []infrastructurev1alpha1.MachinePublishedPort, vmPort int32) (int32, bool) {
	for _, p := range ports {
		if p.VMPort == vmPort {
			return p.HostPort, true
		}
	}
	return 0, false
}

// upsertPublishedPort records allocation in ports, replacing an existing
// entry for the same VM-side port.
func upsertPublishedPort(
	ports []infrastructurev1alpha1.MachinePublishedPort,
	allocation infrastructurev1alpha1.MachinePublishedPort,
) []infrastructurev1alpha1.MachinePublishedPort {
	for i := range ports {
		if ports[i].VMPort == allocation.VMPort {
			ports[i] = allocation
			return ports
		}
	}
	return append(ports, allocation)
}

// recordAddresses writes the allocated IP and the machine hostname into
// status.addresses.
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

// reconcileRootDisk ensures <vm-disks>/<name>-root.qcow2 exists at the spec
// size: qemu-img info checks the current disk, and a disk that is absent or
// at the wrong size is converted from the base image and resized. The size
// probe runs with -U (force-share) so a disk locked by a running VM still
// reports its size, and a convert or resize failure wraps the qemu-img
// stderr (CombinedOutput) so the operator sees why the disk could not be
// provisioned.
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
	if errors.Is(err, errQemuImgInfoParse) {
		return fmt.Errorf("root disk size probe for %q: %w", diskPath, err)
	}

	out, err := r.QemuImg(ctx, "qemu-img", "convert", "-O", "qcow2", r.Config.BaseImage, diskPath)
	if err != nil {
		err = wrapQemuImgErr(err, out)
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to convert root disk %q: %v", diskPath, err)
		return fmt.Errorf("convert root disk %q: %w", diskPath, err)
	}
	out, err = r.QemuImg(ctx, "qemu-img", "resize", diskPath, fmt.Sprintf("%dM", hm.Spec.Disk))
	if err != nil {
		err = wrapQemuImgErr(err, out)
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to resize root disk %q: %v", diskPath, err)
		return fmt.Errorf("resize root disk %q: %w", diskPath, err)
	}

	return nil
}

// wrapQemuImgErr appends the qemu-img CombinedOutput (which carries stderr)
// to the exec error, so a failure like a locked disk or a full device names
// the underlying cause instead of only "exit status 1".
func wrapQemuImgErr(err error, out []byte) error {
	if msg := strings.TrimSpace(string(out)); msg != "" {
		return fmt.Errorf("%w: %s", err, msg)
	}
	return err
}

// errQemuImgInfoParse marks a qemu-img info probe whose output could not be
// parsed as JSON. Unlike an absent or unreadable disk — an exec error that
// means the disk needs converting — malformed info output cannot report a
// size, so the parse error surfaces instead of silently converting a possibly
// correct disk.
var errQemuImgInfoParse = errors.New("parse qemu-img info")

// rootDiskSize reports the virtual size in bytes of the disk at diskPath,
// from qemu-img info. The probe runs with -U (force-share) so a disk locked
// by a running VM still reports its size, and --output=json so the size is
// machine-readable. An absent or unreadable disk is reported as an error; a
// probe that succeeds but returns output that is not valid JSON is reported
// as a parse error (errQemuImgInfoParse).
func (r *HypervisorMachineReconciler) rootDiskSize(ctx context.Context, diskPath string) (int64, error) {
	out, err := r.QemuImg(ctx, "qemu-img", "info", "-U", "--output=json", diskPath)
	if err != nil {
		return 0, err
	}
	var info struct {
		VirtualSize int64 `json:"virtual-size"`
	}
	if err := json.Unmarshal(out, &info); err != nil {
		return 0, fmt.Errorf("%w for %q: %v", errQemuImgInfoParse, diskPath, err)
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

// resolveSSHPublicKey resolves the SSH public key for one machine's CIDATA
// render: the bootstrap config's spec.sshPublicKey first, then — when that is
// empty and HYPERVISOR_SSH_PUBLIC_KEY_FILE is set — the trimmed content of the
// file the variable names. The file content must be non-empty after trimming;
// a missing or empty file, and an empty spec key with the variable unset, are
// errors naming HYPERVISOR_SSH_PUBLIC_KEY_FILE so misconfiguration is
// actionable.
func (r *HypervisorMachineReconciler) resolveSSHPublicKey(
	config *bootstrapv1alpha1.HypervisorConfig,
) (string, error) {
	if config.Spec.SSHPublicKey != "" {
		return config.Spec.SSHPublicKey, nil
	}

	path := r.Config.SSHPublicKeyFile
	if path == "" {
		return "", fmt.Errorf(
			"ssh public key: bootstrap config %q spec.sshPublicKey is empty and HYPERVISOR_SSH_PUBLIC_KEY_FILE is unset",
			config.Name,
		)
	}
	content, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("read ssh public key file %q (HYPERVISOR_SSH_PUBLIC_KEY_FILE): %w", path, err)
	}
	key := strings.TrimSpace(string(content))
	if key == "" {
		return "", fmt.Errorf("ssh public key file %q (HYPERVISOR_SSH_PUBLIC_KEY_FILE) is empty", path)
	}

	return key, nil
}

// reconcileCIDATA renders the three cloud-init parts for the machine through
// the injected renderer with the machine hostname and the SSH public key
// resolved from the linked bootstrap config (spec.sshPublicKey, falling back
// to the HYPERVISOR_SSH_PUBLIC_KEY_FILE file), and builds the CIDATA disk
// image <vm-disks>/<name>-cidata.img carrying the rendered parts under their
// NoCloud file names. Network configuration is DHCP (no static IP). A machine
// with no linked bootstrap config has no key to inject and skips rendering
// without error.
func (r *HypervisorMachineReconciler) reconcileCIDATA(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
) error {
	ref := machine.Spec.Bootstrap.ConfigRef
	if !ref.IsDefined() || ref.Kind != "HypervisorConfig" || ref.Name == "" {
		return nil
	}

	config := &bootstrapv1alpha1.HypervisorConfig{}
	key := client.ObjectKey{Namespace: machine.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("get bootstrap config %q: %w", key, err)
	}

	sshPublicKey, err := r.resolveSSHPublicKey(config)
	if err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to render cloud-init data: %v", err)
		return fmt.Errorf("render cloud-init data for %q: %w", machine.Name, err)
	}
	parts, err := r.RenderCloudInit(cloudinit.Data{
		InstanceID:   machine.Name,
		Hostname:     machine.Name,
		SSHPublicKey: sshPublicKey,
	})
	if err != nil {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to render cloud-init data: %v", err)
		return fmt.Errorf("render cloud-init data for %q: %w", machine.Name, err)
	}

	if err := r.buildCIDATADisk(ctx, hm, parts); err != nil {
		r.Recorder.Eventf(
			hm,
			corev1.EventTypeWarning,
			"FailedProvision",
			"failed to build CIDATA disk for %q: %v",
			machine.Name,
			err,
		)
		return fmt.Errorf("build CIDATA disk for %q: %w", machine.Name, err)
	}

	return nil
}

// buildCIDATADisk produces the machine's CIDATA disk image
// <vm-disks>/<name>-cidata.img: a FAT16 image labeled CIDATA holding the
// three rendered cloud-init parts under their NoCloud file names (user-data,
// meta-data, network-config). The image is formatted with mkdosfs and the
// parts are copied in with mcopy through the exec seam, following the k8labs
// create-cloudinit.sh precedent. The rendered parts are staged under
// <vm-disks>/<name>-cidata-parts/ so the build is reproducible and the
// staging tree is removed with the machine on teardown.
func (r *HypervisorMachineReconciler) buildCIDATADisk(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	parts map[string][]byte,
) error {
	partsDir := filepath.Join(r.Config.VMDiskDir, hm.Name+"-cidata-parts")
	if err := os.MkdirAll(partsDir, 0o755); err != nil {
		return fmt.Errorf("create CIDATA parts dir %q: %w", partsDir, err)
	}
	for name, content := range parts {
		if err := os.WriteFile(filepath.Join(partsDir, name), content, 0o644); err != nil {
			return fmt.Errorf("write CIDATA part %q: %w", name, err)
		}
	}

	diskPath := filepath.Join(r.Config.VMDiskDir, hm.Name+"-cidata.img")
	// A previous reconcile may have left a formatted image; mkdosfs refuses
	// to format an existing filesystem, so rebuild from a clean slate.
	if err := os.Remove(diskPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("remove stale CIDATA disk %q: %w", diskPath, err)
	}
	// Real mkdosfs cannot open a target that does not exist, so pre-create
	// the image file before formatting (the create-cloudinit.sh precedent:
	// a 4 MiB zeroed image).
	f, err := os.OpenFile(diskPath, os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("create CIDATA disk %q: %w", diskPath, err)
	}
	if err := f.Truncate(cidataDiskSize); err != nil {
		_ = f.Close()
		return fmt.Errorf("size CIDATA disk %q: %w", diskPath, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("close CIDATA disk %q: %w", diskPath, err)
	}
	if _, err := r.QemuImg(ctx, "mkdosfs", "-F", "16", "-s", "1", "-n", "CIDATA", diskPath); err != nil {
		return fmt.Errorf("format CIDATA disk %q: %w", diskPath, err)
	}
	for _, name := range []string{"user-data", "meta-data", "network-config"} {
		src := filepath.Join(partsDir, name)
		if _, err := r.QemuImg(ctx, "mcopy", "-i", diskPath, src, "::"+name); err != nil {
			return fmt.Errorf("copy %s into CIDATA disk %q: %w", name, diskPath, err)
		}
	}

	return nil
}

// reconcileVMLifecycle drives the machine's VM through the machine's own
// client (vmClientFor). Before the first boot it hands the VM client the
// boot configuration: the k8netd vhost-user net
// device string — the port socket path of the machine and its MAC, single
// queue pair (REQ-005) —, the firmware image from the provider config, the
// disk images (the root qcow2, then the CIDATA disk when one was built, then
// any packaged confext raws), and the spec cpu/ram shape. The VM
// client pushes that configuration over the cloud-hypervisor API before it
// boots, then the controller records the provider ID; on every reconcile it
// asks the client for the VM state and reports the VMProvisioned condition
// and readiness once the VM reports running. A VM that is not running yet,
// or whose state query fails, is left not ready without error. The k8netd
// port/IP is already ensured by reconcileIdentity before this stage, so the
// port socket exists by the time the VM process starts.
func (r *HypervisorMachineReconciler) reconcileVMLifecycle(
	ctx context.Context,
	hm *infrastructurev1alpha1.HypervisorMachine,
	machine *clusterv1.Machine,
	mac string,
) error {
	netConfig, err := chclient.VhostUserNetConfig(chclient.VhostUserSocketPath(hm.Name), mac)
	if err != nil {
		r.Recorder.Eventf(
			hm,
			corev1.EventTypeWarning,
			"FailedProvision",
			"failed to render vhost-user net config for %q: %v",
			hm.Name,
			err,
		)
		return fmt.Errorf("render vhost-user net config for %q: %w", hm.Name, err)
	}
	vm := r.vmClientFor(hm)
	vm.SetNetConfig(netConfig)

	diskPaths := []string{filepath.Join(r.Config.VMDiskDir, hm.Name+"-root.qcow2")}
	// The CIDATA disk sits directly after the root disk (it occupies
	// /dev/vdb, shifting the confext raws to vdc+); a machine without
	// bootstrap data has no CIDATA disk and carries the root disk alone.
	cidataDisk := filepath.Join(r.Config.VMDiskDir, hm.Name+"-cidata.img")
	if _, err := os.Stat(cidataDisk); err == nil {
		diskPaths = append(diskPaths, cidataDisk)
	} else if !os.IsNotExist(err) {
		r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to stat CIDATA disk for %q: %v", hm.Name, err)
		return fmt.Errorf("stat CIDATA disk for %q: %w", hm.Name, err)
	}
	confextRaws, err := confextRawPaths(r.Config.VMDiskDir, hm.Name)
	if err != nil {
		r.Recorder.Eventf(
			hm,
			corev1.EventTypeWarning,
			"FailedProvision",
			"failed to list confext raws for %q: %v",
			hm.Name,
			err,
		)
		return fmt.Errorf("list confext raws for %q: %w", hm.Name, err)
	}
	diskPaths = append(diskPaths, confextRaws...)

	vm.SetFirmware(r.Config.Firmware)
	vm.SetDiskPaths(diskPaths)
	vm.SetCPU(hm.Spec.CPU)
	vm.SetRAM(hm.Spec.RAM)

	if hm.Status.ProviderID == nil {
		// REQ-004/REQ-008: a machine whose bootstrap config renders runtime
		// confext data must NOT boot before that data exists — the confext
		// raws are attached at first boot and a late attach would require a
		// reboot. The control-plane apiserver renders each machine's
		// bootstrap Secret (PKI + kubelet config) independently, so waiting
		// for it here cannot deadlock; it only orders workers after the
		// control plane has produced their config.
		pending, err := r.bootstrapDataPending(ctx, machine)
		if err != nil {
			return err
		}
		if pending {
			log := ctrl.LoggerFrom(ctx)
			log.Info("bootstrap data not yet available, deferring VM boot", "machine", machine.Name)
			return nil
		}
		if err := vm.EnsureRunning(ctx); err != nil {
			r.Recorder.Eventf(hm, corev1.EventTypeWarning, "FailedProvision", "failed to boot VM for %q: %v", machine.Name, err)
			return fmt.Errorf("boot VM for %q: %w", machine.Name, err)
		}

		providerID := fmt.Sprintf("hypervisor://%s/%s", hm.Spec.ClusterName, hm.Name)
		hm.Status.ProviderID = &providerID
	}

	state, err := vm.Info(ctx)
	if err == nil && state == ch.VMState("Running") {
		markVMProvisioned(hm)
		hm.Status.Ready = true
		// The CAPI v1beta2 machine controller gates InfrastructureReady on
		// status.initialization.provisioned alone; report it with the same
		// conditions under which ready flips true.
		hm.Status.Initialization = &infrastructurev1alpha1.InitializationStatus{Provisioned: true}
	}

	if err := r.Status().Update(ctx, hm); err != nil {
		return fmt.Errorf("update HypervisorMachine status: %w", err)
	}

	return nil
}

// confextRawPaths lists the packaged confext squashfs images (<name>.raw
// files) in the machine's confext output directory under vmDisksDir, sorted
// by file name. A directory that does not exist means the machine has no
// bootstrap data (reconcileConfextDataDisk skipped packaging) and yields no
// paths; any other read error surfaces.
func confextRawPaths(vmDisksDir, name string) ([]string, error) {
	entries, err := os.ReadDir(filepath.Join(vmDisksDir, name+"-data"))
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read confext data dir for %q: %w", name, err)
	}

	var paths []string
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".raw") {
			continue
		}
		paths = append(paths, filepath.Join(vmDisksDir, name+"-data", entry.Name()))
	}
	return paths, nil
}

// markVMProvisioned upserts the VMProvisioned condition as true on the
// machine status, preserving any other conditions.
func markVMProvisioned(hm *infrastructurev1alpha1.HypervisorMachine) {
	meta.SetStatusCondition(&hm.Status.Conditions, metav1.Condition{
		Type:   vmProvisionedCondition,
		Status: metav1.ConditionTrue,
		Reason: "VMProvisioned",
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
	if !ref.IsDefined() || ref.Kind != "HypervisorConfig" || ref.Name == "" {
		return "", nil
	}

	config := &bootstrapv1alpha1.HypervisorConfig{}
	key := client.ObjectKey{Namespace: machine.Namespace, Name: ref.Name}
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

// bootstrapDataPending reports whether the machine's bootstrap provider is a
// HypervisorConfig whose rendered data Secret is not yet available. Such a
// machine must wait before its VM first boot (the confext raws that carry the
// bootstrap config are attached at first boot, and a late attach would need a
// reboot). Machines without a HypervisorConfig bootstrap ref always report
// false — they have no confext payload to wait for.
func (r *HypervisorMachineReconciler) bootstrapDataPending(
	ctx context.Context,
	machine *clusterv1.Machine,
) (bool, error) {
	ref := machine.Spec.Bootstrap.ConfigRef
	if !ref.IsDefined() || ref.Kind != "HypervisorConfig" || ref.Name == "" {
		return false, nil
	}

	config := &bootstrapv1alpha1.HypervisorConfig{}
	key := client.ObjectKey{Namespace: machine.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, config); err != nil {
		if apierrors.IsNotFound(err) {
			// The config object has not been created yet; the machine is not
			// ready to boot until the config controller has rendered it.
			return true, nil
		}
		return false, fmt.Errorf("get bootstrap config %q: %w", key, err)
	}
	return config.Status.DataSecretName == nil, nil
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
