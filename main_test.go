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
	"fmt"
	"io"
	"math/big"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/rest"
	clusterv1 "sigs.k8s.io/cluster-api/api/core/v1beta2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	infrastructurev1alpha1 "github.com/moeryomenko/cluster-api-hypervisor/api/v1alpha1"
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

// Infra-controller wiring contract (test-first): the single manager built
// from this package must register the two infrastructure controllers,
// HypervisorCluster and HypervisorMachine, in addition to the five webhooks,
// and must accept one concurrency flag per controller.
//
// The pinned contract, in prose:
//
//   - The binary documents --hypervisorcluster-concurrency and
//     --hypervisormachine-concurrency in its --help output. Both are integer
//     flags that default to 1, and both reject a non-integer value at
//     startup with a flag parse error.
//   - Started against the envtest control plane with a kubeconfig and a
//     webhook certificate directory, the manager accepts both concurrency
//     flags and runs. The controller-runtime metrics endpoint (the manager
//     does not configure one, so it binds the standard :8080) then exposes,
//     per controller, a controller_runtime_max_concurrent_reconciles series
//     valued at the concurrency flag of that controller. That series is the
//     registration proof: a registered controller announces itself through
//     the metric, and its value proves the flag is plumbed into the
//     controller options. The proof needs no host operation.
//   - Both controllers engage on the test objects without host operations:
//     each records a successful reconcile (controller_runtime_reconcile_total
//     with result "success"). The HypervisorCluster controller reconciles the
//     paused object and honors the paused gate; the HypervisorMachine
//     controller reconciles an unowned machine and stops at owner
//     resolution.
//   - The paused gate holds at the manager level: a HypervisorCluster
//     carrying the standard paused annotation, linked to a CAPI Cluster
//     through the infrastructure reference, the owner reference, and the
//     clusterName link, is left untouched — same resource version, no
//     finalizer, not ready, no conditions — across a settle window. The gate
//     itself is pinned by the controller's own unit suite; this test only
//     proves the controller is wired into the manager without netlink, nft,
//     or dnsmasq state.
//   - The HypervisorMachine controller is registered too, proven without any
//     host operation: a machine with no owning CAPI Machine and no cluster
//     link engages the controller, which stops at owner resolution before
//     any host-side step, leaving the object untouched.
//
// While the manager does not yet wire the controllers, the
// --hypervisormachine-concurrency flag is missing: the binary exits on
// startup with an unknown-flag error and --help does not document the flag,
// so this suite fails (red phase).

// infraConcurrencyFlags are the per-controller concurrency flags the manager
// must document and accept: one for the HypervisorCluster controller, one
// for the HypervisorMachine controller.
var infraConcurrencyFlags = []string{
	"hypervisorcluster-concurrency",
	"hypervisormachine-concurrency",
}

// TestMainInfraControllerFlags runs the provider binary with --help and
// asserts that both per-controller concurrency flags are documented as
// integer flags defaulting to 1, and that the binary rejects a non-integer
// value for each flag at startup.
func TestMainInfraControllerFlags(t *testing.T) {
	bin := buildManagerBinary(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--help")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("manager --help exited with an error: %v (stderr: %s)", err, stderr.String())
	}
	output := stdout.String() + stderr.String()

	for _, flag := range infraConcurrencyFlags {
		t.Run(flag, func(t *testing.T) {
			// The flag is documented with the integer type.
			intFlag := regexp.MustCompile(`(?m)--` + flag + `\s+int\b`)
			if !intFlag.MatchString(output) {
				t.Errorf("manager --help does not document --%s as an int flag", flag)
			}

			// The documented default is 1.
			defaultOne := regexp.MustCompile(`(?m)--` + flag + `[^\n]*\(default 1\)`)
			if !defaultOne.MatchString(output) {
				t.Errorf("manager --help does not document --%s with default 1", flag)
			}

			// A non-integer value is rejected as a flag parse error, which
			// proves the flag is parsed as an integer at runtime rather than
			// accepted as an opaque string.
			reject := exec.CommandContext(ctx, bin, "--"+flag+"=not-an-int")
			var rejectOut, rejectErr bytes.Buffer
			reject.Stdout = &rejectOut
			reject.Stderr = &rejectErr
			if err := reject.Run(); err == nil {
				t.Fatalf("manager accepted --%s=not-an-int, want a flag parse error", flag)
			}
			if msg := rejectErr.String(); !strings.Contains(msg, "invalid argument") {
				t.Errorf("manager rejected --%s=not-an-int with %q, want an invalid-argument parse error", flag, msg)
			}
		})
	}
}

