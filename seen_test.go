// Copyright (c) 2026 Aaron LI
//
// Unit tests for the seen database and the "!seen" query resolution
// (SeenStore.Lookup / SeenResult.Text).  These exercise the store and the
// pure lookup/formatting logic only; no IRC server or goirc connection is
// involved.
//
// Co-authored-by: Deepseek-v4-flash (wit Pi Coding Agent)
//

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// seenTime returns a deterministic time relative to a fixed epoch.
func seenTime(offset int64) time.Time {
	return time.Unix(1700000000+offset, 0).UTC()
}

func newTestStore(t *testing.T, dir string) *SeenStore {
	t.Helper()
	s, err := NewSeenStore(dir, time.Hour)
	if err != nil {
		t.Fatalf("NewSeenStore() error: %v", err)
	}
	return s
}

// seenPopulatedStore builds a store with a known set of records in #ch:
//
//	Aly    join=+10 message=+20
//	bob_   join=+10 exit=+50
//	bob__  exit=+80
//	bobz   exit=+5
//	carol  join=+30
func seenPopulatedStore(t *testing.T) *SeenStore {
	t.Helper()
	s := newTestStore(t, t.TempDir())
	s.Join("#ch", "Aly", seenTime(10))
	s.Message("#ch", "Aly", seenTime(20))
	s.Join("#ch", "bob_", seenTime(10))
	s.Leave("#ch", "bob_", seenTime(50))
	s.Leave("#ch", "bob__", seenTime(80))
	s.Leave("#ch", "bobz", seenTime(5))
	s.Join("#ch", "carol", seenTime(30))
	return s
}

func TestSeenStorePersist(t *testing.T) {
	dir := t.TempDir()
	s := newTestStore(t, dir)

	s.Join("#ch", "Aly", seenTime(10))
	s.Message("#ch", "Aly", seenTime(20))
	s.Leave("#ch", "Aly", seenTime(30))
	s.Stop()

	// The channel database was written out.
	path := filepath.Join(dir, "#ch", "seen.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("seen.json not written: %v", err)
	}
	for _, want := range []string{`"version": 1`, `"channel": "#ch"`, `"Aly"`} {
		if !strings.Contains(string(b), want) {
			t.Errorf("seen.json missing %q: %s", want, b)
		}
	}
	// No leftover temp files.
	entries, err := os.ReadDir(filepath.Join(dir, "#ch"))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "seen.json" {
		t.Errorf("unexpected files in channel dir: %v", entries)
	}

	// Reload from disk and verify the records survive.
	s2 := newTestStore(t, dir)
	defer s2.Stop()
	e, ok := s2.Get("#ch", "aly") // case-insensitive
	if !ok {
		t.Fatal("Get(#ch, aly) not found")
	}
	if e.Join != seenTime(10).Unix() || e.Exit != seenTime(30).Unix() || e.Message != seenTime(20).Unix() {
		t.Errorf("Get() = %+v", e)
	}
}

func TestSeenStoreJoinLeaveRejoin(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer s.Stop()

	s.Join("#ch", "Bob", seenTime(10))
	s.Leave("#ch", "Bob", seenTime(20))
	s.Join("#ch", "Bob", seenTime(30))

	e, _ := s.Get("#ch", "Bob")
	if e.Join != seenTime(30).Unix() || e.Exit != seenTime(20).Unix() || e.Message != 0 {
		t.Errorf("record = %+v", e)
	}
}

func TestSeenStoreQuitAcrossChannels(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer s.Stop()

	s.Join("#a", "Bob", seenTime(10))
	s.Join("#b", "Bob", seenTime(10))
	s.Message("#a", "Carol", seenTime(10))
	s.Quit("Bob", seenTime(40))

	for _, ch := range []string{"#a", "#b"} {
		e, ok := s.Get(ch, "Bob")
		if !ok || e.Exit != seenTime(40).Unix() {
			t.Errorf("channel %s: record = %+v, ok = %v", ch, e, ok)
		}
	}
	// Carol (different nick) must be untouched.
	e, _ := s.Get("#a", "Carol")
	if e.Exit != 0 {
		t.Errorf("Carol exit should stay 0, got %d", e.Exit)
	}
}

