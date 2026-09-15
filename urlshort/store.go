// Copyright (c) 2026 Aaron LI
//
// Storage interface and shared types.
//
// The production implementation is bbolt (store_bbolt.go). The interface keeps
// the handlers testable and documents the exact operations the rest of the
// program relies on.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"errors"
	"time"
)

// Storage errors. Handlers map ErrNotFound to 404 and ErrConflict to 409.
var (
	ErrNotFound = errors.New("not found")
	ErrConflict = errors.New("conflict")
)

// Link is one stored short link. Key is the bucket key and is not persisted.
type Link struct {
	Key       string    `json:"-"`
	Target    string    `json:"target"`
	Rule      string    `json:"rule,omitempty"`
	Owner     string    `json:"owner,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// KeyFunc selects a key for a new link. The store calls it inside the write
// transaction and passes isFree, which reports whether a candidate key is free
// (that is, not used by a different target). The generator must not return a
// key for which isFree returned false.
type KeyFunc func(isFree func(key string) (bool, error)) (string, error)

// DBStats is a snapshot of storage statistics for the status endpoint.
type DBStats struct {
	FileSizeBytes int64 `json:"file_size_bytes"`
	TxN           int   `json:"tx_n"`
	OpenTxN       int   `json:"open_tx_n"`
	FreePageN     int   `json:"free_page_n"`
	PendingPageN  int   `json:"pending_page_n"`
	FreeAlloc     int   `json:"free_alloc_bytes"`
}

// Store is the persistent link store.
type Store interface {
	// Resolve returns the key for a target, or ErrNotFound.
	Resolve(target string) (string, error)
	// Get returns the link stored under key, or ErrNotFound.
	Get(key string) (*Link, error)
	// Create maps target to a key and returns the link and whether it was
	// created. When explicitKey is non-empty it is used as-is; otherwise gen is
	// called to choose a key. Returns ErrConflict on any collision.
	Create(target, rule, owner, explicitKey string, gen KeyFunc) (*Link, bool, error)
	// Update retargets an existing key.
	Update(key, target string) (*Link, error)
	// Delete removes a link.
	Delete(key string) error
	// List returns links whose key starts with prefix, at most limit entries,
	// starting after cursor. It returns the links and the next cursor.
	List(prefix string, limit int, cursor string) ([]*Link, string, error)
	// CountPrefix returns the number of links whose key starts with prefix.
	CountPrefix(prefix string) (int, error)
	// Count returns the total number of links.
	Count() (int, error)
	// CompactTo writes a compacted copy of the database to path.
	CompactTo(path string) error
	// Stats returns storage statistics.
	Stats() (DBStats, error)
	// Close closes the database.
	Close() error
}
