package sources

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"time"
)

// raceDelay is how long one connection attempt runs alone before the next address of the host is tried beside it
const raceDelay = 250 * time.Millisecond

type dialFunc func(ctx context.Context, network, address string) (net.Conn, error)

type ipResolver interface {
	LookupNetIP(ctx context.Context, network, host string) ([]netip.Addr, error)
}

// transport is shared by every source. Go dials a host's addresses one after another and gives the first a fifth of
// the 30 second dial timeout, so one dead address of HyperSync's five cost every new connection 6 seconds
var transport = newTransport()

func newTransport() *http.Transport {
	t := http.DefaultTransport.(*http.Transport).Clone()
	d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	t.DialContext = raceDial(d.DialContext, net.DefaultResolver, raceDelay)
	return t
}

// raceDial connects to whichever address of the host answers first, starting the next attempt after delay or at
// once when an attempt fails
func raceDial(dial dialFunc, r ipResolver, delay time.Duration) dialFunc {
	return func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, err
		}
		if _, err := netip.ParseAddr(host); err == nil {
			return dial(ctx, network, address)
		}
		family := "ip"
		switch network {
		case "tcp4":
			family = "ip4"
		case "tcp6":
			family = "ip6"
		}
		ips, err := r.LookupNetIP(ctx, family, host)
		if err != nil {
			return nil, err
		}
		if len(ips) == 0 {
			return nil, &net.DNSError{Err: "no addresses", Name: host, IsNotFound: true}
		}
		addrs := make([]string, len(ips))
		for i, ip := range ips {
			addrs[i] = net.JoinHostPort(ip.Unmap().String(), port)
		}
		return race(ctx, dial, network, addrs, delay)
	}
}

type dialResult struct {
	conn net.Conn
	err  error
}

func race(ctx context.Context, dial dialFunc, network string, addrs []string, delay time.Duration) (net.Conn, error) {
	if len(addrs) == 1 {
		return dial(ctx, network, addrs[0])
	}
	attempts, cancel := context.WithCancel(ctx)
	results := make(chan dialResult, len(addrs))
	started, running := 0, 0
	start := func() {
		addr := addrs[started]
		started++
		running++
		go func() {
			c, err := dial(attempts, network, addr)
			results <- dialResult{c, err}
		}()
	}
	// the attempts still running end with cancel, and a connection one of them made anyway is closed
	finish := func() {
		cancel()
		go func(n int) {
			for range n {
				if r := <-results; r.conn != nil {
					r.conn.Close()
				}
			}
		}(running)
	}
	start()
	timer := time.NewTimer(delay)
	defer timer.Stop()
	var errs []error
	for {
		select {
		case r := <-results:
			running--
			if r.err == nil {
				finish()
				return r.conn, nil
			}
			errs = append(errs, r.err)
			if started < len(addrs) {
				start()
				timer.Reset(delay)
			} else if running == 0 {
				cancel()
				return nil, errors.Join(errs...)
			}
		case <-timer.C:
			if started < len(addrs) {
				start()
				timer.Reset(delay)
			}
		case <-ctx.Done():
			finish()
			return nil, ctx.Err()
		}
	}
}
