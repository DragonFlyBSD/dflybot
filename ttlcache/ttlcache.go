// SPDX-License-Identifier: MIT
//
// Copyright (c) 2024-2025 Aaron LI
//
// TTL cache
//

package ttlcache

import (
	"context"
	"errors"
	"sync"
	"time"
)

const (
	DefaultTTL = 0  // use default TTL of the TtlCache instance
	NoTTL      = -1 // no expiration
)

const defaultInterval = 5 * time.Second // default cleanup interval
var nopEviction = func(string, any) {}  // default nop eviction callback

var ErrKeyExists = errors.New("key already exists")

type Cache struct {
	items      map[string]*cacheItem
	lock       sync.RWMutex // protect concurrent cleanups
	defaultTTL time.Duration
	onEviction func(string, any)
	cancel     context.CancelFunc
	wg         sync.WaitGroup
}

type cacheItem struct {
	value    any
	expireAt int64 // UnixNano
}

func (i *cacheItem) isExpired(now int64) bool {
	return i.expireAt > 0 && i.expireAt < now
}

var itemPool = sync.Pool{
	New: func() any {
		return &cacheItem{}
	},
}

func New(
	defaultTTL time.Duration,
	interval time.Duration,
	onEviction func(string, any),
) *Cache {
	if interval == 0 {
		interval = defaultTTL / 2
		if interval == 0 {
			interval = defaultInterval
		}
	}
	if onEviction == nil {
		onEviction = nopEviction
	}

	ctx, cancel := context.WithCancel(context.Background())
	c := &Cache{
		items:      make(map[string]*cacheItem),
		defaultTTL: defaultTTL,
		onEviction: onEviction,
		cancel:     cancel,
	}

	c.wg.Add(1)
	go c.clean(ctx, interval)

	return c
}

func (c *Cache) Close() {
	c.cancel()
	c.wg.Wait()
}

// Add the key and value with the TTL.
// If key already exists, return ErrKeyExists.
func (c *Cache) Add(key string, value any, ttl time.Duration) error {
	c.lock.Lock()
	defer c.lock.Unlock()

	item, exists := c.items[key]
	if exists && !item.isExpired(time.Now().UnixNano()) {
		return ErrKeyExists
	}

	if item == nil {
		item = itemPool.Get().(*cacheItem)
	}
	item.value = value
	item.expireAt = c.getExpireAt(ttl)
	c.items[key] = item
	return nil
}

// Similar to Add(), but overwrite the existing one.
func (c *Cache) Set(key string, value any, ttl time.Duration) {
	c.lock.Lock()
	defer c.lock.Unlock()

	item := itemPool.Get().(*cacheItem)
	item.value = value
	item.expireAt = c.getExpireAt(ttl)
	c.items[key] = item
}

// Get the value of key, with a boolean indicating whether it was found.
func (c *Cache) Get(key string) (value any, exists bool) {
	c.lock.RLock()
	defer c.lock.RUnlock()

	item, exists := c.items[key]
	if !exists {
		return nil, false
	}
	if item.isExpired(time.Now().UnixNano()) {
		// Leave and let clean() routine clean it.
		return nil, false
	}
	return item.value, true
}

// Similar to Get() but also remove it.
// NOTE: The eviction callback will be skipped; otherwise, it might simply
// destroy the returned value.
func (c *Cache) Pop(key string) (value any, exists bool) {
	c.lock.Lock()
	defer c.lock.Unlock()

	item, exists := c.items[key]
	if !exists {
		return nil, false
	}

	delete(c.items, key)
	// Skip calling the eviction callback to ensure the value valid.
	itemPool.Put(item)

	if item.isExpired(time.Now().UnixNano()) {
		return nil, false
	}

	return item.value, true
}

// Remove the item of key and invoke the eviction callback.
func (c *Cache) Delete(key string) {
	c.lock.Lock()
	defer c.lock.Unlock()

	item, exists := c.items[key]
	if exists {
		delete(c.items, key)
		c.onEviction(key, item.value)
		itemPool.Put(item)
	}
}

func (c *Cache) getExpireAt(ttl time.Duration) int64 {
	if ttl < 0 {
		return NoTTL
	}
	if ttl == DefaultTTL {
		ttl = c.defaultTTL
	}
	return time.Now().Add(ttl).UnixNano()
}

func (c *Cache) clean(ctx context.Context, interval time.Duration) {
	type kvItem struct {
		key   string
		value any
	}

	ticker := time.NewTicker(interval)
	for {
		select {
		case <-ticker.C:
			// ok
		case <-ctx.Done():
			ticker.Stop()
			c.wg.Done()
			return
		}

		evictedItems := []*kvItem{}
		c.lock.Lock()
		now := time.Now().UnixNano()
		for key, item := range c.items {
			if item.isExpired(now) {
				delete(c.items, key)
				itemPool.Put(item)
				evictedItems = append(evictedItems, &kvItem{
					key:   key,
					value: item.value,
				})
			}
		}
		c.lock.Unlock()

		for _, kv := range evictedItems {
			c.onEviction(kv.key, kv.value)
		}
	}
}
