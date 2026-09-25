package agentgrpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

type Client struct {
	connection *grpc.ClientConn
	service    agentv1.HostAgentClient
}

func Dial(
	ctx context.Context,
	address, serverName string,
	certificate tls.Certificate,
	roots *x509.CertPool,
) (*Client, error) {
	connection, err := grpc.NewClient(
		address,
		grpc.WithTransportCredentials(
			credentials.NewTLS(
				&tls.Config{
					MinVersion:   tls.VersionTLS13,
					ServerName:   serverName,
					Certificates: []tls.Certificate{certificate},
					RootCAs:      roots,
				},
			),
		),
	)
	if err != nil {
		return nil, fmt.Errorf("dial host agent: %w", err)
	}

	return &Client{connection: connection, service: agentv1.NewHostAgentClient(connection)}, nil
}

func (c *Client) Close() error { return c.connection.Close() }

func (c *Client) Health(ctx context.Context) (hostagent.Capabilities, error) {
	response, err := c.service.Health(ctx, &agentv1.HealthRequest{ProtocolMajor: hostagent.ProtocolMajor})
	if err != nil {
		return hostagent.Capabilities{}, mapError(err)
	}

	return hostagent.Capabilities{
		NodeID:              response.GetNodeId(),
		CloudHypervisor:     response.GetCloudHypervisor(),
		K8netdProtocolMajor: response.GetK8NetdProtocolMajor(),
		CanUseKVM:           response.GetCanUseKvm(),
		CanUseUserDBus:      response.GetCanUseUserDbus(),
	}, nil
}

func (c *Client) EnsureVM(
	ctx context.Context,
	mutation hostagent.Mutation,
	desired hostagent.VMDesired,
) (hostagent.VMObserved, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.VMObserved{}, err
	}

	response, err := c.service.EnsureVM(
		ctx,
		&agentv1.EnsureVMRequest{Mutation: mutationTo(mutation), Desired: vmDesiredTo(desired)},
	)
	if err != nil {
		return hostagent.VMObserved{}, mapError(err)
	}

	return vmObservedFrom(response.GetObserved()), nil
}

func (c *Client) GetVM(ctx context.Context, owner hostagent.Owner) (hostagent.VMObserved, error) {
	if err := owner.Validate(); err != nil {
		return hostagent.VMObserved{}, err
	}

	response, err := c.service.GetVM(
		ctx,
		&agentv1.GetVMRequest{Owner: ownerTo(owner), ProtocolMajor: hostagent.ProtocolMajor},
	)
	if err != nil {
		return hostagent.VMObserved{}, mapError(err)
	}

	return vmObservedFrom(response.GetObserved()), nil
}

func (c *Client) StopVM(ctx context.Context, mutation hostagent.Mutation) error {
	_, err := c.service.StopVM(ctx, &agentv1.MutationRequest{Mutation: mutationTo(mutation)})
	return mapError(err)
}

func (c *Client) DeleteVM(ctx context.Context, mutation hostagent.Mutation) error {
	_, err := c.service.DeleteVM(ctx, &agentv1.MutationRequest{Mutation: mutationTo(mutation)})
	return mapError(err)
}

func (c *Client) EnsureNetwork(
	ctx context.Context,
	mutation hostagent.Mutation,
	network hostagent.NetworkRequest,
) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	_, err := c.service.EnsureNetwork(
		ctx,
		&agentv1.EnsureNetworkRequest{
			Mutation: mutationTo(mutation),
			Network:  &agentv1.NetworkRequest{Name: network.Name, Cidr: network.CIDR},
		},
	)

	return mapError(err)
}

func (c *Client) DeleteNetwork(ctx context.Context, mutation hostagent.Mutation) error {
	_, err := c.service.DeleteNetwork(ctx, &agentv1.MutationRequest{Mutation: mutationTo(mutation)})
	return mapError(err)
}

