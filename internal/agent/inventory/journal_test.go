package inventory

import (
	"errors"
	"path/filepath"
	"testing"
)

func journalOperation() Operation {
	return Operation{
		InstallationID: "install-a",
		OwnerUID:       "machine-a",
		NodeID:         "node-a",
		Key:            "ensure-a-1",
		Kind:           "EnsureVM",
		RequestHash:    "sha256:request-a",
		Generation:     1,
	}
}

func TestOperationJournalReplaysTerminalResultAndRejectsChangedInputs(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	operation := journalOperation()

	stored, completed, err := store.BeginOperation(operation)
	if err != nil || completed || stored.State != OperationIntent {
		t.Fatalf("BeginOperation() = %#v, %v, %v", stored, completed, err)
	}

	if err := store.CompleteOperation(operation, OperationCompleted, `{"pid":42}`, ""); err != nil {
		t.Fatal(err)
	}

	stored, completed, err = store.BeginOperation(operation)
	if err != nil || !completed || stored.State != OperationCompleted || stored.Result != `{"pid":42}` {
		t.Fatalf("replay = %#v, %v, %v", stored, completed, err)
	}

	operation.RequestHash = "sha256:different"
	if _, _, err := store.BeginOperation(operation); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed payload error = %v, want conflict", err)
	}

	operation = journalOperation()

	operation.Generation++
	if _, _, err := store.BeginOperation(operation); !errors.Is(err, ErrIdempotencyConflict) {
		t.Fatalf("changed generation error = %v, want conflict", err)
	}
}

func TestOperationJournalRecoversInterruptedIntentAfterReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "inventory.db")

	store, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}

	operation := journalOperation()
	if _, _, err := store.BeginOperation(operation); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	pending, err := store.PendingOperations()
	if err != nil || len(pending) != 1 || pending[0].State != OperationIntent {
		t.Fatalf("PendingOperations() = %#v, %v", pending, err)
	}

	if err := store.CompleteOperation(operation, OperationFailed, "", "observed unit absent"); err != nil {
		t.Fatal(err)
	}

	pending, err = store.PendingOperations()
	if err != nil || len(pending) != 0 {
		t.Fatalf("pending after completion = %#v, %v", pending, err)
	}
}

func TestOperationJournalDoesNotOverwriteTerminalOutcome(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "inventory.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()

	operation := journalOperation()
	if _, _, err := store.BeginOperation(operation); err != nil {
		t.Fatal(err)
	}

	if err := store.CompleteOperation(operation, OperationCompleted, "one", ""); err != nil {
		t.Fatal(err)
	}

	if err := store.CompleteOperation(operation, OperationFailed, "", "different"); !errors.Is(
		err,
		ErrIdempotencyConflict,
	) {
		t.Fatalf("overwrite error = %v, want conflict", err)
	}
}
