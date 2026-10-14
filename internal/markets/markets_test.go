package markets_test

import (
	"encoding/json/v2"
	"errors"
	"slices"
	"testing"
	"time"

	"tokens/internal/fixture"
	"tokens/internal/markets"
	"tokens/internal/sources"
	"tokens/internal/testkit"
)

func load[T any](t *testing.T, path string) T {
	t.Helper()
	var v T
	if err := json.Unmarshal(testkit.Read(t, path), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func TestTopCoins(t *testing.T) {
	s, _ := fixture.Markets(t)
	rows := load[[]struct {
		ID          string  `json:"id"`
		Price       float64 `json:"current_price"`
		Cap         float64 `json:"market_cap"`
		Change      float64 `json:"price_change_percentage_24h"`
		LastUpdated string  `json:"last_updated"`
	}](t, "coingecko/markets.json")
	got, err := s.TopCoins(t.Context(), 5)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "top_coins" || len(got.Items) != 5 || len(got.Coverage) != 0 {
		t.Fatalf("%+v", got)
	}
	for i, c := range got.Items {
		if c.Rank != i+1 || c.ID != rows[i].ID || c.PriceUSD != rows[i].Price || c.MarketCapUSD != rows[i].Cap || c.Change24hPct == nil {
			t.Errorf("item %d: %+v", i, c)
		}
		if c.LogoURL == nil || (*c.LogoURL)[:8] != "https://" {
			t.Errorf("logo of %s: %v", c.ID, c.LogoURL)
		}
	}
	if got.Items[0].Symbol != "BTC" || len(got.Items[0].Tokens) != 0 {
		t.Errorf("bitcoin: %+v", got.Items[0])
	}
	var newest time.Time
	for _, r := range rows {
		if at, _ := time.Parse(time.RFC3339, r.LastUpdated); at.After(newest) {
			newest = at
		}
	}
	if got.AsOf != newest.UTC().Format(time.RFC3339) {
		t.Errorf("as_of %s, want %s", got.AsOf, newest)
	}
}

func TestTopCoinsTokens(t *testing.T) {
	s, _ := fixture.Markets(t)
	got, err := s.TopCoins(t.Context(), 20)
	if err != nil {
		t.Fatal(err)
	}
	by := map[string]markets.TopCoin{}
	for _, c := range got.Items {
		by[c.ID] = c
	}
	usdt := by["tether"].Tokens
	if !slices.Equal(usdt, []markets.CoinToken{{Chain: "eip155:1", Address: "0xdac17f958d2ee523a2206206994597c13d831ec7"}}) {
		t.Errorf("tether: %v", usdt)
	}
	usdc := by["usd-coin"].Tokens
	want := []markets.CoinToken{
		{Chain: "eip155:1", Address: fixture.EthUSDC.String()},
		{Chain: "eip155:143", Address: fixture.MonUSDC.String()},
		{Chain: "eip155:5042", Address: fixture.ArcUSDC.String()},
	}
	if !slices.Equal(usdc, want) {
		t.Errorf("usd-coin: %v", usdc)
	}
}

func TestTopCoinsWithoutTokenMap(t *testing.T) {
	s, _ := fixture.NewMarkets(t)
	if err := s.RefreshTopCoins(t.Context()); err != nil {
		t.Fatal(err)
	}
	got, err := s.TopCoins(t.Context(), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Coverage, []string{"market_token_map_unavailable"}) {
		t.Errorf("coverage %v", got.Coverage)
	}
	for _, c := range got.Items {
		if c.Tokens == nil || len(c.Tokens) != 0 {
			t.Errorf("%s tokens %v", c.ID, c.Tokens)
		}
	}
}

func TestNothingLoadedIsUnavailable(t *testing.T) {
	s, _ := fixture.NewMarkets(t)
	var ue *sources.UnavailableError
	if _, err := s.TopCoins(t.Context(), 5); !errors.As(err, &ue) {
		t.Errorf("top coins: %v", err)
	}
	if _, err := s.ChainTVL(t.Context(), fixture.Monad, 30); !errors.As(err, &ue) {
		t.Errorf("tvl: %v", err)
	}
	if _, err := s.ChainActivity(t.Context(), fixture.Ethereum, 30); !errors.As(err, &ue) {
		t.Errorf("activity: %v", err)
	}
}

func TestCoinGeckoKeyIsSent(t *testing.T) {
	s, f := fixture.NewMarketsWithKey(t, "demo-key")
	if err := s.RefreshTopCoins(t.Context()); err != nil {
		t.Fatal(err)
	}
	if f.LastKey != "demo-key" {
		t.Errorf("key header %q", f.LastKey)
	}
	s, f = fixture.NewMarkets(t)
	if err := s.RefreshTopCoins(t.Context()); err != nil || f.LastKey != "" {
		t.Errorf("no key: %v %q", err, f.LastKey)
	}
}

func TestStaleKeepsLastCopy(t *testing.T) {
	s, f := fixture.Markets(t)
	f.SetDown(true)
	if err := s.RefreshTopCoins(t.Context()); err == nil {
		t.Fatal("a refresh against a source that is down succeeded")
	}
	got, err := s.TopCoins(t.Context(), 1)
	if err != nil || len(got.Items) != 1 || len(got.Coverage) != 0 {
		t.Fatalf("%v %+v", err, got)
	}
	f.Advance(16 * time.Minute)
	got, err = s.TopCoins(t.Context(), 1)
	if err != nil || !slices.Contains(got.Coverage, "market_top_coins_stale") {
		t.Errorf("%v %v", err, got.Coverage)
	}
	f.Advance(4 * time.Hour)
	tvl, err := s.ChainTVL(t.Context(), fixture.Monad, 30)
	if err != nil || !slices.Contains(tvl.Coverage, "chain_defi_stale") {
		t.Errorf("tvl %v %v", err, tvl.Coverage)
	}
	act, err := s.ChainActivity(t.Context(), fixture.Ethereum, 30)
	if err != nil || !slices.Contains(act.Coverage, "chain_activity_stale") {
		t.Errorf("activity %v %v", err, act.Coverage)
	}
}

func TestChainTVL(t *testing.T) {
	s, _ := fixture.Markets(t)
	pts := load[[]struct {
		Date int64   `json:"date"`
		TVL  float64 `json:"tvl"`
	}](t, "llama/tvl.monad.json")
	last := pts[len(pts)-1]
	got, err := s.ChainTVL(t.Context(), fixture.Monad, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "chain_tvl" || got.Chain != fixture.Monad || got.CurrentUSD != last.TVL || len(got.Items) != 30 {
		t.Fatalf("%+v", got)
	}
	if got.Items[29].TVL != last.TVL || got.Items[29].Day != time.Unix(last.Date, 0).UTC().Format(time.DateOnly) {
		t.Errorf("last item %+v", got.Items[29])
	}
	prev, week := pts[len(pts)-2].TVL, pts[len(pts)-8].TVL
	if got.Change1dPct == nil || *got.Change1dPct != round2((last.TVL/prev-1)*100) || got.Change7dPct == nil || *got.Change7dPct != round2((last.TVL/week-1)*100) {
		t.Errorf("changes %v %v", got.Change1dPct, got.Change7dPct)
	}
	if !slices.Equal(got.Coverage, []string{"chain_series_last_day_partial"}) {
		t.Errorf("coverage %v", got.Coverage)
	}
	all, err := s.ChainTVL(t.Context(), fixture.Monad, 0)
	if err != nil || len(all.Items) != len(pts) {
		t.Errorf("all: %v %d of %d", err, len(all.Items), len(pts))
	}
	eth, err := s.ChainTVL(t.Context(), fixture.Ethereum, 365)
	if err != nil || len(eth.Items) != 365 {
		t.Errorf("ethereum: %v %d", err, len(eth.Items))
	}
}

func round2(v float64) float64 { return float64(int64(v*100+sign(v)*0.5)) / 100 }

func sign(v float64) float64 {
	if v < 0 {
		return -1
	}
	return 1
}

func TestUnsupportedChain(t *testing.T) {
	s, _ := fixture.Markets(t)
	if _, err := s.ChainTVL(t.Context(), "eip155:10", 30); !errors.Is(err, markets.ErrUnsupportedChain) {
		t.Errorf("tvl: %v", err)
	}
	if _, err := s.ChainActivity(t.Context(), fixture.Monad, 30); !errors.Is(err, markets.ErrNoActivity) {
		t.Errorf("monad activity: %v", err)
	}
	if _, err := s.ChainActivity(t.Context(), "eip155:10", 30); !errors.Is(err, markets.ErrUnsupportedChain) {
		t.Errorf("activity: %v", err)
	}
}

func TestChainProtocols(t *testing.T) {
	s, _ := fixture.Markets(t)
	got, err := s.ChainProtocols(t.Context(), fixture.Ethereum, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Items) == 0 || len(got.Categories) == 0 {
		t.Fatalf("%+v", got)
	}
	var sum float64
	for i, p := range got.Items {
		if p.Category == "CEX" || p.Category == "Chain" || p.TVLUSD <= 0 {
			t.Errorf("row %+v", p)
		}
		if i > 0 && p.TVLUSD > got.Items[i-1].TVLUSD {
			t.Errorf("%s is out of order", p.Name)
		}
		sum += p.TVLUSD
	}
	var catSum float64
	var n int
	for _, c := range got.Categories {
		catSum += c.TVLUSD
		n += c.Protocols
	}
	if n != len(got.Items) || catSum < sum*0.999999 || catSum > sum*1.000001 {
		t.Errorf("categories hold %d protocols and %v, rows %d and %v", n, catSum, len(got.Items), sum)
	}
	// the recording holds Ethereum CEX and Chain rows, which the answer must not
	type row struct {
		Category string `json:"category"`
	}
	raw := load[[]row](t, "llama/protocols.json")
	if !slices.ContainsFunc(raw, func(r row) bool { return r.Category == "CEX" }) {
		t.Error("the recording holds no CEX row to exclude")
	}
	three, err := s.ChainProtocols(t.Context(), fixture.Ethereum, 3)
	if err != nil || len(three.Items) != 3 || three.Items[0] != got.Items[0] || len(three.Categories) != len(got.Categories) {
		t.Errorf("limit: %v %+v", err, three.Items)
	}
	if !slices.Equal(got.Coverage, []string{"chain_protocols_not_additive", "chain_protocol_change_all_chains"}) {
		t.Errorf("coverage %v", got.Coverage)
	}
	mon, err := s.ChainProtocols(t.Context(), fixture.Monad, 5)
	if err != nil || len(mon.Items) != 5 || mon.Items[0].Name != "Aave V3" {
		t.Errorf("monad: %v %+v", err, mon.Items)
	}
}

func TestChainStablecoins(t *testing.T) {
	s, _ := fixture.Markets(t)
	got, err := s.ChainStablecoins(t.Context(), fixture.Monad)
	if err != nil {
		t.Fatal(err)
	}
	if got.Kind != "chain_stablecoins" || len(got.Items) == 0 || len(got.Items) > 15 || len(got.History) == 0 || len(got.History) > 365 {
		t.Fatalf("%d items %d history", len(got.Items), len(got.History))
	}
	if got.Items[0].Symbol != "USDC" {
		t.Errorf("largest %+v", got.Items[0])
	}
	for i := 1; i < len(got.Items); i++ {
		if got.Items[i].CirculatingUSD > got.Items[i-1].CirculatingUSD {
			t.Errorf("%s is out of order", got.Items[i].Symbol)
		}
	}
	charts := load[[]struct {
		Total struct {
			USD float64 `json:"peggedUSD"`
		} `json:"totalCirculatingUSD"`
	}](t, "llama/stablecharts.monad.json")
	if got.TotalUSD != charts[len(charts)-1].Total.USD || got.History[len(got.History)-1].TotalUSD != got.TotalUSD {
		t.Errorf("total %v", got.TotalUSD)
	}
	eth, err := s.ChainStablecoins(t.Context(), fixture.Ethereum)
	if err != nil || len(eth.Items) != 15 || len(eth.History) != 365 {
		t.Errorf("ethereum: %v %d %d", err, len(eth.Items), len(eth.History))
	}
}

func TestChainDexVolume(t *testing.T) {
	s, _ := fixture.Markets(t)
	raw := load[struct {
		Total24h float64 `json:"total24h"`
		Total7d  float64 `json:"total7d"`
		Change1d float64 `json:"change_1d"`
		Chart    [][]float64
	}](t, "llama/dex.monad.json")
	got, err := s.ChainDexVolume(t.Context(), fixture.Monad, 30)
	if err != nil {
		t.Fatal(err)
	}
	if got.Total24hUSD == nil || *got.Total24hUSD != raw.Total24h || *got.Total7dUSD != raw.Total7d || *got.Change1dPct != raw.Change1d {
		t.Errorf("totals %+v", got)
	}
	if len(got.Items) != 30 || len(got.Protocols) == 0 || len(got.Protocols) > 15 {
		t.Fatalf("%d items %d protocols", len(got.Items), len(got.Protocols))
	}
	for i := 1; i < len(got.Protocols); i++ {
		if got.Protocols[i].Volume24hUSD > got.Protocols[i-1].Volume24hUSD || got.Protocols[i].Volume24hUSD <= 0 {
			t.Errorf("%s is out of order", got.Protocols[i].Name)
		}
	}
	all, err := s.ChainDexVolume(t.Context(), fixture.Monad, 365)
	if err != nil || len(all.Items) == 0 || len(all.Items) > 365 {
		t.Errorf("365: %v %d", err, len(all.Items))
	}
}

func TestChainActivity(t *testing.T) {
	s, _ := fixture.Markets(t)
	got, err := s.ChainActivity(t.Context(), fixture.Ethereum, 30)
	if err != nil {
		t.Fatal(err)
	}
	var metrics []string
	for _, sr := range got.Series {
		metrics = append(metrics, sr.Metric)
		if len(sr.Items) != 30 {
			t.Errorf("%s holds %d days", sr.Metric, len(sr.Items))
		}
	}
	if !slices.Equal(metrics, []string{"txs", "active_accounts", "new_accounts", "gas_price_gwei", "fees_eth", "utilization_pct"}) {
		t.Fatalf("metrics %v", metrics)
	}
	raw := load[struct {
		Chart []struct {
			Date  string `json:"date"`
			Value string `json:"value"`
		} `json:"chart"`
	}](t, "blockscout/stats.networkUtilization.json")
	last := raw.Chart[len(raw.Chart)-1]
	util := got.Series[5].Items
	if util[29].Day != last.Date {
		t.Errorf("last day %s, recording %s", util[29].Day, last.Date)
	}
	for _, p := range util {
		if p.Value < 30 || p.Value > 100 {
			t.Errorf("utilization %v on %s is no percentage of a full block", p.Value, p.Day)
		}
	}
	if !slices.Equal(got.Coverage, []string{"chain_series_last_day_partial"}) {
		t.Errorf("coverage %v", got.Coverage)
	}
	year, err := s.ChainActivity(t.Context(), fixture.Ethereum, 365)
	if err != nil || len(year.Series[0].Items) == 0 {
		t.Errorf("365: %v", err)
	}
}
