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

// Workload-cluster seed contract: after the control-plane apiserver is
// healthy, the provider seeds the fresh workload cluster with (a) the
// cluster-ca -> cluster-admin RBAC binding, (b) a pre-allocated kube-dns
// Service at the fixed clusterIP 10.96.0.10 (so the kubelet clusterDNS is
// deterministic before node configs render), and (c) k8sServiceHost/
// k8sServicePort on the cilium-config ConfigMap pointing Cilium at the
// control-plane apiserver directly. Each step is idempotent and must not
// fail when the CRS has not yet created the cilium-config.
package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes/fake"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// TestSeedClusterAdminUsesPublishedEndpoint pins the published HTTPS endpoint
// used by the management-plane controller. Replacing its hostname with
// pod-local 127.0.0.1 breaks both routing and TLS hostname verification.
func TestSeedClusterAdminUsesPublishedEndpoint(t *testing.T) {
	const cpIP = "192.168.124.2"

	clusterPKI, err := pki.GenerateClusterPKI(cpIP, "k8labs-cp")
	if err != nil {
		t.Fatalf("GenerateClusterPKI: %v", err)
	}

	caBlock, _ := pem.Decode(clusterPKI.CA)
	if caBlock == nil {
		t.Fatal("cluster CA is not PEM encoded")
	}

	caCert, err := x509.ParseCertificate(caBlock.Bytes)
	if err != nil {
		t.Fatalf("parse cluster CA: %v", err)
	}

	keyBlock, _ := pem.Decode(clusterPKI.CAKey)
	if keyBlock == nil {
		t.Fatal("cluster CA key is not PEM encoded")
	}

	caKey, err := x509.ParsePKCS1PrivateKey(keyBlock.Bytes)
	if err != nil {
		t.Fatalf("parse cluster CA key: %v", err)
	}

	serverKey, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("generate test apiserver key: %v", err)
	}

	now := time.Now()
	serverTemplate := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test workload apiserver"},
		NotBefore:    now.Add(-time.Minute),
		NotAfter:     now.Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}

	serverDER, err := x509.CreateCertificate(rand.Reader, serverTemplate, caCert, &serverKey.PublicKey, caKey)
	if err != nil {
		t.Fatalf("create test apiserver certificate: %v", err)
	}

	serverCert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: serverDER}),
		pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(serverKey)}),
	)
	if err != nil {
		t.Fatalf("parse test apiserver keypair: %v", err)
	}

	var (
		sawPublishedAuthority bool
		publishedAuthority    string
	)

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Host == publishedAuthority {
			sawPublishedAuthority = true
		}

		w.Header().Set("Content-Type", "application/json")

		switch r.URL.Path {
		case "/apis/rbac.authorization.k8s.io/v1/clusterrolebindings/cluster-ca-admin":
			_, _ = w.Write(
				[]byte(
					`{"apiVersion":"rbac.authorization.k8s.io/v1","kind":"ClusterRoleBinding","metadata":{"name":"cluster-ca-admin"}}`,
				),
			)
		case "/api/v1/namespaces/kube-system/services/kube-dns":
			_, _ = w.Write(
				[]byte(`{"apiVersion":"v1","kind":"Service","metadata":{"name":"kube-dns","namespace":"kube-system"}}`),
			)
		case "/api/v1/namespaces/kube-system/configmaps/cilium-config":
			_, _ = w.Write(
				[]byte(
					`{"apiVersion":"v1","kind":"ConfigMap","metadata":{"name":"cilium-config","namespace":"kube-system"},"data":{"k8sServiceHost":"192.168.124.2","k8sServicePort":"6443"}}`,
				),
			)
		default:
			http.NotFound(w, r)
		}
	})
	server := httptest.NewUnstartedServer(handler)
	server.TLS = &tls.Config{Certificates: []tls.Certificate{serverCert}}
	port := server.Listener.Addr().(*net.TCPAddr).Port
	publishedAuthority = net.JoinHostPort("localhost", strconv.Itoa(port))

	server.StartTLS()
	t.Cleanup(server.Close)

	serverURL := "https://" + publishedAuthority
	if err := seedClusterAdmin(t.Context(), serverURL, cpIP, clusterPKI); err != nil {
		t.Fatalf("seedClusterAdmin: %v", err)
	}

	if !sawPublishedAuthority {
		t.Errorf("seed client did not reach the published endpoint authority %q", publishedAuthority)
	}
}

// TestSeedClusterAdminBinding pins the RBAC seed: it creates the
// cluster-ca -> cluster-admin binding and is idempotent on a second call.
func TestSeedClusterAdminBinding(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	if err := seedClusterAdminBinding(ctx, cs); err != nil {
		t.Fatalf("seedClusterAdminBinding: %v", err)
	}

	b, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, "cluster-ca-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cluster-ca-admin binding: %v", err)
	}

	if b.RoleRef.Name != "cluster-admin" {
		t.Errorf("binding role = %q, want cluster-admin", b.RoleRef.Name)
	}

	if len(b.Subjects) != 1 || b.Subjects[0].Name != "cluster-ca" {
		t.Errorf("binding subjects = %+v, want [cluster-ca]", b.Subjects)
	}

	// Idempotent: a second call must not error.
	if err := seedClusterAdminBinding(ctx, cs); err != nil {
		t.Fatalf("seedClusterAdminBinding (2nd): %v", err)
	}
}

