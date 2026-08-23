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

// Package pki generates the cluster-scoped PKI and the per-machine kubelet
// artifacts for one cluster, and renders kubeconfigs from PEM material. The
// cluster PKI is generated once per cluster: a self-signed cluster CA, a
// separate self-signed front-proxy CA, an apiserver certificate whose SANs
// include the control-plane static IP and node name, a front-proxy client
// certificate signed by the front-proxy CA, and a service-account keypair
// signed by the cluster CA. Per-machine artifacts are the kubelet certificate
// and key signed by the cluster CA. Kubeconfigs are rendered from PEM material
// with a fixed server URL. Everything is hand-rolled: no kubeadm.
package pki

import (
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"time"

	"gopkg.in/yaml.v3"
)

// ClusterPKI holds the PEM-encoded certificate and key material for one
// cluster: the CA/CAKey signing authority, a distinct front-proxy CA/CAKey,
// the apiserver serving certificate, the front-proxy client certificate signed
// by the front-proxy CA, and the service-account keypair. Every field is a PEM
// block: certificates are CERTIFICATE blocks and private keys are in a
// standard PEM encoding.
type ClusterPKI struct {
	CA                []byte
	CAKey             []byte
	FrontProxyCA      []byte
	FrontProxyCAKey   []byte
	APIServer         []byte
	APIServerKey      []byte
	FrontProxy        []byte
	FrontProxyKey     []byte
	ServiceAccount    []byte
	ServiceAccountKey []byte
}

// kubeconfig is the YAML document RenderKubeconfig produces.
type kubeconfig struct {
	APIVersion     string                   `yaml:"apiVersion"`
	Kind           string                   `yaml:"kind"`
	Clusters       []kubeconfigClusterEntry `yaml:"clusters"`
	Contexts       []kubeconfigContextEntry `yaml:"contexts"`
	CurrentContext string                   `yaml:"current-context"`
	Users          []kubeconfigUserEntry    `yaml:"users"`
}

// kubeconfigClusterEntry is one entry of the kubeconfig clusters list.
type kubeconfigClusterEntry struct {
	Name    string                `yaml:"name"`
	Cluster kubeconfigClusterData `yaml:"cluster"`
}

// kubeconfigClusterData is the cluster payload of a kubeconfig clusters entry.
type kubeconfigClusterData struct {
	Server                   string `yaml:"server"`
	CertificateAuthorityData string `yaml:"certificate-authority-data"`
}

// kubeconfigContextEntry is one entry of the kubeconfig contexts list.
type kubeconfigContextEntry struct {
	Name    string                `yaml:"name"`
	Context kubeconfigContextData `yaml:"context"`
}

// kubeconfigContextData is the context payload of a kubeconfig contexts entry.
type kubeconfigContextData struct {
	Cluster string `yaml:"cluster"`
	User    string `yaml:"user"`
}

// kubeconfigUserEntry is one entry of the kubeconfig users list.
type kubeconfigUserEntry struct {
	Name string             `yaml:"name"`
	User kubeconfigUserData `yaml:"user"`
}

// kubeconfigUserData is the user payload of a kubeconfig users entry.
type kubeconfigUserData struct {
	ClientCertificateData string `yaml:"client-certificate-data"`
	ClientKeyData         string `yaml:"client-key-data"`
}

const (
	// caValidity is how long the generated cluster CA certificate is valid.
	caValidity = 10 * 365 * 24 * time.Hour
	// leafValidity is how long every CA-signed certificate is valid.
	leafValidity = 365 * 24 * time.Hour
	// rsaKeyBits is the RSA key size for every generated key.
	rsaKeyBits = 2048
	// clusterName is the name of the single cluster entry in rendered
	// kubeconfigs.
	clusterName = "kubernetes"
)

