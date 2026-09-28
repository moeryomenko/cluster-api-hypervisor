package inventory

import (
	"errors"
	"path/filepath"
	"testing"
)

func TestStoreUsesWALAndPersistsVM(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	vm := VM{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Unit:           "k8slab-vm-machine-a.service",
		Generation:     1,
		PID:            42,
		Disk:           "/state/a.qcow2",
		APISocket:      "/state/a.sock",
		VhostSocket:    "/run/a.sock",
		Network:        "net-a",
		Port:           "port-a",
		MAC:            "02:00:00:00:00:01",
		IP:             "192.168.124.10",
	}
	if err := store.UpsertVM(vm); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	got, err := store.GetVM("install-a", "machine-a")
	if err != nil {
		t.Fatal(err)
	}

	if got.Unit != vm.Unit || got.IP != vm.IP || got.PID != vm.PID {
		t.Fatalf("GetVM() = %#v, want %#v", got, vm)
	}
}

func TestPublishedPortsPersistInGuestPortOrderAcrossReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	resource := NetworkResource{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Network:        "network-a",
		Port:           "port-a",
		MAC:            "02:00:00:00:00:01",
		IP:             "192.168.124.10",
	}
	if err := store.UpsertNetworkResource(resource); err != nil {
		t.Fatal(err)
	}

	for _, mapping := range []PublishedPort{
		{InstallationID: resource.InstallationID, OwnerUID: resource.OwnerUID, GuestPort: 6443, HostPort: 20001},
		{InstallationID: resource.InstallationID, OwnerUID: resource.OwnerUID, GuestPort: 22, HostPort: 20000},
	} {
		if err := store.UpsertPublishedPort(mapping); err != nil {
			t.Fatal(err)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	ports, err := store.ListPublishedPorts(resource.InstallationID, resource.OwnerUID)
	if err != nil {
		t.Fatal(err)
	}

	if len(ports) != 2 || ports[0].GuestPort != 22 || ports[1].GuestPort != 6443 {
		t.Fatalf("ListPublishedPorts() = %#v", ports)
	}

	if ports[0].HostPort != 20000 || ports[1].HostPort != 20001 {
		t.Fatalf("ListPublishedPorts() = %#v", ports)
	}
}

func TestRecordOperationIsReplaySafeButRejectsConflictingGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	if err := store.RecordOperation("install-a", "machine-a", "key-a", 1); err != nil {
		t.Fatal(err)
	}

	if err := store.RecordOperation("install-a", "machine-a", "key-a", 1); err != nil {
		t.Fatalf("replay: %v", err)
	}

	if err := store.RecordOperation("install-a", "machine-a", "key-a", 2); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("conflict error = %v", err)
	}
}

func TestNetworkResourceLookupByPortPreservesOwnershipAndInstallationScope(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	first := NetworkResource{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Network:        "network-a",
		Port:           "port-a",
		MAC:            "02:00:00:00:00:01",
		IP:             "192.168.124.10",
	}
	if err := store.UpsertNetworkResource(first); err != nil {
		t.Fatal(err)
	}

	got, err := store.GetNetworkResourceByPort(first.InstallationID, first.Port)
	if err != nil || got != first {
		t.Fatalf("GetNetworkResourceByPort() = %#v, %v", got, err)
	}

	if err := store.UpsertNetworkResource(NetworkResource{
		InstallationID: first.InstallationID,
		OwnerUID:       "machine-b",
		NodeID:         "node-a",
		Network:        "network-a",
		Port:           first.Port,
		MAC:            "02:00:00:00:00:02",
		IP:             "192.168.124.11",
	}); err == nil {
		t.Fatal("cross-owner port claim succeeded")
	}

	if err := store.UpsertNetworkResource(NetworkResource{
		InstallationID: "install-b",
		OwnerUID:       "machine-b",
		NodeID:         "node-b",
		Network:        "network-b",
		Port:           first.Port,
		MAC:            "02:00:00:00:00:02",
		IP:             "192.168.125.10",
	}); err != nil {
		t.Fatalf("cross-installation port reuse failed: %v", err)
	}
}
