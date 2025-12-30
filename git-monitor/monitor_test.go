// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Git monitor tests.
//

package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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

//------------------------------------------------------------------------------

type testPoster struct {
	mu       sync.Mutex
	maxLen   int
	messages []string
}

func newTestPoster(maxLen int) *testPoster {
	return &testPoster{
		maxLen:   maxLen,
		messages: []string{},
	}
}

func (p *testPoster) GetMaxLength() int {
	return p.maxLen
}

func (p *testPoster) Post(ctx context.Context, text string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.messages = append(p.messages, text)
	return nil
}

func (p *testPoster) Messages() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	cp := make([]string, len(p.messages))
	copy(cp, p.messages)
	return cp
}

func TestSendAnnouncements_Empty(t *testing.T) {
	ctx := context.Background()
	poster := newTestPoster(512)

	m := &Monitor{
		Name:   "test",
		Poster: poster,
	}

	m.sendAnnouncements(ctx, nil)

	if len(poster.Messages()) != 0 {
		t.Fatalf("expected no messages, got %v", poster.Messages())
	}
}

func TestSendAnnouncements_Basic(t *testing.T) {
	ctx := context.Background()
	poster := newTestPoster(1024)

	m := &Monitor{
		Name:   "testproj",
		Poster: poster,
	}

	ans := []*announcement{
		// New tag
		{
			tag: "v1.0.0",
			info: &commitInfo{
				Hash:        "aaaaaaaaaa",
				AuthorName:  "Alice",
				AuthorEmail: "alice@example.com",
				Subject:     "release v1.0.0",
			},
		},
		// Updated tag
		{
			tag:        "v1.0.1",
			tagUpdated: true,
			info: &commitInfo{
				Hash:        "bbbbbbbbbb",
				AuthorName:  "Bob",
				AuthorEmail: "bob@example.com",
				Subject:     "retag v1.0.1",
			},
		},
		// Normal commit on master
		{
			branch: "master",
			info: &commitInfo{
				Hash:        "cccccccccc",
				AuthorName:  "Carol",
				AuthorEmail: "carol@example.com",
				Subject:     "fix bug",
			},
		},
		// Merge commit on master
		{
			branch:  "master",
			isMerge: true,
			info: &commitInfo{
				Hash:        "dddddddddd",
				AuthorName:  "Dave",
				AuthorEmail: "dave@example.com",
				Subject:     "merge feature",
			},
		},
		// Commits on develop
		{
			branch: "develop",
			info: &commitInfo{
				Hash:        "eeeeeeeeee",
				AuthorName:  "Eve",
				AuthorEmail: "eve@example.com",
				Subject:     "add feature",
			},
		},
		{
			branch: "develop",
			info: &commitInfo{
				Hash:        "ffffffffff",
				AuthorName:  "Foo",
				AuthorEmail: "foo@example.com",
				Subject:     "add feature 2",
			},
		},
	}

	m.sendAnnouncements(ctx, ans)

	msgs := poster.Messages()
	if len(msgs) != 4 {
		t.Fatalf("expected 4 messages, got %d:\n%v", len(msgs), msgs)
	}

	msgTag1, msgTag2, msgDevelop, msgMaster := msgs[0], msgs[1], msgs[2], msgs[3]

	// Tags first
	if !strings.Contains(msgTag1, "[testproj] tag:v1.0.0") {
		t.Errorf("unexpected tag msg: %s", msgTag1)
	}
	if !strings.Contains(msgTag2, "tag:v1.0.1 (updated)") {
		t.Errorf("expected updated tag msg, got: %s", msgTag2)
	}

	// Branch ordering: develop, master
	if !strings.HasPrefix(msgDevelop, "[testproj:develop]") {
		t.Errorf("expected develop branch msg, got: %s", msgDevelop)
	}
	if !strings.HasPrefix(msgMaster, "[testproj:master]") {
		t.Errorf("expected master branch msg, got: %s", msgMaster)
	}

	// Merge commit annotation
	if !strings.Contains(msgMaster, "(merge)") {
		t.Errorf("expected merge annotation, got: %s", msgMaster)
	}
}

func TestSendAnnouncements_Batching(t *testing.T) {
	ctx := context.Background()
	poster := newTestPoster(80) // Small max length to force batching

	m := &Monitor{
		Name:   "test",
		Poster: poster,
	}

	var ans []*announcement
	for i := 0; i < 5; i++ {
		ans = append(ans, &announcement{
			branch: "master",
			info: &commitInfo{
				Hash:        fmt.Sprintf("hash%d", i),
				AuthorName:  "Author",
				AuthorEmail: "a@example.com",
				Subject:     fmt.Sprintf("commit number %d", i),
			},
		})
	}

	m.sendAnnouncements(ctx, ans)

	msgs := poster.Messages()
	if len(msgs) < 2 {
		t.Fatalf("expected batching, got %d messages", len(msgs))
	}

	for _, msg := range msgs {
		if len([]byte(msg)) > poster.GetMaxLength() {
			t.Errorf("message exceeds max length: %d > %d\n%s",
				len([]byte(msg)), poster.GetMaxLength(), msg)
		}
	}
}
