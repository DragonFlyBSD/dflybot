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
)

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
