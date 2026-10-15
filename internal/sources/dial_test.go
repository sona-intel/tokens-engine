package sources

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"net/url"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

type fixedResolver struct {
	ips   []netip.Addr
	asked atomic.Int32
}

func (r *fixedResolver) LookupNetIP(context.Context, string, string) ([]netip.Addr, error) {
	r.asked.Add(1)
	return r.ips, nil
}

func addrs(t *testing.T, ips ...string) *fixedResolver {
	t.Helper()
	r := &fixedResolver{}
	for _, ip := range ips {
		r.ips = append(r.ips, netip.MustParseAddr(ip))
	}
	return r
}

type trackedConn struct {
	net.Conn
	addr   string
	closed atomic.Bool
}

func (c *trackedConn) Close() error {
	c.closed.Store(true)
	return c.Conn.Close()
}

// fakeNet answers each address the way behave says: hang until the dial is called off, fail, or connect after a wait
type fakeNet struct {
	mu     sync.Mutex
	behave map[string]string
	wait   map[string]time.Duration
	dialed []string
	conns  []*trackedConn
	// cancelled counts the hanging attempts that saw their context end
	cancelled atomic.Int32
}

func (f *fakeNet) dial(ctx context.Context, _, address string) (net.Conn, error) {
	f.mu.Lock()
	f.dialed = append(f.dialed, address)
	how, wait := f.behave[address], f.wait[address]
	f.mu.Unlock()
	switch how {
	case "hang":
		<-ctx.Done()
		f.cancelled.Add(1)
		return nil, ctx.Err()
	case "refuse":
		return nil, &net.OpError{Op: "dial", Net: "tcp", Err: syscall.ECONNREFUSED}
	}
	// a connect that ignores the cancel, like one that completed just as the race ended
	time.Sleep(wait)
	a, b := net.Pipe()
	go func() { _ = b.Close() }()
	c := &trackedConn{Conn: a, addr: address}
	f.mu.Lock()
	f.conns = append(f.conns, c)
	f.mu.Unlock()
	return c, nil
}

func (f *fakeNet) dialedAddrs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dialed...)
}

func TestRaceDialSkipsAnAddressThatNeverAnswers(t *testing.T) {
	f := &fakeNet{behave: map[string]string{"193.32.220.135:443": "hang"}}
	dial := raceDial(f.dial, addrs(t, "193.32.220.135", "193.32.220.131", "193.32.220.132"), 30*time.Millisecond)
	start := time.Now()
	c, err := dial(t.Context(), "tcp", "monad.hypersync.xyz:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if took := time.Since(start); took > 2*time.Second {
		t.Errorf("the dial took %s", took)
	}
	if got := c.(*trackedConn).addr; got != "193.32.220.131:443" {
		t.Errorf("connected to %s, want the second address", got)
	}
	// the hanging attempt is called off once another one connected
	deadline := time.Now().Add(2 * time.Second)
	for f.cancelled.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if f.cancelled.Load() != 1 {
		t.Error("the attempt on the dead address kept running")
	}
	if d := f.dialedAddrs(); len(d) != 2 {
		t.Errorf("dialed %v, the third address was not needed", d)
	}
}

func TestRaceDialTriesTheNextAddressAtOnceWhenOneFails(t *testing.T) {
	f := &fakeNet{behave: map[string]string{"10.0.0.1:443": "refuse"}}
	dial := raceDial(f.dial, addrs(t, "10.0.0.1", "10.0.0.2"), time.Hour)
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Second)
	defer cancel()
	c, err := dial(ctx, "tcp", "example.org:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.(*trackedConn).addr; got != "10.0.0.2:443" {
		t.Errorf("connected to %s", got)
	}
}

func TestRaceDialReportsEveryFailure(t *testing.T) {
	f := &fakeNet{behave: map[string]string{"10.0.0.1:443": "refuse", "10.0.0.2:443": "refuse"}}
	dial := raceDial(f.dial, addrs(t, "10.0.0.1", "10.0.0.2"), time.Hour)
	_, err := dial(t.Context(), "tcp", "example.org:443")
	if !errors.Is(err, syscall.ECONNREFUSED) {
		t.Fatalf("error %v", err)
	}
	if d := f.dialedAddrs(); len(d) != 2 {
		t.Errorf("dialed %v", d)
	}
}

func TestRaceDialClosesTheConnectionThatLost(t *testing.T) {
	// the first address connects late, after the second already won
	f := &fakeNet{behave: map[string]string{}, wait: map[string]time.Duration{"10.0.0.1:443": 200 * time.Millisecond}}
	dial := raceDial(f.dial, addrs(t, "10.0.0.1", "10.0.0.2"), 20*time.Millisecond)
	c, err := dial(t.Context(), "tcp", "example.org:443")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if got := c.(*trackedConn).addr; got != "10.0.0.2:443" {
		t.Fatalf("connected to %s", got)
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		done := false
		f.mu.Lock()
		for _, c := range f.conns {
			done = done || c.addr == "10.0.0.1:443" && c.closed.Load()
		}
		f.mu.Unlock()
		if done {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Error("the late connection of the first address was left open")
}

func TestRaceDialStopsWithItsContext(t *testing.T) {
	f := &fakeNet{behave: map[string]string{"10.0.0.1:443": "hang", "10.0.0.2:443": "hang"}}
	dial := raceDial(f.dial, addrs(t, "10.0.0.1", "10.0.0.2"), 10*time.Millisecond)
	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()
	if _, err := dial(ctx, "tcp", "example.org:443"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
}

func TestRaceDialSendsAnAddressStraightThrough(t *testing.T) {
	f := &fakeNet{behave: map[string]string{}}
	r := addrs(t, "10.0.0.9")
	c, err := raceDial(f.dial, r, time.Hour)(t.Context(), "tcp", "127.0.0.1:8080")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if r.asked.Load() != 0 || c.(*trackedConn).addr != "127.0.0.1:8080" {
		t.Errorf("asked the resolver %d times, connected to %s", r.asked.Load(), c.(*trackedConn).addr)
	}
}

// a GET through the transport reaches the server although the host's first address answers nothing
func TestTransportReachesAHostPastADeadAddress(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(`{"height":1}`)) }))
	defer srv.Close()
	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeNet{behave: map[string]string{"192.0.2.1:" + u.Port(): "hang"}}
	d := &net.Dialer{}
	dial := func(ctx context.Context, network, address string) (net.Conn, error) {
		if f.behave[address] == "hang" {
			return f.dial(ctx, network, address)
		}
		return d.DialContext(ctx, network, address)
	}
	tr := http.DefaultTransport.(*http.Transport).Clone()
	tr.DialContext = raceDial(dial, addrs(t, "192.0.2.1", "127.0.0.1"), 50*time.Millisecond)
	defer tr.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(t.Context(), 1500*time.Millisecond)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://hypersync.test:"+u.Port()+"/height", nil)
	if err != nil {
		t.Fatal(err)
	}
	resp, err := (&http.Client{Transport: tr}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("status %d", resp.StatusCode)
	}
}

func TestSourcesUseTheRacingTransport(t *testing.T) {
	if newHTTPClient(time.Second).Transport != transport || transport.DialContext == nil {
		t.Error("a source client dials with Go's serial dialer")
	}
}
