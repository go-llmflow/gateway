package ha

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
)

// MutualTLS returns the server and client configurations of the internal
// channel under mutual TLS 1.3: each replica presents its certificate and
// key, and accepts only a peer whose certificate the CA signed. Replicas
// are dialled by IP (the owner's address comes from the lease), so the
// client checks that the server's certificate carries serverName instead
// of the IP. The HMAC signatures stay: TLS adds confidentiality, the
// signatures keep binding each answer to its request and refusing replays.
func MutualTLS(certPEM, keyPEM, caPEM []byte, serverName string) (server, client *tls.Config, err error) {
	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		return nil, nil, fmt.Errorf("ha tls: certificate and key: %w", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, nil, errors.New("ha tls: the CA file holds no PEM certificate")
	}
	if serverName == "" {
		return nil, nil, errors.New("ha tls: empty server name")
	}
	server = &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		ClientAuth: tls.RequireAndVerifyClientCert, ClientCAs: pool,
	}
	client = &tls.Config{
		MinVersion: tls.VersionTLS13, Certificates: []tls.Certificate{cert},
		RootCAs: pool, ServerName: serverName,
	}
	return server, client, nil
}
