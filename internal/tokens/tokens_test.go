package tokens_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"math/big"
	"slices"
	"strings"
	"testing"
	"time"

	"tokens/internal/evm"
	"tokens/internal/fixture"
	"tokens/internal/index"
	"tokens/internal/sources"
	"tokens/internal/testkit"
	"tokens/internal/tokens"
)

func TestFormatUnits(t *testing.T) {
	cases := []struct {
		raw  string
		dec  uint8
		want string
	}{
		{"4312358755535047", 6, "4312358755.535047"},
		{"1582999999860100", 6, "1582999999.8601"},
		{"100000000090000", 6, "100000000.09"},
		{"1000000", 6, "1"},
		{"5", 6, "0.000005"},
		{"0", 18, "0"},
		{"83465892107442956720626", 18, "83465.892107442956720626"},
		{"-1500", 3, "-1.5"},
		{"42", 0, "42"},
	}
	for _, c := range cases {
		v, _ := new(big.Int).SetString(c.raw, 10)
		if got := tokens.FormatUnits(v, c.dec); got != c.want {
			t.Errorf("%s/%d: got %s, want %s", c.raw, c.dec, got, c.want)
		}
	}
}

func TestInfoEthereum(t *testing.T) {
	svc, _ := fixture.Service(t)
	info, err := svc.Info(t.Context(), fixture.Ethereum, fixture.EthUSDC)
	if err != nil {
		t.Fatal(err)
	}
	if *info.Name != "USD Coin" || *info.Symbol != "USDC" || info.Decimals != 6 || info.TotalSupply.Value == "" {
		t.Errorf("metadata %+v", info)
	}
	// the prices fixture has no Ethereum USDC price
	if info.PriceUSD != nil || info.FDVUSD != nil || !slices.Contains(info.Coverage, "token_price_unknown") {
		t.Errorf("price %v fdv %v coverage %v", info.PriceUSD, info.FDVUSD, info.Coverage)
	}
	if info.HolderCount == nil || *info.HolderCount != 9485913 || info.MarketCapUSD == nil || info.Top10Share == nil || *info.Top10Share <= 0 || *info.Top10Share >= 1 {
		t.Errorf("holders %v mcap %v top10 %v", info.HolderCount, info.MarketCapUSD, info.Top10Share)
	}
	if !slices.Equal(info.Sources, []string{"rpc", "prices", "blockscout"}) {
		t.Errorf("sources %v", info.Sources)
	}
	mkr, err := svc.Info(t.Context(), fixture.Ethereum, fixture.EthMKR)
	if err != nil || *mkr.Name != "Maker" || *mkr.Symbol != "MKR" || mkr.Top10Share != nil {
		t.Errorf("MKR %+v %v", mkr, err)
	}
}

func TestInfoMonad(t *testing.T) {
	svc, f := fixture.Service(t)
	info, err := svc.Info(t.Context(), fixture.Monad, fixture.MonUSDC)
	if err != nil {
		t.Fatal(err)
	}
	if info.PriceUSD == nil || info.FDVUSD == nil || info.MarketCapUSD == nil || info.HolderCount == nil || *info.HolderCount != 76440 || info.Top10Share == nil || *info.Top10Share != 0.772433 {
		t.Fatalf("info %+v", info)
	}
	// GeckoTerminal's market cap is USDC's on every chain, far above Monad's supply
	if !slices.Equal(info.Coverage, []string{"token_market_cap_multichain", "token_holder_count_provider"}) {
		t.Errorf("coverage %v", info.Coverage)
	}
	if f.EnsureCalls != 0 {
		t.Error("token info queued the token in the index")
	}

	f.PricesDown, f.GeckoDown = true, true
	wmon, err := svc.Info(t.Context(), fixture.Monad, fixture.MonWMON)
	if err != nil {
		t.Fatal(err)
	}
	if wmon.PriceUSD != nil || wmon.HolderCount != nil || !slices.Equal(wmon.Sources, []string{"rpc"}) || !slices.Equal(wmon.Coverage, []string{"token_source_unavailable"}) {
		t.Errorf("WMON with its sources down %+v", wmon)
	}
}

