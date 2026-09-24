package inventory

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// BeginOperation durably records a host-operation intent before its external
// effect occurs. completed is true only when an identical operation already
// has a committed terminal result; an intent must be reconciled by its caller.
func (s *Store) BeginOperation(operation Operation) (stored Operation, completed bool, err error) {
	if err := validateOperation(operation); err != nil {
		return Operation{}, false, err
	}

	now := time.Now().UTC().Truncate(time.Second)

	tx, err := s.db.BeginTx(context.Background(), nil)
	if err != nil {
		return Operation{}, false, fmt.Errorf("begin operation intent: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	_, err = tx.Exec(`INSERT OR IGNORE INTO operation_journal
		(installation_id, owner_uid, node_id, idempotency_key, operation_kind, request_hash, generation, state, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, operation.InstallationID, operation.OwnerUID, operation.NodeID, operation.Key, operation.Kind, operation.RequestHash, operation.Generation, OperationIntent, now.Unix(), now.Unix())
	if err != nil {
		return Operation{}, false, fmt.Errorf("record operation intent: %w", err)
	}

	stored, err = operationFromRow(
		tx.QueryRow(
			`SELECT installation_id, owner_uid, node_id, idempotency_key, operation_kind, request_hash, generation, state, result, failure, created_at, updated_at
		FROM operation_journal WHERE installation_id=? AND owner_uid=? AND idempotency_key=?`,
			operation.InstallationID,
			operation.OwnerUID,
			operation.Key,
		),
	)
	if err != nil {
		return Operation{}, false, err
	}

	if stored.NodeID != operation.NodeID || stored.Kind != operation.Kind || stored.RequestHash != operation.RequestHash ||
		stored.Generation != operation.Generation {
		return Operation{}, false, ErrIdempotencyConflict
	}

	if err := tx.Commit(); err != nil {
		return Operation{}, false, fmt.Errorf("commit operation intent: %w", err)
	}

	return stored, stored.State == OperationCompleted || stored.State == OperationFailed, nil
}

// CompleteOperation records the observed outcome only after callers have
// queried the external system. It never performs an external effect itself.
func (s *Store) CompleteOperation(operation Operation, state OperationState, result, failure string) error {
	if state != OperationCompleted && state != OperationFailed {
		return fmt.Errorf("%w: terminal operation state is required", ErrIdempotencyConflict)
	}

	stored, _, err := s.BeginOperation(operation)
	if err != nil {
		return err
	}

	if stored.State != OperationIntent {
		if stored.State == state && stored.Result == result && stored.Failure == failure {
			return nil
		}

		return ErrIdempotencyConflict
	}

	outcome, err := s.db.Exec(`UPDATE operation_journal SET state=?, result=?, failure=?, updated_at=?
		WHERE installation_id=? AND owner_uid=? AND idempotency_key=? AND state=?`, state, result, failure, time.Now().UTC().Truncate(time.Second).Unix(), operation.InstallationID, operation.OwnerUID, operation.Key, OperationIntent)
	if err != nil {
		return fmt.Errorf("record observed operation: %w", err)
	}

	rows, err := outcome.RowsAffected()
	if err != nil {
		return fmt.Errorf("inspect observed operation: %w", err)
	}

	if rows != 1 {
		return ErrIdempotencyConflict
	}

	return nil
}

func (s *Store) PendingOperations() ([]Operation, error) {
	rows, err := s.db.Query(
		`SELECT installation_id, owner_uid, node_id, idempotency_key, operation_kind, request_hash, generation, state, result, failure, created_at, updated_at
		FROM operation_journal WHERE state=? ORDER BY created_at, installation_id, owner_uid, idempotency_key`,
		OperationIntent,
	)
	if err != nil {
		return nil, fmt.Errorf("list pending operations: %w", err)
	}
	defer func() { _ = rows.Close() }()

	operations := []Operation{}

	for rows.Next() {
		operation, err := operationFromRows(rows)
		if err != nil {
			return nil, err
		}

		operations = append(operations, operation)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate pending operations: %w", err)
	}

	return operations, nil
}

func validateOperation(operation Operation) error {
	if operation.InstallationID == "" || operation.OwnerUID == "" || operation.NodeID == "" || operation.Key == "" ||
		operation.Kind == "" ||
		operation.RequestHash == "" ||
		operation.Generation == 0 {
		return fmt.Errorf("%w: operation identity, kind, request hash, and generation are required", ErrIdempotencyConflict)
	}

	return nil
}

func operationFromRow(row *sql.Row) (Operation, error) {
	var (
		operation            Operation
		createdAt, updatedAt int64
	)
	if err := row.Scan(&operation.InstallationID, &operation.OwnerUID, &operation.NodeID, &operation.Key, &operation.Kind, &operation.RequestHash, &operation.Generation, &operation.State, &operation.Result, &operation.Failure, &createdAt, &updatedAt); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Operation{}, ErrOperationNotFound
		}

		return Operation{}, fmt.Errorf("load operation: %w", err)
	}

	operation.CreatedAt = time.Unix(createdAt, 0).UTC()
	operation.UpdatedAt = time.Unix(updatedAt, 0).UTC()

	return operation, nil
}

func operationFromRows(rows *sql.Rows) (Operation, error) {
	var (
		operation            Operation
		createdAt, updatedAt int64
	)
	if err := rows.Scan(&operation.InstallationID, &operation.OwnerUID, &operation.NodeID, &operation.Key, &operation.Kind, &operation.RequestHash, &operation.Generation, &operation.State, &operation.Result, &operation.Failure, &createdAt, &updatedAt); err != nil {
		return Operation{}, fmt.Errorf("load pending operation: %w", err)
	}

	operation.CreatedAt = time.Unix(createdAt, 0).UTC()
	operation.UpdatedAt = time.Unix(updatedAt, 0).UTC()

	return operation, nil
}
