// SPDX-License-Identifier: MIT
//
// Copyright (c) 2025 Aaron LI
//
// Git monitor service.
//

package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"sort"
	"strings"
	"sync"
	"time"
)

type MonitorConfig struct {
	Name      string
	RepoURL   string
	RepoDir   string
	StatePath string
	Interval  time.Duration
	Poster    Poster
}

type Monitor struct {
	config *MonitorConfig
	state  State
	mutex  sync.Mutex
	logger *slog.Logger
}

// State persisted to disk to avoid duplicates.
type State struct {
	// map ref name -> last announced commit sha
	LastSeen map[string]string `json:"last_seen"`
	// map tag name -> tag object sha (or commit sha for lightweight)
	SeenTags map[string]string `json:"seen_tags"`
	// when the state was last updated
	UpdatedAt time.Time `json:"updated_at"`
}

type Poster interface {
	// Get the max message length
	GetMaxLength() int
	// Post the text message
	Post(ctx context.Context, text string) error
}

func NewMonitor(cfg *MonitorConfig, base *slog.Logger) *Monitor {
	if base == nil {
		base = slog.Default()
	}
	logger := base.With(slog.String("repo", cfg.Name))

	return &Monitor{
		config: cfg,
		logger: logger,
		state: State{
			LastSeen: make(map[string]string),
			SeenTags: make(map[string]string),
		},
	}
}

func (m *Monitor) Start(ctx context.Context, wg *sync.WaitGroup) {
	defer func() {
		if err := m.saveState(); err != nil {
			m.logger.Warn("state save failed", "error", err)
		}
		wg.Done()
	}()

	if m.loadState() != nil {
		return
	}
	if m.cloneRepo(ctx) != nil {
		return
	}
	if m.seedState() != nil {
		return
	}
	if err := m.saveState(); err != nil {
		m.logger.Warn("state save failed", "error", err)
	}

	ticker := time.NewTicker(m.config.Interval)
	defer ticker.Stop()
	for {
		if m.updateRepo(ctx) == nil {
			ans := m.collectAnnouncements()
			if len(ans) > 0 {
				m.sendAnnouncements(ctx, ans)
				if err := m.saveState(); err != nil {
					m.logger.Warn("state save failed", "error", err)
				}
			}
		}
		// else: retry at next tick

		select {
		case <-ctx.Done():
			m.logger.Debug("monitor exiting")
			return
		case <-ticker.C:
		}
	}
}

func (m *Monitor) loadState() error {
	var state State
	path := m.config.StatePath
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		m.logger.Debug("state file not exist", "path", path)
		return nil
	} else {
		b, err := os.ReadFile(path)
		if err != nil {
			m.logger.Error("state file read failure", "path", path, "error", err)
			return err
		}
		if err := json.Unmarshal(b, &state); err != nil {
			m.logger.Error("state file unmarshal failure", "path", path, "error", err)
			return err
		}
		m.logger.Info("state loaded", "file", path,
			"branches", len(state.LastSeen), "tags", len(state.SeenTags))
	}

	if state.LastSeen == nil {
		state.LastSeen = make(map[string]string)
	}
	if state.SeenTags == nil {
		state.SeenTags = make(map[string]string)
	}

	m.mutex.Lock()
	m.state = state
	m.mutex.Unlock()

	return nil
}

func (m *Monitor) saveState() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	m.state.UpdatedAt = time.Now()
	b, err := json.MarshalIndent(&m.state, "", "  ")
	if err != nil {
		return fmt.Errorf("state marshal failure: %w", err)
	}

	path := m.config.StatePath
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return fmt.Errorf("state file (%s) write failure: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("state file rename (%s -> %s) failure: %w", tmp, path, err)
	}

	m.logger.Info("state saved", "file", path)
	return nil
}

