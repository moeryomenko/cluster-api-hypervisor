package executor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/artifact"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/systemd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/ch"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/confext"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
)

// Executor is the production HostAgent implementation. It deliberately fails
// closed for operations whose real host implementation has not been installed;
// it never returns the success values used by hostagent.Fake.
type NetworkClient interface {
	CreateNetwork(context.Context, string, string, string, string, string) error
	DeleteNetwork(context.Context, string) error
	CreatePort(context.Context, string) error
	DeletePort(context.Context, string) error
	AttachPort(context.Context, string, string, string) error
	DetachPort(context.Context, string) error
	AllocateIP(context.Context, string, string) (string, error)
	ReleaseIP(context.Context, string, string) error
	PublishPort(context.Context, string, int32) (int32, error)
	UnpublishPort(context.Context, string, int32) error
}

type Executor struct {
	Store           *inventory.Store
	Systemd         systemd.Client
	Artifacts       artifact.Builder
	Network         NetworkClient
	NodeID          string
	CloudHypervisor string
	K8netdSocket    string
	KVMPath         string
	UnitDir         string
	FirmwareRoot    string
	BaseImageRoot   string
	Manifest        *artifact.Manifest
}

var _ hostagent.HostAgent = (*Executor)(nil)

func (e *Executor) Health(context.Context) (hostagent.Capabilities, error) {
	if e.Store == nil || e.Systemd == nil || e.NodeID == "" {
		return hostagent.Capabilities{}, fmt.Errorf(
			"%w: inventory, user systemd, and node ID are required",
			hostagent.ErrUnavailable,
		)
	}

	if _, err := os.Stat(e.KVMPath); err != nil {
		return hostagent.Capabilities{}, fmt.Errorf("%w: KVM device: %v", hostagent.ErrUnavailable, err)
	}

	if _, err := os.Stat(e.K8netdSocket); err != nil {
		return hostagent.Capabilities{}, fmt.Errorf("%w: k8netd socket: %v", hostagent.ErrUnavailable, err)
	}

	return hostagent.Capabilities{
		NodeID:              e.NodeID,
		CloudHypervisor:     e.CloudHypervisor,
		K8netdProtocolMajor: hostagent.ProtocolMajor,
		CanUseKVM:           true,
		CanUseUserDBus:      true,
	}, nil
}

