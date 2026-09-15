// Copyright (c) 2026 Aaron LI
//
// TLS/ACME tests (no real ACME traffic).
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"golang.org/x/crypto/acme"
)

func makeTestCert(t *testing.T, cn string) tls.Certificate {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

type fakeCertManager struct {
	cert  tls.Certificate
	calls int
}

func (f *fakeCertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	f.calls++
	return &f.cert, nil
}
func (f *fakeCertManager) TLSConfig() *tls.Config                  { return &tls.Config{} }
func (f *fakeCertManager) HTTPHandler(h http.Handler) http.Handler { return h }

func TestRecordingCertManager(t *testing.T) {
	status := NewStatusState(time.Now().UTC())
	inner := &fakeCertManager{cert: makeTestCert(t, "example.com")}
	m := &recordingCertManager{inner: inner, status: status, logger: slog.Default()}

	// A challenge handshake must not be recorded.
	if _, err := m.GetCertificate(&tls.ClientHelloInfo{SupportedProtos: []string{acme.ALPNProto}}); err != nil {
		t.Fatal(err)
	}
	if status.Cert() != nil {
		t.Fatal("challenge handshake was recorded")
	}

	// A normal handshake is recorded.
	if _, err := m.GetCertificate(prewarmHello("example.com")); err != nil {
		t.Fatal(err)
	}
	info := status.Cert()
	if info == nil || info.Subject != "example.com" {
		t.Fatalf("cert info = %+v", info)
	}
	if info.DaysLeft < 80 || info.DaysLeft > 91 {
		t.Fatalf("DaysLeft = %d", info.DaysLeft)
	}
}

func TestPrewarm(t *testing.T) {
	status := NewStatusState(time.Now().UTC())
	inner := &fakeCertManager{cert: makeTestCert(t, "example.com")}
	m := &recordingCertManager{inner: inner, status: status, logger: slog.Default()}

	prewarm(m, "example.com", status, slog.Default())
	waitFor(t, "prewarm result", func() bool {
		ok, at, _ := status.Prewarm()
		return ok && !at.IsZero()
	})
	if status.Cert() == nil {
		t.Fatal("prewarm did not record the certificate")
	}
	if inner.calls == 0 {
		t.Fatal("prewarm did not call GetCertificate")
	}
}

func TestManualCertManager(t *testing.T) {
	// Write a self-signed cert/key pair to disk.
	cert := makeTestCert(t, "manual.example.com")
	key := cert.PrivateKey.(*ecdsa.PrivateKey)
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certPath := filepath.Join(dir, "cert.pem")
	keyPath := filepath.Join(dir, "key.pem")
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: cert.Certificate[0]})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
	if err := os.WriteFile(certPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, keyPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	cfg := DefaultConfig()
	cfg.DataDir = dir + "/"
	cfg.Server.PublicURL = "https://example.com"
	cfg.Server.HTTPSPort = 443
	cfg.Server.CertFile = certPath
	cfg.Server.KeyFile = keyPath
	cfg.ACME.Enabled = false
	if err := cfg.applyDerivedDefaults(); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}

	status := NewStatusState(time.Now().UTC())
	m, err := buildCertManager(cfg, status, slog.Default())
	if err != nil {
		t.Fatal(err)
	}
	if m == nil {
		t.Fatal("expected a manager")
	}
	if info := status.Cert(); info == nil || info.Subject != "manual.example.com" {
		t.Fatalf("manual cert info = %+v", info)
	}
	if _, err := m.GetCertificate(prewarmHello("example.com")); err != nil {
		t.Fatalf("GetCertificate: %v", err)
	}
}

func TestBuildCertManagerDisabled(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Server.HTTPSPort = 0
	m, err := buildCertManager(cfg, NewStatusState(time.Now()), slog.Default())
	if err != nil || m != nil {
		t.Fatalf("expected nil manager, got %v err=%v", m, err)
	}
}
