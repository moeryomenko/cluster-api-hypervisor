package artifact

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ImmutableFile identifies an Agent-owned immutable artifact.
type ImmutableFile struct {
	Path   string `json:"path"`
	SHA256 string `json:"sha256"`
}

// Manifest is the authoritative mapping from manager-safe image references to
// immutable files staged under Agent-owned roots.
type Manifest struct {
	Images   map[string]ImmutableFile `json:"images"`
	Firmware ImmutableFile            `json:"firmware"`
}

// LoadManifest reads and verifies an Agent-owned artifact manifest. It verifies
// every staged file before the Agent accepts requests, while EnsureVM retains the
// final firmware verification directly before vm.create.
func LoadManifest(path, baseImageRoot, firmwareRoot string) (Manifest, error) {
	if !filepath.IsAbs(path) || !filepath.IsAbs(baseImageRoot) || !filepath.IsAbs(firmwareRoot) {
		return Manifest{}, fmt.Errorf("manifest and owned roots must be absolute")
	}

	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() {
		return Manifest{}, fmt.Errorf("artifact manifest is not a regular file: %q", path)
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return Manifest{}, fmt.Errorf("read artifact manifest: %w", err)
	}

	var manifest Manifest
	if err := json.Unmarshal(contents, &manifest); err != nil {
		return Manifest{}, fmt.Errorf("parse artifact manifest: %w", err)
	}

	if len(manifest.Images) == 0 {
		return Manifest{}, fmt.Errorf("artifact manifest has no images")
	}

	if err := VerifyOwnedFile(firmwareRoot, manifest.Firmware.Path, manifest.Firmware.SHA256); err != nil {
		return Manifest{}, fmt.Errorf("verify manifest firmware: %w", err)
	}

	for reference, image := range manifest.Images {
		if strings.TrimSpace(reference) == "" {
			return Manifest{}, fmt.Errorf("artifact manifest has an empty image reference")
		}

		if err := VerifyOwnedFile(baseImageRoot, image.Path, image.SHA256); err != nil {
			return Manifest{}, fmt.Errorf("verify manifest image %q: %w", reference, err)
		}
	}

	return manifest, nil
}

// Image resolves an opaque manager-provided reference to an Agent-owned image.
func (m Manifest) Image(reference string) (ImmutableFile, error) {
	image, ok := m.Images[reference]
	if !ok || strings.TrimSpace(reference) == "" {
		return ImmutableFile{}, fmt.Errorf("unknown base image reference %q", reference)
	}

	return image, nil
}
