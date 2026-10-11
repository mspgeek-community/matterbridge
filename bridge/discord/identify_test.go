package bdiscord

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
)

func newTestLimiter(clock *time.Time) *identifyLimiter {
	l := newIdentifyLimiter()
	l.now = func() time.Time { return *clock }
	return l
}

func TestIdentifyLimiterBackoff(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(&clock)

	assert.Equal(t, time.Duration(0), l.reserve(), "first identify is immediate")

	// Sessions that die immediately back off exponentially, capped.
	expected := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 40 * time.Second}
	for _, want := range expected {
		wait := l.reserve()
		assert.Equal(t, want, wait)
		clock = clock.Add(wait)
	}
	for i := 0; i < 10; i++ {
		wait := l.reserve()
		assert.LessOrEqual(t, wait, identifyMaxBackoff)
		clock = clock.Add(wait)
	}
	assert.Equal(t, identifyMaxBackoff, l.backoff)

	// A stable session resets the backoff.
	clock = clock.Add(identifyStableAfter)
	assert.Equal(t, time.Duration(0), l.reserve())
	assert.Equal(t, time.Duration(0), l.backoff)
}

func TestIdentifyLimiterDailyCap(t *testing.T) {
	clock := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newTestLimiter(&clock)
	start := clock

	for i := 0; i < identifyMaxPerWindow; i++ {
		assert.Equal(t, time.Duration(0), l.reserve())
		clock = clock.Add(identifyStableAfter)
	}

	// The cap forces a wait until the oldest identify leaves the window.
	wait := l.reserve()
	assert.Equal(t, start.Add(identifyWindow).Sub(clock), wait)
	assert.Greater(t, wait, time.Duration(0))
}
