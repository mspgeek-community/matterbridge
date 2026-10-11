package bdiscord

import (
	"sync"
	"time"
)

// Discord resets a bot's token after 1000 gateway IDENTIFY calls in 24 hours.
// identifyLimiter keeps this bridge far below that limit by backing off
// exponentially when sessions keep dying shortly after connecting, and by
// enforcing a hard per-day cap.
const (
	identifyWindow       = 24 * time.Hour
	identifyMaxPerWindow = 100
	identifyMinBackoff   = 5 * time.Second
	identifyMaxBackoff   = 10 * time.Minute
	// A session that lived at least this long is considered healthy and
	// resets the backoff.
	identifyStableAfter = 10 * time.Minute
)

type identifyLimiter struct {
	sync.Mutex
	history []time.Time
	backoff time.Duration
	now     func() time.Time
}

func newIdentifyLimiter() *identifyLimiter {
	return &identifyLimiter{now: time.Now}
}

// reserve records an identify attempt and returns how long the caller must
// wait before performing it.
func (l *identifyLimiter) reserve() time.Duration {
	l.Lock()
	defer l.Unlock()

	now := l.now()

	cutoff := now.Add(-identifyWindow)
	i := 0
	for i < len(l.history) && !l.history[i].After(cutoff) {
		i++
	}
	l.history = l.history[i:]

	var wait time.Duration
	if n := len(l.history); n > 0 {
		since := now.Sub(l.history[n-1])
		if since < identifyStableAfter {
			l.backoff *= 2
			if l.backoff < identifyMinBackoff {
				l.backoff = identifyMinBackoff
			}
			if l.backoff > identifyMaxBackoff {
				l.backoff = identifyMaxBackoff
			}
			if since < l.backoff {
				wait = l.backoff - since
			}
		} else {
			l.backoff = 0
		}
	}

	if len(l.history) >= identifyMaxPerWindow {
		if capWait := l.history[0].Add(identifyWindow).Sub(now); capWait > wait {
			wait = capWait
		}
	}

	l.history = append(l.history, now.Add(wait))
	return wait
}
