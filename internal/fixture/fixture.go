// Package fixture builds a token service whose sources answer from the recordings in testdata, for the tests of the service and the server
package fixture

import (
	"context"
	"errors"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/index"
	"tokens/internal/rpc"
	"tokens/internal/sources"
	"tokens/internal/store"
	"tokens/internal/testkit"
	"tokens/internal/tokens"
)

const (
	Ethereum = "eip155:1"
	Monad    = "eip155:143"
)

var (
	EthUSDC = evm.MustAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48")
	EthMKR  = evm.MustAddress("0x9f8f72aa9304c8b593d555f12ef6589cc3a579a2")
	// EthEOA is an account without code, listed by Blockscout with is_contract false
	EthEOA  = evm.MustAddress("0x37305b1cd40574e4c5ce33f8e8306be057fd7341")
	MonUSDC = evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603")
	MonWMON = evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a")
)

// Now is a time on the day the sources were recorded
var Now = time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)

// Fakes are the switches a test can flip
type Fakes struct {
	mu          sync.Mutex
	PricesDown  bool
	GeckoDown   bool
	HyperDown   bool
	IndexFull   bool
	IndexState  string
	HyperCalls  int
	EnsureCalls int
}

func (f *Fakes) get(fn func(*Fakes) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// Service returns a service on recorded answers: contract reads, Blockscout, GeckoTerminal and prices for
// USDC and MKR on Ethereum and USDC and WMON on Monad, the recorded HyperSync page of MKR, and a Monad index
// whose USDC rows are derived from the recorded HyperSync page of USDC
func Service(t testing.TB) (*tokens.Service, *Fakes) {
	t.Helper()
	f := &Fakes{IndexState: store.StateReady}
	cfg := Config()
	bs := serve(t, f, map[string]string{
		"/api/v2/tokens/" + EthUSDC.String():              "blockscout/token.usdc.json",
		"/api/v2/tokens/" + EthMKR.String():               "blockscout/token.mkr.json",
		"/api/v2/tokens/" + EthUSDC.String() + "/holders": "blockscout/holders.usdc.page1.json",
		"/api/v2/tokens/" + EthUSDC.String() + "/holders?address_hash=0x2f9ec5662c1199c028edc1f3eca0c22aa279f2b4&items_count=50&value=100000000090000": "blockscout/holders.usdc.page2.json",
	}, nil)
	gt := serve(t, f, map[string]string{
		"/networks/monad/tokens/" + MonUSDC.String() + "/info": "gecko/info.usdc.json",
		"/networks/monad/tokens/" + MonUSDC.String():           "gecko/token.usdc.json",
		"/networks/monad/tokens/" + MonWMON.String() + "/info": "gecko/info.wmon.json",
	}, func(f *Fakes) bool { return f.GeckoDown })
	pr := serve(t, f, map[string]string{
		"/internal/v1/chains/eip155:143/tokens/" + MonUSDC.String() + "/prices?range=1d": "prices/monad_usdc.json",
	}, func(f *Fakes) bool { return f.PricesDown })

	usdc := page(t, "monad_usdc")
	idx := &fakeIndex{f: f, page: usdc}
	svc := tokens.New(tokens.Deps{
		Config:     cfg,
		Chains:     map[string]tokens.Chain{Ethereum: newChain(t, "ethereum"), Monad: newChain(t, "monad")},
		Blockscout: sources.NewBlockscout(bs.URL, 1000),
		Gecko:      sources.NewGecko(gt.URL, "monad", 600),
		Prices:     sources.NewPrices(pr.URL),
		Logs:       &fakeLogs{f: f, page: page(t, "eth_mkr"), head: 26180999},
		Index:      idx,
		Store:      idx,
		Now:        func() time.Time { return Now },
	})
	return svc, f
}

// Config is the engine's own config with the Ethereum window cut to the recorded 1,000 blocks
func Config() config.Config {
	return config.Config{
		Chains: []config.Chain{
			{ID: Ethereum, Name: "ethereum", Holders: config.HoldersBlockscout},
			{ID: Monad, Name: "monad", Holders: config.HoldersIndex, WrappedNative: MonWMON},
		},
		HyperSyncPerMinute: 3, HyperSyncInteractive: 2,
		MetaCache: time.Minute, HoldersCache: time.Minute, GeckoCache: time.Minute, TransfersCache: time.Minute, PriceCache: time.Minute,
		TransferWindowBlocks: 1000, TransferMinBlocks: 20, TransferPageBudget: 3,
		Index: config.Index{MaxTokens: 50, BackfillBudget: 120, ActivityWindowBlocks: 20000, TailInterval: time.Minute, LagBlocks: 10, WindowDays: 7, TopPerDay: 100},
	}
}

func page(t testing.TB, name string) sources.Page {
	t.Helper()
	p, err := sources.ParsePage(testkit.Read(t, "hypersync/"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func serve(t testing.TB, f *Fakes, routes map[string]string, down func(*Fakes) bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if down != nil && f.get(down) {
			w.WriteHeader(http.StatusBadGateway)
			return
		}
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			q, _ := url.ParseQuery(r.URL.RawQuery)
			key += "?" + q.Encode()
		}
		file, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		_, _ = w.Write(testkit.Read(t, file))
	}))
	t.Cleanup(srv.Close)
	return srv
}