func TestInfoNotAToken(t *testing.T) {
	svc, _ := fixture.Service(t)
	if _, err := svc.Info(t.Context(), fixture.Ethereum, fixture.EthEOA); !errors.Is(err, tokens.ErrNotToken) {
		t.Errorf("EOA: %v", err)
	}
	if _, err := svc.Info(t.Context(), "eip155:8453", fixture.EthUSDC); !errors.Is(err, tokens.ErrUnsupportedChain) {
		t.Errorf("Base: %v", err)
	}
}

func TestHoldersEthereumPagesAcrossBlockscout(t *testing.T) {
	svc, _ := fixture.Service(t)
	ctx := t.Context()
	first, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 48, "")
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Items) != 48 || first.NextCursor == nil || first.Index != nil || first.Source != "blockscout" || *first.HolderCount != 9485913 {
		t.Fatalf("first page %d items, cursor %v", len(first.Items), first.NextCursor)
	}
	if *first.Items[0].Label != "Sky: PSM" || *first.Items[0].IsContract {
		t.Errorf("first holder %+v", first.Items[0])
	}
	second, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 5, *first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	// two from Blockscout's first page, three from its second
	if len(second.Items) != 5 || second.NextCursor == nil {
		t.Fatalf("second page %d items", len(second.Items))
	}
	all, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 53, "")
	if err != nil {
		t.Fatal(err)
	}
	for i, h := range second.Items {
		if h.Address != all.Items[48+i].Address {
			t.Errorf("item %d of the second page is %s, want %s", i, h.Address, all.Items[48+i].Address)
		}
	}
	if _, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 5, "!!"); !errors.Is(err, tokens.ErrBadCursor) {
		t.Errorf("bad cursor: %v", err)
	}
}

func TestHoldersMonadFromTheIndex(t *testing.T) {
	svc, f := fixture.Service(t)
	ctx := t.Context()
	h, err := svc.Holders(ctx, fixture.Monad, fixture.MonUSDC, 3, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.Source != "sona_index" || h.Index == nil || h.Index.State != "ready" || *h.Index.Mode != "activity" || len(h.Items) != 3 || h.NextCursor == nil {
		t.Fatalf("holders %+v", h)
	}
	// activity mode takes the count from GeckoTerminal
	if h.HolderCount == nil || *h.HolderCount != 76440 || !slices.Contains(h.Coverage, "token_holders_from_activity") {
		t.Errorf("count %v coverage %v", h.HolderCount, h.Coverage)
	}
	next, err := svc.Holders(ctx, fixture.Monad, fixture.MonUSDC, 3, *h.NextCursor)
	if err != nil || len(next.Items) == 0 || next.Items[0].Address == h.Items[2].Address {
		t.Fatalf("next page %+v %v", next.Items, err)
	}
	if _, err := svc.Holders(ctx, fixture.Monad, fixture.MonUSDC, 3, eth(t, svc)); !errors.Is(err, tokens.ErrBadCursor) {
		t.Errorf("an Ethereum cursor on Monad: %v", err)
	}

	queued, err := svc.Holders(ctx, fixture.Monad, fixture.MonWMON, 10, "")
	if err != nil || queued.Index.State != "queued" || len(queued.Items) != 0 || !slices.Contains(queued.Coverage, "token_index_partial") {
		t.Errorf("a new token %+v %v", queued, err)
	}
	f.IndexFull = true
	full, err := svc.Holders(ctx, fixture.Monad, fixture.MonWMON, 10, "")
	if err != nil || full.Index.State != "unavailable" || !slices.Contains(full.Coverage, "token_index_unavailable") {
		t.Errorf("a full index %+v %v", full, err)
	}
}

func eth(t *testing.T, svc *tokens.Service) string {
	h, err := svc.Holders(t.Context(), fixture.Ethereum, fixture.EthUSDC, 1, "")
	if err != nil {
		t.Fatal(err)
	}
	return *h.NextCursor
}

func TestTransfersEthereumWindow(t *testing.T) {
	svc, f := fixture.Service(t)
	tr, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 5)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Window == nil || tr.Window.FromBlock != 26180000 || tr.Window.ToBlock != 26180999 || tr.Window.FromTime != "2026-10-13T00:31:47Z" || tr.Window.ToTime != "2026-10-13T03:51:59Z" {
		t.Fatalf("window %+v", tr.Window)
	}
	if len(tr.Items) != 5 || tr.Source != "hypersync" || len(tr.Coverage) != 0 {
		t.Fatalf("transfers %+v", tr)
	}
	for i := 1; i < len(tr.Items); i++ {
		a, _ := new(big.Int).SetString(tr.Items[i-1].Amount.Raw, 10)
		b, _ := new(big.Int).SetString(tr.Items[i].Amount.Raw, 10)
		if b.Cmp(a) > 0 {
			t.Fatal("not largest first")
		}
	}
	calls := f.HyperCalls
	if _, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 100); err != nil || f.HyperCalls != calls {
		t.Errorf("a second call read HyperSync again: %v", err)
	}
	f.HyperDown = true
	var ue *sources.UnavailableError
	if _, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthUSDC, 5); !errors.As(err, &ue) {
		t.Errorf("HyperSync down: %v", err)
	}
}

