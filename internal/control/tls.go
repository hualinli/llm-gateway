package control

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"

	"google.golang.org/grpc/credentials"
)

// TLSConfig describes the certificate material used by control-plane gRPC.
// ServerName is used only by clients when verifying the control endpoint.
type TLSConfig struct {
	CertFile          string
	KeyFile           string
	CAFile            string
	ClientCertFile    string
	ClientKeyFile     string
	ServerName        string
	RequireClientCert bool
}

func (c *TLSConfig) enabled() bool {
	return c != nil && (c.CertFile != "" || c.KeyFile != "" || c.CAFile != "" || c.ClientCertFile != "" || c.ClientKeyFile != "")
}

func serverCredentials(cfg *TLSConfig) (credentials.TransportCredentials, error) {
	if cfg == nil || !cfg.enabled() {
		return nil, nil
	}
	if cfg.CertFile == "" || cfg.KeyFile == "" {
		return nil, fmt.Errorf("control TLS requires both cert and key files")
	}
	cert, err := tls.LoadX509KeyPair(cfg.CertFile, cfg.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load control server certificate: %w", err)
	}
	pool, err := loadCertPool(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load control client CA: %w", err)
	}
	serverTLS := &tls.Config{
		MinVersion:   tls.VersionTLS13,
		Certificates: []tls.Certificate{cert},
	}
	if cfg.RequireClientCert {
		if pool == nil {
			return nil, fmt.Errorf("mTLS requires a client CA file")
		}
		serverTLS.ClientCAs = pool
		serverTLS.ClientAuth = tls.RequireAndVerifyClientCert
	} else if pool != nil {
		serverTLS.ClientCAs = pool
		serverTLS.ClientAuth = tls.VerifyClientCertIfGiven
	}
	return credentials.NewTLS(serverTLS), nil
}

func clientCredentials(cfg *TLSConfig) (credentials.TransportCredentials, error) {
	if cfg == nil || !cfg.enabled() {
		return nil, nil
	}
	pool, err := loadCertPool(cfg.CAFile)
	if err != nil {
		return nil, fmt.Errorf("load control server CA: %w", err)
	}
	if pool == nil {
		return nil, fmt.Errorf("control TLS requires a CA file")
	}
	clientTLS := &tls.Config{
		MinVersion: tls.VersionTLS13,
		RootCAs:    pool,
		ServerName: cfg.ServerName,
	}
	if cfg.ClientCertFile != "" || cfg.ClientKeyFile != "" {
		if cfg.ClientCertFile == "" || cfg.ClientKeyFile == "" {
			return nil, fmt.Errorf("mTLS client requires both cert and key files")
		}
		cert, err := tls.LoadX509KeyPair(cfg.ClientCertFile, cfg.ClientKeyFile)
		if err != nil {
			return nil, fmt.Errorf("load control client certificate: %w", err)
		}
		clientTLS.Certificates = []tls.Certificate{cert}
	}
	return credentials.NewTLS(clientTLS), nil
}

func loadCertPool(path string) (*x509.CertPool, error) {
	if path == "" {
		return nil, nil
	}
	pemBytes, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(pemBytes) {
		return nil, fmt.Errorf("no certificates found")
	}
	return pool, nil
}

// TLSConfigFromEnv returns nil for the local insecure demo. Setting the server
// certificate and key enables TLS; setting CA plus client certs enables mTLS.
func TLSConfigFromEnv() *TLSConfig {
	cfg := &TLSConfig{
		CertFile:          os.Getenv("CONTROL_TLS_CERT_FILE"),
		KeyFile:           os.Getenv("CONTROL_TLS_KEY_FILE"),
		CAFile:            os.Getenv("CONTROL_TLS_CA_FILE"),
		ClientCertFile:    os.Getenv("CONTROL_TLS_CLIENT_CERT_FILE"),
		ClientKeyFile:     os.Getenv("CONTROL_TLS_CLIENT_KEY_FILE"),
		ServerName:        os.Getenv("CONTROL_TLS_SERVER_NAME"),
		RequireClientCert: os.Getenv("CONTROL_TLS_REQUIRE_CLIENT_CERT") == "1" || os.Getenv("CONTROL_TLS_REQUIRE_CLIENT_CERT") == "true",
	}
	if !cfg.enabled() {
		return nil
	}
	return cfg
}