func (e *Executor) EnsureVM(
	ctx context.Context,
	mutation hostagent.Mutation,
	desired hostagent.VMDesired,
) (hostagent.VMObserved, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.VMObserved{}, err
	}

	if e.Manifest != nil {
		desired.Firmware = e.Manifest.Firmware.Path
		desired.FirmwareSHA256 = e.Manifest.Firmware.SHA256
	}

	if e.Systemd == nil || e.Store == nil || e.UnitDir == "" || e.CloudHypervisor == "" || e.FirmwareRoot == "" {
		return hostagent.VMObserved{}, e.notImplemented(
			"EnsureVM requires user systemd, inventory, unit directory, firmware root, and Cloud Hypervisor",
		)
	}

	if desired.UID == "" || desired.Disk == "" || desired.Firmware == "" || desired.FirmwareSHA256 == "" ||
		desired.APISocket == "" || desired.VhostSocket == "" || desired.MAC == "" || desired.CPUs == 0 ||
		desired.MemoryMiB == 0 {
		return hostagent.VMObserved{}, hostagent.ErrInvalidRequest
	}

	operation, err := e.begin(mutation, "EnsureVM")
	if err != nil {
		return hostagent.VMObserved{}, err
	}

	if operation.State == inventory.OperationCompleted {
		return e.GetVM(ctx, mutation.Owner)
	}

	unitName := "k8slab-vm-" + mutation.Owner.UID + ".service"
	arguments := []string{e.CloudHypervisor, "--api-socket", "path=" + desired.APISocket}

	unit, err := systemd.Install(
		ctx,
		e.Systemd,
		e.UnitDir,
		systemd.PersistentUnit{
			InstallationID: mutation.Owner.InstallationID,
			OwnerUID:       mutation.Owner.UID,
			NodeID:         mutation.Owner.NodeID,
			Name:           unitName,
			ExecStart:      arguments,
		},
	)
	if err != nil {
		_ = e.complete(mutation, "EnsureVM", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return hostagent.VMObserved{}, err
	}

	api := ch.NewClient(desired.APISocket)
	if err := waitForSocket(ctx, desired.APISocket); err != nil {
		return hostagent.VMObserved{}, err
	}

	disks := []ch.DiskConfig{{Path: desired.Disk}}
	for _, path := range desired.AdditionalDisks {
		if path == "" {
			return hostagent.VMObserved{}, hostagent.ErrInvalidRequest
		}

		disks = append(disks, ch.DiskConfig{Path: path, Readonly: true})
	}

	checksums := append([]string{desired.DiskSHA256}, desired.AdditionalDiskSHA256s...)

	paths := append([]string{desired.Disk}, desired.AdditionalDisks...)
	if err := e.Artifacts.Verify(paths, checksums); err != nil {
		_ = e.complete(mutation, "EnsureVM", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return hostagent.VMObserved{}, fmt.Errorf("verify VM disks: %w", err)
	}

	if err := artifact.VerifyOwnedFile(e.FirmwareRoot, desired.Firmware, desired.FirmwareSHA256); err != nil {
		_ = e.complete(mutation, "EnsureVM", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return hostagent.VMObserved{}, fmt.Errorf("verify VM firmware: %w", err)
	}

	err = api.Create(ctx, ch.VmConfig{
		Payload: &ch.PayloadConfig{
			Firmware: desired.Firmware,
		},
		Cpus: &ch.CpusConfig{
			BootVCPUs: int(desired.CPUs),
			MaxVCPUs:  int(desired.CPUs),
		},
		Memory: &ch.MemoryConfig{
			Size:   int64(desired.MemoryMiB) * 1024 * 1024,
			Shared: true,
		},
		Disks: disks,
		Net: []ch.NetConfig{
			{
				VhostUser:   true,
				VhostSocket: desired.VhostSocket,
				MAC:         desired.MAC,
				NumQueues:   2,
			},
		},
	})
	if err != nil {
		return hostagent.VMObserved{}, err
	}

	if err := api.Boot(ctx); err != nil {
		return hostagent.VMObserved{}, err
	}

	vm := inventory.VM{
		InstallationID: mutation.Owner.InstallationID,
		OwnerUID:       mutation.Owner.UID,
		NodeID:         mutation.Owner.NodeID,
		Unit:           unitName,
		Generation:     mutation.Generation,
		PID:            int64(unit.PID),
		Disk:           desired.Disk,
		APISocket:      desired.APISocket,
		VhostSocket:    desired.VhostSocket,
		MAC:            desired.MAC,
		IP:             desired.IP,
	}
	if err := e.Store.UpsertVM(vm); err != nil {
		return hostagent.VMObserved{}, err
	}

	observed := hostagent.VMObserved{
		UID:         desired.UID,
		Unit:        unitName,
		PID:         int(unit.PID),
		Running:     unit.PID != 0,
		APISocket:   desired.APISocket,
		VhostSocket: desired.VhostSocket,
		MAC:         desired.MAC,
		IP:          desired.IP,
		Generation:  mutation.Generation,
	}

	encoded, _ := json.Marshal(observed)
	if err := e.complete(mutation, "EnsureVM", operation.RequestHash, inventory.OperationCompleted, string(encoded), ""); err != nil {
		return hostagent.VMObserved{}, err
	}

	return observed, nil
}

func waitForSocket(ctx context.Context, path string) error {
	deadline := time.NewTimer(10 * time.Second)
	defer deadline.Stop()

	ticker := time.NewTicker(50 * time.Millisecond)
	defer ticker.Stop()

	for {
		if info, err := os.Stat(path); err == nil && info.Mode()&os.ModeSocket != 0 {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("%w: Cloud Hypervisor API socket %q did not appear", hostagent.ErrUnavailable, path)
		case <-ticker.C:
		}
	}
}

func (e *Executor) GetVM(ctx context.Context, owner hostagent.Owner) (hostagent.VMObserved, error) {
	if err := owner.Validate(); err != nil {
		return hostagent.VMObserved{}, err
	}

	if e.Store == nil || e.Systemd == nil {
		return hostagent.VMObserved{}, e.notImplemented("GetVM requires inventory and user systemd")
	}

	vm, err := e.Store.GetVM(owner.InstallationID, owner.UID)
	if errors.Is(err, sql.ErrNoRows) {
		return hostagent.VMObserved{}, hostagent.ErrNotFound
	}

	if err != nil {
		return hostagent.VMObserved{}, fmt.Errorf("load VM inventory: %w", err)
	}

	if vm.NodeID != owner.NodeID {
		return hostagent.VMObserved{}, hostagent.ErrUnauthorized
	}

	unit, err := e.Systemd.GetUnit(ctx, vm.Unit)
	if err != nil {
		return hostagent.VMObserved{}, fmt.Errorf("get VM unit %q: %w", vm.Unit, err)
	}

	return hostagent.VMObserved{
		UID:         owner.UID,
		Unit:        vm.Unit,
		PID:         int(unit.PID),
		Running:     unit.PID != 0,
		APISocket:   vm.APISocket,
		VhostSocket: vm.VhostSocket,
		MAC:         vm.MAC,
		IP:          vm.IP,
		Generation:  vm.Generation,
	}, nil
}

func (e *Executor) StopVM(ctx context.Context, mutation hostagent.Mutation) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if e.Store == nil || e.Systemd == nil {
		return e.notImplemented("StopVM requires inventory and user systemd")
	}

	operation, err := e.begin(mutation, "StopVM")
	if err != nil {
		return err
	}

	if operation.State == inventory.OperationCompleted {
		return nil
	}

	if operation.State == inventory.OperationFailed {
		return fmt.Errorf("%w: prior StopVM failed: %s", hostagent.ErrUnavailable, operation.Failure)
	}

	vm, err := e.Store.GetVM(mutation.Owner.InstallationID, mutation.Owner.UID)
	if errors.Is(err, sql.ErrNoRows) {
		return e.complete(mutation, "StopVM", operation.RequestHash, inventory.OperationCompleted, "absent", "")
	}

	if err != nil {
		return fmt.Errorf("load VM inventory: %w", err)
	}

	if vm.NodeID != mutation.Owner.NodeID {
		return hostagent.ErrUnauthorized
	}

	if err := ch.NewClient(vm.APISocket).Shutdown(ctx); err != nil {
		var statusError *ch.StatusError
		if _, statErr := os.Stat(vm.APISocket); statErr == nil && !errors.As(err, &statusError) {
			return fmt.Errorf("graceful VM shutdown: %w", err)
		}
	}

	if err := e.Systemd.StopUnit(ctx, vm.Unit); err != nil {
		_ = e.complete(mutation, "StopVM", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return fmt.Errorf("stop owned VM unit: %w", err)
	}

	return e.complete(mutation, "StopVM", operation.RequestHash, inventory.OperationCompleted, "stopped", "")
}

func (e *Executor) DeleteVM(ctx context.Context, mutation hostagent.Mutation) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if err := e.StopVM(ctx, mutation); err != nil {
		return err
	}

	vm, err := e.Store.GetVM(mutation.Owner.InstallationID, mutation.Owner.UID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return err
	}

	if vm.NodeID != mutation.Owner.NodeID {
		return hostagent.ErrUnauthorized
	}

	if e.Network != nil {
		if err := e.DeletePort(ctx, mutation); err != nil {
			return err
		}
	}

	if !mutation.RetainDisk && e.Artifacts.Root != "" {
		removeOwnedArtifact := func(path string) {
			if relative, err := filepath.Rel(e.Artifacts.Root, path); err == nil && relative != ".." &&
				!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
				_ = os.RemoveAll(path)
			}
		}

		removeOwnedArtifact(vm.Disk)
		removeOwnedArtifact(vm.APISocket)

		prefix := strings.TrimSuffix(filepath.Base(vm.Disk), "-root.qcow2") + "-"
		if entries, err := os.ReadDir(filepath.Dir(vm.Disk)); err == nil {
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), prefix) {
					removeOwnedArtifact(filepath.Join(filepath.Dir(vm.Disk), entry.Name()))
				}
			}
		}
	}

	return e.Store.DeleteVM(mutation.Owner.InstallationID, mutation.Owner.UID)
}