func TestTransfersServeOlderWindowWhenHyperSyncFails(t *testing.T) {
	svc, f := fixture.ShortTransferCache(t)
	fresh, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 5)
	if err != nil || len(fresh.Items) != 5 || slices.Contains(fresh.Coverage, "token_transfer_window_older") {
		t.Fatalf("fresh %+v %v", fresh, err)
	}
	time.Sleep(40 * time.Millisecond)
	f.HyperDown = true
	old, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 5)
	if err != nil {
		t.Fatalf("an older window exists, got %v", err)
	}
	if len(old.Items) != 5 || !slices.Contains(old.Coverage, "token_transfer_window_older") || *old.Window != *fresh.Window || old.Items[0] != fresh.Items[0] {
		t.Errorf("older %+v, fresh %+v", old, fresh)
	}
	var ue *sources.UnavailableError
	if _, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthUSDC, 5); !errors.As(err, &ue) {
		t.Errorf("no copy of USDC: %v", err)
	}
}

func TestTransfersIncompleteAnswerIsNotData(t *testing.T) {
	svc, f := fixture.Service(t)
	f.HyperNoTimes = true
	var ue *sources.UnavailableError
	if _, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 5); !errors.As(err, &ue) {
		t.Fatalf("logs without block times: %v", err)
	}
	f.HyperNoTimes = false
	tr, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 5)
	if err != nil || len(tr.Items) != 5 {
		t.Errorf("the failure was cached: %+v %v", tr, err)
	}
}

func TestTransfersMonadIndex(t *testing.T) {
	svc, _ := fixture.Service(t)
	tr, err := svc.Transfers(t.Context(), fixture.Monad, fixture.MonUSDC, 4)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Source != "sona_index" || tr.Window == nil || tr.Window.FromBlock != 113000000 || tr.Window.ToBlock != 113000039 || len(tr.Items) != 4 ||
		tr.Window.FromTime != "2026-10-13T09:02:33Z" || tr.Window.ToTime != "2026-10-13T09:02:45Z" {
		t.Fatalf("transfers %+v window %+v", tr, tr.Window)
	}
	queued, err := svc.Transfers(t.Context(), fixture.Monad, fixture.MonWMON, 4)
	if err != nil || queued.Window != nil || queued.Index.State != "queued" {
		t.Errorf("a new token %+v %v", queued, err)
	}
}

