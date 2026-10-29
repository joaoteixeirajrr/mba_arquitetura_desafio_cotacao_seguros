package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/quotation"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/resilience"
)

func main() {
	cfg, err := platform.LoadConfig(platform.Environment())
	if err != nil {
		log.Fatalf("invalid configuration: %v", err)
	}

	ctx, stopListening := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopListening()

	stopTelemetry, err := platform.StartTelemetry(context.Background(), cfg.Telemetry)
	if err != nil {
		log.Fatalf("telemetry: %v", err)
	}

	// Business metrics + trace markings. If they fail to build, degrade to the
	// no-op observer rather than refusing to boot: telemetry must not take the
	// platform down.
	var observer quotation.Observer
	metrics, err := platform.NewMetrics()
	if err != nil {
		log.Printf("quotation-api: business metrics off: %v", err)
		observer = nil
	} else {
		observer = metrics
	}

	// Per-partner circuit breakers. The OnTransition hook feeds both the
	// transition counter and the state gauge.
	breakers := resilience.NewGroup(resilience.Settings{
		FailuresToOpen:    cfg.Resilience.FailuresToOpen,
		OpenCooldown:      cfg.Resilience.OpenCooldown,
		HalfOpenSuccesses: cfg.Resilience.HalfOpenSuccesses,
		OnTransition: func(p string, from, to resilience.State) {
			log.Printf("quotation-api: breaker %s %s -> %s", p, from, to)
			if metrics != nil {
				metrics.RecordBreakerTransition(p, from.String(), to.String(), to.Code())
			}
		},
	})

	// Tenant-scoped cache. Noop when disabled; a failed ping only degrades to
	// the no-cache path, it does not stop the boot.
	store := buildCache(cfg.Cache)

	service := quotation.NewResilientService(quotation.ResilientConfig{
		Partners: cfg.Partners,
		Quoter:   partner.NewClient(partner.WithTimeout(cfg.Resilience.PartnerTimeout)),
		Breakers: breakers,
		Cache:    store,
		TTL:      cfg.Cache.TTL,
		Observer: observer,
	})

	api := quotation.NewAPI(service, cfg.Tenants)

	server := &http.Server{
		Addr:              ":" + cfg.Port,
		Handler:           platform.InstrumentHandler(api.Routes(), cfg.Telemetry.ServiceName),
		ReadHeaderTimeout: 5 * time.Second,
	}

	go func() {
		<-ctx.Done()
		log.Print("quotation-api: signal received, shutting down")
		shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdown); err != nil {
			log.Printf("quotation-api: forced shutdown: %v", err)
		}
		if err := stopTelemetry(shutdown); err != nil {
			log.Printf("quotation-api: telemetry shutdown: %v", err)
		}
	}()

	log.Printf("quotation-api listening on %s | partners=%d brokers=%v timeout=%s breaker=%d/%s/%d cache=%t ttl=%s",
		server.Addr, len(cfg.Partners), cfg.Tenants,
		cfg.Resilience.PartnerTimeout, cfg.Resilience.FailuresToOpen, cfg.Resilience.OpenCooldown,
		cfg.Resilience.HalfOpenSuccesses, cfg.Cache.Enabled, cfg.Cache.TTL)
	if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatalf("server stopped: %v", err)
	}
}

// buildCache returns a Redis-backed store when the cache is enabled and
// reachable, otherwise a no-op store. A failed connection degrades to no-cache
// rather than failing the boot.
func buildCache(cfg platform.Cache) cache.Store {
	if !cfg.Enabled {
		log.Print("quotation-api: cache disabled (CACHE_ENABLED=false)")
		return cache.Noop{}
	}
	redis := cache.NewRedis(cfg.RedisAddr, cfg.TTL, cfg.StaleFor)
	pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := redis.Ping(pingCtx); err != nil {
		log.Printf("quotation-api: redis unreachable at %s, running without cache: %v", cfg.RedisAddr, err)
		_ = redis.Close()
		return cache.Noop{}
	}
	log.Printf("quotation-api: cache on | redis=%s ttl=%s stale_for=%s", cfg.RedisAddr, cfg.TTL, cfg.StaleFor)
	return redis
}
