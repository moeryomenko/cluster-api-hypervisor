/*
Copyright 2026 The cluster-api-hypervisor Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

// Package helpers provides test helpers shared by the envtest suites.
package helpers

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	k8sruntime "k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	bootstrapv1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/bootstrap/v1alpha1"
	controlplanev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/controlplane/v1alpha1"
	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
)

// envtestK8sVersion is the Kubernetes version whose envtest binaries the
// harness resolves when KUBEBUILDER_ASSETS is unset. It matches
// ENVTEST_K8S_VERSION in the Makefile, which is the only 1.35.x release
// offered by setup-envtest.
const envtestK8sVersion = "1.35.0"

// EnvTest is the running envtest control plane and the client bound to it.
type EnvTest struct {
	// Client is a controller-runtime client bound to the envtest REST config
	// with the registered scheme.
	Client client.Client
	// Env is the underlying envtest environment, kept so future controller
	// suites can inspect the config.
	Env *envtest.Environment
}

// StartEnvTest starts the envtest control plane with the k8s 1.35.x binaries
// resolved by setup-envtest (KUBEBUILDER_ASSETS / make envtest), registers
// the scheme (clientgoscheme plus infrastructure v1alpha1, bootstrap
// v1alpha1, controlplane v1alpha1), installs the five CRDs from
// config/crd/bases, and stops the control plane when the test completes
// (t.Cleanup). It returns an error when the control plane cannot start (e.g.
// missing binaries).
func StartEnvTest(t *testing.T) (*EnvTest, error) {
	assetsDir, err := binaryAssetsDir()
	if err != nil {
		return nil, err
	}

	scheme := k8sruntime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register client-go scheme: %w", err)
	}

	if err := infrastructurev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register infrastructure v1alpha1 scheme: %w", err)
	}

	if err := bootstrapv1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register bootstrap v1alpha1 scheme: %w", err)
	}

	if err := controlplanev1alpha1.AddToScheme(scheme); err != nil {
		return nil, fmt.Errorf("register controlplane v1alpha1 scheme: %w", err)
	}

	env := &envtest.Environment{
		BinaryAssetsDirectory: assetsDir,
		CRDDirectoryPaths:     []string{crdDirectory()},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		return nil, fmt.Errorf("start envtest control plane: %w", err)
	}

	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Errorf("stop envtest control plane: %v", err)
		}
	})

	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, fmt.Errorf("create envtest client: %w", err)
	}

	return &EnvTest{Client: c, Env: env}, nil
}

// binaryAssetsDir returns the envtest binaries directory: KUBEBUILDER_ASSETS
// when set, otherwise the path resolved by setup-envtest for the pinned
// Kubernetes version.
func binaryAssetsDir() (string, error) {
	if dir := os.Getenv("KUBEBUILDER_ASSETS"); dir != "" {
		return dir, nil
	}

	out, err := exec.Command("go", "tool", "setup-envtest", "use", envtestK8sVersion, "-p", "path").Output()
	if err != nil {
		return "", fmt.Errorf("resolve envtest binaries (set KUBEBUILDER_ASSETS or run make envtest): %w", err)
	}

	return strings.TrimSpace(string(out)), nil
}

// crdDirectory returns the absolute path to the CRD manifests, anchored at
// this source file so the path resolves regardless of the test binary's
// working directory.
func crdDirectory() string {
	_, file, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(file), "..", "..", "config", "crd", "bases")
}
