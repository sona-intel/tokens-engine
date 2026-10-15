// Package fixture builds a token service whose sources answer from the recordings in testdata, for the tests of the service and the server
package fixture

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"math"
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
	"tokens/internal/holdermap"
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
	Arc      = "eip155:5042"
)

var (
	EthUSDC = evm.MustAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48")
	EthMKR  = evm.MustAddress("0x9f8f72aa9304c8b593d555f12ef6589cc3a579a2")
	// EthEOA is an account without code, listed by Blockscout with is_contract false
	EthEOA  = evm.MustAddress("0x37305b1cd40574e4c5ce33f8e8306be057fd7341")
	MonUSDC = evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603")
	MonWMON = evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a")
	// ArcUSDC is the ERC-20 view of Arc's native USDC, whose every move ArcEmitter logs at 18 decimals
	ArcUSDC    = evm.MustAddress("0x3600000000000000000000000000000000000000")
	ArcEmitter = evm.MustAddress("0xfffffffffffffffffffffffffffffffffffffffe")
	ArcEURC    = evm.MustAddress("0xbef5f6d51cb62b58e6a8f77868681825c6fe21c1")
)

// Now is a time on the day the sources were recorded
var Now = time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC)

// Fakes are the switches a test can flip
type Fakes struct {
	mu         sync.Mutex
	PricesDown bool
	GeckoDown  bool
	ListsDown  bool
	HyperDown  bool
	// HyperNoTimes drops the block times from the logs, as an incomplete answer would
	HyperNoTimes bool
	// HyperSlow holds back every HyperSync page for a while
	HyperSlow  time.Duration
	IndexFull  bool
	IndexState string
	// IndexMode is the mode of the indexed Monad USDC, activity unless a test sets full
	IndexMode   string
	HyperCalls  int
	EnsureCalls int
	// NativeCalls counts the getEthBalance batches the Arc chain answered
	NativeCalls int
	// Slow holds back the answers to a path for a while
	Slow map[string]time.Duration

	// Maps builds the holder maps from MapLogs into MapStore, BuildMaps runs its queue
	Maps      *holdermap.Builder
	MapLogs   *MapLogs
	MapStore  *MapStore
	mapBlocks mapBlocks
}

func (f *Fakes) get(fn func(*Fakes) bool) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return fn(f)
}

// SetSlow holds back every answer to path for d
func (f *Fakes) SetSlow(path string, d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.Slow == nil {
		f.Slow = map[string]time.Duration{}
	}
	f.Slow[path] = d
}

func (f *Fakes) slowFor(path string) time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.Slow[path]
}

// Service returns a service on recorded answers: contract reads, Blockscout, GeckoTerminal and prices for
// USDC and MKR on Ethereum and USDC and WMON on Monad, the recorded HyperSync page of MKR, and a Monad index
// whose USDC rows are derived from the recorded HyperSync page of USDC
func Service(t testing.TB) (*tokens.Service, *Fakes) {
	t.Helper()
	return build(t, curated)
}

// ShortTransferCache is the same service whose Ethereum transfer windows stay fresh for 20 ms only, to test what is served after that
func ShortTransferCache(t testing.TB) (*tokens.Service, *Fakes) {
	t.Helper()
	return build(t, shortCache)
}

// Uncurated is the same service with no curated token list, so a search finds only what GeckoTerminal and the index know
func Uncurated(t testing.TB) (*tokens.Service, *Fakes) {
	t.Helper()
	return build(t, uncurated)
}

// ListsFailed is the same service whose curated lists all failed to load on start
func ListsFailed(t testing.TB) (*tokens.Service, *Fakes) {
	t.Helper()
	return build(t, listsFailed)
}

// WithArc is the same service serving Arc too, its index holding the USDC rows of the recorded Arc logs
func WithArc(t testing.TB) (*tokens.Service, *Fakes) {
	t.Helper()
	return build(t, withArc)
}

type listMode int