// TestMainInfraControllersRegistered starts the provider binary against the
// envtest control plane with both per-controller concurrency flags set to a
// non-default value and asserts the wiring contract: the manager runs, the
// metrics endpoint exposes one max-concurrent-reconciles series per
// infrastructure controller carrying the flag value, both controllers record
// a successful reconcile on the test objects without host operations, a
// paused HypervisorCluster linked to a CAPI Cluster stays untouched, and a
// HypervisorMachine without an owning Machine stays untouched.
func TestMainInfraControllersRegistered(t *testing.T) {
	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}
	installCAPICoreCRDs(t, envTest.Env.Config)

	// The test client is built over the manager's own package-level scheme,
	// so it understands exactly the types the manager registers at startup.
	c, err := client.New(envTest.Env.Config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("create envtest client: %v", err)
	}

	const (
		namespace   = "main-infra-wiring"
		clusterName = "paused-capi-cluster"
		machineName = "unowned-machine"
	)

	createInfraNamespace(t, c, namespace)
	cluster := newCAPIInfrastructureCluster(t, c, namespace, clusterName)
	pausedHC := newPausedHypervisorCluster(t, c, namespace, cluster)
	baselineResourceVersion := pausedHC.ResourceVersion
	newBareHypervisorMachine(t, c, namespace, machineName)

	kubeconfigPath := writeEnvTestKubeconfig(t, envTest.Env.KubeConfig)

	certDir := t.TempDir()
	_, serverCertPEM, serverKeyPEM := generateWebhookCerts(t)
	writeFile(t, filepath.Join(certDir, "tls.crt"), serverCertPEM)
	writeFile(t, filepath.Join(certDir, "tls.key"), serverKeyPEM)

	// The concurrency flags are passed at their non-default value 2, so the
	// metrics series values prove the flags are plumbed into the controller
	// options and not merely accepted.
	mgr := startManager(t,
		"--kubeconfig", kubeconfigPath,
		"--webhook-cert-dir", certDir,
		"--webhook-port", "9443",
		"--health-addr", ":9440",
		"--hypervisorcluster-concurrency", "2",
		"--hypervisormachine-concurrency", "2",
	)

	waitForHealthz(t, mgr, "http://127.0.0.1:9440/healthz", 30*time.Second)

	// Registration proof without host operations: the metrics endpoint
	// exposes a max-concurrent-reconciles series per infrastructure
	// controller carrying the concurrency flag value, and both controllers
	// record a successful reconcile on the test objects. A reconcile that
	// attempted host work would surface as an error result instead.
	waitForControllerConcurrency(t, mgr, "hypervisorcluster", 2, 30*time.Second)
	waitForControllerConcurrency(t, mgr, "hypervisormachine", 2, 30*time.Second)
	waitForControllerReconcileSuccess(t, mgr, "hypervisorcluster", 30*time.Second)
	waitForControllerReconcileSuccess(t, mgr, "hypervisormachine", 30*time.Second)

	// The paused gate holds at the manager level: the paused
	// HypervisorCluster is never modified — same resource version, no
	// finalizer, not ready, no conditions — across a settle window.
	pausedKey := client.ObjectKey{Namespace: namespace, Name: clusterName}
	assertPausedClusterUntouched(t, c, pausedKey, baselineResourceVersion)

	// The machine controller stops at owner resolution: the unowned machine
	// gains no finalizer, no status, and no provider ID.
	machineKey := client.ObjectKey{Namespace: namespace, Name: machineName}
	hm := &infrastructurev1alpha1.HypervisorMachine{}
	if err := c.Get(t.Context(), machineKey, hm); err != nil {
		t.Fatalf("Get HypervisorMachine: %v", err)
	}
	if len(hm.Finalizers) != 0 || hm.Status.Ready || len(hm.Status.Addresses) != 0 || hm.Status.ProviderID != nil {
		t.Errorf("unowned HypervisorMachine modified: finalizers=%v ready=%v addresses=%v providerID=%v",
			hm.Finalizers, hm.Status.Ready, hm.Status.Addresses, hm.Status.ProviderID)
	}

	mgr.stop(t)
}

