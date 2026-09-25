package agentgrpc

import (
	"context"
	"fmt"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

type Server struct {
	agentv1.UnimplementedHostAgentServer
	Capabilities hostagent.Capabilities
	Host         hostagent.HostAgent
}

func (s *Server) PrepareRootDisk(
	ctx context.Context,
	request *agentv1.PrepareRootDiskRequest,
) (*agentv1.ArtifactResponse, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	if request.GetName() == "" || request.GetSourceImage() == "" {
		return nil, invalidRequest()
	}

	result, err := s.host().
		PrepareRootDisk(ctx, mutation, hostagent.RootDiskRequest{Name: request.GetName(), SourceImage: request.GetSourceImage()})
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.ArtifactResponse{Paths: result.Paths, Sha256: result.SHA256s}, nil
}

func (s *Server) PrepareCIDATA(
	ctx context.Context,
	request *agentv1.PrepareCIDATARequest,
) (*agentv1.ArtifactResponse, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	files := artifactFiles(request.GetFiles())
	if request.GetName() == "" || len(files) == 0 {
		return nil, invalidRequest()
	}

	result, err := s.host().PrepareCIDATA(ctx, mutation, request.GetName(), files)
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.ArtifactResponse{Paths: result.Paths, Sha256: result.SHA256s}, nil
}

func (s *Server) PrepareConfext(
	ctx context.Context,
	request *agentv1.PrepareConfextRequest,
) (*agentv1.ArtifactResponse, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	files := artifactFiles(request.GetFiles())
	if request.GetName() == "" || len(files) == 0 {
		return nil, invalidRequest()
	}

	result, err := s.host().PrepareConfext(ctx, mutation, request.GetName(), files)
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.ArtifactResponse{Paths: result.Paths, Sha256: result.SHA256s}, nil
}

func artifactFiles(files []*agentv1.ArtifactFile) []hostagent.ArtifactFile {
	result := make([]hostagent.ArtifactFile, 0, len(files))
	for _, file := range files {
		result = append(result, hostagent.ArtifactFile{Name: file.GetName(), Content: file.GetContent()})
	}

	return result
}

func (s *Server) Health(ctx context.Context, request *agentv1.HealthRequest) (*agentv1.Capabilities, error) {
	if request.GetProtocolMajor() != hostagent.ProtocolMajor {
		return nil, status.Errorf(
			codes.FailedPrecondition,
			"%v: got %d, want %d",
			hostagent.ErrProtocol,
			request.GetProtocolMajor(),
			hostagent.ProtocolMajor,
		)
	}

	capabilities := s.Capabilities
	if s.Host != nil {
		var err error

		capabilities, err = s.Host.Health(ctx)
		if err != nil {
			return nil, status.Error(codeFor(err), err.Error())
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
	if request == nil || request.GetDesired() == nil {
		return nil, invalidRequest()
	}

	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	observed, err := s.host().EnsureVM(ctx, mutation, vmDesiredFrom(request.GetDesired()))
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.VMResponse{Observed: vmObservedTo(observed)}, nil
}

func (s *Server) GetVM(ctx context.Context, request *agentv1.GetVMRequest) (*agentv1.VMResponse, error) {
	owner, err := ownerFrom(request.GetOwner(), request.GetProtocolMajor())
	if err != nil {
		return nil, rpcError(err)
	}

	observed, err := s.host().GetVM(ctx, owner)
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.VMResponse{Observed: vmObservedTo(observed)}, nil
}

func (s *Server) StopVM(ctx context.Context, request *agentv1.MutationRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	return empty(s.host().StopVM(ctx, mutation))
}

func (s *Server) DeleteVM(ctx context.Context, request *agentv1.MutationRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	return empty(s.host().DeleteVM(ctx, mutation))
}

func (s *Server) EnsureNetwork(ctx context.Context, request *agentv1.EnsureNetworkRequest) (*agentv1.Empty, error) {
	if request == nil || request.GetNetwork() == nil {
		return nil, invalidRequest()
	}

	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	return empty(
		s.host().
			EnsureNetwork(ctx, mutation, hostagent.NetworkRequest{Name: request.GetNetwork().GetName(), CIDR: request.GetNetwork().GetCidr()}),
	)
}

func (s *Server) DeleteNetwork(ctx context.Context, request *agentv1.MutationRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	return empty(s.host().DeleteNetwork(ctx, mutation))
}

func (s *Server) EnsurePort(ctx context.Context, request *agentv1.EnsurePortRequest) (*agentv1.PortResponse, error) {
	if request == nil || request.GetPort() == nil {
		return nil, invalidRequest()
	}

	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	observed, err := s.host().
		EnsurePort(ctx, mutation, hostagent.PortRequest{Name: request.GetPort().GetName(), Network: request.GetPort().GetNetwork(), MAC: request.GetPort().GetMac()})
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.PortResponse{Observed: portObservedTo(observed)}, nil
}

func (s *Server) DeletePort(ctx context.Context, request *agentv1.MutationRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	return empty(s.host().DeletePort(ctx, mutation))
}

func (s *Server) AllocateIP(
	ctx context.Context,
	request *agentv1.MutationRequest,
) (*agentv1.AllocateIPResponse, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	ip, err := s.host().AllocateIP(ctx, mutation)
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.AllocateIPResponse{Ip: ip}, nil
}

func (s *Server) ReleaseIP(ctx context.Context, request *agentv1.ReleaseIPRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	if request.GetIp() == "" {
		return nil, invalidRequest()
	}

	return empty(s.host().ReleaseIP(ctx, mutation, request.GetIp()))
}

func (s *Server) PublishPort(
	ctx context.Context,
	request *agentv1.PublishPortRequest,
) (*agentv1.PublishPortResponse, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	hostPort, err := s.host().PublishPort(ctx, mutation, request.GetGuestPort(), request.GetHostPort())
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.PublishPortResponse{HostPort: hostPort}, nil
}

func (s *Server) ReleasePort(ctx context.Context, request *agentv1.ReleasePortRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	return empty(s.host().ReleasePort(ctx, mutation, request.GetGuestPort(), request.GetHostPort()))
}

func (s *Server) Diagnostics(
	ctx context.Context,
	request *agentv1.DiagnosticsRequest,
) (*agentv1.DiagnosticsResponse, error) {
	owner, err := ownerFrom(request.GetOwner(), request.GetProtocolMajor())
	if err != nil {
		return nil, rpcError(err)
	}

	diagnostics, err := s.host().Diagnostics(ctx, owner)
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.DiagnosticsResponse{
		Diagnostics: &agentv1.Diagnostics{OwnerUid: diagnostics.OwnerUID, Messages: diagnostics.Messages},
	}, nil
}

func (s *Server) AcquireProbe(
	ctx context.Context,
	request *agentv1.AcquireProbeRequest,
) (*agentv1.ProbeResponse, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	if request.GetNetwork() == "" {
		return nil, invalidRequest()
	}

	lease, err := s.host().AcquireProbe(ctx, mutation, request.GetNetwork())
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.ProbeResponse{Lease: probeLeaseTo(lease)}, nil
}

func (s *Server) ReleaseProbe(ctx context.Context, request *agentv1.ReleaseProbeRequest) (*agentv1.Empty, error) {
	mutation, err := mutationFrom(request.GetMutation())
	if err != nil {
		return nil, rpcError(err)
	}

	if request.GetUid() == "" {
		return nil, invalidRequest()
	}

	return empty(s.host().ReleaseProbe(ctx, mutation, request.GetUid()))
}

func (s *Server) host() hostagent.HostAgent {
	if s.Host == nil {
		return unavailableAgent{}
	}

	return s.Host
}

type unavailableAgent struct{}

func (a unavailableAgent) Health(context.Context) (hostagent.Capabilities, error) {
	return hostagent.Capabilities{}, a.unavailable()
}

func (unavailableAgent) unavailable() error {
	return fmt.Errorf("%w: implementation is not configured", hostagent.ErrUnavailable)
}

func (a unavailableAgent) EnsureVM(
	context.Context,
	hostagent.Mutation,
	hostagent.VMDesired,
) (hostagent.VMObserved, error) {
	return hostagent.VMObserved{}, a.unavailable()
}

func (a unavailableAgent) GetVM(context.Context, hostagent.Owner) (hostagent.VMObserved, error) {
	return hostagent.VMObserved{}, a.unavailable()
}

func (a unavailableAgent) StopVM(context.Context, hostagent.Mutation) error { return a.unavailable() }

func (a unavailableAgent) DeleteVM(context.Context, hostagent.Mutation) error { return a.unavailable() }

func (a unavailableAgent) EnsureNetwork(context.Context, hostagent.Mutation, hostagent.NetworkRequest) error {
	return a.unavailable()
}

func (a unavailableAgent) DeleteNetwork(context.Context, hostagent.Mutation) error {
	return a.unavailable()
}

func (a unavailableAgent) EnsurePort(
	context.Context,
	hostagent.Mutation,
	hostagent.PortRequest,
) (hostagent.PortObserved, error) {
	return hostagent.PortObserved{}, a.unavailable()
}

func (a unavailableAgent) DeletePort(context.Context, hostagent.Mutation) error {
	return a.unavailable()
}

func (a unavailableAgent) AllocateIP(context.Context, hostagent.Mutation) (string, error) {
	return "", a.unavailable()
}

func (a unavailableAgent) ReleaseIP(context.Context, hostagent.Mutation, string) error {
	return a.unavailable()
}

func (a unavailableAgent) PublishPort(context.Context, hostagent.Mutation, uint32, uint32) (uint32, error) {
	return 0, a.unavailable()
}

func (a unavailableAgent) ReleasePort(context.Context, hostagent.Mutation, uint32, uint32) error {
	return a.unavailable()
}

func (a unavailableAgent) Diagnostics(context.Context, hostagent.Owner) (hostagent.Diagnostics, error) {
	return hostagent.Diagnostics{}, a.unavailable()
}

func (a unavailableAgent) AcquireProbe(context.Context, hostagent.Mutation, string) (hostagent.ProbeLease, error) {
	return hostagent.ProbeLease{}, a.unavailable()
}

func (a unavailableAgent) ReleaseProbe(context.Context, hostagent.Mutation, string) error {
	return a.unavailable()
}

func (a unavailableAgent) PrepareRootDisk(
	context.Context,
	hostagent.Mutation,
	hostagent.RootDiskRequest,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, a.unavailable()
}

func (a unavailableAgent) PrepareCIDATA(
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, a.unavailable()
}

func (a unavailableAgent) PrepareConfext(
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, a.unavailable()
}

func mutationFrom(value *agentv1.Mutation) (hostagent.Mutation, error) {
	if value == nil || value.GetOwner() == nil {
		return hostagent.Mutation{}, hostagent.ErrInvalidRequest
	}

	mutation := hostagent.Mutation{
		ProtocolMajor:  value.GetProtocolMajor(),
		Generation:     value.GetGeneration(),
		IdempotencyKey: value.GetIdempotencyKey(),
		Owner: hostagent.Owner{
			InstallationID: value.GetOwner().GetInstallationId(),
			NodeID:         value.GetOwner().GetNodeId(),
			UID:            value.GetOwner().GetUid(),
		},
	}

	return mutation, mutation.Validate()
}

func ownerFrom(value *agentv1.Owner, protocol uint32) (hostagent.Owner, error) {
	if protocol != hostagent.ProtocolMajor {
		return hostagent.Owner{}, fmt.Errorf("%w: got %d, want %d", hostagent.ErrProtocol, protocol, hostagent.ProtocolMajor)
	}

	owner := hostagent.Owner{InstallationID: value.GetInstallationId(), NodeID: value.GetNodeId(), UID: value.GetUid()}

	return owner, owner.Validate()
}

func vmDesiredFrom(value *agentv1.VMDesired) hostagent.VMDesired {
	return hostagent.VMDesired{
		UID:         value.GetUid(),
		Name:        value.GetName(),
		Image:       value.GetImage(),
		Firmware:    value.GetFirmware(),
		APISocket:   value.GetApiSocket(),
		VhostSocket: value.GetVhostSocket(),
		Disk:        value.GetDisk(),
		MAC:         value.GetMac(),
		IP:          value.GetIp(),
		CPUs:        value.GetCpus(),
		MemoryMiB:   value.GetMemoryMib(),
	}
}

func vmObservedTo(value hostagent.VMObserved) *agentv1.VMObserved {
	return &agentv1.VMObserved{
		Uid:         value.UID,
		Unit:        value.Unit,
		Pid:         int64(value.PID),
		GuestUptime: value.GuestUptime,
		Running:     value.Running,
		ApiSocket:   value.APISocket,
		VhostSocket: value.VhostSocket,
		Ip:          value.IP,
		Mac:         value.MAC,
		Generation:  value.Generation,
	}
}

func portObservedTo(value hostagent.PortObserved) *agentv1.PortObserved {
	return &agentv1.PortObserved{
		Name:      value.Name,
		Network:   value.Network,
		Mac:       value.MAC,
		Ip:        value.IP,
		Published: value.Published,
	}
}

func probeLeaseTo(value hostagent.ProbeLease) *agentv1.ProbeLease {
	return &agentv1.ProbeLease{
		Uid:       value.UID,
		Network:   value.Network,
		Port:      value.Port,
		Mac:       value.MAC,
		Ip:        value.IP,
		ExpiresAt: value.ExpiresAt,
	}
}

func empty(err error) (*agentv1.Empty, error) {
	if err != nil {
		return nil, rpcError(err)
	}

	return &agentv1.Empty{}, nil
}

func invalidRequest() error {
	return status.Error(codes.InvalidArgument, hostagent.ErrInvalidRequest.Error())
}
func rpcError(err error) error { return status.Error(codeFor(err), err.Error()) }

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
