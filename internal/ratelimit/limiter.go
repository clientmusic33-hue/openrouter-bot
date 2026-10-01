// Package ratelimit implements multi-tier, per-category request rate limiting
// with in-memory windows and optional Redis backing.
package ratelimit

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"openrouter-bot/internal/cache"
)

// Roles supported by the rate limiter.
const (
	RoleGuest   = "GUEST"
	RoleUser    = "USER"
	RolePremium = "PREMIUM"
	RoleAdmin   = "ADMIN"
)

// Categories of bot operations that carry independent rate limits.
const (
	CategoryChat     = "chat"
	CategoryAgent    = "agent"
	CategoryResearch = "research"
	CategoryImage    = "image"
	CategoryFile     = "file"
)

type windowState struct {
	start time.Time
	count int
}

// Limiter enforces per-user, per-role, and per-category request limits per minute.
type Limiter struct {
	mu      sync.Mutex
	windows map[string]windowState
	redis   *cache.RedisClient
}

// New creates a new Limiter with optional Redis backing.
func New(redis *cache.RedisClient) *Limiter {
	return &Limiter{
		windows: make(map[string]windowState),
		redis:   redis,
	}
}

// EffectiveLimit calculates the allowed requests per minute for a given base
// limit, user role, and action category.
//
// Compatibility baseline: if basePerMinute <= 0, rate limiting is disabled (0).
// Admins are never rate limited (0).
func EffectiveLimit(basePerMinute int, role, category string) int {
	if basePerMinute <= 0 {
		return 0
	}

	role = strings.ToUpper(strings.TrimSpace(role))
	if role == RoleAdmin {
		return 0
	}

	limit := float64(basePerMinute)

	// Role multiplier.
	switch role {
	case RoleGuest:
		limit = maxFloat(1, limit*0.5)
	case RolePremium:
		limit = limit * 2.0
	default: // USER
	}

	// Category multiplier (heavy operations have tighter limits unless base is very small).
	switch strings.ToLower(strings.TrimSpace(category)) {
	case CategoryAgent, CategoryResearch:
		limit = maxFloat(1, limit*0.4)
	case CategoryImage, CategoryFile:
		limit = maxFloat(1, limit*0.5)
	default: // CategoryChat
	}

	out := int(limit)
	if out < 1 {
		out = 1
	}
	return out
}

// Allow checks and records one request for userID under role and category.
func (l *Limiter) Allow(userID, role, category string, basePerMinute int) error {
	limit := EffectiveLimit(basePerMinute, role, category)
	if limit <= 0 {
		return nil
	}

	if category == "" {
		category = CategoryChat
	}
	key := fmt.Sprintf("rl:%s:%s", userID, strings.ToLower(category))

	if l != nil && l.redis != nil {
		if count, err := l.redis.IncrWithTTL(key, time.Minute); err == nil {
			if int(count) > limit {
				return fmt.Errorf("rate limit of %d %s requests/min exceeded", limit, category)
			}
			return nil
		}
	}

	if l == nil {
		return nil
	}

	l.mu.Lock()
	defer l.mu.Unlock()

	now := time.Now()
	w := l.windows[key]
	if w.start.IsZero() || now.Sub(w.start) >= time.Minute {
		w = windowState{start: now, count: 0}
	}

	if w.count >= limit {
		retryAfter := (time.Minute - now.Sub(w.start)).Round(time.Second)
		if retryAfter < time.Second {
			retryAfter = time.Second
		}
		return fmt.Errorf(
			"rate limit of %d %s requests per minute exceeded, retry in %s",
			limit, category, retryAfter,
		)
	}

	w.count++
	l.windows[key] = w
	return nil
}

func maxFloat(a, b float64) float64 {
	if a > b {
		return a
	}
	return b
}