const (
	curated listMode = iota
	uncurated
	listsFailed
	shortCache
	withArc
)

func build(t testing.TB, mode listMode) (*tokens.Service, *Fakes) {
	t.Helper()
	f := &Fakes{IndexState: store.StateReady, IndexMode: store.ModeActivity, ListsDown: mode == listsFailed}
	cfg := Config()
	if mode == shortCache {
		cfg.TransfersCache = 20 * time.Millisecond
	}
	if mode == withArc {
		cfg.Chains = append(cfg.Chains, ArcChain())
	}
	bs := serve(t, f, map[string]string{
		"/api/v2/tokens/" + EthUSDC.String():              "blockscout/token.usdc.json",
		"/api/v2/tokens/" + EthMKR.String():               "blockscout/token.mkr.json",
		"/api/v2/tokens/" + EthUSDC.String() + "/holders": "blockscout/holders.usdc.page1.json",
		"/api/v2/tokens/" + EthUSDC.String() + "/holders?address_hash=0x2f9ec5662c1199c028edc1f3eca0c22aa279f2b4&items_count=50&value=100000000090000": "blockscout/holders.usdc.page2.json",
		"/api/v2/tokens/" + EthUNI.String() + "/holders": "blockscout/holders.uni.page1.json",
		"/api/v2/tokens/" + EthUNI.String() + "/holders?address_hash=0xc52f8618ef7986ed7b47b4337b2249dea857ae4e&items_count=50&value=2690684850100000000000000": "blockscout/holders.uni.page2.json",
	}, nil)
	geckoRoutes := map[string]string{
		"/networks/monad/tokens/" + MonUSDC.String() + "/info": "gecko/info.usdc.json",
		"/networks/monad/tokens/" + MonUSDC.String():           "gecko/token.usdc.json",
		"/networks/monad/tokens/" + MonWMON.String() + "/info": "gecko/info.wmon.json",
	}
	for _, network := range []string{"eth", "monad"} {
		for _, q := range []string{"usdc", "mon", "wmon", "pepe"} {
			geckoRoutes["/search/pools?include=base_token%2Cquote_token&network="+network+"&query="+q] = "gecko/search." + network + "." + q + ".json"
		}
	}
	if mode == withArc {
		geckoRoutes["/networks/arc/tokens/"+ArcUSDC.String()+"/info"] = "gecko/info.arc_usdc.json"
		geckoRoutes["/networks/arc/tokens/"+ArcUSDC.String()] = "gecko/token.arc_usdc.json"
	}
	gt := serve(t, f, geckoRoutes, func(f *Fakes) bool { return f.GeckoDown })
	listFiles := serve(t, f, map[string]string{
		"/uniswap": "lists/uniswap.json", "/monad": "lists/monad.json",
	}, func(f *Fakes) bool { return f.ListsDown })
	pr := serve(t, f, map[string]string{
		"/internal/v1/chains/eip155:143/tokens/" + MonUSDC.String() + "/prices?range=1d": "prices/monad_usdc.json",
	}, func(f *Fakes) bool { return f.PricesDown })

	var lists *tokens.Lists
	if mode != uncurated {
		cfg.TokenLists = []config.TokenList{
			{Name: "uniswap-default", URL: listFiles.URL + "/uniswap"},
			{Name: "monad-mainnet", URL: listFiles.URL + "/monad"},
		}
		lists = tokens.NewLists(cfg, sources.NewTokenLists(time.Second))
		lists.Load(t.Context())
	}
	mon, _ := cfg.Chain(Monad)
	// the recorded query starts at block 113,000,000
	idx := &fakeIndex{f: f, chain: Monad, id: 1, start: 113000000, feed: index.FeedOf(mon, MonUSDC, 6), page: page(t, "monad_usdc")}
	eth := newChain(t, "ethereum")
	chains := map[string]tokens.Chain{Ethereum: eth, Monad: newChain(t, "monad")}
	indexes := map[string]tokens.Index{Monad: idx}
	held := fakeStore{idx}
	if mode == withArc {
		arcIdx := &fakeIndex{f: f, chain: Arc, id: 3, start: 25941000, feed: index.FeedOf(ArcChain(), ArcUSDC, 6), page: ArcPage(t)}
		arc := newChain(t, "arc")
		arc.f, arc.native = f, recordedNative(t, arcIdx.feed, arcIdx.rows())
		chains[Arc], indexes[Arc], held = arc, arcIdx, append(held, arcIdx)
	}
	f.MapLogs, f.MapStore, f.mapBlocks = NewMapLogs(UNIMapPages(t), UNIMapHead), NewMapStore(), mapBlocks{c: eth}
	f.Maps = holdermap.New(cfg.HolderMap, f.MapLogs, f.MapStore, slog.New(slog.DiscardHandler)).WithClock(func() time.Time { return Now })
	svc := tokens.New(tokens.Deps{
		Config:     cfg,
		Chains:     chains,
		Blockscout: sources.NewBlockscout(bs.URL, 1000),
		Gecko:      sources.NewGecko(gt.URL, 6000),
		Prices:     sources.NewPrices(pr.URL),
		Logs:       &fakeLogs{f: f, page: page(t, "eth_mkr"), head: 26180999},
		Indexes:    indexes,
		Store:      held,
		Lists:      lists,
		HolderMaps: f.Maps,
		Now:        func() time.Time { return Now },
	})
	return svc, f
}

