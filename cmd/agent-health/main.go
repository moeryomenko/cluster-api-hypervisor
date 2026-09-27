package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/moeryomenko/cluster-api-hypervisor/internal/agentgrpc"
)

func main() {
	var address, serverName, certificateDir, caFile string
	flag.StringVar(&address, "address", "127.0.0.1:9444", "HostAgent address")
	flag.StringVar(&serverName, "server-name", "hypervisor-agent", "HostAgent TLS server name")
	flag.StringVar(&certificateDir, "client-cert-dir", "", "directory containing tls.crt and tls.key")
	flag.StringVar(&caFile, "ca", "", "HostAgent CA certificate")
	flag.Parse()

	certificate, err := tls.LoadX509KeyPair(
		filepath.Join(certificateDir, "tls.crt"),
		filepath.Join(certificateDir, "tls.key"),
	)
	if err != nil {
		fatal(err)
	}

	caPEM, err := os.ReadFile(caFile)
	if err != nil {
		fatal(err)
	}

	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		fatal(fmt.Errorf("parse HostAgent CA: no certificate found"))
	}

	client, err := agentgrpc.Dial(context.Background(), address, serverName, certificate, roots)
	if err != nil {
		fatal(err)
	}
	defer func() { _ = client.Close() }()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	if _, err := client.Health(ctx); err != nil {
		fatal(err)
	}
}

func fatal(err error) {
	fmt.Fprintln(os.Stderr, err)
	os.Exit(1)
}
