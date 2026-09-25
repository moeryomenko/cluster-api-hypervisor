package executor

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/systemd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

type fakeNetwork struct{ createdNetwork, createdPort string }

func (f *fakeNetwork) CreateNetwork(_ context.Context, name, _, _, _, _ string) error {
	f.createdNetwork = name
	return nil
}
func (*fakeNetwork) DeleteNetwork(context.Context, string) error { return nil }
func (f *fakeNetwork) CreatePort(_ context.Context, name string) error {
	f.createdPort = name
	return nil
}
func (*fakeNetwork) DeletePort(context.Context, string) error                 { return nil }
func (*fakeNetwork) AttachPort(context.Context, string, string, string) error { return nil }
func (*fakeNetwork) DetachPort(context.Context, string) error                 { return nil }
func (*fakeNetwork) AllocateIP(context.Context, string, string) (string, error) {
	return "192.168.124.10", nil
}
func (*fakeNetwork) ReleaseIP(context.Context, string, string) error           { return nil }
func (*fakeNetwork) PublishPort(context.Context, string, int32) (int32, error) { return 20000, nil }

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

func TestEnsureNetworkAndPortUseTypedK8netdAdapter(t *testing.T) {
	executor, _, _ := testExecutor(t)
	network := &fakeNetwork{}
	executor.Network = network

	mutation := mutation()
	if err := executor.EnsureNetwork(context.Background(), mutation, hostagent.NetworkRequest{Name: "network-a", CIDR: "192.168.124.0/24"}); err != nil {
		t.Fatal(err)
	}

	port, err := executor.EnsurePort(
		context.Background(),
		mutation,
		hostagent.PortRequest{Name: "port-a", Network: "network-a", MAC: "02:00:00:00:00:01"},
	)
	if err != nil {
		t.Fatal(err)
	}

	if network.createdNetwork != "network-a" || network.createdPort != "port-a" || port.IP != "192.168.124.10" {
		t.Fatalf("network=%#v port=%#v", network, port)
	}
}

func TestNetworkResourceOwnershipSupportsPublishAndCleanup(t *testing.T) {
	executor, _, store := testExecutor(t)
	executor.Network = &fakeNetwork{}

	mutation := mutation()
	if err := store.UpsertNetworkResource(inventory.NetworkResource{InstallationID: mutation.Owner.InstallationID, OwnerUID: mutation.Owner.UID, NodeID: mutation.Owner.NodeID, Network: "network-a", Port: "port-a", MAC: "02:00:00:00:00:01", IP: "192.168.124.10"}); err != nil {
		t.Fatal(err)
	}

	hostPort, err := executor.PublishPort(context.Background(), mutation, 6443, 0)
	if err != nil || hostPort != 20000 {
		t.Fatalf("PublishPort()=%d,%v", hostPort, err)
	}

	if ip, err := executor.AllocateIP(context.Background(), mutation); err != nil || ip != "192.168.124.10" {
		t.Fatalf("AllocateIP()=%q,%v", ip, err)
	}

	if err := executor.DeletePort(context.Background(), mutation); err != nil {
		t.Fatal(err)
	}

	if _, err := store.GetNetworkResource(mutation.Owner.InstallationID, mutation.Owner.UID); !errors.Is(
		err,
		sql.ErrNoRows,
	) {
		t.Fatalf("network resource remains: %v", err)
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
