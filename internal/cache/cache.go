package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type cacheEntry struct {
	value     string
	createdAt time.Time
	expiresAt time.Time
}

// ResponseCache caches deterministic, low-risk responses in memory (and
// optionally in Redis when configured).
type ResponseCache struct {
	mu         sync.RWMutex
	entries    map[string]cacheEntry
	maxEntries int
	ttl        time.Duration
	redis      *RedisClient

	hits   atomic.Int64
	misses atomic.Int64
}

// NewResponseCache creates a bounded response cache.
func NewResponseCache(maxEntries int, ttl time.Duration, redis *RedisClient) *ResponseCache {
	if maxEntries <= 0 {
		maxEntries = 512
	}
	if ttl <= 0 {
		ttl = 15 * time.Minute
	}

	return &ResponseCache{
		entries:    make(map[string]cacheEntry, maxEntries),
		maxEntries: maxEntries,
		ttl:        ttl,
		redis:      redis,
	}
}

// KeyInput holds all parameters that influence a deterministic response.
type KeyInput struct {
	Model        string
	SystemPrompt string
	Temperature  float64
	Task         string
	Prompt       string
}

// BuildKey computes a collision-resistant cache key.
func BuildKey(in KeyInput) string {
	normPrompt := strings.ToLower(strings.Join(strings.Fields(in.Prompt), " "))
	raw := fmt.Sprintf(
		"v1|m=%s|t=%.2f|task=%s|sys=%s|p=%s",
		strings.ToLower(strings.TrimSpace(in.Model)),
		in.Temperature,
		strings.ToUpper(strings.TrimSpace(in.Task)),
		strings.TrimSpace(in.SystemPrompt),
		normPrompt,
	)
	sum := sha256.Sum256([]byte(raw))
	return "resp:" + hex.EncodeToString(sum[:])
}

var nonCacheableMarkers = []string{
	"password", "secret", "api_key", "apikey", "token", "private key",
	"credit card", "ssn", "my name is", "remember that", "right now",
	"current time", "today's date", "weather in", "stock price",
}

// IsCacheable decides whether a request is safe and deterministic enough to
// serve from cache. Multi-turn conversations, sensitive user prompts, and
// high-temperature creative writing are never cached.
func IsCacheable(prompt string, historyLen int, temperature float64) bool {
	if historyLen > 0 {
		return false
	}
	if temperature > 0.8 {
		return false
	}
	trimmed := strings.TrimSpace(prompt)
	if len(trimmed) < 4 || len(trimmed) > 4000 {
		return false
	}

	lower := strings.ToLower(trimmed)
	for _, marker := range nonCacheableMarkers {
		if strings.Contains(lower, marker) {
			return false
		}
	}

	return true
}

// Get looks up a cached response by key.
func (c *ResponseCache) Get(key string) (string, bool) {
	if c == nil || key == "" {
		return "", false
	}

	now := time.Now()
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()

	if ok {
		if now.Before(entry.expiresAt) {
			c.hits.Add(1)
			return entry.value, true
		}
		c.mu.Lock()
		delete(c.entries, key)
		c.mu.Unlock()
	}

	if c.redis != nil {
		if val, ok := c.redis.Get(key); ok && val != "" {
			c.mu.Lock()
			c.entries[key] = cacheEntry{
				value:     val,
				createdAt: now,
				expiresAt: now.Add(c.ttl),
			}
			c.mu.Unlock()
			c.hits.Add(1)
			return val, true
		}
	}

	c.misses.Add(1)
	return "", false
}

// Set stores a response in the cache.
func (c *ResponseCache) Set(key, value string) {
	if c == nil || key == "" || strings.TrimSpace(value) == "" {
		return
	}

	now := time.Now()
	c.mu.Lock()
	if len(c.entries) >= c.maxEntries {
		c.evictLocked(now)
	}
	c.entries[key] = cacheEntry{
		value:     value,
		createdAt: now,
		expiresAt: now.Add(c.ttl),
	}
	c.mu.Unlock()

	if c.redis != nil {
		_ = c.redis.SetEX(key, value, c.ttl)
	}
}

func (c *ResponseCache) evictLocked(now time.Time) {
	var oldestKey string
	var oldestTime time.Time

	for k, v := range c.entries {
		if now.After(v.expiresAt) {
			delete(c.entries, k)
			continue
		}
		if oldestKey == "" || v.createdAt.Before(oldestTime) {
			oldestKey = k
			oldestTime = v.createdAt
		}
	}

	if len(c.entries) >= c.maxEntries && oldestKey != "" {
		delete(c.entries, oldestKey)
	}
}

// Stats returns cache size, hit count, and miss count.
func (c *ResponseCache) Stats() (size int, hits int64, misses int64) {
	if c == nil {
		return 0, 0, 0
	}
	c.mu.RLock()
	size = len(c.entries)
	c.mu.RUnlock()

	return size, c.hits.Load(), c.misses.Load()
}
