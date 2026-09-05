// Copyright (c) 2026 Aaron LI
//
// Per-channel "seen" database backing the IRC "!seen" command.
//
// The state is loaded on startup, kept in memory, periodically flushed to
// disk, and saved again on shutdown.
//
// Co-authored-by: Deepseek-v4-flash (wit Pi Coding Agent)
//

package main

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	seenVersion      = 1
	seenFile         = "seen.json"
	defaultSeenFlush = 10 * time.Second
)

// SeenEntry holds the last observed activity times (Unix seconds) of a nick
// in one channel.  A zero time means the corresponding event was never seen.
type SeenEntry struct {
	Join    int64 `json:"join,omitempty"`
	Exit    int64 `json:"exit,omitempty"`
	Message int64 `json:"message,omitempty"`
}

// Last returns the most recent event time, or 0 if the entry is empty.
func (e SeenEntry) Last() int64 {
	switch {
	case e.Join >= e.Exit && e.Join >= e.Message:
		return e.Join
	case e.Exit >= e.Message:
		return e.Exit
	}
	return e.Message
}

// SeenData is the on-disk JSON structure of one channel's database.
type SeenData struct {
	Version   int                  `json:"version"`
	Channel   string               `json:"channel"`
	UpdatedAt int64                `json:"updated_at"`
	Nicks     map[string]SeenEntry `json:"nicks"`
}

func (d *SeenData) Snapshot() *SeenData {
	nd := &SeenData{
		Version:   d.Version,
		Channel:   d.Channel,
		UpdatedAt: time.Now().Unix(),
		Nicks:     make(map[string]SeenEntry, len(d.Nicks)),
	}
	for nick, e := range d.Nicks {
		nd.Nicks[nick] = e
	}
	return nd
}

// SeenStore manages the per-channel seen databases and their periodic flush
// to disk.  All methods are safe for concurrent use.
type SeenStore struct {
	dir      string
	interval time.Duration

	mu    sync.Mutex
	data  map[string]*SeenData // by channel
	dirty map[string]bool      // channels with unsaved changes

	stop     chan struct{}
	stopOnce sync.Once
	wg       sync.WaitGroup
}

func NewSeenStore(dir string, interval time.Duration) (*SeenStore, error) {
	if interval <= 0 {
		interval = defaultSeenFlush
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, fmt.Errorf("create data dir %q: %w", dir, err)
	}

	s := &SeenStore{
		dir:      dir,
		interval: interval,
		data:     make(map[string]*SeenData),
		dirty:    make(map[string]bool),
		stop:     make(chan struct{}),
	}
	s.loadAll()

	s.wg.Add(1)
	go s.flushLoop()

	return s, nil
}

// Stop flushes all databases and stops the periodic flush.  Idempotent.
func (s *SeenStore) Stop() {
	s.stopOnce.Do(func() {
		close(s.stop)
		s.wg.Wait()
		s.saveAll()
	})
}

// loadAll reads the database of every channel directory under data_dir.
// Missing or unreadable files are skipped so that data_dir may be shared with
// git-monitor (whose subdirectories hold no seen.json).
func (s *SeenStore) loadAll() {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		slog.Warn("Seen database scan failed", "dir", s.dir, "error", err)
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		ch := e.Name()
		path := filepath.Join(s.dir, ch, seenFile)
		b, err := os.ReadFile(path)
		if err != nil {
			continue // no database yet, e.g. git-monitor's repo dirs
		}
		var d SeenData
		if err := json.Unmarshal(b, &d); err != nil {
			slog.Warn("Seen database unreadable; starting empty",
				"channel", ch, "path", path, "error", err)
			continue
		}
		if d.Version != seenVersion {
			slog.Warn("Seen database version unsupported; starting empty",
				"channel", ch, "version", d.Version)
			continue
		}
		if d.Nicks == nil {
			d.Nicks = make(map[string]SeenEntry)
		}
		s.data[ch] = &d
		slog.Info("Seen database loaded", "channel", ch, "nicks", len(d.Nicks))
	}
}

