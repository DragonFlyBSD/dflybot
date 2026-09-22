// Copyright (c) 2026 Aaron LI
//
// REST API handlers.
//
// All endpoints except /health require a bearer token. JSON in and out, body
// limit 64 KiB, strict decoding.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"time"
)

const (
	// apiRoot is the reserved parent of every API version. Namespaces, link
	// keys, and redirects must never use it as a prefix.
	apiRoot = "/.api/"

	// apiBase is the versioned API base.
	apiBase = apiRoot + "v1"

	apiPathHealth = apiBase + "/health"
	apiPathStatus = apiBase + "/status"
	apiPathWhoami = apiBase + "/whoami"
	apiPathLinks  = apiBase + "/links"
)

// isAPIPath reports whether p targets the API tree, including apiBase itself
// (the API index).
func isAPIPath(p string) bool {
	return p == apiBase || strings.HasPrefix(p, apiBase+"/")
}

// apiAuth is the token requirement of an endpoint.
type apiAuth uint8

const (
	authPublic apiAuth = iota // no token required
	authAny                   // any valid token
	authAdmin                 // admin token only
)

// apiEndpoint describes one API path: its allowed methods, token
// requirement, and handler. The fields are unexported; the index is rendered
// from this table.
type apiEndpoint struct {
	methods []string
	path    string
	auth    apiAuth
	handle  func(*Server, http.ResponseWriter, *http.Request, *Client)
}

// apiEndpoints is the single source of truth for routing, authentication, and
// the API index. It is populated in init() to break the initialization cycle
// between the table (which references the handlers) and apiIndex (which reads
// the table).
var apiEndpoints []apiEndpoint

func init() {
	apiEndpoints = []apiEndpoint{
		{[]string{http.MethodGet}, apiBase, authPublic, (*Server).handleAPIIndex},
		{[]string{http.MethodGet}, apiPathHealth, authPublic, (*Server).handleHealth},
		{[]string{http.MethodGet}, apiPathStatus, authAdmin, (*Server).handleStatus},
		{[]string{http.MethodGet}, apiPathWhoami, authAny, (*Server).handleWhoami},
		{[]string{http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete},
			apiPathLinks, authAny, (*Server).handleLinks},
	}
}

// apiIndexEntry is one method/path row in the API index.
type apiIndexEntry struct {
	Method string `json:"method"`
	Path   string `json:"path"`
}

// apiIndex expands the endpoint table into method/path rows.
func apiIndex() []apiIndexEntry {
	var out []apiIndexEntry
	for _, ep := range apiEndpoints {
		for _, m := range ep.methods {
			out = append(out, apiIndexEntry{Method: m, Path: ep.path})
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// JSON helpers

func writeJSON(w http.ResponseWriter, status int, v any) {
	h := w.Header()
	h.Set("Content-Type", "application/json; charset=utf-8")
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

type apiErrorBody struct {
	Error apiErrorDetail `json:"error"`
}

type apiErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func writeAPIError(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, apiErrorBody{Error: apiErrorDetail{Code: code, Message: message}})
}

func (s *Server) writeInternalError(w http.ResponseWriter, err error, context string) {
	s.logger.Error(context, "error", err)
	s.status.SetError(err)
	writeAPIError(w, http.StatusInternalServerError, "internal", "internal server error")
}

func decodeJSON(w http.ResponseWriter, r *http.Request, v any) bool {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		var mbe *http.MaxBytesError
		if errors.As(err, &mbe) {
			writeAPIError(w, http.StatusRequestEntityTooLarge, "payload_too_large",
				"request body too large")
			return false
		}
		if errors.Is(err, io.EOF) {
			writeAPIError(w, http.StatusBadRequest, "bad_request",
				"request body is required")
			return false
		}
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"invalid JSON: "+err.Error())
		return false
	}
	if err := dec.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"request body must contain a single JSON object")
		return false
	}
	return true
}