func TestHolderMapEthereum(t *testing.T) {
	svc, f := fixture.Service(t)
	ctx := t.Context()
	m, err := svc.HolderMap(ctx, fixture.Ethereum, fixture.EthUNI, 100)
	if err != nil {
		t.Fatal(err)
	}
	if m.Kind != "holder_map" || len(m.Nodes) != 100 || m.Links.Status != "building" || m.Links.BuiltAt != nil || len(m.Links.Items) != 0 || m.Links.WindowDays != 30 ||
		len(m.Clusters) != 0 || !slices.Equal(m.Coverage, []string{"token_holders_ranked_by_provider"}) {
		t.Fatalf("first answer %+v", m)
	}
	// the answer never waits for HyperSync
	if f.MapLogs.Calls != 0 {
		t.Fatalf("%d HyperSync calls before the build", f.MapLogs.Calls)
	}
	timelock, dead := m.Nodes[0], m.Nodes[1]
	if timelock.Kind != "contract" || *timelock.Label != "Uniswap V2: UNI Timelock" || timelock.Balance != "262247996.305835021033106178" || *timelock.Share != 0.2622479963 ||
		dead.Kind != "wallet" || dead.Address != "0x000000000000000000000000000000000000dead" || timelock.Cluster != nil {
		t.Errorf("nodes %+v %+v", timelock, dead)
	}

	f.BuildMaps(ctx)
	m, err = svc.HolderMap(ctx, fixture.Ethereum, fixture.EthUNI, 100)
	if err != nil {
		t.Fatal(err)
	}
	if m.Links.Status != "ready" || *m.Links.BuiltAt != "2026-10-13T18:00:00Z" || m.Links.WindowDays != 30 || len(m.Links.Items) != 6 || f.MapLogs.Calls != 3 {
		t.Fatalf("links %+v after %d pages", m.Links, f.MapLogs.Calls)
	}
	top := m.Links.Items[0]
	if top.From != "0x28c6c06298d514db089934071355e5743bf21d60" || top.To != "0xf977814e90da44bfa03b6295a0616a897441acec" || top.Transfers != 1 || top.Amount != "4324434.389" {
		t.Errorf("largest link %+v", top)
	}
	// every linked node is in a cluster, every cluster has two or more nodes and its share is the sum of theirs
	linked := map[string]bool{}
	for _, l := range m.Links.Items {
		linked[l.From], linked[l.To] = true, true
		if l.From >= l.To {
			t.Errorf("link %+v is not in address order", l)
		}
	}
	sizes, shares := map[int]int{}, map[int]float64{}
	for _, n := range m.Nodes {
		if (n.Cluster != nil) != linked[n.Address] {
			t.Errorf("node %s cluster %v linked %v", n.Address, n.Cluster, linked[n.Address])
		}
		if n.Cluster != nil {
			sizes[*n.Cluster]++
			shares[*n.Cluster] += *n.Share
		}
	}
	if len(m.Clusters) != 3 || m.Clusters[0].ID != 1 || m.Clusters[0].Size != 3 || m.Clusters[0].Share != 0.06152204057 {
		t.Fatalf("clusters %+v", m.Clusters)
	}
	for i, c := range m.Clusters {
		if c.Size < 2 || sizes[c.ID] != c.Size || math.Abs(shares[c.ID]-c.Share) > 1e-9 || i > 0 && c.Share > m.Clusters[i-1].Share {
			t.Errorf("cluster %+v: %d nodes, share %v", c, sizes[c.ID], shares[c.ID])
		}
	}

	// a map of the 50 largest is its own build, over its own nodes
	small, err := svc.HolderMap(ctx, fixture.Ethereum, fixture.EthUNI, 50)
	if err != nil || small.Links.Status != "building" || len(small.Nodes) != 50 {
		t.Fatalf("50 nodes %+v %v", small.Links, err)
	}
	f.BuildMaps(ctx)
	small, _ = svc.HolderMap(ctx, fixture.Ethereum, fixture.EthUNI, 50)
	if small.Links.Status != "ready" || len(small.Links.Items) != 2 || len(small.Clusters) != 1 || len(f.MapLogs.Queries[3].Topics1) != 50 {
		t.Errorf("50 nodes %+v %+v", small.Links, small.Clusters)
	}
}

func TestHolderMapEthereumLinksFail(t *testing.T) {
	svc, f := fixture.Service(t)
	ctx := t.Context()
	f.MapLogs.SetDown(true)
	if _, err := svc.HolderMap(ctx, fixture.Ethereum, fixture.EthUNI, 100); err != nil {
		t.Fatal(err)
	}
	f.BuildMaps(ctx)
	m, err := svc.HolderMap(ctx, fixture.Ethereum, fixture.EthUNI, 100)
	if err != nil || m.Links.Status != "unavailable" || len(m.Nodes) != 100 || !slices.Equal(m.Coverage, []string{"token_holders_ranked_by_provider", "token_holder_map_links_failed"}) {
		t.Fatalf("after a failed build %+v %v", m, err)
	}
	if _, err := svc.HolderMap(ctx, fixture.Ethereum, fixture.EthEOA, 100); !errors.Is(err, tokens.ErrNotToken) {
		t.Errorf("an EOA: %v", err)
	}
}

