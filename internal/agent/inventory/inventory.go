package inventory

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	_ "modernc.org/sqlite"
)

const schemaVersion = 2

var (
	ErrIdempotencyConflict = errors.New("inventory idempotency conflict")
	ErrOperationNotFound   = errors.New("inventory operation not found")
)

type OperationState string

const (
	OperationIntent    OperationState = "intent"
	OperationCompleted OperationState = "completed"
	OperationFailed    OperationState = "failed"
)

type Operation struct {
	InstallationID string
	OwnerUID       string
	NodeID         string
	Key            string
	Kind           string
	RequestHash    string
	Generation     uint64
	State          OperationState
	Result         string
	Failure        string
	CreatedAt      time.Time
	UpdatedAt      time.Time
}

type VM struct {
	InstallationID string
	OwnerUID       string
	NodeID         string
	Unit           string
	Generation     uint64
	PID            int64
	Disk           string
	APISocket      string
	VhostSocket    string
	Network        string
	Port           string
	MAC            string
	IP             string
}

type NetworkResource struct {
	InstallationID string
	OwnerUID       string
	NodeID         string
	Network        string
	Port           string
	MAC            string
	IP             string
}

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return nil, fmt.Errorf("create inventory directory: %w", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open inventory: %w", err)
	}

	store := &Store{db: db}
	if err := store.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}

	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON; PRAGMA busy_timeout=5000;"); err != nil {
		return fmt.Errorf("configure inventory SQLite: %w", err)
	}

	_, err := s.db.Exec(migrateQuery, schemaVersion)
	if err != nil {
		return fmt.Errorf("migrate inventory: %w", err)
	}

	return nil
}

func (s *Store) RecordOperation(installationID, ownerUID, key string, generation uint64) error {
	result, err := s.db.Exec(
		recordOperationQuery,
		installationID,
		ownerUID,
		key,
		generation,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("record idempotency operation: %w", err)
	}

	rows, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect idempotency operation: %w", err)
	}

	if rows == 1 {
		return nil
	}

	var existing uint64
	if err := s.db.QueryRow(operationGenerationQuery, installationID, ownerUID, key).Scan(&existing); err != nil {
		return fmt.Errorf("load idempotency operation: %w", err)
	}

	if existing != generation {
		return ErrIdempotencyConflict
	}

	return nil
}

func (s *Store) UpsertNetworkResource(resource NetworkResource) error {
	_, err := s.db.Exec(
		upsertNetworkResourceQuery,
		resource.InstallationID,
		resource.OwnerUID,
		resource.NodeID,
		resource.Network,
		resource.Port,
		resource.MAC,
		resource.IP,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert network resource: %w", err)
	}

	return nil
}

func (s *Store) GetNetworkResource(installationID, ownerUID string) (NetworkResource, error) {
	var resource NetworkResource

	err := s.db.QueryRow(getNetworkResourceQuery, installationID, ownerUID).
		Scan(&resource.InstallationID, &resource.OwnerUID, &resource.NodeID, &resource.Network, &resource.Port, &resource.MAC, &resource.IP)
	if err != nil {
		return NetworkResource{}, err
	}

	return resource, nil
}

func (s *Store) DeleteNetworkResource(installationID, ownerUID string) error {
	if _, err := s.db.Exec(deleteNetworkResourceQuery, installationID, ownerUID); err != nil {
		return fmt.Errorf("delete network resource: %w", err)
	}

	return nil
}

func (s *Store) UpsertVM(vm VM) error {
	_, err := s.db.Exec(
		upsertVMQuery,
		vm.InstallationID,
		vm.OwnerUID,
		vm.NodeID,
		vm.Unit,
		vm.Generation,
		vm.PID,
		vm.Disk,
		vm.APISocket,
		vm.VhostSocket,
		vm.Network,
		vm.Port,
		vm.MAC,
		vm.IP,
		time.Now().Unix(),
	)
	if err != nil {
		return fmt.Errorf("upsert VM inventory: %w", err)
	}

	return nil
}

func (s *Store) GetVM(installationID, ownerUID string) (VM, error) {
	var vm VM

	err := s.db.QueryRow(getVMQuery, installationID, ownerUID).
		Scan(&vm.InstallationID,
			&vm.OwnerUID,
			&vm.NodeID,
			&vm.Unit,
			&vm.Generation,
			&vm.PID,
			&vm.Disk,
			&vm.APISocket,
			&vm.VhostSocket,
			&vm.Network,
			&vm.Port,
			&vm.MAC,
			&vm.IP)
	if err != nil {
		return VM{}, err
	}

	return vm, nil
}