func requireMethod(w http.ResponseWriter, r *http.Request, methods ...string) bool {
	for _, m := range methods {
		if r.Method == m {
			return true
		}
	}
	w.Header().Set("Allow", strings.Join(methods, ", "))
	writeAPIError(w, http.StatusMethodNotAllowed, "method_not_allowed", "method not allowed")
	return false
}

// ---------------------------------------------------------------------------
// Dispatch and authentication

func (s *Server) handleAPI(w http.ResponseWriter, r *http.Request) {
	if !s.allowAPIIP(w, r) {
		return
	}

	path := strings.TrimSuffix(r.URL.Path, "/")
	var ep *apiEndpoint
	for i := range apiEndpoints {
		if apiEndpoints[i].path == path {
			ep = &apiEndpoints[i]
			break
		}
	}
	if ep == nil {
		writeAPIError(w, http.StatusNotFound, "not_found", "unknown endpoint")
		return
	}
	if !requireMethod(w, r, ep.methods...) {
		return
	}

	var c *Client
	if ep.auth != authPublic {
		ok := false
		c, ok = s.authenticate(w, r)
		if !ok {
			return
		}
		if ep.auth == authAdmin && !c.IsAdmin {
			writeAPIError(w, http.StatusForbidden, "forbidden", "admin required")
			return
		}
	}
	if ep.auth == authAny && !s.allowAPI(w, c) {
		return
	}

	ep.handle(s, w, r, c)
}

func (s *Server) authenticate(w http.ResponseWriter, r *http.Request) (*Client, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized",
			"missing or malformed Authorization header")
		return nil, false
	}
	token := strings.TrimSpace(strings.TrimPrefix(h, prefix))
	c, ok := s.auth.Authenticate(token)
	if !ok {
		writeAPIError(w, http.StatusUnauthorized, "unauthorized", "invalid token")
		return nil, false
	}
	accessInfoFrom(r).Client = c.Name
	return c, true
}

func (s *Server) allowAPI(w http.ResponseWriter, c *Client) bool {
	if s.apiLimiter.Allow(c.Name) {
		return true
	}
	w.Header().Set("Retry-After", "1")
	writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
	return false
}

// allowAPIIP applies the per-IP limiter to every API request before
// authentication, covering public endpoints, unknown paths, and failed
// authentication.
func (s *Server) allowAPIIP(w http.ResponseWriter, r *http.Request) bool {
	key := rateKey(clientIPFrom(r))
	if key == "" || !s.apiIPLimiter.Allow(key) {
		w.Header().Set("Retry-After", "1")
		writeAPIError(w, http.StatusTooManyRequests, "rate_limited", "too many requests")
		return false
	}
	return true
}

// ---------------------------------------------------------------------------
// Simple endpoints

func (s *Server) handleAPIIndex(w http.ResponseWriter, r *http.Request, _ *Client) {
	writeJSON(w, http.StatusOK, struct {
		Name      string          `json:"name"`
		Version   string          `json:"version"`
		Endpoints []apiIndexEntry `json:"endpoints"`
	}{programName, version, apiIndex()})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request, _ *Client) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *Server) handleWhoami(w http.ResponseWriter, r *http.Request, c *Client) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":       c.Name,
		"admin":      c.IsAdmin,
		"namespaces": c.Namespaces,
	})
}

// ---------------------------------------------------------------------------
// Links

func (s *Server) handleLinks(w http.ResponseWriter, r *http.Request, c *Client) {
	switch r.Method {
	case http.MethodGet:
		if r.URL.Query().Has("key") {
			s.getLink(w, r, c)
		} else {
			s.listLinks(w, r, c)
		}
	case http.MethodPost:
		s.createLink(w, r, c)
	case http.MethodPut:
		s.updateLink(w, r, c)
	case http.MethodDelete:
		s.deleteLink(w, r, c)
	default:
		requireMethod(w, r, http.MethodGet, http.MethodPost, http.MethodPut, http.MethodDelete)
	}
}

