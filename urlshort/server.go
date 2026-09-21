// Copyright (c) 2026 Aaron LI
//
// HTTP server: listeners, middleware chain, routing, rate limiting, and the
// status state used by the API.
//
// Middleware order (outermost first): recover panic -> request ID -> host check
// -> body limit -> rate limit -> access log -> route.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"container/list"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
	"golang.org/x/time/rate"
)

// ---------------------------------------------------------------------------
// Certificate manager interface

// CertManager supplies TLS certificates. *autocert.Manager satisfies it (see
// tls.go); tests can substitute a fake.
type CertManager interface {
	GetCertificate(hello *tls.ClientHelloInfo) (*tls.Certificate, error)
	TLSConfig() *tls.Config
	HTTPHandler(fallback http.Handler) http.Handler
}

// ---------------------------------------------------------------------------
// Status state

// CertInfo summarises the certificate currently served.
type CertInfo struct {
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	NotBefore time.Time `json:"not_before"`
	NotAfter  time.Time `json:"not_after"`
	DaysLeft  int       `json:"days_left"`
}

// StatusState is the mutable state exposed by the health API.
type StatusState struct {
	mu         sync.Mutex
	startedAt  time.Time
	cert       *CertInfo
	prewarmOK  bool
	prewarmAt  time.Time
	prewarmErr string
	lastError  string
	lastErrAt  time.Time
}

// NewStatusState returns a status state anchored at the given start time.
func NewStatusState() *StatusState {
	return &StatusState{startedAt: time.Now().UTC()}
}

// StartedAt returns the process start time.
func (s *StatusState) StartedAt() time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.startedAt
}

// SetCert records the certificate metadata.
func (s *StatusState) SetCert(c *CertInfo) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.cert = c
}

// Cert returns the recorded certificate metadata, if any.
func (s *StatusState) Cert() *CertInfo {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cert
}

// SetPrewarm records the startup pre-warm result.
func (s *StatusState) SetPrewarm(ok bool, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prewarmOK = ok
	s.prewarmAt = time.Now().UTC()
	if err != nil {
		s.prewarmErr = err.Error()
	} else {
		s.prewarmErr = ""
	}
}

// Prewarm returns the pre-warm result.
func (s *StatusState) Prewarm() (ok bool, at time.Time, errMsg string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.prewarmOK, s.prewarmAt, s.prewarmErr
}

// SetError records the most recent notable error.
func (s *StatusState) SetError(err error) {
	if err == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastError = err.Error()
	s.lastErrAt = time.Now().UTC()
}

// LastError returns the most recent notable error.
func (s *StatusState) LastError() (time.Time, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErrAt, s.lastError
}

// ---------------------------------------------------------------------------
// Rate limiter

// RateLimiter is a bounded LRU of per-key token buckets.
type RateLimiter struct {
	mu      sync.Mutex
	limit   rate.Limit
	burst   int
	max     int
	entries map[string]*list.Element
	order   *list.List
}

type rateEntry struct {
	key     string
	limiter *rate.Limiter
}

// NewRateLimiter returns a limiter with max tracked keys.
func NewRateLimiter(perSecond float64, burst, max int) *RateLimiter {
	if max <= 0 {
		max = 10000
	}
	return &RateLimiter{
		limit:   rate.Limit(perSecond),
		burst:   burst,
		max:     max,
		entries: make(map[string]*list.Element),
		order:   list.New(),
	}
}

// Allow reports whether the key may proceed now.
func (rl *RateLimiter) Allow(key string) bool {
	rl.mu.Lock()
	el, ok := rl.entries[key]
	if ok {
		rl.order.MoveToFront(el)
	} else {
		if rl.order.Len() >= rl.max {
			if back := rl.order.Back(); back != nil {
				rl.order.Remove(back)
				delete(rl.entries, back.Value.(*rateEntry).key)
			}
		}
		el = rl.order.PushFront(&rateEntry{
			key:     key,
			limiter: rate.NewLimiter(rl.limit, rl.burst),
		})
		rl.entries[key] = el
	}
	lim := el.Value.(*rateEntry).limiter
	rl.mu.Unlock()
	return lim.Allow()
}

// ---------------------------------------------------------------------------
// Server

