package sched

import (
	"math"
	"time"
)

// CloudTasksBackoff is the retry schedule Cloud Tasks documents for
// google.cloud.tasks.v2.RetryConfig: start at MinBackoff, double MaxDoublings
// times, then grow by the last doubled interval each retry, never exceeding
// MaxBackoff.
//
// It lives here rather than in internal/service/tasks because Cloud Scheduler
// documents the same min/max backoff and doublings semantics, and one service
// package must not import another (docs/architecture.md §3, #599). It is a
// schedule, not a policy: each service still decides its own configuration and
// defaults, and Pub/Sub redelivery does not use it (§8).
//
// The fields match internal/service/tasks.RetryConfig one for one, so that type
// converts to this one directly.
type CloudTasksBackoff struct {
	// MaxAttempts counts the first attempt. Negative is unlimited, as in the
	// API.
	MaxAttempts  int
	MinBackoff   time.Duration
	MaxBackoff   time.Duration
	MaxDoublings int
	// MaxRetryDuration bounds retrying in time, from the first attempt; zero
	// is unlimited (#578).
	MaxRetryDuration time.Duration
}

// DefaultCloudTasksBackoff is the Cloud Tasks service default: 100 attempts,
// 0.1s to 1h, 16 doublings.
func DefaultCloudTasksBackoff() CloudTasksBackoff {
	return CloudTasksBackoff{
		MaxAttempts:  100,
		MinBackoff:   100 * time.Millisecond,
		MaxBackoff:   time.Hour,
		MaxDoublings: 16,
	}
}

// WithDefaults fills each unset field with the Cloud Tasks service default.
//
// Cloud Tasks defaults each field on its own. Defaulting the whole config
// only when MaxAttempts was unset threw away a queue's MinBackoff whenever
// its MaxAttempts was left out, and left MaxDoublings at 0 — no doubling at
// all — whenever it was, which an unset field never means (#276). Proto3
// cannot tell unset from zero, and the service reads zero as unset, so this
// does too. A negative MaxAttempts is the API's "unlimited" and is kept.
func (b CloudTasksBackoff) WithDefaults() CloudTasksBackoff {
	d := DefaultCloudTasksBackoff()
	if b.MaxAttempts == 0 {
		b.MaxAttempts = d.MaxAttempts
	}
	if b.MinBackoff <= 0 {
		b.MinBackoff = d.MinBackoff
	}
	if b.MaxBackoff <= 0 {
		b.MaxBackoff = d.MaxBackoff
	}
	if b.MaxDoublings <= 0 {
		b.MaxDoublings = d.MaxDoublings
	}
	return b
}

// Delay returns the wait before the given retry. Attempt 1 is the first retry.
//
// MaxDoublings caps exponential growth: beyond it, Cloud Tasks increases the
// delay linearly rather than continuing to double (#276). With 10s, 300s and
// 3 doublings that is 10s, 20s, 40s, 80s, 160s, 240s, 300s, 300s...
//
// Delay reads the fields as they are; call WithDefaults first for a config
// that may leave some unset.
func (b CloudTasksBackoff) Delay(attempt int) time.Duration {
	if attempt < 1 {
		return 0
	}
	minB, maxB := float64(b.MinBackoff), float64(b.MaxBackoff)
	k, doublings := attempt-1, b.MaxDoublings
	var d float64
	if k <= doublings {
		d = minB * math.Pow(2, float64(k))
	} else {
		step := minB * math.Pow(2, float64(doublings))
		d = step * float64(k-doublings+1)
	}
	// Compared as floats: a large attempt count would overflow a Duration and
	// come out negative, which would fire at once.
	if d > maxB || math.IsInf(d, 0) || math.IsNaN(d) {
		return b.MaxBackoff
	}
	return time.Duration(d)
}

// ShouldRetry reports whether another attempt is permitted after the given
// number of attempts. A negative MaxAttempts is unlimited, as in the API.
func (b CloudTasksBackoff) ShouldRetry(attempts int) bool {
	if b.MaxAttempts < 0 {
		return true
	}
	return attempts < b.MaxAttempts
}

// ShouldRetryAfter is ShouldRetry with MaxRetryDuration: once it is set, a
// task is retried until both limits are reached, measured from its first
// attempt, as google.cloud.tasks.v2.RetryConfig documents (#578).
func (b CloudTasksBackoff) ShouldRetryAfter(attempts int, sinceFirst time.Duration) bool {
	if b.ShouldRetry(attempts) {
		return true
	}
	return b.MaxRetryDuration > 0 && sinceFirst < b.MaxRetryDuration
}
