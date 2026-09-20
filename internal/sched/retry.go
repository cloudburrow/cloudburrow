package sched

import (
	"math"
	"time"
)

// Backoff computes retry delays.
//
// Retry policy is per service, not shared: Pub/Sub redelivery after an
// ack-deadline expiry and Cloud Tasks retry with backoff are different
// mechanisms with different configuration surfaces, and forcing them through
// one policy would misrepresent both (docs/architecture.md §8).
type Backoff struct {
	// Min is the first delay.
	Min time.Duration
	// Max caps any single delay.
	Max time.Duration
	// Multiplier scales each successive delay.
	Multiplier float64
	// MaxAttempts bounds total attempts. Zero means unlimited.
	MaxAttempts int
}

// DefaultBackoff is a conservative starting policy for owned work.
func DefaultBackoff() Backoff {
	return Backoff{Min: 100 * time.Millisecond, Max: 10 * time.Second, Multiplier: 2, MaxAttempts: 5}
}

// Delay returns the wait before the given attempt. Attempt 1 is the first
// retry, so Delay(1) is Min.
func (b Backoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	min := b.Min
	if min <= 0 {
		min = 100 * time.Millisecond
	}
	mult := b.Multiplier
	if mult < 1 {
		mult = 1
	}
	d := float64(min) * math.Pow(mult, float64(attempt-1))

	// Guard the conversion: a large attempt count overflows int64 and would
	// otherwise produce a negative duration that never fires.
	if b.Max > 0 && (d > float64(b.Max) || math.IsInf(d, 0)) {
		return b.Max
	}
	if d > float64(math.MaxInt64) {
		return b.Max
	}
	return time.Duration(d)
}

// ShouldRetry reports whether another attempt is permitted.
func (b Backoff) ShouldRetry(attempt int) bool {
	if b.MaxAttempts <= 0 {
		return true
	}
	return attempt < b.MaxAttempts
}
