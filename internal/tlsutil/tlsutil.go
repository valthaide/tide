package tlsutil

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
	"time"

	"github.com/valthaide/tide/internal/envcompat"
)

// ResolveCertPaths returns TLS cert/key paths from env or dataDir/tls/.
func ResolveCertPaths(dataDir string) (certFile, keyFile string) {
	certFile = envcompat.Getenv("TIDE_TLS_CERT")
	keyFile = envcompat.Getenv("TIDE_TLS_KEY")
	if certFile != "" && keyFile != "" {
		return certFile, keyFile
	}
	dir := filepath.Join(dataDir, "tls")
	return filepath.Join(dir, "cert.pem"), filepath.Join(dir, "key.pem")
}

// EnsureServerCert returns cert/key paths, generating a self-signed cert if missing.
func EnsureServerCert(dataDir string) (certFile, keyFile string, err error) {
	certFile, keyFile = ResolveCertPaths(dataDir)
	if fileExists(certFile) && fileExists(keyFile) {
		return certFile, keyFile, nil
	}

	dir := filepath.Dir(certFile)
	if err := os.MkdirAll(dir, 0700); err != nil {
		return "", "", err
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return "", "", err
	}

	serial, err := rand.Int(rand.Reader, big.NewInt(0).Lsh(big.NewInt(1), 128))
	if err != nil {
		return "", "", err
	}

	host, _ := os.Hostname()
	template := x509.Certificate{
		SerialNumber: serial,
		Subject:      pkix.Name{Organization: []string{"Tide"}, CommonName: host},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost", host},
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1"), net.ParseIP("::1")},
	}

	der, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return "", "", err
	}

	certOut, err := os.OpenFile(certFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", "", err
	}
	if err := pem.Encode(certOut, &pem.Block{Type: "CERTIFICATE", Bytes: der}); err != nil {
		_ = certOut.Close()
		return "", "", err
	}
	_ = certOut.Close()

	keyBytes, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return "", "", err
	}
	keyOut, err := os.OpenFile(keyFile, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return "", "", err
	}
	if err := pem.Encode(keyOut, &pem.Block{Type: "EC PRIVATE KEY", Bytes: keyBytes}); err != nil {
		_ = keyOut.Close()
		return "", "", err
	}
	_ = keyOut.Close()

	return certFile, keyFile, nil
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
