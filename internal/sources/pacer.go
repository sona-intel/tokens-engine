package sources

import (
	"context"
	"sync"
	"time"
)

// Priority orders the waiters of a pacer: a visitor's request goes before background work
type Priority int

const (
	Background Priority = iota
	Interactive
)

// Pacer hands out request slots at a steady rate with a small burst, interactive waiters first, and stops for a pause a source asked for
type Pacer struct {
	gap   time.Duration
	burst int
	now   func() time.Time

	mu      sync.Mutex
	tokens  float64
	last    time.Time
	paused  time.Time
	waiting [2][]chan struct{}
	timer   *time.Timer
}

// NewPacer allows perMinute requests a minute on average and up to burst at once
func NewPacer(perMinute float64, burst int) *Pacer {
	p := &Pacer{gap: time.Duration(float64(time.Minute) / perMinute), burst: max(burst, 1), now: time.Now}
	p.tokens = float64(p.burst)
	p.last = p.now()
	return p
}

// Wait blocks until the caller may send one request or ctx ends
func (p *Pacer) Wait(ctx context.Context, prio Priority) error {
	ch := make(chan struct{})
	p.mu.Lock()
	p.waiting[prio] = append(p.waiting[prio], ch)
	p.dispatchLocked()
	p.mu.Unlock()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		p.mu.Lock()
		defer p.mu.Unlock()
		select {
		case <-ch:
			// the slot was granted while ctx ended, so it is used up
			return ctx.Err()
		default:
		}
		p.remove(prio, ch)
		return ctx.Err()
	}
}

// Pause stops every grant until now plus d, a later pause wins
func (p *Pacer) Pause(d time.Duration) {
	if d <= 0 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if until := p.now().Add(d); until.After(p.paused) {
		p.paused = until
	}
	p.dispatchLocked()
}

// PausedFor is how long the current pause still lasts
func (p *Pacer) PausedFor() time.Duration {
	p.mu.Lock()
	defer p.mu.Unlock()
	return max(p.paused.Sub(p.now()), 0)
}

func (p *Pacer) remove(prio Priority, ch chan struct{}) {
	q := p.waiting[prio]
	for i, c := range q {
		if c == ch {
			p.waiting[prio] = append(q[:i:i], q[i+1:]...)
			return
		}
	}
}

func (p *Pacer) refill(now time.Time) {
	if now.After(p.last) {
		p.tokens = min(float64(p.burst), p.tokens+float64(now.Sub(p.last))/float64(p.gap))
		p.last = now
	}
}

// dispatchLocked grants what it can now and arms a timer for the rest
func (p *Pacer) dispatchLocked() {
	now := p.now()
	p.refill(now)
	for len(p.waiting[Interactive])+len(p.waiting[Background]) > 0 {
		if wait := p.paused.Sub(now); wait > 0 {
			p.arm(wait)
			return
		}
		if p.tokens < 1 {
			p.arm(time.Duration((1 - p.tokens) * float64(p.gap)))
			return
		}
		p.tokens--
		prio := Background
		if len(p.waiting[Interactive]) > 0 {
			prio = Interactive
		}
		close(p.waiting[prio][0])
		p.waiting[prio] = p.waiting[prio][1:]
	}
}

func (p *Pacer) arm(d time.Duration) {
	d = max(d, time.Millisecond)
	if p.timer != nil {
		p.timer.Stop()
	}
	p.timer = time.AfterFunc(d, func() {
		p.mu.Lock()
		defer p.mu.Unlock()
		p.dispatchLocked()
	})
}
