package agentgrpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

func TestServerImplementsEveryAdvertisedHostAgentRPC(t *testing.T) {
	server := &Server{Host: &recordingAgent{}}
	mutation := request().GetMutation()
	owner := mutation.GetOwner()
	ctx := context.Background()

	calls := []struct {
		name string
		call func() error
	}{
		{"health", func() error {
			_, err := server.Health(ctx, &agentv1.HealthRequest{ProtocolMajor: hostagent.ProtocolMajor})
			return err
		}},
		{"ensure-vm", func() error { _, err := server.EnsureVM(ctx, request()); return err }},
		{"get-vm", func() error {
			_, err := server.GetVM(ctx, &agentv1.GetVMRequest{Owner: owner, ProtocolMajor: hostagent.ProtocolMajor})
			return err
		}},
		{"stop-vm", func() error { _, err := server.StopVM(ctx, &agentv1.MutationRequest{Mutation: mutation}); return err }},
		{
			"delete-vm",
			func() error { _, err := server.DeleteVM(ctx, &agentv1.MutationRequest{Mutation: mutation}); return err },
		},
		{"ensure-network", func() error {
			_, err := server.EnsureNetwork(
				ctx,
				&agentv1.EnsureNetworkRequest{
					Mutation: mutation,
					Network:  &agentv1.NetworkRequest{Name: "net-a", Cidr: "192.168.124.0/24"},
				},
			)

			return err
		}},
		{"delete-network", func() error {
			_, err := server.DeleteNetwork(ctx, &agentv1.MutationRequest{Mutation: mutation})
			return err
		}},
		{"ensure-port", func() error {
			_, err := server.EnsurePort(
				ctx,
				&agentv1.EnsurePortRequest{
					Mutation: mutation,
					Port:     &agentv1.PortRequest{Name: "port-a", Network: "net-a", Mac: "02:00:00:00:00:01"},
				},
			)

			return err
		}},
		{"delete-port", func() error {
			_, err := server.DeletePort(ctx, &agentv1.MutationRequest{Mutation: mutation})
			return err
		}},
		{"allocate-ip", func() error {
			_, err := server.AllocateIP(ctx, &agentv1.MutationRequest{Mutation: mutation})
			return err
		}},
		{"release-ip", func() error {
			_, err := server.ReleaseIP(ctx, &agentv1.ReleaseIPRequest{Mutation: mutation, Ip: "192.168.124.10"})
			return err
		}},
		{"publish-port", func() error {
			_, err := server.PublishPort(ctx, &agentv1.PublishPortRequest{Mutation: mutation, GuestPort: 6443, HostPort: 0})
			return err
		}},
		{"release-port", func() error {
			_, err := server.ReleasePort(ctx, &agentv1.ReleasePortRequest{Mutation: mutation, GuestPort: 6443, HostPort: 30000})
			return err
		}},
		{"diagnostics", func() error {
			_, err := server.Diagnostics(ctx, &agentv1.DiagnosticsRequest{Owner: owner, ProtocolMajor: hostagent.ProtocolMajor})
			return err
		}},
		{"acquire-probe", func() error {
			_, err := server.AcquireProbe(ctx, &agentv1.AcquireProbeRequest{Mutation: mutation, Network: "net-a"})
			return err
		}},
		{"release-probe", func() error {
			_, err := server.ReleaseProbe(ctx, &agentv1.ReleaseProbeRequest{Mutation: mutation, Uid: "probe-a"})
			return err
		}},
	}
	for _, testCase := range calls {
		t.Run(testCase.name, func(t *testing.T) {
			if err := testCase.call(); err != nil {
				t.Fatalf("RPC returned %v", err)
			}
		})
	}
}

func TestReadOnlyRPCsRejectWrongProtocolBeforeHostCall(t *testing.T) {
	server := &Server{Host: &recordingAgent{}}

	_, err := server.GetVM(
		context.Background(),
		&agentv1.GetVMRequest{Owner: request().GetMutation().GetOwner(), ProtocolMajor: hostagent.ProtocolMajor + 1},
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("GetVM() error=%v, want FailedPrecondition", err)
	}

	_, err = server.Diagnostics(
		context.Background(),
		&agentv1.DiagnosticsRequest{Owner: request().GetMutation().GetOwner(), ProtocolMajor: hostagent.ProtocolMajor + 1},
	)
	if status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("Diagnostics() error=%v, want FailedPrecondition", err)
	}
}

func TestServerWithoutHostReturnsUnavailable(t *testing.T) {
	server := &Server{}

	_, err := server.DeleteVM(context.Background(), &agentv1.MutationRequest{Mutation: request().GetMutation()})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("DeleteVM() error=%v, want Unavailable", err)
	}
}
