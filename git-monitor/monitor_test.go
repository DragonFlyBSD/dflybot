// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Git monitor tests.
//

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newTestRepo(t *testing.T) string {
	t.Helper()

	dir := t.TempDir()

	cmds := [][]string{
		{"git", "init", "-b", "main"},
		{"git", "config", "user.name", "Test User"},
		{"git", "config", "user.email", "test@example.com"},
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git command (%s) failed: %v (%s)", cmd.String(), err, out)
		}
	}

	return dir
}

// Helper: commit file with message
func gitCommit(t *testing.T, dir string, msg string) string {
	t.Helper()

	file := filepath.Join(dir, "file.txt")
	err := os.WriteFile(file, []byte(msg+time.Now().String()), 0644)
	if err != nil {
		t.Fatalf("failed to write file (%s): %v", file, err)
	}

	cmds := [][]string{
		{"git", "add", "."},
		{"git", "commit", "-m", msg},
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git command (%s) failed: %v (%s)", cmd.String(), err, out)
		}
	}

	out, _ := exec.Command("git", "-C", dir, "rev-parse", "HEAD").Output()
	return strings.TrimSpace(string(out))
}

// Helper: create merge commit
func gitMerge(t *testing.T, dir string) {
	t.Helper()

	cmds := [][]string{
		{"git", "checkout", "-b", "feature"},
		{"git", "commit", "--allow-empty", "-m", "feature commit"},
		{"git", "checkout", "main"},
		{"git", "merge", "--no-ff", "feature", "-m", "merge feature"},
	}
	for _, c := range cmds {
		cmd := exec.Command(c[0], c[1:]...)
		cmd.Dir = dir
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git command (%s) failed: %v (%s)", cmd.String(), err, out)
		}
	}
}

// Helper: tag the given commit
func gitTag(t *testing.T, dir string, tag string, ref string) {
	t.Helper()

	if ref == "" {
		ref = "HEAD"
	}
	cmd := exec.Command("git", "tag", "-f", tag, ref)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git command (%s) failed: %v (%s)", cmd.String(), err, out)
	}
}

// Test: load/save state round-trip
func TestStateSaveLoad(t *testing.T) {
	dir := t.TempDir()
	statePath := filepath.Join(dir, "state.json")
	state := State{
		LastSeen: map[string]string{"branch:main": "abc"},
		SeenTags: map[string]string{"v1.0": "def"},
	}

	m := &Monitor{
		Name:      "test",
		StatePath: statePath,
		state:     state,
	}
	if err := m.saveState(); err != nil {
		t.Fatal(err)
	}

	m2 := &Monitor{StatePath: statePath}
	if err := m2.loadState(); err != nil {
		t.Fatal(err)
	}

	for k, v := range state.LastSeen {
		if v2 := m2.state.LastSeen[k]; v2 != v {
			t.Fatalf("LastSeen[%q] mismatch, expected %q, got %q", k, v, v2)
		}
	}
	for k, v := range state.SeenTags {
		if v2 := m2.state.SeenTags[k]; v2 != v {
			t.Fatalf("SeenTags[%q] mismatch, expected %q, got %q", k, v, v2)
		}
	}
}

// Test: seedState initializes branches and tags
func TestSeedState(t *testing.T) {
	repo := newTestRepo(t)
	gitCommit(t, repo, "initial")

	m := &Monitor{
		Name:    "test",
		RepoDir: repo + "/.git",
		state: State{
			LastSeen: map[string]string{},
			SeenTags: map[string]string{},
		},
	}

	if err := m.seedState(); err != nil {
		t.Fatal(err)
	}

	if len(m.state.LastSeen) == 0 {
		t.Fatalf("expected branches seeded")
	}
}

// Test: collect new commits
func TestCommitAnnouncement(t *testing.T) {
	repo := newTestRepo(t)
	first := gitCommit(t, repo, "first")

	m := &Monitor{
		Name:    "test",
		RepoDir: repo + "/.git",
		state: State{
			LastSeen: map[string]string{"branch:main": first},
			SeenTags: map[string]string{},
		},
	}

	second := gitCommit(t, repo, "second")

	ans := m.collectAnnouncements()
	if len(ans) != 1 {
		t.Fatalf("expected 1 announcement, got %d", len(ans))
	}
	if ans[0].info.Subject != "second" {
		t.Fatalf("wrong commit subject")
	}

	if m.state.LastSeen["branch:main"] != second {
		t.Fatalf("LastSeen mismatch")
	}
}

// Test: detect merge commit
func TestMergeCommit(t *testing.T) {
	repo := newTestRepo(t)
	base := gitCommit(t, repo, "base")

	m := &Monitor{
		Name:    "test",
		RepoDir: repo + "/.git",
		state: State{
			LastSeen: map[string]string{"branch:main": base},
			SeenTags: map[string]string{},
		},
	}

	gitMerge(t, repo)

	ans := m.collectAnnouncements()
	foundMerge := false
	for _, a := range ans {
		if a.isMerge {
			foundMerge = true
		}
	}
	if !foundMerge {
		t.Fatalf("expected merge commit")
	}
}

// Test: collect tag announcements
func TestTagAnnouncement(t *testing.T) {
	repo := newTestRepo(t)
	tag := "v1.0"

	sha := gitCommit(t, repo, "release")
	gitTag(t, repo, tag, sha)

	m := &Monitor{
		Name:    "test",
		RepoDir: repo + "/.git",
		state: State{
			LastSeen: map[string]string{},
			SeenTags: map[string]string{},
		},
	}

	ans := m.collectAnnouncements()
	if len(ans) != 1 {
		t.Fatalf("expected 1 announcement, got %d", len(ans))
	}
	if ans[0].tag != tag {
		t.Fatalf("expected announcement for tag %q, got %q", tag, ans[0].tag)
	}
	if ans[0].info.Hash != sha {
		t.Fatalf("tag points to wrong commit %q, expected %q", ans[0].info.Hash, sha)
	}
}

// Test: tag update to another commit
func TestTagUpdate(t *testing.T) {
	repo := newTestRepo(t)
	tag := "v1.0"

	sha := gitCommit(t, repo, "release")
	gitTag(t, repo, tag, sha)

	m := &Monitor{
		Name:    "test",
		RepoDir: repo + "/.git",
		state: State{
			LastSeen: map[string]string{},
			SeenTags: map[string]string{},
		},
	}

	m.collectAnnouncements()

	sha = gitCommit(t, repo, "new release")

	m.collectAnnouncements()

	gitTag(t, repo, tag, sha)

	ans := m.collectAnnouncements()
	if len(ans) != 1 {
		t.Fatalf("expected 1 announcement, got %d", len(ans))
	}
	if ans[0].tag != tag {
		t.Fatalf("expected announcement for tag %q, got %q", tag, ans[0].tag)
	}
	if !ans[0].tagUpdated {
		t.Fatalf("expected tag updated")
	}
	if ans[0].info.Hash != sha {
		t.Fatalf("tag points to wrong commit %q, expected %q", ans[0].info.Hash, sha)
	}
}
