// Copyright (c) 2026 Aaron LI
//
// Authentication and namespace tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"
)

func testAuthenticator(t *testing.T) *Authenticator {
	t.Helper()
	digest := sha256.Sum256([]byte("digest-token-value-1234567890"))
	a, err := NewAuthenticator([]ClientConfig{
		{
			Enabled:    true,
			Name:       "git",
			Namespaces: []string{"/g/"},
			// Rotation: two plaintext tokens for one client.
			Tokens: []string{"plain-token-value-1234567890ab", "rotated-token-value-1234567890"},
		},
		{
			Enabled:      true,
			Name:         "git2",
			Namespaces:   []string{"/git/"},
			TokensSHA256: []string{hex.EncodeToString(digest[:])},
		},
		{
			Enabled: true,
			Name:    "admin",
			Admin:   true,
			Tokens:  []string{"admin-token-value-1234567890abc"},
		},
		{
			Enabled:    false,
			Name:       "disabled",
			Namespaces: []string{"/x/"},
			Tokens:     []string{"disabled-token-1234567890abcdef"},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

func TestAuthenticate(t *testing.T) {
	a := testAuthenticator(t)

	c, ok := a.Authenticate("plain-token-value-1234567890ab")
	if !ok || c.Name != "git" {
		t.Fatalf("plaintext auth: %v %v", c, ok)
	}
	if c2, ok := a.Authenticate("rotated-token-value-1234567890"); !ok || c2.Name != "git" {
		t.Fatalf("rotated token auth: %v %v", c2, ok)
	}
	c, ok = a.Authenticate("digest-token-value-1234567890")
	if !ok || c.Name != "git2" {
		t.Fatalf("digest auth: %v %v", c, ok)
	}
	c, ok = a.Authenticate("admin-token-value-1234567890abc")
	if !ok || !c.Admin {
		t.Fatalf("admin auth: %v %v", c, ok)
	}
	if _, ok := a.Authenticate("wrong-token"); ok {
		t.Fatal("wrong token accepted")
	}
	if _, ok := a.Authenticate(""); ok {
		t.Fatal("empty token accepted")
	}
	if _, ok := a.Authenticate("disabled-token-1234567890abcdef"); ok {
		t.Fatal("disabled client accepted")
	}
	if len(a.Clients()) != 3 {
		t.Fatalf("enabled clients = %d, want 3", len(a.Clients()))
	}
}

func TestNamespaceBoundary(t *testing.T) {
	a := testAuthenticator(t)
	git, _ := a.Authenticate("plain-token-value-1234567890ab")
	if !git.CanAccess("/g/d/123") {
		t.Error("/g/d/123 should be allowed for /g/")
	}
	if git.CanAccess("/git/repo") {
		t.Error("/git/repo must not be allowed for /g/ (prefix must include the slash)")
	}
	if git.CanAccess("/gh/x") {
		t.Error("/gh/x must not be allowed for /g/")
	}
	admin, _ := a.Authenticate("admin-token-value-1234567890abc")
	if !admin.CanAccess("/anything/at/all") {
		t.Error("admin should access any key")
	}
	if got := git.FirstNamespace(); got != "/g/" {
		t.Errorf("FirstNamespace = %q", got)
	}
}

func TestSharedTokenRejected(t *testing.T) {
	tok := strings.Repeat("z", 32)
	_, err := NewAuthenticator([]ClientConfig{
		{Enabled: true, Name: "a", Admin: true, Tokens: []string{tok}},
		{Enabled: true, Name: "b", Admin: true, Tokens: []string{tok}},
	})
	if err == nil || !strings.Contains(err.Error(), "shares a token") {
		t.Fatalf("expected shared-token error, got %v", err)
	}
}
