// SPDX-License-Identifier: MIT
//
// Copyright (c) 2024-2025 Aaron LI
//
// TTL cache - tests
//

package ttlcache

import (
	"sync/atomic"
	"testing"
	"time"
)

func TestAdd1(t *testing.T) {
	// No eviction.
	ttl, ttl_11 := 10*time.Millisecond, 11*time.Millisecond
	cache := New(ttl, 10*time.Second, nil)
	defer cache.Close()

	key := "hello"

	// Empty set.
	if v, ok := cache.Get(key); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key, v, ok)
	}
	if v, ok := cache.Pop(key); ok || v != nil {
		t.Errorf(`Pop(%q) = (%v, %t); want (nil, false)`, key, v, ok)
	}
	cache.Delete(key) // should work

	val1, val2 := 1, 2
	if err := cache.Add(key, val1, DefaultTTL); err != nil {
		t.Errorf(`Add(%q) = %v; want nil`, key, err)
	}
	if err := cache.Add(key, val2, DefaultTTL); err != ErrKeyExists {
		t.Errorf(`Add(%q) = %v; want ErrKeyExists`, key, err)
	}

	// Should get the old value.
	if v, ok := cache.Get(key); !ok || v != val1 {
		t.Errorf(`Get(%q) = (%v, %t); want (%v, true)`, key, v, ok, val1)
	}

	// Set() should overwrite the old value.
	cache.Set(key, val2, DefaultTTL)
	if v, ok := cache.Get(key); !ok || v != val2 {
		t.Errorf(`Get(%q) = (%v, %t); want (%v, true)`, key, v, ok, val2)
	}

	// Expire it.
	time.Sleep(ttl_11)
	if v, ok := cache.Get(key); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key, v, ok)
	}

	// Test custom TTL.
	val3 := 3
	cache.Set(key, val3, ttl*2)
	// Should not expire.
	time.Sleep(ttl_11)
	if v, ok := cache.Get(key); !ok || v != val3 {
		t.Errorf(`Get(%q) = (%v, %t); want (%v, true)`, key, v, ok, val3)
	}
	time.Sleep(ttl_11)
	// Now expired.
	if v, ok := cache.Get(key); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key, v, ok)
	}
}

func TestAdd2(t *testing.T) {
	// No eviction.
	ttl := 10 * time.Millisecond
	cache := New(ttl, 10*time.Second, nil)
	defer cache.Close()

	key := "hello"
	val1, val2 := 1, 2

	cache.Set(key, val1, ttl)

	time.Sleep(ttl + time.Millisecond)
	// Now expired
	if v, ok := cache.Get(key); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key, v, ok)
	}
	// Not cleaned, but should allow Add().
	if err := cache.Add(key, val2, ttl); err != nil {
		t.Errorf(`Add(%q) = %v; want nil`, key, err)
	}
	if v, ok := cache.Get(key); !ok || v != val2 {
		t.Errorf(`Get(%q) = (%v, %t); want (%v, true)`, key, v, ok, val2)
	}
}

func TestPopDelete(t *testing.T) {
	// No expiration.
	cache := New(10*time.Second, 0, nil)
	defer cache.Close()

	key1, value1 := "hello", 1
	key2, value2 := "world", 2
	cache.Set(key1, value1, NoTTL)
	cache.Set(key2, value2, NoTTL)

	if v, ok := cache.Pop(key1); !ok || v == nil {
		t.Errorf(`Pop(%q) = (%v, %t); want (%v, true)`, key1, v, ok, value1)
	}
	// Popped, so no result.
	if v, ok := cache.Pop(key1); ok || v != nil {
		t.Errorf(`Pop(%q) = (%v, %t); want (nil, false)`, key1, v, ok)
	}
	// Get() should return nil.
	if v, ok := cache.Get(key1); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key1, v, ok)
	}

	// Delete it and check.
	cache.Delete(key2)
	if v, ok := cache.Get(key2); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key2, v, ok)
	}
}

func TestNoTTL(t *testing.T) {
	ttl := 10 * time.Millisecond
	cache := New(ttl, 0, nil)
	defer cache.Close()

	key1, value1 := "hello", 1
	key2, value2 := "world", 2
	cache.Set(key1, value1, DefaultTTL)
	cache.Set(key2, value2, NoTTL)

	time.Sleep(ttl * 2)
	// key1 expired, key2 not.
	if v, ok := cache.Get(key1); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key1, v, ok)
	}
	if v, ok := cache.Get(key2); !ok || v != value2 {
		t.Errorf(`Get(%q) = (%v, %t); want (%v, true)`, key2, v, ok, value2)
	}
}

func TestEviction1(t *testing.T) {
	var evicted atomic.Uint32
	cache := New(10*time.Millisecond, 20*time.Millisecond,
		func(key string, value any) { evicted.Add(1) })
	defer cache.Close()

	key := "hello"

	cache.Set(key, "hoho", 100*time.Millisecond)
	if v, ok := cache.Get(key); !ok || v == nil {
		t.Errorf(`Get(%q) = (%v, %t); want (!nil, true)`, key, v, ok)
	}
	if v, ok := cache.Pop(key); !ok || v == nil {
		t.Errorf(`Pop(%q) = (%v, %t); want (!nil, true)`, key, v, ok)
	}
	// Pop() should skip the callback.
	if n := evicted.Load(); n != 0 {
		t.Errorf(`(a) evicted = %d; want 0`, n)
	}

	time.Sleep(30 * time.Millisecond)
	// Nothing to clean up and eviction.
	if n := evicted.Load(); n != 0 {
		t.Errorf(`(b) evicted = %d; want 0`, n)
	}
}

func TestEviction2(t *testing.T) {
	var evicted atomic.Uint32
	cache := New(10*time.Millisecond, 20*time.Millisecond,
		func(key string, value any) { evicted.Add(1) })
	defer cache.Close()

	keys := []string{"hello", "world", "yo"}
	for _, key := range keys {
		cache.Set(key, "hoho", 10*time.Millisecond)
	}

	time.Sleep(30 * time.Millisecond)
	if n := evicted.Load(); int(n) != len(keys) {
		t.Errorf(`evicted = %d; want %d`, n, len(keys))
	}

	if n := len(cache.items); n != 0 {
		t.Errorf(`len(items) = %d; want 0`, n)
	}
}

func TestEviction3(t *testing.T) {
	var evicted atomic.Uint32
	cache := New(100*time.Millisecond, 20*time.Millisecond,
		func(key string, value any) { evicted.Add(1) })
	defer cache.Close()

	key := "hello"
	cache.Set(key, "hoho", 10*time.Millisecond)
	time.Sleep(10 * time.Millisecond)
	if v, ok := cache.Get(key); ok || v != nil {
		t.Errorf(`Get(%q) = (%v, %t); want (nil, false)`, key, v, ok)
	}
	if n := evicted.Load(); n != 0 {
		t.Errorf(`(a) evicted = %d; want 0`, n)
	}

	time.Sleep(20 * time.Millisecond)
	if n := evicted.Load(); n != 1 {
		t.Errorf(`(b) evicted = %d; want 1`, n)
	}
}
