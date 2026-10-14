package rpc

import (
	"context"
	"sync"
	"time"
)

// Lane tells the pacer whose eth_call a request is
type Lane int

const (
	// Background is the default: index reads, pre-warm and everything else no visitor's request caused
	Background Lane = iota
	// Visitor is a call that a visitor's request triggers, served first and never behind background calls
	Visitor
)

const (
	// DefaultBackgroundPerSecond is how many background eth_call requests a chain gets a second, one in flight at a time
	DefaultBackgroundPerSecond = 2.0
	// VisitorPerSecond and VisitorBurst limit the visitor lane of a chain
	VisitorPerSecond = 5.0
	VisitorBurst     = 5
)

type laneKey struct{}

// WithVisitor marks ctx as a visitor's request, so its eth_calls use the visitor lane
func WithVisitor(ctx context.Context) context.Context {
	return context.WithValue(ctx, laneKey{}, Visitor)
}

// LaneOf is the lane of ctx, Background unless WithVisitor marked it
func LaneOf(ctx context.Context) Lane {
	if l, ok := ctx.Value(laneKey{}).(Lane); ok {
		return l
	}
	return Background
}

// clock is the time source of a Pacer, tests replace it
type clock interface {
	Now() time.Time
	// After sends once d has passed, stop releases it
	After(d time.Duration) (<-chan time.Time, func())
}

type realClock struct{}

func (realClock) Now() time.Time { return time.Now() }

func (realClock) After(d time.Duration) (<-chan time.Time, func()) {
	t := time.NewTimer(d)
	return t.C, func() { t.Stop() }
}

// Pacer holds the eth_call budget of one chain. The visitor lane is a token bucket that never looks at the background lane;
// the background lane starts at most one request per gap and keeps one in flight.
type Pacer struct {
	clk clock
	gap time.Duration

	bgSem chan struct{}

	mu      sync.Mutex
	bgNext  time.Time
	tokens  float64
	last    time.Time
	vPerSec float64
	vBurst  float64
}

// NewPacer allows backgroundPerSecond background requests a second with one in flight, and the visitor lane VisitorPerSecond with VisitorBurst at once
func NewPacer(backgroundPerSecond float64) *Pacer {
	return newPacer(backgroundPerSecond, realClock{})
}

func newPacer(backgroundPerSecond float64, clk clock) *Pacer {
	p := &Pacer{
		clk:     clk,
		gap:     time.Duration(float64(time.Second) / backgroundPerSecond),
		bgSem:   make(chan struct{}, 1),
		vPerSec: VisitorPerSecond,
		vBurst:  VisitorBurst,
	}
	p.tokens = p.vBurst
	p.last = clk.Now()
	return p
}

// Acquire waits for a slot in the lane of ctx, ctx ending ends the wait. The caller calls release when its request is done.
func (p *Pacer) Acquire(ctx context.Context) (release func(), err error) {
	if LaneOf(ctx) == Visitor {
		return func() {}, p.visitor(ctx)
	}
	return p.background(ctx)
}

func (p *Pacer) visitor(ctx context.Context) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		p.mu.Lock()
		now := p.clk.Now()
		if now.After(p.last) {
			p.tokens = min(p.vBurst, p.tokens+now.Sub(p.last).Seconds()*p.vPerSec)
			p.last = now
		}
		if p.tokens >= 1 {
			p.tokens--
			p.mu.Unlock()
			return nil
		}
		wait := time.Duration((1 - p.tokens) / p.vPerSec * float64(time.Second))
		p.mu.Unlock()
		if err := p.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

func (p *Pacer) background(ctx context.Context) (func(), error) {
	select {
	case p.bgSem <- struct{}{}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	release := func() { <-p.bgSem }
	p.mu.Lock()
	start := p.bgNext
	if now := p.clk.Now(); start.Before(now) {
		start = now
	}
	// the next request starts a gap after this one, whether it comes straight away or after a wait
	p.bgNext = start.Add(p.gap)
	wait := start.Sub(p.clk.Now())
	p.mu.Unlock()
	if wait > 0 {
		if err := p.sleep(ctx, wait); err != nil {
			// the call never started, so its slot goes back
			p.mu.Lock()
			p.bgNext = start
			p.mu.Unlock()
			release()
			return nil, err
		}
	}
	return release, nil
}

func (p *Pacer) sleep(ctx context.Context, d time.Duration) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	ch, stop := p.clk.After(max(d, time.Millisecond))
	defer stop()
	select {
	case <-ch:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