func (e *Executor) EnsureNetwork(
	ctx context.Context,
	mutation hostagent.Mutation,
	request hostagent.NetworkRequest,
) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if e.Network == nil || request.Name == "" {
		return e.notImplemented("EnsureNetwork requires k8netd and network name")
	}

	prefix, err := netip.ParsePrefix(request.CIDR)
	if err != nil || !prefix.Addr().Is4() || prefix.Bits() != 24 {
		return hostagent.ErrInvalidRequest
	}

	base := prefix.Masked().Addr().As4()
	gateway := netip.AddrFrom4([4]byte{base[0], base[1], base[2], base[3] + 1})
	start := netip.AddrFrom4([4]byte{base[0], base[1], base[2], base[3] + 2})

	end := netip.AddrFrom4([4]byte{base[0], base[1], base[2], base[3] + 254})
	if err := e.Network.CreateNetwork(ctx, request.Name, request.CIDR, gateway.String(), start.String(), end.String()); err != nil &&
		!errors.Is(err, k8netd.ErrAlreadyExists) {
		return fmt.Errorf("ensure network: %w", err)
	}

	return nil
}

func (e *Executor) DeleteNetwork(ctx context.Context, mutation hostagent.Mutation) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if e.Network == nil {
		return e.notImplemented("DeleteNetwork requires k8netd")
	}

	err := e.Network.DeleteNetwork(ctx, mutation.Owner.UID)
	if errors.Is(err, k8netd.ErrNotFound) {
		return nil
	}

	return err
}

