package agentgrpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type Client struct {
	connection *grpc.ClientConn
	service    agentv1.HostAgentClient
}

func Dial(ctx context.Context, address, serverName string, certificate tls.Certificate, roots *x509.CertPool) (*Client, error) {
	_ = ctx
	connection, err := grpc.NewClient(address, grpc.WithTransportCredentials(credentials.NewTLS(&tls.Config{
		MinVersion:   tls.VersionTLS13,
		ServerName:   serverName,
		Certificates: []tls.Certificate{certificate},
		RootCAs:      roots,
	})))
	if err != nil {
		return nil, fmt.Errorf("dial host agent: %w", err)
	}
	return &Client{connection: connection, service: agentv1.NewHostAgentClient(connection)}, nil
}

func (c *Client) Close() error { return c.connection.Close() }

func (c *Client) Health(ctx context.Context) (hostagent.Capabilities, error) {
	response, err := c.service.Health(ctx, &agentv1.HealthRequest{ProtocolMajor: hostagent.ProtocolMajor})
	if err != nil {
		return hostagent.Capabilities{}, err
	}
	return hostagent.Capabilities{
		NodeID: response.GetNodeId(), CloudHypervisor: response.GetCloudHypervisor(),
		K8netdProtocolMajor: response.GetK8NetdProtocolMajor(), CanUseKVM: response.GetCanUseKvm(), CanUseUserDBus: response.GetCanUseUserDbus(),
	}, nil
}

func (c *Client) EnsureVM(ctx context.Context, mutation hostagent.Mutation, desired hostagent.VMDesired) (hostagent.VMObserved, error) {
	if err := mutation.Validate(); err != nil {
		return hostagent.VMObserved{}, err
	}
	response, err := c.service.EnsureVM(ctx, &agentv1.EnsureVMRequest{
		Mutation: &agentv1.Mutation{ProtocolMajor: mutation.ProtocolMajor, Owner: &agentv1.Owner{InstallationId: mutation.Owner.InstallationID, NodeId: mutation.Owner.NodeID, Uid: mutation.Owner.UID}, Generation: mutation.Generation, IdempotencyKey: mutation.IdempotencyKey},
		Desired:  &agentv1.VMDesired{Uid: desired.UID, Name: desired.Name, Image: desired.Image, Firmware: desired.Firmware, ApiSocket: desired.APISocket, VhostSocket: desired.VhostSocket, Disk: desired.Disk, Mac: desired.MAC, Ip: desired.IP, Cpus: desired.CPUs, MemoryMib: desired.MemoryMiB},
	})
	if err != nil {
		return hostagent.VMObserved{}, err
	}
	observed := response.GetObserved()
	return hostagent.VMObserved{UID: observed.GetUid(), Unit: observed.GetUnit(), PID: int(observed.GetPid()), GuestUptime: observed.GetGuestUptime(), Running: observed.GetRunning(), APISocket: observed.GetApiSocket(), VhostSocket: observed.GetVhostSocket(), IP: observed.GetIp(), MAC: observed.GetMac(), Generation: observed.GetGeneration()}, nil
}