// createInfraNamespace creates the namespace the infra wiring objects live
// in and schedules its removal on cleanup.
func createInfraNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()

	if err := c.Create(t.Context(), &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}}); err != nil {
		t.Fatalf("create namespace %q: %v", name, err)
	}
	t.Cleanup(func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = c.Delete(cleanupCtx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: name}})
	})
}

// newCAPIInfrastructureCluster creates a CAPI Cluster whose
// infrastructureRef names the HypervisorCluster with the same name, exactly
// as the CAPI topology links the two objects.
func newCAPIInfrastructureCluster(t *testing.T, c client.Client, namespace, name string) *clusterv1.Cluster {
	t.Helper()

	cluster := &clusterv1.Cluster{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
		Spec: clusterv1.ClusterSpec{
			InfrastructureRef: clusterv1.ContractVersionedObjectReference{
				APIGroup: infrastructurev1alpha1.GroupVersion.Group,
				Kind:     "HypervisorCluster",
				Name:     name,
			},
		},
	}
	if err := c.Create(t.Context(), cluster); err != nil {
		t.Fatalf("create Cluster %q: %v", name, err)
	}
	return cluster
}

// newPausedHypervisorCluster creates the HypervisorCluster for cluster,
// linked back through the owner reference and the clusterName link, and
// carries the standard paused annotation so a registered cluster controller
// must skip it. The freshly created object is returned so the test can pin
// its resource version as the untouched baseline.
func newPausedHypervisorCluster(
	t *testing.T,
	c client.Client,
	namespace string,
	cluster *clusterv1.Cluster,
) *infrastructurev1alpha1.HypervisorCluster {
	t.Helper()

	hc := &infrastructurev1alpha1.HypervisorCluster{
		ObjectMeta: metav1.ObjectMeta{
			Name:      cluster.Name,
			Namespace: namespace,
			Annotations: map[string]string{
				clusterv1.PausedAnnotation: "true",
			},
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion: clusterv1.GroupVersion.String(),
					Kind:       "Cluster",
					Name:       cluster.Name,
					UID:        cluster.UID,
				},
			},
		},
		Spec: infrastructurev1alpha1.HypervisorClusterSpec{
			ClusterName: cluster.Name,
			Network: infrastructurev1alpha1.HypervisorClusterNetworkSpec{
				CIDR:       "192.168.124.0/24",
				Gateway:    "192.168.124.1",
				DNSIP:      "192.168.124.1",
				BridgeName: "k8sbr0",
				NATTable:   "k8slab",
			},
		},
	}
	if err := c.Create(t.Context(), hc); err != nil {
		t.Fatalf("create paused HypervisorCluster %q: %v", cluster.Name, err)
	}
	return hc
}

// newBareHypervisorMachine creates a HypervisorMachine with no owning CAPI
// Machine and no cluster link. A registered HypervisorMachine controller
// engages on the object and stops at owner resolution, before any host-side
// step.
func newBareHypervisorMachine(t *testing.T, c client.Client, namespace, name string) {
	t.Helper()

	hm := &infrastructurev1alpha1.HypervisorMachine{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: namespace},
	}
	if err := c.Create(t.Context(), hm); err != nil {
		t.Fatalf("create HypervisorMachine %q: %v", name, err)
	}
}

// installCAPICoreCRDs installs the cluster-api core CRDs (Cluster and
// Machine) into the envtest control plane. The provider CRDs are already
// installed by the committed harness; the cluster-api CRDs ship in the module
// cache of the pinned cluster-api version.
func installCAPICoreCRDs(t *testing.T, cfg *rest.Config) {
	t.Helper()

	dir, err := capiCRDDirectory()
	if err != nil {
		t.Fatalf("resolve cluster-api module directory: %v", err)
	}
	paths := []string{
		filepath.Join(dir, "cluster.x-k8s.io_clusters.yaml"),
		filepath.Join(dir, "cluster.x-k8s.io_machines.yaml"),
	}
	if _, err := envtest.InstallCRDs(cfg, envtest.CRDInstallOptions{Paths: paths}); err != nil {
		t.Fatalf("install cluster-api core CRDs: %v", err)
	}
}

