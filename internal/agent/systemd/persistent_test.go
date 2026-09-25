package systemd

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type persistentFake struct {
	fakeClient
	reloaded bool
	enabled  []string
}

func (f *persistentFake) Reload(context.Context) error { f.reloaded = true; return nil }
func (f *persistentFake) EnableUnitFiles(_ context.Context, names []string) error {
	f.enabled = append(f.enabled, names...)
	return nil
}

func (f *persistentFake) StartUnit(_ context.Context, name string) (Unit, error) {
	unit := Unit{Name: name, PID: 42}
	f.units[name] = unit

	return unit, nil
}

func TestPersistentUnitRenderIsDeterministicAndOwned(t *testing.T) {
	unit := PersistentUnit{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Name:           "k8slab-vm-machine-a.service",
		ExecStart:      []string{"/usr/bin/cloud-hypervisor", "--api-socket", "/state/machine-a.sock"},
		Environment:    map[string]string{"ZED": "last", "ALPHA": "first"},
	}

	content, err := unit.Render()
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{"K8LABS_INSTALLATION_ID=install-a", "K8LABS_OWNER_UID=machine-a", "K8LABS_NODE_ID=node-a", "Environment=ALPHA=first\nEnvironment=ZED=last", `ExecStart="/usr/bin/cloud-hypervisor" "--api-socket" "/state/machine-a.sock"`, "WantedBy=default.target"} {
		if !strings.Contains(content, want) {
			t.Fatalf("rendered unit missing %q:\n%s", want, content)
		}
	}
}

func TestInstallAtomicallyWritesReloadsEnablesAndStarts(t *testing.T) {
	client := &persistentFake{fakeClient: fakeClient{units: map[string]Unit{}}}
	directory := filepath.Join(t.TempDir(), "systemd", "user")
	unit := PersistentUnit{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Name:           "k8slab-vm-machine-a.service",
		ExecStart:      []string{"/bin/true"},
	}

	started, err := Install(context.Background(), client, directory, unit)
	if err != nil {
		t.Fatal(err)
	}

	if started.PID != 42 || !client.reloaded || len(client.enabled) != 1 || client.enabled[0] != unit.Name {
		t.Fatalf("install lifecycle = %#v reloaded=%v enabled=%v", started, client.reloaded, client.enabled)
	}

	content, err := os.ReadFile(filepath.Join(directory, unit.Name))
	if err != nil || !strings.Contains(string(content), "ExecStart=\"/bin/true\"") {
		t.Fatalf("unit file content=%q error=%v", content, err)
	}
}

func TestPersistentUnitRejectsUnsafeIdentifiersAndArguments(t *testing.T) {
	unit := PersistentUnit{
		InstallationID: "install-a",
		OwnerUID:       "../machine",
		NodeID:         "node-a",
		Name:           "escape.service",
		ExecStart:      []string{"/bin/true"},
	}
	if _, err := unit.Render(); !errors.Is(err, ErrInvalidUnit) {
		t.Fatalf("Render() error=%v, want ErrInvalidUnit", err)
	}

	unit = PersistentUnit{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Name:           "k8slab.service",
		ExecStart:      []string{"/bin/true\nmalicious"},
	}
	if _, err := unit.Render(); !errors.Is(err, ErrInvalidUnit) {
		t.Fatalf("Render() error=%v, want ErrInvalidUnit", err)
	}
}