// Server wires the store, rules, auth, and logging into an HTTP service.
type Server struct {
	cfg         *Config
	store       Store
	rules       *Ruleset
	auth        *Authenticator
	logs        *AccessLogger
	logger      *slog.Logger
	certs       CertManager
	status      *StatusState
	maintenance *Maintenance

	redirectLimiter *RateLimiter
	apiLimiter      *RateLimiter
	allowedHosts    map[string]bool
	trustedProxies  []netip.Prefix
	hstsValue       string

	// Handler for normal (HTTPS or http-only) traffic.
	mainHandler http.Handler
	// Handler for port 80 traffic (i.e., ACME challenge + HTTPS redirect)
	httpHandler http.Handler

	mu          sync.Mutex
	listenAddrs []string

	shutdownTimeout time.Duration
	readTimeout     time.Duration
	writeTimeout    time.Duration
	idleTimeout     time.Duration
}

// NewServer builds the server and its handlers.
func NewServer(
	cfg *Config,
	store Store,
	certs CertManager,
	status *StatusState,
	base *slog.Logger,
) (srv *Server, err error) {
	if base == nil {
		base = slog.Default()
	}
	if status == nil {
		status = NewStatusState()
	}

	if cfg.Server.HTTPSPort > 0 && certs == nil {
		return nil, errors.New("https_port is set but no certificate manager is configured")
	}

	logs, err := NewAccessLogger(cfg.LogsDir(), cfg.AccessLog.RetentionDays,
		time.Duration(cfg.AccessLog.FlushInterval)*time.Second, nil, base)
	if err != nil {
		return nil, err
	}
	// Roll back the access logger if a later step fails. Registered only after
	// logs is created so the receiver is never nil.
	defer func() {
		if err != nil {
			logs.Close()
		}
	}()

	rules, err := NewRuleset(cfg.Rules, cfg.Abbreviations)
	if err != nil {
		return nil, err
	}
	auth, err := NewAuthenticator(cfg.Clients)
	if err != nil {
		return nil, err
	}

	logger := base.With(slog.String("comp", "server"))
	srv = &Server{
		cfg:             cfg,
		store:           store,
		rules:           rules,
		auth:            auth,
		logs:            logs,
		logger:          logger,
		certs:           certs,
		status:          status,
		redirectLimiter: NewRateLimiter(cfg.RateLimit.RedirectRate, cfg.RateLimit.RedirectBurst, 10000),
		apiLimiter:      NewRateLimiter(cfg.RateLimit.APIRate, cfg.RateLimit.APIBurst, 10000),
		allowedHosts:    make(map[string]bool),
		shutdownTimeout: time.Duration(cfg.Server.ShutdownTimeout) * time.Second,
		readTimeout:     time.Duration(cfg.Server.ReadTimeout) * time.Second,
		writeTimeout:    time.Duration(cfg.Server.WriteTimeout) * time.Second,
		idleTimeout:     time.Duration(cfg.Server.IdleTimeout) * time.Second,
	}
	for _, h := range cfg.AllowedHosts() {
		srv.allowedHosts[h] = true
	}
	srv.trustedProxies = cfg.GetTrustedProxies()
	if hsts := cfg.Server.HSTS; hsts.Enabled {
		srv.hstsValue = hsts.HeaderValue()
	}
	srv.mainHandler = srv.buildMainHandler()
	srv.httpHandler = srv.buildHTTPHandler()
	return srv, nil
}

// Close releases the resources owned by the server. It is safe to call more
// than once and after Serve has returned. The caller owns the Store.
func (s *Server) Close() {
	if s.logs != nil {
		s.logs.Close()
	}
}

// SetMaintenance attaches the backup maintenance state for the status endpoint.
func (s *Server) SetMaintenance(m *Maintenance) {
	s.maintenance = m
}

// newServer builds an http.Server with the configured timeouts. HTTP/2 is
// configured by the caller.
func (s *Server) newServer(h http.Handler) *http.Server {
	return &http.Server{
		Handler:        h,
		ReadTimeout:    s.readTimeout,
		WriteTimeout:   s.writeTimeout,
		IdleTimeout:    s.idleTimeout,
		MaxHeaderBytes: s.cfg.Server.MaxHeaderBytes,
		ErrorLog:       slog.NewLogLogger(s.logger.Handler(), slog.LevelWarn),
	}
}

