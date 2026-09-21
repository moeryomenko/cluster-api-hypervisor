package hostagent

import (
	"context"
	"errors"
	"testing"
)

func validMutation() Mutation {
	return Mutation{
		ProtocolMajor:  ProtocolMajor,
		Owner:          Owner{InstallationID: "install-a", NodeID: "node-a", UID: "machine-a"},
		Generation:     1,
		IdempotencyKey: "machine-a-1",
	}
}

func TestMutationValidateRejectsProtocolBeforeOwnerSideEffects(t *testing.T) {
	mutation := validMutation()
	mutation.ProtocolMajor = ProtocolMajor + 1
	if err := mutation.Validate(); !errors.Is(err, ErrProtocol) {
		t.Fatalf("Validate() error = %v, want ErrProtocol", err)
	}
}

func TestMutationValidateRequiresStableIdentityAndReplayKey(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*Mutation)
	}{
		{name: "missing owner", mutate: func(m *Mutation) { m.Owner.UID = "" }},
		{name: "missing generation", mutate: func(m *Mutation) { m.Generation = 0 }},
		{name: "missing idempotency key", mutate: func(m *Mutation) { m.IdempotencyKey = "" }},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			mutation := validMutation()
			testCase.mutate(&mutation)
			if err := mutation.Validate(); !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("Validate() error = %v, want ErrInvalidRequest", err)
			}
		})
	}
}

func TestFakeSatisfiesHostAgentContract(t *testing.T) {
	var agent HostAgent = &Fake{}
	if _, err := agent.Health(context.Background()); err != nil {
		t.Fatalf("Health() error = %v", err)
	}
}
