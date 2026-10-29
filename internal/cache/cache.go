// Package cache implements the tenant-scoped quote cache that sits in front of
// the partner boundary. The cache key ALWAYS contains the tenant (the broker):
// returning broker A a quote negotiated by broker B is a pricing error and a
// personal-data incident under the LGPD (README.md). That isolation is the
// whole point of this package.
//
// Key layout (written out literally, and mirrored in docs/sad.md section 4):
//
//	quote:v1:{tenant}:{coverage}:{sha256(normalized risk)}
//
// where the normalized risk is the driver document + birth year + plate + model
// + year + value, lower-cased and trimmed, joined with '|'. The tenant and the
// coverage are kept as readable prefixes so a human can scan keys per broker;
// the risk itself is hashed so that CPF and plate never appear in a key.
package cache

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"github.com/GuilhermeOliveira591/mba_arquitetura_desafio_cotacao_seguros/internal/partner"
)

// keyVersion lets the key layout evolve without serving stale shapes.
const keyVersion = "v1"

// Risk is the normalized identity of what is being quoted. It deliberately
// mirrors the fields the partner request carries, so the same risk maps to the
// same key regardless of casing or spacing.
type Risk struct {
	Document   string
	BirthYear  int
	Plate      string
	Model      string
	Year       int
	ValueCents int64
	Coverage   string
}

// Key builds the tenant-scoped cache key. The tenant is a mandatory, readable
// prefix; the risk is hashed so no CPF or plate ever lands in a key.
func Key(tenant string, r Risk) string {
	tenant = strings.ToLower(strings.TrimSpace(tenant))
	coverage := strings.ToLower(strings.TrimSpace(r.Coverage))

	risk := strings.Join([]string{
		strings.ToLower(strings.TrimSpace(r.Document)),
		fmt.Sprintf("%d", r.BirthYear),
		strings.ToLower(strings.TrimSpace(r.Plate)),
		strings.ToLower(strings.TrimSpace(r.Model)),
		fmt.Sprintf("%d", r.Year),
		fmt.Sprintf("%d", r.ValueCents),
		coverage,
	}, "|")

	sum := sha256.Sum256([]byte(risk))
	return fmt.Sprintf("quote:%s:%s:%s:%s", keyVersion, tenant, coverage, hex.EncodeToString(sum[:]))
}

// Entry is what the cache stores: the aggregated partner quotes plus the moment
// they were stored, so a fallback response can report the quote's age.
type Entry struct {
	Quotes   []partner.Quote `json:"quotes"`
	StoredAt time.Time       `json:"stored_at"`
}

// Age returns how old the cached entry is relative to now.
func (e Entry) Age(now time.Time) time.Duration {
	return now.Sub(e.StoredAt)
}