// httpsServer builds the http.Server and *tls.Config for an HTTPS listener. When
// http2_enabled is false, a non-nil TLSNextProto without an "h2" entry disables
// the automatic HTTP/2 configuration in net/http.
func (s *Server) httpsServer() (*http.Server, *tls.Config) {
	srv := s.newServer(s.mainHandler)
	cfg := s.certs.TLSConfig()
	if s.cfg.Server.HTTP2Enabled {
		if !slices.Contains(cfg.NextProtos, "h2") {
			cfg.NextProtos = append([]string{"h2"}, cfg.NextProtos...)
		}
	} else {
		srv.TLSNextProto = map[string]func(*http.Server, *tls.Conn, http.Handler){}
		if i := slices.Index(cfg.NextProtos, "h2"); i >= 0 {
			cfg.NextProtos = slices.Delete(cfg.NextProtos, i, i+1)
		}
	}
	return srv, cfg
}

func (s *Server) buildMainHandler() http.Handler {
	var route http.Handler = http.HandlerFunc(s.routeMain)
	if s.hstsValue != "" {
		next := route
		route = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Strict-Transport-Security", s.hstsValue)
			next.ServeHTTP(w, r)
		})
	}
	return s.wrap(route)
}

// buildHTTPHandler builds the port-80 handler: ACME http-01 (when enabled)
// with a redirect-to-HTTPS fallback.
func (s *Server) buildHTTPHandler() http.Handler {
	var route http.Handler = http.HandlerFunc(s.redirectToHTTPS)
	if s.cfg.ACME.Enabled && s.cfg.ACME.HTTP01Fallback && s.certs != nil {
		route = s.certs.HTTPHandler(route)
	}
	return s.wrap(route)
}

// ---------------------------------------------------------------------------
// Middleware

func (s *Server) wrap(route http.Handler) http.Handler {
	var h http.Handler = route
	h = s.accessLogMiddleware(h)
	h = s.rateLimitMiddleware(h)
	h = s.bodyLimitMiddleware(h)
	h = s.hostCheckMiddleware(h)
	h = s.requestIDMiddleware(h)
	h = s.recoverMiddleware(h)
	return h
}

func (s *Server) recoverMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				s.logger.Error("panic serving request", "panic", rec, "path", r.URL.Path,
					"method", r.Method, "header", r.Header)
				s.status.SetError(fmt.Errorf("panic: %v", rec))
				writePlainError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}

type contextKey int

const (
	requestIDKey contextKey = iota
	accessInfoKey
	clientIPKey
)

// accessInfo is filled in by handlers for the access log.
type accessInfo struct {
	Type   string
	Client string
	Action string
	Key    string
	Target string
	Rule   string
}

func accessInfoFrom(r *http.Request) *accessInfo {
	if ai, ok := r.Context().Value(accessInfoKey).(*accessInfo); ok {
		return ai
	}
	return &accessInfo{}
}

// clientIPFrom returns the client IP computed by requestIDMiddleware.
func clientIPFrom(r *http.Request) netip.Addr {
	if ip, ok := r.Context().Value(clientIPKey).(netip.Addr); ok {
		return ip
	}
	return netip.Addr{}
}

func (s *Server) requestIDMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-ID")
		if id == "" {
			var b [16]byte
			if _, err := rand.Read(b[:]); err == nil {
				id = hex.EncodeToString(b[:])
			} else {
				id = strconv.FormatInt(time.Now().UnixNano(), 36)
			}
		}
		w.Header().Set("X-Request-ID", id)
		ctx := context.WithValue(r.Context(), requestIDKey, id)
		ctx = context.WithValue(ctx, accessInfoKey, &accessInfo{})
		ctx = context.WithValue(ctx, clientIPKey, s.clientIP(r))
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (s *Server) hostCheckMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodConnect {
			writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if r.RequestURI == "*" {
			writePlainError(w, http.StatusBadRequest, "asterisk-form request target not allowed")
			return
		}
		if r.URL.IsAbs() {
			writePlainError(w, http.StatusBadRequest, "absolute-form request target not allowed")
			return
		}
		host := r.Host
		if h, _, err := net.SplitHostPort(host); err == nil {
			host = h
		}
		if host == "" || !s.allowedHosts[strings.ToLower(host)] {
			writePlainError(w, http.StatusMisdirectedRequest, "misdirected request")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) bodyLimitMiddleware(next http.Handler) http.Handler {
	const maxAPIBodyBytes = 64 * 1024 // 64KB

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			r.Body = http.MaxBytesReader(w, r.Body, maxAPIBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) rateLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isAPIPath(r.URL.Path) {
			// API rate limiting is per token and happens after authentication.
			next.ServeHTTP(w, r)
			return
		}
		key := rateKey(clientIPFrom(r))
		if key == "" || !s.redirectLimiter.Allow(key) {
			w.Header().Set("Retry-After", "1")
			writePlainError(w, http.StatusTooManyRequests, "rate limited")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) accessLogMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if s.logs == nil {
			next.ServeHTTP(w, r)
			return
		}

		start := time.Now()
		rec := &statusRecorder{ResponseWriter: w}
		next.ServeHTTP(rec, r)

		status := rec.status
		if status == 0 {
			status = http.StatusOK
		}
		ai := accessInfoFrom(r)
		typ := ai.Type
		if typ == "" {
			typ = classifyAccessType(r.URL.Path)
		}
		id, _ := r.Context().Value(requestIDKey).(string)
		remote := stripPort(r.RemoteAddr)
		if ip := clientIPFrom(r); ip.IsValid() {
			remote = ip.String()
		}
		entry := AccessEntry{
			Timestamp:  start.UTC(),
			Type:       typ,
			RemoteIP:   remote,
			Method:     r.Method,
			Host:       r.Host,
			Path:       r.URL.Path,
			Key:        ai.Key,
			Status:     status,
			Target:     ai.Target,
			Rule:       ai.Rule,
			Client:     ai.Client,
			Action:     ai.Action,
			RequestID:  id,
			UserAgent:  r.UserAgent(),
			Referer:    r.Referer(),
			DurationMS: float64(time.Since(start).Microseconds()) / 1000.0,
			Bytes:      rec.bytes,
		}
		s.logs.Log(entry)
	})
}