// flushLoop periodically persists the dirty channel databases.
func (s *SeenStore) flushLoop() {
	defer s.wg.Done()
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-ticker.C:
			s.flushDirty()
		}
	}
}

func (s *SeenStore) flushDirty() {
	for {
		s.mu.Lock()
		var ch string
		for c, dirty := range s.dirty {
			if dirty {
				ch = c
				break
			}
		}
		s.mu.Unlock()
		if ch == "" {
			return
		}
		s.write(ch)
	}
}

func (s *SeenStore) saveAll() {
	s.mu.Lock()
	chs := make([]string, 0, len(s.data))
	for ch := range s.data {
		chs = append(chs, ch)
	}
	s.mu.Unlock()
	for _, ch := range chs {
		s.write(ch)
	}
}

// write persists one channel database (atomic tmp-file + rename), taking a
// snapshot so that the file I/O does not hold the lock.
func (s *SeenStore) write(ch string) {
	s.mu.Lock()
	d, ok := s.data[ch]
	if !ok {
		s.mu.Unlock()
		return
	}
	snap := d.Snapshot()
	s.dirty[ch] = false
	s.mu.Unlock()

	if err := s.writeFile(ch, snap); err != nil {
		slog.Warn("Seen database save failed", "channel", ch, "error", err)
	}
}

func (s *SeenStore) writeFile(ch string, d *SeenData) error {
	dir := filepath.Join(s.dir, ch)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	b, err := json.MarshalIndent(d, "", "  ")
	if err != nil {
		return err
	}
	path := filepath.Join(dir, seenFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		return err
	}
	return nil
}

// set applies a mutation to the record of nick in channel (creating the
// record and database as needed), then marks the channel dirty.
func (s *SeenStore) set(ch, nick string, apply func(*SeenEntry)) {
	if ch == "" || nick == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data[ch]
	if d == nil {
		d = &SeenData{
			Version: seenVersion,
			Channel: ch,
			Nicks:   make(map[string]SeenEntry),
		}
		s.data[ch] = d
	}
	key := ciKey(d.Nicks, nick)
	if key == "" {
		key = nick
	}
	e := d.Nicks[key]
	apply(&e)
	d.Nicks[key] = e
	s.dirty[ch] = true
}

// Join records that nick joined (or rejoined) channel at time t.
func (s *SeenStore) Join(ch, nick string, t time.Time) {
	s.set(ch, nick, func(e *SeenEntry) { e.Join = t.Unix() })
}

// Leave records that nick parted from or was kicked off channel at time t.
func (s *SeenStore) Leave(ch, nick string, t time.Time) {
	s.set(ch, nick, func(e *SeenEntry) { e.Exit = t.Unix() })
}

// Message records that nick spoke in channel at time t.
func (s *SeenStore) Message(ch, nick string, t time.Time) {
	s.set(ch, nick, func(e *SeenEntry) { e.Message = t.Unix() })
}

// Quit records nick's exit (at time t) from every channel it was seen in.
func (s *SeenStore) Quit(nick string, t time.Time) {
	if nick == "" {
		return
	}
	u := t.Unix()
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, d := range s.data {
		if key := ciKey(d.Nicks, nick); key != "" {
			e := d.Nicks[key]
			e.Exit = u
			d.Nicks[key] = e
			s.dirty[ch] = true
		}
	}
}

