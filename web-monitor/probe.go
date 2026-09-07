// Copyright (c) 2026 Aaron LI
//
// Web monitor that periodically probes the configured web services and
// announces site failures/recoveries (with hysteresis) and expiring TLS
// certificates to IRC via dflybot's webhook.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"time"
)

// Default timeouts (milliseconds), used when the config value is unset.
const (
	defaultDNSMs     = 3000
	defaultConnectMs = 3000
	defaultHeaderMs  = 5000
	defaultBodyMs    = 10000
)

// timeouts are the resolved probe timeouts of a web.
type timeouts struct {
	dns     time.Duration
	connect time.Duration
	header  time.Duration
	body    time.Duration
}

func resolveTimeouts(c *ConfigTimeouts) timeouts {
	ms := func(v, def int) time.Duration {
		if v <= 0 {
			v = def
		}
		return time.Duration(v) * time.Millisecond
	}
	return timeouts{
		dns:     ms(c.DNS, defaultDNSMs),
		connect: ms(c.Connect, defaultConnectMs),
		header:  ms(c.Header, defaultHeaderMs),
		body:    ms(c.Body, defaultBodyMs),
	}
}

// prober performs the HTTP probes of one web.
type prober struct {
	url       string
	client    *http.Client
	statusOK  func(int) bool
	verifyTLS bool
}

// newProber builds the HTTP client for one web from the per-web and the
// global (tls, timeouts) settings.
func newProber(web *ConfigWeb, tlsCfg *ConfigTLS, to timeouts, follow bool, verify bool) (*prober, error) {
	u, err := url.Parse(web.URL)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") {
		return nil, fmt.Errorf("invalid url %q", web.URL)
	}

	resolver := &net.Resolver{
		PreferGo: true,
		Dial: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: to.dns}
			return d.DialContext(ctx, network, address)
		},
	}
	dialer := &net.Dialer{Timeout: to.connect, Resolver: resolver}

	var tlsConf *tls.Config
	if u.Scheme == "https" {
		tlsConf = &tls.Config{}
		if !verify {
			tlsConf.InsecureSkipVerify = true
		} else if tlsCfg.CAFile != "" {
			pem, err := os.ReadFile(tlsCfg.CAFile)
			if err != nil {
				return nil, fmt.Errorf("read CA file %s: %w", tlsCfg.CAFile, err)
			}
			pool := x509.NewCertPool()
			if !pool.AppendCertsFromPEM(pem) {
				return nil, fmt.Errorf("no certificates in CA file %s", tlsCfg.CAFile)
			}
			tlsConf.RootCAs = pool
		}
	}

	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSClientConfig:       tlsConf,
		TLSHandshakeTimeout:   to.connect,
		ResponseHeaderTimeout: to.header,
		Proxy:                 http.ProxyFromEnvironment,
	}
	client := &http.Client{Transport: transport, Timeout: to.body}
	if !follow {
		client.CheckRedirect = func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}
	}

	return &prober{
		url:       web.URL,
		client:    client,
		statusOK:  makeStatusCheck(web.StatusCodes),
		verifyTLS: verify,
	}, nil
}

// makeStatusCheck returns the expected-status predicate (empty list = any
// 2xx).
func makeStatusCheck(codes []int) func(int) bool {
	if len(codes) == 0 {
		return func(s int) bool { return s >= 200 && s < 300 }
	}
	set := make(map[int]struct{}, len(codes))
	for _, c := range codes {
		set[c] = struct{}{}
	}
	return func(s int) bool {
		_, ok := set[s]
		return ok
	}
}

// certInfo describes the leaf certificate of a verified HTTPS probe.
type certInfo struct {
	notAfterUnix int64
	daysLeft     int
}

// probeResult is the outcome of one probe.
type probeResult struct {
	ok     bool
	status int   // HTTP status, 0 if none
	ms     int64 // time until response headers
	reason string
	cert   *certInfo // set only for verified HTTPS with a certificate
}

// Probe performs one GET request and evaluates the result.
func (p *prober) Probe() *probeResult {
	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, p.url, nil)
	if err != nil {
		return &probeResult{ok: false, reason: err.Error()}
	}
	resp, err := p.client.Do(req)
	ms := time.Since(start).Milliseconds()
	res := &probeResult{ms: ms}
	if err != nil {
		res.ok = false
		res.reason = classifyError(err)
		return res
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body) // drain; the client Timeout bounds it

	res.status = resp.StatusCode
	res.ok = p.statusOK(resp.StatusCode)
	if !res.ok {
		res.reason = fmt.Sprintf("unexpected status %d", resp.StatusCode)
		return res
	}

	if p.verifyTLS && resp.TLS != nil && len(resp.TLS.PeerCertificates) > 0 {
		leaf := resp.TLS.PeerCertificates[0]
		res.cert = &certInfo{
			notAfterUnix: leaf.NotAfter.Unix(),
			daysLeft:     int(math.Floor(time.Until(leaf.NotAfter).Hours() / 24)),
		}
	}
	return res
}

// classifyError reduces a probe error to a short, URL-free reason.
func classifyError(err error) string {
	var uerr *url.Error
	if errors.As(err, &uerr) && uerr.Timeout() {
		return "timeout"
	}
	msg := err.Error()
	// Drop the verbose `Get "https://...": ` prefix when present.
	if uerr != nil {
		msg = uerr.Err.Error()
	}
	if msg == "" {
		return "error"
	}
	return msg
}
