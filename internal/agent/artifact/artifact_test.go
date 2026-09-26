package artifact

import (
	"context"
	"crypto/sha256"
	"fmt"
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

func TestVerifyOwnedFileRejectsSymlinksAndChecksumMismatch(t *testing.T) {
	root := t.TempDir()

	firmware := filepath.Join(root, "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256([]byte("firmware"))

	checksum := fmt.Sprintf("%x", digest)
	if err := VerifyOwnedFile(root, firmware, checksum); err != nil {
		t.Fatalf("VerifyOwnedFile() error = %v", err)
	}

	if err := VerifyOwnedFile(root, firmware, "00"+checksum[2:]); err == nil {
		t.Fatal("VerifyOwnedFile() accepted a checksum mismatch")
	}

	link := filepath.Join(root, "firmware-link.fd")
	if err := os.Symlink(firmware, link); err != nil {
		t.Fatal(err)
	}

	if err := VerifyOwnedFile(root, link, checksum); err == nil {
		t.Fatal("VerifyOwnedFile() accepted a symbolic link")
	}
}

func TestVerifyOwnedFileRejectsInvalidInputAndOutsidePath(t *testing.T) {
	root := t.TempDir()

	firmware := filepath.Join(root, "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o600); err != nil {
		t.Fatal(err)
	}

	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte("firmware")))
	for _, test := range []struct {
		name, path, checksum string
	}{
		{name: "relative path", path: "CLOUDHV.fd", checksum: checksum},
		{name: "short checksum", path: firmware, checksum: checksum[:len(checksum)-1]},
		{name: "malformed checksum", path: firmware, checksum: "zz" + checksum[2:]},
		{name: "outside root", path: filepath.Join(t.TempDir(), "CLOUDHV.fd"), checksum: checksum},
	} {
		t.Run(test.name, func(t *testing.T) {
			if err := VerifyOwnedFile(root, test.path, test.checksum); err == nil {
				t.Fatal("VerifyOwnedFile() accepted invalid firmware input")
			}
		})
	}
}

func TestVerifyOwnedFileAllowsResolvedOwnedRoot(t *testing.T) {
	parent := t.TempDir()

	root := filepath.Join(parent, "firmware")
	if err := os.Mkdir(root, 0o750); err != nil {
		t.Fatal(err)
	}

	rootLink := filepath.Join(parent, "firmware-root")
	if err := os.Symlink(root, rootLink); err != nil {
		t.Fatal(err)
	}

	firmware := filepath.Join(rootLink, "CLOUDHV.fd")
	if err := os.WriteFile(firmware, []byte("firmware"), 0o600); err != nil {
		t.Fatal(err)
	}

	checksum := fmt.Sprintf("%x", sha256.Sum256([]byte("firmware")))
	if err := VerifyOwnedFile(rootLink, firmware, checksum); err != nil {
		t.Fatalf("VerifyOwnedFile() error = %v", err)
	}
}

func TestVerifyRejectsEscapesAndChecksumMismatch(t *testing.T) {
	root := t.TempDir()
	owned := filepath.Join(root, "disk.img")

	outside := filepath.Join(t.TempDir(), "outside.img")
	if err := os.WriteFile(owned, []byte("owned"), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	digest := sha256.Sum256([]byte("owned"))
	checksum := fmt.Sprintf("%x", digest)

	b := Builder{Root: root}
	if err := b.Verify([]string{owned}, []string{checksum}); err != nil {
		t.Fatalf("verify owned file: %v", err)
	}

	if err := b.Verify([]string{owned}, []string{"00" + checksum[2:]}); err == nil {
		t.Fatal("checksum mismatch accepted")
	}

	link := filepath.Join(root, "escape.img")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}

	if err := b.Verify([]string{link}, []string{fmt.Sprintf("%x", sha256.Sum256([]byte("outside")))}); err == nil {
		t.Fatal("symlink escape accepted")
	}
}

func TestVerifyRejectsPathOutsideOwnedRoot(t *testing.T) {
	root := t.TempDir()

	outside := filepath.Join(t.TempDir(), "outside.img")
	if err := os.WriteFile(outside, []byte("outside"), 0o600); err != nil {
		t.Fatal(err)
	}

	b := Builder{Root: root}
	if err := b.Verify([]string{outside}, []string{fmt.Sprintf("%x", sha256.Sum256([]byte("outside")))}); err == nil {
		t.Fatal("outside path accepted")
	}
}
