package executor

import (
	"context"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

func TestEnsureVMPersistsOwnedSystemdUnit(t *testing.T) {
	executor, systemdClient, _ := testExecutor(t)
	mutation := mutation()
	mutation.IdempotencyKey = "ensure-machine-a-1"

	observed, err := executor.EnsureVM(
		context.Background(),
		mutation,
		hostagent.VMDesired{
			UID:         "machine-a",
			Disk:        "/host-state/vms/machine-a-root.qcow2",
			Firmware:    "/host-state/CLOUDHV.fd",
			APISocket:   "/host-state/vms/machine-a.sock",
			VhostSocket: "/run/user/1000/k8snet/machine-a.sock",
			MAC:         "02:00:00:00:00:01",
			CPUs:        1,
			MemoryMiB:   512,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	if observed.Unit == "" || observed.PID != 41 {
		t.Fatalf("observed=%#v", observed)
	}

	if _, err := executor.EnsureVM(context.Background(), mutation, hostagent.VMDesired{UID: "machine-a", Disk: "/host-state/vms/machine-a-root.qcow2", Firmware: "/host-state/CLOUDHV.fd", APISocket: "/host-state/vms/machine-a.sock", VhostSocket: "/run/user/1000/k8snet/machine-a.sock", MAC: "02:00:00:00:00:01", CPUs: 1, MemoryMiB: 512}); err != nil {
		t.Fatal(err)
	}

	if len(systemdClient.units) != 1 {
		t.Fatalf("units=%#v", systemdClient.units)
	}
}