func classifyAccessType(path string) string {
	switch {
	case strings.HasPrefix(path, "/.well-known/acme-challenge/"):
		return AccessTypeACME
	case strings.HasPrefix(path, apiRoot):
		return AccessTypeAPI
	case path == "/":
		return AccessTypeHome
	default:
		return AccessTypeRedirect
	}
}

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int64
}

func (r *statusRecorder) WriteHeader(code int) {
	if r.status == 0 {
		r.status = code
	}
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += int64(n)
	return n, err
}

func (r *statusRecorder) Flush() {
	if f, ok := r.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// ---------------------------------------------------------------------------
// Routing

func (s *Server) routeMain(w http.ResponseWriter, r *http.Request) {
	p := r.URL.Path
	switch {
	case isAPIPath(p):
		s.handleAPI(w, r)
	case strings.HasPrefix(p, "/.well-known/"):
		writePlainError(w, http.StatusNotFound, "404 page not found")
	case p == "/":
		s.handleHome(w, r)
	case p == "/robots.txt":
		if !requireGetHead(w, r) {
			return
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		w.Write([]byte("User-agent: *\nDisallow: /\n"))
	case p == "/favicon.ico":
		if !requireGetHead(w, r) {
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		s.handleRedirect(w, r)
	}
}

// redirectToHTTPS redirects the request to HTTPS.
func (s *Server) redirectToHTTPS(w http.ResponseWriter, r *http.Request) {
	writeSecurityHeaders(w)
	host := stripPort(r.Host)
	if s.cfg.Server.HTTPSPort != 443 {
		host = net.JoinHostPort(host, strconv.Itoa(s.cfg.Server.HTTPSPort))
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	w.Header().Set("Location", "https://"+host+r.RequestURI)
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusPermanentRedirect)
}

// ---------------------------------------------------------------------------
// Helpers

// stripPort removes the port of an address (e.g., RemoteAddr, Host header).
func stripPort(addr string) string {
	if host, _, err := net.SplitHostPort(addr); err == nil {
		return host
	}
	return addr
}

// isTrustedProxy reports whether ip is one of the configured trusted proxies.
func (s *Server) isTrustedProxy(ip netip.Addr) bool {
	for _, p := range s.trustedProxies {
		if p.Contains(ip) {
			return true
		}
	}
	return false
}

// clientIP extracts the real client IP. Forwarding headers are only trusted
// when the direct peer is a configured trusted proxy (C13, section 14).
// X-Forwarded-For is walked right-to-left, skipping trusted proxies, so a
// client-injected value cannot spoof the result; X-Real-IP is the fallback.
func (s *Server) clientIP(r *http.Request) netip.Addr {
	direct, err := netip.ParseAddr(stripPort(r.RemoteAddr))
	if err != nil {
		return netip.Addr{}
	}
	direct = direct.Unmap()
	if !s.isTrustedProxy(direct) {
		return direct
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		items := strings.Split(xff, ",")
		for i := len(items) - 1; i >= 0; i-- {
			cand, err := netip.ParseAddr(strings.TrimSpace(items[i]))
			if err != nil {
				break
			}
			cand = cand.Unmap()
			if i == 0 || !s.isTrustedProxy(cand) {
				return cand
			}
		}
	}
	if xri := strings.TrimSpace(r.Header.Get("X-Real-IP")); xri != "" {
		if cand, err := netip.ParseAddr(xri); err == nil {
			return cand.Unmap()
		}
	}
	return direct
}

// rateKey buckets an address for rate limiting: IPv4 /32 and IPv6 /64.
func rateKey(ip netip.Addr) string {
	if !ip.IsValid() {
		return ""
	}
	ip = ip.Unmap()
	if ip.Is4() {
		return ip.String()
	}
	p, err := ip.Prefix(64)
	if err != nil {
		return ip.String()
	}
	return p.String()
}

func writeSecurityHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
}

// writePlainError writes a non-JSON error with the security headers required
// on every error response (section 14).
func writePlainError(w http.ResponseWriter, status int, msg string) {
	writeSecurityHeaders(w)
	w.Header().Set("Cache-Control", "no-store")
	http.Error(w, msg, status)
}

// requireGetHead enforces the GET/HEAD-only rule for static paths.
func requireGetHead(w http.ResponseWriter, r *http.Request) bool {
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		return true
	}
	w.Header().Set("Allow", "GET, HEAD")
	writePlainError(w, http.StatusMethodNotAllowed, "method not allowed")
	return false
}

// ---------------------------------------------------------------------------
// Listeners and serving

// listenTCP binds one TCP listener, setting IPV6_V6ONLY on IPv6 sockets so the
// IPv4 and IPv6 sockets can coexist (C6).
func listenTCP(ctx context.Context, addr string, port int) (net.Listener, error) {
	lc := net.ListenConfig{
		Control: func(network, address string, c syscall.RawConn) error {
			var setErr error
			if err := c.Control(func(fd uintptr) {
				if strings.Contains(addr, ":") {
					setErr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IPV6,
						unix.IPV6_V6ONLY, 1)
				}
			}); err != nil {
				return err
			}
			return setErr
		},
	}
	return lc.Listen(ctx, "tcp", net.JoinHostPort(addr, strconv.Itoa(port)))
}

