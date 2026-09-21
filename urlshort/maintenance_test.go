// Copyright (c) 2026 Aaron LI
//
// Backup/compaction maintenance tests.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type maintClock struct {
	ns atomic.Int64
}

func newMaintClock(t time.Time) *maintClock {
	c := &maintClock{}
	c.ns.Store(t.UnixNano())
	return c
}
func (c *maintClock) now() time.Time          { return time.Unix(0, c.ns.Load()).UTC() }
func (c *maintClock) advance(d time.Duration) { c.ns.Add(int64(d)) }

func newTestMaintenance(t *testing.T, store Store, now func() time.Time) *Maintenance {
	t.Helper()
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.DataDir = dir
	cfg.Server.PublicURL = "https://example.com"
	cfg.Server.HTTPPort = 80
	cfg.Server.HTTPSPort = 0
	cfg.ACME.Enabled = false
	cfg.Backup.Enabled = true
	cfg.Backup.Dir = filepath.Join(dir, "backup") + "/"
	cfg.Backup.HourUTC = 3
	cfg.Backup.RunOnStart = false
	cfg.Backup.RetentionDays = 30
	cfg.Backup.StartupRetentionCount = 3
	cfg.Backup.CompactTxMaxBytes = 1 << 20
	m := NewMaintenance(cfg, store, nil)
	m.now = now
	return m
}

func TestMaintenanceBackup(t *testing.T) {
	store := newTestStore(t)
	for i := 0; i < 200; i++ {
		if _, _, err := store.Create("https://x/"+string(rune('a'+i%26))+string(rune('0'+i/26)), "", "", "/g/"+string(rune('a'+i%26))+string(rune('0'+i/26)), nil); err != nil {
			t.Fatal(err)
		}
	}
	for i := 0; i < 150; i++ {
		// Delete a subset to create free pages; ignore not-found.
		store.Delete("/g/" + string(rune('a'+i%26)) + string(rune('0'+i/26)))
	}

	clock := newMaintClock(time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC))
	m := newTestMaintenance(t, store, clock.now)
	if err := m.RunOnce("scheduled"); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	backup := filepath.Join(m.cfg.Backup.Dir, "links-2026-09-11.db")
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup missing: %v", err)
	}
	if _, _, err := verifyBoltFile(backup); err != nil {
		t.Fatalf("backup does not verify: %v", err)
	}
	st := m.Status()
	if !st.LastBackupOK || st.LastBackupFile != backup {
		t.Fatalf("status = %+v", st)
	}

	// Running the scheduled job again skips the existing file.
	if err := m.RunOnce("scheduled"); err != nil {
		t.Fatalf("second RunOnce: %v", err)
	}
}

func TestMaintenanceStartupNames(t *testing.T) {
	store := newTestStore(t)
	clock := newMaintClock(time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC))
	m := newTestMaintenance(t, store, clock.now)

	if err := m.RunOnce("startup"); err != nil {
		t.Fatal(err)
	}
	clock.advance(time.Second)
	if err := m.RunOnce("startup"); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(m.cfg.Backup.Dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("startup backups = %d files, want 2", len(entries))
	}
}

func TestMaintenanceStartupRetentionCount(t *testing.T) {
	store := newTestStore(t)
	if _, _, err := store.Create("https://x/1", "", "", "/g/1", nil); err != nil {
		t.Fatal(err)
	}
	clock := newMaintClock(time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC))
	m := newTestMaintenance(t, store, clock.now)
	m.cfg.Backup.StartupRetentionCount = 2

	// Create four same-day startup backups with distinct second timestamps.
	for i := 0; i < 4; i++ {
		clock.advance(time.Second)
		if err := m.RunOnce("startup"); err != nil {
			t.Fatal(err)
		}
	}
	m.cleanup(clock.now())
	if n := countBackups(t, m.cfg.Backup.Dir, true); n != 2 {
		t.Fatalf("startup backups = %d, want 2", n)
	}

	// Daily backups are governed only by retention_days, never by the cap.
	for _, day := range []string{"2026-09-08", "2026-09-09", "2026-09-10", "2026-09-11"} {
		if err := os.WriteFile(filepath.Join(m.cfg.Backup.Dir, "links-"+day+".db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.cleanup(clock.now())
	if n := countBackups(t, m.cfg.Backup.Dir, false); n != 4 {
		t.Fatalf("daily backups = %d, want 4", n)
	}
	if n := countBackups(t, m.cfg.Backup.Dir, true); n != 2 {
		t.Fatalf("startup backups after cleanup = %d, want 2", n)
	}
}

// countBackups counts `.db` backups of one kind by filename.
func countBackups(t *testing.T, dir string, startup bool) int {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasPrefix(name, "links-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(name, "links-"), ".db")
		if (len(rest) > 10 && rest[10] == 'T') == startup {
			n++
		}
	}
	return n
}

func TestMaintenanceRetentionDays(t *testing.T) {
	store := newTestStore(t)
	clock := newMaintClock(time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC))
	m := newTestMaintenance(t, store, clock.now)
	dir := m.cfg.Backup.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, day := range []string{"2026-07-01", "2026-09-10"} {
		if err := os.WriteFile(filepath.Join(dir, "links-"+day+".db"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	m.cleanup(clock.now())
	if _, err := os.Stat(filepath.Join(dir, "links-2026-07-01.db")); !os.IsNotExist(err) {
		t.Fatalf("old backup not removed (err=%v)", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "links-2026-09-10.db")); err != nil {
		t.Fatalf("recent backup missing: %v", err)
	}
}

func TestMaintenanceFailureLeavesLiveDB(t *testing.T) {
	store := newTestStore(t)
	if _, _, err := store.Create("https://x/1", "", "", "/g/1", nil); err != nil {
		t.Fatal(err)
	}
	clock := newMaintClock(time.Date(2026, 9, 11, 3, 0, 0, 0, time.UTC))
	m := newTestMaintenance(t, store, clock.now)
	// Make a component of the backup path a file so MkdirAll fails.
	blocker := filepath.Join(m.cfg.DataDir, "blocker")
	if err := os.WriteFile(blocker, []byte("not a dir"), 0o600); err != nil {
		t.Fatal(err)
	}
	m.cfg.Backup.Dir = blocker + "/"
	if err := m.RunOnce("startup"); err == nil {
		t.Fatal("expected backup failure")
	}
	// The live database still works.
	link, err := store.Get("/g/1")
	if err != nil || link.Target != "https://x/1" {
		t.Fatalf("live DB damaged: %+v err=%v", link, err)
	}
}

func TestMaintenanceDisabled(t *testing.T) {
	store := newTestStore(t)
	clock := newMaintClock(time.Now())
	m := newTestMaintenance(t, store, clock.now)
	m.cfg.Backup.Enabled = false
	if err := m.RunOnce("startup"); err != nil {
		t.Fatalf("disabled RunOnce: %v", err)
	}
}
