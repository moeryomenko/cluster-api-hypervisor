package executor

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/artifact"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/systemd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

type fakeNetwork struct {
	createdNetwork string
	createdPort    string
	calls          []string
	publishCalls   []int32
	unpublishCalls []int32
	publishResult  int32
	publishErr     error
	unpublishErr   error
}

func (f *fakeNetwork) CreateNetwork(_ context.Context, name, _, _, _, _ string) error {
	f.createdNetwork = name
	f.calls = append(f.calls, "CreateNetwork")

	return nil
}
func (f *fakeNetwork) DeleteNetwork(context.Context, string) error { return nil }
func (f *fakeNetwork) CreatePort(_ context.Context, name string) error {
	f.createdPort = name
	f.calls = append(f.calls, "CreatePort")

	return nil
}

func (f *fakeNetwork) DeletePort(context.Context, string) error {
	f.calls = append(f.calls, "DeletePort")
	return nil
}

func (f *fakeNetwork) AttachPort(context.Context, string, string, string) error {
	f.calls = append(f.calls, "AttachPort")
	return nil
}

func (f *fakeNetwork) DetachPort(context.Context, string) error {
	f.calls = append(f.calls, "DetachPort")
	return nil
}

func (f *fakeNetwork) AllocateIP(context.Context, string, string) (string, error) {
	f.calls = append(f.calls, "AllocateIP")
	return "192.168.124.10", nil
}

func (f *fakeNetwork) ReleaseIP(context.Context, string, string) error {
	f.calls = append(f.calls, "ReleaseIP")
	return nil
}

func (f *fakeNetwork) PublishPort(_ context.Context, _ string, guestPort int32) (int32, error) {
	f.calls = append(f.calls, "PublishPort")

	f.publishCalls = append(f.publishCalls, guestPort)
	if f.publishErr != nil {
		return 0, f.publishErr
	}

	if f.publishResult == 0 {
		return 20000, nil
	}

	return f.publishResult, nil
}

func (f *fakeNetwork) UnpublishPort(_ context.Context, _ string, guestPort int32) error {
	f.calls = append(f.calls, "UnpublishPort")
	f.unpublishCalls = append(f.unpublishCalls, guestPort)

	return f.unpublishErr
}

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

	return &Executor{
		Store:           store,
		Systemd:         systemdClient,
		NodeID:          "node-a",
		UnitDir:         filepath.Join(t.TempDir(), "systemd"),
		CloudHypervisor: "/bin/true",
		FirmwareRoot:    t.TempDir(),
	}, systemdClient, store
}

func TestHealthRequiresNodeID(t *testing.T) {
	executor, _, _ := testExecutor(t)
	executor.NodeID = ""

	_, err := executor.Health(context.Background())
	if !errors.Is(err, hostagent.ErrUnavailable) {
		t.Fatalf("Health() error = %v, want unavailable", err)
	}
}

func testVMExecutor(t *testing.T) (*Executor, hostagent.VMDesired, <-chan string) {
	t.Helper()

	executor, _, _ := testExecutor(t)
	root := t.TempDir()
	firmware := filepath.Join(root, "CLOUDHV.fd")

	disk := filepath.Join(root, "machine-a-root.qcow2")
	for path, content := range map[string]string{firmware: "firmware", disk: "disk"} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	apiSocket := filepath.Join(root, "api.sock")

	listener, err := net.Listen("unix", apiSocket)
	if err != nil {
		t.Fatal(err)
	}

	requests := make(chan string, 2)

	server := &http.Server{Handler: http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests <- request.URL.Path

		writer.WriteHeader(http.StatusNoContent)
	})}
	go func() { _ = server.Serve(listener) }()

	t.Cleanup(func() {
		_ = server.Shutdown(context.Background())
		_ = listener.Close()
	})

	firmwareHash := sha256.Sum256([]byte("firmware"))
	diskHash := sha256.Sum256([]byte("disk"))
	executor.FirmwareRoot = root
	executor.Artifacts = artifact.Builder{Root: root}

	return executor, hostagent.VMDesired{
		UID:            "machine-a",
		Firmware:       firmware,
		FirmwareSHA256: fmt.Sprintf("%x", firmwareHash),
		APISocket:      apiSocket,
		VhostSocket:    "/run/user/1000/k8snet/machine-a.sock",
		Disk:           disk,
		DiskSHA256:     fmt.Sprintf("%x", diskHash),
		MAC:            "02:00:00:00:00:01",
		CPUs:           1,
		MemoryMiB:      512,
	}, requests
}

