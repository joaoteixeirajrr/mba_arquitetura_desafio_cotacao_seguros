package platform

import (
	"fmt"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	Port       string
	Partners   []Partner
	Tenants    []string
	Telemetry  Telemetry
	Resilience Resilience
	Cache      Cache
}

type Partner struct {
	Name    string
	BaseURL string
}

// Resilience holds the circuit-breaker and timeout parameters that protect the
// partner boundary. Every value here is defended in docs/sad.md section 4.
type Resilience struct {
	// PartnerTimeout is the hard ceiling for a single partner call. A call that
	// exceeds it is cancelled and counts as a failure for the breaker. Defended
	// against partner-slow (~1.7s) and partner-degrading (up to 6s).
	PartnerTimeout time.Duration
	// FailuresToOpen is the number of consecutive failures (errors or timeouts)
	// that trip a per-partner breaker from closed to open.
	FailuresToOpen int
	// OpenCooldown is how long the breaker stays open before letting a single
	// probe through (half-open). While open, the partner is NOT called.
	OpenCooldown time.Duration
	// HalfOpenSuccesses is how many consecutive probe successes close the breaker.
	HalfOpenSuccesses int
}

// Cache holds the tenant-scoped Redis cache parameters. TTL is a business
// decision bounded by the 24h commercial ceiling; defended in docs/sad.md.
type Cache struct {
	// Enabled turns the Redis cache on. When false, the service degrades to the
	// no-cache path without failing.
	Enabled bool
	// RedisAddr is the host:port of the Redis used as cache.
	RedisAddr string
	// TTL is how long an aggregated quote stays fresh in the cache.
	TTL time.Duration
	// StaleFor is how long past the TTL a cached quote may still be served as an
	// explicit fallback when the partner boundary is unavailable (breaker open).
	StaleFor time.Duration
}

type Telemetry struct {
	ServiceName string
	Endpoint    string
	Enabled     bool
}

const defaultEndpoints = "partner-slow=http://localhost:9001," +
	"partner-flaky=http://localhost:9002," +
	"partner-degrading=http://localhost:9003"

const defaultTenants = "corretora-a,corretora-b"

const defaultCollector = "http://localhost:4317"

const defaultServiceName = "quotation-api"

// Resilience defaults — defended in docs/sad.md section 4.
const (
	defaultPartnerTimeout    = 2 * time.Second
	defaultFailuresToOpen    = 5
	defaultOpenCooldown      = 30 * time.Second
	defaultHalfOpenSuccesses = 2
)

// Cache defaults — defended in docs/sad.md sections 4 and 8.
const (
	defaultRedisAddr = "localhost:6379"
	defaultCacheTTL  = 300 * time.Second
	defaultStaleFor  = 24 * time.Hour
)

