package executor

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/artifact"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

func TestPrepareConfextBuildsAndReplaysRawArtifact(t *testing.T) {
	executor, _, store := testExecutor(t)
	executor.Artifacts = artifact.Builder{Root: filepath.Join(t.TempDir(), "artifacts"), Run: commandRunner{}}
	mutation := mutation()
	mutation.IdempotencyKey = "confext-machine-a-1"
	files := []hostagent.ArtifactFile{{Name: "z-kubelet/etc/kubernetes/kubelet.conf", Content: []byte("config")}}

	result, err := executor.PrepareConfext(context.Background(), mutation, "machine-a", files)
	if err != nil {
		t.Fatal(err)
	}

	if len(result.Paths) != 1 || len(result.SHA256s) != 1 {
		t.Fatalf("result=%#v", result)
	}

	replayed, err := executor.PrepareConfext(context.Background(), mutation, "machine-a", files)
	if err != nil {
		t.Fatal(err)
	}

	if replayed.Paths[0] != result.Paths[0] || replayed.SHA256s[0] != result.SHA256s[0] {
		t.Fatalf("replayed=%#v result=%#v", replayed, result)
	}

	if _, err := store.PendingOperations(); err != nil {
		t.Fatal(err)
	}
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return nil, nil
}
