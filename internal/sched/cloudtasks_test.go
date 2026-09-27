package sched

import (
	"testing"
	"time"
)

// The worked example in the Cloud Tasks RetryConfig reference: 10s, 300s and
// 3 doublings retry at 10, 20, 40, 80, 160, 240, 300 (#276).
func TestCloudTasksBackoffGrowsLinearlyAfterMaxDoublings(t *testing.T) {
	t.Parallel()
	b := CloudTasksBackoff{MaxAttempts: 10, MinBackoff: 10 * time.Second, MaxBackoff: 300 * time.Second, MaxDoublings: 3}.WithDefaults()
	want := []time.Duration{10, 20, 40, 80, 160, 240, 300, 300}
	for i, w := range want {
		if got := b.Delay(i + 1); got != w*time.Second {
			t.Errorf("Delay(%d) = %v, want %v", i+1, got, w*time.Second)
		}
	}
	if b.Delay(0) != 0 {
		t.Error("Delay(0) is not a retry and must be zero")
	}
	if b.Delay(1<<30) != 300*time.Second {
		t.Error("a huge attempt count did not cap at MaxBackoff")
	}
}

// Each field defaults on its own, and a negative MaxAttempts is unlimited.
func TestCloudTasksBackoffDefaultsEachField(t *testing.T) {
	t.Parallel()
	got := CloudTasksBackoff{MinBackoff: 2 * time.Second, MaxBackoff: 20 * time.Second}.WithDefaults()
	want := CloudTasksBackoff{MaxAttempts: 100, MinBackoff: 2 * time.Second, MaxBackoff: 20 * time.Second, MaxDoublings: 16}
	if got != want {
		t.Errorf("WithDefaults = %+v, want %+v", got, want)
	}
	if (CloudTasksBackoff{}).WithDefaults() != DefaultCloudTasksBackoff() {
		t.Error("an empty config did not take every default")
	}
	if b := (CloudTasksBackoff{MaxAttempts: -1}).WithDefaults(); !b.ShouldRetry(1 << 20) {
		t.Error("MaxAttempts -1 is unlimited in the API, and was treated as a limit")
	}
}

// MaxRetryDuration keeps a task past MaxAttempts until it, too, runs out.
func TestCloudTasksBackoffShouldRetryAfter(t *testing.T) {
	t.Parallel()
	b := CloudTasksBackoff{MaxAttempts: 2, MaxRetryDuration: time.Minute}.WithDefaults()
	if !b.ShouldRetryAfter(5, 30*time.Second) {
		t.Error("within max_retry_duration a task must be retried past max_attempts")
	}
	if b.ShouldRetryAfter(5, time.Minute) {
		t.Error("with both limits reached the task must stop")
	}
	if (CloudTasksBackoff{MaxAttempts: 2}).WithDefaults().ShouldRetryAfter(2, time.Hour) {
		t.Error("with no max_retry_duration, the attempt limit alone decides")
	}
}