func (e *Executor) EnsurePort(
	ctx context.Context,
	mutation hostagent.Mutation,
	request hostagent.PortRequest,
) (hostagent.PortObserved, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.PortObserved{}, err
	}

	if e.Network == nil || e.Store == nil || request.Name == "" || request.Network == "" || request.MAC == "" {
		return hostagent.PortObserved{}, e.notImplemented("EnsurePort requires k8netd, inventory, and port identity")
	}

	resource, err := e.Store.GetNetworkResourceByPort(mutation.Owner.InstallationID, request.Name)
	switch {
	case err == nil:
		if resource.OwnerUID != mutation.Owner.UID {
			return hostagent.PortObserved{}, fmt.Errorf(
				"%w: port %q is owned by %q",
				hostagent.ErrConflict,
				request.Name,
				resource.OwnerUID,
			)
		}

		if resource.NodeID != mutation.Owner.NodeID || resource.Network != request.Network || resource.MAC != request.MAC {
			return hostagent.PortObserved{}, fmt.Errorf(
				"%w: port %q identity does not match recorded resource",
				hostagent.ErrConflict,
				request.Name,
			)
		}

		return observedPort(resource), nil
	case !errors.Is(err, sql.ErrNoRows):
		return hostagent.PortObserved{}, fmt.Errorf("inspect network resource for port %q: %w", request.Name, err)
	}

	if err := e.Network.CreatePort(ctx, request.Name); err != nil && !errors.Is(err, k8netd.ErrAlreadyExists) {
		return hostagent.PortObserved{}, err
	}

	if err := e.Network.AttachPort(ctx, request.Name, request.Network, request.MAC); err != nil &&
		!errors.Is(err, k8netd.ErrAlreadyExists) {
		return hostagent.PortObserved{}, err
	}

	ip, err := e.Network.AllocateIP(ctx, request.Network, request.MAC)
	if err != nil {
		return hostagent.PortObserved{}, err
	}

	resource = inventory.NetworkResource{
		InstallationID: mutation.Owner.InstallationID,
		OwnerUID:       mutation.Owner.UID,
		NodeID:         mutation.Owner.NodeID,
		Network:        request.Network,
		Port:           request.Name,
		MAC:            request.MAC,
		IP:             ip,
	}
	if err := e.Store.UpsertNetworkResource(resource); err != nil {
		if existing, lookupErr := e.Store.GetNetworkResourceByPort(resource.InstallationID, resource.Port); lookupErr == nil {
			if existing.OwnerUID != resource.OwnerUID {
				return hostagent.PortObserved{}, fmt.Errorf(
					"%w: port %q is owned by %q",
					hostagent.ErrConflict,
					resource.Port,
					existing.OwnerUID,
				)
			}

			if existing.NodeID == resource.NodeID && existing.Network == resource.Network && existing.MAC == resource.MAC {
				return observedPort(existing), nil
			}

			return hostagent.PortObserved{}, fmt.Errorf(
				"%w: port %q identity does not match recorded resource",
				hostagent.ErrConflict,
				resource.Port,
			)
		}

		return hostagent.PortObserved{}, err
	}

	return observedPort(resource), nil
}

