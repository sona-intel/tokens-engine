package sources

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

func TestPacerBurstThenRate(t *testing.T) {
	p := NewPacer(600, 2) // one every 100 ms after a burst of two
	start := time.Now()
	for range 4 {
		if err := p.Wait(t.Context(), Background); err != nil {
			t.Fatal(err)
		}
	}
	if d := time.Since(start); d < 150*time.Millisecond || d > time.Second {
		t.Errorf("4 grants took %s, want about 200ms", d)
	}
}

func TestPacerServesInteractiveFirst(t *testing.T) {
	p := NewPacer(600, 1)
	if err := p.Wait(t.Context(), Background); err != nil {
		t.Fatal(err)
	}
	var (
		mu    sync.Mutex
		order []Priority
		wg    sync.WaitGroup
	)
	wait := func(prio Priority) {
		defer wg.Done()
		if err := p.Wait(t.Context(), prio); err == nil {
			mu.Lock()
			order = append(order, prio)
			mu.Unlock()
		}
	}
	wg.Add(1)
	go wait(Background)
	time.Sleep(20 * time.Millisecond)
	wg.Add(1)
	go wait(Interactive)
	wg.Wait()
	if len(order) != 2 || order[0] != Interactive {
		t.Errorf("order %v, want the interactive waiter first", order)
	}
}

func TestPacerPauseAndCancel(t *testing.T) {
	p := NewPacer(6000, 5)
	p.Pause(150 * time.Millisecond)
	if p.PausedFor() <= 0 {
		t.Fatal("no pause")
	}
	start := time.Now()
	if err := p.Wait(t.Context(), Interactive); err != nil {
		t.Fatal(err)
	}
	if d := time.Since(start); d < 140*time.Millisecond {
		t.Errorf("granted after %s, inside the pause", d)
	}
	p.Pause(time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Millisecond)
	defer cancel()
	if err := p.Wait(ctx, Background); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("wait during a pause: %v", err)
	}
	p.mu.Lock()
	left := len(p.waiting[Background]) + len(p.waiting[Interactive])
	p.mu.Unlock()
	if left != 0 {
		t.Errorf("%d waiters left after cancel", left)
	}
}

// fakeClock drives a pacer without real waiting
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func jobPacer(c *fakeClock) *Pacer {
	p := NewPacer(2, 2).WithJobLane(1, time.Minute)
	p.now = c.now
	p.last = c.now()
	return p
}

func granted(p *Pacer, prio Priority) (ch chan struct{}) {
	ch = make(chan struct{})
	p.mu.Lock()
	p.waiting[prio] = append(p.waiting[prio], ch)
	p.dispatchLocked()
	p.mu.Unlock()
	return ch
}

func isOpen(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

func TestPacerJobLeavesASlotForVisitors(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	p := jobPacer(c)
	job := granted(p, Job)
	if !isOpen(job) {
		t.Fatal("a full bucket should grant a job")
	}
	// the job took one of two slots: a visitor that arrives right after is served without waiting
	if !isOpen(granted(p, Interactive)) {
		t.Error("a visitor waited behind a job")
	}
}

func TestPacerJobWaitsForReserveAndGap(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	p := jobPacer(c)
	if !isOpen(granted(p, Job)) {
		t.Fatal("first job")
	}
	second := granted(p, Job)
	// 30 s refills the bucket to two slots but the jobs are not a minute apart yet
	c.advance(30 * time.Second)
	p.mu.Lock()
	p.dispatchLocked()
	p.mu.Unlock()
	if isOpen(second) {
		t.Error("a second job ran within the minute")
	}
	c.advance(31 * time.Second)
	p.mu.Lock()
	p.dispatchLocked()
	p.mu.Unlock()
	if !isOpen(second) {
		t.Error("the second job did not run a minute later")
	}
}

func TestPacerVisitorsBeatWaitingJobs(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	p := jobPacer(c)
	// spend the bucket on visitors, then queue a job and another visitor: the visitor gets the next slot
	granted(p, Interactive)
	granted(p, Interactive)
	job := granted(p, Job)
	visitor := granted(p, Interactive)
	c.advance(31 * time.Second)
	p.mu.Lock()
	p.dispatchLocked()
	p.mu.Unlock()
	if !isOpen(visitor) || isOpen(job) {
		t.Errorf("visitor %v, job %v after one slot", isOpen(visitor), isOpen(job))
	}
}

func TestPacerJobTotalStaysInBudget(t *testing.T) {
	c := &fakeClock{t: time.Unix(1000, 0)}
	p := jobPacer(c)
	n := 0
	var chs []chan struct{}
	for range 5 {
		chs = append(chs, granted(p, Job))
		chs = append(chs, granted(p, Interactive))
	}
	for range 120 {
		c.advance(time.Second)
		p.mu.Lock()
		p.dispatchLocked()
		p.mu.Unlock()
	}
	for _, ch := range chs {
		if isOpen(ch) {
			n++
		}
	}
	// 2 a minute with a burst of 2 is at most 2 + 4 grants in 2 minutes
	if n > 6 {
		t.Errorf("%d grants in 2 minutes", n)
	}
}
