package ratelimit

import (
	"sort"
	"sync"
	"time"
)

// fakeClock is the injected clock these tests run on (L4). Nothing here sleeps for a rate
// limit: a test that waits out a real minute is a test nobody runs on save, and one that
// waits out a shortened minute proves something about the shortening.
type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{now: time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) After(d time.Duration) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	t := &fakeTimer{deadline: c.now.Add(d), ch: make(chan time.Time, 1)}
	if !t.deadline.After(c.now) {
		t.ch <- c.now
		return t.ch
	}
	c.timers = append(c.timers, t)
	return t.ch
}

// Advance moves the clock and fires every timer the move passed, in deadline order, so a
// test sees the same sequence a real clock would produce.
func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	now := c.now

	sort.Slice(c.timers, func(i, j int) bool {
		return c.timers[i].deadline.Before(c.timers[j].deadline)
	})
	var pending []*fakeTimer
	var fired []*fakeTimer
	for _, t := range c.timers {
		if t.deadline.After(now) {
			pending = append(pending, t)
			continue
		}
		fired = append(fired, t)
	}
	c.timers = pending
	c.mu.Unlock()

	for _, t := range fired {
		t.ch <- now
	}
}
