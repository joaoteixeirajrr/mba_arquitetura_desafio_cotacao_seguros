package resilience

import (
	"testing"
	"time"
)

// fakeClock is a deterministic clock: no sleeps, no wall-clock dependence.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func testSettings(clock *fakeClock) Settings {
	return Settings{
		FailuresToOpen:    5,
		OpenCooldown:      30 * time.Second,
		HalfOpenSuccesses: 2,
		Now:               clock.now,
	}
}

func TestBreakerOpensAfterConsecutiveFailures(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	b := newBreaker("partner-flaky", testSettings(clock))

	// 4 failures must not open the breaker (threshold is 5).
	for i := 0; i < 4; i++ {
		if !b.Allow() {
			t.Fatalf("call %d was rejected before the threshold", i+1)
		}
		b.Failure()
	}
	if got := b.State(); got != StateClosed {
		t.Fatalf("state %s after 4 failures, want closed", got)
	}

	// The 5th consecutive failure opens it.
	if !b.Allow() {
		t.Fatal("5th call rejected before the breaker opened")
	}
	b.Failure()
	if got := b.State(); got != StateOpen {
		t.Fatalf("state %s after 5 failures, want open", got)
	}
}

func TestBreakerOpenDoesNotCallThePartner(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	b := newBreaker("partner-flaky", testSettings(clock))

	for i := 0; i < 5; i++ {
		b.Allow()
		b.Failure()
	}
	if b.State() != StateOpen {
		t.Fatal("breaker did not open")
	}

	// The operational meaning of open: Allow is false, so the partner is NOT called.
	if b.Allow() {
		t.Fatal("open breaker allowed a call — the partner would be contacted, defeating the breaker")
	}
}

func TestBreakerRecoversThroughHalfOpen(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	b := newBreaker("partner-flaky", testSettings(clock))

	for i := 0; i < 5; i++ {
		b.Allow()
		b.Failure()
	}
	if b.State() != StateOpen {
		t.Fatal("breaker did not open")
	}

	// Before the cooldown elapses it stays open.
	clock.advance(29 * time.Second)
	if b.State() != StateOpen {
		t.Fatal("breaker left open state before the cooldown")
	}

	// After the cooldown it moves to half-open and lets exactly one probe.
	clock.advance(2 * time.Second)
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state %s after cooldown, want half_open", got)
	}
	if !b.Allow() {
		t.Fatal("half-open breaker rejected the first probe")
	}
	if b.Allow() {
		t.Fatal("half-open breaker allowed a second concurrent probe")
	}

	// First probe succeeds; still half-open (needs 2).
	b.Success()
	if got := b.State(); got != StateHalfOpen {
		t.Fatalf("state %s after 1 success, want half_open (needs 2)", got)
	}

	// Second probe succeeds; breaker closes.
	if !b.Allow() {
		t.Fatal("half-open breaker rejected the second probe")
	}
	b.Success()
	if got := b.State(); got != StateClosed {
		t.Fatalf("state %s after 2 successes, want closed", got)
	}
}

func TestBreakerReopensOnFailedProbe(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	b := newBreaker("partner-flaky", testSettings(clock))

	for i := 0; i < 5; i++ {
		b.Allow()
		b.Failure()
	}
	clock.advance(30 * time.Second)
	if b.State() != StateHalfOpen {
		t.Fatal("breaker did not move to half-open after cooldown")
	}

	b.Allow()
	b.Failure() // probe fails
	if got := b.State(); got != StateOpen {
		t.Fatalf("state %s after a failed probe, want open", got)
	}
}

// TestBreakerOpensOnTheDeterministicBurst replays the documented failure
// sequence of partner-flaky — a burst of 9 consecutive failures at sequences
// 49..57 (docs/smoke-test-factibilidade.md, locked by
// cmd/partner-mock/feasibility_test.go) — and asserts the breaker opens inside
// it, without any sleep or randomness.
func TestBreakerOpensOnTheDeterministicBurst(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}

	var transitions []string
	s := testSettings(clock)
	s.OnTransition = func(partner string, from, to State) {
		transitions = append(transitions, from.String()+"->"+to.String())
	}
	b := newBreaker("partner-flaky", s)

	// A failure sequence with a clean run and then the 9-long burst at 49..57.
	failed := make([]bool, 60)
	for i := 48; i <= 56; i++ { // 0-based indexes for sequences 49..57
		failed[i] = true
	}

	openedAt := 0
	for seq := 1; seq <= 60; seq++ {
		if !b.Allow() {
			// Short-circuited: partner not called. Let the cooldown pass so the
			// replay keeps exercising the burst rather than stalling.
			clock.advance(s.OpenCooldown)
			continue
		}
		if failed[seq-1] {
			b.Failure()
		} else {
			b.Success()
		}
		if openedAt == 0 && b.State() == StateOpen {
			openedAt = seq
		}
	}

	if openedAt == 0 {
		t.Fatal("the breaker never opened across the documented burst")
	}
	// Threshold 5 within a 9-long burst starting at seq 49: opens by seq 53.
	if openedAt > 53 {
		t.Fatalf("breaker opened on seq %d, expected by seq 53 (burst 49..57, threshold 5)", openedAt)
	}
	if len(transitions) == 0 {
		t.Fatal("no state transitions were observed")
	}
	t.Logf("opened on seq %d; transitions: %v", openedAt, transitions)
}

func TestGroupIsolatesPartners(t *testing.T) {
	clock := &fakeClock{t: time.Unix(0, 0)}
	g := NewGroup(testSettings(clock))

	flaky := g.For("partner-flaky")
	slow := g.For("partner-slow")

	for i := 0; i < 5; i++ {
		flaky.Allow()
		flaky.Failure()
	}

	if flaky.State() != StateOpen {
		t.Fatal("partner-flaky breaker did not open")
	}
	if slow.State() != StateClosed {
		t.Fatal("partner-slow breaker opened even though only partner-flaky failed — scope leaked")
	}
	if g.For("partner-flaky") != flaky {
		t.Fatal("Group.For returned a different breaker for the same partner")
	}
}