func observedPort(resource inventory.NetworkResource) hostagent.PortObserved {
	return hostagent.PortObserved{
		Name:      resource.Port,
		Network:   resource.Network,
		MAC:       resource.MAC,
		IP:        resource.IP,
		Published: map[uint32]uint32{},
	}
}

func (e *Executor) DeletePort(ctx context.Context, mutation hostagent.Mutation) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if e.Network == nil || e.Store == nil {
		return e.notImplemented("DeletePort requires k8netd inventory")
	}

	resource, err := e.Store.GetNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}

	if err != nil {
		return err
	}

	if resource.NodeID != mutation.Owner.NodeID {
		return hostagent.ErrUnauthorized
	}

	published, err := e.Store.ListPublishedPorts(mutation.Owner.InstallationID, mutation.Owner.UID)
	if err != nil {
		return err
	}

	for _, mapping := range published {
		if err := e.Network.UnpublishPort(ctx, resource.Port, int32(mapping.GuestPort)); err != nil &&
			!errors.Is(err, k8netd.ErrNotFound) {
			return err
		}

		if err := e.Store.DeletePublishedPort(mutation.Owner.InstallationID, mutation.Owner.UID, mapping.GuestPort); err != nil {
			return err
		}
	}

	if err := e.Network.ReleaseIP(ctx, resource.Network, resource.MAC); err != nil && !errors.Is(err, k8netd.ErrNotFound) {
		return err
	}

	if err := e.Network.DetachPort(ctx, resource.Port); err != nil && !errors.Is(err, k8netd.ErrNotFound) {
		return err
	}

	if err := e.Network.DeletePort(ctx, resource.Port); err != nil && !errors.Is(err, k8netd.ErrNotFound) {
		return err
	}

	return e.Store.DeleteNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID)
}

func (e *Executor) AllocateIP(ctx context.Context, mutation hostagent.Mutation) (string, error) {
	if err := mutation.Validate(); err != nil {
		return "", err
	}

	resource, err := e.Store.GetNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID)
	if err != nil {
		return "", err
	}

	if resource.NodeID != mutation.Owner.NodeID {
		return "", hostagent.ErrUnauthorized
	}

	return e.Network.AllocateIP(ctx, resource.Network, resource.MAC)
}

func (e *Executor) ReleaseIP(ctx context.Context, mutation hostagent.Mutation, ip string) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	resource, err := e.Store.GetNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID)
	if err != nil {
		return err
	}

	if resource.IP != ip || resource.NodeID != mutation.Owner.NodeID {
		return hostagent.ErrUnauthorized
	}

	return e.Network.ReleaseIP(ctx, resource.Network, resource.MAC)
}

