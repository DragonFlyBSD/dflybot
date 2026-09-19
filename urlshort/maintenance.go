// Copyright (c) 2026 Aaron LI
//
// Automatic compacted backup of the live bbolt database.
//
// A single goroutine owns the schedule. The live database is never modified
// and writes are not blocked: the backup content is a compacted copy produced
// with bbolt.Compact, verified, and atomically renamed into place (C19-C23).
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

// MaintenanceStatus is exposed under the "maintenance" object of /status.
type MaintenanceStatus struct {
	BackupDir           string     `json:"backup_dir"`
	LastBackupAt        *time.Time `json:"last_backup_at,omitempty"`
	LastBackupOK        bool       `json:"last_backup_ok"`
	LastBackupFile      string     `json:"last_backup_file,omitempty"`
	LastBackupSizeBytes int64      `json:"last_backup_size_bytes,omitempty"`
	BackupFiles         int        `json:"backup_files"`
	LastBackupError     string     `json:"last_backup_error,omitempty"`
}

// Maintenance runs the daily compacted backup and retention.
type Maintenance struct {
	cfg    *Config
	store  Store
	logger *slog.Logger
	now    func() time.Time

	runMu sync.Mutex

	mu     sync.Mutex
	status MaintenanceStatus

	cancel context.CancelFunc
	done   chan struct{}
}

// NewMaintenance builds the maintenance job. The clock is injectable for tests.
func NewMaintenance(cfg *Config, store Store, base *slog.Logger) *Maintenance {
	if base == nil {
		base = slog.Default()
	}
	logger := base.With(slog.String("comp", "maintenance"))

	return &Maintenance{
		cfg:    cfg,
		store:  store,
		logger: logger,
		now:    func() time.Time { return time.Now().UTC() },
		status: MaintenanceStatus{BackupDir: cfg.Backup.Dir},
	}
}

// Start launches the scheduler. It performs the startup backup when configured
// and then runs once per day at backup.hour_utc.
func (m *Maintenance) Start(ctx context.Context) {
	if !m.cfg.Backup.Enabled {
		return
	}
	ctx, cancel := context.WithCancel(ctx)
	m.cancel = cancel
	m.done = make(chan struct{})
	go m.loop(ctx)
}

// Stop cancels the scheduler and waits for the in-flight job up to the
// configured shutdown timeout (section 6.6.1).
func (m *Maintenance) Stop() {
	if m.cancel == nil {
		return
	}
	m.cancel()
	timeout := time.Duration(m.cfg.Server.ShutdownTimeout) * time.Second
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-m.done:
	case <-timer.C:
		m.logger.Warn("timed out waiting for the maintenance job to stop")
	}
}

func (m *Maintenance) loop(ctx context.Context) {
	defer close(m.done)
	m.cleanup(m.now())
	if m.cfg.Backup.RunOnStart {
		if err := m.RunOnce("startup"); err != nil {
			m.logger.Warn("startup backup failed", "error", err)
		}
	}
	for {
		next := m.nextRun(m.now())
		timer := time.NewTimer(time.Until(next))
		select {
		case <-ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			if err := m.RunOnce("scheduled"); err != nil {
				m.logger.Warn("scheduled backup failed", "error", err)
			}
		}
	}
}

// nextRun returns the next occurrence of backup.hour_utc after now.
func (m *Maintenance) nextRun(now time.Time) time.Time {
	now = now.UTC()
	next := time.Date(now.Year(), now.Month(), now.Day(), m.cfg.Backup.HourUTC, 0, 0, 0, time.UTC)
	if !next.After(now) {
		next = next.Add(24 * time.Hour)
	}
	return next
}

// RunOnce performs one backup. trigger is "startup" or "scheduled".
func (m *Maintenance) RunOnce(trigger string) error {
	if !m.cfg.Backup.Enabled {
		return nil
	}
	m.runMu.Lock()
	defer m.runMu.Unlock()

	err := m.backup(trigger)
	m.cleanup(m.now())
	m.record(err)
	if err != nil {
		m.logger.Warn("compacted backup failed", "trigger", trigger, "error", err)
	}
	return err
}

