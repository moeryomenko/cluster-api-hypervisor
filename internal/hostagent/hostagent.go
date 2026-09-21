package hostagent

import (
	"context"
	"errors"
	"fmt"
)

const ProtocolMajor uint32 = 1

var (
	ErrInvalidRequest = errors.New("invalid host-agent request")
	ErrConflict       = errors.New("host-agent operation conflict")
	ErrNotFound       = errors.New("host-agent resource not found")
	ErrUnauthorized   = errors.New("host-agent request unauthorized")
	ErrUnavailable    = errors.New("host-agent unavailable")
	ErrProtocol       = errors.New("host-agent protocol mismatch")
)

func IsInvalid(err error) bool     { return errors.Is(err, ErrInvalidRequest) }
func IsProtocol(err error) bool    { return errors.Is(err, ErrProtocol) }
func IsNotFound(err error) bool    { return errors.Is(err, ErrNotFound) }
func IsConflict(err error) bool    { return errors.Is(err, ErrConflict) }
func IsUnavailable(err error) bool { return errors.Is(err, ErrUnavailable) }

type Owner struct {
	InstallationID string
	NodeID         string
	UID            string
}

func (o Owner) Validate() error {
	if o.InstallationID == "" || o.NodeID == "" || o.UID == "" {
		return fmt.Errorf("%w: owner installation_id, node_id, and uid are required", ErrInvalidRequest)
	}
	return nil
}

type Mutation struct {
	ProtocolMajor  uint32
	Owner          Owner
	Generation     uint64
	IdempotencyKey string
}

func (m Mutation) Validate() error {
	if m.ProtocolMajor != ProtocolMajor {
		return fmt.Errorf("%w: got %d, want %d", ErrProtocol, m.ProtocolMajor, ProtocolMajor)
	}
	if err := m.Owner.Validate(); err != nil {
		return err
	}
	if m.Generation == 0 || m.IdempotencyKey == "" {
		return fmt.Errorf("%w: generation and idempotency_key are required", ErrInvalidRequest)
	}
	return nil
}

type Capabilities struct {
	NodeID              string
	CloudHypervisor     string
	K8netdProtocolMajor uint32
	CanUseKVM           bool
	CanUseUserDBus      bool
}

type VMDesired struct {
	UID         string
	Name        string
	Image       string
	Firmware    string
	APISocket   string
	VhostSocket string
	Disk        string
	MAC         string
	IP          string
	CPUs        uint32
	MemoryMiB   uint32
}

type VMObserved struct {
	UID         string
	Unit        string
	PID         int
	GuestUptime uint64
	Running     bool
	APISocket   string
	VhostSocket string
	IP          string
	MAC         string
	Generation  uint64
}

type NetworkRequest struct {
	Name string
	CIDR string
}

type PortRequest struct {
	Name    string
	Network string
	MAC     string
}

type PortObserved struct {
	Name      string
	Network   string
	MAC       string
	IP        string
	Published map[uint32]uint32
}

type Diagnostics struct {
	OwnerUID string
	Messages []string
}

type ProbeLease struct {
	UID       string
	Network   string
	Port      string
	MAC       string
	IP        string
	ExpiresAt int64
}

type HostAgent interface {
	Health(context.Context) (Capabilities, error)
	EnsureVM(context.Context, Mutation, VMDesired) (VMObserved, error)
	GetVM(context.Context, Owner) (VMObserved, error)
	StopVM(context.Context, Mutation) error
	DeleteVM(context.Context, Mutation) error
	EnsureNetwork(context.Context, Mutation, NetworkRequest) error
	DeleteNetwork(context.Context, Mutation) error
	EnsurePort(context.Context, Mutation, PortRequest) (PortObserved, error)
	DeletePort(context.Context, Mutation) error
	AllocateIP(context.Context, Mutation) (string, error)
	ReleaseIP(context.Context, Mutation, string) error
	PublishPort(context.Context, Mutation, uint32, uint32) (uint32, error)
	ReleasePort(context.Context, Mutation, uint32, uint32) error
	Diagnostics(context.Context, Owner) (Diagnostics, error)
	AcquireProbe(context.Context, Mutation, string) (ProbeLease, error)
	ReleaseProbe(context.Context, Mutation, string) error
}

type Fake struct{}

var _ HostAgent = (*Fake)(nil)

func (*Fake) Health(context.Context) (Capabilities, error) { return Capabilities{}, nil }
func (*Fake) EnsureVM(context.Context, Mutation, VMDesired) (VMObserved, error) {
	return VMObserved{}, nil
}
func (*Fake) GetVM(context.Context, Owner) (VMObserved, error)              { return VMObserved{}, ErrNotFound }
func (*Fake) StopVM(context.Context, Mutation) error                        { return nil }
func (*Fake) DeleteVM(context.Context, Mutation) error                      { return nil }
func (*Fake) EnsureNetwork(context.Context, Mutation, NetworkRequest) error { return nil }
func (*Fake) DeleteNetwork(context.Context, Mutation) error                 { return nil }
func (*Fake) EnsurePort(context.Context, Mutation, PortRequest) (PortObserved, error) {
	return PortObserved{}, nil
}
func (*Fake) DeletePort(context.Context, Mutation) error                            { return nil }
func (*Fake) AllocateIP(context.Context, Mutation) (string, error)                  { return "", nil }
func (*Fake) ReleaseIP(context.Context, Mutation, string) error                     { return nil }
func (*Fake) PublishPort(context.Context, Mutation, uint32, uint32) (uint32, error) { return 0, nil }
func (*Fake) ReleasePort(context.Context, Mutation, uint32, uint32) error           { return nil }
func (*Fake) Diagnostics(context.Context, Owner) (Diagnostics, error)               { return Diagnostics{}, nil }
func (*Fake) AcquireProbe(context.Context, Mutation, string) (ProbeLease, error) {
	return ProbeLease{}, nil
}
func (*Fake) ReleaseProbe(context.Context, Mutation, string) error { return nil }
