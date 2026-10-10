package ipp

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	certFile = "tls-cert.pem"
	keyFile  = "tls-key.pem"
)

// LoadOrCreateCert returns the server certificate stored in dir, creating a
// self-signed ECDSA P-256 one when it is missing, unreadable or close to expiry.
// An empty dir yields a fresh in-memory certificate.
func LoadOrCreateCert(dir string) (tls.Certificate, error) {
	if dir != "" {
		cert, err := tls.LoadX509KeyPair(filepath.Join(dir, certFile), filepath.Join(dir, keyFile))
		if err == nil && cert.Leaf != nil && time.Until(cert.Leaf.NotAfter) > 30*24*time.Hour {
			return cert, nil
		}
	}
	certPEM, keyPEM, err := newSelfSigned(certHosts())
	if err != nil {
		return tls.Certificate{}, err
	}
	if dir != "" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return tls.Certificate{}, err
		}
		if err := writeFileAtomic(filepath.Join(dir, keyFile), keyPEM, 0o600); err != nil {
			return tls.Certificate{}, err
		}
		if err := writeFileAtomic(filepath.Join(dir, certFile), certPEM, 0o644); err != nil {
			return tls.Certificate{}, err
		}
	}
	return tls.X509KeyPair(certPEM, keyPEM)
}

func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func certHosts() []string {
	hosts := []string{"localhost", "127.0.0.1", "::1"}
	if h, err := os.Hostname(); err == nil && h != "" {
		h = strings.TrimSuffix(h, ".")
		short, _, _ := strings.Cut(h, ".")
		hosts = append(hosts, h, short+".local")
	}
	if addrs, err := net.InterfaceAddrs(); err == nil {
		for _, a := range addrs {
			if ipn, ok := a.(*net.IPNet); ok && !ipn.IP.IsLinkLocalUnicast() {
				hosts = append(hosts, ipn.IP.String())
			}
		}
	}
	return hosts
}

func newSelfSigned(hosts []string) (certPEM, keyPEM []byte, err error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, err
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 127))
	if err != nil {
		return nil, nil, err
	}
	cn := "ql-airprint"
	for _, h := range hosts {
		if net.ParseIP(h) == nil && h != "localhost" {
			cn = h
			break
		}
	}
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: cn, Organization: []string{"ql-airprint"}},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}
	seen := map[string]bool{}
	for _, h := range hosts {
		if seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("ipp: create certificate: %w", err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return nil, nil, err
	}
	certPEM = pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyPEM = pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if certPEM == nil || keyPEM == nil {
		return nil, nil, errors.New("ipp: pem encode failed")
	}
	return certPEM, keyPEM, nil
}
