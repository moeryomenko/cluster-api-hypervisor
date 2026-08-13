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

// Manager startup contract (test-first): the provider binary, started against
// the envtest control plane with a kubeconfig, a webhook certificate
// directory, a webhook port, and a health address, must
//
//   - keep running (it must not exit immediately),
//   - answer GET /healthz on the health address with 200 OK, and
//   - serve the admission webhook paths over TLS using the serving certificate
//     provisioned in the certificate directory (tls.crt/tls.key), so a client
//     trusting that directory's CA completes a TLS handshake and receives an
//     HTTP-level response.
//
// The webhook response status is not part of the contract: a plain GET against
// an admission path may answer 400/404/405 and still satisfy the TLS
// handshake requirement. The manager skeleton that implements this contract
// lands in a later task; while main.go is the empty stub, this suite fails
// (red phase).
package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/test/helpers"
)

// runningManager is a started provider binary process with its output
// captured. The process is reaped by a background goroutine so callers can
// observe an early exit without waiting for the health endpoints.
type runningManager struct {
	cmd    *exec.Cmd
	stdout bytes.Buffer
	stderr bytes.Buffer
	exited chan struct{}
}

// TestManagerStartsWithEnvTestAndServesHealthz builds the real provider
// binary, starts it against the envtest control plane, and asserts the
// startup contract: the process stays up, /healthz answers 200, and the
// webhook listener speaks TLS with the provisioned serving certificate.
func TestManagerStartsWithEnvTestAndServesHealthz(t *testing.T) {
	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}

	kubeconfigPath := writeEnvTestKubeconfig(t, envTest.Env.KubeConfig)

	certDir := t.TempDir()
	caPEM, serverCertPEM, serverKeyPEM := generateWebhookCerts(t)
	writeFile(t, filepath.Join(certDir, "tls.crt"), serverCertPEM)
	writeFile(t, filepath.Join(certDir, "tls.key"), serverKeyPEM)
	caPath := filepath.Join(certDir, "ca.crt")
	writeFile(t, caPath, caPEM)

	mgr := startManager(t,
		"--kubeconfig", kubeconfigPath,
		"--webhook-cert-dir", certDir,
		"--webhook-port", "9443",
		"--health-addr", ":9440",
	)

	waitForHealthz(t, mgr, "http://127.0.0.1:9440/healthz", 30*time.Second)

	const webhookURL = "https://127.0.0.1:9443/validate-infrastructure-cluster-x-k8s-io-v1alpha1-hypervisorcluster"
	assertWebhookSpeaksTLS(t, mgr, webhookURL, caPath, 15*time.Second)

	mgr.stop(t)
}

// startManager builds the provider binary, starts it with args, and captures
// its output. The process is killed in cleanup if the test fails before the
// test calls stop.
func startManager(t *testing.T, args ...string) *runningManager {
	t.Helper()

	mgr := &runningManager{
		cmd:    exec.Command(buildManagerBinary(t), args...),
		exited: make(chan struct{}),
	}
	mgr.cmd.Stdout = &mgr.stdout
	mgr.cmd.Stderr = &mgr.stderr
	if err := mgr.cmd.Start(); err != nil {
		t.Fatalf("start manager binary: %v", err)
	}
	go func() {
		_ = mgr.cmd.Wait()
		close(mgr.exited)
	}()
	t.Cleanup(func() {
		if mgr.cmd.Process != nil {
			_ = mgr.cmd.Process.Kill()
		}
		select {
		case <-mgr.exited:
		case <-time.After(5 * time.Second):
			t.Logf("manager process did not exit after kill")
		}
	})
	return mgr
}

// stop terminates the manager with SIGTERM and waits up to 10 seconds for a
// clean exit before falling back to SIGKILL.
func (m *runningManager) stop(t *testing.T) {
	t.Helper()

	select {
	case <-m.exited:
		return
	default:
	}

	if err := m.cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Logf("send SIGTERM to manager: %v", err)
	}
	select {
	case <-m.exited:
		if m.cmd.ProcessState.ExitCode() != 0 {
			t.Logf("manager exited with status %d after SIGTERM (stderr: %s)",
				m.cmd.ProcessState.ExitCode(), m.stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = m.cmd.Process.Kill()
		<-m.exited
		t.Logf("manager ignored SIGTERM for 10s; killed")
	}
}

// alive reports whether the manager process has already exited.
func (m *runningManager) alive() bool {
	select {
	case <-m.exited:
		return false
	default:
		return true
	}
}

// buildManagerBinary compiles the provider binary from the repository root
// into a temp directory. The build runs offline: module downloads are
// disabled so a missing module cache fails fast instead of hanging on the
// network.
func buildManagerBinary(t *testing.T) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Minute)
	defer cancel()

	bin := filepath.Join(t.TempDir(), "cluster-api-hypervisor")
	cmd := exec.CommandContext(ctx, "go", "build", "-o", bin, ".")
	cmd.Env = append(os.Environ(), "GOPROXY=off", "GOTOOLCHAIN=local")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("go build manager binary: %v: %s", err, out)
	}
	return bin
}

