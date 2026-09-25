package executor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/artifact"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/systemd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

// Executor is the production HostAgent implementation. It deliberately fails
// closed for operations whose real host implementation has not been installed;
// it never returns the success values used by hostagent.Fake.
type Executor struct {
	Store           *inventory.Store
	Systemd         systemd.Client
	Artifacts       artifact.Builder
	NodeID          string
	CloudHypervisor string
	K8netdSocket    string
	KVMPath         string
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

func (e *Executor) EnsureVM(context.Context, hostagent.Mutation, hostagent.VMDesired) (hostagent.VMObserved, error) {
	return hostagent.VMObserved{}, e.notImplemented(
		"EnsureVM requires disk, CIDATA, Cloud Hypervisor API, and k8netd transaction adapters",
	)
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

	if err := e.Systemd.StopUnit(ctx, vm.Unit); err != nil {
		_ = e.complete(mutation, "StopVM", operation.RequestHash, inventory.OperationFailed, "", err.Error())
		return fmt.Errorf("stop owned VM unit: %w", err)
	}

	return e.complete(mutation, "StopVM", operation.RequestHash, inventory.OperationCompleted, "stopped", "")
}

func (e *Executor) DeleteVM(context.Context, hostagent.Mutation) error {
	return e.notImplemented("DeleteVM requires disk, network, and unit-file cleanup adapters")
}

func (e *Executor) EnsureNetwork(context.Context, hostagent.Mutation, hostagent.NetworkRequest) error {
	return e.notImplemented("EnsureNetwork requires k8netd adapter")
}

func (e *Executor) DeleteNetwork(context.Context, hostagent.Mutation) error {
	return e.notImplemented("DeleteNetwork requires k8netd adapter")
}

func (e *Executor) EnsurePort(
	context.Context,
	hostagent.Mutation,
	hostagent.PortRequest,
) (hostagent.PortObserved, error) {
	return hostagent.PortObserved{}, e.notImplemented("EnsurePort requires k8netd adapter")
}

func (e *Executor) DeletePort(context.Context, hostagent.Mutation) error {
	return e.notImplemented("DeletePort requires k8netd adapter")
}

func (e *Executor) AllocateIP(context.Context, hostagent.Mutation) (string, error) {
	return "", e.notImplemented("AllocateIP requires k8netd adapter")
}

func (e *Executor) ReleaseIP(context.Context, hostagent.Mutation, string) error {
	return e.notImplemented("ReleaseIP requires k8netd adapter")
}

func (e *Executor) PublishPort(context.Context, hostagent.Mutation, uint32, uint32) (uint32, error) {
	return 0, e.notImplemented("PublishPort requires k8netd adapter")
}

func (e *Executor) ReleasePort(context.Context, hostagent.Mutation, uint32, uint32) error {
	return e.notImplemented("ReleasePort requires k8netd adapter")
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
	operation, err := e.begin(mutation, "PrepareRootDisk")
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
	path, err := e.Artifacts.PrepareRootDisk(ctx, request.Name, request.SourceImage)
	if err != nil {
		return hostagent.ArtifactResult{}, err
	}
	result := hostagent.ArtifactResult{Paths: []string{path}}
	encoded, _ := json.Marshal(result)
	if err := e.complete(mutation, "PrepareRootDisk", operation.RequestHash, inventory.OperationCompleted, string(encoded), ""); err != nil {
		return hostagent.ArtifactResult{}, err
	}
	return result, nil
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
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, e.notImplemented("PrepareConfext requires confext packager adapter")
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

func (e *Executor) begin(mutation hostagent.Mutation, kind string) (inventory.Operation, error) {
	requestHash := operationHash(kind, mutation)
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

func operationHash(kind string, mutation hostagent.Mutation) string {
	contents, _ := json.Marshal(struct {
		Kind     string
		Mutation hostagent.Mutation
	}{kind, mutation})
	sum := sha256.Sum256(contents)

	return hex.EncodeToString(sum[:])
}
