package executor

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/systemd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

type fakeSystemd struct {
	units   map[string]systemd.Unit
	stopped []string
}

func (*fakeSystemd) Reload(context.Context) error                    { return nil }
func (*fakeSystemd) EnableUnitFiles(context.Context, []string) error { return nil }
func (f *fakeSystemd) StartUnit(_ context.Context, name string) (systemd.Unit, error) {
	return f.units[name], nil
}

func (*fakeSystemd) StartTransientUnit(context.Context, string, []systemd.Property) (systemd.Unit, error) {
	return systemd.Unit{}, nil
}

func (f *fakeSystemd) StopUnit(_ context.Context, name string) error {
	f.stopped = append(f.stopped, name)
	delete(f.units, name)

	return nil
}

func (f *fakeSystemd) GetUnit(_ context.Context, name string) (systemd.Unit, error) {
	unit, ok := f.units[name]
	if !ok {
		return systemd.Unit{}, errors.New("not loaded")
	}

	return unit, nil
}
func (f *fakeSystemd) ListUnits(context.Context) ([]systemd.Unit, error) { return nil, nil }

func mutation() hostagent.Mutation {
	return hostagent.Mutation{
		ProtocolMajor:  hostagent.ProtocolMajor,
		Owner:          hostagent.Owner{InstallationID: "install-a", NodeID: "node-a", UID: "machine-a"},
		Generation:     1,
		IdempotencyKey: "stop-machine-a-1",
	}
}

func testExecutor(t *testing.T) (*Executor, *fakeSystemd, *inventory.Store) {
	t.Helper()

	store, err := inventory.Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() { _ = store.Close() })

	systemdClient := &fakeSystemd{
		units: map[string]systemd.Unit{"k8slab-vm-machine-a.service": {Name: "k8slab-vm-machine-a.service", PID: 41}},
	}

	return &Executor{Store: store, Systemd: systemdClient, NodeID: "node-a"}, systemdClient, store
}

func TestExecutorNeverReturnsFakeSuccessForUnimplementedHostOperations(t *testing.T) {
	executor, _, _ := testExecutor(t)

	_, err := executor.EnsureVM(context.Background(), mutation(), hostagent.VMDesired{})
	if !errors.Is(err, hostagent.ErrUnavailable) {
		t.Fatalf("EnsureVM() error=%v, want unavailable", err)
	}

	if err := executor.DeleteNetwork(context.Background(), mutation()); !errors.Is(err, hostagent.ErrUnavailable) {
		t.Fatalf("DeleteNetwork() error=%v, want unavailable", err)
	}
}

func TestStopVMUsesJournalAndStopsOnlyOwnedInventoryUnit(t *testing.T) {
	executor, systemdClient, store := testExecutor(t)
	if err := store.UpsertVM(inventory.VM{InstallationID: "install-a", OwnerUID: "machine-a", NodeID: "node-a", Unit: "k8slab-vm-machine-a.service"}); err != nil {
		t.Fatal(err)
	}

	if err := executor.StopVM(context.Background(), mutation()); err != nil {
		t.Fatal(err)
	}

	if len(systemdClient.stopped) != 1 || systemdClient.stopped[0] != "k8slab-vm-machine-a.service" {
		t.Fatalf("stopped=%v", systemdClient.stopped)
	}

	if err := executor.StopVM(context.Background(), mutation()); err != nil {
		t.Fatalf("replay error=%v", err)
	}

	if len(systemdClient.stopped) != 1 {
		t.Fatalf("replay stopped=%v, want one call", systemdClient.stopped)
	}
}

func TestGetVMDeniesNodeMismatch(t *testing.T) {
	executor, _, store := testExecutor(t)
	if err := store.UpsertVM(inventory.VM{InstallationID: "install-a", OwnerUID: "machine-a", NodeID: "other-node", Unit: "k8slab-vm-machine-a.service"}); err != nil {
		t.Fatal(err)
	}

	_, err := executor.GetVM(context.Background(), mutation().Owner)
	if !errors.Is(err, hostagent.ErrUnauthorized) {
		t.Fatalf("GetVM() error=%v, want unauthorized", err)
	}
}