// writeEnvTestKubeconfig persists the envtest control plane kubeconfig to a
// temp file and returns its path.
func writeEnvTestKubeconfig(t *testing.T, data []byte) string {
	t.Helper()

	if len(data) == 0 {
		t.Fatalf("envtest control plane did not expose a kubeconfig")
	}
	path := filepath.Join(t.TempDir(), "kubeconfig")
	writeFile(t, path, data)
	return path
}

// waitForHealthz polls url until it answers 200 OK or the deadline passes.
// An early process exit fails the test immediately with the captured stderr.
func waitForHealthz(t *testing.T, mgr *runningManager, url string, timeout time.Duration) {
	t.Helper()

	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for {
		if !mgr.alive() {
			t.Fatalf("manager exited before serving %s (last: %s); stderr:\n%s", url, last, mgr.stderr.String())
		}
		resp, err := client.Get(url)
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			switch {
			case readErr != nil:
				last = readErr.Error()
			case resp.StatusCode == http.StatusOK:
				return
			default:
				last = string(body)
			}
		} else {
			last = err.Error()
		}
		if time.Now().After(deadline) {
			t.Fatalf("health endpoint %s never answered 200 (last: %s); stderr:\n%s", url, last, mgr.stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertWebhookSpeaksTLS dials url with the provisioned CA as the trust root
// and requires an HTTP-level response: any status (400/404/405/...) is
// acceptable, the contract is that the TLS handshake succeeds with the
// provisioned certificate. A certificate verification error fails immediately;
// connection-level errors are retried until the deadline.
func assertWebhookSpeaksTLS(t *testing.T, mgr *runningManager, url, caPath string, timeout time.Duration) {
	t.Helper()

	caPEM, err := os.ReadFile(caPath)
	if err != nil {
		t.Fatalf("read CA certificate %s: %v", caPath, err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		t.Fatalf("CA file %s contains no usable certificates", caPath)
	}

	client := &http.Client{
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
		},
		Timeout: 2 * time.Second,
	}

	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		resp, err := client.Get(url)
		if err == nil {
			_ = resp.Body.Close()
			return
		}
		if isTLSVerificationError(err) {
			t.Fatalf("webhook TLS certificate rejected by the provisioned CA: %v", err)
		}
		lastErr = err
		if time.Now().After(deadline) {
			t.Fatalf("webhook TLS endpoint %s never became reachable (last error: %v); stderr:\n%s",
				url, lastErr, mgr.stderr.String())
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// isTLSVerificationError reports whether err is a certificate or hostname
// verification failure (as opposed to a connection-level error).
func isTLSVerificationError(err error) bool {
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	var invalidCertificate x509.CertificateInvalidError
	if errors.As(err, &invalidCertificate) {
		return true
	}
	var hostnameError x509.HostnameError
	if errors.As(err, &hostnameError) {
		return true
	}
	var verificationError *tls.CertificateVerificationError
	return errors.As(err, &verificationError)
}

// generateWebhookCerts returns a self-signed CA and a server certificate
// signed by it (SANs: 127.0.0.1, ::1, localhost) as PEM blocks, mirroring the
// static webhook certificate provisioning of the install contract.
func generateWebhookCerts(t *testing.T) (caPEM, serverCertPEM, serverKeyPEM []byte) {
	t.Helper()

	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate CA key: %v", err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "cluster-api-hypervisor test CA"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create CA certificate: %v", err)
	}
	caCert, err := x509.ParseCertificate(caDER)
	if err != nil {
		t.Fatalf("parse CA certificate: %v", err)
	}

	serverKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatalf("generate webhook server key: %v", err)
	}
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "cluster-api-hypervisor webhook"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
		DNSNames:     []string{"localhost"},
	}
	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create webhook server certificate: %v", err)
	}
	serverKeyDER, err := x509.MarshalECPrivateKey(serverKey)
	if err != nil {
		t.Fatalf("marshal webhook server key: %v", err)
	}

	caPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	serverCertPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER})
	serverKeyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: serverKeyDER})
	return caPEM, serverCertPEM, serverKeyPEM
}

// writeFile writes data to path with owner-only permissions, failing the test
// on error.
func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()

	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}
