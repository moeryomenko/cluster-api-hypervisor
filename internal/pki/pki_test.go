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

// The cluster PKI and kubeconfig generation contract (test-first).
//
// This suite pins the certificate and kubeconfig material the bootstrap and
// control-plane providers generate for a cluster, mirroring the phase-B
// hand-rolled PKI: no kubeadm anywhere. Cluster-scoped PKI is generated once
// per cluster: a self-signed cluster CA, a distinct self-signed front-proxy
// CA, an apiserver certificate whose SANs include the control-plane node's
// static IP and node name, a front-proxy client certificate signed by the
// front-proxy CA, and a service-account keypair. Per-machine artifacts are the
// kubelet certificate and key signed by the cluster CA. Kubeconfigs are
// rendered from PEM material with a fixed server URL.
//
// The contract, in prose:
//
//   - ClusterPKI holds the PEM-encoded certificate and key material for one
//     cluster: CA/CAKey (the cluster signing authority),
//     FrontProxyCA/FrontProxyCAKey (a distinct front-proxy signing
//     authority), APIServer/APIServerKey (the apiserver serving certificate),
//     FrontProxy/FrontProxyKey (the front-proxy client certificate),
//     ServiceAccount/ServiceAccountKey (the service-account keypair). Every
//     field is a PEM block: certificates are CERTIFICATE blocks, keys are
//     private keys in any standard PEM encoding.
//   - GenerateClusterPKI(cpIP, cpName string) (ClusterPKI, error) generates
//     the cluster PKI. cpIP is the control-plane node's static IP and cpName
//     its node name; both must be non-empty and cpIP must be a valid IP
//     literal. The cluster CA and the front-proxy CA are self-signed and
//     distinct; the apiserver certificate carries cpIP as an IP SAN and cpName
//     as a DNS SAN; the apiserver and service-account certificates are signed
//     by the cluster CA and the front-proxy client certificate is signed by
//     the front-proxy CA — never self-signed.
//   - GenerateKubeletCert(clusterPKI ClusterPKI, nodeName string)
//     (certPEM, keyPEM []byte, err error) generates one node's kubelet
//     certificate and key, signed by the cluster CA. The certificate common
//     name is exactly nodeName, and nodeName must be non-empty. The key and
//     certificate belong to the same keypair.
//   - RenderKubeconfig(caPEM []byte, serverURL, user string, clientCert,
//     clientKey []byte) ([]byte, error) renders a kubeconfig whose cluster
//     server is exactly serverURL, whose certificate-authority-data is the
//     base64 encoding of caPEM, whose user is named exactly user, and whose
//     client-certificate-data and client-key-data are the base64 encodings of
//     clientCert and clientKey. Given identical inputs it always returns
//     byte-identical output. serverURL must be non-empty and caPEM must be a
//     parseable PEM certificate.
//
// Determinism is pinned for the kubeconfig render only. PKI generation draws
// fresh random keys on every call, so GenerateClusterPKI and
// GenerateKubeletCert are deliberately NOT pinned to be byte-identical across
// calls; their output is pinned through parse/verify semantics instead.
package pki_test

import (
	"bytes"
	"crypto"
	"crypto/x509"
	"encoding/base64"
	"encoding/pem"
	"net"
	"slices"
	"testing"

	"gopkg.in/yaml.v3"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/pki"
)

// Compile-time pins: the exported type and functions must exist with exactly
// these names and signatures.
var (
	_ []byte = pki.ClusterPKI{}.CA
	_ []byte = pki.ClusterPKI{}.CAKey
	_ []byte = pki.ClusterPKI{}.APIServer
	_ []byte = pki.ClusterPKI{}.APIServerKey
	_ []byte = pki.ClusterPKI{}.FrontProxy
	_ []byte = pki.ClusterPKI{}.FrontProxyKey
	_ []byte = pki.ClusterPKI{}.ServiceAccount
	_ []byte = pki.ClusterPKI{}.ServiceAccountKey
	_ []byte = pki.ClusterPKI{}.FrontProxyCA
	_ []byte = pki.ClusterPKI{}.FrontProxyCAKey

	_ func(string, string) (pki.ClusterPKI, error)                 = pki.GenerateClusterPKI
	_ func(pki.ClusterPKI, string) ([]byte, []byte, error)         = pki.GenerateKubeletCert
	_ func([]byte, string, string, []byte, []byte) ([]byte, error) = pki.RenderKubeconfig
)

