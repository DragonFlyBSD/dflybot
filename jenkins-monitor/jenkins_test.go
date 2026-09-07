// Copyright (c) 2026 Aaron LI
//
// Unit tests for the Jenkins REST client request building and auth.
//
// Co-authored-by: DeepSeek-v4-flash (with Pi Coding Agent)
//

package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestJenkinsClientRequests(t *testing.T) {
	var gotPath, gotAuth string
	var gotTree []string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotTree = r.URL.Query()["tree"]
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(struct {
			Color string `json:"color"`
		}{"blue"})
	}))
	defer ts.Close()

	cfg := &ConfigJenkins{
		Name:     "dragonfly",
		URL:      ts.URL,
		User:     "aly",
		APIToken: "secret",
		Jobs:     []string{"DragonFlyBSD"},
	}
	c := newJenkinsClient(cfg)

	if _, err := c.lastCompleted("Foo Bar"); err != nil {
		t.Fatal(err)
	}
	if gotPath != "/job/Foo Bar/api/json" {
		t.Errorf("path = %q", gotPath)
	}
	if len(gotTree) != 1 ||
		!strings.Contains(gotTree[0], "lastCompletedBuild[number,result,timestamp,url]") {
		t.Errorf("tree = %v", gotTree)
	}
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Errorf("no basic auth header: %q", gotAuth)
	}

	// No credentials configured -> no Authorization header.
	c2 := newJenkinsClient(&ConfigJenkins{URL: ts.URL, Jobs: []string{"X"}})
	if _, err := c2.lastCompleted("X"); err != nil {
		t.Fatal(err)
	}
	if gotAuth != "" {
		t.Errorf("unexpected auth header: %q", gotAuth)
	}
}

func TestJenkinsClientNotFound(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer ts.Close()

	c := newJenkinsClient(&ConfigJenkins{URL: ts.URL, Jobs: []string{"X"}})
	if _, err := c.lastCompleted("X"); err != errNotFound {
		t.Errorf("err = %v, want errNotFound", err)
	}
	if _, err := c.computers(); err != errNotFound {
		t.Errorf("err = %v, want errNotFound", err)
	}
}
