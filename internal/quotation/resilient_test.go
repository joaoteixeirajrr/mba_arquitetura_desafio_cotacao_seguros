package quotation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

// countingQuoter records which partners were actually called (thread-safe), so
// a test can assert that an open breaker did NOT contact the partner.
type countingQuoter struct {
	mu       sync.Mutex
	calls    map[string]int
	failOn   map[string]bool
	premiums map[string]int64
}

func newCountingQuoter() *countingQuoter {
	return &countingQuoter{
		calls:    map[string]int{},
		failOn:   map[string]bool{},
		premiums: defaultPremiums(),
	}
}

func (q *countingQuoter) Quote(_ context.Context, p platform.Partner, _ any) (partner.Quote, error) {
	q.mu.Lock()
	q.calls[p.Name]++
	fail := q.failOn[p.Name]
	premium := q.premiums[p.Name]
	q.mu.Unlock()

	if fail {
		return partner.Quote{}, &partner.Error{Partner: p.Name, Status: 503, Reason: "503"}
	}
	return partner.Quote{Partner: p.Name, PremiumCents: premium, Currency: "BRL"}, nil
}

func (q *countingQuoter) callCount(name string) int {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.calls[name]
}

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

// memStore is an in-memory Store with TTL/stale windows for service tests.
type memStore struct {
	mu    sync.Mutex
	data  map[string]cache.Entry
	ttl   time.Duration
	stale time.Duration
	now   func() time.Time
}

func newMemStore(ttl, stale time.Duration, now func() time.Time) *memStore {
	return &memStore{data: map[string]cache.Entry{}, ttl: ttl, stale: stale, now: now}
}

func (m *memStore) Get(_ context.Context, key string) (cache.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	if !ok || e.Age(m.now()) > m.ttl {
		return cache.Entry{}, false
	}
	return e, true
}

func (m *memStore) GetStale(_ context.Context, key string) (cache.Entry, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.data[key]
	if !ok || e.Age(m.now()) > m.stale {
		return cache.Entry{}, false
	}
	return e, true
}

func (m *memStore) Set(_ context.Context, key string, e cache.Entry) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[key] = e
}

func breakerSettings(clock *fakeClock) resilience.Settings {
	return resilience.Settings{
		FailuresToOpen:    5,
		OpenCooldown:      30 * time.Second,
		HalfOpenSuccesses: 2,
		Now:               clock.now,
	}
}

func newResilient(clock *fakeClock, q Quoter, store cache.Store) *ResilientService {
	return NewResilientService(ResilientConfig{
		Partners: threePartners,
		Quoter:   q,
		Breakers: resilience.NewGroup(breakerSettings(clock)),
		Cache:    store,
		TTL:      300 * time.Second,
		Now:      clock.now,
	})
}

func TestResilientAggregatesAllPartnersWhenHealthy(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	q := newCountingQuoter()
	svc := newResilient(clock, q, cache.Noop{})

	resp, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}
	if len(resp.Quotes) != 3 {
		t.Fatalf("%d quotes, want 3", len(resp.Quotes))
	}
	if resp.Degraded != nil {
		t.Fatalf("healthy response marked degraded: %+v", resp.Degraded)
	}
	// Sorted ascending by premium.
	for i := 1; i < len(resp.Quotes); i++ {
		if resp.Quotes[i-1].PremiumCents > resp.Quotes[i].PremiumCents {
			t.Fatalf("quotes not sorted by premium: %+v", resp.Quotes)
		}
	}
}

func TestResilientReturnsPartialWhenOnePartnerFails(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	q := newCountingQuoter()
	q.failOn["partner-flaky"] = true
	svc := newResilient(clock, q, cache.Noop{})

	resp, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("Quote returned an error instead of a partial response: %v", err)
	}
	if len(resp.Quotes) != 2 {
		t.Fatalf("%d quotes, want 2 (one partner down)", len(resp.Quotes))
	}
	if resp.Degraded == nil || resp.Degraded.Source != "partial" {
		t.Fatalf("partial response not marked degraded: %+v", resp.Degraded)
	}
	if len(resp.Degraded.Missing) != 1 || resp.Degraded.Missing[0] != "partner-flaky" {
		t.Fatalf("degraded.missing %v, want [partner-flaky]", resp.Degraded.Missing)
	}
}