// seedState sets state's LastSeen for all branches to current tip and records
// existing tags.
func (m *Monitor) seedState() error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if len(m.state.LastSeen) > 0 || len(m.state.SeenTags) > 0 {
		return nil
	}

	m.logger.Info("seeding state with current repo tips")

	// branches
	refs, err := m.listRefs("refs/heads")
	if err != nil {
		return err
	}
	for name, sha := range refs {
		m.state.LastSeen["branch:"+name] = sha
	}
	m.logger.Info("seeded branches", "count", len(m.state.LastSeen))

	// tags
	tags, err := m.listRefs("refs/tags")
	if err != nil {
		return err
	}
	for t, sha := range tags {
		m.state.SeenTags[t] = sha
	}
	m.logger.Info("seeded tags", "count", len(m.state.SeenTags))

	m.state.UpdatedAt = time.Now()
	return nil
}

func (m *Monitor) cloneRepo(ctx context.Context) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	if _, err := os.Stat(m.config.RepoDir); err == nil {
		m.logger.Debug("repo directory already exists", "dir", m.config.RepoDir)
		return nil
	}

	m.logger.Info("cloning repo", "url", m.config.RepoURL, "dir", m.config.RepoDir)
	cmd := exec.CommandContext(ctx, "git", "clone", "--mirror", m.config.RepoURL, m.config.RepoDir)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		m.logger.Error("repo clone failed", "command", cmd, "error", err)
		return err
	}

	m.logger.Info("repo cloned", "command", cmd)
	return nil
}

func (m *Monitor) updateRepo(ctx context.Context) error {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	cmd := exec.CommandContext(ctx, "git", "--git-dir="+m.config.RepoDir,
		"remote", "update", "--prune")
	m.logger.Debug("updating repo", "command", cmd)

	var out bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &out
	if err := cmd.Run(); err != nil {
		m.logger.Error("repo update failed", "command", cmd,
			"error", err, "output", out.String())
		return err
	}

	m.logger.Debug("repo updated")
	return nil
}

type announcement struct {
	branch     string
	tag        string
	info       *commitInfo
	tagUpdated bool
	isMerge    bool
}

// collectAnnouncements finds new commits and tags and returns formatted
// messages.  It also updates the provided state in-memory (but caller is
// responsible to persist).
func (m *Monitor) collectAnnouncements() []*announcement {
	m.mutex.Lock()
	defer m.mutex.Unlock()

	// branches
	var branchAns []*announcement
	branches, err := m.listRefs("refs/heads")
	if err == nil {
		// iterate branches in sorted order for determinism
		names := make([]string, 0, len(branches))
		for n := range branches {
			names = append(names, n)
		}
		sort.Strings(names)
		for _, b := range names {
			ref := "branch:" + b
			newSHA := branches[b]
			oldSHA := m.state.LastSeen[ref]
			if oldSHA == "" {
				// not seen before — seed it
				m.state.LastSeen[ref] = newSHA
				continue
			}
			if oldSHA == newSHA {
				continue // nothing new
			}
			// get list of commits old..new, oldest first
			commits, err := m.revList(oldSHA, newSHA)
			if err != nil {
				// do not update state — try again next time
				continue
			}
			for _, sha := range commits {
				ci, err := m.getCommitInfo(sha)
				if err != nil {
					continue
				}
				isMerge, _ := m.isMergeCommit(sha)
				branchAns = append(branchAns, &announcement{
					branch:  b,
					info:    ci,
					isMerge: isMerge,
				})
				m.state.LastSeen[ref] = sha
			}
		}
	}

	// tags
	var tagAns []*announcement
	tags, err := m.listRefs("refs/tags")
	if err == nil {
		names := make([]string, 0, len(tags))
		for t := range tags {
			names = append(names, t)
		}
		sort.Strings(names)
		for _, t := range names {
			sha := tags[t]
			prev, seen := m.state.SeenTags[t]
			if !seen || prev != sha {
				// New tag, or tag has been updated to point to
				// difference commit (rare).
				//
				// For annotated tags, objectname is the tag object; we
				// want the commit the tag points to.
				commitSHA, err := m.derefTagToCommit(t)
				if err != nil {
					// fallback to the objectname
					commitSHA = sha
				}
				ci, err := m.getCommitInfo(commitSHA)
				if err == nil {
					tagAns = append(tagAns, &announcement{
						tag:        t,
						info:       ci,
						tagUpdated: seen,
					})
				}
				// If err != nil, still mark seen to avoid
				// loop.
				m.state.SeenTags[t] = commitSHA
			}
		}
	}

	m.logger.Debug("collected announcements", "branches", len(branchAns), "tags", len(tagAns))
	return append(branchAns, tagAns...)
}