type linkView struct {
	Key       string    `json:"key"`
	ShortURL  string    `json:"short_url"`
	Target    string    `json:"target"`
	Rule      string    `json:"rule,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type createResponse struct {
	linkView
	Created bool `json:"created"`
}

func (s *Server) linkView(l *Link) linkView {
	base := s.cfg.PublicURL()
	return linkView{
		Key:       l.Key,
		ShortURL:  base.JoinPath(l.Key).String(),
		Target:    l.Target,
		Rule:      l.Rule,
		CreatedAt: l.CreatedAt,
		UpdatedAt: l.UpdatedAt,
	}
}

func (s *Server) storeError(w http.ResponseWriter, err error, context string) {
	switch {
	case errors.Is(err, ErrForbidden):
		writeAPIError(w, http.StatusForbidden, "forbidden",
			"key is outside the caller's namespaces")
	case errors.Is(err, ErrConflict):
		// Operator-actionable: hash exhaustion or a cross-rule collision (7.4).
		s.logger.Warn("link conflict", "error", err)
		writeAPIError(w, http.StatusConflict, "conflict", err.Error())
	case errors.Is(err, ErrNotFound):
		writeAPIError(w, http.StatusNotFound, "not_found", "link not found")
	default:
		s.writeInternalError(w, err, context)
	}
}

func (s *Server) getLink(w http.ResponseWriter, r *http.Request, c *Client) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"key query parameter is required")
		return
	}
	if !c.CanAccess(key) {
		writeAPIError(w, http.StatusForbidden, "forbidden",
			"key is outside the caller's namespaces")
		return
	}
	link, err := s.store.Get(key)
	if err != nil {
		s.storeError(w, err, "Get link failed")
		return
	}

	ai := accessInfoFrom(r)
	ai.Action = "resolve"
	ai.Key = key
	writeJSON(w, http.StatusOK, s.linkView(link))
}

func (s *Server) listLinks(w http.ResponseWriter, r *http.Request, c *Client) {
	q := r.URL.Query()
	nsParam := q.Get("namespace")
	limit := 100
	if v := q.Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			writeAPIError(w, http.StatusBadRequest, "bad_request",
				"limit must be a positive integer")
			return
		}
		limit = n
	}
	if limit > 1000 {
		limit = 1000
	}
	cursor := q.Get("cursor")

	var (
		links []*Link
		next  string
		err   error
	)
	switch {
	case nsParam != "":
		if !c.CanAccess(nsParam) {
			writeAPIError(w, http.StatusForbidden, "forbidden",
				"namespace is outside the caller's namespaces")
			return
		}
		links, next, err = s.store.List(nsParam, limit, cursor)
	case c.IsAdmin:
		links, next, err = s.store.List("", limit, cursor)
	case len(c.Namespaces) == 1:
		links, next, err = s.store.List(c.Namespaces[0], limit, cursor)
	default:
		links, next, err = s.listFiltered(c, limit, cursor)
	}
	if err != nil {
		s.writeInternalError(w, err, "List links failed")
		return
	}

	views := make([]linkView, 0, len(links))
	for _, l := range links {
		views = append(views, s.linkView(l))
	}
	accessInfoFrom(r).Action = "resolve"
	writeJSON(w, http.StatusOK, map[string]any{
		"links":       views,
		"next_cursor": next,
	})
}

// listFiltered lists links across a client's multiple namespaces.
func (s *Server) listFiltered(c *Client, limit int, cursor string) ([]*Link, string, error) {
	out := []*Link{}
	next := cursor
	for len(out) < limit {
		batch, n, err := s.store.List("", limit, next)
		if err != nil {
			return nil, "", err
		}
		for _, l := range batch {
			if c.CanAccess(l.Key) {
				out = append(out, l)
			}
			if len(out) >= limit {
				next = l.Key
				break
			}
		}
		if len(out) >= limit {
			return out, next, nil
		}
		if n == "" {
			return out, "", nil
		}
		next = n
	}
	return out, next, nil
}

func (s *Server) createLink(w http.ResponseWriter, r *http.Request, c *Client) {
	var req struct {
		Target string `json:"target"`
		Key    string `json:"key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	target, err := canonicalizeTarget(req.Target)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"invalid target: "+err.Error())
		return
	}

	if req.Key == "" {
		// A target that already has a key returns it, even when the key is
		// outside the caller's namespace: it is already published.
		if key, err := s.store.Resolve(target); err == nil {
			if link, err := s.store.Get(key); err == nil {
				s.finishCreate(w, r, link, false)
				return
			}
		}
	}

	if req.Key != "" {
		if err := ValidateKey(req.Key); err != nil {
			writeAPIError(w, http.StatusBadRequest, "bad_request",
				"invalid key: "+err.Error())
			return
		}
		if !c.CanAccess(req.Key) {
			writeAPIError(w, http.StatusForbidden, "forbidden",
				"key is outside the caller's namespaces")
			return
		}
		link, created, err := s.store.Create(target, "", c.Name, req.Key, nil)
		if err != nil {
			s.storeError(w, err, "Create link failed")
			return
		}
		s.finishCreate(w, r, link, created)
		return
	}

	rule, vars, matched := s.rules.Match(target)
	var (
		gen      KeyFunc
		ruleName string
	)
	if matched {
		ruleName = rule.Name
		// Render a key to validate the namespace.
		full, err := rule.Render(vars)
		if err != nil {
			s.writeInternalError(w, err, "rule render failed")
			return
		}
		if !c.CanAccess(full) {
			writeAPIError(w, http.StatusForbidden, "forbidden",
				"rule key is outside the caller's namespaces")
			return
		}
		gen = func(isFree func(string) (bool, error)) (string, error) {
			key, err := rule.GenerateKey(vars, isFree)
			if err != nil {
				return "", err
			}
			if !c.CanAccess(key) {
				return "", ErrForbidden
			}
			return key, nil
		}
	} else {
		ns := c.FirstNamespace()
		if ns == "" {
			writeAPIError(w, http.StatusBadRequest, "bad_request",
				"no namespace available")
			return
		}
		gen = func(isFree func(string) (bool, error)) (string, error) {
			return GenerateRandomKey(ns, isFree)
		}
	}

	link, created, err := s.store.Create(target, ruleName, c.Name, "", gen)
	if err != nil {
		s.storeError(w, err, "Create link failed")
		return
	}
	s.finishCreate(w, r, link, created)
}