func TestEnsureVMRejectsMissingFirmwareChecksum(t *testing.T) {
	executor, desired, requests := testVMExecutor(t)
	desired.FirmwareSHA256 = ""

	_, err := executor.EnsureVM(context.Background(), mutation(), desired)
	if !errors.Is(err, hostagent.ErrInvalidRequest) {
		t.Fatalf("EnsureVM() error = %v, want invalid request", err)
	}

	select {
	case request := <-requests:
		t.Fatalf("EnsureVM() made Cloud Hypervisor request %q", request)
	default:
	}
}

func TestEnsureVMRejectsFirmwareMismatchBeforeCreate(t *testing.T) {
	executor, desired, requests := testVMExecutor(t)
	desired.FirmwareSHA256 = "00" + desired.FirmwareSHA256[2:]

	_, err := executor.EnsureVM(context.Background(), mutation(), desired)
	if err == nil {
		t.Fatal("EnsureVM() accepted a firmware checksum mismatch")
	}

	select {
	case request := <-requests:
		t.Fatalf("EnsureVM() made Cloud Hypervisor request %q before rejecting firmware", request)
	default:
	}
}

func TestEnsureVMVerifiesFirmwareBeforeCreatingVM(t *testing.T) {
	executor, desired, requests := testVMExecutor(t)

	if _, err := executor.EnsureVM(context.Background(), mutation(), desired); err != nil {
		t.Fatalf("EnsureVM() error = %v", err)
	}

	for _, want := range []string{"/api/v1/vm.create", "/api/v1/vm.boot"} {
		select {
		case got := <-requests:
			if got != want {
				t.Fatalf("Cloud Hypervisor request = %q, want %q", got, want)
			}
		default:
			t.Fatalf("missing Cloud Hypervisor request %q", want)
		}
	}
}