// decodeCert decodes a single PEM certificate block and parses it as an x509
// certificate, failing the test on any structural problem.
func decodeCert(t *testing.T, pemBytes []byte) *x509.Certificate {
	t.Helper()

	block, rest := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("no PEM block in certificate bytes")
	}
	if block.Type != "CERTIFICATE" {
		t.Fatalf("PEM block type %q, want CERTIFICATE", block.Type)
	}
	if len(rest) != 0 {
		t.Fatalf("trailing data after certificate PEM block")
	}

	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		t.Fatalf("ParseCertificate error: %v", err)
	}

	return cert
}

// decodePrivateKey parses a private key from its PEM encoding, accepting the
// PKCS#8, PKCS#1, and EC encodings, and returns it as a crypto.Signer.
func decodePrivateKey(t *testing.T, pemBytes []byte) crypto.Signer {
	t.Helper()

	block, rest := pem.Decode(pemBytes)
	if block == nil {
		t.Fatalf("no PEM block in private key bytes")
	}
	if len(rest) != 0 {
		t.Fatalf("trailing data after private key PEM block")
	}

	if key, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		signer, ok := key.(crypto.Signer)
		if !ok {
			t.Fatalf("PKCS#8 key does not implement crypto.Signer")
		}
		return signer
	}
	if key, err := x509.ParsePKCS1PrivateKey(block.Bytes); err == nil {
		return key
	}
	if key, err := x509.ParseECPrivateKey(block.Bytes); err == nil {
		return key
	}
	t.Fatalf("private key bytes parse as neither PKCS#8, PKCS#1, nor EC")
	return nil
}

// assertPublicKeyMatches fails when the certificate public key and the given
// private key do not belong to the same keypair.
func assertPublicKeyMatches(t *testing.T, cert *x509.Certificate, key crypto.Signer) {
	t.Helper()

	certPK, err := x509.MarshalPKIXPublicKey(cert.PublicKey)
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey(cert) error: %v", err)
	}
	keyPK, err := x509.MarshalPKIXPublicKey(key.Public())
	if err != nil {
		t.Fatalf("MarshalPKIXPublicKey(key) error: %v", err)
	}
	if !bytes.Equal(certPK, keyPK) {
		t.Errorf("certificate public key does not match the private key")
	}
}

// verifyAgainstCA verifies cert as a leaf against the given CA certificate and
// returns the verification result. The caller decides whether an error is
// expected.
func verifyAgainstCA(cert, ca *x509.Certificate) error {
	roots := x509.NewCertPool()
	roots.AddCert(ca)

	_, err := cert.Verify(x509.VerifyOptions{
		Roots:     roots,
		KeyUsages: []x509.ExtKeyUsage{x509.ExtKeyUsageAny},
	})

	return err
}

// assertNotSignedBy fails when cert unexpectedly verifies against ca.
func assertNotSignedBy(t *testing.T, cert, ca *x509.Certificate) {
	t.Helper()

	if err := verifyAgainstCA(cert, ca); err == nil {
		t.Errorf("certificate unexpectedly verifies against a CA that did not sign it")
	}
}