func (s *Server) finishCreate(w http.ResponseWriter, r *http.Request, link *Link, created bool) {
	ai := accessInfoFrom(r)
	ai.Key = link.Key
	ai.Target = link.Target
	ai.Rule = link.Rule
	if created {
		ai.Action = "create"
	} else {
		ai.Action = "resolve"
	}
	status := http.StatusOK
	if created {
		status = http.StatusCreated
	}
	writeJSON(w, status, createResponse{linkView: s.linkView(link), Created: created})
}

// canonicalizeTarget parses and canonicalizes a target URL. It lowercases the
// scheme and host, drops default ports, and rejects userinfo. Path, query, and
// fragment are preserved exactly.
func canonicalizeTarget(raw string) (string, error) {
	const maxTargetLen = 8192

	if raw == "" {
		return "", errors.New("empty target")
	}
	if len(raw) > maxTargetLen {
		return "", fmt.Errorf("target longer than %d bytes", maxTargetLen)
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid URL: %w", err)
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme != "http" && scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q (only http and https)", u.Scheme)
	}
	if u.User != nil {
		return "", errors.New("URL userinfo is not allowed")
	}
	host := strings.ToLower(u.Hostname())
	if host == "" {
		return "", errors.New("URL has no host")
	}
	port := u.Port()
	if (scheme == "http" && port == "80") || (scheme == "https" && port == "443") {
		port = ""
	}
	switch {
	case port != "":
		u.Host = net.JoinHostPort(host, port)
	case strings.Contains(host, ":"):
		u.Host = "[" + host + "]"
	default:
		u.Host = host
	}
	u.Scheme = scheme
	return u.String(), nil
}