/* Chain */

type chain struct {
	meta    map[evm.Address][]evm.Result3
	headers map[uint64]rpc.Header
}

func newChain(t testing.TB, name string) *chain {
	t.Helper()
	c := &chain{meta: map[evm.Address][]evm.Result3{}, headers: map[uint64]rpc.Header{}}
	files := map[string]evm.Address{}
	switch name {
	case "ethereum":
		files = map[string]evm.Address{"eth_usdc": EthUSDC, "eth_mkr": EthMKR, "eth_eoa": EthEOA}
	case "monad":
		files = map[string]evm.Address{"monad_usdc": MonUSDC, "monad_wmon": MonWMON}
	}
	for file, token := range files {
		res, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/meta."+file+".json"), 4)
		if err != nil {
			t.Fatal(err)
		}
		c.meta[token] = res
	}
	headers := map[string][]uint64{"ethereum": {26180000, 26180999}, "monad": {1, 113000000, 113000039}}[name]
	for _, n := range headers {
		num, at := testkit.Header(t, "rpc/header."+name+"_"+itoa(n)+".json")
		c.headers[n] = rpc.Header{Number: num, Time: at}
	}
	return c
}

func itoa(n uint64) string { return new(big.Int).SetUint64(n).String() }

var errUnrecorded = errors.New("no recording for this call")

func (c *chain) Aggregate3(_ context.Context, calls []evm.Call3) ([]evm.Result3, error) {
	if len(calls) != 4 {
		return nil, errUnrecorded
	}
	res, ok := c.meta[calls[0].Target]
	if !ok {
		return nil, errUnrecorded
	}
	return res, nil
}

func (c *chain) HeaderByNumber(_ context.Context, n uint64) (rpc.Header, error) {
	h, ok := c.headers[n]
	if !ok {
		return rpc.Header{}, errUnrecorded
	}
	return h, nil
}

func (c *chain) BlockNumber(context.Context) (uint64, error) { return 0, errUnrecorded }

/* HyperSync */

type fakeLogs struct {
	f    *Fakes
	page sources.Page
	head uint64
}