func TestHolderMapMonad(t *testing.T) {
	svc, f := fixture.Service(t)
	ctx := t.Context()
	// USDC follows recent activity, so its list is partial and the map shows none of it
	m, err := svc.HolderMap(ctx, fixture.Monad, fixture.MonUSDC, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nodes) != 0 || m.Links.Status != "unavailable" || len(m.Clusters) != 0 ||
		!slices.Equal(m.Coverage, []string{"token_holder_map_index_incomplete", "token_holder_map_links_ethereum_only"}) {
		t.Fatalf("activity mode %+v", m)
	}
	f.IndexMode = "full"
	m, err = svc.HolderMap(ctx, fixture.Monad, fixture.MonUSDC, 50)
	if err != nil || len(m.Nodes) == 0 || m.Links.Status != "unavailable" || !slices.Equal(m.Coverage, []string{"token_balances_from_transfers", "token_holder_map_links_ethereum_only"}) {
		t.Fatalf("full mode %+v %v", m, err)
	}
	if n := m.Nodes[0]; n.Kind != "unknown" || n.Label != nil || n.Cluster != nil || n.Share == nil {
		t.Errorf("node %+v", n)
	}
	f.IndexState = "indexing"
	if m, _ := svc.HolderMap(ctx, fixture.Monad, fixture.MonUSDC, 50); len(m.Nodes) != 0 || !slices.Contains(m.Coverage, "token_holder_map_index_incomplete") {
		t.Errorf("indexing %+v", m)
	}
	f.IndexFull = true
	if m, _ := svc.HolderMap(ctx, fixture.Monad, fixture.MonWMON, 50); len(m.Nodes) != 0 || !slices.Contains(m.Coverage, "token_index_unavailable") {
		t.Errorf("a full index %+v", m)
	}
	if f.MapLogs.Calls != 0 || f.Maps.Stats().Queued != 0 {
		t.Error("a Monad map queued a HyperSync read")
	}
}

