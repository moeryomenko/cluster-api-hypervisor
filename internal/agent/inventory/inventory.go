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

const schemaVersion = 1

var ErrIdempotencyConflict = errors.New("inventory idempotency conflict")

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
	if _, err := s.db.Exec("PRAGMA journal_mode=WAL; PRAGMA foreign_keys=ON;"); err != nil {
		return fmt.Errorf("configure inventory SQLite: %w", err)
	}

	_, err := s.db.Exec(`
CREATE TABLE IF NOT EXISTS schema_migrations (version INTEGER PRIMARY KEY);
CREATE TABLE IF NOT EXISTS operations (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  idempotency_key TEXT NOT NULL,
  generation INTEGER NOT NULL,
  created_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid, idempotency_key)
);
CREATE TABLE IF NOT EXISTS vms (
  installation_id TEXT NOT NULL,
  owner_uid TEXT NOT NULL,
  node_id TEXT NOT NULL,
  unit TEXT NOT NULL,
  generation INTEGER NOT NULL,
  pid INTEGER NOT NULL,
  disk TEXT NOT NULL,
  api_socket TEXT NOT NULL,
  vhost_socket TEXT NOT NULL,
  network TEXT NOT NULL,
  port TEXT NOT NULL,
  mac TEXT NOT NULL,
  ip TEXT NOT NULL,
  updated_at INTEGER NOT NULL,
  PRIMARY KEY (installation_id, owner_uid),
  UNIQUE (installation_id, unit)
);
INSERT OR IGNORE INTO schema_migrations(version) VALUES (?);`, schemaVersion)
	if err != nil {
		return fmt.Errorf("migrate inventory: %w", err)
	}

	return nil
}

func (s *Store) RecordOperation(installationID, ownerUID, key string, generation uint64) error {
	result, err := s.db.Exec(
		`INSERT OR IGNORE INTO operations(installation_id, owner_uid, idempotency_key, generation, created_at) VALUES (?, ?, ?, ?, ?)`,
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
	if err := s.db.QueryRow(`SELECT generation FROM operations WHERE installation_id=? AND owner_uid=? AND idempotency_key=?`, installationID, ownerUID, key).Scan(&existing); err != nil {
		return fmt.Errorf("load idempotency operation: %w", err)
	}

	if existing != generation {
		return ErrIdempotencyConflict
	}

	return nil
}

func (s *Store) UpsertVM(vm VM) error {
	_, err := s.db.Exec(
		`INSERT INTO vms(installation_id, owner_uid, node_id, unit, generation, pid, disk, api_socket, vhost_socket, network, port, mac, ip, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(installation_id, owner_uid) DO UPDATE SET node_id=excluded.node_id, unit=excluded.unit, generation=excluded.generation, pid=excluded.pid, disk=excluded.disk, api_socket=excluded.api_socket, vhost_socket=excluded.vhost_socket, network=excluded.network, port=excluded.port, mac=excluded.mac, ip=excluded.ip, updated_at=excluded.updated_at`,
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

	err := s.db.QueryRow(`SELECT installation_id, owner_uid, node_id, unit, generation, pid, disk, api_socket, vhost_socket, network, port, mac, ip FROM vms WHERE installation_id=? AND owner_uid=?`, installationID, ownerUID).
		Scan(
			&vm.InstallationID, &vm.OwnerUID, &vm.NodeID, &vm.Unit, &vm.Generation, &vm.PID, &vm.Disk, &vm.APISocket, &vm.VhostSocket, &vm.Network, &vm.Port, &vm.MAC, &vm.IP,
		)
	if err != nil {
		return VM{}, err
	}

	return vm, nil
}
