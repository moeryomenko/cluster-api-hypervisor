package systemd

import (
	"bufio"
	"context"
	"os/exec"
	"strings"
	"testing"

	"github.com/godbus/dbus/v5"
)

type fakeClient struct {
	started []string
	units   map[string]Unit
}

func (*fakeClient) Reload(context.Context) error { return nil }

func (*fakeClient) EnableUnitFiles(context.Context, []string) error { return nil }

func (f *fakeClient) StartUnit(_ context.Context, name string) (Unit, error) {
	unit := Unit{Name: name, Path: "/org/freedesktop/systemd1/unit/test"}
	f.units[name] = unit

	return unit, nil
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

type testManager struct {
	reloads  int
	unitPath dbus.ObjectPath
}

func (m *testManager) Reload() *dbus.Error {
	m.reloads++
	return nil
}

func (m *testManager) GetUnit(string) (dbus.ObjectPath, *dbus.Error) {
	return m.unitPath, nil
}

type testProperties struct {
	interfaceName string
	propertyName  string
}

func (p *testProperties) Get(interfaceName, propertyName string) (dbus.Variant, *dbus.Error) {
	p.interfaceName = interfaceName

	p.propertyName = propertyName
	if interfaceName != serviceInterface || propertyName != "MainPID" {
		return dbus.Variant{}, dbus.NewError("org.freedesktop.DBus.Error.InvalidArgs", []any{"unexpected property"})
	}

	return dbus.MakeVariant(uint32(42)), nil
}

func TestDBusClientUsesSystemdServiceDestination(t *testing.T) {
	address := startTestBus(t)

	service := connectTestBus(t, address)
	defer func() { _ = service.Close() }()

	unitPath := dbus.ObjectPath("/org/freedesktop/systemd1/unit/test_2eservice")
	manager := &testManager{unitPath: unitPath}
	properties := &testProperties{}

	if err := service.Export(manager, dbus.ObjectPath(managerPath), managerInterface); err != nil {
		t.Fatal(err)
	}

	if err := service.Export(properties, unitPath, "org.freedesktop.DBus.Properties"); err != nil {
		t.Fatal(err)
	}

	if reply, err := service.RequestName(managerService, dbus.NameFlagDoNotQueue); err != nil ||
		reply != dbus.RequestNameReplyPrimaryOwner {
		t.Fatalf("RequestName() = %d, %v", reply, err)
	}

	client, err := ConnectUser(address)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = client.Close() }()

	if err := client.Reload(t.Context()); err != nil {
		t.Fatal(err)
	}

	unit, err := client.GetUnit(t.Context(), "test.service")
	if err != nil {
		t.Fatal(err)
	}

	if manager.reloads != 1 {
		t.Fatalf("Reload calls = %d, want 1", manager.reloads)
	}

	if unit.Path != unitPath || unit.PID != 42 {
		t.Fatalf("GetUnit() = %#v", unit)
	}

	if properties.interfaceName != serviceInterface || properties.propertyName != "MainPID" {
		t.Fatalf("property lookup = %s.%s", properties.interfaceName, properties.propertyName)
	}
}

func startTestBus(t *testing.T) string {
	t.Helper()

	if _, err := exec.LookPath("dbus-daemon"); err != nil {
		t.Skip("dbus-daemon is required for D-Bus integration tests")
	}

	command := exec.Command("dbus-daemon", "--session", "--nofork", "--print-address=1")

	output, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}

	if err := command.Start(); err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		_ = command.Process.Kill()
		_ = command.Wait()
	})

	address, err := bufio.NewReader(output).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}

	return strings.TrimSpace(address)
}

func connectTestBus(t *testing.T, address string) *dbus.Conn {
	t.Helper()

	connection, err := dbus.Dial(address)
	if err != nil {
		t.Fatal(err)
	}

	if err := connection.Auth(nil); err != nil {
		_ = connection.Close()

		t.Fatal(err)
	}

	if err := connection.Hello(); err != nil {
		_ = connection.Close()

		t.Fatal(err)
	}

	return connection
}