// GenerateClusterPKI generates the cluster-scoped PKI for one cluster and
// returns it as PEM material. cpIP is the control-plane node's reserved
// internal IP (from AllocateIP, not hardcoded) and cpName its node name; both
// must be non-empty and cpIP must be a valid IP literal. The cluster CA and
// the front-proxy CA are self-signed; the apiserver certificate carries both
// cpIP and 127.0.0.1 as IP SANs and cpName as a DNS SAN; the apiserver and
// service-account certificates are signed by the cluster CA; and the
// front-proxy client certificate is signed by the distinct front-proxy CA.
func GenerateClusterPKI(cpIP, cpName string) (ClusterPKI, error) {
	ip := net.ParseIP(cpIP)
	if ip == nil {
		return ClusterPKI{}, fmt.Errorf("control-plane IP %q is not a valid IP literal", cpIP)
	}
	loopback := net.ParseIP("127.0.0.1")
	if loopback == nil {
		return ClusterPKI{}, fmt.Errorf("loopback IP is not a valid IP literal")
	}
	if cpName == "" {
		return ClusterPKI{}, fmt.Errorf("control-plane name must not be empty")
	}

	caKey, err := newKey()
	if err != nil {
		return ClusterPKI{}, err
	}
	caCert, caPEM, err := newCACertificate("cluster-ca", caKey)
	if err != nil {
		return ClusterPKI{}, err
	}
	frontProxyCAKey, err := newKey()
	if err != nil {
		return ClusterPKI{}, err
	}
	frontProxyCACert, frontProxyCAPEM, err := newCACertificate("front-proxy-ca", frontProxyCAKey)
	if err != nil {
		return ClusterPKI{}, err
	}

	apiserverCert, apiserverKey, err := newLeafKeyAndCert(
		"kube-apiserver",
		x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		[]string{cpName},
		[]net.IP{ip, loopback},
		caCert, caKey,
	)
	if err != nil {
		return ClusterPKI{}, err
	}
	frontProxyCert, frontProxyKey, err := newLeafKeyAndCert(
		"front-proxy-client",
		x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		nil, nil,
		frontProxyCACert, frontProxyCAKey,
	)
	if err != nil {
		return ClusterPKI{}, err
	}
	serviceAccountCert, serviceAccountKey, err := newLeafKeyAndCert(
		"service-account",
		x509.KeyUsageDigitalSignature,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		nil, nil,
		caCert, caKey,
	)
	if err != nil {
		return ClusterPKI{}, err
	}

	return ClusterPKI{
		CA:                caPEM,
		CAKey:             pemKey(caKey),
		FrontProxyCA:      frontProxyCAPEM,
		FrontProxyCAKey:   pemKey(frontProxyCAKey),
		APIServer:         apiserverCert,
		APIServerKey:      apiserverKey,
		FrontProxy:        frontProxyCert,
		FrontProxyKey:     frontProxyKey,
		ServiceAccount:    serviceAccountCert,
		ServiceAccountKey: serviceAccountKey,
	}, nil
}

// GenerateKubeletCert generates one node's kubelet certificate and key, signed
// by the cluster CA. The certificate common name is exactly nodeName; nodeName
// must be non-empty and the PKI must hold CA material. The returned key and
// certificate belong to the same keypair.
func GenerateKubeletCert(clusterPKI ClusterPKI, nodeName string) (certPEM, keyPEM []byte, err error) {
	if nodeName == "" {
		return nil, nil, fmt.Errorf("node name must not be empty")
	}
	if len(clusterPKI.CA) == 0 || len(clusterPKI.CAKey) == 0 {
		return nil, nil, fmt.Errorf("cluster PKI has no CA material")
	}

	caCert, err := parseCACertificate(clusterPKI.CA)
	if err != nil {
		return nil, nil, err
	}
	caKey, err := parseCAKey(clusterPKI.CAKey)
	if err != nil {
		return nil, nil, err
	}

	return newLeafKeyAndCert(
		nodeName,
		x509.KeyUsageDigitalSignature|x509.KeyUsageKeyEncipherment,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth, x509.ExtKeyUsageServerAuth},
		nil, nil,
		caCert, caKey,
	)
}

// RenderKubeconfig renders a kubeconfig YAML document from PEM material. The
// cluster server is exactly serverURL, the certificate-authority-data is the
// base64 encoding of caPEM, the user is named exactly user, and the client
// certificate and key appear as base64 client-certificate-data and
// client-key-data. serverURL must be non-empty and caPEM must be a parseable
// PEM certificate. Given identical inputs the output is byte-identical.
func RenderKubeconfig(caPEM []byte, serverURL, user string, clientCert, clientKey []byte) ([]byte, error) {
	if serverURL == "" {
		return nil, fmt.Errorf("server URL must not be empty")
	}
	if _, err := parseCACertificate(caPEM); err != nil {
		return nil, err
	}

	cfg := kubeconfig{
		APIVersion: "v1",
		Kind:       "Config",
		Clusters: []kubeconfigClusterEntry{{
			Name: clusterName,
			Cluster: kubeconfigClusterData{
				Server:                   serverURL,
				CertificateAuthorityData: base64.StdEncoding.EncodeToString(caPEM),
			},
		}},
		Contexts: []kubeconfigContextEntry{{
			Name: clusterName,
			Context: kubeconfigContextData{
				Cluster: clusterName,
				User:    user,
			},
		}},
		CurrentContext: clusterName,
		Users: []kubeconfigUserEntry{{
			Name: user,
			User: kubeconfigUserData{
				ClientCertificateData: base64.StdEncoding.EncodeToString(clientCert),
				ClientKeyData:         base64.StdEncoding.EncodeToString(clientKey),
			},
		}},
	}

	out, err := yaml.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("marshaling kubeconfig: %w", err)
	}
	return out, nil
}

