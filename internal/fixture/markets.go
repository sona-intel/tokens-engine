package fixture

import (
	"context"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/markets"
	"tokens/internal/sources"
	"tokens/internal/testkit"
)

// MarketFakes are the switches of the market sources and the clock of the market service
type MarketFakes struct {
	mu        sync.Mutex
	now       time.Time
	Down      bool
	CoinKey   string
	LastKey   string
	Requests  map[string]int
	BadStatus map[string]int
}

// Advance moves the clock of the market service
func (f *MarketFakes) Advance(d time.Duration) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.now = f.now.Add(d)
}

// SetDown makes every market source answer 502
func (f *MarketFakes) SetDown(v bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.Down = v
}

func (f *MarketFakes) clock() time.Time {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.now
}

// MarketsConfig is the markets part of the engine's config with the pacing of the sources lifted for tests
func MarketsConfig() config.Markets {
	return config.Markets{
		CoinGeckoGap: time.Millisecond, TopCoinsRefresh: 5 * time.Minute, PlatformsRefresh: 24 * time.Hour,
		LlamaPerMinute: 60000, DefiRefresh: time.Hour, ActivityRefresh: time.Hour,
		Chains: []config.MarketChain{{ID: Ethereum, LlamaName: "Ethereum", Activity: true, FeesMetric: "fees_eth"}, {ID: Monad, LlamaName: "Monad"}},
	}
}

var marketRoutes = map[string]string{
	"/cg/coins/markets":                                  "coingecko/markets.json",
	"/cg/coins/list":                                     "coingecko/coins_list.json",
	"/llama/v2/historicalChainTvl/Monad":                 "llama/tvl.monad.json",
	"/llama/v2/historicalChainTvl/Ethereum":              "llama/tvl.ethereum.json",
	"/llama/protocols":                                   "llama/protocols.json",
	"/llama/overview/dexs/Monad":                         "llama/dex.monad.json",
	"/llama/overview/dexs/Ethereum":                      "llama/dex.ethereum.json",
	"/stable/stablecoins":                                "llama/stablecoins.json",
	"/stable/stablecoincharts/Monad":                     "llama/stablecharts.monad.json",
	"/stable/stablecoincharts/Ethereum":                  "llama/stablecharts.ethereum.json",
	"/stats-service/api/v1/lines/newTxns":                "blockscout/stats.newTxns.json",
	"/stats-service/api/v1/lines/activeAccounts":         "blockscout/stats.activeAccounts.json",
	"/stats-service/api/v1/lines/newAccounts":            "blockscout/stats.newAccounts.json",
	"/stats-service/api/v1/lines/averageGasPrice":        "blockscout/stats.averageGasPrice.json",
	"/stats-service/api/v1/lines/txnsFee":                "blockscout/stats.txnsFee.json",
	"/stats-service/api/v1/lines/networkUtilization":     "blockscout/stats.networkUtilization.json",
	"/arc/stats-service/api/v1/lines/newTxns":            "blockscout/stats.arc.newTxns.json",
	"/arc/stats-service/api/v1/lines/activeAccounts":     "blockscout/stats.arc.activeAccounts.json",
	"/arc/stats-service/api/v1/lines/newAccounts":        "blockscout/stats.arc.newAccounts.json",
	"/arc/stats-service/api/v1/lines/averageGasPrice":    "blockscout/stats.arc.averageGasPrice.json",
	"/arc/stats-service/api/v1/lines/txnsFee":            "blockscout/stats.arc.txnsFee.json",
	"/arc/stats-service/api/v1/lines/networkUtilization": "blockscout/stats.arc.networkUtilization.json",
}

// NewMarkets builds a market service whose sources answer from the recordings, with nothing loaded yet
func NewMarkets(t testing.TB) (*markets.Service, *MarketFakes) {
	t.Helper()
	return NewMarketsWithKey(t, "")
}

// NewMarketsWithKey is NewMarkets with a CoinGecko demo key, which every CoinGecko request must carry
func NewMarketsWithKey(t testing.TB, key string) (*markets.Service, *MarketFakes) {
	t.Helper()
	return newMarkets(t, key, false, nil, nil)
}

// NewMarketsWithMonadCount is NewMarkets with Monad's activity counted from its blocks by count, as the engine's config has it
func NewMarketsWithMonadCount(t testing.TB, count markets.StatsSource) (*markets.Service, *MarketFakes) {
	t.Helper()
	return newMarkets(t, "", false, count, nil)
}

// MonadLines answers the lines a count of Monad's blocks keeps, the same value on the day before Now and on Now's day, counted to 10 minutes before Now
type MonadLines map[string]float64

