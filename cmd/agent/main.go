package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"

	agentv1 "github.com/moeryomenko/cluster-api-hypervisor/api/agent/v1"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/artifact"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/executor"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/inventory"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agent/systemd"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/agentgrpc"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/hostagent"
	"github.com/moeryomenko/cluster-api-hypervisor/internal/k8netd"
)

func main() {
	var (
		listenAddress        string
		certificateDirectory string
		clientCAFile         string
		inventoryPath        string
		nodeID               string
		userDBusAddress      string
		kvmPath              string
		k8netdSocket         string
		cloudHypervisorPath  string
		artifactRoot         string
		unitDir              string
	)

	flag.StringVar(&listenAddress, "listen", ":9444", "gRPC listen address")
	flag.StringVar(&certificateDirectory, "tls-cert-dir", "/tls/server", "directory containing tls.crt and tls.key")
	flag.StringVar(&clientCAFile, "client-ca", "/tls/ca/ca.crt", "PEM client CA certificate")
	flag.StringVar(&nodeID, "node-id", "", "Kubernetes node identity")
	flag.StringVar(&inventoryPath, "inventory", "/state/inventory.db", "SQLite inventory path")
	flag.StringVar(&userDBusAddress, "user-dbus-address", "unix:path=/run/user/1000/bus", "host user D-Bus address")
	flag.StringVar(&kvmPath, "kvm", "/dev/kvm", "KVM device path")
	flag.StringVar(&k8netdSocket, "k8netd-socket", "/run/user/1000/k8snet/control.sock", "k8netd control socket")
	flag.StringVar(&artifactRoot, "artifact-root", "/host-state/vms", "owned VM artifact root")
	flag.StringVar(&unitDir, "unit-dir", "/home/eryoma/.config/systemd/user", "persistent user systemd unit directory")
	flag.StringVar(
		&cloudHypervisorPath,
		"cloud-hypervisor",
		"/usr/bin/cloud-hypervisor",
		"Cloud Hypervisor executable path",
	)
	flag.Parse()

	store, err := inventory.Open(inventoryPath)
	if err != nil {
		fatalf("open inventory: %v", err)
	}
	defer func() { _ = store.Close() }()

	userSystemd, err := systemd.ConnectUser(userDBusAddress)
	if err != nil {
		fatalf("connect user systemd: %v", err)
	}
	defer func() { _ = userSystemd.Close() }()

	host := &executor.Executor{
		Store:   store,
		Systemd: userSystemd,
		Artifacts: artifact.Builder{
			Root:    artifactRoot,
			QemuImg: "qemu-img",
			Mkdosfs: "mkdosfs",
			Mcopy:   "mcopy",
			Run:     commandRunner{},
		},
		Network:         k8netd.NewClient(k8netdSocket),
		NodeID:          nodeID,
		CloudHypervisor: cloudHypervisorPath,
		K8netdSocket:    k8netdSocket,
		KVMPath:         kvmPath,
		UnitDir:         unitDir,
	}

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
		Host:         host,
		Capabilities: hostagent.Capabilities{NodeID: nodeID},
	})

	if err := server.Serve(listener); err != nil {
		fatalf("serve gRPC: %v", err)
	}
}

type commandRunner struct{}

func (commandRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).CombinedOutput()
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "hypervisor-agent: "+format+"\n", args...)
	os.Exit(1)
}
