package cache

import (
	"context"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

func sampleRisk() Risk {
	return Risk{
		Document:   "12345678901",
		BirthYear:  1988,
		Plate:      "ABC1D23",
		Model:      "Gol 1.0",
		Year:       2020,
		ValueCents: 8500000,
		Coverage:   "comprehensive",
	}
}

// TestKeyIsolatesTenants is the reproves-alone requirement: the same risk for
// two different brokers MUST produce two different keys.
func TestKeyIsolatesTenants(t *testing.T) {
	a := Key("corretora-a", sampleRisk())
	b := Key("corretora-b", sampleRisk())

	if a == b {
		t.Fatalf("the same risk produced the same key for two brokers: %s", a)
	}
}

func TestKeyContainsTheTenant(t *testing.T) {
	key := Key("corretora-a", sampleRisk())
	if want := "quote:v1:corretora-a:"; !hasPrefix(key, want) {
		t.Fatalf("key %q does not start with the tenant prefix %q", key, want)
	}
}

func TestKeyDoesNotLeakPersonalData(t *testing.T) {
	r := sampleRisk()
	key := Key("corretora-a", r)
	// CPF and plate must be hashed away, never present verbatim in the key.
	if contains(key, r.Document) {
		t.Fatalf("key %q leaks the driver document", key)
	}
	if contains(key, r.Plate) {
		t.Fatalf("key %q leaks the plate", key)
	}
}

func TestKeyIsStableUnderCasingAndSpacing(t *testing.T) {
	r1 := sampleRisk()
	r2 := sampleRisk()
	r2.Plate = "  abc1d23  "
	r2.Document = " 12345678901 "

	if Key("corretora-a", r1) != Key(" Corretora-A ", r2) {
		t.Fatal("normalization is not stable: equivalent inputs produced different keys")
	}
}

func TestKeyChangesWithTheRisk(t *testing.T) {
	base := Key("corretora-a", sampleRisk())
	other := sampleRisk()
	other.Plate = "XYZ9Z99"
	if base == Key("corretora-a", other) {
		t.Fatal("different plates produced the same key")
	}
}

// fakeStore is a deterministic in-memory Store with the same TTL/stale
// semantics as Redis, driven by a fake clock.
type fakeStore struct {
	ttl   time.Duration
	stale time.Duration
	now   func() time.Time
	data  map[string]Entry
}

func newFakeStore(ttl, stale time.Duration, now func() time.Time) *fakeStore {
	return &fakeStore{ttl: ttl, stale: stale, now: now, data: map[string]Entry{}}
}

func (f *fakeStore) Get(_ context.Context, key string) (Entry, bool) {
	e, ok := f.data[key]
	if !ok || e.Age(f.now()) > f.ttl {
		return Entry{}, false
	}
	return e, true
}

func (f *fakeStore) GetStale(_ context.Context, key string) (Entry, bool) {
	e, ok := f.data[key]
	if !ok || e.Age(f.now()) > f.stale {
		return Entry{}, false
	}
	return e, true
}

func (f *fakeStore) Set(_ context.Context, key string, e Entry) {
	if e.StoredAt.IsZero() {
		e.StoredAt = f.now()
	}
	f.data[key] = e
}

func TestFreshAndStaleWindows(t *testing.T) {
	clock := &struct{ t time.Time }{t: time.Unix(1_000_000, 0)}
	now := func() time.Time { return clock.t }

	store := newFakeStore(300*time.Second, 24*time.Hour, now)
	key := Key("corretora-a", sampleRisk())
	store.Set(context.Background(), key, Entry{
		Quotes:   []partner.Quote{{Partner: "partner-flaky", PremiumCents: 90000}},
		StoredAt: now(),
	})

	// Within the TTL: a fresh hit.
	if _, ok := store.Get(context.Background(), key); !ok {
		t.Fatal("fresh entry missed inside the TTL")
	}

	// Past the TTL but within the stale window: fresh miss, stale hit.
	clock.t = clock.t.Add(301 * time.Second)
	if _, ok := store.Get(context.Background(), key); ok {
		t.Fatal("entry past the TTL was served as fresh")
	}
	stale, ok := store.GetStale(context.Background(), key)
	if !ok {
		t.Fatal("entry inside the stale window was not served as stale")
	}
	if age := stale.Age(now()); age < 300*time.Second {
		t.Fatalf("stale age %s, expected past the TTL", age)
	}

	// Past the stale window: gone entirely.
	clock.t = clock.t.Add(24 * time.Hour)
	if _, ok := store.GetStale(context.Background(), key); ok {
		t.Fatal("entry past the stale window was still served")
	}
}

func TestNoopNeverHits(t *testing.T) {
	var s Store = Noop{}
	s.Set(context.Background(), "k", Entry{})
	if _, ok := s.Get(context.Background(), "k"); ok {
		t.Fatal("Noop returned a hit")
	}
	if _, ok := s.GetStale(context.Background(), "k"); ok {
		t.Fatal("Noop returned a stale hit")
	}
}

func hasPrefix(s, p string) bool { return len(s) >= len(p) && s[:len(p)] == p }

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
