// Copyright (c) 2026 Aaron LI
//
// HTTP/2 (ALPN) and h2c tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"context"
	"crypto/tls"
	"net"
	"net/http"
	"testing"

	"golang.org/x/net/http2"
)

// TestHTTP2OverTLS checks that HTTP/2 is negotiated over TLS when enabled and
// that it is not negotiated when disabled.
func TestHTTP2OverTLS(t *testing.T) {
	cases := []struct {
		name    string
		enabled bool
		want    int
	}{
		{"enabled", true, 2},
		{"disabled", false, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			env := newTestEnv(t)
			env.cfg.Server.HTTP2Enabled = tc.enabled

			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			srv, tlsCfg := env.srv.httpsServer()
			go func() { _ = srv.Serve(tls.NewListener(ln, tlsCfg)) }()
			defer srv.Close()

			client := &http.Client{Transport: &http.Transport{
				TLSClientConfig:   &tls.Config{InsecureSkipVerify: true},
				ForceAttemptHTTP2: true,
			}}
			resp, err := client.Get("https://" + ln.Addr().String() + "/.api/v1/health")
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.ProtoMajor != tc.want {
				t.Fatalf("ProtoMajor = %d, want %d", resp.ProtoMajor, tc.want)
			}
		})
	}
}

// TestH2C checks cleartext HTTP/2 when enabled, and HTTP/1.1 when disabled.
func TestH2C(t *testing.T) {
	t.Run("enabled", func(t *testing.T) {
		env := newTestEnv(t)
		env.cfg.Server.H2CEnabled = true
		env.cfg.Server.HTTPSPort = 0 // http-only mode: serve the main handler

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := env.srv.newServer(env.srv.cleartextHandler())
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()

		resp, err := h2cClient().Get("http://" + ln.Addr().String() + "/.api/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.ProtoMajor != 2 {
			t.Fatalf("ProtoMajor = %d, want 2", resp.ProtoMajor)
		}
	})

	t.Run("disabled", func(t *testing.T) {
		env := newTestEnv(t)
		env.cfg.Server.H2CEnabled = false
		env.cfg.Server.HTTPSPort = 0

		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		srv := env.srv.newServer(env.srv.cleartextHandler())
		go func() { _ = srv.Serve(ln) }()
		defer srv.Close()

		// A normal HTTP/1.1 client works.
		resp, err := http.Get("http://" + ln.Addr().String() + "/.api/v1/health")
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.ProtoMajor != 1 {
			t.Fatalf("ProtoMajor = %d, want 1", resp.ProtoMajor)
		}

		// An h2c prior-knowledge client must not be served.
		if _, err := h2cClient().Get("http://" + ln.Addr().String() + "/.api/v1/health"); err == nil {
			t.Fatal("h2c request succeeded while h2c_enabled = false")
		}
	})
}

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
}
