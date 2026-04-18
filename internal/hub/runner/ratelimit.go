package runner

import (
	"sync"
	"time"
)

// rateLimiter implements a simple per-key token bucket. Capacity = burst,
// refill at refillRatePerSec tokens/second. Used by hub.Runner to cap how
// fast a single agent can be hit (PROJECT.md §7.6: 30 collects/min default).
type rateLimiter struct {
	mu       sync.Mutex
	buckets  map[string]*bucket
	capacity float64
	refill   float64 // tokens per second
}

type bucket struct {
	tokens  float64
	updated time.Time
}

func newRateLimiter(perMinute int) *rateLimiter {
	if perMinute <= 0 {
		perMinute = 30
	}
	return &rateLimiter{
		buckets:  map[string]*bucket{},
		capacity: float64(perMinute),
		refill:   float64(perMinute) / 60.0,
	}
}

// allow returns true if a token was available and consumed for key.
// Buckets are lazily initialized at full capacity (burst-friendly first
// call) so a freshly enrolled agent can absorb a quick discovery flurry.
func (r *rateLimiter) allow(key string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	now := time.Now()
	b, ok := r.buckets[key]
	if !ok {
		b = &bucket{tokens: r.capacity, updated: now}
		r.buckets[key] = b
	}
	elapsed := now.Sub(b.updated).Seconds()
	b.tokens += elapsed * r.refill
	if b.tokens > r.capacity {
		b.tokens = r.capacity
	}
	b.updated = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