func (e *Executor) PublishPort(
	ctx context.Context,
	mutation hostagent.Mutation,
	guestPort, hostPort uint32,
) (uint32, error) {
	if err := mutation.Validate(); err != nil {
		return 0, err
	}

	if guestPort == 0 || guestPort > 65535 || hostPort != 0 || e.Network == nil || e.Store == nil {
		return 0, hostagent.ErrInvalidRequest
	}

	resource, err := e.Store.GetNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID)
	if err != nil {
		return 0, err
	}

	if resource.NodeID != mutation.Owner.NodeID {
		return 0, hostagent.ErrUnauthorized
	}

	operation, err := e.begin(mutation, "PublishPort", guestPort)
	if err != nil {
		return 0, err
	}

	if operation.State == inventory.OperationCompleted {
		var result uint32
		if err := json.Unmarshal([]byte(operation.Result), &result); err != nil {
			return 0, fmt.Errorf("decode published host port: %w", err)
		}

		return result, nil
	}

	if operation.State == inventory.OperationFailed {
		return 0, portOperationFailure("PublishPort", operation.Failure)
	}

	result, err := e.Network.PublishPort(ctx, resource.Port, int32(guestPort))
	if err != nil {
		_ = e.complete(mutation, "PublishPort", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return 0, err
	}

	if result <= 0 || result > 65535 {
		err := hostagent.ErrInvalidRequest
		_ = e.complete(mutation, "PublishPort", operation.RequestHash, inventory.OperationFailed, "", err.Error())

		return 0, err
	}

	published := uint32(result)
	if err := e.Store.UpsertPublishedPort(inventory.PublishedPort{
		InstallationID: mutation.Owner.InstallationID,
		OwnerUID:       mutation.Owner.UID,
		GuestPort:      guestPort,
		HostPort:       published,
	}); err != nil {
		_ = e.complete(mutation, "PublishPort", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return 0, err
	}

	encoded, _ := json.Marshal(published)
	if err := e.complete(mutation, "PublishPort", operation.RequestHash, inventory.OperationCompleted, string(encoded), ""); err != nil {
		return 0, err
	}

	return published, nil
}

func (e *Executor) ReleasePort(ctx context.Context, mutation hostagent.Mutation, guestPort, hostPort uint32) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if guestPort == 0 || guestPort > 65535 || hostPort == 0 || hostPort > 65535 || e.Network == nil || e.Store == nil {
		return hostagent.ErrInvalidRequest
	}

	resource, err := e.Store.GetNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID)
	if err != nil {
		return err
	}

	if resource.NodeID != mutation.Owner.NodeID {
		return hostagent.ErrUnauthorized
	}

	published, mappingErr := e.Store.GetPublishedPort(mutation.Owner.InstallationID, mutation.Owner.UID, guestPort)
	if mappingErr != nil && !errors.Is(mappingErr, sql.ErrNoRows) {
		return mappingErr
	}

	if mappingErr == nil && published.HostPort != hostPort {
		return hostagent.ErrInvalidRequest
	}

	operation, err := e.begin(mutation, "ReleasePort", struct {
		GuestPort uint32
		HostPort  uint32
	}{guestPort, hostPort})
	if err != nil {
		return err
	}

	if operation.State == inventory.OperationCompleted {
		return nil
	}

	if operation.State == inventory.OperationFailed {
		return portOperationFailure("ReleasePort", operation.Failure)
	}

	if errors.Is(mappingErr, sql.ErrNoRows) {
		return hostagent.ErrNotFound
	}

	if err := e.Network.UnpublishPort(ctx, resource.Port, int32(guestPort)); err != nil &&
		!errors.Is(err, k8netd.ErrNotFound) {
		_ = e.complete(mutation, "ReleasePort", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return err
	}

	if err := e.Store.DeletePublishedPort(mutation.Owner.InstallationID, mutation.Owner.UID, guestPort); err != nil {
		_ = e.complete(mutation, "ReleasePort", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return err
	}

	return e.complete(mutation, "ReleasePort", operation.RequestHash, inventory.OperationCompleted, "released", "")
}

func portOperationFailure(kind, failure string) error {
	return fmt.Errorf("%w: prior %s failed: %s", hostagent.ErrUnavailable, kind, failure)
}

func (e *Executor) AcquireProbe(context.Context, hostagent.Mutation, string) (hostagent.ProbeLease, error) {
	return hostagent.ProbeLease{}, e.notImplemented("AcquireProbe requires k8netd and VM adapters")
}

func (e *Executor) ReleaseProbe(context.Context, hostagent.Mutation, string) error {
	return e.notImplemented("ReleaseProbe requires k8netd and VM adapters")
}

func (e *Executor) PrepareRootDisk(
	ctx context.Context,
	mutation hostagent.Mutation,
	request hostagent.RootDiskRequest,
) (hostagent.ArtifactResult, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	if e.Manifest != nil {
		image, err := e.Manifest.Image(request.SourceImage)
		if err != nil {
			return hostagent.ArtifactResult{}, err
		}

		request.SourceImage = image.Path
	}

	operation, err := e.begin(mutation, "PrepareRootDisk")
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}

	if operation.State == inventory.OperationCompleted {
		var result hostagent.ArtifactResult
		if err := json.Unmarshal([]byte(operation.Result), &result); err != nil {
			return hostagent.ArtifactResult{}, err
		}

		if err := addArtifactChecksums(&result); err != nil {
			return hostagent.ArtifactResult{}, err
		}

		return result, nil
	}

	path, err := e.Artifacts.PrepareRootDisk(ctx, request.Name, request.SourceImage)
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}

	result := hostagent.ArtifactResult{Paths: []string{path}}
	if err := addArtifactChecksums(&result); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	encoded, _ := json.Marshal(result)
	if err := e.complete(mutation, "PrepareRootDisk", operation.RequestHash, inventory.OperationCompleted, string(encoded), ""); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	return result, nil
}