// newCACertificate creates a self-signed CA certificate with the given common
// name from key and returns the parsed certificate and its PEM encoding.
func newCACertificate(cn string, key *rsa.PrivateKey) (*x509.Certificate, []byte, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	notBefore, notAfter := validityWindow(caValidity)
	template := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("creating CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parsing generated CA certificate: %w", err)
	}
	return cert, pemCert(der), nil
}

// newLeafKeyAndCert generates a fresh key and a CA-signed leaf certificate
// with the given parameters, returning both as PEM.
func newLeafKeyAndCert(
	cn string,
	keyUsage x509.KeyUsage,
	extKeyUsage []x509.ExtKeyUsage,
	dnsNames []string,
	ipAddresses []net.IP,
	caCert *x509.Certificate,
	caKey crypto.Signer,
) (certPEM, keyPEM []byte, err error) {
	key, err := newKey()
	if err != nil {
		return nil, nil, err
	}
	template, err := newLeafTemplate(cn, keyUsage, extKeyUsage, dnsNames, ipAddresses)
	if err != nil {
		return nil, nil, err
	}
	cert, err := signCert(template, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("signing %s certificate: %w", cn, err)
	}
	return cert, pemKey(key), nil
}

// newLeafTemplate builds the certificate template for a CA-signed leaf with
// the given common name, key usage, extended key usage, DNS SANs, and IP
// SANs. Every leaf shares the same validity window and gets a fresh random
// serial number.
func newLeafTemplate(
	cn string,
	keyUsage x509.KeyUsage,
	extKeyUsage []x509.ExtKeyUsage,
	dnsNames []string,
	ipAddresses []net.IP,
) (*x509.Certificate, error) {
	serial, err := newSerial()
	if err != nil {
		return nil, err
	}
	notBefore, notAfter := validityWindow(leafValidity)
	return &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn},
		NotBefore:             notBefore,
		NotAfter:              notAfter,
		KeyUsage:              keyUsage,
		ExtKeyUsage:           extKeyUsage,
		BasicConstraintsValid: true,
		DNSNames:              dnsNames,
		IPAddresses:           ipAddresses,
	}, nil
}

// newKey generates a fresh 2048-bit RSA key.
func newKey() (*rsa.PrivateKey, error) {
	key, err := rsa.GenerateKey(rand.Reader, rsaKeyBits)
	if err != nil {
		return nil, fmt.Errorf("generating RSA key: %w", err)
	}
	return key, nil
}

// newSerial returns a fresh random positive serial number for a certificate.
func newSerial() (*big.Int, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return nil, fmt.Errorf("generating certificate serial number: %w", err)
	}
	if serial.Sign() == 0 {
		serial.SetInt64(1)
	}
	return serial, nil
}

// parseCACertificate decodes caPEM as a single PEM certificate.
func parseCACertificate(caPEM []byte) (*x509.Certificate, error) {
	block, _ := pem.Decode(caPEM)
	if block == nil || block.Type != "CERTIFICATE" {
		return nil, fmt.Errorf("CA bytes are not a PEM certificate")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing CA certificate: %w", err)
	}
	return cert, nil
}

// parseCAKey decodes caKeyPEM as a PKCS#1 RSA private key.
func parseCAKey(caKeyPEM []byte) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode(caKeyPEM)
	if block == nil {
		return nil, fmt.Errorf("CA key bytes are not a PEM private key")
	}
	key, err := x509.ParsePKCS1PrivateKey(block.Bytes)
	if err != nil {
		return nil, fmt.Errorf("parsing CA private key: %w", err)
	}
	return key, nil
}

// pemCert encodes a DER certificate as a PEM CERTIFICATE block.
func pemCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// pemKey encodes an RSA private key as a PKCS#1 PEM block.
func pemKey(key *rsa.PrivateKey) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
}

// signCert creates a certificate from template, signed by the CA certificate
// parent with the signer key, and returns its PEM encoding.
func signCert(template, parent *x509.Certificate, pub crypto.PublicKey, signer crypto.Signer) ([]byte, error) {
	der, err := x509.CreateCertificate(rand.Reader, template, parent, pub, signer)
	if err != nil {
		return nil, fmt.Errorf("signing certificate: %w", err)
	}
	return pemCert(der), nil
}

// validityWindow returns the NotBefore/NotAfter pair for a certificate valid
// for duration, started slightly in the past so a clock that lags a little
// still sees it as valid.
func validityWindow(duration time.Duration) (notBefore, notAfter time.Time) {
	now := time.Now()
	return now.Add(-time.Hour), now.Add(duration)
}
