// SPDX-License-Identifier: GPL-3.0-or-later

// Package pki is Switchyard's small private CA: it issues the coordinator's
// server certificate and each node's client certificate, and defines the
// one-time enrollment token that carries the CA fingerprint so a new node can
// trust the coordinator before it has the CA.
package pki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jm2/switchyard/internal/paths"
	"github.com/jm2/switchyard/internal/version"
)

// File names inside a coordinator directory.
const (
	CACertFile     = "ca.crt"
	CAKeyFile      = "ca.key"
	ServerCertFile = "server.crt"
	ServerKeyFile  = "server.key"
)

func serial() *big.Int {
	n, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 126))
	if err != nil {
		panic(err)
	}
	return n
}

func newKey() *ecdsa.PrivateKey {
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		panic(err)
	}
	return k
}

func encodeCert(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

func encodeKey(k *ecdsa.PrivateKey) ([]byte, error) {
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		return nil, err
	}
	return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
}

// ParseCert decodes the first PEM certificate.
func ParseCert(pemBytes []byte) (*x509.Certificate, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil || b.Type != "CERTIFICATE" {
		return nil, errors.New("no PEM certificate found")
	}
	return x509.ParseCertificate(b.Bytes)
}

// ParseKey decodes a PKCS#8 PEM private key.
func ParseKey(pemBytes []byte) (crypto.Signer, error) {
	b, _ := pem.Decode(pemBytes)
	if b == nil {
		return nil, errors.New("no PEM key found")
	}
	k, err := x509.ParsePKCS8PrivateKey(b.Bytes)
	if err != nil {
		return nil, err
	}
	s, ok := k.(crypto.Signer)
	if !ok {
		return nil, errors.New("key is not a signer")
	}
	return s, nil
}

// Fingerprint is the hex SHA-256 of a certificate's DER bytes.
func Fingerprint(c *x509.Certificate) string {
	sum := sha256.Sum256(c.Raw)
	return hex.EncodeToString(sum[:])
}

func writeFile(path string, data []byte, mode os.FileMode) error {
	return paths.WriteFile(path, data, mode)
}

// InitCA creates a CA and a server certificate valid for hosts (DNS names or
// IP addresses; localhost and 127.0.0.1 are always added) in dir. It refuses
// to overwrite an existing CA.
func InitCA(dir string, hosts []string) error {
	if _, err := os.Stat(filepath.Join(dir, CACertFile)); err == nil {
		return fmt.Errorf("%s already exists in %s; refusing to overwrite the CA", CACertFile, dir)
	}
	caKey := newKey()
	now := time.Now().Add(-time.Hour)
	caTmpl := &x509.Certificate{
		SerialNumber:          serial(),
		Subject:               pkix.Name{CommonName: version.Name + " CA", Organization: []string{version.Name}},
		NotBefore:             now,
		NotAfter:              now.AddDate(10, 0, 0),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
		IsCA:                  true,
		MaxPathLenZero:        true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	caCert, _ := x509.ParseCertificate(caDER)
	caKeyPEM, err := encodeKey(caKey)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, CAKeyFile), caKeyPEM, 0o600); err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, CACertFile), encodeCert(caDER), 0o644); err != nil {
		return err
	}
	return IssueServerCert(dir, caCert, caKey, hosts)
}

// IssueServerCert (re)issues the coordinator's server certificate.
func IssueServerCert(dir string, caCert *x509.Certificate, caKey crypto.Signer, hosts []string) error {
	key := newKey()
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: version.Bin + " coordinator", Organization: []string{version.Name}},
		NotBefore:    now,
		NotAfter:     now.AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	seen := map[string]bool{}
	for _, h := range append([]string{"localhost", "127.0.0.1", "::1"}, hosts...) {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		if ip := net.ParseIP(h); ip != nil {
			tmpl.IPAddresses = append(tmpl.IPAddresses, ip)
		} else {
			tmpl.DNSNames = append(tmpl.DNSNames, h)
		}
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyPEM, err := encodeKey(key)
	if err != nil {
		return err
	}
	if err := writeFile(filepath.Join(dir, ServerKeyFile), keyPEM, 0o600); err != nil {
		return err
	}
	// The chain includes the CA so enrolling nodes can pin it by fingerprint.
	chain := append(encodeCert(der), encodeCert(caCert.Raw)...)
	return writeFile(filepath.Join(dir, ServerCertFile), chain, 0o644)
}

// CA is a loaded certificate authority.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	PEM  []byte
}

// LoadCA reads ca.crt and ca.key from dir.
func LoadCA(dir string) (*CA, error) {
	certPEM, err := os.ReadFile(filepath.Join(dir, CACertFile))
	if err != nil {
		return nil, err
	}
	keyPEM, err := os.ReadFile(filepath.Join(dir, CAKeyFile))
	if err != nil {
		return nil, err
	}
	cert, err := ParseCert(certPEM)
	if err != nil {
		return nil, err
	}
	key, err := ParseKey(keyPEM)
	if err != nil {
		return nil, err
	}
	return &CA{Cert: cert, Key: key, PEM: certPEM}, nil
}

// Pool returns a cert pool containing the CA.
func (ca *CA) Pool() *x509.CertPool {
	p := x509.NewCertPool()
	p.AddCert(ca.Cert)
	return p
}

