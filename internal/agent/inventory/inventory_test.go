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
	vm := VM{InstallationID: "install-a", OwnerUID: "machine-a", NodeID: "node-a", Unit: "k8slab-vm-machine-a.service", Generation: 1, PID: 42, Disk: "/state/a.qcow2", APISocket: "/state/a.sock", VhostSocket: "/run/a.sock", Network: "net-a", Port: "port-a", MAC: "02:00:00:00:00:01", IP: "192.168.124.10"}
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
	defer store.Close()
	got, err := store.GetVM("install-a", "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if got.Unit != vm.Unit || got.IP != vm.IP || got.PID != vm.PID {
		t.Fatalf("GetVM() = %#v, want %#v", got, vm)
	}
}

func TestRecordOperationIsReplaySafeButRejectsConflictingGeneration(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
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