func (c *Client) EnsurePort(
	ctx context.Context,
	mutation hostagent.Mutation,
	port hostagent.PortRequest,
) (hostagent.PortObserved, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.PortObserved{}, err
	}

	response, err := c.service.EnsurePort(
		ctx,
		&agentv1.EnsurePortRequest{
			Mutation: mutationTo(mutation),
			Port:     &agentv1.PortRequest{Name: port.Name, Network: port.Network, Mac: port.MAC},
		},
	)
	if err != nil {
		return hostagent.PortObserved{}, mapError(err)
	}

	return portObservedFrom(response.GetObserved()), nil
}

func (c *Client) DeletePort(ctx context.Context, mutation hostagent.Mutation) error {
	_, err := c.service.DeletePort(ctx, &agentv1.MutationRequest{Mutation: mutationTo(mutation)})
	return mapError(err)
}

func (c *Client) AllocateIP(ctx context.Context, mutation hostagent.Mutation) (string, error) {
	if err := mutation.Validate(); err != nil {
		return "", err
	}

	response, err := c.service.AllocateIP(ctx, &agentv1.MutationRequest{Mutation: mutationTo(mutation)})
	if err != nil {
		return "", mapError(err)
	}

	return response.GetIp(), nil
}

func (c *Client) ReleaseIP(ctx context.Context, mutation hostagent.Mutation, ip string) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if ip == "" {
		return hostagent.ErrInvalidRequest
	}

	_, err := c.service.ReleaseIP(ctx, &agentv1.ReleaseIPRequest{Mutation: mutationTo(mutation), Ip: ip})

	return mapError(err)
}

func (c *Client) PublishPort(
	ctx context.Context,
	mutation hostagent.Mutation,
	guestPort, hostPort uint32,
) (uint32, error) {
	if err := mutation.Validate(); err != nil {
		return 0, err
	}

	response, err := c.service.PublishPort(
		ctx,
		&agentv1.PublishPortRequest{Mutation: mutationTo(mutation), GuestPort: guestPort, HostPort: hostPort},
	)
	if err != nil {
		return 0, mapError(err)
	}

	return response.GetHostPort(), nil
}

func (c *Client) ReleasePort(ctx context.Context, mutation hostagent.Mutation, guestPort, hostPort uint32) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	_, err := c.service.ReleasePort(
		ctx,
		&agentv1.ReleasePortRequest{Mutation: mutationTo(mutation), GuestPort: guestPort, HostPort: hostPort},
	)

	return mapError(err)
}

func (c *Client) Diagnostics(ctx context.Context, owner hostagent.Owner) (hostagent.Diagnostics, error) {
	if err := owner.Validate(); err != nil {
		return hostagent.Diagnostics{}, err
	}

	response, err := c.service.Diagnostics(
		ctx,
		&agentv1.DiagnosticsRequest{Owner: ownerTo(owner), ProtocolMajor: hostagent.ProtocolMajor},
	)
	if err != nil {
		return hostagent.Diagnostics{}, mapError(err)
	}

	diagnostics := response.GetDiagnostics()

	return hostagent.Diagnostics{OwnerUID: diagnostics.GetOwnerUid(), Messages: diagnostics.GetMessages()}, nil
}

func (c *Client) AcquireProbe(
	ctx context.Context,
	mutation hostagent.Mutation,
	network string,
) (hostagent.ProbeLease, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.ProbeLease{}, err
	}

	if network == "" {
		return hostagent.ProbeLease{}, hostagent.ErrInvalidRequest
	}

	response, err := c.service.AcquireProbe(
		ctx,
		&agentv1.AcquireProbeRequest{Mutation: mutationTo(mutation), Network: network},
	)
	if err != nil {
		return hostagent.ProbeLease{}, mapError(err)
	}

	return probeLeaseFrom(response.GetLease()), nil
}

func (c *Client) ReleaseProbe(ctx context.Context, mutation hostagent.Mutation, uid string) error {
	if err := mutation.Validate(); err != nil {
		return err
	}

	if uid == "" {
		return hostagent.ErrInvalidRequest
	}

	_, err := c.service.ReleaseProbe(ctx, &agentv1.ReleaseProbeRequest{Mutation: mutationTo(mutation), Uid: uid})

	return mapError(err)
}