func addArtifactChecksums(result *hostagent.ArtifactResult) error {
	if len(result.Paths) == 0 {
		return hostagent.ErrInvalidRequest
	}

	result.SHA256s = make([]string, 0, len(result.Paths))
	for _, path := range result.Paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read artifact %q: %w", path, err)
		}

		digest := sha256.Sum256(contents)
		result.SHA256s = append(result.SHA256s, hex.EncodeToString(digest[:]))
	}

	return nil
}

func (e *Executor) PrepareCIDATA(
	ctx context.Context,
	mutation hostagent.Mutation,
	name string,
	files []hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	parts := map[string][]byte{}
	for _, file := range files {
		parts[file.Name] = file.Content
	}

	operation, err := e.begin(mutation, "PrepareCIDATA")
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}

	if operation.State == inventory.OperationCompleted {
		var result hostagent.ArtifactResult

		_ = json.Unmarshal([]byte(operation.Result), &result)

		return result, nil
	}

	result, err := e.Artifacts.PrepareCIDATA(ctx, name, parts)
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}

	response := hostagent.ArtifactResult{Paths: []string{result.Path}, SHA256s: []string{result.SHA256}}

	encoded, _ := json.Marshal(response)
	if err := e.complete(mutation, "PrepareCIDATA", operation.RequestHash, inventory.OperationCompleted, string(encoded), ""); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	return response, nil
}