func (s *Server) updateLink(w http.ResponseWriter, r *http.Request, c *Client) {
	var req struct {
		Target string `json:"target"`
		Key    string `json:"key"`
	}
	if !decodeJSON(w, r, &req) {
		return
	}
	target, err := canonicalizeTarget(req.Target)
	if err != nil {
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"invalid target: "+err.Error())
		return
	}
	if req.Key == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request", "key is required")
		return
	}
	if !c.CanAccess(req.Key) {
		writeAPIError(w, http.StatusForbidden, "forbidden",
			"key is outside the caller's namespaces")
		return
	}

	link, err := s.store.Update(req.Key, target)
	if err != nil {
		s.storeError(w, err, "Update link failed")
		return
	}

	ai := accessInfoFrom(r)
	ai.Action = "update"
	ai.Key = req.Key
	ai.Target = link.Target
	writeJSON(w, http.StatusOK, s.linkView(link))
}

func (s *Server) deleteLink(w http.ResponseWriter, r *http.Request, c *Client) {
	key := r.URL.Query().Get("key")
	if key == "" {
		writeAPIError(w, http.StatusBadRequest, "bad_request",
			"key query parameter is required")
		return
	}
	if !c.CanAccess(key) {
		writeAPIError(w, http.StatusForbidden, "forbidden",
			"key is outside the caller's namespaces")
		return
	}
	if err := s.store.Delete(key); err != nil {
		s.storeError(w, err, "Delete link failed")
		return
	}
	ai := accessInfoFrom(r)
	ai.Action = "delete"
	ai.Key = key
	w.WriteHeader(http.StatusNoContent)
}

// ---------------------------------------------------------------------------
// Status

type statusResponse struct {
	Version       string             `json:"version"`
	StartedAt     time.Time          `json:"started_at"`
	UptimeSeconds int64              `json:"uptime_seconds"`
	Server        statusServer       `json:"server"`
	ACME          statusACME         `json:"acme"`
	Links         statusLinks        `json:"links"`
	Clients       []statusClient     `json:"clients"`
	AccessLog     AccessLogStats     `json:"access_log"`
	DB            statusDB           `json:"db"`
	Maintenance   *MaintenanceStatus `json:"maintenance,omitempty"`
	Runtime       statusRuntime      `json:"runtime"`
	LastError     *statusError       `json:"last_error,omitempty"`
}

type statusServer struct {
	PublicURL    string   `json:"public_url"`
	Listeners    []string `json:"listeners"`
	HTTPEnabled  bool     `json:"http_enabled"`
	HTTPSEnabled bool     `json:"https_enabled"`
}

type statusACME struct {
	Enabled          bool       `json:"enabled"`
	DirectoryURL     string     `json:"directory_url"`
	Staging          bool       `json:"staging"`
	Certificate      *CertInfo  `json:"certificate,omitempty"`
	LastPrewarmOK    bool       `json:"last_prewarm_ok"`
	LastPrewarmAt    *time.Time `json:"last_prewarm_at,omitempty"`
	LastPrewarmError string     `json:"last_prewarm_error,omitempty"`
}

type statusLinks struct {
	Total      int            `json:"total"`
	Namespaces map[string]int `json:"namespaces"`
}

type statusClient struct {
	Name       string   `json:"name"`
	Namespaces []string `json:"namespaces"`
	Admin      bool     `json:"admin"`
}

type statusRuntime struct {
	Goroutines       int    `json:"goroutines"`
	MemoryAllocBytes uint64 `json:"memory_alloc_bytes"`
}

