// Copyright (c) 2026 Aaron LI
//
// TLS configuration: autocert with tls-alpn-01, manual certificates, startup
// pre-warm, and certificate metadata recording for the status endpoint.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"time"

	"golang.org/x/crypto/acme"
	"golang.org/x/crypto/acme/autocert"
)

// buildCertManager constructs the certificate manager for the configuration.
// It returns nil when TLS is disabled (https_port == 0).
func buildCertManager(cfg *Config, status *StatusState, logger *slog.Logger) (CertManager, error) {
	if cfg.Server.HTTPSPort == 0 {
		return nil, nil
	}
	if cfg.ACME.Enabled {
		m, err := newACMEManager(cfg)
		if err != nil {
			return nil, err
		}
		return &recordingCertManager{inner: m, status: status, logger: logger}, nil
	}

	cert, err := tls.LoadX509KeyPair(cfg.Server.CertFile, cfg.Server.KeyFile)
	if err != nil {
		return nil, fmt.Errorf("load manual certificate: %w", err)
	}
	inner := &manualCertManager{cert: &cert}
	if info, err := certInfo(&cert); err == nil {
		status.SetCert(info)
	} else {
		logger.Warn("could not parse manual certificate", "error", err)
	}
	return &recordingCertManager{inner: inner, status: status, logger: logger}, nil
}

// newACMEManager builds the autocert manager. When http01_fallback is false,
// Manager.HTTPHandler is never called, so autocert uses tls-alpn-01 only (C3).
func newACMEManager(cfg *Config) (*autocert.Manager, error) {
	cacheDir := cfg.ACME.CacheDir
	if err := os.MkdirAll(cacheDir, 0o700); err != nil {
		return nil, fmt.Errorf("create acme cache dir %q: %w", cacheDir, err)
	}
	client := &acme.Client{
		DirectoryURL: cfg.ACME.DirectoryURL,
		HTTPClient:   &http.Client{Timeout: time.Duration(cfg.ACME.IssueTimeout) * time.Second},
	}
	if cfg.ACME.AccountKey != nil {
		client.Key = cfg.ACME.AccountKey
	}
	return &autocert.Manager{
		Prompt:      autocert.AcceptTOS,
		Cache:       autocert.DirCache(cacheDir),
		HostPolicy:  autocert.HostWhitelist(cfg.AllowedHosts()...),
		RenewBefore: time.Duration(cfg.ACME.RenewBeforeDays) * 24 * time.Hour,
		Email:       cfg.ACME.Email,
		Client:      client,
	}, nil
}

// manualCertManager serves a fixed certificate.
type manualCertManager struct {
	cert *tls.Certificate
}

func (m *manualCertManager) GetCertificate(*tls.ClientHelloInfo) (*tls.Certificate, error) {
	return m.cert, nil
}

func (m *manualCertManager) TLSConfig() *tls.Config {
	return &tls.Config{
		Certificates: []tls.Certificate{*m.cert},
		MinVersion:   tls.VersionTLS12,
		NextProtos:   []string{"h2", "http/1.1"},
	}
}

func (m *manualCertManager) HTTPHandler(fallback http.Handler) http.Handler {
	return fallback
}

// recordingCertManager wraps another manager and records the certificate the
// service actually serves, ignoring challenge handshakes (C17).
type recordingCertManager struct {
	inner  CertManager
	status *StatusState
	logger *slog.Logger
}

func (m *recordingCertManager) GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	cert, err := m.inner.GetCertificate(hello)
	if err != nil {
		// Surface certificate/issuance failures (including renewal failures
		// triggered by a handshake) via /status (section 12.5).
		m.status.SetError(err)
		return nil, err
	}

	// Skip recording if the handshake is an ACME tls-alpn-01 challenge.
	if hello != nil && len(hello.SupportedProtos) == 1 && hello.SupportedProtos[0] == acme.ALPNProto {
		return cert, nil
	}

	if info, err := certInfo(cert); err != nil {
		m.logger.Warn("could not parse served certificate", "error", err)
	} else {
		m.status.SetCert(info)
	}
	return cert, nil
}

func (m *recordingCertManager) TLSConfig() *tls.Config {
	cfg := m.inner.TLSConfig().Clone()
	cfg.GetCertificate = m.GetCertificate
	cfg.MinVersion = tls.VersionTLS12
	// Keep NextProtos including acme.ALPNProto (set by autocert).
	return cfg
}

func (m *recordingCertManager) HTTPHandler(fallback http.Handler) http.Handler {
	return m.inner.HTTPHandler(fallback)
}

// certInfo extracts display metadata from the leaf certificate.
func certInfo(cert *tls.Certificate) (*CertInfo, error) {
	if cert == nil || len(cert.Certificate) == 0 {
		return nil, errors.New("empty certificate")
	}
	leaf, err := x509.ParseCertificate(cert.Certificate[0])
	if err != nil {
		return nil, err
	}
	return &CertInfo{
		Subject:   leaf.Subject.CommonName,
		Issuer:    leaf.Issuer.CommonName,
		NotBefore: leaf.NotBefore.UTC(),
		NotAfter:  leaf.NotAfter.UTC(),
		DaysLeft:  int(time.Until(leaf.NotAfter).Hours() / 24),
	}, nil
}

// prewarmHello builds the synthetic ECDSA-capable, normal-ALPN hello used for
// pre-warming (C15, C17): nil signature/curve/cipher lists would make autocert
// issue an RSA certificate.
func prewarmHello(domain string) *tls.ClientHelloInfo {
	return &tls.ClientHelloInfo{
		ServerName:       domain,
		SupportedProtos:  []string{"h2", "http/1.1"},
		CipherSuites:     []uint16{tls.TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256},
		SupportedCurves:  []tls.CurveID{tls.CurveP256},
		SignatureSchemes: []tls.SignatureScheme{tls.ECDSAWithP256AndSHA256},
	}
}

// prewarm requests the certificate at startup in a goroutine. It never blocks
// startup and never fails the process (C18).
func prewarm(m CertManager, domain string, status *StatusState, logger *slog.Logger) {
	if m == nil || domain == "" {
		return
	}
	go func() {
		defer func() {
			if rec := recover(); rec != nil {
				logger.Error("panic during certificate pre-warm", "panic", rec)
				status.SetPrewarm(false, fmt.Errorf("panic: %v", rec))
			}
		}()
		if _, err := m.GetCertificate(prewarmHello(domain)); err != nil {
			logger.Warn("certificate pre-warm failed", "domain", domain, "error", err)
			status.SetPrewarm(false, err)
			return
		}
		logger.Info("certificate pre-warm succeeded", "domain", domain)
		status.SetPrewarm(true, nil)
	}()
}