func (m *Monitor) sendAnnouncements(ctx context.Context, ans []*announcement) {
	// Announce tags: one tag per message
	for _, a := range ans {
		if a.tag == "" {
			continue
		}
		tag := "tag:" + a.tag
		if a.tagUpdated {
			tag += " (updated)"
		}
		msg := fmt.Sprintf("[%s] %s %s <%s> (%s) %s",
			m.config.Name, tag, a.info.AuthorName, a.info.AuthorEmail,
			shortSHA(a.info.Hash), sanitize(a.info.Subject))
		m.logger.Info("announce tag", "tag", a.tag, "msg", msg)
		m.config.Poster.Post(ctx, msg)
	}

	// Announce commits: branch by branch
	msgs := make(map[string][]string)
	for _, a := range ans {
		if a.branch == "" {
			continue
		}
		subj := sanitize(a.info.Subject)
		if a.isMerge {
			subj = "(merge) " + subj
		}
		msg := fmt.Sprintf("%s <%s> (%s) %s",
			a.info.AuthorName, a.info.AuthorEmail,
			shortSHA(a.info.Hash), subj)
		msgs[a.branch] = append(msgs[a.branch], msg)
	}
	// Order by branches
	branches := make([]string, 0, len(msgs))
	for b := range msgs {
		branches = append(branches, b)
	}
	sort.Strings(branches)
	// Batch the messages
	const separator = " || "
	for _, b := range branches {
		bmsgs := msgs[b]
		m.logger.Info("announce commits", "branch", b, "count", len(bmsgs))
		prompt := fmt.Sprintf("[%s:%s] ", m.config.Name, b)
		maxLen := m.config.Poster.GetMaxLength() - len(prompt)
		curLen := 0
		var batch []string
		for _, am := range bmsgs {
			amLen := len([]byte(am))
			sepLen := 0
			if len(batch) > 0 {
				sepLen = len([]byte(separator))
			}
			if curLen+sepLen+amLen <= maxLen {
				batch = append(batch, am)
				curLen += sepLen + amLen
			} else {
				msg := prompt + strings.Join(batch, separator)
				m.logger.Info("announce commits in batch",
					"count", len(batch), "msg", msg)
				m.config.Poster.Post(ctx, msg)
				// start a new batch
				batch = []string{am}
				curLen = amLen
			}
		}
		// send the final batch
		if len(batch) > 0 {
			msg := prompt + strings.Join(batch, separator)
			m.logger.Info("announce commits in batch",
				"count", len(batch), "msg", msg)
			m.config.Poster.Post(ctx, msg)
		}
	}
}

// listRefs returns map of shortname->sha for refs under the provided prefix.
func (m *Monitor) listRefs(prefix string) (map[string]string, error) {
	cmd := exec.Command("git", "--git-dir="+m.config.RepoDir, "for-each-ref",
		"--format=%(refname:short) %(objectname)", prefix)
	m.logger.Debug("listing repo refs", "prefix", prefix, "command", cmd)

	out, err := cmd.Output()
	if err != nil {
		m.logger.Error("repo listing refs failed", "command", cmd,
			"error", err, "output", string(out))
		return nil, err
	}

	refs := map[string]string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		parts := strings.Split(line, " ")
		if len(parts) != 2 {
			m.logger.Warn("ignore invalid ref", "line", line)
			continue
		}
		name, sha := parts[0], parts[1]
		refs[name] = sha
	}
	if err := scanner.Err(); err != nil {
		m.logger.Warn("repo refs scanning failure", "error", err)
		return nil, err
	}

	return refs, nil
}