// Rename moves the record of nick old (in every channel) to neu, preserving
// its history; a nick change is not a leave+join.
func (s *SeenStore) Rename(old, neu string) {
	if old == "" || neu == "" || strings.EqualFold(old, neu) {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for ch, d := range s.data {
		key := ciKey(d.Nicks, old)
		if key == "" {
			continue
		}
		e := d.Nicks[key]
		delete(d.Nicks, key)
		dest := ciKey(d.Nicks, neu)
		if dest == "" {
			dest = neu
		}
		ne := d.Nicks[dest]
		if ne.Join == 0 {
			ne.Join = e.Join
		}
		if ne.Exit == 0 {
			ne.Exit = e.Exit
		}
		if ne.Message == 0 {
			ne.Message = e.Message
		}
		d.Nicks[dest] = ne
		s.dirty[ch] = true
	}
}

// Get returns the record of nick (matched case-insensitively) in channel.
func (s *SeenStore) Get(ch, nick string) (SeenEntry, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data[ch]
	if d == nil {
		return SeenEntry{}, false
	}
	key := ciKey(d.Nicks, nick)
	if key == "" {
		return SeenEntry{}, false
	}
	return d.Nicks[key], true
}

// Nicks returns a copy of the records of channel (keyed by the last observed
// nick spelling), suitable for queries.
func (s *SeenStore) Nicks(ch string) map[string]SeenEntry {
	s.mu.Lock()
	defer s.mu.Unlock()
	d := s.data[ch]
	if d == nil {
		return make(map[string]SeenEntry)
	}
	out := make(map[string]SeenEntry, len(d.Nicks))
	for nick, e := range d.Nicks {
		out[nick] = e
	}
	return out
}

// ciKey returns the existing nick that case-insensitively the given nick.
func ciKey(m map[string]SeenEntry, nick string) string {
	for k := range m {
		if strings.EqualFold(k, nick) {
			return k
		}
	}
	return ""
}

// seenMatch is a nick candidate during the !seen prefix matching.
type seenMatch struct {
	nick    string
	entry   SeenEntry
	present bool
}

// Lookup resolves a !seen query against the seen database of channel and
// the currently-present nicks (from the IRC state tracker).  Nick
// comparisons are case-insensitive (as per IRC).
//
// The result is either:
//   - a "present"/"not present" reply for the exact nick, or for the sole
//     prefix-matched candidate (a currently-present candidate is preferred
//     over a merely recorded one);
//   - an ambiguity request listing the prefix-matched candidates when
//     several of them match;
//   - "no record" when nothing matches.
func (s *SeenStore) Lookup(channel string, query string, present []string) *SeenResult {
	nicks := s.Nicks(channel)
	lq := strings.ToLower(query)
	pres := make(map[string]string, len(present)) // lower nick -> display nick
	for _, n := range present {
		if _, ok := pres[strings.ToLower(n)]; !ok {
			pres[strings.ToLower(n)] = n
		}
	}

	ci := func(n string) (SeenEntry, bool) {
		for k, v := range nicks {
			if strings.EqualFold(k, n) {
				return v, true
			}
		}
		return SeenEntry{}, false
	}

	// Exact nick, currently present.
	if nick, ok := pres[lq]; ok {
		entry, _ := ci(nick)
		return &SeenResult{
			query:   query,
			found:   true,
			present: true,
			nick:    nick,
			entry:   entry,
		}
	}
	// Exact nick, has history.
	if entry, ok := ci(query); ok {
		return &SeenResult{
			query: query,
			found: true,
			nick:  query,
			entry: entry,
		}
	}

	// Prefix matching: union of the database records and the present nicks.
	matches := make(map[string]seenMatch) // lower nick -> candidate
	add := func(nick string, entry SeenEntry, present bool) {
		ln := strings.ToLower(nick)
		if ln == lq || !strings.HasPrefix(ln, lq) {
			return
		}
		m, ok := matches[ln]
		if !ok {
			m = seenMatch{nick: nick}
		}
		m.present = m.present || present
		if m.entry.Last() == 0 {
			m.entry = entry
		}
		matches[ln] = m
	}
	for _, nick := range pres {
		add(nick, SeenEntry{}, true)
	}
	for n, entry := range nicks {
		add(n, entry, false)
	}
	if len(matches) == 0 {
		return &SeenResult{query: query}
	}

	// Resolve to a single candidate whenever possible, preferring one
	// that is currently present.
	var presentMatches []string
	for ln, m := range matches {
		if m.present {
			presentMatches = append(presentMatches, ln)
		}
	}
	if len(presentMatches) == 1 {
		m := matches[presentMatches[0]]
		return &SeenResult{
			query:   query,
			found:   true,
			present: true,
			nick:    m.nick,
			entry:   m.entry,
		}
	}
	if len(matches) == 1 {
		for _, m := range matches {
			return &SeenResult{
				query: query,
				found: true,
				nick:  m.nick,
				entry: m.entry,
			}
		}
	}

	// Several candidates: ask for disambiguation.  List the currently
	// present ones when there are any (the present nick was seen in this
	// channel), otherwise all the recorded ones, sorted for determinism.
	list := make([]string, 0, len(matches))
	if len(presentMatches) > 0 {
		for _, ln := range presentMatches {
			list = append(list, matches[ln].nick)
		}
	} else {
		for _, m := range matches {
			list = append(list, m.nick)
		}
	}
	sort.Strings(list)
	return &SeenResult{query: query, found: true, others: list}
}

type SeenResult struct {
	query   string
	found   bool
	present bool // the matched nick is currently in the channel
	nick    string
	entry   SeenEntry
	others  []string // prefix-matched nicks
}

// Text builds the reply for the !seen query.
func (r *SeenResult) Text() string {
	if !r.found {
		return fmt.Sprintf("no record of %q", r.query)
	}
	if len(r.others) > 1 {
		return fmt.Sprintf("multiple nicks matching %q are available: %s; please be more specific",
			r.query, strings.Join(r.others, ", "))
	}

	var b strings.Builder
	if !strings.EqualFold(r.nick, r.query) {
		fmt.Fprintf(&b, "no exact %q; ", r.query)
	}
	if r.present {
		fmt.Fprintf(&b, "%s is present", r.nick)
	} else {
		fmt.Fprintf(&b, "%s is not present", r.nick)
	}

	now := time.Now().Unix()
	var facts []string
	if r.present {
		if r.entry.Join > 0 {
			facts = append(facts, "joined "+seenTimeText(r.entry.Join, now))
		}
		if r.entry.Message > 0 {
			facts = append(facts, "last message "+seenTimeText(r.entry.Message, now))
		}
	} else {
		if r.entry.Exit > 0 {
			facts = append(facts, "left "+seenTimeText(r.entry.Exit, now))
		}
		if r.entry.Join > 0 {
			facts = append(facts, "joined "+seenTimeText(r.entry.Join, now))
		}
		if r.entry.Message > 0 {
			facts = append(facts, "last message "+seenTimeText(r.entry.Message, now))
		}
	}
	if len(facts) > 0 {
		b.WriteString(" (" + strings.Join(facts, "; ") + ")")
	}
	return b.String()
}

// seenTimeText renders an event time as absolute UTC plus relative age, e.g.
// "2025-09-05 04:20:10 UTC, 2h ago".
func seenTimeText(u, now int64) string {
	return time.Unix(u, 0).UTC().Format("2006-01-02 15:04:05") + " UTC, " + formatSeenAge(now-u)
}

// formatSeenAge renders a duration in seconds as a compact relative age.
func formatSeenAge(d int64) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < 60:
		return fmt.Sprintf("%ds ago", d)
	case d < 3600:
		return fmt.Sprintf("%dm ago", d/60)
	case d < 86400:
		h, m := d/3600, (d%3600)/60
		if m == 0 {
			return fmt.Sprintf("%dh ago", h)
		}
		return fmt.Sprintf("%dh%dm ago", h, m)
	default:
		days, h := d/86400, (d%86400)/3600
		if h == 0 {
			return fmt.Sprintf("%dd ago", days)
		}
		return fmt.Sprintf("%dd%dh ago", days, h)
	}
}
