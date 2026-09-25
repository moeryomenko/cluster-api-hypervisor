package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

func TestEnsureVMFailsClosedWhenUnitDoesNotExposeAPISocket(t *testing.T) {
	executor, _, _ := testExecutor(t)
	mutation := mutation()
	mutation.IdempotencyKey = "ensure-machine-a-1"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := executor.EnsureVM(
		ctx,
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
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("EnsureVM() error=%v, want canceled", err)
	}
}