// NewNodeKey returns a fresh node key and a CSR for it.
func NewNodeKey(hostID string) (keyPEM, csrPEM []byte, err error) {
	key := newKey()
	csrDER, err := x509.CreateCertificateRequest(rand.Reader, &x509.CertificateRequest{
		Subject: pkix.Name{CommonName: hostID},
	}, key)
	if err != nil {
		return nil, nil, err
	}
	keyPEM, err = encodeKey(key)
	if err != nil {
		return nil, nil, err
	}
	return keyPEM, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE REQUEST", Bytes: csrDER}), nil
}

// SignNodeCSR issues a client certificate for hostID. The CSR's requested
// subject is ignored; the host id comes from the enrollment token.
func (ca *CA) SignNodeCSR(csrPEM []byte, hostID string) ([]byte, error) {
	b, _ := pem.Decode(csrPEM)
	if b == nil || b.Type != "CERTIFICATE REQUEST" {
		return nil, errors.New("no PEM certificate request")
	}
	csr, err := x509.ParseCertificateRequest(b.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, fmt.Errorf("bad CSR signature: %w", err)
	}
	now := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial(),
		Subject:      pkix.Name{CommonName: hostID, OrganizationalUnit: []string{"node"}, Organization: []string{version.Name}},
		NotBefore:    now,
		NotAfter:     now.AddDate(5, 0, 0),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, ca.Cert, csr.PublicKey, ca.Key)
	if err != nil {
		return nil, err
	}
	return encodeCert(der), nil
}

// ServerTLS returns a TLS config serving dir's server certificate. With
// clientCA set, clients must present a certificate signed by it.
func ServerTLS(dir string, clientCA *CA) (*tls.Config, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(dir, ServerCertFile), filepath.Join(dir, ServerKeyFile))
	if err != nil {
		return nil, err
	}
	cfg := &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS13}
	if clientCA != nil {
		cfg.ClientAuth = tls.RequireAndVerifyClientCert
		cfg.ClientCAs = clientCA.Pool()
	}
	return cfg, nil
}

// ClientTLS returns a TLS config trusting caPEM, optionally presenting a
// client certificate.
func ClientTLS(caPEM, certPEM, keyPEM []byte, serverName string) (*tls.Config, error) {
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		return nil, errors.New("invalid CA certificate")
	}
	cfg := &tls.Config{RootCAs: pool, ServerName: serverName, MinVersion: tls.VersionTLS13}
	if certPEM != nil {
		c, err := tls.X509KeyPair(certPEM, keyPEM)
		if err != nil {
			return nil, err
		}
		cfg.Certificates = []tls.Certificate{c}
	}
	return cfg, nil
}

// PinnedTLS trusts a server only if its chain contains a CA certificate with
// the given fingerprint and the leaf verifies against that CA. Used for
// enrollment, before the node has the CA.
func PinnedTLS(fingerprint, serverName string) *tls.Config {
	return &tls.Config{
		MinVersion:         tls.VersionTLS13,
		InsecureSkipVerify: true, // replaced by VerifyPeerCertificate below
		VerifyPeerCertificate: func(raw [][]byte, _ [][]*x509.Certificate) error {
			var ca *x509.Certificate
			for _, r := range raw {
				sum := sha256.Sum256(r)
				if hex.EncodeToString(sum[:]) == fingerprint {
					c, err := x509.ParseCertificate(r)
					if err != nil {
						return err
					}
					ca = c
				}
			}
			if ca == nil {
				return errors.New("coordinator certificate chain does not contain the pinned CA")
			}
			if len(raw) == 0 {
				return errors.New("no server certificate")
			}
			leaf, err := x509.ParseCertificate(raw[0])
			if err != nil {
				return err
			}
			pool := x509.NewCertPool()
			pool.AddCert(ca)
			_, err = leaf.Verify(x509.VerifyOptions{Roots: pool, DNSName: serverName})
			return err
		},
	}
}

// Token is a one-time enrollment token.
type Token struct {
	API         string `json:"a"` // coordinator API address host:port
	Secret      string `json:"s"`
	Fingerprint string `json:"f"` // CA fingerprint
	Host        string `json:"h"` // host id the token enrolls
}

const tokenPrefix = "swyd1."

// Encode renders the token as a single copy-pasteable string.
func (t Token) Encode() string {
	b, _ := json.Marshal(t)
	return tokenPrefix + base64.RawURLEncoding.EncodeToString(b)
}

// DecodeToken parses a token string.
func DecodeToken(s string) (Token, error) {
	var t Token
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, tokenPrefix) {
		return t, errors.New("not a swyd enrollment token")
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(s, tokenPrefix))
	if err != nil {
		return t, fmt.Errorf("bad token encoding: %w", err)
	}
	if err := json.Unmarshal(b, &t); err != nil {
		return t, fmt.Errorf("bad token: %w", err)
	}
	if t.API == "" || t.Secret == "" || t.Fingerprint == "" || t.Host == "" {
		return t, errors.New("token is missing fields")
	}
	return t, nil
}

// RandomSecret returns n random bytes, hex encoded.
func RandomSecret(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

// HashSecret returns the hex SHA-256 of a secret, for storage.
func HashSecret(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}
