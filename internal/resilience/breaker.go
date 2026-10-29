// Package resilience implements the circuit breaker that protects the partner
// boundary (internal/partner/client.go). The breaker is scoped per partner: a
// Group holds one independent Breaker per partner name, so one insurer sinking
// never short-circuits the other two.
//
// The three states are named explicitly (StateClosed, StateOpen, StateHalfOpen)
// and the operational meaning of "open" is enforced in code: Allow returns
// false while open, so the caller must NOT call the partner. That is what saves
// the R$ 0,04 per query and the wait time — not a counter with a nice name.
package resilience

import (
	"sync"
	"time"
)

// State is one of the three circuit-breaker states.
type State int

const (
	// StateClosed lets every call through and counts consecutive failures.
	StateClosed State = iota
	// StateOpen rejects every call (the partner is NOT contacted) until the
	// cooldown elapses.
	StateOpen
	// StateHalfOpen lets a single probe through to test whether the partner has
	// recovered.
	StateHalfOpen
)

func (s State) String() string {
	switch s {
	case StateClosed:
		return "closed"
	case StateOpen:
		return "open"
	case StateHalfOpen:
		return "half_open"
	default:
		return "unknown"
	}
}

// Settings configures a Breaker. The values come from platform.Resilience and
// are defended in docs/sad.md section 4.
type Settings struct {
	// FailuresToOpen is the number of consecutive failures that trips the
	// breaker from closed to open.
	FailuresToOpen int
	// OpenCooldown is how long the breaker stays open before allowing a probe.
	OpenCooldown time.Duration
	// HalfOpenSuccesses is how many consecutive probe successes close the breaker.
	HalfOpenSuccesses int
	// Now is the clock. Nil defaults to time.Now; tests inject a fake clock to
	// stay deterministic (no sleeps).
	Now func() time.Time
	// OnTransition, if set, is called on every state change with the partner
	// name, the previous state and the new state. Used to emit metrics/traces.
	OnTransition func(partner string, from, to State)
}

// Breaker is a single per-partner circuit breaker. It is safe for concurrent use.
type Breaker struct {
	partner  string
	settings Settings
	now      func() time.Time

	mu                   sync.Mutex
	state                State
	consecutiveFailures  int
	consecutiveSuccesses int
	openedAt             time.Time
	probeInFlight        bool
}

func newBreaker(partner string, s Settings) *Breaker {
	now := s.Now
	if now == nil {
		now = time.Now
	}
	return &Breaker{partner: partner, settings: s, now: now, state: StateClosed}
}

// State returns the breaker's current state, advancing an expired open circuit
// to half-open first.
func (b *Breaker) State() State {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()
	return b.state
}

// Allow reports whether a call may be made to the partner right now. A false
// return means the breaker is open (or a probe is already in flight) and the
// partner MUST NOT be contacted.
func (b *Breaker) Allow() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()

	switch b.state {
	case StateOpen:
		return false
	case StateHalfOpen:
		// Exactly one probe at a time while half-open.
		if b.probeInFlight {
			return false
		}
		b.probeInFlight = true
		return true
	default: // StateClosed
		return true
	}
}

// Success records a successful call and may close a half-open breaker.
func (b *Breaker) Success() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()

	switch b.state {
	case StateHalfOpen:
		b.probeInFlight = false
		b.consecutiveSuccesses++
		if b.consecutiveSuccesses >= b.settings.HalfOpenSuccesses {
			b.transition(StateClosed)
		}
	default:
		b.consecutiveFailures = 0
	}
}

// Failure records a failed call (error or timeout) and may open the breaker.
func (b *Breaker) Failure() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refresh()

	switch b.state {
	case StateHalfOpen:
		// A failed probe sends the breaker straight back to open.
		b.probeInFlight = false
		b.transition(StateOpen)
	case StateClosed:
		b.consecutiveFailures++
		if b.consecutiveFailures >= b.settings.FailuresToOpen {
			b.transition(StateOpen)
		}
	}
}

// refresh advances an open breaker to half-open once the cooldown has elapsed.
// The caller must hold b.mu.
func (b *Breaker) refresh() {
	if b.state == StateOpen && b.now().Sub(b.openedAt) >= b.settings.OpenCooldown {
		b.transition(StateHalfOpen)
	}
}

// transition changes the state and resets the relevant counters. The caller
// must hold b.mu.
func (b *Breaker) transition(to State) {
	from := b.state
	if from == to {
		return
	}
	b.state = to
	switch to {
	case StateOpen:
		b.openedAt = b.now()
		b.consecutiveFailures = 0
		b.consecutiveSuccesses = 0
		b.probeInFlight = false
	case StateHalfOpen:
		b.consecutiveSuccesses = 0
		b.probeInFlight = false
	case StateClosed:
		b.consecutiveFailures = 0
		b.consecutiveSuccesses = 0
		b.probeInFlight = false
	}
	if b.settings.OnTransition != nil {
		b.settings.OnTransition(b.partner, from, to)
	}
}

// Group holds one Breaker per partner name, created lazily. It is the
// per-partner scope: the scope decision defended in docs/sad.md section 4.
type Group struct {
	settings Settings
	mu       sync.Mutex
	breakers map[string]*Breaker
}

// NewGroup builds a Group that creates per-partner breakers from s.
func NewGroup(s Settings) *Group {
	return &Group{settings: s, breakers: make(map[string]*Breaker)}
}

// For returns the breaker for the given partner, creating it on first use.
func (g *Group) For(partner string) *Breaker {
	g.mu.Lock()
	defer g.mu.Unlock()
	if b, ok := g.breakers[partner]; ok {
		return b
	}
	b := newBreaker(partner, g.settings)
	g.breakers[partner] = b
	return b
}
