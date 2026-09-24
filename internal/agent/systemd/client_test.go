package systemd

import (
	"context"
	"testing"
)

type fakeClient struct {
	started []string
	units   map[string]Unit
}

func (f *fakeClient) StartTransientUnit(_ context.Context, name string, _ []Property) (Unit, error) {
	unit := Unit{Name: name, Path: "/org/freedesktop/systemd1/unit/test"}
	f.started = append(f.started, name)
	f.units[name] = unit

	return unit, nil
}

func (f *fakeClient) StopUnit(_ context.Context, name string) error {
	delete(f.units, name)
	return nil
}

func (f *fakeClient) GetUnit(_ context.Context, name string) (Unit, error) { return f.units[name], nil }

func (f *fakeClient) ListUnits(_ context.Context) ([]Unit, error) {
	units := make([]Unit, 0, len(f.units))
	for _, unit := range f.units {
		units = append(units, unit)
	}

	return units, nil
}

func TestClientContractSupportsOwnedTransientLifecycle(t *testing.T) {
	var client Client = &fakeClient{units: map[string]Unit{}}

	unit, err := client.StartTransientUnit(context.Background(), "k8slab-vm-machine-a.service", nil)
	if err != nil || unit.Name == "" {
		t.Fatalf("StartTransientUnit() unit=%v err=%v", unit, err)
	}

	if err := client.StopUnit(context.Background(), unit.Name); err != nil {
		t.Fatal(err)
	}
}
