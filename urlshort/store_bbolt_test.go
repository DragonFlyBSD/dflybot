// Copyright (c) 2026 Aaron LI
//
// bbolt store tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func newTestStore(t *testing.T) *BoltStore {
	t.Helper()
	path := filepath.Join(t.TempDir(), "links.db")
	s, err := OpenBoltStore(path, 1<<20)
	if err != nil {
		t.Fatalf("OpenBoltStore: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func TestStoreCRUD(t *testing.T) {
	s := newTestStore(t)

	link, created, err := s.Create("https://example.com/a", "rule-a", "owner", "/a/1", nil)
	if err != nil || !created {
		t.Fatalf("Create: created=%v err=%v", created, err)
	}
	if link.Target != "https://example.com/a" || link.Rule != "rule-a" || link.Owner != "owner" {
		t.Fatalf("unexpected link: %+v", link)
	}

	got, err := s.Get("/a/1")
	if err != nil || got.Target != link.Target {
		t.Fatalf("Get: %+v err=%v", got, err)
	}
	key, err := s.Resolve("https://example.com/a")
	if err != nil || key != "/a/1" {
		t.Fatalf("Resolve: %q err=%v", key, err)
	}

	// Idempotent create returns the existing link.
	again, created2, err := s.Create("https://example.com/a", "rule-a", "owner", "/a/1", nil)
	if err != nil || created2 || again.Target != link.Target {
		t.Fatalf("idempotent Create: %+v created=%v err=%v", again, created2, err)
	}

	// Retarget.
	up, err := s.Update("/a/1", "https://example.com/b")
	if err != nil || up.Target != "https://example.com/b" {
		t.Fatalf("Update: %+v err=%v", up, err)
	}
	if _, err := s.Resolve("https://example.com/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target should be gone, got %v", err)
	}
	if k, _ := s.Resolve("https://example.com/b"); k != "/a/1" {
		t.Fatalf("new target resolve = %q", k)
	}

	// Delete frees both sides.
	if err := s.Delete("/a/1"); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := s.Get("/a/1"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete: %v", err)
	}
	if _, err := s.Resolve("https://example.com/b"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Resolve after delete: %v", err)
	}
}

func TestStoreUpdateIdempotent(t *testing.T) {
	s := newTestStore(t)
	base := time.Unix(1700000000, 0)
	calls := 0
	s.now = func() time.Time {
		calls++
		return base.Add(time.Duration(calls) * time.Second)
	}

	link, _, err := s.Create("https://example.com/a", "", "", "/a/1", nil)
	if err != nil {
		t.Fatal(err)
	}
	created := link.UpdatedAt
	afterCreate := calls

	// Updating to the same target is a no-op: no write, no updated_at bump,
	// and the clock is not consulted.
	again, err := s.Update("/a/1", "https://example.com/a")
	if err != nil {
		t.Fatal(err)
	}
	if again.Target != "https://example.com/a" || !again.CreatedAt.Equal(link.CreatedAt) {
		t.Fatalf("no-op update = %+v", again)
	}
	if !again.UpdatedAt.Equal(created) {
		t.Fatalf("updated_at changed on no-op: %v -> %v", created, again.UpdatedAt)
	}
	if calls != afterCreate {
		t.Fatalf("no-op update consulted the clock")
	}

	// A real retarget bumps updated_at and re-points the target bucket.
	changed, err := s.Update("/a/1", "https://example.com/b")
	if err != nil {
		t.Fatal(err)
	}
	if !changed.UpdatedAt.After(created) {
		t.Fatalf("updated_at not bumped on retarget: %v", changed.UpdatedAt)
	}
	if _, err := s.Resolve("https://example.com/a"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old target still resolves: %v", err)
	}
}

