package control

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestGRPCMutualTLSGetConfig(t *testing.T) {
	certs := makeTestCertificates(t)
	store := NewMemoryStore()
	srv, err := NewServer(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := srv.ApplyConfig(map[string]string{"test-key": "tenant-a"}, routes()); err != nil {
		t.Fatal(err)
	}

	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	gs, err := NewGRPCServerWithTLS(srv, &TLSConfig{
		CertFile:          certs.serverCert,
		KeyFile:           certs.serverKey,
		CAFile:            certs.caCert,
		RequireClientCert: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	go gs.Serve(lis)
	defer func() {
		gs.Stop()
		_ = lis.Close()
	}()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	client, err := DialContextWithTLS(ctx, lis.Addr().String(), &TLSConfig{
		CAFile:         certs.caCert,
		ClientCertFile: certs.clientCert,
		ClientKeyFile:  certs.clientKey,
		ServerName:     "localhost",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer client.Close()
	cfg, err := client.GetConfig(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 || !hasKeyRecord(cfg.KeyRecords, "tenant-a") {
		t.Fatalf("unexpected mTLS config: %+v", cfg)
	}
}

func TestTLSConfigRejectsIncompleteMaterial(t *testing.T) {
	if _, err := NewGRPCServerWithTLS(newTestServer(t), &TLSConfig{CertFile: "cert.pem"}); err == nil {
		t.Fatal("incomplete server TLS config unexpectedly accepted")
	}
	if _, err := DialContextWithTLS(context.Background(), "127.0.0.1:1", &TLSConfig{CAFile: "missing.pem"}); err == nil {
		t.Fatal("missing client CA unexpectedly accepted")
	}
}

func newTestServer(t *testing.T) *Server {
	t.Helper()
	srv, err := NewServer(NewMemoryStore())
	if err != nil {
		t.Fatal(err)
	}
	return srv
}

type testCertificates struct {
	caCert, serverCert, serverKey, clientCert, clientKey string
}

func makeTestCertificates(t *testing.T) testCertificates {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTemplate := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test control CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTemplate, caTemplate, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caCert := filepath.Join(dir, "ca.pem")
	writePEM(t, caCert, "CERTIFICATE", caDER)

	serverCert, serverKey := makeLeafCertificate(t, dir, "server", caTemplate, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth})
	clientCert, clientKey := makeLeafCertificate(t, dir, "client", caTemplate, caKey, []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth})
	return testCertificates{caCert: caCert, serverCert: serverCert, serverKey: serverKey, clientCert: clientCert, clientKey: clientKey}
}

func makeLeafCertificate(t *testing.T, dir, name string, ca *x509.Certificate, caKey *ecdsa.PrivateKey, usages []x509.ExtKeyUsage) (string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(int64(len(name) + 2)),
		Subject:      pkix.Name{CommonName: name},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		ExtKeyUsage:  usages,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, name+".crt")
	keyPath := filepath.Join(dir, name+".key")
	writePEM(t, certPath, "CERTIFICATE", der)
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	writePEM(t, keyPath, "PRIVATE KEY", keyDER)
	return certPath, keyPath
}

func writePEM(t *testing.T, path, kind string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: data}), 0o600); err != nil {
		t.Fatal(err)
	}
}
