package cache

import (
	"sync"
	"time"
)

type Item struct {
	Value     any
	FetchedAt time.Time
	ExpiresAt time.Time
}

type TTLCache struct {
	mu       sync.RWMutex
	items    map[string]Item
	stopCh   chan struct{}
	stopOnce sync.Once
}

func New(evictionInterval time.Duration) *TTLCache {
	c := &TTLCache{
		items:  make(map[string]Item),
		stopCh: make(chan struct{}),
	}
	go c.startEviction(evictionInterval)
	return c
}

func (c *TTLCache) Get(key string, maxStaleness time.Duration) (any, time.Duration, bool) {
	c.mu.RLock()
	item, ok := c.items[key]
	c.mu.RUnlock()
	if !ok {
		return nil, 0, false
	}

	now := time.Now()
	if now.After(item.ExpiresAt) {
		c.mu.Lock()
		delete(c.items, key)
		c.mu.Unlock()
		return nil, 0, false
	}

	staleness := now.Sub(item.FetchedAt)
	if maxStaleness >= 0 && staleness > maxStaleness {
		return nil, staleness, false
	}
	return item.Value, staleness, true
}

func (c *TTLCache) Set(key string, value any, ttl time.Duration) {
	now := time.Now()
	c.mu.Lock()
	c.items[key] = Item{
		Value:     value,
		FetchedAt: now,
		ExpiresAt: now.Add(ttl),
	}
	c.mu.Unlock()
}

func (c *TTLCache) Stop() {
	c.stopOnce.Do(func() { close(c.stopCh) })
}

func (c *TTLCache) startEviction(evictionInterval time.Duration) {
	ticker := time.NewTicker(evictionInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			c.evictExpired()
		case <-c.stopCh:
			return
		}
	}
}

func (c *TTLCache) evictExpired() {
	now := time.Now()
	c.mu.Lock()
	for key, item := range c.items {
		if now.After(item.ExpiresAt) {
			delete(c.items, key)
		}
	}
	c.mu.Unlock()
}
