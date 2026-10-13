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