// TestSeedKubeDNSService pins the kube-dns pre-allocation: the Service is
// created in kube-system with the fixed clusterIP 10.96.0.10 and a second
// call is a no-op.
func TestSeedKubeDNSService(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	if err := seedKubeDNSService(ctx, cs); err != nil {
		t.Fatalf("seedKubeDNSService: %v", err)
	}

	svc, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get kube-dns Service: %v", err)
	}

	if svc.Spec.ClusterIP != "10.96.0.10" {
		t.Errorf("kube-dns clusterIP = %q, want 10.96.0.10", svc.Spec.ClusterIP)
	}

	if svc.Spec.Selector["k8s-app"] != "kube-dns" {
		t.Errorf("kube-dns selector = %v, want k8s-app=kube-dns", svc.Spec.Selector)
	}

	// Idempotent.
	if err := seedKubeDNSService(ctx, cs); err != nil {
		t.Fatalf("seedKubeDNSService (2nd): %v", err)
	}
}

// TestSeedKubeDNSServiceHonorsExisting pins that a kube-dns Service already
// present (e.g. from a prior run) is left untouched.
func TestSeedKubeDNSServiceHonorsExisting(t *testing.T) {
	ctx := context.Background()
	existing := &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "kube-dns", Namespace: "kube-system"},
		Spec:       corev1.ServiceSpec{ClusterIP: "10.96.0.10"},
	}
	cs := fake.NewSimpleClientset(existing)

	if err := seedKubeDNSService(ctx, cs); err != nil {
		t.Fatalf("seedKubeDNSService on existing Service: %v", err)
	}

	got, err := cs.CoreV1().Services("kube-system").Get(ctx, "kube-dns", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get kube-dns: %v", err)
	}

	if got.Spec.ClusterIP != "10.96.0.10" {
		t.Errorf("kube-dns clusterIP changed to %q", got.Spec.ClusterIP)
	}
}

// TestSeedCiliumConfig pins the cilium-config patch: it sets
// k8sServiceHost/k8sServicePort to the control-plane IP and is idempotent.
func TestSeedCiliumConfig(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: "kube-system"},
		Data:       map[string]string{"kube-proxy-replacement": "true"},
	})

	if err := seedCiliumConfig(ctx, cs, "192.168.124.20"); err != nil {
		t.Fatalf("seedCiliumConfig: %v", err)
	}

	cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "cilium-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cilium-config: %v", err)
	}

	if cm.Data["k8sServiceHost"] != "192.168.124.20" {
		t.Errorf("k8sServiceHost = %q, want 192.168.124.20", cm.Data["k8sServiceHost"])
	}

	if cm.Data["k8sServicePort"] != strconv.Itoa(controlPlaneAPIServerPort) {
		t.Errorf("k8sServicePort = %q, want %d", cm.Data["k8sServicePort"], controlPlaneAPIServerPort)
	}
	// The pre-existing key must survive the patch.
	if cm.Data["kube-proxy-replacement"] != "true" {
		t.Errorf("kube-proxy-replacement lost after patch: %v", cm.Data)
	}

	// Idempotent.
	if err := seedCiliumConfig(ctx, cs, "192.168.124.20"); err != nil {
		t.Fatalf("seedCiliumConfig (2nd): %v", err)
	}
}

// TestSeedCiliumConfigMissingConfigMap pins the tolerate-missing behavior:
// before the CRS has applied the cilium-config, the seed is a no-op (the
// reconcile retries on the next pass) rather than an error.
func TestSeedCiliumConfigMissingConfigMap(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()

	if err := seedCiliumConfig(ctx, cs, "192.168.124.20"); err != nil {
		t.Fatalf("seedCiliumConfig on missing ConfigMap: %v", err)
	}

	_, err := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "cilium-config", metav1.GetOptions{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("cilium-config should not be created by the seed: %v", err)
	}
}

// TestSeedCiliumConfigEmptyIP pins that an empty control-plane IP is a no-op
// (the CP machine has no internal IP yet).
func TestSeedCiliumConfigEmptyIP(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset(&corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "cilium-config", Namespace: "kube-system"},
		Data:       map[string]string{},
	})

	if err := seedCiliumConfig(ctx, cs, ""); err != nil {
		t.Fatalf("seedCiliumConfig with empty IP: %v", err)
	}

	cm, err := cs.CoreV1().ConfigMaps("kube-system").Get(ctx, "cilium-config", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get cilium-config: %v", err)
	}

	if _, ok := cm.Data["k8sServiceHost"]; ok {
		t.Errorf("k8sServiceHost set with empty CP IP")
	}
}

// TestSeedClusterAdminPinsRbacTypes guards the RBAC wire types used by the
// seed against accidental drift.
func TestSeedClusterAdminPinsRbacTypes(t *testing.T) {
	ctx := context.Background()
	cs := fake.NewSimpleClientset()
	_ = seedClusterAdminBinding(ctx, cs)

	b, err := cs.RbacV1().ClusterRoleBindings().Get(ctx, "cluster-ca-admin", metav1.GetOptions{})
	if err != nil {
		t.Fatalf("get binding: %v", err)
	}

	if b.RoleRef.APIGroup != rbacv1.GroupName || b.RoleRef.Kind != "ClusterRole" {
		t.Errorf("roleRef = %+v, want rbac.authorization.k8s.io/ClusterRole", b.RoleRef)
	}
}
