package quotation

import (
	"context"
	"errors"
	"sort"
	"sync"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

// Observer receives the business events the PoC must prove. It is the seam
// between the service and the telemetry (internal/platform), so the service has
// no direct dependency on OpenTelemetry. A nil observer is a no-op.
type Observer interface {
	// CacheHit/CacheMiss record cache outcomes per tenant.
	CacheHit(ctx context.Context, tenant string)
	CacheMiss(ctx context.Context, tenant string)
	// PartnerLatency records the duration of a live partner call and whether it
	// failed, with domain meaning (per partner).
	PartnerLatency(ctx context.Context, partnerName string, d time.Duration, failed bool)
	// ShortCircuited marks (on the trace) a request that skipped a partner
	// because its breaker was open — the partner was NOT called.
	ShortCircuited(ctx context.Context, partnerName string)
	// ServedFromCache marks (on the trace) a response served from cache.
	ServedFromCache(ctx context.Context, stale bool)
}

type nopObserver struct{}

func (nopObserver) CacheHit(context.Context, string)                            {}
func (nopObserver) CacheMiss(context.Context, string)                           {}
func (nopObserver) PartnerLatency(context.Context, string, time.Duration, bool) {}
func (nopObserver) ShortCircuited(context.Context, string)                      {}
func (nopObserver) ServedFromCache(context.Context, bool)                       {}

// ResilientService is the "depois" of the challenge: it protects the partner
// boundary with a per-partner circuit breaker and a timeout, serves a
// tenant-scoped cache, aggregates the partners in parallel (the optional
// bonus), and falls back to a stale cached quote or an explicit partial
// response instead of failing the whole request. It never invents a premium.
type ResilientService struct {
	partners []platform.Partner
	quoter   Quoter
	breakers *resilience.Group
	cache    cache.Store
	ttl      time.Duration
	observer Observer
	now      func() time.Time
}

// ResilientConfig wires a ResilientService. Cache and Observer may be nil (they
// default to a no-op cache and a no-op observer).
type ResilientConfig struct {
	Partners []platform.Partner
	Quoter   Quoter
	Breakers *resilience.Group
	Cache    cache.Store
	TTL      time.Duration
	Observer Observer
	Now      func() time.Time
}

// NewResilientService builds the resilient service used by cmd/quotation-api.
func NewResilientService(cfg ResilientConfig) *ResilientService {
	if cfg.Cache == nil {
		cfg.Cache = cache.Noop{}
	}
	if cfg.Observer == nil {
		cfg.Observer = nopObserver{}
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	return &ResilientService{
		partners: cfg.Partners,
		quoter:   cfg.Quoter,
		breakers: cfg.Breakers,
		cache:    cfg.Cache,
		ttl:      cfg.TTL,
		observer: cfg.Observer,
		now:      cfg.Now,
	}
}

type partnerResult struct {
	name  string
	quote partner.Quote
	ok    bool
	open  bool // breaker was open: partner was not called
}

// Quote answers a quotation, degrading instead of failing. Flow:
//  1. a fresh cache hit is served immediately (cheapest, no partner call);
//  2. otherwise the partners are queried in parallel, each guarded by its own
//     breaker (open => the partner is not called);
//  3. if every partner is unavailable, a stale cached quote is served as an
//     explicit fallback; only if there is none does the request fail.
func (s *ResilientService) Quote(ctx context.Context, tenant string, request Request) (Response, error) {
	start := s.now()
	key := cache.Key(tenant, request.Risk())

	// 1. Fresh cache hit: no partner is contacted.
	if entry, ok := s.cache.Get(ctx, key); ok {
		s.observer.CacheHit(ctx, tenant)
		s.observer.ServedFromCache(ctx, false)
		return Response{
			TenantID:  tenant,
			Quotes:    sortByPremium(entry.Quotes),
			ElapsedMs: elapsed(start, s.now()),
			Degraded: &Degraded{
				Source:     "cache",
				AgeSeconds: int64(entry.Age(s.now()).Seconds()),
			},
		}, nil
	}
	s.observer.CacheMiss(ctx, tenant)

	// 2. Query the partners in parallel, each behind its breaker.
	results := s.queryPartners(ctx, tenant, request)

	live := make([]partner.Quote, 0, len(results))
	var missing []string
	for _, r := range results {
		if r.ok {
			live = append(live, r.quote)
		} else {
			missing = append(missing, r.name)
		}
	}

	// All partners available and answered: normal, complete response. Cache it.
	if len(live) == len(s.partners) {
		s.cache.Set(ctx, key, cache.Entry{Quotes: live, StoredAt: s.now()})
		return Response{
			TenantID:  tenant,
			Quotes:    sortByPremium(live),
			ElapsedMs: elapsed(start, s.now()),
		}, nil
	}

	// Some partners answered: serve a partial, explicitly degraded response.
	if len(live) > 0 {
		return Response{
			TenantID:  tenant,
			Quotes:    sortByPremium(live),
			ElapsedMs: elapsed(start, s.now()),
			Degraded:  &Degraded{Source: "partial", Missing: missing},
		}, nil
	}

	// 3. No live quote at all: fall back to a stale cached quote if there is one.
	if entry, ok := s.cache.GetStale(ctx, key); ok {
		s.observer.ServedFromCache(ctx, true)
		return Response{
			TenantID:  tenant,
			Quotes:    sortByPremium(entry.Quotes),
			ElapsedMs: elapsed(start, s.now()),
			Degraded: &Degraded{
				Source:     "cache",
				Missing:    missing,
				AgeSeconds: int64(entry.Age(s.now()).Seconds()),
			},
		}, nil
	}

	// Nothing to serve: surface the partner failure, preserving the 502 contract.
	return Response{}, firstPartnerError(results)
}

func (s *ResilientService) queryPartners(ctx context.Context, tenant string, request Request) []partnerResult {
	forPartner := partnerRequest{Broker: tenant, Request: request}
	results := make([]partnerResult, len(s.partners))

	var wg sync.WaitGroup
	for i, p := range s.partners {
		wg.Add(1)
		go func(i int, p platform.Partner) {
			defer wg.Done()
			results[i] = s.callOne(ctx, tenant, p, forPartner)
		}(i, p)
	}
	wg.Wait()
	return results
}

func (s *ResilientService) callOne(ctx context.Context, tenant string, p platform.Partner, req partnerRequest) partnerResult {
	breaker := s.breakers.For(p.Name)

	// Open breaker: the partner is NOT called. This is the whole point of the
	// breaker — it saves the per-call cost and the wait.
	if !breaker.Allow() {
		s.observer.ShortCircuited(ctx, p.Name)
		return partnerResult{name: p.Name, open: true}
	}

	callStart := s.now()
	quote, err := s.quoter.Quote(ctx, p, req)
	s.observer.PartnerLatency(ctx, p.Name, s.now().Sub(callStart), err != nil)

	if err != nil {
		breaker.Failure()
		return partnerResult{name: p.Name}
	}
	breaker.Success()
	return partnerResult{name: p.Name, quote: quote, ok: true}
}

func sortByPremium(quotes []partner.Quote) []partner.Quote {
	out := make([]partner.Quote, len(quotes))
	copy(out, quotes)
	sort.Slice(out, func(i, j int) bool { return out[i].PremiumCents < out[j].PremiumCents })
	return out
}

func elapsed(start, end time.Time) int64 { return end.Sub(start).Milliseconds() }

// firstPartnerError returns a *partner.Error so the handler keeps the existing
// 502 "partner insurer unavailable" contract. Short-circuited partners are
// reported too, so the failure names a partner even when none was called.
func firstPartnerError(results []partnerResult) error {
	for _, r := range results {
		if !r.ok {
			reason := "partner insurer unavailable"
			if r.open {
				reason = "partner insurer circuit open"
			}
			return &partner.Error{Partner: r.name, Reason: reason}
		}
	}
	return errors.New("no partner answered")
}