func (e *Executor) PrepareConfext(
	ctx context.Context,
	mutation hostagent.Mutation,
	name string,
	files []hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	if name == "" || len(files) == 0 {
		return hostagent.ArtifactResult{}, hostagent.ErrInvalidRequest
	}

	operation, err := e.begin(mutation, "PrepareConfext")
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}

	if operation.State == inventory.OperationCompleted {
		var result hostagent.ArtifactResult
		if err := json.Unmarshal([]byte(operation.Result), &result); err != nil {
			return hostagent.ArtifactResult{}, err
		}

		return result, nil
	}

	tree := make(map[string][]byte, len(files))
	for _, file := range files {
		tree[file.Name] = file.Content
	}

	staging := filepath.Join(e.Artifacts.Root, name+"-confext")
	output := filepath.Join(e.Artifacts.Root, name+"-data")

	packager := confext.NewPackager()
	if err := packager.WriteTree(tree, staging); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	paths, err := packager.BuildRaws(ctx, staging, output)
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}

	result := hostagent.ArtifactResult{Paths: paths}
	for _, path := range paths {
		contents, err := os.ReadFile(path)
		if err != nil {
			return hostagent.ArtifactResult{}, err
		}

		digest := sha256.Sum256(contents)
		result.SHA256s = append(result.SHA256s, hex.EncodeToString(digest[:]))
	}

	encoded, _ := json.Marshal(result)
	if err := e.complete(mutation, "PrepareConfext", operation.RequestHash, inventory.OperationCompleted, string(encoded), ""); err != nil {
		return hostagent.ArtifactResult{}, err
	}

	return result, nil
}

func (e *Executor) Diagnostics(ctx context.Context, owner hostagent.Owner) (hostagent.Diagnostics, error) {
	if err := owner.Validate(); err != nil {
		return hostagent.Diagnostics{}, err
	}

	observed, err := e.GetVM(ctx, owner)
	if errors.Is(err, hostagent.ErrNotFound) {
		return hostagent.Diagnostics{OwnerUID: owner.UID, Messages: []string{"vm not found"}}, nil
	}

	if err != nil {
		return hostagent.Diagnostics{}, err
	}

	return hostagent.Diagnostics{
		OwnerUID: owner.UID,
		Messages: []string{fmt.Sprintf("unit=%s running=%t pid=%d", observed.Unit, observed.Running, observed.PID)},
	}, nil
}

func (e *Executor) begin(mutation hostagent.Mutation, kind string, inputs ...any) (inventory.Operation, error) {
	requestHash := operationHash(kind, mutation, inputs)
	operation := inventory.Operation{
		InstallationID: mutation.Owner.InstallationID,
		OwnerUID:       mutation.Owner.UID,
		NodeID:         mutation.Owner.NodeID,
		Key:            mutation.IdempotencyKey,
		Kind:           kind,
		RequestHash:    requestHash,
		Generation:     mutation.Generation,
	}

	stored, _, err := e.Store.BeginOperation(operation)
	if errors.Is(err, inventory.ErrIdempotencyConflict) {
		return inventory.Operation{}, fmt.Errorf("%w: %v", hostagent.ErrConflict, err)
	}

	if err != nil {
		return inventory.Operation{}, fmt.Errorf("journal %s intent: %w", kind, err)
	}

	return stored, nil
}

func (e *Executor) complete(
	mutation hostagent.Mutation,
	kind, requestHash string,
	state inventory.OperationState,
	result, failure string,
) error {
	operation := inventory.Operation{
		InstallationID: mutation.Owner.InstallationID,
		OwnerUID:       mutation.Owner.UID,
		NodeID:         mutation.Owner.NodeID,
		Key:            mutation.IdempotencyKey,
		Kind:           kind,
		RequestHash:    requestHash,
		Generation:     mutation.Generation,
	}
	if err := e.Store.CompleteOperation(operation, state, result, failure); err != nil {
		return fmt.Errorf("journal %s result: %w", kind, err)
	}

	return nil
}

func (e *Executor) notImplemented(detail string) error {
	return fmt.Errorf("%w: %s", hostagent.ErrUnavailable, detail)
}

func operationHash(kind string, mutation hostagent.Mutation, inputs []any) string {
	contents, _ := json.Marshal(struct {
		Kind     string
		Mutation hostagent.Mutation
		Inputs   []any
	}{kind, mutation, inputs})
	sum := sha256.Sum256(contents)

	return hex.EncodeToString(sum[:])
}
