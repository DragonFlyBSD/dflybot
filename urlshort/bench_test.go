// Copyright (c) 2026 Aaron LI
//
// Benchmarks for the request hot paths. Run:
//
//	go test -run '^$' -bench . -benchmem -count=10 ./...
//
// See BENCHMARKS.md for comparing and profiling.

package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"path/filepath"
	"testing"
	"time"
)

const benchTarget = "https://github.com/DragonFlyBSD/DragonFlyBSD/pull/12345"

// newBenchStore opens a temporary store holding one link.
func newBenchStore(b *testing.B) *BoltStore {
	b.Helper()
	s, err := OpenBoltStore(filepath.Join(b.TempDir(), "links.db"), 1<<20)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { s.Close() })
	if _, _, err := s.Create(benchTarget, "github-pr", "bench", "/gh/dfbsd/p/12345", nil); err != nil {
		b.Fatal(err)
	}
	return s
}

// BenchmarkStoreGet is the dominant cost of a redirect (bbolt + JSON decode).
func BenchmarkStoreGet(b *testing.B) {
	s := newBenchStore(b)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := s.Get("/gh/dfbsd/p/12345"); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkRedirectHandler is the end-to-end redirect path below the router.
func BenchmarkRedirectHandler(b *testing.B) {
	s := newBenchStore(b)
	srv := &Server{store: s, logger: slog.Default(), status: NewStatusState()}
	req := httptest.NewRequest(http.MethodGet, "/gh/dfbsd/p/12345", nil)
	req = req.WithContext(context.WithValue(req.Context(), accessInfoKey, &accessInfo{}))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		srv.handleRedirect(rec, req)
	}
}

// BenchmarkMiddlewareChain measures the per-request middleware cost with a
// no-op route (excludes store and access log writes).
func BenchmarkMiddlewareChain(b *testing.B) {
	srv := &Server{
		redirectLimiter: NewRateLimiter(1e9, 1e9, 1000),
		apiIPLimiter:    NewRateLimiter(1e9, 1e9, 1000),
		apiLimiter:      NewRateLimiter(1e9, 1e9, 1000),
		allowedHosts:    map[string]bool{"example.com": true},
		logger:          slog.Default(),
		status:          NewStatusState(),
	}
	h := srv.wrap(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	req := httptest.NewRequest(http.MethodGet, "/gh/dfbsd/p/12345", nil)
	req.Host = "example.com"
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
	}
}

// BenchmarkRateKey measures the per-request limiter key allocation.
func BenchmarkRateKey(b *testing.B) {
	cases := []struct {
		name string
		ip   netip.Addr
	}{
		{"ipv4", netip.MustParseAddr("192.0.2.5")},
		{"ipv6", netip.MustParseAddr("2001:db8::1")},
	}
	for _, tc := range cases {
		b.Run(tc.name, func(b *testing.B) {
			b.ReportAllocs()
			var key string
			for i := 0; i < b.N; i++ {
				key = rateKey(tc.ip)
			}
			_ = key
		})
	}
}

// BenchmarkWriteJSON measures the API response encoder.
func BenchmarkWriteJSON(b *testing.B) {
	rec := httptest.NewRecorder()
	v := linkView{
		Key:       "/gh/dfbsd/p/12345",
		ShortURL:  "https://example.com/gh/dfbsd/p/12345",
		Target:    benchTarget,
		Rule:      "github-pr",
		CreatedAt: time.Unix(1700000000, 0).UTC(),
		UpdatedAt: time.Unix(1700000000, 0).UTC(),
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rec.Body.Reset()
		writeJSON(rec, http.StatusOK, v)
	}
}

func benchAccessEntry() AccessEntry {
	return AccessEntry{
		Timestamp:  time.Unix(1700000000, 0).UTC(),
		Type:       AccessTypeRedirect,
		RemoteIP:   "198.51.100.7",
		Method:     http.MethodGet,
		Host:       "example.com",
		Path:       "/gh/dfbsd/p/12345",
		Key:        "/gh/dfbsd/p/12345",
		Status:     http.StatusFound,
		Target:     benchTarget,
		Rule:       "github-pr",
		RequestID:  "0123456789abcdef0123456789abcdef",
		UserAgent:  "Mozilla/5.0 (compatible; curl/8.5.0)",
		Referer:    "https://example.org/",
		DurationMS: 0.42,
		Bytes:      64,
	}
}

// TestAccessLogEnqueueAllocs checks that Log copies the entry into the channel
// without allocating. It is a test rather than a benchmark because the property
// is deterministic (0 allocs) and a throughput benchmark would mostly measure
// channel contention.
func TestAccessLogEnqueueAllocs(t *testing.T) {
	l := &AccessLogger{
		ch:     make(chan AccessEntry, 8192),
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		now:    func() time.Time { return time.Unix(1700000000, 0).UTC() },
	}
	e := benchAccessEntry()
	if n := testing.AllocsPerRun(1000, func() { l.Log(e) }); n != 0 {
		t.Fatalf("AccessLogger.Log allocates %v per call, want 0", n)
	}
}
