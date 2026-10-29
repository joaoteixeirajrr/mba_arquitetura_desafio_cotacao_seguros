package partner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

func testPartner(h http.Handler) (platform.Partner, func()) {
	server := httptest.NewServer(h)
	return platform.Partner{Name: "partner-flaky", BaseURL: server.URL}, server.Close
}

func TestQuoteReadsThePartnerResponse(t *testing.T) {
	var path, contentType string
	var received map[string]any

	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, contentType = r.URL.Path, r.Header.Get("Content-Type")
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &received)

		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"partner":"ignored","quote_id":"q-1","premium_cents":123456,` +
			`"currency":"BRL","coverage_cents":5000000,"valid_for_seconds":300}`))
	}))
	defer closeServer()

	quote, err := NewClient().Quote(context.Background(), p, map[string]string{"broker": "corretora-a"})
	if err != nil {
		t.Fatalf("Quote: %v", err)
	}

	if path != "/quotes" {
		t.Errorf("path %q, want /quotes", path)
	}
	if contentType != "application/json" {
		t.Errorf("Content-Type %q, want application/json", contentType)
	}
	if received["broker"] != "corretora-a" {
		t.Errorf("partner received broker %v, want corretora-a", received["broker"])
	}
	if quote.PremiumCents != 123456 || quote.QuoteID != "q-1" {
		t.Errorf("quote read incorrectly: %+v", quote)
	}
	if quote.Partner != "partner-flaky" {
		t.Errorf("partner %q, want partner-flaky", quote.Partner)
	}
}

func TestQuoteIdentifiesThePartnerThatFailed(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, `{"error":"partner unavailable"}`, http.StatusServiceUnavailable)
	}))
	defer closeServer()

	_, err := NewClient().Quote(context.Background(), p, map[string]string{})
	if err == nil {
		t.Fatal("a 503 from the partner was treated as success")
	}

	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("error %v is not a *partner.Error", err)
	}
	if failure.Partner != "partner-flaky" || failure.Status != http.StatusServiceUnavailable {
		t.Fatalf("error does not identify the failure: %+v", failure)
	}
}

func TestQuoteFailsOnUnreadableResponse(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("not json"))
	}))
	defer closeServer()

	if _, err := NewClient().Quote(context.Background(), p, map[string]string{}); err == nil {
		t.Fatal("unreadable response was accepted")
	}
}

func TestQuoteHonoursCancellation(t *testing.T) {
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	}))
	defer closeServer()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewClient().Quote(ctx, p, map[string]string{}); err == nil {
		t.Fatal("cancelled context did not interrupt the call")
	}
}

func TestQuoteTimesOut(t *testing.T) {
	release := make(chan struct{})
	p, closeServer := testPartner(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-release:
		case <-r.Context().Done():
		}
	}))
	defer closeServer()
	defer close(release)

	client := NewClient(WithTimeout(50 * time.Millisecond))

	_, err := client.Quote(context.Background(), p, map[string]string{})
	if err == nil {
		t.Fatal("a call slower than the timeout was treated as success")
	}

	var failure *Error
	if !errors.As(err, &failure) {
		t.Fatalf("timeout error %v is not a *partner.Error", err)
	}
	if failure.Partner != "partner-flaky" {
		t.Fatalf("timeout error does not identify the partner: %+v", failure)
	}
}
