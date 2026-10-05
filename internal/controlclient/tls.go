package controlclient

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"strings"

	"google.golang.org/grpc/credentials"
)

// TLSConfig describes the certificate material the Console uses to reach the
// Control AdminService. It deliberately mirrors the Gateway client half of
// internal/control.TLSConfig so both data-plane and control-plane clients read
// the same CONTROL_TLS_* variables:
//
//	CONTROL_TLS_CA_FILE                  CA that signed the control server cert
//	CONTROL_TLS_CLIENT_CERT_FILE         Console client certificate
//	CONTROL_TLS_CLIENT_KEY_FILE          Console client private key
//	CONTROL_TLS_SERVER_NAME              expected server name (SAN) of Control
//
// The Console never serves control-plane TLS, so CONTROL_TLS_CERT_FILE /
// CONTROL_TLS_KEY_FILE (the Control *server* certificate) are intentionally not
// read here.
type TLSConfig struct {
	CAFile         string
	ClientCertFile string
	ClientKeyFile  string
	ServerName     string
}

// enabled reports whether any control TLS material was configured. A completely
// empty configuration selects the explicit local-demo insecure transport.
func (c *TLSConfig) enabled() bool {
	if c == nil {
		return false
	}
	return strings.TrimSpace(c.CAFile) != "" ||
		strings.TrimSpace(c.ClientCertFile) != "" ||
		strings.TrimSpace(c.ClientKeyFile) != ""
}

// TLSConfigFromEnv reads the CONTROL_TLS_* variables for the Console client.
// lookup is injected so the wiring is testable without touching the process
// environment. It returns (nil, nil) when nothing is configured.
func TLSConfigFromEnv(lookup func(string) string) (*TLSConfig, error) {
	if lookup == nil {
		lookup = os.Getenv
	}
	cfg := &TLSConfig{
		CAFile:         strings.TrimSpace(lookup("CONTROL_TLS_CA_FILE")),
		ClientCertFile: strings.TrimSpace(lookup("CONTROL_TLS_CLIENT_CERT_FILE")),
		ClientKeyFile:  strings.TrimSpace(lookup("CONTROL_TLS_CLIENT_KEY_FILE")),
		ServerName:     strings.TrimSpace(lookup("CONTROL_TLS_SERVER_NAME")),
	}
	if !cfg.enabled() {
		return nil, nil
	}
	// Reject a half-configured channel instead of silently falling back to
	// plaintext: an operator who set CONTROL_TLS_CA_FILE expects mTLS.
	if cfg.CAFile == "" {
		return nil, fmt.Errorf("CONTROL_TLS_CA_FILE is required when any CONTROL_TLS_* variable is set")
	}
	if (cfg.ClientCertFile == "") != (cfg.ClientKeyFile == "") {
		return nil, fmt.Errorf("CONTROL_TLS_CLIENT_CERT_FILE and CONTROL_TLS_CLIENT_KEY_FILE must be set together")
	}
	return cfg, nil
}

// transportCredentials builds the gRPC transport credentials. A nil/empty
// configuration returns (nil, nil) and the caller decides whether insecure
// transport is acceptable.
func transportCredentials(cfg *TLSConfig) (credentials.TransportCredentials, error) {
	if !cfg.enabled() {
		return nil, nil
	}
	pool, err := loadCertPool(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load control server CA: %w", err)
	}
	clientTLS := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: cfg.ServerName,
	}
	cert, err := loadClientCertificate(cfg)
	if err != nil {
		return nil, err
	}
	if cert != nil {
		clientTLS.Certificates = []tls.Certificate{*cert}
	}
	return credentials.NewTLS(clientTLS), nil
}

func loadClientCertificate(cfg *TLSConfig) (*tls.Certificate, error) {
	if cfg == nil || (cfg.ClientCertFile == "" && cfg.ClientKeyFile == "") {
		return nil, nil
	}
	if cfg.ClientCertFile == "" || cfg.ClientKeyFile == "" {
		return nil, fmt.Errorf("mTLS client requires both CONTROL_TLS_CLIENT_CERT_FILE and CONTROL_TLS_CLIENT_KEY_FILE")
	}
	cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
	if err != nil {
		return nil, fmt.Errorf("load control client certificate: %w", err)
	}
	return &cert, nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	if strings.TrimSpace(path) == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found in %s", path)
	}
	return pool, nil
}

// verifyClientIdentity fails fast when the certificate the Console is about to
// present is not the identity Control has been configured to trust
// (CONTROL_CONSOLE_SERVICE_NAME on the Control side). Without this check a
// deployment that accidentally mounts the gateway client certificate only fails
// at the first RPC, with a PERMISSION_DENIED that looks like a policy problem.
func verifyClientIdentity(cert *tls.Certificate, serviceName string) error {
	name := strings.TrimSpace(serviceName)
	if name == "" || cert == nil || len(cert.Certificate) == 0 {
		return nil
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return fmt.Errorf("parse control client certificate: %w", err)
	}
	if certificateMatches(leaf, name) {
		return nil
	}
	return fmt.Errorf("control client certificate is not the configured console service %q (CN=%q)", name, leaf.Subject.CommonName)
}

// certificateMatches mirrors Control's adminIdentityMatches: the configured
// service name may be the CommonName or any DNS/email/URI SAN.
func certificateMatches(certificate *x509.Certificate, name string) bool {
	if certificate == nil || strings.TrimSpace(name) == "" {
		return false
	}
	if certificate.Subject.CommonName == name {
		return true
	}
	for _, dns := range certificate.DNSNames {
		if dns == name {
			return true
		}
	}
	for _, email := range certificate.EmailAddresses {
		if email == name {
			return true
		}
	}
	for _, uri := range certificate.URIs {
		if uri.String() == name {
			return true
		}
	}
	return false
}
