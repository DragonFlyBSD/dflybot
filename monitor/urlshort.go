// Copyright (c) 2026 Aaron LI
//
// URL shortener client: resolve a target URL to its shortened form through
// the urlshort REST API.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package monitor

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ConfigURLShort is the "urlshort" section of the monitor config files.
type ConfigURLShort struct {
	// API is the base URL of the URL shortener API, including the version
	// prefix, e.g. "https://example.com/.api/v1".
	API string `toml:"api"`
	// Token is the bearer token used to authenticate with the API.
	Token string `toml:"token"`
	// Timeout is the total HTTP request timeout in seconds (default 5).
	Timeout int `toml:"timeout"`
}

// Shortener resolves a target URL to its shortened form.
type Shortener interface {
	Shorten(ctx context.Context, target string) (string, error)
	// ShortenBatch resolves several targets in one request. The returned map
	// holds the targets that succeeded; missing targets should fall back to
	// their full URL. A non-nil error reports a request-level failure, and the
	// map may still contain earlier successes.
	ShortenBatch(ctx context.Context, targets []string) (map[string]string, error)
}

// URLShortener is the urlshort API implementation of Shortener.
type URLShortener struct {
	api    string
	token  string
	client *http.Client
}

const (
	defaultURLShortTimeout = 5 * time.Second
	urlShortMaxBody        = 64 * 1024
	urlShortBatchMaxBody   = 1024 * 1024
	urlShortBatchSize      = 100
)

type shortBatchRequest struct {
	Items []shortBatchItem `json:"items"`
}

type shortBatchItem struct {
	Target string `json:"target"`
}

type shortBatchResponse struct {
	Results []shortBatchResult `json:"results"`
}

type shortBatchResult struct {
	Link *struct {
		ShortURL string `json:"short_url"`
	} `json:"link"`
}

// NewURLShortener validates cfg and returns a URL shortener client.
func NewURLShortener(cfg *ConfigURLShort) (*URLShortener, error) {
	if cfg == nil {
		return nil, fmt.Errorf("urlshort: missing config")
	}
	api := strings.TrimRight(strings.TrimSpace(cfg.API), "/")
	u, err := url.Parse(api)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, fmt.Errorf("urlshort: invalid api URL %q", cfg.API)
	}
	if strings.TrimSpace(cfg.Token) == "" {
		return nil, fmt.Errorf("urlshort: token is required")
	}
	timeout := defaultURLShortTimeout
	if cfg.Timeout > 0 {
		timeout = time.Duration(cfg.Timeout) * time.Second
	}
	return &URLShortener{
		api:    api,
		token:  cfg.Token,
		client: &http.Client{Timeout: timeout},
	}, nil
}

// Shorten returns the short URL for target.  It sends one POST request to the
// API's /links endpoint and returns the response's short_url.
func (s *URLShortener) Shorten(ctx context.Context, target string) (string, error) {
	payload, err := json.Marshal(struct {
		Target string `json:"target"`
	}{target})
	if err != nil {
		return "", fmt.Errorf("urlshort: marshal request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/links",
		bytes.NewReader(payload))
	if err != nil {
		return "", fmt.Errorf("urlshort: create request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return "", fmt.Errorf("urlshort: request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, urlShortMaxBody))
	if err != nil {
		return "", fmt.Errorf("urlshort: read response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return "", fmt.Errorf("urlshort: %s", apiErrorMessage(resp.StatusCode, body))
	}

	var out struct {
		ShortURL string `json:"short_url"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return "", fmt.Errorf("urlshort: decode response: %w", err)
	}
	if out.ShortURL == "" {
		return "", fmt.Errorf("urlshort: response missing short_url")
	}
	return out.ShortURL, nil
}

// ShortenBatch resolves several targets in as few requests as possible. It
// returns the map of shortened targets and a request-level error, if any; the
// map still holds the targets that did succeed.
func (s *URLShortener) ShortenBatch(ctx context.Context, targets []string) (map[string]string, error) {
	short := make(map[string]string, len(targets))
	var firstErr error
	for start := 0; start < len(targets); start += urlShortBatchSize {
		end := start + urlShortBatchSize
		if end > len(targets) {
			end = len(targets)
		}
		chunk, err := s.shortenBatch(ctx, targets[start:end])
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		for target, url := range chunk {
			short[target] = url
		}
	}
	return short, firstErr
}

func (s *URLShortener) shortenBatch(ctx context.Context, targets []string) (map[string]string, error) {
	reqBody := shortBatchRequest{Items: make([]shortBatchItem, len(targets))}
	for i, target := range targets {
		reqBody.Items[i].Target = target
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("urlshort: marshal batch request: %w", err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.api+"/links/batch",
		bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("urlshort: create batch request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+s.token)

	resp, err := s.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("urlshort: batch request failed: %w", err)
	}
	defer resp.Body.Close()

	body, err := io.ReadAll(io.LimitReader(resp.Body, urlShortBatchMaxBody))
	if err != nil {
		return nil, fmt.Errorf("urlshort: read batch response: %w", err)
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("urlshort: %s", apiErrorMessage(resp.StatusCode, body))
	}

	var out shortBatchResponse
	if err := json.Unmarshal(body, &out); err != nil {
		return nil, fmt.Errorf("urlshort: decode batch response: %w", err)
	}

	// Results are in request order, so pair them with the input slice rather
	// than the (canonicalized) target echoed by the server.
	short := make(map[string]string, len(targets))
	for i, res := range out.Results {
		if i >= len(targets) {
			break
		}
		if res.Link != nil && res.Link.ShortURL != "" {
			short[targets[i]] = res.Link.ShortURL
		}
	}
	return short, nil
}

func apiErrorMessage(status int, body []byte) string {
	var e struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(body, &e); err == nil &&
		(e.Error.Code != "" || e.Error.Message != "") {
		return fmt.Sprintf("http status %d: %s: %s", status, e.Error.Code, e.Error.Message)
	}
	return fmt.Sprintf("http status %d", status)
}