// revList returns list of commit SHAs from old..new (exclusive of old,
// inclusive of new) reversed to oldest->newest.
func (m *Monitor) revList(oldSHA, newSHA string) ([]string, error) {
	cmd := exec.Command("git", "--git-dir="+m.config.RepoDir, "rev-list",
		"--reverse", fmt.Sprintf("%s..%s", oldSHA, newSHA))
	m.logger.Debug("listing commits", "command", cmd)

	out, err := cmd.Output()
	if err != nil {
		m.logger.Error("repo listing commits failed", "command", cmd,
			"error", err, "output", string(out))
		return nil, err
	}

	lines := []string{}
	scanner := bufio.NewScanner(bytes.NewReader(out))
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line != "" {
			lines = append(lines, line)
		}
	}
	if err := scanner.Err(); err != nil {
		m.logger.Warn("repo commits scanning failure", "error", err)
		return nil, err
	}

	return lines, nil
}

type commitInfo struct {
	Hash        string
	AuthorName  string
	AuthorEmail string
	Date        string
	Subject     string
}

// getCommitInfo extracts commit metadata using git show -s --format=...
func (m *Monitor) getCommitInfo(sha string) (*commitInfo, error) {
	cmd := exec.Command("git", "--git-dir="+m.config.RepoDir, "show",
		"--no-patch", "--format=%H%n%an%n%ae%n%ad%n%s", sha)
	out, err := cmd.Output()
	if err != nil {
		m.logger.Error("show sha failed", "command", cmd,
			"error", err, "output", string(out))
		return nil, err
	}

	parts := strings.Split(string(out), "\n")
	ci := &commitInfo{
		Hash:        strings.TrimSpace(parts[0]),
		AuthorName:  strings.TrimSpace(parts[1]),
		AuthorEmail: strings.TrimSpace(parts[2]),
		Date:        strings.TrimSpace(parts[3]),
		Subject:     strings.TrimSpace(parts[4]),
	}
	return ci, nil
}

// isMergeCommit returns true if commit has more than one parent
func (m *Monitor) isMergeCommit(sha string) (bool, error) {
	cmd := exec.Command("git", "--git-dir="+m.config.RepoDir, "rev-list",
		"--parents", "-n", "1", sha)
	out, err := cmd.Output()
	if err != nil {
		m.logger.Error("rev-list parents failed", "command", cmd,
			"error", err, "output", string(out))
		return false, err
	}

	// format: <sha> <parent1> <parent2> ...
	parts := strings.Fields(string(out))
	if len(parts) > 2 {
		return true, nil
	}
	return false, nil
}

// derefTagToCommit tries to resolve a tag name to the commit SHA it points to.
func (m *Monitor) derefTagToCommit(tag string) (string, error) {
	cmd := exec.Command("git", "--git-dir="+m.config.RepoDir, "rev-parse",
		"--verify", tag+"^{commit}")
	out, err := cmd.Output()
	if err != nil {
		m.logger.Error("rev-parse tag->commit failed", "command", cmd,
			"error", err, "output", string(out))
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

// shortSHA returns short form
func shortSHA(sha string) string {
	if len(sha) >= 7 {
		return sha[:7]
	}
	return sha
}

// sanitize commit subject/body for IRC: replace newlines/tabs and collapse spaces.
func sanitize(s string) string {
	if s == "" {
		return ""
	}
	// Replace newlines and tabs with spaces
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	// collapse multiple spaces
	s = strings.Join(strings.Fields(s), " ")
	return s
}
