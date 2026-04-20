// gencerts generates a self-signed CA and a gateway certificate signed by it.
// Run once on the gateway machine, then distribute ca.crt to exit node operators.
//
// Usage: ./gencerts [output-dir]   (default: current directory)
package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log"
	"math/big"
	"os"
	"path/filepath"
	"time"
)

func main() {
	outDir := "."
	if len(os.Args) > 1 {
		outDir = os.Args[1]
	}
	if err := os.MkdirAll(outDir, 0700); err != nil {
		log.Fatalf("create output dir: %v", err)
	}

	// CA
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("generate CA key: %v", err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "Ambush CA"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("create CA cert: %v", err)
	}
	caCert, _ := x509.ParseCertificate(caDER)

	writeCert(filepath.Join(outDir, "ca.crt"), caDER)
	writeKey(filepath.Join(outDir, "ca.key"), caKey)

	// Gateway cert signed by CA
	// ServerName is fixed as "ambush-gateway" — exit nodes verify this name,
	// so the cert works regardless of the gateway's actual IP or domain.
	gwKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		log.Fatalf("generate gateway key: %v", err)
	}
	gwTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "ambush-gateway"},
		DNSNames:     []string{"ambush-gateway"},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(2 * 365 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	gwDER, err := x509.CreateCertificate(rand.Reader, gwTmpl, caCert, &gwKey.PublicKey, caKey)
	if err != nil {
		log.Fatalf("create gateway cert: %v", err)
	}

	writeCert(filepath.Join(outDir, "gateway.crt"), gwDER)
	writeKey(filepath.Join(outDir, "gateway.key"), gwKey)

	log.Printf("wrote ca.crt  ca.key  gateway.crt  gateway.key → %s", outDir)
	log.Println("next steps:")
	log.Println("  1. set TLS_CERT=gateway.crt and TLS_KEY=gateway.key in cmd/gateway/.env")
	log.Println("  2. distribute ca.crt to exit node operators (keep ca.key secret)")
	log.Println("  3. exit nodes: place ca.crt at ~/.ambush/ca.crt and set gateway URL to wss://")
}

func writeCert(path string, der []byte) {
	f := mustCreate(path, 0644)
	defer f.Close()
	must(pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: der}))
}

func writeKey(path string, key *ecdsa.PrivateKey) {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		log.Fatalf("marshal key: %v", err)
	}
	f := mustCreate(path, 0600)
	defer f.Close()
	must(pem.Encode(f, &pem.Block{Type: "EC PRIVATE KEY", Bytes: der}))
}

func mustCreate(path string, mode os.FileMode) *os.File {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		log.Fatalf("create %s: %v", path, err)
	}
	return f
}

func must(err error) {
	if err != nil {
		log.Fatal(err)
	}
}