// TestResilientOpenBreakerDoesNotCallThePartner is the headline guarantee: once
// the breaker is open, the partner is NOT contacted on the next request.
func TestResilientOpenBreakerDoesNotCallThePartner(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	q := newCountingQuoter()
	q.failOn["partner-flaky"] = true
	svc := newResilient(clock, q, cache.Noop{})

	// 5 requests drive partner-flaky's breaker to open (threshold 5).
	for i := 0; i < 5; i++ {
		if _, err := svc.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
			t.Fatalf("request %d failed unexpectedly: %v", i+1, err)
		}
	}
	callsBefore := q.callCount("partner-flaky")

	// Next request: breaker is open, partner-flaky must NOT be called.
	resp, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("request after open breaker failed: %v", err)
	}
	if got := q.callCount("partner-flaky"); got != callsBefore {
		t.Fatalf("partner-flaky was called %d more time(s) while its breaker was open", got-callsBefore)
	}
	if resp.Degraded == nil {
		t.Fatal("response with an open breaker was not marked degraded")
	}
	// The other two partners still answer.
	if len(resp.Quotes) != 2 {
		t.Fatalf("%d quotes with partner-flaky short-circuited, want 2", len(resp.Quotes))
	}
}

func TestResilientServesFreshCacheWithoutCallingPartners(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	q := newCountingQuoter()
	store := newMemStore(300*time.Second, 24*time.Hour, clock.now)
	svc := newResilient(clock, q, store)

	// First request warms the cache (all partners healthy).
	if _, err := svc.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("warm-up failed: %v", err)
	}
	callsAfterWarmup := q.callCount("partner-slow")

	// Second identical request within the TTL: served from cache, no partner call.
	resp, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("cached request failed: %v", err)
	}
	if got := q.callCount("partner-slow"); got != callsAfterWarmup {
		t.Fatalf("partners were called on a fresh cache hit (%d extra)", got-callsAfterWarmup)
	}
	if resp.Degraded == nil || resp.Degraded.Source != "cache" {
		t.Fatalf("cache hit not marked as served from cache: %+v", resp.Degraded)
	}
}

// TestResilientServesStaleCacheWhenAllPartnersDown exercises the fallback: with
// a warm (now stale) cache and every partner failing, the stale quote is served
// with its age, instead of a 502.
func TestResilientServesStaleCacheWhenAllPartnersDown(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	q := newCountingQuoter()
	store := newMemStore(300*time.Second, 24*time.Hour, clock.now)
	svc := newResilient(clock, q, store)

	// Warm the cache.
	if _, err := svc.Quote(context.Background(), "corretora-a", validRequest()); err != nil {
		t.Fatalf("warm-up failed: %v", err)
	}

	// Age past the TTL (still inside the stale window) and down all partners.
	clock.advance(301 * time.Second)
	q.failOn["partner-slow"] = true
	q.failOn["partner-flaky"] = true
	q.failOn["partner-degrading"] = true

	resp, err := svc.Quote(context.Background(), "corretora-a", validRequest())
	if err != nil {
		t.Fatalf("expected a stale fallback, got error: %v", err)
	}
	if resp.Degraded == nil || resp.Degraded.Source != "cache" {
		t.Fatalf("stale fallback not marked as cache: %+v", resp.Degraded)
	}
	if resp.Degraded.AgeSeconds < 300 {
		t.Fatalf("stale age %ds, expected >= 300s", resp.Degraded.AgeSeconds)
	}
	if len(resp.Quotes) != 3 {
		t.Fatalf("%d quotes from stale cache, want 3", len(resp.Quotes))
	}
}

func TestResilientFailsWhenNoLiveQuoteAndNoCache(t *testing.T) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	q := newCountingQuoter()
	q.failOn["partner-slow"] = true
	q.failOn["partner-flaky"] = true
	q.failOn["partner-degrading"] = true
	svc := newResilient(clock, q, cache.Noop{})

	if _, err := svc.Quote(context.Background(), "corretora-a", validRequest()); err == nil {
		t.Fatal("all partners down and no cache, but the request succeeded")
	}
}
