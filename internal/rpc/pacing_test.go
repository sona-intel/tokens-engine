package rpc

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeTimer struct {
	at time.Time
	ch chan time.Time
}

type fakeClock struct {
	mu     sync.Mutex
	now    time.Time
	timers []*fakeTimer
}

func newFakeClock() *fakeClock { return &fakeClock{now: time.Unix(1_000_000, 0)} }

func (f *fakeClock) Now() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

func (f *fakeClock) After(d time.Duration) (<-chan time.Time, func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	t := &fakeTimer{at: f.now.Add(d), ch: make(chan time.Time, 1)}
	f.timers = append(f.timers, t)
	return t.ch, func() {
		f.mu.Lock()
		defer f.mu.Unlock()
		for i, x := range f.timers {
			if x == t {
				f.timers = append(f.timers[:i], f.timers[i+1:]...)
				return
			}
		}
	}
}

func (f *fakeClock) Advance(d time.Duration) {
	f.mu.Lock()
	f.now = f.now.Add(d)
	var due []*fakeTimer
	rest := f.timers[:0]
	for _, t := range f.timers {
		if !t.at.After(f.now) {
			due = append(due, t)
		} else {
			rest = append(rest, t)
		}
	}
	f.timers = rest
	now := f.now
	f.mu.Unlock()
	for _, t := range due {
		t.ch <- now
	}
}

// waiting is the number of timers that goroutines sleep on
func (f *fakeClock) waiting() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.timers)
}

func (f *fakeClock) waitFor(t *testing.T, n int) {
	t.Helper()
	for range 2000 {
		if f.waiting() >= n {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("%d timers armed, want %d", f.waiting(), n)
}

func TestVisitorNeverWaitsBehindBackground(t *testing.T) {
	clk := newFakeClock()
	p := newPacer(2, clk)
	// one background call in flight and two more queued behind it
	release, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		go func() {
			if rel, err := p.Acquire(t.Context()); err == nil {
				rel()
			}
		}()
	}
	visitor := WithVisitor(t.Context())
	done := make(chan error, VisitorBurst)
	for range VisitorBurst {
		go func() { _, err := p.Acquire(visitor); done <- err }()
	}
	for range VisitorBurst {
		select {
		case err := <-done:
			if err != nil {
				t.Fatal(err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("a visitor waited behind background calls")
		}
	}
	release()
}

func TestVisitorBurstIsFivePerSecond(t *testing.T) {
	clk := newFakeClock()
	p := newPacer(2, clk)
	ctx := WithVisitor(t.Context())
	for range VisitorBurst {
		if _, err := p.Acquire(ctx); err != nil {
			t.Fatal(err)
		}
	}
	got := make(chan struct{})
	go func() {
		_, _ = p.Acquire(ctx)
		close(got)
	}()
	clk.waitFor(t, 1)
	select {
	case <-got:
		t.Fatal("the sixth visitor call went through at once")
	default:
	}
	clk.Advance(199 * time.Millisecond)
	select {
	case <-got:
		t.Fatal("the sixth call went through before a token refilled")
	case <-time.After(20 * time.Millisecond):
	}
	clk.Advance(2 * time.Millisecond)
	select {
	case <-got:
	case <-time.After(2 * time.Second):
		t.Fatal("the sixth call did not go through once a token refilled")
	}
}

func TestBackgroundStaysAtTwoPerSecond(t *testing.T) {
	clk := newFakeClock()
	p := newPacer(2, clk)
	var mu sync.Mutex
	var starts []time.Time
	var wg sync.WaitGroup
	const n = 9
	for range n {
		wg.Go(func() {
			rel, err := p.Acquire(t.Context())
			if err != nil {
				t.Error(err)
				return
			}
			mu.Lock()
			starts = append(starts, clk.Now())
			mu.Unlock()
			rel()
		})
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	for {
		select {
		case <-finished:
			mu.Lock()
			defer mu.Unlock()
			if len(starts) != n {
				t.Fatalf("%d starts, want %d", len(starts), n)
			}
			for i := range starts {
				for j := i + 1; j < len(starts); j++ {
					if starts[j].Sub(starts[i]) < time.Second && j-i >= 2 {
						t.Fatalf("calls %d and %d started %v apart, more than 2 in a second", i, j, starts[j].Sub(starts[i]))
					}
				}
			}
			if span := starts[n-1].Sub(starts[0]); span < 4*time.Second {
				t.Fatalf("%d calls took %v, 2 a second needs at least 4s", n, span)
			}
			return
		case <-time.After(time.Millisecond):
			clk.Advance(100 * time.Millisecond)
		}
	}
}

func TestBackgroundKeepsOneInFlight(t *testing.T) {
	clk := newFakeClock()
	p := newPacer(2, clk)
	first, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	second := make(chan func(), 1)
	go func() {
		rel, err := p.Acquire(t.Context())
		if err == nil {
			second <- rel
		}
	}()
	for range 50 {
		clk.Advance(time.Second)
		time.Sleep(time.Millisecond)
	}
	select {
	case <-second:
		t.Fatal("a second background call started while the first was in flight")
	default:
	}
	first()
	for {
		select {
		case rel := <-second:
			rel()
			return
		case <-time.After(time.Millisecond):
			clk.Advance(time.Second)
		}
	}
}

func TestBackgroundWaitEndsWithItsContext(t *testing.T) {
	clk := newFakeClock()
	p := newPacer(2, clk)
	hold, err := p.Acquire(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	errc := make(chan error, 1)
	go func() { _, err := p.Acquire(ctx); errc <- err }()
	time.Sleep(10 * time.Millisecond)
	cancel()
	select {
	case err := <-errc:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("a background wait ignored its context")
	}
	hold()
	// the slot is free again for the next call
	next := make(chan error, 1)
	go func() {
		rel, err := p.Acquire(t.Context())
		if err == nil {
			rel()
		}
		next <- err
	}()
	for done := false; !done; {
		select {
		case err := <-next:
			if err != nil {
				t.Fatal(err)
			}
			done = true
		case <-time.After(time.Millisecond):
			clk.Advance(100 * time.Millisecond)
		}
	}

	paced, stop := context.WithCancel(t.Context())
	stop()
	if _, err := p.Acquire(WithVisitor(paced)); !errors.Is(err, context.Canceled) {
		t.Fatalf("an ended visitor context got %v", err)
	}
}

func TestLaneOfContext(t *testing.T) {
	if LaneOf(t.Context()) != Background || LaneOf(WithVisitor(t.Context())) != Visitor {
		t.Fatal("the default lane is background and WithVisitor marks a visitor")
	}
}
