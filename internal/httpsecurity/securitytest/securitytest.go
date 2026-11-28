// Package securitytest supplies test-only certificate/environment fixtures.
// Application binaries do not import it.
package securitytest

import (
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

func ClearEnv(t *testing.T) {
	t.Helper()
	for _, key := range []string{"HTTP_WEBHOOK_BASIC_AUTH_USERNAME", "HTTP_WEBHOOK_BASIC_AUTH_PASSWORD_ENV", "HTTP_METRICS_BASIC_AUTH_USERNAME", "HTTP_METRICS_BASIC_AUTH_PASSWORD_ENV", "HTTP_INSPECTION_BASIC_AUTH_USERNAME", "HTTP_INSPECTION_BASIC_AUTH_PASSWORD_ENV", "HTTP_TLS_CERT_FILE", "HTTP_TLS_KEY_FILE"} {
		t.Setenv(key, "")
	}
}

func Auth(t *testing.T, endpoint, username, password string) {
	t.Helper()
	reference := "HTTP_SECURITY_TEST_" + endpoint + "_SECRET"
	t.Setenv("HTTP_"+endpoint+"_BASIC_AUTH_USERNAME", username)
	t.Setenv("HTTP_"+endpoint+"_BASIC_AUTH_PASSWORD_ENV", reference)
	t.Setenv(reference, password)
}

func Certificate(t *testing.T) (certFile, keyFile string, roots *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "localhost"},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		DNSNames: []string{"localhost"}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true, IsCA: true}
	data, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	private, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: data})
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "server.crt"), filepath.Join(dir, "server.key")
	if err := os.WriteFile(certFile, certPEM, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: private}), 0600); err != nil {
		t.Fatal(err)
	}
	roots = x509.NewCertPool()
	if !roots.AppendCertsFromPEM(certPEM) {
		t.Fatal("invalid generated cert")
	}
	return
}