func ownerTo(owner hostagent.Owner) *agentv1.Owner {
	return &agentv1.Owner{InstallationId: owner.InstallationID, NodeId: owner.NodeID, Uid: owner.UID}
}

func mutationTo(mutation hostagent.Mutation) *agentv1.Mutation {
	return &agentv1.Mutation{
		ProtocolMajor:  mutation.ProtocolMajor,
		Owner:          ownerTo(mutation.Owner),
		Generation:     mutation.Generation,
		IdempotencyKey: mutation.IdempotencyKey,
	}
}

func vmDesiredTo(value hostagent.VMDesired) *agentv1.VMDesired {
	return &agentv1.VMDesired{
		Uid:         value.UID,
		Name:        value.Name,
		Image:       value.Image,
		Firmware:    value.Firmware,
		ApiSocket:   value.APISocket,
		VhostSocket: value.VhostSocket,
		Disk:        value.Disk,
		Mac:         value.MAC,
		Ip:          value.IP,
		Cpus:        value.CPUs,
		MemoryMib:   value.MemoryMiB,
	}
}

func vmObservedFrom(value *agentv1.VMObserved) hostagent.VMObserved {
	return hostagent.VMObserved{
		UID:         value.GetUid(),
		Unit:        value.GetUnit(),
		PID:         int(value.GetPid()),
		GuestUptime: value.GetGuestUptime(),
		Running:     value.GetRunning(),
		APISocket:   value.GetApiSocket(),
		VhostSocket: value.GetVhostSocket(),
		IP:          value.GetIp(),
		MAC:         value.GetMac(),
		Generation:  value.GetGeneration(),
	}
}

func portObservedFrom(value *agentv1.PortObserved) hostagent.PortObserved {
	return hostagent.PortObserved{
		Name:      value.GetName(),
		Network:   value.GetNetwork(),
		MAC:       value.GetMac(),
		IP:        value.GetIp(),
		Published: value.GetPublished(),
	}
}

func probeLeaseFrom(value *agentv1.ProbeLease) hostagent.ProbeLease {
	return hostagent.ProbeLease{
		UID:       value.GetUid(),
		Network:   value.GetNetwork(),
		Port:      value.GetPort(),
		MAC:       value.GetMac(),
		IP:        value.GetIp(),
		ExpiresAt: value.GetExpiresAt(),
	}
}

func mapError(err error) error {
	if err == nil {
		return nil
	}

	if !errors.Is(err, context.Canceled) && !errors.Is(err, context.DeadlineExceeded) {
		switch status.Code(err) {
		case codes.InvalidArgument:
			return fmt.Errorf("%w: %v", hostagent.ErrInvalidRequest, err)
		case codes.FailedPrecondition:
			return fmt.Errorf("%w: %v", hostagent.ErrProtocol, err)
		case codes.NotFound:
			return fmt.Errorf("%w: %v", hostagent.ErrNotFound, err)
		case codes.Aborted:
			return fmt.Errorf("%w: %v", hostagent.ErrConflict, err)
		case codes.Unavailable:
			return fmt.Errorf("%w: %v", hostagent.ErrUnavailable, err)
		}
	}

	return err
}

func (*Client) PrepareRootDisk(
	context.Context,
	hostagent.Mutation,
	hostagent.RootDiskRequest,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, hostagent.ErrUnavailable
}

func (*Client) PrepareCIDATA(
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, hostagent.ErrUnavailable
}

func (*Client) PrepareConfext(
	context.Context,
	hostagent.Mutation,
	string,
	[]hostagent.ArtifactFile,
) (hostagent.ArtifactResult, error) {
	return hostagent.ArtifactResult{}, hostagent.ErrUnavailable
}

var _ hostagent.HostAgent = (*Client)(nil)