func LoadConfig(env func(string) string) (Config, error) {
	cfg := Config{Port: text(env, "PORT", "8080")}

	var err error
	if cfg.Partners, err = parsePartners(text(env, "PARTNER_ENDPOINTS", defaultEndpoints)); err != nil {
		return Config{}, err
	}
	if cfg.Tenants, err = parseTenants(text(env, "TENANTS", defaultTenants)); err != nil {
		return Config{}, err
	}
	if cfg.Telemetry, err = parseTelemetry(env); err != nil {
		return Config{}, err
	}
	if cfg.Resilience, err = parseResilience(env); err != nil {
		return Config{}, err
	}
	if cfg.Cache, err = parseCache(env); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func parseResilience(env func(string) string) (Resilience, error) {
	timeout, err := duration(env, "PARTNER_TIMEOUT", defaultPartnerTimeout)
	if err != nil {
		return Resilience{}, err
	}
	failures, err := positiveInt(env, "BREAKER_FAILURES_TO_OPEN", defaultFailuresToOpen)
	if err != nil {
		return Resilience{}, err
	}
	cooldown, err := duration(env, "BREAKER_OPEN_COOLDOWN", defaultOpenCooldown)
	if err != nil {
		return Resilience{}, err
	}
	successes, err := positiveInt(env, "BREAKER_HALF_OPEN_SUCCESSES", defaultHalfOpenSuccesses)
	if err != nil {
		return Resilience{}, err
	}
	return Resilience{
		PartnerTimeout:    timeout,
		FailuresToOpen:    failures,
		OpenCooldown:      cooldown,
		HalfOpenSuccesses: successes,
	}, nil
}

func parseCache(env func(string) string) (Cache, error) {
	enabled, err := boolean(env, "CACHE_ENABLED", true)
	if err != nil {
		return Cache{}, err
	}
	ttl, err := duration(env, "CACHE_TTL", defaultCacheTTL)
	if err != nil {
		return Cache{}, err
	}
	staleFor, err := duration(env, "CACHE_STALE_FOR", defaultStaleFor)
	if err != nil {
		return Cache{}, err
	}
	return Cache{
		Enabled:   enabled,
		RedisAddr: text(env, "REDIS_ADDR", defaultRedisAddr),
		TTL:       ttl,
		StaleFor:  staleFor,
	}, nil
}

func parseTelemetry(env func(string) string) (Telemetry, error) {
	telemetry := Telemetry{
		ServiceName: text(env, "OTEL_SERVICE_NAME", defaultServiceName),
		Endpoint:    strings.TrimSpace(text(env, "OTEL_EXPORTER_OTLP_ENDPOINT", defaultCollector)),
	}

	switch disabled := strings.ToLower(strings.TrimSpace(env("OTEL_SDK_DISABLED"))); disabled {
	case "", "false":
		telemetry.Enabled = true
	case "true":
		return Telemetry{Enabled: false}, nil
	default:
		return Telemetry{}, fmt.Errorf("OTEL_SDK_DISABLED: %q is neither true nor false", disabled)
	}

	if u, err := url.Parse(telemetry.Endpoint); err != nil || u.Scheme == "" || u.Host == "" {
		return Telemetry{}, fmt.Errorf("OTEL_EXPORTER_OTLP_ENDPOINT: %q is not an absolute URL", telemetry.Endpoint)
	}
	return telemetry, nil
}

func parsePartners(raw string) ([]Partner, error) {
	var partners []Partner
	seen := map[string]bool{}

	for _, item := range strings.Split(raw, ",") {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}

		name, endpoint, found := strings.Cut(item, "=")
		name, endpoint = strings.TrimSpace(name), strings.TrimSpace(endpoint)
		if !found || name == "" || endpoint == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not in the name=url format", item)
		}

		if u, err := url.Parse(endpoint); err != nil || u.Scheme == "" || u.Host == "" {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: %q is not an absolute URL", endpoint)
		}
		if seen[name] {
			return nil, fmt.Errorf("PARTNER_ENDPOINTS: partner %q is repeated", name)
		}

		seen[name] = true
		partners = append(partners, Partner{Name: name, BaseURL: strings.TrimRight(endpoint, "/")})
	}

	if len(partners) == 0 {
		return nil, fmt.Errorf("PARTNER_ENDPOINTS: at least one partner is required")
	}
	return partners, nil
}

func parseTenants(raw string) ([]string, error) {
	var tenants []string
	for _, t := range strings.Split(raw, ",") {
		if t = strings.TrimSpace(t); t != "" {
			tenants = append(tenants, t)
		}
	}
	if len(tenants) == 0 {
		return nil, fmt.Errorf("TENANTS: at least one broker is required")
	}
	return tenants, nil
}

func Environment() func(string) string { return os.Getenv }

func text(env func(string) string, key, fallback string) string {
	if v := env(key); v != "" {
		return v
	}
	return fallback
}

func duration(env func(string) string, key string, fallback time.Duration) (time.Duration, error) {
	raw := strings.TrimSpace(env(key))
	if raw == "" {
		return fallback, nil
	}
	d, err := time.ParseDuration(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not a valid duration (e.g. 2s, 500ms)", key, raw)
	}
	if d <= 0 {
		return 0, fmt.Errorf("%s: %q must be greater than zero", key, raw)
	}
	return d, nil
}

func positiveInt(env func(string) string, key string, fallback int) (int, error) {
	raw := strings.TrimSpace(env(key))
	if raw == "" {
		return fallback, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s: %q is not an integer", key, raw)
	}
	if n <= 0 {
		return 0, fmt.Errorf("%s: %q must be greater than zero", key, raw)
	}
	return n, nil
}

func boolean(env func(string) string, key string, fallback bool) (bool, error) {
	raw := strings.ToLower(strings.TrimSpace(env(key)))
	switch raw {
	case "":
		return fallback, nil
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, fmt.Errorf("%s: %q is neither true nor false", key, raw)
	}
}
