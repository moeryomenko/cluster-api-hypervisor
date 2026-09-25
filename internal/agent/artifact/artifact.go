package artifact

import (
	"context"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type Runner interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type Builder struct {
	Root, QemuImg, Mkdosfs, Mcopy string
	Run                           Runner
}

type CIDATA struct {
	Path   string
	SHA256 string
}

func (b Builder) PrepareRootDisk(ctx context.Context, name, source string) (string, error) {
	if err := b.validName(name); err != nil {
		return "", err
	}

	if !filepath.IsAbs(source) || !regularFile(source) {
		return "", fmt.Errorf("invalid source image %q", source)
	}

	if err := os.MkdirAll(b.Root, 0o750); err != nil {
		return "", fmt.Errorf("create artifact root: %w", err)
	}

	destination := filepath.Join(b.Root, name+"-root.qcow2")
	if !within(b.Root, destination) {
		return "", fmt.Errorf("root disk path escaped artifact root")
	}

	if regularFile(destination) {
		return destination, nil
	}

	if _, err := b.Run.Run(ctx, b.QemuImg, "convert", "-O", "qcow2", source, destination); err != nil {
		return "", fmt.Errorf("create root disk: %w", err)
	}

	return destination, nil
}

func (b Builder) PrepareCIDATA(ctx context.Context, name string, parts map[string][]byte) (CIDATA, error) {
	if err := b.validName(name); err != nil {
		return CIDATA{}, err
	}

	for _, required := range []string{"user-data", "meta-data", "network-config"} {
		if _, ok := parts[required]; !ok {
			return CIDATA{}, fmt.Errorf("CIDATA part %q is required", required)
		}
	}

	if err := os.MkdirAll(b.Root, 0o750); err != nil {
		return CIDATA{}, fmt.Errorf("create artifact root: %w", err)
	}

	staging := filepath.Join(b.Root, name+"-cidata-parts")
	if !within(b.Root, staging) {
		return CIDATA{}, fmt.Errorf("CIDATA staging escaped artifact root")
	}

	if err := os.MkdirAll(staging, 0o750); err != nil {
		return CIDATA{}, fmt.Errorf("create CIDATA staging: %w", err)
	}

	keys := make([]string, 0, len(parts))
	for key := range parts {
		keys = append(keys, key)
	}

	sort.Strings(keys)

	for _, key := range keys {
		if strings.Contains(key, "/") || key == "." || key == ".." {
			return CIDATA{}, fmt.Errorf("invalid CIDATA part %q", key)
		}

		if err := os.WriteFile(filepath.Join(staging, key), parts[key], 0o600); err != nil {
			return CIDATA{}, fmt.Errorf("write CIDATA part: %w", err)
		}
	}

	path := filepath.Join(b.Root, name+"-cidata.img")
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return CIDATA{}, err
	}

	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return CIDATA{}, err
	}

	if err := file.Truncate(4 << 20); err != nil {
		_ = file.Close()
		return CIDATA{}, err
	}

	if err := file.Close(); err != nil {
		return CIDATA{}, err
	}

	if _, err := b.Run.Run(ctx, b.Mkdosfs, "-F", "16", "-s", "1", "-n", "CIDATA", path); err != nil {
		return CIDATA{}, fmt.Errorf("format CIDATA: %w", err)
	}

	for _, key := range []string{"user-data", "meta-data", "network-config"} {
		if _, err := b.Run.Run(ctx, b.Mcopy, "-i", path, filepath.Join(staging, key), "::"+key); err != nil {
			return CIDATA{}, fmt.Errorf("copy CIDATA part %s: %w", key, err)
		}
	}

	contents, err := os.ReadFile(path)
	if err != nil {
		return CIDATA{}, err
	}

	digest := sha256.Sum256(contents)

	return CIDATA{Path: path, SHA256: fmt.Sprintf("%x", digest)}, nil
}

func (b Builder) validName(name string) error {
	if b.Run == nil || b.Root == "" || !filepath.IsAbs(b.Root) || name == "" || strings.ContainsAny(name, "/\\\r\n") {
		return fmt.Errorf("invalid artifact builder or name")
	}

	return nil
}

func within(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func regularFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular()
}
