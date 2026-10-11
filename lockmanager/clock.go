package lockmanager

import (
	"context"
	"sync"
	"time"
)

// clock.go holds the injectable time source of one lock manager.
//
// Lock waiting must never depend on wall-clock ordering: an acceptance test has to
// be able to advance time to a deadline and observe one definite outcome, and it has
// to do that without sleeping. Every deadline in the manager therefore comes from a
// Clock, and production uses the monotonic system clock.
//
// A test hook is production-disabled by construction: it is installed only when a
// caller passes one explicitly, so the default manager carries no channel, no
// barrier and no coordination state at all.

// Clock is the time source of one lock manager.
//
// Implementations must be safe for concurrent use, because a waiter, a detector and a
// diagnostics reader may all consult the same clock.
type Clock interface {
	// Now reports the current time. Implementations should be monotonic, so an
	// elapsed duration can never go backwards.
	Now() time.Time
	// After returns a channel that is closed or signalled once the deadline is
	// reached. A caller must always have a second way to stop waiting, because
	// cancellation and a grant can both arrive before the deadline.
	After(deadline time.Time) <-chan time.Time
}

// SystemClock is the production clock: the standard monotonic time source.
type SystemClock struct{}

// Now reports the current time.
func (SystemClock) Now() time.Time { return time.Now() }

// After reports when the deadline is reached.
func (SystemClock) After(deadline time.Time) <-chan time.Time {
	d := time.Until(deadline)
	if d <= 0 {
		// An already-expired deadline must fire immediately rather than schedule a
		// negative timer.
		ch := make(chan time.Time, 1)
		ch <- time.Now()
		close(ch)
		return ch
	}
	return time.After(d)
}

// FakeClock is a deterministic clock for tests.
//
// It never sleeps and never reads the wall clock: Advance moves it and fires exactly
// the timers whose deadline it passed, in deadline order, so a test can establish
// "the deadline fired before the grant" or the opposite as a fact rather than a race.
type FakeClock struct {
	mu      sync.Mutex
	now     time.Time
	timers  []*fakeTimer
	stopped bool
	seq     uint64
}

type fakeTimer struct {
	deadline time.Time
	ch       chan time.Time
	seq      uint64
	fired    bool
}

// NewFakeClock returns a fake clock starting at the given instant.
func NewFakeClock(start time.Time) *FakeClock { return &FakeClock{now: start} }

// Now reports the fake current time.
func (c *FakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

// After registers one deadline timer.
func (c *FakeClock) After(deadline time.Time) <-chan time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	ch := make(chan time.Time, 1)
	if c.stopped {
		return ch
	}
	c.seq++
	timer := &fakeTimer{deadline: deadline, ch: ch, seq: c.seq}
	if !deadline.After(c.now) {
		timer.fired = true
		ch <- c.now
		return ch
	}
	c.timers = append(c.timers, timer)
	return ch
}

// Advance moves the fake clock forward and fires every timer it passed, in deadline
// order. It returns how many timers fired.
func (c *FakeClock) Advance(d time.Duration) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
	fired := 0
	for {
		index := -1
		for i, timer := range c.timers {
			if timer.fired {
				continue
			}
			if timer.deadline.After(c.now) {
				continue
			}
			if index == -1 || timer.deadline.Before(c.timers[index].deadline) ||
				(timer.deadline.Equal(c.timers[index].deadline) && timer.seq < c.timers[index].seq) {
				index = i
			}
		}
		if index == -1 {
			break
		}
		timer := c.timers[index]
		timer.fired = true
		timer.ch <- timer.deadline
		fired++
	}
	// Drop fired timers so a long test cannot grow the slice without bound.
	live := c.timers[:0]
	for _, timer := range c.timers {
		if !timer.fired {
			live = append(live, timer)
		}
	}
	c.timers = live
	return fired
}

// PendingTimers reports how many timers are still waiting.
func (c *FakeClock) PendingTimers() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.timers)
}

// Stop disables the clock: every later After returns a channel that never fires.
func (c *FakeClock) Stop() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.stopped = true
	c.timers = nil
}

// Barrier is a deterministic hand-off between two goroutines in a test.
//
// It is the replacement for sleep-based ordering: a participant calls Arrive and the
// test calls Wait, so the test observes the event as a fact instead of guessing that
// it has happened. The zero value is not usable; use NewBarrier.
type Barrier struct {
	once sync.Once
	ch   chan struct{}
}

// NewBarrier returns an unreached barrier.
func NewBarrier() *Barrier { return &Barrier{ch: make(chan struct{})} }

// Arrive marks the barrier reached. Repeated arrivals are safe.
func (b *Barrier) Arrive() { b.once.Do(func() { close(b.ch) }) }

// Wait blocks until the barrier is reached or the context ends. It reports whether
// the barrier was reached.
func (b *Barrier) Wait(ctx context.Context) bool {
	select {
	case <-b.ch:
		return true
	case <-ctx.Done():
		return false
	}
}

// Reached reports whether the barrier was reached without blocking.
func (b *Barrier) Reached() bool {
	select {
	case <-b.ch:
		return true
	default:
		return false
	}
}
