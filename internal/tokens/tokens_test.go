package tokens_test

import (
	"errors"
	"math"
	"math/big"
	"slices"
	"testing"

	"tokens/internal/fixture"
	"tokens/internal/sources"
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