// Serve binds all configured listeners and blocks until ctx is canceled or a
// fatal serve error occurs.
func (s *Server) Serve(ctx context.Context) error {
	var servers []*http.Server
	errCh := make(chan error, 16)

	add := func(ln net.Listener, srv *http.Server, desc string) {
		servers = append(servers, srv)
		s.mu.Lock()
		s.listenAddrs = append(s.listenAddrs, desc)
		s.mu.Unlock()
		go func() {
			if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
				errCh <- fmt.Errorf("%s: %w", desc, err)
			}
		}()
	}
	closeAll := func() {
		for _, srv := range servers {
			srv.Close()
		}
	}

	for _, addr := range s.cfg.Server.ListenAddresses {
		if s.cfg.Server.HTTPPort > 0 {
			ln, err := listenTCP(ctx, addr, s.cfg.Server.HTTPPort)
			if err != nil {
				closeAll()
				return fmt.Errorf("listen http %s:%d: %w", addr, s.cfg.Server.HTTPPort, err)
			}
			h := s.mainHandler
			if s.cfg.Server.HTTPSPort > 0 {
				h = s.httpHandler
			}
			add(ln, s.newServer(h), ln.Addr().String())
		}
		if s.cfg.Server.HTTPSPort > 0 {
			ln, err := listenTCP(ctx, addr, s.cfg.Server.HTTPSPort)
			if err != nil {
				closeAll()
				return fmt.Errorf("listen https %s:%d: %w", addr, s.cfg.Server.HTTPSPort, err)
			}
			srv, tlsCfg := s.httpsServer()
			add(tls.NewListener(ln, tlsCfg), srv, ln.Addr().String())
		}
	}
	if len(servers) == 0 {
		return errors.New("no listeners configured")
	}

	select {
	case <-ctx.Done():
		return s.shutdown(servers)
	case err := <-errCh:
		s.shutdown(servers)
		return err
	}
}

func (s *Server) shutdown(servers []*http.Server) error {
	timeout := s.shutdownTimeout
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	var firstErr error
	for _, srv := range servers {
		if err := srv.Shutdown(ctx); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// Listeners returns the descriptions of the bound listeners.
func (s *Server) Listeners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.listenAddrs...)
}