// statusDB matches the status shape in section 10.4: file size plus a
// tx_stats object.
type statusDB struct {
	FileSizeBytes int64         `json:"file_size_bytes"`
	TxStats       statusTxStats `json:"tx_stats"`
}

type statusTxStats struct {
	TxN          int `json:"tx_n"`
	OpenTxN      int `json:"open_tx_n"`
	FreePageN    int `json:"free_page_n"`
	PendingPageN int `json:"pending_page_n"`
	FreeAlloc    int `json:"free_alloc_bytes"`
}

type statusError struct {
	Time    time.Time `json:"time"`
	Message string    `json:"message"`
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request, _ *Client) {
	total, err := s.store.Count()
	if err != nil {
		s.writeInternalError(w, err, "status count failed")
		return
	}
	dbStats, err := s.store.Stats()
	if err != nil {
		s.writeInternalError(w, err, "status db stats failed")
		return
	}

	nsSet := map[string]bool{}
	for _, c := range s.auth.Clients() {
		for _, ns := range c.Namespaces {
			nsSet[ns] = true
		}
	}
	namespaces := map[string]int{}
	for ns := range nsSet {
		n, err := s.store.CountPrefix(ns)
		if err != nil {
			s.writeInternalError(w, err, "status namespace count failed")
			return
		}
		namespaces[ns] = n
	}

	clients := make([]statusClient, 0, len(s.auth.Clients()))
	for _, c := range s.auth.Clients() {
		clients = append(clients, statusClient{
			Name:       c.Name,
			Namespaces: c.Namespaces,
			Admin:      c.IsAdmin,
		})
	}

	startedAt := s.status.StartedAt()
	var mem runtime.MemStats
	runtime.ReadMemStats(&mem)
	prewarmOK, prewarmAt, prewarmErr := s.status.Prewarm()
	logsStat := AccessLogStats{}
	if s.logs != nil {
		logsStat = s.logs.Stats()
	}

	resp := statusResponse{
		Version:       version,
		StartedAt:     startedAt,
		UptimeSeconds: int64(time.Since(startedAt).Seconds()),
		Server: statusServer{
			PublicURL:    s.cfg.PublicURL().String(),
			Listeners:    s.Listeners(),
			HTTPEnabled:  s.cfg.Server.HTTPPort > 0,
			HTTPSEnabled: s.cfg.Server.HTTPSPort > 0,
		},
		ACME: statusACME{
			Enabled:          s.cfg.ACME.Enabled,
			DirectoryURL:     s.cfg.ACME.DirectoryURL,
			Staging:          strings.Contains(s.cfg.ACME.DirectoryURL, "staging"),
			Certificate:      s.status.Cert(),
			LastPrewarmOK:    prewarmOK,
			LastPrewarmError: prewarmErr,
		},
		Links: statusLinks{
			Total:      total,
			Namespaces: namespaces,
		},
		Clients:   clients,
		AccessLog: logsStat,
		DB: statusDB{
			FileSizeBytes: dbStats.FileSizeBytes,
			TxStats: statusTxStats{
				TxN:          dbStats.TxN,
				OpenTxN:      dbStats.OpenTxN,
				FreePageN:    dbStats.FreePageN,
				PendingPageN: dbStats.PendingPageN,
				FreeAlloc:    dbStats.FreeAlloc,
			},
		},
		Runtime: statusRuntime{
			Goroutines:       runtime.NumGoroutine(),
			MemoryAllocBytes: mem.Alloc,
		},
	}
	if !prewarmAt.IsZero() {
		t := prewarmAt
		resp.ACME.LastPrewarmAt = &t
	}
	if s.maintenance != nil {
		ms := s.maintenance.Status()
		resp.Maintenance = &ms
	}
	if t, msg := s.status.LastError(); msg != "" {
		resp.LastError = &statusError{Time: t, Message: msg}
	}
	writeJSON(w, http.StatusOK, resp)
}
