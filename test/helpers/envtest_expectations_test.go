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

// Harness contract (test-first, task 14): the envtest harness lives in
// test/helpers/envtest.go, implemented by task 15. This file pins the exact
// contract the controllers suite depends on. Until envtest.go exists, this
// package fails to compile with "undefined: EnvTest" and "undefined:
// StartEnvTest" — the intended red phase state.
//
// Contract for task 15:
//
//	type EnvTest struct {
//		// Client is a controller-runtime client bound to the envtest REST
//		// config with the registered scheme.
//		Client client.Client
//		// Env is the underlying envtest environment, kept so future
//		// controller suites can inspect the config.
//		Env *envtest.Environment
//	}
//
//	// StartEnvTest starts the envtest control plane with the k8s 1.35.x
//	// binaries resolved by setup-envtest (KUBEBUILDER_ASSETS / make
//	// envtest), registers the scheme (clientgoscheme plus
//	// infrastructure v1alpha1, bootstrap v1alpha1, controlplane v1alpha1),
//	// installs the five CRDs from config/crd/bases, and stops the control
//	// plane when the test completes (t.Cleanup). It returns an error when
//	// the control plane cannot start (e.g. missing binaries).
//	func StartEnvTest(t *testing.T) (*EnvTest, error)
package helpers

import "testing"

// TestEnvTestContract pins the harness entry point. It compiles only when
// task 15 provides StartEnvTest with exactly the signature below; the pin
// itself is a compile-time assertion, so the test body is intentionally
// empty at runtime.
func TestEnvTestContract(t *testing.T) {
	var _ func(t *testing.T) (*EnvTest, error) = StartEnvTest
}