func (l *fakeLogs) Page(_ context.Context, _ string, q sources.LogQuery, _ sources.Priority) (sources.Page, error) {
	l.f.mu.Lock()
	l.f.HyperCalls++
	down := l.f.HyperDown
	l.f.mu.Unlock()
	if down {
		return sources.Page{}, &sources.UnavailableError{Source: "hypersync", RetryAfter: 20 * time.Second, Err: errors.New("429")}
	}
	out := sources.Page{NextBlock: min(q.To, l.page.NextBlock), ArchiveHeight: l.head}
	for _, x := range l.page.Logs {
		if x.Block >= q.From && x.Block < q.To && slices.Contains(q.Addresses, x.Address) {
			out.Logs = append(out.Logs, x)
		}
	}
	return out, nil
}

func (l *fakeLogs) Height(context.Context, string) (uint64, error) { return l.head, nil }

func (l *fakeLogs) PausedFor() time.Duration { return 0 }

/* Index */

// fakeIndex holds Monad USDC in activity mode over the recorded page, with the positive balance changes of that page as its rows
type fakeIndex struct {
	f    *Fakes
	page sources.Page
}

func (x *fakeIndex) Chain() string { return Monad }

func (x *fakeIndex) row() store.Token {
	// the recorded query starts at block 113,000,000
	start := uint64(113000000)
	return store.Token{
		ID: 1, Chain: Monad, Token: MonUSDC, State: x.f.IndexState, Mode: store.ModeActivity, Decimals: 6,
		StartBlock: start, NextBlock: x.page.NextBlock, Holders: int64(len(x.rows())),
	}
}

func (x *fakeIndex) Lookup(_ context.Context, token evm.Address) (store.Token, bool, error) {
	if token != MonUSDC {
		return store.Token{}, false, nil
	}
	return x.row(), true, nil
}

func (x *fakeIndex) Ensure(_ context.Context, token evm.Address, decimals uint8) (store.Token, error) {
	x.f.mu.Lock()
	x.f.EnsureCalls++
	full := x.f.IndexFull
	x.f.mu.Unlock()
	if full && token != MonUSDC {
		return store.Token{}, store.ErrFull
	}
	if token != MonUSDC {
		return store.Token{ID: 2, Chain: Monad, Token: token, State: store.StateQueued, Mode: store.ModeFull, Decimals: decimals}, nil
	}
	return x.row(), nil
}

func (x *fakeIndex) Head(context.Context) uint64 { return x.page.ArchiveHeight }

func (x *fakeIndex) moves() ([]sources.Log, []index.Move) {
	var (
		logs  []sources.Log
		moves []index.Move
	)
	for _, l := range x.page.Logs {
		if m, ok := index.Decode(l, false); ok {
			logs, moves = append(logs, l), append(moves, m)
		}
	}
	return logs, moves
}

func (x *fakeIndex) rows() []store.Balance {
	_, moves := x.moves()
	var out []store.Balance
	for a, v := range index.Deltas(moves) {
		if v.Sign() > 0 {
			out = append(out, store.Balance{Holder: a, Balance: v})
		}
	}
	slices.SortFunc(out, func(a, b store.Balance) int {
		if c := b.Balance.Cmp(a.Balance); c != 0 {
			return c
		}
		return strings.Compare(a.Holder.String(), b.Holder.String())
	})
	return out
}

func (x *fakeIndex) Holders(_ context.Context, id int32, limit int, after *store.HolderCursor) ([]store.Balance, error) {
	if id != 1 {
		return nil, nil
	}
	rows := x.rows()
	if after != nil {
		i := slices.IndexFunc(rows, func(b store.Balance) bool {
			c := b.Balance.Cmp(after.Balance)
			return c < 0 || c == 0 && b.Holder.String() > after.Holder.String()
		})
		if i < 0 {
			return nil, nil
		}
		rows = rows[i:]
	}
	return rows[:min(limit, len(rows))], nil
}

func (x *fakeIndex) TopTransfers(_ context.Context, id int32, fromDay string, limit int) ([]store.Transfer, error) {
	if id != 1 {
		return nil, nil
	}
	logs, moves := x.moves()
	out := index.Largest(logs, moves, fromDay, 100)
	return out[:min(limit, len(out))], nil
}
