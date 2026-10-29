package cache

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/redis/go-redis/v9"
)

// Store is the cache contract the quotation service depends on. Both the Redis
// store and the in-memory fake used in tests implement it.
type Store interface {
	// Get returns a FRESH entry (stored within the TTL) for the key. The second
	// result is false on a miss. A backend failure degrades to a miss, never an
	// error: the cache must not be able to take the platform down.
	Get(ctx context.Context, key string) (Entry, bool)
	// GetStale returns an entry that may be past its TTL but still within the
	// stale window, for use as an explicit fallback when the partner boundary is
	// unavailable. The returned entry's Age tells the caller how old it is.
	GetStale(ctx context.Context, key string) (Entry, bool)
	// Set stores an aggregated quote under the key.
	Set(ctx context.Context, key string, entry Entry)
}

// Noop is a cache that stores nothing. It is used when CACHE_ENABLED=false so
// the service runs the no-cache path without special-casing a nil cache.
type Noop struct{}

func (Noop) Get(context.Context, string) (Entry, bool)      { return Entry{}, false }
func (Noop) GetStale(context.Context, string) (Entry, bool) { return Entry{}, false }
func (Noop) Set(context.Context, string, Entry)             {}

// Redis is the tenant-scoped cache backed by Redis. Freshness is decided by the
// entry's StoredAt relative to TTL (not by Redis key expiry), so that a value
// can be fresh for TTL yet still retrievable as stale up to StaleFor — the two
// windows the fallback policy needs. Redis key expiry is set to StaleFor so the
// ephemeral store discards personal data on its own (LGPD retention).
type Redis struct {
	client *redis.Client
	ttl    time.Duration
	stale  time.Duration
	now    func() time.Time
}

// NewRedis builds a Redis-backed cache. ttl is the freshness window; staleFor is
// how long past the TTL an entry may still serve as a fallback (and also the
// Redis key expiry).
func NewRedis(addr string, ttl, staleFor time.Duration) *Redis {
	return &Redis{
		client: redis.NewClient(&redis.Options{Addr: addr}),
		ttl:    ttl,
		stale:  staleFor,
		now:    time.Now,
	}
}

// Ping checks connectivity; callers may log but should not fail boot on error.
func (r *Redis) Ping(ctx context.Context) error {
	return r.client.Ping(ctx).Err()
}

// Close releases the Redis connection pool.
func (r *Redis) Close() error { return r.client.Close() }

func (r *Redis) read(ctx context.Context, key string) (Entry, bool) {
	raw, err := r.client.Get(ctx, key).Bytes()
	if err != nil {
		// Miss (redis.Nil) or any backend failure both degrade to a miss.
		if !errors.Is(err, redis.Nil) {
			// A backend error is a miss, not a crash: the cache never takes the
			// platform down.
			return Entry{}, false
		}
		return Entry{}, false
	}
	var entry Entry
	if err := json.Unmarshal(raw, &entry); err != nil {
		return Entry{}, false
	}
	return entry, true
}

// Get returns an entry only if it is still within the TTL freshness window.
func (r *Redis) Get(ctx context.Context, key string) (Entry, bool) {
	entry, ok := r.read(ctx, key)
	if !ok {
		return Entry{}, false
	}
	if entry.Age(r.now()) > r.ttl {
		return Entry{}, false
	}
	return entry, true
}

// GetStale returns an entry within the stale window (past TTL, under StaleFor).
func (r *Redis) GetStale(ctx context.Context, key string) (Entry, bool) {
	entry, ok := r.read(ctx, key)
	if !ok {
		return Entry{}, false
	}
	if entry.Age(r.now()) > r.stale {
		return Entry{}, false
	}
	return entry, true
}

// Set stores the entry with a Redis expiry of StaleFor, so the key disappears on
// its own once it can no longer serve even as a fallback.
func (r *Redis) Set(ctx context.Context, key string, entry Entry) {
	if entry.StoredAt.IsZero() {
		entry.StoredAt = r.now()
	}
	raw, err := json.Marshal(entry)
	if err != nil {
		return
	}
	// Best effort: a failed write is a future miss, never a request failure.
	_ = r.client.Set(ctx, key, raw, r.stale).Err()
}
