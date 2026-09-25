package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"
)

type run struct{ calls [][]string }

func (r *run) Run(_ context.Context, command string, args ...string) ([]byte, error) {
	r.calls = append(r.calls, append([]string{command}, args...))
	return nil, nil
}

func TestPrepareRootDiskIsIdempotent(t *testing.T) {
	root := t.TempDir()

	source := filepath.Join(root, "source.raw")
	if err := os.WriteFile(source, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	runner := &run{}
	b := Builder{Root: root, QemuImg: "qemu-img", Run: runner}

	path, err := b.PrepareRootDisk(context.Background(), "machine-a", source)
	if err != nil {
		t.Fatal(err)
	}

	if len(runner.calls) != 1 || filepath.Base(path) != "machine-a-root.qcow2" {
		t.Fatalf("calls=%v path=%s", runner.calls, path)
	}

	if err := os.WriteFile(path, []byte("disk"), 0o600); err != nil {
		t.Fatal(err)
	}

	_, err = b.PrepareRootDisk(context.Background(), "machine-a", source)
	if err != nil || len(runner.calls) != 1 {
		t.Fatalf("repeat=%v calls=%v", err, runner.calls)
	}
}

func TestPrepareCIDATACreatesRequiredToolSequence(t *testing.T) {
	runner := &run{}
	b := Builder{Root: t.TempDir(), Mkdosfs: "mkdosfs", Mcopy: "mcopy", Run: runner}

	result, err := b.PrepareCIDATA(
		context.Background(),
		"machine-a",
		map[string][]byte{"user-data": []byte("u"), "meta-data": []byte("m"), "network-config": []byte("n")},
	)
	if err != nil {
		t.Fatal(err)
	}

	if result.SHA256 == "" || len(runner.calls) != 4 {
		t.Fatalf("result=%#v calls=%v", result, runner.calls)
	}
}

func TestPrepareCIDATARejectsIncompleteAndUnsafeInput(t *testing.T) {
	b := Builder{Root: t.TempDir(), Run: &run{}}
	if _, err := b.PrepareCIDATA(context.Background(), "../escape", map[string][]byte{}); err == nil {
		t.Fatal("unsafe name accepted")
	}

	if _, err := b.PrepareCIDATA(context.Background(), "machine-a", map[string][]byte{"user-data": nil}); err == nil {
		t.Fatal("incomplete parts accepted")
	}
}