// ArcChain is Arc as the engine's config has it
func ArcChain() config.Chain {
	return config.Chain{
		ID: Arc, Name: "arc", Holders: config.HoldersIndex, GeckoNetwork: "arc", ActivityWindowBlocks: 14400,
		Native: &config.Native{Token: ArcUSDC, Emitter: ArcEmitter, Decimals: 18},
	}
}

// ArcPage is the recorded eth_getLogs answer of Arc blocks 25,941,000 to 25,941,019 as a HyperSync page
func ArcPage(t testing.TB) sources.Page {
	t.Helper()
	p, err := sources.ParsePage(testkit.LogsPage(t, "rpc/logs.arc_usdc.json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// recordedNative is the recorded getEthBalance answer of each holder, the recording must have asked for exactly these holders
func recordedNative(t testing.TB, f index.Feed, rows []store.Balance) map[evm.Address]evm.Result3 {
	t.Helper()
	calls := make([]evm.Call3, len(rows))
	for i, r := range rows {
		calls[i] = f.BalanceCall(r.Holder)
	}
	if !bytes.Equal(evm.EncodeAggregate3(calls), testkit.CallData(t, "rpc/ethbalance.arc_usdc.request.json")) {
		t.Fatal("the recorded getEthBalance calls are not the holders of the recorded logs")
	}
	res, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/ethbalance.arc_usdc.json"), len(rows))
	if err != nil {
		t.Fatal(err)
	}
	out := make(map[evm.Address]evm.Result3, len(rows))
	for i, r := range rows {
		out[r.Holder] = res[i]
	}
	return out
}

// Config is the engine's own config with the Ethereum window cut to the recorded 1,000 blocks
func Config() config.Config {
	return config.Config{
		Chains: []config.Chain{
			{ID: Ethereum, Name: "ethereum", Holders: config.HoldersBlockscout, GeckoNetwork: "eth"},
			{ID: Monad, Name: "monad", Holders: config.HoldersIndex, WrappedNative: MonWMON, GeckoNetwork: "monad"},
		},
		HyperSyncPerMinute: 3, HyperSyncInteractive: 2,
		MetaCache: time.Minute, HoldersCache: time.Minute, GeckoCache: time.Minute, SearchCache: 5 * time.Minute, TokenListRefresh: 24 * time.Hour, TokenListTimeout: time.Second, TransfersCache: time.Minute, PriceCache: time.Minute,
		TransferWindowBlocks: 1000, TransferMinBlocks: 20, TransferPageBudget: 3,
		Index:     config.Index{MaxTokens: 50, BackfillBudget: 120, ActivityWindowBlocks: 20000, TailInterval: time.Minute, LagBlocks: 10, WindowDays: 7, TopPerDay: 100},
		HolderMap: config.HolderMap{WindowDays: 30, PageBudget: 8, FreshFor: 12 * time.Hour, KeepFor: 168 * time.Hour, QueueMax: 20, RetryFailedAfter: 10 * time.Minute},
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
		if d := f.slowFor(r.URL.Path); d > 0 {
			select {
			case <-time.After(d):
			case <-r.Context().Done():
				return
			}
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
	// native answers getEthBalance by holder, f counts the batches
	native map[evm.Address]evm.Result3
	f      *Fakes
}

func newChain(t testing.TB, name string) *chain {
	t.Helper()
	c := &chain{meta: map[evm.Address][]evm.Result3{}, headers: map[uint64]rpc.Header{}}
	files := map[string]evm.Address{}
	switch name {
	case "ethereum":
		files = map[string]evm.Address{"eth_usdc": EthUSDC, "eth_mkr": EthMKR, "eth_eoa": EthEOA, "eth_uni": EthUNI}
	case "monad":
		files = map[string]evm.Address{"monad_usdc": MonUSDC, "monad_wmon": MonWMON}
	case "arc":
		files = map[string]evm.Address{"arc_usdc": ArcUSDC, "arc_eurc": ArcEURC}
	}
	for file, token := range files {
		res, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/meta."+file+".json"), 4)
		if err != nil {
			t.Fatal(err)
		}
		c.meta[token] = res
	}
	headers := map[string][]uint64{"ethereum": {26180000, 26180999, UNIMapFrom, UNIMapHead}, "monad": {1, 113000000, 113000039}, "arc": {25941000, 25941019}}[name]
	for _, n := range headers {
		num, at := testkit.Header(t, "rpc/header."+name+"_"+itoa(n)+".json")
		c.headers[n] = rpc.Header{Number: num, Time: at}
	}
	return c
}

func itoa(n uint64) string { return new(big.Int).SetUint64(n).String() }

var errUnrecorded = errors.New("no recording for this call")

func (c *chain) Aggregate3(_ context.Context, calls []evm.Call3) ([]evm.Result3, error) {
	if len(calls) > 0 && calls[0].Target == evm.Multicall3 {
		return c.nativeBalances(calls)
	}
	if len(calls) != 4 {
		return nil, errUnrecorded
	}
	res, ok := c.meta[calls[0].Target]
	if !ok {
		return nil, errUnrecorded
	}
	return res, nil
}

func (c *chain) nativeBalances(calls []evm.Call3) ([]evm.Result3, error) {
	out := make([]evm.Result3, len(calls))
	for i, call := range calls {
		if call.Target != evm.Multicall3 || len(call.Data) != 36 || !bytes.Equal(call.Data[:4], evm.SelGetEthBalance[:]) {
			return nil, errUnrecorded
		}
		r, ok := c.native[evm.Address(call.Data[16:])]
		if !ok {
			return nil, errUnrecorded
		}
		out[i] = r
	}
	c.f.mu.Lock()
	c.f.NativeCalls++
	c.f.mu.Unlock()
	return out, nil
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

func (l *fakeLogs) Page(ctx context.Context, _ string, q sources.LogQuery, _ sources.Priority) (sources.Page, error) {
	l.f.mu.Lock()
	l.f.HyperCalls++
	down, noTimes, slow := l.f.HyperDown, l.f.HyperNoTimes, l.f.HyperSlow
	l.f.mu.Unlock()
	if slow > 0 {
		select {
		case <-time.After(slow):
		case <-ctx.Done():
			return sources.Page{}, ctx.Err()
		}
	}
	if down {
		return sources.Page{}, &sources.UnavailableError{Source: "hypersync", RetryAfter: 20 * time.Second, Err: errors.New("429")}
	}
	out := sources.Page{NextBlock: min(q.To, l.page.NextBlock), ArchiveHeight: l.head}
	for _, x := range l.page.Logs {
		if x.Block >= q.From && x.Block < q.To && slices.Contains(q.Addresses, x.Address) {
			if noTimes {
				x.Time = 0
			}
			out.Logs = append(out.Logs, x)
		}
	}
	return out, nil
}

func (l *fakeLogs) Height(context.Context, string) (uint64, error) { return l.head, nil }

func (l *fakeLogs) PausedFor() time.Duration { return 0 }

/* Index */

// fakeIndex holds one token over the recorded page read through the token's feed, its rows the positive balance changes
type fakeIndex struct {
	f     *Fakes
	chain string
	id    int32
	start uint64
	feed  index.Feed
	page  sources.Page
}

func (x *fakeIndex) Chain() string { return x.chain }

func (x *fakeIndex) row() store.Token {
	return store.Token{
		ID: x.id, Chain: x.chain, Token: x.feed.Token, State: x.f.IndexState, Mode: x.f.IndexMode, Decimals: x.feed.Decimals,
		StartBlock: x.start, NextBlock: x.page.NextBlock, Holders: int64(len(x.rows())),
	}
}

func (x *fakeIndex) Lookup(_ context.Context, token evm.Address) (store.Token, bool, error) {
	if token != x.feed.Token {
		return store.Token{}, false, nil
	}
	return x.row(), true, nil
}

func (x *fakeIndex) Ensure(_ context.Context, token evm.Address, decimals uint8) (store.Token, error) {
	x.f.mu.Lock()
	x.f.EnsureCalls++
	full := x.f.IndexFull
	x.f.mu.Unlock()
	if full && token != x.feed.Token {
		return store.Token{}, store.ErrFull
	}
	if token != x.feed.Token {
		return store.Token{ID: x.id + 1, Chain: x.chain, Token: token, State: store.StateQueued, Mode: store.ModeFull, Decimals: decimals}, nil
	}
	return x.row(), nil
}

func (x *fakeIndex) Head(context.Context) uint64 { return x.page.ArchiveHeight }

func (x *fakeIndex) moves() ([]sources.Log, []index.Move) {
	return x.feed.Moves(x.page.Logs, 0, math.MaxUint64)
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

func (x *fakeIndex) holders(limit int, after *store.HolderCursor) []store.Balance {
	rows := x.rows()
	if after != nil {
		i := slices.IndexFunc(rows, func(b store.Balance) bool {
			c := b.Balance.Cmp(after.Balance)
			return c < 0 || c == 0 && b.Holder.String() > after.Holder.String()
		})
		if i < 0 {
			return nil
		}
		rows = rows[i:]
	}
	return rows[:min(limit, len(rows))]
}

func (x *fakeIndex) transfers(fromDay string, limit int) []store.Transfer {
	logs, moves := x.moves()
	out := index.Largest(logs, moves, fromDay, 100)
	return out[:min(limit, len(out))]
}

// fakeStore answers the reads of the service from the fake index whose token row has the asked id
type fakeStore []*fakeIndex

func (s fakeStore) Holders(_ context.Context, id int32, limit int, after *store.HolderCursor) ([]store.Balance, error) {
	for _, x := range s {
		if x.id == id {
			return x.holders(limit, after), nil
		}
	}
	return nil, nil
}

func (s fakeStore) TopTransfers(_ context.Context, id int32, fromDay string, limit int) ([]store.Transfer, error) {
	for _, x := range s {
		if x.id == id {
			return x.transfers(fromDay, limit), nil
		}
	}
	return nil, nil
}

func (s fakeStore) IndexedTokens(_ context.Context, chain string) ([]evm.Address, error) {
	for _, x := range s {
		if x.chain == chain {
			return []evm.Address{x.feed.Token}, nil
		}
	}
	return nil, nil
}
