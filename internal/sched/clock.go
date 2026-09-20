// Package sched provides cancellable background work, due-time scheduling and
// bounded retry and backoff over an injected clock, so that tests advance
// virtual time instead of sleeping.
//
// The clock is injected for code CloudBurrow owns. External components cannot
// have their clocks advanced, so waiting on those is bounded polling instead
// (docs/architecture.md §9).
package sched

import (
	"sort"
	"sync"
	"time"
)

// Clock abstracts time so scheduling can be tested deterministically.
type Clock interface {
	Now() time.Time
	// After returns a channel that receives once d has elapsed.
	After(d time.Duration) <-chan time.Time
}

// RealClock is the production clock.
type RealClock struct{}

func (RealClock) Now() time.Time                         { return time.Now() }
func (RealClock) After(d time.Duration) <-chan time.Time { return time.After(d) }

// FakeClock is a controllable clock for tests.
//
// Advancing it fires every waiter whose deadline has passed, so a test can
// verify retry and backoff behaviour in microseconds rather than waiting for
// real delays. A test that sleeps to observe a retry is a defect.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	waiters []*waiter
}

type waiter struct {
	deadline time.Time
	ch       chan time.Time
}

// NewFakeClock returns a clock started at the given instant.
func NewFakeClock(start time.Time) *FakeClock {
	return &FakeClock{now: start}
}

func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *FakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if d <= 0 {
		ch <- c.now
		return ch
	}
	c.waiters = append(c.waiters, &waiter{deadline: c.now.Add(d), ch: ch})
	return ch
}

// Advance moves time forward and fires every waiter whose deadline has passed.
func (c *FakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now

	var due []*waiter
	var remaining []*waiter
	for _, w := range c.waiters {
		if !w.deadline.After(now) {
			due = append(due, w)
		} else {
			remaining = append(remaining, w)
		}
	}
	c.waiters = remaining
	c.mu.Unlock()

	// Fire in deadline order so a test observes the same sequence real time
	// would produce.
	sort.Slice(due, func(i, j int) bool { return due[i].deadline.Before(due[j].deadline) })
	for _, w := range due {
		w.ch <- now
	}
}

// Waiters reports how many timers are outstanding, so a test can wait for a
// worker to actually arm its timer before advancing.
func (c *FakeClock) Waiters() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.waiters)
}
