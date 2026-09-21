package agentgrpc

import (
	"context"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Server struct {
	agentv1.UnimplementedHostAgentServer
	Capabilities hostagent.Capabilities
	Host         hostagent.HostAgent
}

func (s *Server) Health(_ context.Context, request *agentv1.HealthRequest) (*agentv1.Capabilities, error) {
	if request.GetProtocolMajor() != hostagent.ProtocolMajor {
		return nil, status.Errorf(codes.FailedPrecondition, "%v: got %d, want %d", hostagent.ErrProtocol, request.GetProtocolMajor(), hostagent.ProtocolMajor)
	}
	capabilities := s.Capabilities
	if s.Host != nil {
		var err error
		capabilities, err = s.Host.Health(context.Background())
		if err != nil {
			return nil, status.Error(codes.Unavailable, err.Error())
		}
	}
	return &agentv1.Capabilities{
		ProtocolMajor:       hostagent.ProtocolMajor,
		NodeId:              capabilities.NodeID,
		CloudHypervisor:     capabilities.CloudHypervisor,
		K8NetdProtocolMajor: capabilities.K8netdProtocolMajor,
		CanUseKvm:           capabilities.CanUseKVM,
		CanUseUserDbus:      capabilities.CanUseUserDBus,
	}, nil
}

func (s *Server) EnsureVM(ctx context.Context, request *agentv1.EnsureVMRequest) (*agentv1.VMResponse, error) {
	if request == nil || request.GetMutation() == nil || request.GetDesired() == nil {
		return nil, status.Error(codes.InvalidArgument, hostagent.ErrInvalidRequest.Error())
	}
	mutation, err := toMutation(request.GetMutation())
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if s.Host == nil {
		return nil, status.Error(codes.Unimplemented, "host-agent implementation is not configured")
	}
	desired := hostagent.VMDesired{
		UID: request.GetDesired().GetUid(), Name: request.GetDesired().GetName(),
		Image: request.GetDesired().GetImage(), Firmware: request.GetDesired().GetFirmware(),
		APISocket: request.GetDesired().GetApiSocket(), VhostSocket: request.GetDesired().GetVhostSocket(),
		Disk: request.GetDesired().GetDisk(), MAC: request.GetDesired().GetMac(), IP: request.GetDesired().GetIp(),
		CPUs: request.GetDesired().GetCpus(), MemoryMiB: request.GetDesired().GetMemoryMib(),
	}
	observed, err := s.Host.EnsureVM(ctx, mutation, desired)
	if err != nil {
		return nil, status.Error(codeFor(err), err.Error())
	}
	return &agentv1.VMResponse{Observed: &agentv1.VMObserved{
		Uid: observed.UID, Unit: observed.Unit, Pid: int64(observed.PID), GuestUptime: observed.GuestUptime,
		Running: observed.Running, ApiSocket: observed.APISocket, VhostSocket: observed.VhostSocket,
		Ip: observed.IP, Mac: observed.MAC, Generation: observed.Generation,
	}}, nil
}

func toMutation(value *agentv1.Mutation) (hostagent.Mutation, error) {
	if value.GetOwner() == nil {
		return hostagent.Mutation{}, hostagent.ErrInvalidRequest
	}
	mutation := hostagent.Mutation{
		ProtocolMajor: value.GetProtocolMajor(), Generation: value.GetGeneration(), IdempotencyKey: value.GetIdempotencyKey(),
		Owner: hostagent.Owner{InstallationID: value.GetOwner().GetInstallationId(), NodeID: value.GetOwner().GetNodeId(), UID: value.GetOwner().GetUid()},
	}
	return mutation, mutation.Validate()
}

func codeFor(err error) codes.Code {
	switch {
	case hostagent.IsInvalid(err):
		return codes.InvalidArgument
	case hostagent.IsProtocol(err):
		return codes.FailedPrecondition
	case hostagent.IsNotFound(err):
		return codes.NotFound
	case hostagent.IsConflict(err):
		return codes.Aborted
	case hostagent.IsUnavailable(err):
		return codes.Unavailable
	default:
		return codes.Internal
	}
}

var _ agentv1.HostAgentServer = (*Server)(nil)