func TestSeenStoreRenamePreservesHistory(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer s.Stop()

	s.Join("#ch", "Bob", seenTime(10))
	s.Message("#ch", "Bob", seenTime(20))
	s.Rename("Bob", "Bob_")

	if _, ok := s.Get("#ch", "Bob"); ok {
		t.Error("old nick still present after rename")
	}
	e, ok := s.Get("#ch", "bob_")
	if !ok || e.Join != seenTime(10).Unix() || e.Message != seenTime(20).Unix() {
		t.Errorf("new nick record = %+v, ok = %v", e, ok)
	}
}

func TestSeenStoreNicksSnapshotIsACopy(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer s.Stop()

	s.Join("#ch", "Bob", seenTime(10))
	nicks := s.Nicks("#ch")
	nicks["Bob"] = SeenEntry{Join: 999} // mutate the copy
	e, ok := s.Get("#ch", "Bob")
	if !ok || e.Join != seenTime(10).Unix() {
		t.Errorf("store mutated via snapshot: %+v", e)
	}
	if _, ok := s.Nicks("#nope")["x"]; ok {
		t.Error("unknown channel returned nicks")
	}
}

func TestSeenStoreCorruptFileLoadsEmpty(t *testing.T) {
	dir := t.TempDir()
	chDir := filepath.Join(dir, "#ch")
	if err := os.MkdirAll(chDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(chDir, "seen.json"), []byte("{oops"), 0o644); err != nil {
		t.Fatal(err)
	}

	s := newTestStore(t, dir) // must not fail
	defer s.Stop()
	if _, ok := s.Get("#ch", "x"); ok {
		t.Error("corrupt database should load empty")
	}
	// ... and must be usable afterwards.
	s.Join("#ch", "x", seenTime(1))
}

func TestSeenStoreAutoCreatesDataDir(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "a", "b")
	s := newTestStore(t, dir)
	defer s.Stop()
	if _, err := os.Stat(dir); err != nil {
		t.Errorf("data dir not created: %v", err)
	}
}

func TestSeenStorePeriodicFlush(t *testing.T) {
	s, err := NewSeenStore(t.TempDir(), 10*time.Millisecond)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()

	s.Join("#ch", "Bob", seenTime(10))
	path := filepath.Join(s.dir, "#ch", "seen.json")
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return // flushed before Stop()
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("periodic flush did not write the database")
}

func TestSeenStoreConcurrentAccess(t *testing.T) {
	s := newTestStore(t, t.TempDir())
	defer s.Stop()

	done := make(chan struct{})
	go func() {
		for i := int64(0); i < 1000; i++ {
			s.Join("#ch", "Bob", seenTime(i))
			s.Message("#ch", "Bob", seenTime(i))
			s.Leave("#ch", "Bob", seenTime(i))
			s.Get("#ch", "Bob")
			s.Nicks("#ch")
			s.Lookup("#ch", "Bob", []string{"Bob"})
		}
		close(done)
	}()
	go func() {
		for i := int64(0); i < 1000; i++ {
			s.Join("#ch", "Carol", seenTime(i))
			s.Rename("Carol", "Carol_")
			s.Quit("Carol_", seenTime(i))
		}
	}()
	<-done
}

func TestSeenStoreZeroIntervalDefaults(t *testing.T) {
	s, err := NewSeenStore(t.TempDir(), 0) // 0 -> default flush interval
	if err != nil {
		t.Fatal(err)
	}
	defer s.Stop()
}

func TestSeenEntryLast(t *testing.T) {
	tests := []struct {
		e    SeenEntry
		want int64
	}{
		{SeenEntry{Join: 3, Exit: 5, Message: 1}, 5},
		{SeenEntry{Join: 0, Exit: 0, Message: 7}, 7},
		{SeenEntry{}, 0},
	}
	for _, tt := range tests {
		if got := tt.e.Last(); got != tt.want {
			t.Errorf("Last(%+v) = %d, want %d", tt.e, got, tt.want)
		}
	}
}