// arcNative is the recorded eth_getBalance of each holder on Arc, cut to USDC's 6 decimals
func arcNative(t *testing.T) map[string]*big.Int {
	t.Helper()
	var (
		req []struct {
			ID     int      `json:"id"`
			Params []string `json:"params"`
		}
		resp []struct {
			ID     int    `json:"id"`
			Result string `json:"result"`
		}
	)
	if err := errors.Join(json.Unmarshal(testkit.Read(t, "rpc/getbalance.arc_usdc.request.json"), &req), json.Unmarshal(testkit.Read(t, "rpc/getbalance.arc_usdc.json"), &resp)); err != nil {
		t.Fatal(err)
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
	out := map[string]*big.Int{}
	for _, r := range resp {
		v, ok := new(big.Int).SetString(strings.TrimPrefix(r.Result, "0x"), 16)
		if !ok || r.ID < 0 || r.ID >= len(req) {
			t.Fatalf("answer %d is malformed", r.ID)
		}
		out[strings.ToLower(req[r.ID].Params[0])] = v.Quo(v, scale)
	}
	return out
}

func TestInfoArc(t *testing.T) {
	svc, f := fixture.WithArc(t)
	info, err := svc.Info(t.Context(), fixture.Arc, fixture.ArcUSDC)
	if err != nil {
		t.Fatal(err)
	}
	if *info.Symbol != "USDC" || info.Decimals != 6 || info.TotalSupply.Decimals != 6 {
		t.Errorf("metadata %+v", info)
	}
	// the holder figures come from GeckoTerminal's arc network, the prices fixture has no Arc price
	if info.HolderCount == nil || *info.HolderCount != 37270 || info.Top10Share == nil || *info.Top10Share != 0.397904 || info.MarketCapUSD == nil || info.LogoURL == nil {
		t.Errorf("holders %v top10 %v mcap %v logo %v", info.HolderCount, info.Top10Share, info.MarketCapUSD, info.LogoURL)
	}
	if !slices.Equal(info.Sources, []string{"rpc", "prices", "geckoterminal"}) || !slices.Equal(info.Coverage, []string{"token_price_unknown", "token_holder_count_provider"}) {
		t.Errorf("sources %v coverage %v", info.Sources, info.Coverage)
	}
	if f.EnsureCalls != 0 || f.NativeCalls != 0 {
		t.Error("token info queued the token or read native balances")
	}
}

// Every USDC balance listed is the holder's native balance, read once a page and kept, not what 20 recorded blocks add up to
func TestHoldersArcUSDCAreNativeBalances(t *testing.T) {
	svc, f := fixture.WithArc(t)
	ctx := t.Context()
	native := arcNative(t)
	p := fixture.ArcPage(t)
	_, moves := index.FeedOf(fixture.ArcChain(), fixture.ArcUSDC, 6).Moves(p.Logs, 0, p.NextBlock)
	stored := index.Deltas(moves)
	h, err := svc.Holders(ctx, fixture.Arc, fixture.ArcUSDC, 5, "")
	if err != nil {
		t.Fatal(err)
	}
	if h.Source != "sona_index" || h.Index == nil || h.Index.State != "ready" || len(h.Items) != 5 || h.NextCursor == nil || h.HolderCount == nil || *h.HolderCount != 37270 {
		t.Fatalf("holders %+v", h)
	}
	if !slices.Equal(h.Coverage, []string{"token_holders_from_activity", "token_native_coin", "token_holder_count_provider"}) {
		t.Errorf("coverage %v", h.Coverage)
	}
	next, err := svc.Holders(ctx, fixture.Arc, fixture.ArcUSDC, 5, *h.NextCursor)
	if err != nil || len(next.Items) != 5 || next.Items[0].Address == h.Items[4].Address {
		t.Fatalf("next page %+v %v", next.Items, err)
	}
	differ := 0
	for _, it := range append(h.Items, next.Items...) {
		want, ok := native[it.Address]
		if !ok || it.Balance.Raw != want.String() || it.Balance.Decimals != 6 || it.Share == nil {
			t.Errorf("%s holds %+v, its native balance is %v", it.Address, it.Balance, want)
		}
		if stored[evm.MustAddress(it.Address)].Cmp(want) != 0 {
			differ++
		}
	}
	if differ == 0 {
		t.Error("every native balance equals the stored one, so the check is untested")
	}
	if _, err := svc.Holders(ctx, fixture.Arc, fixture.ArcUSDC, 5, ""); err != nil || f.NativeCalls != 2 {
		t.Errorf("%d getEthBalance batches for two pages read three times, %v", f.NativeCalls, err)
	}
	// EURC is no native coin, it is queued like any token and nothing reads native balances for it
	eurc, err := svc.Holders(ctx, fixture.Arc, fixture.ArcEURC, 5, "")
	if err != nil || eurc.Index.State != "queued" || slices.Contains(eurc.Coverage, "token_native_coin") || f.NativeCalls != 2 {
		t.Errorf("EURC %+v %v", eurc, err)
	}
}

// Arc's USDC transfers are the system emitter's logs, plain sends included, each move once and in 6 decimals
func TestTransfersArcUSDCFromTheSystemEmitter(t *testing.T) {
	svc, _ := fixture.WithArc(t)
	tr, err := svc.Transfers(t.Context(), fixture.Arc, fixture.ArcUSDC, 100)
	if err != nil {
		t.Fatal(err)
	}
	if tr.Window == nil || tr.Window.FromBlock != 25941000 || tr.Window.ToBlock != 25941019 || tr.Window.FromTime != "2026-10-14T15:07:55Z" || tr.Window.ToTime != "2026-10-14T15:08:05Z" {
		t.Fatalf("window %+v", tr.Window)
	}
	if len(tr.Items) != 100 || !slices.Equal(tr.Coverage, []string{"token_holders_from_activity", "token_native_coin"}) {
		t.Fatalf("%d items, coverage %v", len(tr.Items), tr.Coverage)
	}
	p := fixture.ArcPage(t)
	emitted := map[string]sources.Log{}
	viaToken := map[string]bool{}
	for _, l := range p.Logs {
		switch l.Address {
		case fixture.ArcEmitter:
			emitted[l.TxHash.String()+"/"+new(big.Int).SetUint64(l.LogIndex).String()] = l
		case fixture.ArcUSDC:
			viaToken[l.TxHash.String()] = true
		}
	}
	scale := new(big.Int).Exp(big.NewInt(10), big.NewInt(12), nil)
	plain := 0
	for _, it := range tr.Items {
		l, ok := emitted[it.Tx+"/"+new(big.Int).SetUint64(it.LogIndex).String()]
		if !ok || it.Amount.Decimals != 6 || it.Amount.Raw != new(big.Int).Quo(new(big.Int).SetBytes(l.Data), scale).String() {
			t.Fatalf("item %+v is no emitter log in 6 decimals", it)
		}
		if !viaToken[it.Tx] {
			plain++
		}
	}
	if plain == 0 {
		t.Error("no plain send among the transfers")
	}
}

func TestHolderMapArcUSDC(t *testing.T) {
	svc, f := fixture.WithArc(t)
	f.IndexMode = "full"
	native := arcNative(t)
	m, err := svc.HolderMap(t.Context(), fixture.Arc, fixture.ArcUSDC, 50)
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Nodes) != len(native) || m.Links.Status != "unavailable" ||
		!slices.Equal(m.Coverage, []string{"token_balances_from_transfers", "token_native_coin", "token_holder_map_links_ethereum_only"}) {
		t.Fatalf("map %+v", m)
	}
	for _, n := range m.Nodes {
		if want := native[n.Address]; want == nil || n.Balance != tokens.FormatUnits(want, 6) || n.Kind != "unknown" {
			t.Errorf("node %+v, native balance %v", n, want)
		}
	}
}