func (m *Maintenance) backup(trigger string) error {
	dir := m.cfg.Backup.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("create backup dir: %w", err)
	}
	now := m.now().UTC()

	var final string
	if trigger == "startup" {
		final = filepath.Join(dir, "links-"+now.Format("2006-01-02T150405")+".db")
	} else {
		final = filepath.Join(dir, "links-"+now.Format("2006-01-02")+".db")
		if linksN, targetsN, err := verifyBoltFile(final); err == nil {
			m.logger.Info("backup already exists, skipping",
				"file", final, "links", linksN, "targets", targetsN)
			return nil
		}
	}

	part := final + ".part"
	_ = os.Remove(part)

	var liveSize int64
	if st, err := m.store.Stats(); err == nil {
		liveSize = st.FileSizeBytes
	}

	start := time.Now()
	if err := m.store.CompactTo(part); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("compact: %w", err)
	}
	linksN, targetsN, err := verifyBoltFile(part)
	if err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("verify compacted backup: %w", err)
	}
	if err := os.Rename(part, final); err != nil {
		_ = os.Remove(part)
		return fmt.Errorf("publish backup: %w", err)
	}
	fsyncDir(dir)

	fi, err := os.Stat(final)
	if err != nil {
		return err
	}
	m.logger.Info("compacted backup written",
		"trigger", trigger,
		"file", final,
		"live_size_bytes", liveSize,
		"size_bytes", fi.Size(),
		"links", linksN,
		"targets", targetsN,
		"duration_ms", time.Since(start).Milliseconds())
	return nil
}

func (m *Maintenance) record(err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.status.BackupDir = m.cfg.Backup.Dir
	if err != nil {
		m.status.LastBackupOK = false
		m.status.LastBackupError = err.Error()
		return
	}
	now := m.now().UTC()
	m.status.LastBackupOK = true
	m.status.LastBackupError = ""
	m.status.LastBackupAt = &now
	files, _ := listBackups(m.cfg.Backup.Dir)
	if len(files) > 0 {
		last := files[len(files)-1]
		m.status.LastBackupFile = last.path
		if fi, err := os.Stat(last.path); err == nil {
			m.status.LastBackupSizeBytes = fi.Size()
		}
	}
}

// cleanup applies retention_days and retention_count and removes stale .part
// files. Failures only warn.
func (m *Maintenance) cleanup(now time.Time) {
	dir := m.cfg.Backup.Dir
	if err := os.MkdirAll(dir, 0o700); err != nil {
		m.logger.Warn("backup retention: create dir failed", "error", err)
		return
	}
	files, err := listBackups(dir)
	if err != nil {
		m.logger.Warn("backup retention listing failed", "error", err)
		return
	}

	cutoff := now.UTC().AddDate(0, 0, -m.cfg.Backup.RetentionDays)
	kept := files[:0]
	for _, f := range files {
		if m.cfg.Backup.RetentionDays > 0 && f.date.Before(cutoff) {
			if err := os.Remove(f.path); err != nil {
				m.logger.Warn("backup retention delete failed",
					"file", f.path, "error", err)
			}
			continue
		}
		kept = append(kept, f)
	}

	if n := m.cfg.Backup.RetentionCount; n > 0 && len(kept) > n {
		for _, f := range kept[:len(kept)-n] {
			if err := os.Remove(f.path); err != nil {
				m.logger.Warn("backup count retention delete failed",
					"file", f.path, "error", err)
			}
		}
	}

	// Remove stale .part files older than one day.
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if now.UTC().Sub(info.ModTime()) > 24*time.Hour {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// Status returns the current maintenance status.
func (m *Maintenance) Status() MaintenanceStatus {
	m.mu.Lock()
	st := m.status
	m.mu.Unlock()
	if files, err := listBackups(m.cfg.Backup.Dir); err == nil {
		st.BackupFiles = len(files)
	}
	st.BackupDir = m.cfg.Backup.Dir
	return st
}

type backupFile struct {
	path string
	date time.Time
}

// listBackups returns the backup files sorted oldest first.
func listBackups(dir string) ([]backupFile, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var out []backupFile
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasPrefix(name, "links-") || !strings.HasSuffix(name, ".db") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(name, "links-"), ".db")
		if len(rest) < 10 {
			continue
		}
		d, err := time.ParseInLocation("2006-01-02", rest[:10], time.UTC)
		if err != nil {
			continue
		}
		out = append(out, backupFile{path: filepath.Join(dir, name), date: d})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].date.Before(out[j].date) })
	return out, nil
}

// fsyncDir flushes a directory entry (the rename) to disk.
func fsyncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	defer d.Close()
	_ = d.Sync()
}