// TestGenerateClusterPKIProducesParsableMaterial pins the shape of the
// generated cluster PKI: every field is a PEM block, the CA is self-signed and
// marked as a CA, and each certificate/key pair belongs together.
func TestGenerateClusterPKIProducesParsableMaterial(t *testing.T) {
	const (
		cpIP   = "192.168.124.10"
		cpName = "cp1"
	)

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}

	ca := decodeCert(t, pk.CA)
	if !ca.IsCA {
		t.Errorf("CA certificate IsCA = false, want true")
	}
	if err := verifyAgainstCA(ca, ca); err != nil {
		t.Errorf("CA does not verify as its own signer: %v", err)
	}
	assertPublicKeyMatches(t, ca, decodePrivateKey(t, pk.CAKey))

	t.Run("apiserver", func(t *testing.T) {
		cert := decodeCert(t, pk.APIServer)
		assertPublicKeyMatches(t, cert, decodePrivateKey(t, pk.APIServerKey))
	})

	t.Run("front-proxy", func(t *testing.T) {
		cert := decodeCert(t, pk.FrontProxy)
		assertPublicKeyMatches(t, cert, decodePrivateKey(t, pk.FrontProxyKey))
	})

	t.Run("front-proxy-ca", func(t *testing.T) {
		cert := decodeCert(t, pk.FrontProxyCA)
		assertPublicKeyMatches(t, cert, decodePrivateKey(t, pk.FrontProxyCAKey))
	})

	t.Run("service-account", func(t *testing.T) {
		cert := decodeCert(t, pk.ServiceAccount)
		assertPublicKeyMatches(t, cert, decodePrivateKey(t, pk.ServiceAccountKey))
	})
}

// TestGenerateClusterPKIApiserverCertificateSANs pins the SAN membership of
// the apiserver certificate: the control-plane static IP appears as an IP SAN
// and the node name appears as a DNS SAN.
func TestGenerateClusterPKIApiserverCertificateSANs(t *testing.T) {
	const (
		cpIP   = "192.168.124.10"
		cpName = "cp1"
	)

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	cert := decodeCert(t, pk.APIServer)

	wantIP := net.ParseIP(cpIP)
	if wantIP == nil {
		t.Fatalf("test IP %q does not parse", cpIP)
	}

	t.Run("control-plane IP is an IP SAN", func(t *testing.T) {
		for _, got := range cert.IPAddresses {
			if got.Equal(wantIP) {
				return
			}
		}
		t.Errorf("apiserver certificate IP SANs %v do not contain %s", cert.IPAddresses, cpIP)
	})

	t.Run("node name is a DNS SAN", func(t *testing.T) {
		if slices.Contains(cert.DNSNames, cpName) {
			return
		}
		t.Errorf("apiserver certificate DNS SANs %v do not contain %q", cert.DNSNames, cpName)
	})
}

// TestGenerateClusterPKICertificatesVerifyAgainstCA pins that the apiserver
// and service-account certificates are signed by the generated cluster CA,
// and that none of them verifies against a different cluster's CA (they are
// not self-signed). The front-proxy client certificate is pinned separately
// by TestGenerateClusterPKIFrontProxySignedByFrontProxyCA.
func TestGenerateClusterPKICertificatesVerifyAgainstCA(t *testing.T) {
	const (
		cpIP   = "192.168.124.10"
		cpName = "cp1"
	)

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	ca := decodeCert(t, pk.CA)

	certs := []struct {
		name string
		cert *x509.Certificate
	}{
		{name: "apiserver", cert: decodeCert(t, pk.APIServer)},
		{name: "service-account", cert: decodeCert(t, pk.ServiceAccount)},
	}

	for _, entry := range certs {
		t.Run(entry.name, func(t *testing.T) {
			if err := verifyAgainstCA(entry.cert, ca); err != nil {
				t.Errorf("%s certificate does not verify against the cluster CA: %v", entry.name, err)
			}
		})
	}

	// A different generated cluster's CA must not validate this cluster's
	// certificates: each certificate is CA-signed, not self-signed.
	other, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI (second cluster) error: %v", err)
	}
	otherCA := decodeCert(t, other.CA)

	t.Run("not signed by a different CA", func(t *testing.T) {
		for _, entry := range certs {
			t.Run(entry.name, func(t *testing.T) {
				assertNotSignedBy(t, entry.cert, otherCA)
			})
		}
	})
}

