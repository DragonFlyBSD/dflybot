// Copyright (c) 2026 Aaron LI
//
// bbolt storage implementation.
//
// Buckets:
//
//	meta    : "schema_version" -> uint32 (big endian)
//	links   : key    -> JSON Link
//	targets : target -> key
//
// The two link buckets are always written in the same transaction, so
// targets[target] == key exactly when links[key] exists.
//
// Co-authored-by: DeepSeek-v4.1-flash (with Pi Coding Agent)

package main

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"os"
	"time"

	bolt "go.etcd.io/bbolt"
)

const (
	bucketMeta    = "meta"
	bucketLinks   = "links"
	bucketTargets = "targets"

	schemaVersionKey = "schema_version"
	schemaVersion    = uint32(1)
)

// BoltStore is the bbolt-backed Store.
type BoltStore struct {
	db                *bolt.DB
	path              string
	compactTxMaxBytes int64
	now               func() time.Time
}

// OpenBoltStore opens (creating if needed) the database at path.
func OpenBoltStore(path string, compactTxMaxBytes int64) (*BoltStore, error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return nil, fmt.Errorf("open bbolt %q: %w", path, err)
	}
	s := &BoltStore{
		db:                db,
		path:              path,
		compactTxMaxBytes: compactTxMaxBytes,
		now:               func() time.Time { return time.Now().UTC() },
	}
	if err := s.init(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

func (s *BoltStore) init() error {
	return s.db.Update(func(tx *bolt.Tx) error {
		meta, err := tx.CreateBucketIfNotExists([]byte(bucketMeta))
		if err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(bucketLinks)); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(bucketTargets)); err != nil {
			return err
		}
		raw := meta.Get([]byte(schemaVersionKey))
		if raw == nil {
			var buf [4]byte
			binary.BigEndian.PutUint32(buf[:], schemaVersion)
			return meta.Put([]byte(schemaVersionKey), buf[:])
		}
		if len(raw) != 4 {
			return fmt.Errorf("corrupt schema version (%d bytes)", len(raw))
		}
		v := binary.BigEndian.Uint32(raw)
		if v > schemaVersion {
			return fmt.Errorf("database schema version %d is newer than supported version %d", v, schemaVersion)
		}
		// Older versions: run migrations here as they are added.
		return nil
	})
}

func (s *BoltStore) Resolve(target string) (string, error) {
	var key string
	err := s.db.View(func(tx *bolt.Tx) error {
		v := tx.Bucket([]byte(bucketTargets)).Get([]byte(target))
		if v == nil {
			return ErrNotFound
		}
		key = string(v)
		return nil
	})
	return key, err
}

func (s *BoltStore) Get(key string) (*Link, error) {
	var link *Link
	err := s.db.View(func(tx *bolt.Tx) error {
		raw := tx.Bucket([]byte(bucketLinks)).Get([]byte(key))
		if raw == nil {
			return ErrNotFound
		}
		var l Link
		if err := json.Unmarshal(raw, &l); err != nil {
			return fmt.Errorf("decode link %q: %w", key, err)
		}
		l.Key = key
		link = &l
		return nil
	})
	return link, err
}

func (s *BoltStore) Create(target, rule, owner, explicitKey string, gen KeyFunc) (*Link, bool, error) {
	var out *Link
	created := false
	err := s.db.Update(func(tx *bolt.Tx) error {
		links := tx.Bucket([]byte(bucketLinks))
		targets := tx.Bucket([]byte(bucketTargets))

		if k := targets.Get([]byte(target)); k != nil {
			if explicitKey != "" && explicitKey != string(k) {
				return fmt.Errorf("%w: target already mapped to key %q", ErrConflict, string(k))
			}
			raw := links.Get(k)
			if raw == nil {
				return fmt.Errorf("inconsistent database: target %q has no link", target)
			}
			var l Link
			if err := json.Unmarshal(raw, &l); err != nil {
				return err
			}
			l.Key = string(k)
			out, created = &l, false
			return nil
		}

		var key string
		if explicitKey != "" {
			if links.Get([]byte(explicitKey)) != nil {
				return fmt.Errorf("%w: key %q already exists", ErrConflict, explicitKey)
			}
			key = explicitKey
		} else {
			if gen == nil {
				return fmt.Errorf("no key generator for target %q", target)
			}
			k, err := gen(func(candidate string) (bool, error) {
				return links.Get([]byte(candidate)) == nil, nil
			})
			if err != nil {
				return err
			}
			if k == "" {
				return fmt.Errorf("key generator returned an empty key")
			}
			if links.Get([]byte(k)) != nil {
				return fmt.Errorf("%w: generated key %q already exists", ErrConflict, k)
			}
			key = k
		}

		now := s.now()
		l := &Link{Target: target, Rule: rule, Owner: owner, CreatedAt: now, UpdatedAt: now}
		raw, err := json.Marshal(l)
		if err != nil {
			return err
		}
		if err := links.Put([]byte(key), raw); err != nil {
			return err
		}
		if err := targets.Put([]byte(target), []byte(key)); err != nil {
			return err
		}
		l.Key = key
		out, created = l, true
		return nil
	})
	if err != nil {
		return nil, false, err
	}
	return out, created, nil
}

