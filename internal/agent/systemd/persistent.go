package systemd

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var ErrInvalidUnit = errors.New("invalid persistent unit")

var identifier = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9_.-]{0,127}$`)

type PersistentUnit struct {
	InstallationID string
	OwnerUID       string
	NodeID         string
	Name           string
	Description    string
	ExecStart      []string
	Environment    map[string]string
	Restart        string
}

func (u PersistentUnit) Validate() error {
	if !identifier.MatchString(u.InstallationID) || !identifier.MatchString(u.OwnerUID) ||
		!identifier.MatchString(u.NodeID) ||
		!identifier.MatchString(u.Name) ||
		!strings.HasSuffix(u.Name, ".service") {
		return fmt.Errorf(
			"%w: installation ID, owner UID, node ID, and .service name must be safe identifiers",
			ErrInvalidUnit,
		)
	}

	if len(u.ExecStart) == 0 || u.ExecStart[0] == "" || strings.ContainsAny(u.ExecStart[0], "\r\n") {
		return fmt.Errorf("%w: executable is required", ErrInvalidUnit)
	}

	for _, value := range u.ExecStart {
		if strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: exec arguments cannot contain newlines", ErrInvalidUnit)
		}
	}

	for key, value := range u.Environment {
		if !identifier.MatchString(key) || strings.ContainsAny(value, "\r\n") {
			return fmt.Errorf("%w: environment is invalid", ErrInvalidUnit)
		}
	}

	if u.Restart != "" && u.Restart != "on-failure" && u.Restart != "always" && u.Restart != "no" {
		return fmt.Errorf("%w: restart policy %q", ErrInvalidUnit, u.Restart)
	}

	return nil
}

func (u PersistentUnit) Render() (string, error) {
	if err := u.Validate(); err != nil {
		return "", err
	}

	description := u.Description
	if description == "" {
		description = "k8labs owned VM " + u.OwnerUID
	}

	restart := u.Restart
	if restart == "" {
		restart = "on-failure"
	}

	var content strings.Builder
	fmt.Fprintf(&content, "[Unit]\nDescription=%s\n", escapeUnitValue(description))
	content.WriteString("[Service]\nType=simple\n")
	fmt.Fprintf(&content, "Environment=K8LABS_INSTALLATION_ID=%s\n", escapeUnitValue(u.InstallationID))
	fmt.Fprintf(&content, "Environment=K8LABS_OWNER_UID=%s\n", escapeUnitValue(u.OwnerUID))
	fmt.Fprintf(&content, "Environment=K8LABS_NODE_ID=%s\n", escapeUnitValue(u.NodeID))

	keys := make([]string, 0, len(u.Environment))
	for key := range u.Environment {
		keys = append(keys, key)
	}

	sortStrings(keys)

	for _, key := range keys {
		fmt.Fprintf(&content, "Environment=%s=%s\n", key, escapeUnitValue(u.Environment[key]))
	}

	fmt.Fprintf(&content, "ExecStart=%s\nRestart=%s\nRestartSec=2\n", quoteExec(u.ExecStart), restart)
	content.WriteString("[Install]\nWantedBy=default.target\n")

	return content.String(), nil
}

// Install persists an owned unit with atomic replacement before telling
// user-systemd to reload, enable and start it.
func Install(ctx context.Context, client Client, directory string, unit PersistentUnit) (Unit, error) {
	content, err := unit.Render()
	if err != nil {
		return Unit{}, err
	}

	if !filepath.IsAbs(directory) {
		return Unit{}, fmt.Errorf("%w: unit directory must be absolute", ErrInvalidUnit)
	}

	if err := os.MkdirAll(directory, 0o750); err != nil {
		return Unit{}, fmt.Errorf("create unit directory: %w", err)
	}

	path := filepath.Join(directory, unit.Name)
	if filepath.Dir(path) != filepath.Clean(directory) {
		return Unit{}, fmt.Errorf("%w: unit path escaped directory", ErrInvalidUnit)
	}

	temporary, err := os.CreateTemp(directory, "."+unit.Name+".*")
	if err != nil {
		return Unit{}, fmt.Errorf("create temporary unit: %w", err)
	}

	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err := temporary.Chmod(0o640); err != nil {
		_ = temporary.Close()
		return Unit{}, fmt.Errorf("chmod temporary unit: %w", err)
	}

	if _, err := temporary.WriteString(content); err != nil {
		_ = temporary.Close()
		return Unit{}, fmt.Errorf("write temporary unit: %w", err)
	}

	if err := temporary.Sync(); err != nil {
		_ = temporary.Close()
		return Unit{}, fmt.Errorf("sync temporary unit: %w", err)
	}

	if err := temporary.Close(); err != nil {
		return Unit{}, fmt.Errorf("close temporary unit: %w", err)
	}

	if err := os.Rename(temporaryPath, path); err != nil {
		return Unit{}, fmt.Errorf("replace unit file: %w", err)
	}

	if err := client.Reload(ctx); err != nil {
		return Unit{}, err
	}

	if err := client.EnableUnitFiles(ctx, []string{unit.Name}); err != nil {
		return Unit{}, err
	}

	unitState, err := client.StartUnit(ctx, unit.Name)
	if err != nil {
		return Unit{}, err
	}

	return unitState, nil
}

func escapeUnitValue(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, `\`, `\\`), `"`, `\"`)
}

func quoteExec(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = `"` + escapeUnitValue(value) + `"`
	}

	return strings.Join(quoted, " ")
}

func sortStrings(values []string) {
	for i := 1; i < len(values); i++ {
		for j := i; j > 0 && values[j] < values[j-1]; j-- {
			values[j], values[j-1] = values[j-1], values[j]
		}
	}
}
