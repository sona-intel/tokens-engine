package fixture

import (
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
		Chains: []config.MarketChain{{ID: Ethereum, LlamaName: "Ethereum", Activity: true}, {ID: Monad, LlamaName: "Monad"}},
	}
}

var marketRoutes = map[string]string{
	"/cg/coins/markets":                              "coingecko/markets.json",
	"/cg/coins/list":                                 "coingecko/coins_list.json",
	"/llama/v2/historicalChainTvl/Monad":             "llama/tvl.monad.json",
	"/llama/v2/historicalChainTvl/Ethereum":          "llama/tvl.ethereum.json",
	"/llama/protocols":                               "llama/protocols.json",
	"/llama/overview/dexs/Monad":                     "llama/dex.monad.json",
	"/llama/overview/dexs/Ethereum":                  "llama/dex.ethereum.json",
	"/stable/stablecoins":                            "llama/stablecoins.json",
	"/stable/stablecoincharts/Monad":                 "llama/stablecharts.monad.json",
	"/stable/stablecoincharts/Ethereum":              "llama/stablecharts.ethereum.json",
	"/stats-service/api/v1/lines/newTxns":            "blockscout/stats.newTxns.json",
	"/stats-service/api/v1/lines/activeAccounts":     "blockscout/stats.activeAccounts.json",
	"/stats-service/api/v1/lines/newAccounts":        "blockscout/stats.newAccounts.json",
	"/stats-service/api/v1/lines/averageGasPrice":    "blockscout/stats.averageGasPrice.json",
	"/stats-service/api/v1/lines/txnsFee":            "blockscout/stats.txnsFee.json",
	"/stats-service/api/v1/lines/networkUtilization": "blockscout/stats.networkUtilization.json",
}

// NewMarkets builds a market service whose sources answer from the recordings, with nothing loaded yet
func NewMarkets(t testing.TB) (*markets.Service, *MarketFakes) {
	t.Helper()
	return NewMarketsWithKey(t, "")
}

// NewMarketsWithKey is NewMarkets with a CoinGecko demo key, which every CoinGecko request must carry
func NewMarketsWithKey(t testing.TB, key string) (*markets.Service, *MarketFakes) {
	t.Helper()
	f := &MarketFakes{now: Now, Requests: map[string]int{}, CoinKey: key}
	files := map[string][]byte{}
	for path, file := range marketRoutes {
		files[path] = testkit.Read(t, file)
	}
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
	s := markets.New(markets.Deps{
		Config:     cfg,
		CoinGecko:  sources.NewCoinGecko(srv.URL+"/cg", f.CoinKey, cfg.CoinGeckoGap).WithHTTPClient(srv.Client()),
		Llama:      sources.NewLlama(srv.URL+"/llama", srv.URL+"/stable", cfg.LlamaPerMinute).WithHTTPClient(srv.Client()),
		Blockscout: sources.NewBlockscout(srv.URL, 1000).WithHTTPClient(srv.Client()),
		Now:        f.clock,
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
