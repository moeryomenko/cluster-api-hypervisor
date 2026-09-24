package agentgrpc

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"net/url"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

const ManagerSPIFFEURI = "spiffe://k8labs/provider/manager"

func ServerTLSConfig(serverCertificate tls.Certificate, clientCAs *x509.CertPool) *tls.Config {
	return &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{serverCertificate},
		ClientAuth:   tls.RequireAndVerifyClientCert,
		ClientCAs:    clientCAs,
	}
}

func RequireManagerIdentity(ctx context.Context) error {
	peerInfo, ok := peer.FromContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing client peer")
	}

	tlsInfo, ok := peerInfo.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.VerifiedChains) == 0 {
		return status.Error(codes.Unauthenticated, "verified client certificate is required")
	}

	for _, uri := range tlsInfo.State.VerifiedChains[0][0].URIs {
		if uri.String() == ManagerSPIFFEURI {
			return nil
		}
	}

	return status.Error(codes.PermissionDenied, "client certificate does not have the manager SPIFFE URI")
}

func ManagerAuthInterceptor(
	ctx context.Context,
	request any,
	info *grpc.UnaryServerInfo,
	handler grpc.UnaryHandler,
) (any, error) {
	if info.FullMethod == "/k8labs.agent.v1.HostAgent/Health" {
		return handler(ctx, request)
	}

	if err := RequireManagerIdentity(ctx); err != nil {
		return nil, err
	}

	return handler(ctx, request)
}

func ParseSPIFFEURI(value string) (*url.URL, error) {
	uri, err := url.Parse(value)
	if err != nil || uri.Scheme != "spiffe" || uri.Host == "" {
		return nil, fmt.Errorf("invalid SPIFFE URI %q", value)
	}

	return uri, nil
}
