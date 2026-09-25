package agentgrpc

import (
	"context"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

type recordingAgent struct {
	called bool
}

func (r *recordingAgent) Health(context.Context) (hostagent.Capabilities, error) {
	return hostagent.Capabilities{NodeID: "node-a"}, nil
}

func (r *recordingAgent) EnsureVM(
	context.Context,
	hostagent.Mutation,
	hostagent.VMDesired,
) (hostagent.VMObserved, error) {
	r.called = true
	return hostagent.VMObserved{UID: "machine-a", Running: true}, nil
}

func (*recordingAgent) GetVM(context.Context, hostagent.Owner) (hostagent.VMObserved, error) {
	return hostagent.VMObserved{UID: "machine-a", Running: true}, nil
}
func (*recordingAgent) StopVM(context.Context, hostagent.Mutation) error   { return nil }
func (*recordingAgent) DeleteVM(context.Context, hostagent.Mutation) error { return nil }
func (*recordingAgent) EnsureNetwork(context.Context, hostagent.Mutation, hostagent.NetworkRequest) error {
	return nil
}
func (*recordingAgent) DeleteNetwork(context.Context, hostagent.Mutation) error { return nil }

func (*recordingAgent) EnsurePort(
	context.Context,
	hostagent.Mutation,
	hostagent.PortRequest,
) (hostagent.PortObserved, error) {
	return hostagent.PortObserved{}, nil
}
func (*recordingAgent) DeletePort(context.Context, hostagent.Mutation) error { return nil }
func (*recordingAgent) AllocateIP(context.Context, hostagent.Mutation) (string, error) {
	return "", nil
}
func (*recordingAgent) ReleaseIP(context.Context, hostagent.Mutation, string) error { return nil }
func (*recordingAgent) PublishPort(context.Context, hostagent.Mutation, uint32, uint32) (uint32, error) {
	return 0, nil
}

func (*recordingAgent) ReleasePort(context.Context, hostagent.Mutation, uint32, uint32) error {
	return nil
}

func (*recordingAgent) Diagnostics(context.Context, hostagent.Owner) (hostagent.Diagnostics, error) {
	return hostagent.Diagnostics{}, nil
}

func (*recordingAgent) AcquireProbe(context.Context, hostagent.Mutation, string) (hostagent.ProbeLease, error) {
	return hostagent.ProbeLease{}, nil
}

func (*recordingAgent) ReleaseProbe(context.Context, hostagent.Mutation, string) error { return nil }

func (*recordingAgent) PrepareRootDisk(
	context.Context,
	hostagent.Mutation,
	hostagent.RootDiskRequest,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, nil
}

func (*recordingAgent) PrepareCIDATA(
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, nil
}

func (*recordingAgent) PrepareConfext(
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, nil
}

func request() *agentv1.EnsureVMRequest {
	return &agentv1.EnsureVMRequest{
		Mutation: &agentv1.Mutation{
			ProtocolMajor: hostagent.ProtocolMajor,
			Owner:         &agentv1.Owner{InstallationId: "install-a", NodeId: "node-a", Uid: "machine-a"},
			Generation:    1, IdempotencyKey: "machine-a-1",
		},
		Desired: &agentv1.VMDesired{Uid: "machine-a", Name: "vm-a"},
	}
}

func TestEnsureVMRejectsWrongProtocolBeforeHostCall(t *testing.T) {
	agent := &recordingAgent{}
	server := &Server{Host: agent}
	req := request()
	req.Mutation.ProtocolMajor++

	_, err := server.EnsureVM(context.Background(), req)
	if status.Code(err) != codes.FailedPrecondition || agent.called {
		t.Fatalf("EnsureVM() error=%v called=%v, want failed precondition without host call", err, agent.called)
	}
}

func TestEnsureVMCallsHostAfterValidation(t *testing.T) {
	agent := &recordingAgent{}
	server := &Server{Host: agent}

	response, err := server.EnsureVM(context.Background(), request())
	if err != nil || response.GetObserved().GetUid() != "machine-a" || !agent.called {
		t.Fatalf("EnsureVM() response=%v error=%v called=%v", response, err, agent.called)
	}
}