func TestFormatSeenAge(t *testing.T) {
	tests := []struct {
		sec  int64
		want string
	}{
		{0, "0s ago"},
		{5, "5s ago"},
		{125, "2m ago"},
		{3665, "1h1m ago"},
		{7200, "2h ago"},
		{90000, "1d1h ago"},
		{172800, "2d ago"},
	}
	for _, tt := range tests {
		if got := formatSeenAge(tt.sec); got != tt.want {
			t.Errorf("formatSeenAge(%d) = %q, want %q", tt.sec, got, tt.want)
		}
	}
}

func TestSeenTimeText(t *testing.T) {
	got := seenTimeText(seenTime(100).Unix(), seenTime(130).Unix())
	want := "2023-11-14 22:15:00 UTC, 30s ago"
	if got != want {
		t.Errorf("seenTimeText() = %q, want %q", got, want)
	}
}

func TestSeenLookup(t *testing.T) {
	// Read-only lookup tests against a shared populated store.
	s := seenPopulatedStore(t)
	defer s.Stop()

	t.Run("exact present", func(t *testing.T) {
		r := s.Lookup("#ch", "aly", []string{"Aly", "zoe"})
		if !r.found || !r.present || r.nick != "Aly" {
			t.Fatalf("result = %+v", r)
		}
		if r.entry.Join != seenTime(10).Unix() || r.entry.Message != seenTime(20).Unix() {
			t.Errorf("entry = %+v", r.entry)
		}
	})
	t.Run("exact present case-insensitive query", func(t *testing.T) {
		r := s.Lookup("#ch", "ALY", []string{"Aly"})
		if !r.found || !r.present || r.nick != "Aly" {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("exact present without history", func(t *testing.T) {
		r := s.Lookup("#ch", "zoe", []string{"Zoe"})
		if !r.found || !r.present || r.nick != "Zoe" || r.entry != (SeenEntry{}) {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("exact absent", func(t *testing.T) {
		r := s.Lookup("#ch", "carol", nil)
		if !r.found || r.present || r.nick != "carol" || r.entry.Join != seenTime(30).Unix() {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("single present prefix carries db history", func(t *testing.T) {
		r := s.Lookup("#ch", "bob", []string{"bob__"})
		if !r.found || !r.present || r.nick != "bob__" {
			t.Fatalf("result = %+v", r)
		}
		if r.entry.Exit != seenTime(80).Unix() {
			t.Errorf("entry = %+v", r.entry)
		}
	})
	t.Run("present prefix merges db entry and presence case", func(t *testing.T) {
		r := s.Lookup("#ch", "bob", []string{"Bob_"})
		if !r.found || !r.present || r.nick != "Bob_" {
			t.Fatalf("result = %+v", r)
		}
		if r.entry.Exit != seenTime(50).Unix() {
			t.Errorf("entry = %+v", r.entry)
		}
	})
	t.Run("several present prefix ambiguous", func(t *testing.T) {
		r := s.Lookup("#ch", "bob", []string{"bob_", "bob__"})
		if !r.found || r.present {
			t.Fatalf("result = %+v", r)
		}
		// Only the present candidates are listed (bobz is recorded only).
		if len(r.others) != 2 || r.others[0] != "bob_" || r.others[1] != "bob__" {
			t.Errorf("others = %v", r.others)
		}
	})
	t.Run("single absent prefix", func(t *testing.T) {
		r := s.Lookup("#ch", "car", []string{"Aly"})
		if !r.found || r.present || r.nick != "carol" {
			t.Fatalf("result = %+v", r)
		}
		if r.entry.Join != seenTime(30).Unix() {
			t.Errorf("entry = %+v", r.entry)
		}
	})
	t.Run("several absent prefix ambiguous", func(t *testing.T) {
		r := s.Lookup("#ch", "bo", []string{"Aly"})
		if !r.found || r.present {
			t.Fatalf("result = %+v", r)
		}
		want := []string{"bob_", "bob__", "bobz"}
		if len(r.others) != len(want) {
			t.Fatalf("others = %v, want %v", r.others, want)
		}
		for i := range want {
			if r.others[i] != want[i] {
				t.Errorf("others = %v, want %v", r.others, want)
			}
		}
	})
	t.Run("no match", func(t *testing.T) {
		r := s.Lookup("#ch", "zzz", []string{"Aly"})
		if r.found {
			t.Fatalf("result = %+v", r)
		}
	})
	t.Run("no match in unknown channel", func(t *testing.T) {
		r := s.Lookup("#nope", "aly", nil)
		if r.found {
			t.Fatalf("result = %+v", r)
		}
	})
}

func TestSeenResultText(t *testing.T) {
	// The absolute part of the rendered times is deterministic for these
	// fixed offsets; only the relative age depends on the current time.
	t10 := seenTime(10).Unix() // 2023-11-14 22:13:30 UTC
	t20 := seenTime(20).Unix() // 2023-11-14 22:13:40 UTC
	t40 := seenTime(40).Unix() // 2023-11-14 22:14:00 UTC
	t50 := seenTime(50).Unix() // 2023-11-14 22:14:10 UTC

	t.Run("no record", func(t *testing.T) {
		r := &SeenResult{query: "zzz"}
		if got := r.Text(); got != `no record of "zzz"` {
			t.Errorf("got %q", got)
		}
	})
	t.Run("present with history", func(t *testing.T) {
		r := &SeenResult{query: "aly", found: true, present: true, nick: "Aly",
			entry: SeenEntry{Join: t10, Message: t20}}
		got := r.Text()
		if !strings.HasPrefix(got, "Aly is present") {
			t.Fatalf("got %q", got)
		}
		for _, want := range []string{"joined 2023-11-14 22:13:30 UTC, ",
			"last message 2023-11-14 22:13:40 UTC, "} {
			if !strings.Contains(got, want) {
				t.Errorf("got %q, missing %q", got, want)
			}
		}
	})
	t.Run("present without history", func(t *testing.T) {
		r := &SeenResult{query: "zoe", found: true, present: true, nick: "Zoe"}
		if got := r.Text(); got != "Zoe is present" {
			t.Errorf("got %q", got)
		}
	})
	t.Run("absent with history", func(t *testing.T) {
		r := &SeenResult{query: "bob_", found: true, nick: "bob_",
			entry: SeenEntry{Join: t10, Exit: t50, Message: t40}}
		got := r.Text()
		for _, want := range []string{"bob_ is not present",
			"left 2023-11-14 22:14:10 UTC, ",
			"joined 2023-11-14 22:13:30 UTC, ",
			"last message 2023-11-14 22:14:00 UTC, "} {
			if !strings.Contains(got, want) {
				t.Errorf("got %q, missing %q", got, want)
			}
		}
	})
	t.Run("absent without exit", func(t *testing.T) {
		r := &SeenResult{query: "carol", found: true, nick: "carol",
			entry: SeenEntry{Join: t10, Message: t20}}
		got := r.Text()
		if !strings.Contains(got, "carol is not present") {
			t.Fatalf("got %q", got)
		}
		if strings.Contains(got, "left ") {
			t.Errorf("absent reply mentions exit when unknown: %q", got)
		}
	})
	t.Run("prefix match annotated", func(t *testing.T) {
		r := &SeenResult{query: "aly", found: true, present: true, nick: "Aly_",
			entry: SeenEntry{Join: t10}}
		if got := r.Text(); !strings.HasPrefix(got, `no exact "aly"; Aly_ is present`) {
			t.Errorf("got %q", got)
		}
	})
	t.Run("ambiguous", func(t *testing.T) {
		r := &SeenResult{query: "bob", found: true,
			others: []string{"bob_", "bob__"}}
		got := r.Text()
		want := `multiple nicks matching "bob" are available: bob_, bob__; ` +
			"please be more specific"
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	})
}
