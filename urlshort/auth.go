// Copyright (c) 2026 Aaron LI
//
// Bearer-token authentication and namespace authorization.
//
// Plaintext tokens are hashed with SHA-256 at startup. Presented tokens are
// hashed and compared with crypto/subtle without an early exit over clients
// (C12). Tokens and digests are never logged.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"fmt"
	"strings"
)

// Client is one authenticated API caller.
type Client struct {
	Name       string
	IsAdmin    bool
	Namespaces []string // Every namespace ends with a slash '/'.

	// Hash tokens to achieve constant-time comparison for security.
	digests [][sha256.Size]byte
}

// Authenticator holds all enabled clients.
type Authenticator struct {
	clients []*Client
}

// NewAuthenticator builds the digest sets. Disabled clients are skipped.
func NewAuthenticator(cfgs []ClientConfig) (*Authenticator, error) {
	a := &Authenticator{}
	seen := make(map[[sha256.Size]byte]string)
	for i := range cfgs {
		cfg := &cfgs[i]
		if !cfg.Enabled {
			continue
		}
		c := &Client{Name: cfg.Name, IsAdmin: cfg.Admin, Namespaces: cfg.Namespaces}
		for _, tok := range cfg.Tokens {
			d := sha256.Sum256([]byte(tok))
			c.digests = append(c.digests, d)
		}
		for _, d := range c.digests {
			if other, ok := seen[d]; ok {
				return nil, fmt.Errorf("client %q shares a token with client %q",
					cfg.Name, other)
			}
			seen[d] = cfg.Name
		}
		a.clients = append(a.clients, c)
	}
	return a, nil
}

// Authenticate returns the client owning token, if any. It compares against
// every digest of every client without an early exit.
func (a *Authenticator) Authenticate(token string) (*Client, bool) {
	if token == "" {
		return nil, false
	}
	presented := sha256.Sum256([]byte(token))
	var matched *Client
	for _, c := range a.clients {
		for _, d := range c.digests {
			if subtle.ConstantTimeCompare(presented[:], d[:]) == 1 {
				matched = c
			}
		}
	}
	return matched, matched != nil
}

// CanAccess reports whether the client may read or write key.
func (c *Client) CanAccess(key string) bool {
	if c.IsAdmin {
		return true
	}
	for _, ns := range c.Namespaces {
		if strings.HasPrefix(key, ns) {
			return true
		}
	}
	return false
}

// FirstNamespace returns the first configured namespace, used as the prefix for
// random fallback keys. It returns "" for a client without namespaces.
func (c *Client) FirstNamespace() string {
	if len(c.Namespaces) == 0 {
		if c.IsAdmin {
			// Admin clients fall back to the root.
			return "/"
		}
		return ""
	}
	return c.Namespaces[0]
}

// Clients returns the enabled clients (for the status endpoint).
func (a *Authenticator) Clients() []*Client {
	return a.clients
}
