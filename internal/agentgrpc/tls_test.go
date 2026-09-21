package agentgrpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"math/big"
	"net"
	"net/url"
	"testing"
	"time"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

func certificateAuthority(t *testing.T) (*x509.Certificate, *ecdsa.PrivateKey, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test-ca"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature}
	der, err := x509.CreateCertificate(rand.Reader, certificate, certificate, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return parsed, key, pool
}

func leafCertificate(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, dns string, uri string, client bool) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	certificate := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: "leaf"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature}
	if dns != "" {
		certificate.DNSNames = []string{dns}
	}
	if uri != "" {
		parsed, err := url.Parse(uri)
		if err != nil {
			t.Fatal(err)
		}
		certificate.URIs = []*url.URL{parsed}
	}
	if client {
		certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}
	} else {
		certificate.ExtKeyUsage = []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}
	}
	der, err := x509.CreateCertificate(rand.Reader, certificate, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestMTLSAuthorizesOnlyManagerSPIFFEURI(t *testing.T) {
	ca, caKey, pool := certificateAuthority(t)
	serverCertificate := leafCertificate(t, ca, caKey, "agent.test", "", false)
	managerCertificate := leafCertificate(t, ca, caKey, "", ManagerSPIFFEURI, true)
	wrongCertificate := leafCertificate(t, ca, caKey, "", "spiffe://k8labs/provider/other", true)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(ServerTLSConfig(serverCertificate, pool))), grpc.UnaryInterceptor(ManagerAuthInterceptor))
	agentv1.RegisterHostAgentServer(server, &Server{Host: &recordingAgent{}})
	defer server.Stop()
	go server.Serve(listener)

	manager, err := Dial(context.Background(), listener.Addr().String(), "agent.test", managerCertificate, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.Close()
	mutation := hostagent.Mutation{ProtocolMajor: hostagent.ProtocolMajor, Owner: hostagent.Owner{InstallationID: "install-a", NodeID: "node-a", UID: "machine-a"}, Generation: 1, IdempotencyKey: "machine-a-1"}
	if _, err := manager.EnsureVM(context.Background(), mutation, hostagent.VMDesired{UID: "machine-a", Name: "vm-a"}); err != nil {
		t.Fatalf("manager identity rejected: %v", err)
	}

	wrong, err := Dial(context.Background(), listener.Addr().String(), "agent.test", wrongCertificate, pool)
	if err != nil {
		t.Fatal(err)
	}
	defer wrong.Close()
	_, err = wrong.EnsureVM(context.Background(), mutation, hostagent.VMDesired{UID: "machine-a", Name: "vm-a"})
	if status.Code(err) != codes.PermissionDenied {
		t.Fatalf("wrong URI error = %v, want PermissionDenied", err)
	}
}
