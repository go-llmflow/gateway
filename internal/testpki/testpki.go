// Package testpki makes real X.509 material for tests: CAs, intermediate
// CAs and leaves with ECDSA or Ed25519 keys, as PEM over several lines,
// exactly as an operator's files hold them.
package testpki

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"sync/atomic"
	"time"
)

var serial atomic.Int64

// CA signs certificates.
type CA struct {
	Cert *x509.Certificate
	Key  crypto.Signer
	// chain is this CA's certificate and its parents' below the root, PEM.
	chain []byte
}

func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

func certPEM(der []byte) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
}

// NewRoot returns a self-signed root CA.
func NewRoot(name string) *CA {
	key := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	tpl := caTemplate(name)
	der := must(x509.CreateCertificate(rand.Reader, tpl, tpl, &key.PublicKey, key))
	return &CA{Cert: must(x509.ParseCertificate(der)), Key: key}
}

func caTemplate(name string) *x509.Certificate {
	return &x509.Certificate{SerialNumber: big.NewInt(serial.Add(1)), Subject: pkix.Name{CommonName: name},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(24 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageCRLSign}
}

// PEM is the CA's own certificate.
func (ca *CA) PEM() []byte { return certPEM(ca.Cert.Raw) }

// Intermediate returns a CA signed by ca.
func (ca *CA) Intermediate(name string) *CA {
	key := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
	der := must(x509.CreateCertificate(rand.Reader, caTemplate(name), ca.Cert, &key.PublicKey, ca.Key))
	return &CA{Cert: must(x509.ParseCertificate(der)), Key: key, chain: append(certPEM(der), ca.chain...)}
}

// Leaf returns a certificate for dnsName (client and server use), valid
// from notBefore to notAfter, followed by the intermediates up to the
// root (excluded), and its private key in PKCS #8. keyType is "ecdsa" or
// "ed25519".
func (ca *CA) Leaf(dnsName, keyType string, notBefore, notAfter time.Time) (cert, key []byte) {
	var pub crypto.PublicKey
	var priv crypto.Signer
	switch keyType {
	case "ed25519":
		p, k := must2(ed25519.GenerateKey(rand.Reader))
		pub, priv = p, k
	default:
		k := must(ecdsa.GenerateKey(elliptic.P256(), rand.Reader))
		pub, priv = &k.PublicKey, k
	}
	tpl := &x509.Certificate{SerialNumber: big.NewInt(serial.Add(1)), Subject: pkix.Name{CommonName: dnsName}, DNSNames: []string{dnsName},
		NotBefore: notBefore, NotAfter: notAfter, KeyUsage: x509.KeyUsageDigitalSignature,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth}}
	der := must(x509.CreateCertificate(rand.Reader, tpl, ca.Cert, pub, ca.Key))
	kd := must(x509.MarshalPKCS8PrivateKey(priv))
	return append(certPEM(der), ca.chain...), pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: kd})
}

// ValidLeaf is Leaf valid from an hour ago for a day.
func (ca *CA) ValidLeaf(dnsName, keyType string) (cert, key []byte) {
	return ca.Leaf(dnsName, keyType, time.Now().Add(-time.Hour), time.Now().Add(24*time.Hour))
}

func must2[A, B any](a A, b B, err error) (A, B) {
	if err != nil {
		panic(err)
	}
	return a, b
}