/* A source that answers late */

// budget is a caller's deadline like sona-api's, and lateFor holds a source back past it
const (
	budget  = 800 * time.Millisecond
	lateFor = 1500 * time.Millisecond
)

func withBudget(t *testing.T) (context.Context, time.Time) {
	t.Helper()
	ctx, cancel := context.WithTimeout(t.Context(), budget)
	t.Cleanup(cancel)
	dl, _ := ctx.Deadline()
	return ctx, dl
}

// answered fails when the record left after the caller's deadline, which the caller sees as a timeout
func answered(t *testing.T, deadline time.Time, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("a late optional source failed the record: %v", err)
	}
	if left := time.Until(deadline); left < 50*time.Millisecond {
		t.Errorf("the record left %s before the deadline", left)
	}
}

func TestHoldersEthereumAnswersWithoutALateHolderCount(t *testing.T) {
	svc, f := fixture.Service(t)
	f.SetSlow("/api/v2/tokens/"+fixture.EthUSDC.String(), lateFor)
	ctx, dl := withBudget(t)
	h, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 10, "")
	answered(t, dl, err)
	if len(h.Items) != 10 || h.HolderCount != nil || !slices.Equal(h.Coverage, []string{"token_source_unavailable", "token_holders_ranked_by_provider"}) {
		t.Errorf("holders %d items, count %v, coverage %v", len(h.Items), h.HolderCount, h.Coverage)
	}
}

func TestHoldersIndexAnswersWithoutALateGeckoCount(t *testing.T) {
	svc, f := fixture.WithArc(t)
	f.SetSlow("/networks/monad/tokens/"+fixture.MonUSDC.String()+"/info", lateFor)
	f.SetSlow("/networks/arc/tokens/"+fixture.ArcUSDC.String()+"/info", lateFor)
	for _, c := range []struct {
		chain string
		token evm.Address
	}{{fixture.Monad, fixture.MonUSDC}, {fixture.Arc, fixture.ArcUSDC}} {
		ctx, dl := withBudget(t)
		h, err := svc.Holders(ctx, c.chain, c.token, 5, "")
		answered(t, dl, err)
		if len(h.Items) != 5 || h.Index == nil || h.HolderCount != nil || !slices.Contains(h.Coverage, "token_source_unavailable") || slices.Contains(h.Coverage, "token_holder_count_provider") {
			t.Errorf("%s holders %d items, count %v, coverage %v", c.chain, len(h.Items), h.HolderCount, h.Coverage)
		}
	}
}