// TestGenerateClusterPKIFrontProxySignedByFrontProxyCA pins the front-proxy
// signing authority: the front-proxy CA is a distinct, self-signed CA and the
// front-proxy client certificate is signed by it, never by the cluster CA.
func TestGenerateClusterPKIFrontProxySignedByFrontProxyCA(t *testing.T) {
	const (
		cpIP   = "192.168.124.10"
		cpName = "cp1"
	)

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	ca := decodeCert(t, pk.CA)
	frontProxyCA := decodeCert(t, pk.FrontProxyCA)
	frontProxyCert := decodeCert(t, pk.FrontProxy)

	t.Run("front-proxy CA is a valid CA certificate", func(t *testing.T) {
		if !frontProxyCA.IsCA {
			t.Errorf("front-proxy CA certificate IsCA = false, want true")
		}
		if err := verifyAgainstCA(frontProxyCA, frontProxyCA); err != nil {
			t.Errorf("front-proxy CA does not verify as its own signer: %v", err)
		}
	})

	t.Run("front-proxy client certificate is signed by the front-proxy CA", func(t *testing.T) {
		if err := verifyAgainstCA(frontProxyCert, frontProxyCA); err != nil {
			t.Errorf("front-proxy certificate does not verify against the front-proxy CA: %v", err)
		}
	})

	t.Run("front-proxy CA is distinct from the cluster CA", func(t *testing.T) {
		if bytes.Equal(pk.FrontProxyCA, pk.CA) {
			t.Errorf("front-proxy CA PEM equals the cluster CA PEM, want distinct CAs")
		}
		if err := verifyAgainstCA(frontProxyCA, ca); err == nil {
			t.Errorf("front-proxy CA unexpectedly verifies against the cluster CA")
		}
	})

	t.Run("front-proxy client certificate is not signed by the cluster CA", func(t *testing.T) {
		assertNotSignedBy(t, frontProxyCert, ca)
	})

	t.Run("not signed by a different cluster's front-proxy CA", func(t *testing.T) {
		other, err := pki.GenerateClusterPKI(cpIP, cpName)
		if err != nil {
			t.Fatalf("GenerateClusterPKI (second cluster) error: %v", err)
		}
		assertNotSignedBy(t, frontProxyCert, decodeCert(t, other.FrontProxyCA))
	})
}

// TestGenerateClusterPKIRejectsInvalidInputs pins input validation: an empty
// control-plane IP, a malformed IP literal, an out-of-range IP literal, and an
// empty node name are all rejected.
func TestGenerateClusterPKIRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		cpIP   string
		cpName string
	}{
		{name: "empty control-plane IP", cpIP: "", cpName: "cp1"},
		{name: "malformed control-plane IP", cpIP: "not-an-ip", cpName: "cp1"},
		{name: "out-of-range control-plane IP", cpIP: "999.1.1.1", cpName: "cp1"},
		{name: "empty control-plane name", cpIP: "192.168.124.10", cpName: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := pki.GenerateClusterPKI(tt.cpIP, tt.cpName); err == nil {
				t.Errorf("GenerateClusterPKI(%q, %q) succeeded, want an error", tt.cpIP, tt.cpName)
			}
		})
	}
}