func (s *BoltStore) Update(key, target string) (*Link, error) {
	var out *Link
	err := s.db.Update(func(tx *bolt.Tx) error {
		links := tx.Bucket([]byte(bucketLinks))
		targets := tx.Bucket([]byte(bucketTargets))

		raw := links.Get([]byte(key))
		if raw == nil {
			return ErrNotFound
		}
		var l Link
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		if existing := targets.Get([]byte(target)); existing != nil && string(existing) != key {
			return fmt.Errorf("%w: target already mapped to key %q", ErrConflict, string(existing))
		}
		if l.Target != target {
			if err := targets.Delete([]byte(l.Target)); err != nil {
				return err
			}
			if err := targets.Put([]byte(target), []byte(key)); err != nil {
				return err
			}
			l.Target = target
		}
		l.UpdatedAt = s.now()
		nr, err := json.Marshal(&l)
		if err != nil {
			return err
		}
		if err := links.Put([]byte(key), nr); err != nil {
			return err
		}
		l.Key = key
		out = &l
		return nil
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (s *BoltStore) Delete(key string) error {
	return s.db.Update(func(tx *bolt.Tx) error {
		links := tx.Bucket([]byte(bucketLinks))
		targets := tx.Bucket([]byte(bucketTargets))
		raw := links.Get([]byte(key))
		if raw == nil {
			return ErrNotFound
		}
		var l Link
		if err := json.Unmarshal(raw, &l); err != nil {
			return err
		}
		if err := targets.Delete([]byte(l.Target)); err != nil {
			return err
		}
		return links.Delete([]byte(key))
	})
}

func (s *BoltStore) List(prefix string, limit int, cursor string) ([]*Link, string, error) {
	if limit <= 0 {
		limit = 100
	}
	if limit > 1000 {
		limit = 1000
	}
	links := []*Link{}
	next := ""
	err := s.db.View(func(tx *bolt.Tx) error {
		b := tx.Bucket([]byte(bucketLinks))
		c := b.Cursor()
		var k, v []byte
		if cursor == "" {
			k, v = c.First()
		} else {
			k, v = c.Seek([]byte(cursor))
			if k != nil && string(k) == cursor {
				k, v = c.Next()
			}
		}
		pfx := []byte(prefix)
		for ; k != nil && bytes.HasPrefix(k, pfx); k, v = c.Next() {
			var l Link
			if err := json.Unmarshal(v, &l); err != nil {
				return err
			}
			l.Key = string(k)
			links = append(links, &l)
			next = string(k)
			if len(links) >= limit {
				break
			}
		}
		return nil
	})
	if err != nil {
		return nil, "", err
	}
	if len(links) < limit {
		next = ""
	}
	return links, next, nil
}

func (s *BoltStore) CountPrefix(prefix string) (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		c := tx.Bucket([]byte(bucketLinks)).Cursor()
		pfx := []byte(prefix)
		for k, _ := c.First(); k != nil && bytes.HasPrefix(k, pfx); k, _ = c.Next() {
			n++
		}
		return nil
	})
	return n, err
}

func (s *BoltStore) Count() (int, error) {
	n := 0
	err := s.db.View(func(tx *bolt.Tx) error {
		n = tx.Bucket([]byte(bucketLinks)).Stats().KeyN
		return nil
	})
	return n, err
}

func (s *BoltStore) CompactTo(path string) error {
	dst, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second})
	if err != nil {
		return fmt.Errorf("open compacted database %q: %w", path, err)
	}
	compactErr := bolt.Compact(dst, s.db, s.compactTxMaxBytes)
	syncErr := dst.Sync()
	closeErr := dst.Close()
	if compactErr != nil {
		return compactErr
	}
	if syncErr != nil {
		return syncErr
	}
	return closeErr
}

func (s *BoltStore) Stats() (StoreStats, error) {
	fi, err := os.Stat(s.path)
	if err != nil {
		return StoreStats{}, err
	}
	st := s.db.Stats()
	return StoreStats{
		FileSizeBytes: fi.Size(),
		TxN:           st.TxN,
		OpenTxN:       st.OpenTxN,
		FreePageN:     st.FreePageN,
		PendingPageN:  st.PendingPageN,
		FreeAlloc:     st.FreeAlloc,
	}, nil
}

func (s *BoltStore) Close() error {
	return s.db.Close()
}

// verifyBoltFile opens path read-only, runs bbolt's consistency check, and
// returns the links/targets key counts. It is used to verify compacted backups
// before publishing them (C22).
func verifyBoltFile(path string) (linksN, targetsN int, err error) {
	db, err := bolt.Open(path, 0o600, &bolt.Options{Timeout: time.Second, ReadOnly: true})
	if err != nil {
		return 0, 0, err
	}
	defer db.Close()
	err = db.View(func(tx *bolt.Tx) error {
		for e := range tx.Check() {
			if e != nil {
				return fmt.Errorf("check: %w", e)
			}
		}
		lb := tx.Bucket([]byte(bucketLinks))
		tb := tx.Bucket([]byte(bucketTargets))
		if lb == nil || tb == nil {
			return fmt.Errorf("missing links or targets bucket")
		}
		linksN = lb.Stats().KeyN
		targetsN = tb.Stats().KeyN
		if linksN != targetsN {
			return fmt.Errorf("links count %d != targets count %d", linksN, targetsN)
		}
		// Referential integrity for small databases.
		if linksN <= 100000 {
			return lb.ForEach(func(k, v []byte) error {
				var l Link
				if err := json.Unmarshal(v, &l); err != nil {
					return err
				}
				if got := tb.Get([]byte(l.Target)); string(got) != string(k) {
					return fmt.Errorf("link %q target %q maps to %q", k, l.Target, got)
				}
				return nil
			})
		}
		return nil
	})
	return linksN, targetsN, err
}
