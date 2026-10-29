package platform

import (
	"reflect"
	"testing"
	"time"
)

func env(pairs map[string]string) func(string) string {
	return func(key string) string { return pairs[key] }
}

func TestDefaultConfigPointsAtTheComposeEnvironment(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Port != "8080" {
		t.Errorf("port %q, want 8080", cfg.Port)
	}
	want := []Partner{
		{Name: "partner-slow", BaseURL: "http://localhost:9001"},
		{Name: "partner-flaky", BaseURL: "http://localhost:9002"},
		{Name: "partner-degrading", BaseURL: "http://localhost:9003"},
	}
	if !reflect.DeepEqual(cfg.Partners, want) {
		t.Errorf("partners %+v, want %+v", cfg.Partners, want)
	}
	if !reflect.DeepEqual(cfg.Tenants, []string{"corretora-a", "corretora-b"}) {
		t.Errorf("brokers %v, want corretora-a and corretora-b", cfg.Tenants)
	}
}

func TestConfigReadsPartnersAndTenantsFromTheEnvironment(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"PORT":              "9090",
		"PARTNER_ENDPOINTS": " a=http://a:8080/ , b=http://b:8080 ",
		"TENANTS":           " corretora-x , corretora-y ",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := []Partner{{Name: "a", BaseURL: "http://a:8080"}, {Name: "b", BaseURL: "http://b:8080"}}
	if !reflect.DeepEqual(cfg.Partners, want) {
		t.Errorf("partners %+v, want %+v", cfg.Partners, want)
	}
	if !reflect.DeepEqual(cfg.Tenants, []string{"corretora-x", "corretora-y"}) {
		t.Errorf("unexpected brokers %v", cfg.Tenants)
	}
	if cfg.Port != "9090" {
		t.Errorf("port %q, want 9090", cfg.Port)
	}
}

func TestTelemetryIsOnByDefault(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := Telemetry{ServiceName: "quotation-api", Endpoint: "http://localhost:4317", Enabled: true}
	if cfg.Telemetry != want {
		t.Errorf("telemetry %+v, want %+v", cfg.Telemetry, want)
	}
}

func TestTelemetryReadsTheStandardOTelVariables(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"OTEL_SERVICE_NAME":           "quotation-api-poc",
		"OTEL_EXPORTER_OTLP_ENDPOINT": " http://otel-collector:4317 ",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	want := Telemetry{ServiceName: "quotation-api-poc", Endpoint: "http://otel-collector:4317", Enabled: true}
	if cfg.Telemetry != want {
		t.Errorf("telemetry %+v, want %+v", cfg.Telemetry, want)
	}
}

func TestTelemetryCanBeTurnedOff(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{"OTEL_SDK_DISABLED": "TRUE"}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	if cfg.Telemetry.Enabled {
		t.Error("OTEL_SDK_DISABLED=TRUE did not turn the SDK off")
	}
}

func TestTelemetryOffSkipsEndpointValidation(t *testing.T) {
	if _, err := LoadConfig(env(map[string]string{
		"OTEL_SDK_DISABLED":           "true",
		"OTEL_EXPORTER_OTLP_ENDPOINT": "not-a-url",
	})); err != nil {
		t.Fatalf("LoadConfig with the SDK off: %v", err)
	}
}

func TestInvalidConfigFails(t *testing.T) {
	cases := map[string]map[string]string{
		"partner without url":                {"PARTNER_ENDPOINTS": "partner-slow"},
		"relative url":                       {"PARTNER_ENDPOINTS": "partner-slow=/quotes"},
		"url without host":                   {"PARTNER_ENDPOINTS": "partner-slow=http://"},
		"partner without name":               {"PARTNER_ENDPOINTS": "=http://a:8080"},
		"repeated partner":                   {"PARTNER_ENDPOINTS": "a=http://a:8080,a=http://b:8080"},
		"empty partner list":                 {"PARTNER_ENDPOINTS": " , "},
		"empty tenant list":                  {"TENANTS": " , "},
		"collector without scheme":           {"OTEL_EXPORTER_OTLP_ENDPOINT": "otel-collector:4317"},
		"collector without host":             {"OTEL_EXPORTER_OTLP_ENDPOINT": "http://"},
		"OTEL_SDK_DISABLED is not a boolean": {"OTEL_SDK_DISABLED": "maybe"},
	}

	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(vars)); err == nil {
				t.Fatalf("invalid configuration (%v) was accepted", vars)
			}
		})
	}
}

func TestResilienceAndCacheDefaults(t *testing.T) {
	cfg, err := LoadConfig(env(nil))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	wantR := Resilience{
		PartnerTimeout:    2 * time.Second,
		FailuresToOpen:    5,
		OpenCooldown:      30 * time.Second,
		HalfOpenSuccesses: 2,
	}
	if cfg.Resilience != wantR {
		t.Errorf("resilience %+v, want %+v", cfg.Resilience, wantR)
	}

	wantC := Cache{
		Enabled:   true,
		RedisAddr: "localhost:6379",
		TTL:       300 * time.Second,
		StaleFor:  24 * time.Hour,
	}
	if cfg.Cache != wantC {
		t.Errorf("cache %+v, want %+v", cfg.Cache, wantC)
	}
}

func TestResilienceAndCacheReadFromEnvironment(t *testing.T) {
	cfg, err := LoadConfig(env(map[string]string{
		"PARTNER_TIMEOUT":             "1500ms",
		"BREAKER_FAILURES_TO_OPEN":    "3",
		"BREAKER_OPEN_COOLDOWN":       "10s",
		"BREAKER_HALF_OPEN_SUCCESSES": "1",
		"CACHE_ENABLED":               "false",
		"REDIS_ADDR":                  "redis:6379",
		"CACHE_TTL":                   "120s",
		"CACHE_STALE_FOR":             "1h",
	}))
	if err != nil {
		t.Fatalf("LoadConfig: %v", err)
	}

	wantR := Resilience{
		PartnerTimeout:    1500 * time.Millisecond,
		FailuresToOpen:    3,
		OpenCooldown:      10 * time.Second,
		HalfOpenSuccesses: 1,
	}
	if cfg.Resilience != wantR {
		t.Errorf("resilience %+v, want %+v", cfg.Resilience, wantR)
	}

	wantC := Cache{
		Enabled:   false,
		RedisAddr: "redis:6379",
		TTL:       120 * time.Second,
		StaleFor:  time.Hour,
	}
	if cfg.Cache != wantC {
		t.Errorf("cache %+v, want %+v", cfg.Cache, wantC)
	}
}

func TestInvalidResilienceAndCacheConfigFails(t *testing.T) {
	cases := map[string]map[string]string{
		"timeout not a duration":      {"PARTNER_TIMEOUT": "fast"},
		"timeout zero":                {"PARTNER_TIMEOUT": "0s"},
		"failures not an int":         {"BREAKER_FAILURES_TO_OPEN": "many"},
		"failures zero":               {"BREAKER_FAILURES_TO_OPEN": "0"},
		"cooldown negative":           {"BREAKER_OPEN_COOLDOWN": "-5s"},
		"half-open successes not int": {"BREAKER_HALF_OPEN_SUCCESSES": "two"},
		"cache enabled not a bool":    {"CACHE_ENABLED": "maybe"},
		"ttl not a duration":          {"CACHE_TTL": "soon"},
	}

	for name, vars := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := LoadConfig(env(vars)); err == nil {
				t.Fatalf("invalid configuration (%v) was accepted", vars)
			}
		})
	}
}
