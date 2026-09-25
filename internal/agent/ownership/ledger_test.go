package ownership

import (
	"errors"
	"path/filepath"
	"reflect"
	"testing"
)

func TestRegisterRetainsExplicitPredecessorOwnership(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "ownership.json")

	ledger, err := Register(path, "kind-uid-a")
	if err != nil || !ledger.Owns("kind-uid-a") || ledger.Owns("kind-uid-b") {
		t.Fatalf("first ledger=%#v error=%v", ledger, err)
	}

	ledger, err = Register(path, "kind-uid-b")
	if err != nil || !ledger.Owns("kind-uid-a") || !ledger.Owns("kind-uid-b") {
		t.Fatalf("second ledger=%#v error=%v", ledger, err)
	}

	loaded, err := Load(path)
	if err != nil || !reflect.DeepEqual(loaded, ledger) {
		t.Fatalf("Load()=%#v error=%v, want %#v", loaded, err, ledger)
	}
}

func TestRegisterRejectsUnsafeInstallationID(t *testing.T) {
	_, err := Register(filepath.Join(t.TempDir(), "ownership.json"), "../unowned")
	if !errors.Is(err, ErrInvalidInstallationID) {
		t.Fatalf("Register() error=%v, want ErrInvalidInstallationID", err)
	}
}

func TestLedgerRejectsUnsortedOrDuplicatePredecessors(t *testing.T) {
	ledger := Ledger{Current: "kind-uid-b", Predecessors: []string{"kind-uid-b", "kind-uid-a"}}
	if !errors.Is(ledger.Validate(), ErrInvalidInstallationID) {
		t.Fatalf("Validate() error=%v, want ErrInvalidInstallationID", ledger.Validate())
	}
}
