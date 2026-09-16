// Copyright (c) 2026 Aaron LI
//
// HTTP/2 (ALPN) and h2c tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto/tls"
	"net"
	"net/http"
	"testing"
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