func TestStoreConflict(t *testing.T) {
	s := newTestStore(t)
	if _, _, err := s.Create("https://example.com/a", "", "", "/a/1", nil); err != nil {
		t.Fatal(err)
	}
	// Same key, different target.
	if _, _, err := s.Create("https://example.com/b", "", "", "/a/1", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// Same target, different explicit key.
	if _, _, err := s.Create("https://example.com/a", "", "", "/a/2", nil); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	// Update to an occupied target.
	if _, _, err := s.Create("https://example.com/c", "", "", "/a/3", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update("/a/3", "https://example.com/a"); !errors.Is(err, ErrConflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
}

func TestStoreGenerateWithExtension(t *testing.T) {
	s := newTestStore(t)
	// Occupy /g/d/728aaaa.
	if _, _, err := s.Create("https://x/1", "", "", "/g/d/728aaaa", nil); err != nil {
		t.Fatal(err)
	}
	// Generator extends the hash until free.
	gen := func(isFree func(string) (bool, error)) (string, error) {
		full := "728aaaa0111222333444555666777888999aabbcc"
		for l := 8; l <= len(full); l++ {
			cand := "/g/d/" + full[:l]
			free, err := isFree(cand)
			if err != nil {
				return "", err
			}
			if free {
				return cand, nil
			}
		}
		return "", fmt.Errorf("%w: no unique hash prefix", ErrConflict)
	}
	link, created, err := s.Create("https://x/2", "r", "", "", gen)
	if err != nil || !created {
		t.Fatalf("Create: %+v created=%v err=%v", link, created, err)
	}
	if link.Target != "https://x/2" {
		t.Fatalf("unexpected link %+v", link)
	}
	if key, _ := s.Resolve("https://x/2"); key != "/g/d/728aaaa0" {
		t.Fatalf("resolved key = %q, want /g/d/728aaaa0", key)
	}
}

func TestStoreConcurrentCreate(t *testing.T) {
	s := newTestStore(t)
	const n = 16
	var wg sync.WaitGroup
	keys := make([]string, n)
	created := make([]bool, n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			l, c, err := s.Create("https://same/target", "r", "o", "/s/1", nil)
			if l != nil {
				keys[i] = l.Target
			}
			created[i] = c
			errs[i] = err
		}(i)
	}
	wg.Wait()
	creates := 0
	for i := 0; i < n; i++ {
		if errs[i] != nil {
			t.Fatalf("goroutine %d: %v", i, errs[i])
		}
		if created[i] {
			creates++
		}
	}
	if creates != 1 {
		t.Fatalf("expected exactly one create, got %d", creates)
	}
	if n, _ := s.Count(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
}

func TestStoreListPagination(t *testing.T) {
	s := newTestStore(t)
	total := 25
	for i := 0; i < total; i++ {
		key := fmt.Sprintf("/g/x/%02d", i)
		if _, _, err := s.Create(fmt.Sprintf("https://x/%d", i), "", "", key, nil); err != nil {
			t.Fatal(err)
		}
	}
	// Unrelated prefix.
	if _, _, err := s.Create("https://y/1", "", "", "/other/1", nil); err != nil {
		t.Fatal(err)
	}

	seen := 0
	cursor := ""
	for {
		links, next, err := s.List("/g/", 10, cursor)
		if err != nil {
			t.Fatal(err)
		}
		seen += len(links)
		if next == "" {
			break
		}
		cursor = next
	}
	if seen != total {
		t.Fatalf("listed %d links, want %d", seen, total)
	}

	n, err := s.CountPrefix("/g/")
	if err != nil || n != total {
		t.Fatalf("CountPrefix = %d err=%v", n, err)
	}
	if n, _ := s.Count(); n != total+1 {
		t.Fatalf("Count = %d, want %d", n, total+1)
	}

	// A prefix whose keys do not sort first must still be found (regression:
	// both List and CountPrefix started at the bucket's first key).
	if n, err := s.CountPrefix("/other/"); err != nil || n != 1 {
		t.Fatalf("CountPrefix(/other/) = %d err=%v, want 1", n, err)
	}
	if links, _, err := s.List("/other/", 10, ""); err != nil || len(links) != 1 {
		t.Fatalf("List(/other/) = %d err=%v, want 1", len(links), err)
	}
	if links, _, err := s.List("/missing/", 10, ""); err != nil || len(links) != 0 {
		t.Fatalf("List(/missing/) = %d err=%v, want 0", len(links), err)
	}

	// Limit cap.
	links, _, err := s.List("/g/", 5000, "")
	if err != nil || len(links) != total {
		t.Fatalf("List with big limit: %d err=%v", len(links), err)
	}
}

func TestStoreCompactTo(t *testing.T) {
	s := newTestStore(t)
	for i := 0; i < 200; i++ {
		if _, _, err := s.Create(fmt.Sprintf("https://x/%d", i), "", "", fmt.Sprintf("/g/%d", i), nil); err != nil {
			t.Fatal(err)
		}
	}
	// Delete many to create free pages.
	for i := 0; i < 150; i++ {
		if err := s.Delete(fmt.Sprintf("/g/%d", i)); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.Stats()
	if err != nil {
		t.Fatal(err)
	}

	backup := filepath.Join(t.TempDir(), "links-backup.db")
	if err := s.CompactTo(backup); err != nil {
		t.Fatalf("CompactTo: %v", err)
	}
	linksN, targetsN, err := verifyBoltFile(backup)
	if err != nil {
		t.Fatalf("verifyBoltFile: %v", err)
	}
	if linksN != 50 || targetsN != 50 {
		t.Fatalf("verify counts links=%d targets=%d, want 50", linksN, targetsN)
	}
	if fi, err := os.Stat(backup); err != nil || fi.Size() >= before.FileSizeBytes {
		t.Fatalf("compacted size %v not smaller than live %d (err=%v)", fi, before.FileSizeBytes, err)
	}
}