func TestExecutorNeverReturnsFakeSuccessForUnimplementedHostOperations(t *testing.T) {
	executor, _, _ := testExecutor(t)

	_, err := executor.EnsureVM(context.Background(), mutation(), hostagent.VMDesired{})
	if !errors.Is(err, hostagent.ErrInvalidRequest) {
		t.Fatalf("EnsureVM() error=%v, want invalid request", err)
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

func seedNetworkResource(t *testing.T, store *inventory.Store, m hostagent.Mutation) {
	t.Helper()

	if err := store.UpsertNetworkResource(inventory.NetworkResource{
		InstallationID: m.Owner.InstallationID,
		OwnerUID:       m.Owner.UID,
		NodeID:         m.Owner.NodeID,
		Network:        "network-a",
		Port:           "port-a",
		MAC:            "02:00:00:00:00:01",
		IP:             "192.168.124.10",
	}); err != nil {
		t.Fatal(err)
	}
}

func TestPublishPortReplaysPersistedResultAfterInventoryReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")

	store, err := inventory.Open(path)
	if err != nil {
		t.Fatal(err)
	}

	m := mutation()
	seedNetworkResource(t, store, m)

	firstNetwork := &fakeNetwork{publishResult: 20100}
	first := &Executor{Store: store, Network: firstNetwork}

	got, err := first.PublishPort(context.Background(), m, 6443, 0)
	if err != nil || got != 20100 {
		t.Fatalf("first PublishPort() = %d, %v", got, err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = inventory.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	secondNetwork := &fakeNetwork{}
	second := &Executor{Store: store, Network: secondNetwork}

	got, err = second.PublishPort(context.Background(), m, 6443, 0)
	if err != nil || got != 20100 {
		t.Fatalf("replayed PublishPort() = %d, %v", got, err)
	}

	if len(secondNetwork.publishCalls) != 0 {
		t.Fatalf("replay published externally: %#v", secondNetwork.publishCalls)
	}
}

func TestReleasePortValidatesMappingAndReplaysWithoutUnpublish(t *testing.T) {
	executor, _, store := testExecutor(t)
	network := &fakeNetwork{publishResult: 20100}
	executor.Network = network
	publishMutation := mutation()
	publishMutation.IdempotencyKey = "publish-machine-a-1"
	seedNetworkResource(t, store, publishMutation)

	if _, err := executor.PublishPort(context.Background(), publishMutation, 6443, 0); err != nil {
		t.Fatal(err)
	}

	releaseMutation := mutation()

	releaseMutation.IdempotencyKey = "release-machine-a-1"
	if err := executor.ReleasePort(context.Background(), releaseMutation, 6443, 20101); !errors.Is(
		err,
		hostagent.ErrInvalidRequest,
	) {
		t.Fatalf("ReleasePort mismatched host port error = %v", err)
	}

	if len(network.unpublishCalls) != 0 {
		t.Fatalf("mismatched release unpublished: %#v", network.unpublishCalls)
	}

	if err := executor.ReleasePort(context.Background(), releaseMutation, 6443, 20100); err != nil {
		t.Fatal(err)
	}

	if err := executor.ReleasePort(context.Background(), releaseMutation, 6443, 20100); err != nil {
		t.Fatalf("release replay: %v", err)
	}

	if len(network.unpublishCalls) != 1 || network.unpublishCalls[0] != 6443 {
		t.Fatalf("unpublish calls = %#v", network.unpublishCalls)
	}
}

func TestDeletePortUnpublishesMappingsBeforeNetworkTeardown(t *testing.T) {
	executor, _, store := testExecutor(t)
	network := &fakeNetwork{}
	executor.Network = network
	m := mutation()
	seedNetworkResource(t, store, m)

	for _, mapping := range []inventory.PublishedPort{
		{InstallationID: m.Owner.InstallationID, OwnerUID: m.Owner.UID, GuestPort: 6443, HostPort: 20100},
		{InstallationID: m.Owner.InstallationID, OwnerUID: m.Owner.UID, GuestPort: 22, HostPort: 20022},
	} {
		if err := store.UpsertPublishedPort(mapping); err != nil {
			t.Fatal(err)
		}
	}

	if err := executor.DeletePort(context.Background(), m); err != nil {
		t.Fatal(err)
	}

	wantCalls := []string{"UnpublishPort", "UnpublishPort", "ReleaseIP", "DetachPort", "DeletePort"}
	if len(network.calls) != len(wantCalls) {
		t.Fatalf("calls = %#v, want %#v", network.calls, wantCalls)
	}

	for i := range wantCalls {
		if network.calls[i] != wantCalls[i] {
			t.Fatalf("calls = %#v, want %#v", network.calls, wantCalls)
		}
	}

	if len(network.unpublishCalls) != 2 || network.unpublishCalls[0] != 22 || network.unpublishCalls[1] != 6443 {
		t.Fatalf("unpublished = %#v", network.unpublishCalls)
	}

	if _, err := store.GetNetworkResource(m.Owner.InstallationID, m.Owner.UID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("network resource remains: %v", err)
	}
}

func TestDeletePortRetainsMappingWhenUnpublishFails(t *testing.T) {
	executor, _, store := testExecutor(t)
	network := &fakeNetwork{unpublishErr: errors.New("unpublish failed")}
	executor.Network = network
	m := mutation()
	seedNetworkResource(t, store, m)

	if err := store.UpsertPublishedPort(inventory.PublishedPort{
		InstallationID: m.Owner.InstallationID,
		OwnerUID:       m.Owner.UID,
		GuestPort:      6443,
		HostPort:       20100,
	}); err != nil {
		t.Fatal(err)
	}

	if err := executor.DeletePort(context.Background(), m); err == nil {
		t.Fatal("DeletePort() succeeded after unpublish failure")
	}

	if _, err := store.GetPublishedPort(m.Owner.InstallationID, m.Owner.UID, 6443); err != nil {
		t.Fatalf("published mapping was removed: %v", err)
	}

	if _, err := store.GetNetworkResource(m.Owner.InstallationID, m.Owner.UID); err != nil {
		t.Fatalf("network resource was removed: %v", err)
	}

	if len(network.calls) != 1 || network.calls[0] != "UnpublishPort" {
		t.Fatalf("calls after failed unpublish = %#v", network.calls)
	}
}

func TestDeleteVMRemovesOwnedDiskFamilyButNotUnrelatedArtifacts(t *testing.T) {
	executor, _, store := testExecutor(t)
	root := t.TempDir()
	executor.Artifacts.Root = root

	disk := filepath.Join(root, "machine-a-root.qcow2")
	apiSocket := filepath.Join(root, "machine-a-api.sock")

	ownedFiles := []string{
		disk,
		filepath.Join(root, "machine-a-cidata.img"),
		filepath.Join(root, "machine-a-cidata-parts", "network-config"),
		filepath.Join(root, "machine-a-data", "01.raw"),
		filepath.Join(root, "machine-a-confext-staging", "etc", "hostname"),
	}
	for _, path := range ownedFiles {
		if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
			t.Fatal(err)
		}

		if err := os.WriteFile(path, []byte(path), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	unrelated := filepath.Join(root, "other-machine-root.qcow2")
	if err := os.WriteFile(unrelated, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.UpsertVM(inventory.VM{
		InstallationID: mutation().Owner.InstallationID,
		OwnerUID:       mutation().Owner.UID,
		NodeID:         mutation().Owner.NodeID,
		Unit:           "k8slab-vm-machine-a.service",
		Disk:           disk,
		APISocket:      apiSocket,
	}); err != nil {
		t.Fatal(err)
	}

	if err := executor.DeleteVM(context.Background(), mutation()); err != nil {
		t.Fatal(err)
	}

	for _, path := range ownedFiles {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Errorf("owned artifact %q remains: %v", path, err)
		}
	}

	if _, err := os.Stat(unrelated); err != nil {
		t.Fatalf("unrelated artifact removed: %v", err)
	}
}

func TestDeleteVMRetainsDiskFamilyWhenRequested(t *testing.T) {
	executor, _, store := testExecutor(t)
	root := t.TempDir()
	executor.Artifacts.Root = root
	disk := filepath.Join(root, "machine-a-root.qcow2")

	cidata := filepath.Join(root, "machine-a-cidata.img")
	for _, path := range []string{disk, cidata} {
		if err := os.WriteFile(path, []byte("keep"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.UpsertVM(inventory.VM{
		InstallationID: mutation().Owner.InstallationID,
		OwnerUID:       mutation().Owner.UID,
		NodeID:         mutation().Owner.NodeID,
		Unit:           "k8slab-vm-machine-a.service",
		Disk:           disk,
	}); err != nil {
		t.Fatal(err)
	}

	requested := mutation()

	requested.RetainDisk = true
	if err := executor.DeleteVM(context.Background(), requested); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{disk, cidata} {
		if _, err := os.Stat(path); err != nil {
			t.Errorf("retained artifact %q missing: %v", path, err)
		}
	}
}

func TestDeleteVMDoesNotFollowOwnedSymlink(t *testing.T) {
	executor, _, store := testExecutor(t)
	root := t.TempDir()
	executor.Artifacts.Root = root
	external := t.TempDir()

	externalFile := filepath.Join(external, "keep")
	if err := os.WriteFile(externalFile, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}

	link := filepath.Join(root, "machine-a-data")
	if err := os.Symlink(external, link); err != nil {
		t.Fatal(err)
	}

	disk := filepath.Join(root, "machine-a-root.qcow2")
	if err := os.WriteFile(disk, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := store.UpsertVM(inventory.VM{
		InstallationID: mutation().Owner.InstallationID,
		OwnerUID:       mutation().Owner.UID,
		NodeID:         mutation().Owner.NodeID,
		Unit:           "k8slab-vm-machine-a.service",
		Disk:           disk,
	}); err != nil {
		t.Fatal(err)
	}

	if err := executor.DeleteVM(context.Background(), mutation()); err != nil {
		t.Fatal(err)
	}

	if _, err := os.Stat(externalFile); err != nil {
		t.Fatalf("external symlink target was removed: %v", err)
	}

	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Errorf("owned symlink remains: %v", err)
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
