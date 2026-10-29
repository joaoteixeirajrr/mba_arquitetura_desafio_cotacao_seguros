package quotation

import (
	"fmt"
	"strings"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/cache"
	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

type Request struct {
	Driver   Driver  `json:"driver"`
	Vehicle  Vehicle `json:"vehicle"`
	Coverage string  `json:"coverage"`
}

type Driver struct {
	Document  string `json:"document"`
	BirthYear int    `json:"birth_year"`
}

type Vehicle struct {
	Plate      string `json:"plate"`
	Model      string `json:"model"`
	Year       int    `json:"year"`
	ValueCents int64  `json:"value_cents"`
}

var coverages = map[string]bool{"comprehensive": true, "third_party": true}

const defaultCoverage = "comprehensive"

func (r *Request) Normalize() error {
	r.Coverage = strings.TrimSpace(r.Coverage)
	if r.Coverage == "" {
		r.Coverage = defaultCoverage
	}

	r.Driver.Document = strings.TrimSpace(r.Driver.Document)
	r.Vehicle.Plate = strings.ToUpper(strings.TrimSpace(r.Vehicle.Plate))
	r.Vehicle.Model = strings.TrimSpace(r.Vehicle.Model)

	switch {
	case r.Driver.Document == "":
		return fmt.Errorf("driver.document is required")
	case r.Driver.BirthYear < 1900:
		return fmt.Errorf("driver.birth_year is required and must be a valid year")
	case r.Vehicle.Plate == "":
		return fmt.Errorf("vehicle.plate is required")
	case r.Vehicle.Year < 1900:
		return fmt.Errorf("vehicle.year is required and must be a valid year")
	case r.Vehicle.ValueCents <= 0:
		return fmt.Errorf("vehicle.value_cents must be greater than zero")
	case !coverages[r.Coverage]:
		return fmt.Errorf("coverage must be comprehensive or third_party")
	}
	return nil
}

type partnerRequest struct {
	Broker string `json:"broker"`
	Request
}

// Risk projects the request onto the cache's normalized risk identity. It is
// called after Normalize, so casing and spacing are already settled.
func (r Request) Risk() cache.Risk {
	return cache.Risk{
		Document:   r.Driver.Document,
		BirthYear:  r.Driver.BirthYear,
		Plate:      r.Vehicle.Plate,
		Model:      r.Vehicle.Model,
		Year:       r.Vehicle.Year,
		ValueCents: r.Vehicle.ValueCents,
		Coverage:   r.Coverage,
	}
}

type Response struct {
	TenantID  string          `json:"tenant_id"`
	Quotes    []partner.Quote `json:"quotes"`
	ElapsedMs int64           `json:"elapsed_ms"`
	// Degraded tells the broker that this is not a complete, live answer. It is
	// the contract change the fallback introduces: when true, the broker must
	// treat the quotes as partial and/or aged. Absent (false) on the normal path.
	Degraded *Degraded `json:"degraded,omitempty"`
}

// Degraded carries everything the broker needs to know that a response is not a
// complete live quote, so it is never mistaken for a normal one. A premium is
// always from a partner or from traceable cache — never invented.
type Degraded struct {
	// Source is "partial" (some partners answered live, others were skipped or
	// failed) or "cache" (served entirely from a previous quote).
	Source string `json:"source"`
	// Missing lists the partners that did not contribute a live quote.
	Missing []string `json:"missing,omitempty"`
	// AgeSeconds is how old the cached quote is, in seconds. Zero for a purely
	// partial (live) degraded response.
	AgeSeconds int64 `json:"age_seconds,omitempty"`
}