func TestInfoAnswersWithoutLateSources(t *testing.T) {
	svc, f := fixture.Service(t)
	f.SetSlow("/networks/monad/tokens/"+fixture.MonUSDC.String()+"/info", lateFor)
	f.SetSlow("/internal/v1/chains/eip155:143/tokens/"+fixture.MonUSDC.String()+"/prices", lateFor)
	ctx, dl := withBudget(t)
	info, err := svc.Info(ctx, fixture.Monad, fixture.MonUSDC)
	answered(t, dl, err)
	// the market cap answered in time, the price and the holder figures did not
	if *info.Symbol != "USDC" || info.PriceUSD != nil || info.HolderCount != nil || info.Top10Share != nil || info.MarketCapUSD == nil {
		t.Errorf("info %+v", info)
	}
	if !slices.Equal(info.Sources, []string{"rpc", "geckoterminal"}) || !slices.Equal(info.Coverage, []string{"token_source_unavailable"}) {
		t.Errorf("sources %v coverage %v", info.Sources, info.Coverage)
	}

	f.SetSlow("/api/v2/tokens/"+fixture.EthUSDC.String()+"/holders", lateFor)
	ctx, dl = withBudget(t)
	eth, err := svc.Info(ctx, fixture.Ethereum, fixture.EthUSDC)
	answered(t, dl, err)
	if eth.HolderCount == nil || eth.Top10Share != nil || !slices.Contains(eth.Coverage, "token_source_unavailable") {
		t.Errorf("Ethereum info count %v top10 %v coverage %v", eth.HolderCount, eth.Top10Share, eth.Coverage)
	}
}

// the read a late source started keeps going, so the next caller gets the full record from the cache
func TestALateSourceFillsTheCacheForTheNextCaller(t *testing.T) {
	svc, f := fixture.Service(t)
	f.SetSlow("/api/v2/tokens/"+fixture.EthUSDC.String(), budget)
	ctx, dl := withBudget(t)
	first, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 10, "")
	answered(t, dl, err)
	if first.HolderCount != nil {
		t.Fatal("the late count was waited for")
	}
	time.Sleep(time.Until(dl) + 200*time.Millisecond)
	ctx, dl = withBudget(t)
	second, err := svc.Holders(ctx, fixture.Ethereum, fixture.EthUSDC, 10, "")
	answered(t, dl, err)
	if second.HolderCount == nil || *second.HolderCount != 9485913 || slices.Contains(second.Coverage, "token_source_unavailable") {
		t.Errorf("second call count %v coverage %v", second.HolderCount, second.Coverage)
	}
}

// HyperSync's visitor share is 2 requests a minute, so a new Ethereum window can take longer than any caller waits
func TestTransfersEthereumAnswerBeforeASlowWindow(t *testing.T) {
	svc, f := fixture.Service(t)
	f.HyperSlow = lateFor
	ctx, dl := withBudget(t)
	_, err := svc.Transfers(ctx, fixture.Ethereum, fixture.EthMKR, 5)
	var ue *sources.UnavailableError
	if !errors.As(err, &ue) || ue.RetryAfter <= 0 {
		t.Fatalf("a window still read answered %v, want 503 with a time to come back", err)
	}
	if left := time.Until(dl); left < 50*time.Millisecond {
		t.Errorf("the answer left %s before the deadline", left)
	}
	// the read goes on, and the next caller gets the window from the cache
	time.Sleep(lateFor)
	ctx, dl = withBudget(t)
	tr, err := svc.Transfers(ctx, fixture.Ethereum, fixture.EthMKR, 5)
	answered(t, dl, err)
	if len(tr.Items) != 5 || f.HyperCalls != 1 {
		t.Errorf("%d transfers after %d HyperSync pages", len(tr.Items), f.HyperCalls)
	}
}

func TestTransfersEthereumServeTheOlderWindowWhileANewOneIsRead(t *testing.T) {
	svc, f := fixture.ShortTransferCache(t)
	fresh, err := svc.Transfers(t.Context(), fixture.Ethereum, fixture.EthMKR, 5)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(40 * time.Millisecond)
	f.HyperSlow = lateFor
	ctx, dl := withBudget(t)
	old, err := svc.Transfers(ctx, fixture.Ethereum, fixture.EthMKR, 5)
	answered(t, dl, err)
	if !slices.Contains(old.Coverage, "token_transfer_window_older") || *old.Window != *fresh.Window {
		t.Errorf("coverage %v window %+v", old.Coverage, old.Window)
	}
}