// TestGenerateKubeletCertificate pins the per-machine kubelet artifact: the
// certificate verifies against the cluster CA, its common name is exactly the
// node name, the key matches the certificate, and it is not signed by a
// different cluster's CA. Empty node names and a PKI without a CA are
// rejected.
func TestGenerateKubeletCertificate(t *testing.T) {
	const (
		cpIP     = "192.168.124.10"
		cpName   = "cp1"
		nodeName = "worker1"
	)

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	ca := decodeCert(t, pk.CA)

	certPEM, keyPEM, err := pki.GenerateKubeletCert(pk, nodeName)
	if err != nil {
		t.Fatalf("GenerateKubeletCert error: %v", err)
	}
	cert := decodeCert(t, certPEM)
	key := decodePrivateKey(t, keyPEM)

	t.Run("verifies against the cluster CA", func(t *testing.T) {
		if err := verifyAgainstCA(cert, ca); err != nil {
			t.Errorf("kubelet certificate does not verify against the cluster CA: %v", err)
		}
	})

	t.Run("common name follows Kubernetes convention", func(t *testing.T) {
		want := "system:node:" + nodeName
		if cert.Subject.CommonName != want {
			t.Errorf("kubelet certificate CN = %q, want %q", cert.Subject.CommonName, want)
		}
	})

	t.Run("key matches the certificate", func(t *testing.T) {
		assertPublicKeyMatches(t, cert, key)
	})

	t.Run("not signed by a different CA", func(t *testing.T) {
		other, err := pki.GenerateClusterPKI(cpIP, cpName)
		if err != nil {
			t.Fatalf("GenerateClusterPKI (second cluster) error: %v", err)
		}
		assertNotSignedBy(t, cert, decodeCert(t, other.CA))
	})

	t.Run("rejects an empty node name", func(t *testing.T) {
		if _, _, err := pki.GenerateKubeletCert(pk, ""); err == nil {
			t.Error("GenerateKubeletCert with an empty node name succeeded, want an error")
		}
	})

	t.Run("rejects a PKI without a CA", func(t *testing.T) {
		if _, _, err := pki.GenerateKubeletCert(pki.ClusterPKI{}, nodeName); err == nil {
			t.Error("GenerateKubeletCert with an empty PKI succeeded, want an error")
		}
	})
}

