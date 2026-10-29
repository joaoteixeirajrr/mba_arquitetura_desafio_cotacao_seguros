package partner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/platform"
)

type Quote struct {
	Partner         string `json:"partner"`
	QuoteID         string `json:"quote_id"`
	PremiumCents    int64  `json:"premium_cents"`
	Currency        string `json:"currency"`
	CoverageCents   int64  `json:"coverage_cents"`
	ValidForSeconds int64  `json:"valid_for_seconds"`
}

type Client struct {
	http *http.Client
}

// Option configures a Client.
type Option func(*Client)

// WithTimeout sets a hard ceiling on each partner call. A call that exceeds it
// is cancelled and surfaces as a *partner.Error, which the circuit breaker
// counts as a failure. This is the fourth hole closed: the http.Client used to
// have Timeout zero (internal/partner/client.go).
func WithTimeout(d time.Duration) Option {
	return func(c *Client) { c.http.Timeout = d }
}

// NewClient builds a partner client. With no options it keeps the original
// behaviour (no timeout), which the starter tests rely on; main.go passes
// WithTimeout to enforce the ceiling defended in docs/sad.md.
func NewClient(opts ...Option) *Client {
	c := &Client{http: &http.Client{Transport: platform.InstrumentTransport(http.DefaultTransport)}}
	for _, opt := range opts {
		opt(c)
	}
	return c
}

const responseLimit = 1 << 20

func (c *Client) Quote(ctx context.Context, p platform.Partner, request any) (Quote, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("invalid request: %v", err)}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, p.BaseURL+"/quotes", bytes.NewReader(body))
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error()}
	}
	req.Header.Set("Content-Type", "application/json")

	response, err := c.http.Do(req)
	if err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: err.Error()}
	}
	defer func() { _ = response.Body.Close() }()

	if response.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, io.LimitReader(response.Body, responseLimit))
		return Quote{}, &Error{
			Partner: p.Name,
			Status:  response.StatusCode,
			Reason:  fmt.Sprintf("partner replied %d", response.StatusCode),
		}
	}

	var quote Quote
	if err := json.NewDecoder(io.LimitReader(response.Body, responseLimit)).Decode(&quote); err != nil {
		return Quote{}, &Error{Partner: p.Name, Reason: fmt.Sprintf("unreadable response: %v", err)}
	}

	quote.Partner = p.Name
	return quote, nil
}

type Error struct {
	Partner string
	Status  int
	Reason  string
}

func (e *Error) Error() string {
	return fmt.Sprintf("partner %s: %s", e.Partner, e.Reason)
}
