package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agentgrpc"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
)

func main() {
	var (
		listenAddress        string
		certificateDirectory string
		clientCAFile         string
		inventoryPath        string
		nodeID               string
	)

	flag.StringVar(&listenAddress, "listen", ":9444", "gRPC listen address")
	flag.StringVar(&certificateDirectory, "tls-cert-dir", "/tls/server", "directory containing tls.crt and tls.key")
	flag.StringVar(&clientCAFile, "client-ca", "/tls/ca/ca.crt", "PEM client CA certificate")
	flag.StringVar(&nodeID, "node-id", "", "Kubernetes node identity")
	flag.StringVar(&inventoryPath, "inventory", "/state/inventory.db", "SQLite inventory path")
	flag.Parse()

	store, err := inventory.Open(inventoryPath)
	if err != nil {
		fatalf("open inventory: %v", err)
	}
	defer func() { _ = store.Close() }()

	_ = store

	certificate, err := tls.LoadX509KeyPair(
		filepath.Join(certificateDirectory, "tls.crt"),
		filepath.Join(certificateDirectory, "tls.key"),
	)
	if err != nil {
		fatalf("load server certificate: %v", err)
	}

	clientCA, err := os.ReadFile(clientCAFile)
	if err != nil {
		fatalf("read client CA: %v", err)
	}

	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCA) {
		fatalf("parse client CA: no certificate found")
	}

	listener, err := net.Listen("tcp", listenAddress)
	if err != nil {
		fatalf("listen: %v", err)
	}

	server := grpc.NewServer(
		grpc.Creds(credentials.NewTLS(agentgrpc.ServerTLSConfig(certificate, clientCAs))),
		grpc.UnaryInterceptor(agentgrpc.ManagerAuthInterceptor),
	)
	agentv1.RegisterHostAgentServer(server, &agentgrpc.Server{
		Host:         &hostagent.Fake{},
		Capabilities: hostagent.Capabilities{NodeID: nodeID},
	})

	if err := server.Serve(listener); err != nil {
		fatalf("serve gRPC: %v", err)
	}
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hypervisor-agent: "+format+"\n", args...)
	os.Exit(1)
}