func (m MonadLines) StatsLine(_ context.Context, id, _, _ string) ([]sources.StatPoint, error) {
	v, ok := m[id]
	if !ok {
		return nil, markets.ErrNoStatsLine
	}
	return []sources.StatPoint{{Day: Now.AddDate(0, 0, -1).Format(time.DateOnly), Value: v}, {Day: Now.Format(time.DateOnly), Value: v}}, nil
}

func (MonadLines) CountedTo(context.Context) (time.Time, error) {
	return Now.Add(-10 * time.Minute), nil
}

// NewMarketsWithArc is NewMarkets with Arc added as a chain that has activity only: refresh its activity alone, no DeFi recording covers Arc
func NewMarketsWithArc(t testing.TB) (*markets.Service, *MarketFakes) {
	t.Helper()
	return newMarkets(t, "", true, nil, nil)
}

// NewMarketsWithArcStablecoins is NewMarketsWithArc with the stablecoin list of Arc's coins alone and chart as Arc's stablecoin chart, for the DeFi stablecoin refresh only
func NewMarketsWithArcStablecoins(t testing.TB, chart []byte) (*markets.Service, *MarketFakes) {
	t.Helper()
	return newMarkets(t, "", true, nil, map[string][]byte{
		"/stable/stablecoins":          testkit.Read(t, "llama/stablecoins.arc.json"),
		"/stable/stablecoincharts/Arc": chart,
	})
}

func newMarkets(t testing.TB, key string, arc bool, monad markets.StatsSource, replace map[string][]byte) (*markets.Service, *MarketFakes) {
	t.Helper()
	f := &MarketFakes{now: Now, Requests: map[string]int{}, CoinKey: key}
	files := map[string][]byte{}
	for path, file := range marketRoutes {
		files[path] = testkit.Read(t, file)
	}
	maps.Copy(files, replace)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		down, status := f.Down, f.BadStatus[r.URL.Path]
		f.Requests[r.URL.Path]++
		if strings.HasPrefix(r.URL.Path, "/cg/") {
			f.LastKey = r.Header.Get("x-cg-demo-api-key")
		}
		f.mu.Unlock()
		body, ok := files[r.URL.Path]
		switch {
		case down:
			w.WriteHeader(http.StatusBadGateway)
		case status != 0:
			w.WriteHeader(status)
		case !ok:
			w.WriteHeader(http.StatusNotFound)
		default:
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(body)
		}
	}))
	t.Cleanup(srv.Close)
	cfg := MarketsConfig()
	if arc {
		cfg.Chains = append(cfg.Chains, config.MarketChain{ID: Arc, LlamaName: "Arc", Activity: true, StatsURL: "https://explorer.arc.io", FeesMetric: "fees_usdc"})
	}
	stats := map[string]markets.StatsSource{
		Ethereum: sources.NewBlockscout(srv.URL, 1000).WithHTTPClient(srv.Client()),
		Arc:      sources.NewBlockscout(srv.URL+"/arc", 1000).WithHTTPClient(srv.Client()),
	}
	if monad != nil {
		for i, c := range cfg.Chains {
			if c.ID == Monad {
				cfg.Chains[i].Activity, cfg.Chains[i].ActivitySource = true, config.ActivityBlocks
			}
		}
		stats[Monad] = monad
	}
	s := markets.New(markets.Deps{
		Config:    cfg,
		CoinGecko: sources.NewCoinGecko(srv.URL+"/cg", f.CoinKey, cfg.CoinGeckoGap).WithHTTPClient(srv.Client()),
		Llama:     sources.NewLlama(srv.URL+"/llama", srv.URL+"/stable", cfg.LlamaPerMinute).WithHTTPClient(srv.Client()),
		Stats:     stats,
		Now:       f.clock,
	})
	return s, f
}

// Markets is NewMarkets with every source read once
func Markets(t testing.TB) (*markets.Service, *MarketFakes) {
	t.Helper()
	s, f := NewMarkets(t)
	RefreshMarkets(t, s)
	return s, f
}

// RefreshMarkets reads every market source once and fails the test on an error
func RefreshMarkets(t testing.TB, s *markets.Service) {
	t.Helper()
	ctx := t.Context()
	for name, err := range map[string]error{
		"top coins": s.RefreshTopCoins(ctx), "platforms": s.RefreshPlatforms(ctx), "tvl": s.RefreshTVL(ctx),
		"protocols": s.RefreshProtocols(ctx), "stablecoins": s.RefreshStablecoins(ctx), "dex": s.RefreshDex(ctx), "activity": s.RefreshActivity(ctx),
	} {
		if err != nil {
			t.Fatalf("refresh %s: %v", name, err)
		}
	}
}
