package oai_test

// The test binary's own certificate authority (W06-10 hardening 3, H3-1
// gate (h)): the route dials its rows through internal/llm's pool clients,
// which verify against the system roots and negotiate HTTP/2 over TLS.
// TestMain names a fresh CA in SSL_CERT_FILE before any test runs — the
// system roots load once, on the first verification — so the route trusts
// the HTTP/2 test backends that serve the leaf of that CA. httptest's
// built-in certificate stays untrusted (the unknown-CA gate). SSL_CERT_FILE
// replaces only the bundle file; the system certificate directories still
// load. internal/oai may not import crypto/tls (depguard oai-isolation):
// the backends serve through http.Server.ServeTLS from PEM files, and the
// negotiated protocol is read by reflection.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// wireCertFile and wireKeyFile are a server certificate for 127.0.0.1 under
// the binary's CA and its key (PEM).
var wireCertFile, wireKeyFile string

// sslCertFile is SSL_CERT_FILE as the process started with it (set: whether
// it was set): a child process the tests start (the Python SDK harness, uv)
// gets this value back, not the binary's CA, which would leave it trusting
// nothing but that CA.
var (
	sslCertFile    string
	sslCertFileSet bool
)

func TestMain(m *testing.M) {
	os.Exit(runWithWireCA(m))
}

func runWithWireCA(m *testing.M) int {
	dir, err := os.MkdirTemp("", "oai-wire-ca-")
	if err != nil {
		fmt.Fprintln(os.Stderr, "wire CA:", err)
		return 1
	}
	defer func() { _ = os.RemoveAll(dir) }()
	if err := writeWireCA(dir); err != nil {
		fmt.Fprintln(os.Stderr, "wire CA:", err)
		return 1
	}
	sslCertFile, sslCertFileSet = os.LookupEnv("SSL_CERT_FILE")
	if err := os.Setenv("SSL_CERT_FILE", filepath.Join(dir, "ca.pem")); err != nil {
		fmt.Fprintln(os.Stderr, "wire CA:", err)
		return 1
	}
	wireCertFile, wireKeyFile = filepath.Join(dir, "leaf.pem"), filepath.Join(dir, "leaf.key")
	return m.Run()
}

// writeWireCA writes ca.pem, leaf.pem and leaf.key into dir.
func writeWireCA(dir string) error {
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	now := time.Now()
	caTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "oai wire test CA"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour),
		IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	ca, err := x509.ParseCertificate(caDER)
	if err != nil {
		return err
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return err
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2), Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(48 * time.Hour),
		IPAddresses: []net.IP{net.IPv4(127, 0, 0, 1)}, DNSNames: []string{"localhost"},
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, ca, &leafKey.PublicKey, caKey)
	if err != nil {
		return err
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		return err
	}
	for name, b := range map[string]*pem.Block{
		"ca.pem":   {Type: "CERTIFICATE", Bytes: caDER},
		"leaf.pem": {Type: "CERTIFICATE", Bytes: leafDER},
		"leaf.key": {Type: "EC PRIVATE KEY", Bytes: keyDER},
	} {
		if err := os.WriteFile(filepath.Join(dir, name), pem.EncodeToMemory(b), 0o600); err != nil {
			return err
		}
	}
	return nil
}

// serveH2 serves h over TLS (the binary's CA) on l — http.Server.ServeTLS
// offers h2 by ALPN — and returns the base URL.
func serveH2(t testing.TB, l net.Listener, h http.Handler) string {
	t.Helper()
	srv := &http.Server{Handler: h, ReadHeaderTimeout: 10 * time.Second, ErrorLog: log.New(io.Discard, "", 0)}
	go func() { _ = srv.ServeTLS(l, wireCertFile, wireKeyFile) }()
	t.Cleanup(func() { _ = srv.Close() })
	return "https://" + l.Addr().String()
}

// negotiatedProtocol is the ALPN result of a client connection (the
// *tls.Conn's ConnectionState, read by reflection), "" for a plain one.
func negotiatedProtocol(c net.Conn) string {
	m := reflect.ValueOf(c).MethodByName("ConnectionState")
	if !m.IsValid() {
		return ""
	}
	return m.Call(nil)[0].FieldByName("NegotiatedProtocol").String()
}

// connLog records the raw connections a listener accepted (before TLS).
type connLog struct {
	net.Listener
	conns chan net.Conn
}

func newConnLog(l net.Listener) *connLog { return &connLog{Listener: l, conns: make(chan net.Conn, 16)} }

func (l *connLog) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err == nil {
		select {
		case l.conns <- c:
		default:
		}
	}
	return c, err
}
