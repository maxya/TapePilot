package auth

import (
	"net"
	"sync"
	"time"
)

type bucket struct {
	tokens  float64
	updated time.Time
}
type limiter struct {
	mu      sync.Mutex
	entries map[string]bucket
	global  bucket
	burst   float64
	period  time.Duration
}

func (l *limiter) allow(remote string, now time.Time) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.entries == nil {
		l.entries = make(map[string]bucket)
	}
	ip, _, err := net.SplitHostPort(remote)
	if err != nil {
		ip = remote
	}
	if len(ip) > 128 {
		return false
	}
	refill := func(b bucket, burst float64, period time.Duration) bucket {
		if b.updated.IsZero() {
			return bucket{burst, now}
		}
		b.tokens += now.Sub(b.updated).Seconds() / period.Seconds()
		if b.tokens > burst {
			b.tokens = burst
		}
		b.updated = now
		return b
	}
	// Bound aggregate expensive work even when remote addresses vary.
	l.global = refill(l.global, 20, l.period/2)
	if l.global.tokens < 1 {
		return false
	}
	b, known := l.entries[ip]
	if !known && len(l.entries) >= 256 {
		for key, entry := range l.entries {
			if now.Sub(entry.updated) > 10*time.Minute {
				delete(l.entries, key)
			}
		}
		if len(l.entries) >= 256 {
			return false
		}
	}
	b = refill(b, l.burst, l.period)
	if b.tokens < 1 {
		l.entries[ip] = b
		return false
	}
	b.tokens--
	l.global.tokens--
	l.entries[ip] = b
	return true
}
