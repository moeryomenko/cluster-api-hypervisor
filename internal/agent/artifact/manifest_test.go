package artifact

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestLoadManifestVerifiesOwnedArtifacts(t *testing.T) {
	root := t.TempDir()
	images := filepath.Join(root, "images")
	firmware := filepath.Join(root, "firmware")

	if err := os.MkdirAll(images, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(firmware, 0o700); err != nil {
		t.Fatal(err)
	}

	imagePath := filepath.Join(images, "k8labs-base.qcow2")
	firmwarePath := filepath.Join(firmware, "CLOUDHV.fd")

	if err := os.WriteFile(imagePath, []byte("image"), 0o400); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(firmwarePath, []byte("firmware"), 0o400); err != nil {
		t.Fatal(err)
	}

	imageDigest := sha256.Sum256([]byte("image"))
	firmwareDigest := sha256.Sum256([]byte("firmware"))
	manifestPath := filepath.Join(root, "manifest.json")

	contents := fmt.Sprintf(
		`{"images":{"kubernetes-v1.32.13":{"path":%q,"sha256":%q}},"firmware":{"path":%q,"sha256":%q}}`,
		imagePath,
		fmt.Sprintf("%x", imageDigest),
		firmwarePath,
		fmt.Sprintf("%x", firmwareDigest),
	)
	if err := os.WriteFile(manifestPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	manifest, err := LoadManifest(manifestPath, images, firmware)
	if err != nil {
		t.Fatal(err)
	}

	resolved, err := manifest.Image("kubernetes-v1.32.13")
	if err != nil {
		t.Fatal(err)
	}

	if resolved.Path != imagePath {
		t.Fatalf("resolved image path = %q, want %q", resolved.Path, imagePath)
	}
}

func TestLoadManifestRejectsUnownedOrMalformedArtifacts(t *testing.T) {
	root := t.TempDir()
	images := filepath.Join(root, "images")
	firmware := filepath.Join(root, "firmware")

	if err := os.MkdirAll(images, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := os.MkdirAll(firmware, 0o700); err != nil {
		t.Fatal(err)
	}

	firmwarePath := filepath.Join(firmware, "CLOUDHV.fd")
	if err := os.WriteFile(firmwarePath, []byte("firmware"), 0o400); err != nil {
		t.Fatal(err)
	}

	firmwareDigest := sha256.Sum256([]byte("firmware"))
	manifestPath := filepath.Join(root, "manifest.json")

	contents := fmt.Sprintf(
		`{"images":{"kubernetes-v1.32.13":{"path":"/outside.qcow2","sha256":"not-a-checksum"}},"firmware":{"path":%q,"sha256":%q}}`,
		firmwarePath,
		fmt.Sprintf("%x", firmwareDigest),
	)
	if err := os.WriteFile(manifestPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := LoadManifest(manifestPath, images, firmware); err == nil {
		t.Fatal("LoadManifest accepted an unowned malformed image")
	}
}
