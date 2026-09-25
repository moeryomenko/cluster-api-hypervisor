package ownership

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

var ErrInvalidInstallationID = errors.New("invalid installation ID")

type Ledger struct {
	Current      string   `json:"current"`
	Predecessors []string `json:"predecessors"`
}

func (l Ledger) Owns(installationID string) bool {
	if installationID == l.Current {
		return true
	}

	return sort.SearchStrings(l.Predecessors, installationID) < len(l.Predecessors) &&
		l.Predecessors[sort.SearchStrings(l.Predecessors, installationID)] == installationID
}

func Load(path string) (Ledger, error) {
	contents, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Ledger{}, nil
	}

	if err != nil {
		return Ledger{}, fmt.Errorf("read ownership ledger: %w", err)
	}

	var ledger Ledger
	if err := json.Unmarshal(contents, &ledger); err != nil {
		return Ledger{}, fmt.Errorf("decode ownership ledger: %w", err)
	}

	if err := ledger.Validate(); err != nil {
		return Ledger{}, err
	}

	return ledger, nil
}

// Register makes current the active installation and retains the prior active
// installation as an explicit predecessor. It never authorizes an arbitrary
// name prefix.
func Register(path, current string) (Ledger, error) {
	if err := validateID(current); err != nil {
		return Ledger{}, err
	}

	ledger, err := Load(path)
	if err != nil {
		return Ledger{}, err
	}

	if ledger.Current != "" && ledger.Current != current {
		ledger.Predecessors = append(ledger.Predecessors, ledger.Current)
	}

	ledger.Current = current

	ledger.Predecessors = uniqueSorted(ledger.Predecessors)
	if err := ledger.Validate(); err != nil {
		return Ledger{}, err
	}

	if err := writeAtomic(path, ledger); err != nil {
		return Ledger{}, err
	}

	return ledger, nil
}

func (l Ledger) Validate() error {
	if l.Current != "" {
		if err := validateID(l.Current); err != nil {
			return err
		}
	}

	for _, predecessor := range l.Predecessors {
		if err := validateID(predecessor); err != nil {
			return err
		}

		if predecessor == l.Current {
			return fmt.Errorf("%w: current installation cannot be its own predecessor", ErrInvalidInstallationID)
		}
	}

	if !sort.StringsAreSorted(l.Predecessors) {
		return fmt.Errorf("%w: predecessors must be sorted", ErrInvalidInstallationID)
	}

	for index := 1; index < len(l.Predecessors); index++ {
		if l.Predecessors[index-1] == l.Predecessors[index] {
			return fmt.Errorf("%w: duplicate predecessor", ErrInvalidInstallationID)
		}
	}

	return nil
}

func validateID(value string) error {
	if value == "" || len(value) > 128 || strings.ContainsAny(value, "/\\\r\n\t ") {
		return fmt.Errorf("%w: %q", ErrInvalidInstallationID, value)
	}

	return nil
}

func uniqueSorted(values []string) []string {
	sort.Strings(values)

	result := values[:0]
	for _, value := range values {
		if len(result) == 0 || result[len(result)-1] != value {
			result = append(result, value)
		}
	}

	return result
}

func writeAtomic(path string, ledger Ledger) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("ownership ledger path must be absolute")
	}

	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return fmt.Errorf("create ownership ledger directory: %w", err)
	}

	contents, err := json.MarshalIndent(ledger, "", "  ")
	if err != nil {
		return fmt.Errorf("encode ownership ledger: %w", err)
	}

	temporary, err := os.CreateTemp(filepath.Dir(path), ".ledger.*")
	if err != nil {
		return fmt.Errorf("create temporary ownership ledger: %w", err)
	}

	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := temporary.Chmod(0o600); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("chmod temporary ownership ledger: %w", err)
	}

	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("write ownership ledger: %w", err)
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return fmt.Errorf("sync ownership ledger: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close ownership ledger: %w", err)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("replace ownership ledger: %w", err)
	}

	return nil
}