// capiCRDDirectory resolves the CRD manifest directory of the pinned
// sigs.k8s.io/cluster-api module.
func capiCRDDirectory() (string, error) {
	out, err := exec.Command("go", "list", "-m", "-f", "{{.Dir}}", "sigs.k8s.io/cluster-api").Output()
	if err != nil {
		return "", fmt.Errorf("go list -m sigs.k8s.io/cluster-api: %w", err)
	}
	return filepath.Join(strings.TrimSpace(string(out)), "config", "crd", "bases"), nil
}

// scrapeMetrics performs one GET of the manager metrics endpoint and returns
// the body on a 200 response. The manager binds the metrics server to the
// controller-runtime default address :8080.
func scrapeMetrics(mgr *runningManager) (string, error) {
	httpClient := &http.Client{Timeout: 2 * time.Second}
	resp, err := httpClient.Get("http://127.0.0.1:8080/metrics")
	if err != nil {
		return "", err
	}
	body, err := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	if err != nil {
		return "", err
	}
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("metrics endpoint status %d", resp.StatusCode)
	}
	return string(body), nil
}

// waitForControllerConcurrency polls the manager metrics endpoint until the
// named controller reports the expected max-concurrent-reconciles value.
func waitForControllerConcurrency(
	t *testing.T,
	mgr *runningManager,
	controller string,
	want int,
	timeout time.Duration,
) {
	t.Helper()

	re := regexp.MustCompile(`controller_runtime_max_concurrent_reconciles\{controller="` + controller + `"\} (\d+)`)
	deadline := time.Now().Add(timeout)
	var lastBody string
	for {
		if !mgr.alive() {
			t.Fatalf("manager exited while waiting for controller %q concurrency; stderr:\n%s", controller, mgr.stderr.String())
		}
		body, err := scrapeMetrics(mgr)
		if err == nil {
			lastBody = body
			if m := re.FindStringSubmatch(body); m != nil {
				if got, err := strconv.Atoi(m[1]); err == nil && got == want {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller %q never reported max-concurrent-reconciles %d (metrics body:\n%s)", controller, want, lastBody)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// waitForControllerReconcileSuccess polls the manager metrics endpoint until
// the named controller records at least one successful reconcile. A
// registered controller that engaged on the test objects without a host
// operation records a success; a controller that attempted host work and
// failed records an error result instead, which the failure message
// surfaces.
func waitForControllerReconcileSuccess(t *testing.T, mgr *runningManager, controller string, timeout time.Duration) {
	t.Helper()

	re := regexp.MustCompile(
		`controller_runtime_reconcile_total\{controller="` + controller + `",result="success"\} (\d+)`,
	)
	deadline := time.Now().Add(timeout)
	var lastBody string
	for {
		if !mgr.alive() {
			t.Fatalf("manager exited while waiting for controller %q reconcile; stderr:\n%s", controller, mgr.stderr.String())
		}
		body, err := scrapeMetrics(mgr)
		if err == nil {
			lastBody = body
			if m := re.FindStringSubmatch(body); m != nil {
				if n, err := strconv.Atoi(m[1]); err == nil && n >= 1 {
					return
				}
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("controller %q never reported a successful reconcile (metrics body:\n%s)", controller, lastBody)
		}
		time.Sleep(200 * time.Millisecond)
	}
}

// assertPausedClusterUntouched polls the paused HypervisorCluster for a
// settle window, failing as soon as the object deviates from its creation
// baseline: resource version, finalizers, readiness, and conditions must all
// stay as created.
func assertPausedClusterUntouched(t *testing.T, c client.Client, key client.ObjectKey, baselineResourceVersion string) {
	t.Helper()

	deadline := time.Now().Add(10 * time.Second)
	for {
		hc := &infrastructurev1alpha1.HypervisorCluster{}
		if err := c.Get(t.Context(), key, hc); err != nil {
			t.Fatalf("Get paused HypervisorCluster: %v", err)
		}
		if hc.ResourceVersion != baselineResourceVersion {
			t.Fatalf("paused HypervisorCluster modified: resourceVersion %s -> %s", baselineResourceVersion, hc.ResourceVersion)
		}
		if len(hc.Finalizers) != 0 {
			t.Fatalf("paused HypervisorCluster gained finalizers %v", hc.Finalizers)
		}
		if hc.Status.Ready || len(hc.Status.Conditions) != 0 {
			t.Fatalf("paused HypervisorCluster status changed: ready=%v conditions=%v", hc.Status.Ready, hc.Status.Conditions)
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}

// Bootstrap and control-plane wiring contract (test-first): the single
// manager built from this package must register the bootstrap and
// control-plane controllers, HypervisorConfig and HypervisorControlPlane, in
// addition to the two infrastructure controllers and the five webhooks, and
// must accept one concurrency flag per controller.
//
// The pinned contract, in prose:
//
//   - The binary documents --hypervisorconfig-concurrency and
//     --hypervisorcontrolplane-concurrency in its --help output. Both are
//     integer flags that default to 1, and both reject a non-integer value at
//     startup with a flag parse error.
//   - Started against the envtest control plane with a kubeconfig and a
//     webhook certificate directory, the manager accepts both concurrency
//     flags and runs. The controller-runtime metrics endpoint (the manager
//     does not configure one, so it binds the standard :8080) then exposes,
//     per controller, a controller_runtime_max_concurrent_reconciles series
//     valued at the concurrency flag of that controller. That series is the
//     registration proof: a registered controller announces itself through
//     the metric, and its value proves the flag is plumbed into the
//     controller options. The proof needs no host operation and no fixture
//     objects.
//   - The registration proof is deliberately metric-only and creates no
//     HypervisorConfig or HypervisorControlPlane objects. A HypervisorConfig
//     linked to a CAPI Machine and Cluster would trigger real PKI generation
//     (a cluster PKI Secret and a rendered data Secret), and a paused probe
//     would be filtered by the controllers' paused event gate before any
//     reconcile; the metrics series alone prove registration and flag
//     plumbing. With no objects on the control plane, neither controller
//     records any reconcile across a settle window, which is the zero
//     side-effect proof: a reconcile that ran would surface as a
//     controller_runtime_reconcile_total series.
//
// While the manager does not yet wire the controllers, the
// --hypervisorconfig-concurrency and --hypervisorcontrolplane-concurrency
// flags are missing: the binary exits on startup with an unknown-flag error
// and --help does not document the flags, so this suite fails (red phase).

// bootstrapConcurrencyFlags are the per-controller concurrency flags the
// manager must document and accept for the bootstrap and control-plane
// controllers: one for the HypervisorConfig controller, one for the
// HypervisorControlPlane controller.
var bootstrapConcurrencyFlags = []string{
	"hypervisorconfig-concurrency",
	"hypervisorcontrolplane-concurrency",
}

// TestMainBootstrapControllerFlags runs the provider binary with --help and
// asserts that both per-controller concurrency flags are documented as
// integer flags defaulting to 1, and that the binary rejects a non-integer
// value for each flag at startup.
func TestMainBootstrapControllerFlags(t *testing.T) {
	bin := buildManagerBinary(t)

	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin, "--help")
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("manager --help exited with an error: %v (stderr: %s)", err, stderr.String())
	}
	output := stdout.String() + stderr.String()

	for _, flag := range bootstrapConcurrencyFlags {
		t.Run(flag, func(t *testing.T) {
			// The flag is documented with the integer type.
			intFlag := regexp.MustCompile(`(?m)--` + flag + `\s+int\b`)
			if !intFlag.MatchString(output) {
				t.Errorf("manager --help does not document --%s as an int flag", flag)
			}

			// The documented default is 1.
			defaultOne := regexp.MustCompile(`(?m)--` + flag + `[^\n]*\(default 1\)`)
			if !defaultOne.MatchString(output) {
				t.Errorf("manager --help does not document --%s with default 1", flag)
			}

			// A non-integer value is rejected as a flag parse error, which
			// proves the flag is parsed as an integer at runtime rather than
			// accepted as an opaque string.
			reject := exec.CommandContext(ctx, bin, "--"+flag+"=not-an-int")
			var rejectOut, rejectErr bytes.Buffer
			reject.Stdout = &rejectOut
			reject.Stderr = &rejectErr
			if err := reject.Run(); err == nil {
				t.Fatalf("manager accepted --%s=not-an-int, want a flag parse error", flag)
			}
			if msg := rejectErr.String(); !strings.Contains(msg, "invalid argument") {
				t.Errorf("manager rejected --%s=not-an-int with %q, want an invalid-argument parse error", flag, msg)
			}
		})
	}
}

// TestMainBootstrapControllersRegistered starts the provider binary against
// the envtest control plane with both per-controller concurrency flags set to
// a non-default value and asserts the wiring contract: the manager runs, the
// metrics endpoint exposes one max-concurrent-reconciles series per
// controller carrying the flag value, and neither controller records any
// reconcile across a settle window. The proof creates no fixture objects: a
// HypervisorConfig linked to a Machine and Cluster would trigger real PKI
// generation and data-Secret rendering, and a paused probe would be filtered
// by the controllers' paused event gate; the metrics series alone prove
// registration and flag plumbing, and the reconcile series absence proves
// zero side effects.
func TestMainBootstrapControllersRegistered(t *testing.T) {
	envTest, err := helpers.StartEnvTest(t)
	if err != nil {
		t.Fatalf("helpers.StartEnvTest: %v", err)
	}
	installCAPICoreCRDs(t, envTest.Env.Config)

	kubeconfigPath := writeEnvTestKubeconfig(t, envTest.Env.KubeConfig)

	certDir := t.TempDir()
	_, serverCertPEM, serverKeyPEM := generateWebhookCerts(t)
	writeFile(t, filepath.Join(certDir, "tls.crt"), serverCertPEM)
	writeFile(t, filepath.Join(certDir, "tls.key"), serverKeyPEM)

	// The concurrency flags are passed at their non-default value 2, so the
	// metrics series values prove the flags are plumbed into the controller
	// options and not merely accepted.
	mgr := startManager(t,
		"--kubeconfig", kubeconfigPath,
		"--webhook-cert-dir", certDir,
		"--webhook-port", "9443",
		"--health-addr", ":9440",
		"--hypervisorconfig-concurrency", "2",
		"--hypervisorcontrolplane-concurrency", "2",
	)

	waitForHealthz(t, mgr, "http://127.0.0.1:9440/healthz", 30*time.Second)

	// Registration proof without host operations or side effects: the metrics
	// endpoint exposes a max-concurrent-reconciles series per controller
	// carrying the concurrency flag value.
	waitForControllerConcurrency(t, mgr, "hypervisorconfig", 2, 30*time.Second)
	waitForControllerConcurrency(t, mgr, "hypervisorcontrolplane", 2, 30*time.Second)

	// Zero reconcile side effects: with no fixture objects on the control
	// plane, neither controller records any reconcile during a settle window.
	// A reconcile that ran would surface as a reconcile_total series and fail
	// the assertion.
	assertNoControllerReconciles(t, mgr, "hypervisorconfig", 5*time.Second)
	assertNoControllerReconciles(t, mgr, "hypervisorcontrolplane", 5*time.Second)

	mgr.stop(t)
}

// assertNoControllerReconciles polls the manager metrics endpoint for a
// settle window and fails as soon as the named controller records a
// reconcile, proven by a controller_runtime_reconcile_total series whose
// count is above 0. controller-runtime pre-creates the reconcile_total
// series at value 0 when a controller starts (Controller.Start's
// initMetrics), so series presence alone is not a side effect; a reconcile
// that ran carries a count above 0 and fails the assertion.
func assertNoControllerReconciles(t *testing.T, mgr *runningManager, controller string, timeout time.Duration) {
	t.Helper()

	re := regexp.MustCompile(`controller_runtime_reconcile_total\{controller="` + controller + `"(?:,[^}]*)?\} (\d+)`)
	deadline := time.Now().Add(timeout)
	for {
		if !mgr.alive() {
			t.Fatalf(
				"manager exited while watching controller %q reconcile activity; stderr:\n%s",
				controller,
				mgr.stderr.String(),
			)
		}
		body, err := scrapeMetrics(mgr)
		if err == nil {
			for _, m := range re.FindAllStringSubmatch(body, -1) {
				n, err := strconv.Atoi(m[1])
				if err != nil {
					t.Fatalf("controller %q reconcile_total series has non-integer value %q: %s", controller, m[1], m[0])
				}
				if n > 0 {
					t.Fatalf("controller %q recorded an unexpected reconcile count %d: %s", controller, n, m[0])
				}
			}
		}
		if time.Now().After(deadline) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
}
