package cache

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func keep(d time.Duration) TTL[int] { return func(int, error) time.Duration { return d } }

func TestJoinsConcurrentCallers(t *testing.T) {
	c := New[int](10, time.Second)
	var calls atomic.Int32
	release := make(chan struct{})
	var wg sync.WaitGroup
	for range 5 {
		wg.Go(func() {
			v, err := c.Get(t.Context(), "k", keep(time.Minute), func(context.Context) (int, error) {
				calls.Add(1)
				<-release
				return 7, nil
			})
			if err != nil || v != 7 {
				t.Errorf("got %d %v", v, err)
			}
		})
	}
	time.Sleep(20 * time.Millisecond)
	close(release)
	wg.Wait()
	if calls.Load() != 1 {
		t.Errorf("%d fetches", calls.Load())
	}
	if v, _, ok := c.Peek("k"); !ok || v != 7 {
		t.Error("the answer was not kept")
	}
}

func TestExpiryAndErrors(t *testing.T) {
	c := New[int](10, time.Second)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	boom := errors.New("boom")
	ttl := func(_ int, err error) time.Duration {
		if err != nil {
			return 0
		}
		return time.Minute
	}
	n := 0
	fetch := func(context.Context) (int, error) {
		n++
		if n == 1 {
			return 0, boom
		}
		return n, nil
	}
	if _, err := c.Get(t.Context(), "k", ttl, fetch); !errors.Is(err, boom) {
		t.Fatal(err)
	}
	if v, _ := c.Get(t.Context(), "k", ttl, fetch); v != 2 {
		t.Errorf("an error was kept: got %d", v)
	}
	if v, _ := c.Get(t.Context(), "k", ttl, fetch); v != 2 {
		t.Errorf("a fresh answer was fetched again: got %d", v)
	}
	now = now.Add(2 * time.Minute)
	if v, _ := c.Get(t.Context(), "k", ttl, fetch); v != 3 {
		t.Errorf("an expired answer was served: got %d", v)
	}
}

// A caller that gives up does not stop the fetch, which still fills the cache
func TestFetchOutlivesCaller(t *testing.T) {
	c := New[int](10, time.Second)
	done := make(chan struct{})
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Millisecond)
	defer cancel()
	_, err := c.Get(ctx, "k", keep(time.Minute), func(fctx context.Context) (int, error) {
		defer close(done)
		time.Sleep(50 * time.Millisecond)
		return 9, fctx.Err()
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("caller got %v", err)
	}
	<-done
	time.Sleep(10 * time.Millisecond)
	if v, err, ok := c.Peek("k"); !ok || err != nil || v != 9 {
		t.Errorf("cache holds %d %v %v", v, err, ok)
	}
}

func TestEvictsWhenFull(t *testing.T) {
	c := New[int](10, time.Second)
	for i := range 25 {
		_, _ = c.Get(t.Context(), strconv.Itoa(i), keep(time.Duration(i+1)*time.Minute), func(context.Context) (int, error) { return i, nil })
	}
	if n := c.Len(); n > 10 {
		t.Errorf("%d entries, the limit is 10", n)
	}
	if _, _, ok := c.Peek("24"); !ok {
		t.Error("the newest entry was evicted")
	}
}