// TestRenderKubeconfig pins kubeconfig rendering: the cluster server is
// exactly the given URL, the certificate-authority-data is the base64 of the
// CA PEM, the user is named exactly as given, and the client certificate and
// key appear as base64. Rendering is deterministic for identical inputs.
// Empty server URLs and CA bytes that are not a PEM certificate are rejected.
func TestRenderKubeconfig(t *testing.T) {
	const (
		cpIP   = "192.168.124.10"
		cpName = "cp1"
		user   = "system:node:worker1"
		server = "https://192.168.124.10:6443"
	)

	pk, err := pki.GenerateClusterPKI(cpIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	certPEM, keyPEM, err := pki.GenerateKubeletCert(pk, "worker1")
	if err != nil {
		t.Fatalf("GenerateKubeletCert error: %v", err)
	}

	got, err := pki.RenderKubeconfig(pk.CA, server, user, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("RenderKubeconfig error: %v", err)
	}

	var cfg struct {
		Clusters []struct {
			Cluster struct {
				Server                   string `yaml:"server"`
				CertificateAuthorityData string `yaml:"certificate-authority-data"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
		Users []struct {
			Name string `yaml:"name"`
			User struct {
				ClientCertificateData string `yaml:"client-certificate-data"`
				ClientKeyData         string `yaml:"client-key-data"`
			} `yaml:"user"`
		} `yaml:"users"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("rendered kubeconfig does not parse as YAML: %v\n%s", err, got)
	}

	t.Run("server URL", func(t *testing.T) {
		if len(cfg.Clusters) == 0 {
			t.Fatalf("kubeconfig has no clusters: %s", got)
		}
		if cfg.Clusters[0].Cluster.Server != server {
			t.Errorf("kubeconfig server = %q, want %q", cfg.Clusters[0].Cluster.Server, server)
		}
	})

	t.Run("CA material", func(t *testing.T) {
		if len(cfg.Clusters) == 0 {
			t.Fatalf("kubeconfig has no clusters: %s", got)
		}
		decoded, err := base64.StdEncoding.DecodeString(cfg.Clusters[0].Cluster.CertificateAuthorityData)
		if err != nil {
			t.Fatalf("certificate-authority-data is not base64: %v", err)
		}
		if !bytes.Equal(decoded, pk.CA) {
			t.Errorf("certificate-authority-data does not round-trip to the CA PEM")
		}
	})

	t.Run("client certificate material", func(t *testing.T) {
		if len(cfg.Users) == 0 {
			t.Fatalf("kubeconfig has no users: %s", got)
		}
		if cfg.Users[0].Name != user {
			t.Errorf("kubeconfig user name = %q, want %q", cfg.Users[0].Name, user)
		}
		decoded, err := base64.StdEncoding.DecodeString(cfg.Users[0].User.ClientCertificateData)
		if err != nil {
			t.Fatalf("client-certificate-data is not base64: %v", err)
		}
		if !bytes.Equal(decoded, certPEM) {
			t.Errorf("client-certificate-data does not round-trip to the client certificate PEM")
		}
	})

	t.Run("client key material", func(t *testing.T) {
		if len(cfg.Users) == 0 {
			t.Fatalf("kubeconfig has no users: %s", got)
		}
		decoded, err := base64.StdEncoding.DecodeString(cfg.Users[0].User.ClientKeyData)
		if err != nil {
			t.Fatalf("client-key-data is not base64: %v", err)
		}
		if !bytes.Equal(decoded, keyPEM) {
			t.Errorf("client-key-data does not round-trip to the client key PEM")
		}
	})

	t.Run("deterministic output for fixed inputs", func(t *testing.T) {
		again, err := pki.RenderKubeconfig(pk.CA, server, user, certPEM, keyPEM)
		if err != nil {
			t.Fatalf("RenderKubeconfig (second call) error: %v", err)
		}
		if !bytes.Equal(got, again) {
			t.Errorf("RenderKubeconfig output differs between calls with identical inputs")
		}
	})

	t.Run("rejects an empty server URL", func(t *testing.T) {
		if _, err := pki.RenderKubeconfig(pk.CA, "", user, certPEM, keyPEM); err == nil {
			t.Error("RenderKubeconfig with an empty server URL succeeded, want an error")
		}
	})

	t.Run("rejects CA bytes that are not a PEM certificate", func(t *testing.T) {
		if _, err := pki.RenderKubeconfig([]byte("not a certificate"), server, user, certPEM, keyPEM); err == nil {
			t.Error("RenderKubeconfig with invalid CA bytes succeeded, want an error")
		}
	})
}

// TASK-011 VC-06 REQ-006 — endpoint + PKI: SANs contain reserved internal IP and 127.0.0.1.
//
// Grill-me edge cases: reserved IP is not hardcoded .20 (dynamic AllocateIP),
// loopback must be IP SAN not DNS SAN, both IPs must coexist (not replaced).
// RED: current impl only puts cpIP into SANs, so the loopback assertion fails.
func TestGenerateClusterPKISANsContainReservedIPAndLoopback(t *testing.T) {
	// Use a non-default reserved IP to prove the test does not hardcode 192.168.124.20.
	const (
		reservedIP = "192.168.124.77"
		cpName     = "cp-0"
	)
	loopback := net.ParseIP("127.0.0.1")
	reserved := net.ParseIP(reservedIP)
	if loopback == nil || reserved == nil {
		t.Fatalf("test IPs do not parse")
	}
	pk, err := pki.GenerateClusterPKI(reservedIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	cert := decodeCert(t, pk.APIServer)

	hasReserved := false
	hasLoopback := false
	for _, ip := range cert.IPAddresses {
		if ip.Equal(reserved) {
			hasReserved = true
		}
		if ip.Equal(loopback) {
			hasLoopback = true
		}
	}
	if !hasReserved {
		t.Errorf("apiserver cert IP SANs %v do not contain reserved internal IP %s", cert.IPAddresses, reservedIP)
	}
	if !hasLoopback {
		t.Errorf("apiserver cert IP SANs %v do not contain 127.0.0.1 (REQ-006 VC-06)", cert.IPAddresses)
	}
	// DNS SAN must still contain cpName; the IP SAN change must not drop it.
	foundDNS := slices.Contains(cert.DNSNames, cpName)
	if !foundDNS {
		t.Errorf("apiserver cert DNS SANs %v do not contain %q after loopback addition", cert.DNSNames, cpName)
	}
}

// TestGenerateClusterPKISANsWithDynamicReservedIP proves the reserved IP is
// not hardcoded to 192.168.124.20: a different pool address still appears
// alongside 127.0.0.1.
func TestGenerateClusterPKISANsWithDynamicReservedIP(t *testing.T) {
	cases := []string{"192.168.124.50", "192.168.124.90", "10.0.0.10"}
	loopback := net.ParseIP("127.0.0.1")
	for _, reservedIP := range cases {
		t.Run(reservedIP, func(t *testing.T) {
			pk, err := pki.GenerateClusterPKI(reservedIP, "cp-0")
			if err != nil {
				t.Fatalf("GenerateClusterPKI(%q) error: %v", reservedIP, err)
			}
			cert := decodeCert(t, pk.APIServer)
			reserved := net.ParseIP(reservedIP)
			hasReserved, hasLoopback := false, false
			for _, ip := range cert.IPAddresses {
				if ip.Equal(reserved) {
					hasReserved = true
				}
				if ip.Equal(loopback) {
					hasLoopback = true
				}
			}
			if !hasReserved {
				t.Errorf("IP SANs %v missing reserved IP %s", cert.IPAddresses, reservedIP)
			}
			if !hasLoopback {
				t.Errorf("IP SANs %v missing 127.0.0.1 for reserved IP %s", cert.IPAddresses, reservedIP)
			}
		})
	}
}

// TestGenerateClusterPKILoopbackIsIPSANNotDNSSAN ensures 127.0.0.1 is an IP SAN,
// not accidentally placed in DNSNames.
func TestGenerateClusterPKILoopbackIsIPSANNotDNSSAN(t *testing.T) {
	pk, err := pki.GenerateClusterPKI("192.168.124.77", "cp-0")
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	cert := decodeCert(t, pk.APIServer)
	for _, dns := range cert.DNSNames {
		if dns == "127.0.0.1" {
			t.Errorf("127.0.0.1 appears as DNS SAN %v, want IP SAN", cert.DNSNames)
		}
	}
	loopback := net.ParseIP("127.0.0.1")
	for _, ip := range cert.IPAddresses {
		if ip.Equal(loopback) {
			return
		}
	}
	t.Errorf("127.0.0.1 not found as IP SAN: %v", cert.IPAddresses)
}

// TestRenderKubeconfigLoopbackServerURL pins that the rendered kubeconfig
// server URL must be https://127.0.0.1:6443 per REQ-006/VC-06, not the
// reserved internal IP. This test drives RenderKubeconfig directly with the
// loopback URL; the controller test below drives it via reconcile.
func TestRenderKubeconfigLoopbackServerURL(t *testing.T) {
	const (
		reservedIP  = "192.168.124.77"
		cpName      = "cp-0"
		loopbackURL = "https://127.0.0.1:6443"
		user        = "system:node:cp-0"
	)
	pk, err := pki.GenerateClusterPKI(reservedIP, cpName)
	if err != nil {
		t.Fatalf("GenerateClusterPKI error: %v", err)
	}
	certPEM, keyPEM, err := pki.GenerateKubeletCert(pk, cpName)
	if err != nil {
		t.Fatalf("GenerateKubeletCert error: %v", err)
	}
	got, err := pki.RenderKubeconfig(pk.CA, loopbackURL, user, certPEM, keyPEM)
	if err != nil {
		t.Fatalf("RenderKubeconfig error: %v", err)
	}
	var cfg struct {
		Clusters []struct {
			Cluster struct {
				Server string `yaml:"server"`
			} `yaml:"cluster"`
		} `yaml:"clusters"`
	}
	if err := yaml.Unmarshal(got, &cfg); err != nil {
		t.Fatalf("rendered kubeconfig does not parse as YAML: %v\n%s", err, got)
	}
	if len(cfg.Clusters) == 0 || cfg.Clusters[0].Cluster.Server != loopbackURL {
		t.Errorf("kubeconfig server = %q, want %q", cfg.Clusters[0].Cluster.Server, loopbackURL)
	}
	if cfg.Clusters[0].Cluster.Server != "https://127.0.0.1:6443" {
		t.Errorf("kubeconfig server must be exactly https://127.0.0.1:6443, got %q", cfg.Clusters[0].Cluster.Server)
	}
}
