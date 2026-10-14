// Package app wires the sources, the index and the service together for main
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"tokens/internal/chainstats"
	"tokens/internal/config"
	"tokens/internal/holdermap"
	"tokens/internal/index"
	"tokens/internal/markets"
	"tokens/internal/rpc"
	"tokens/internal/server"
	"tokens/internal/sources"
	"tokens/internal/store"
	"tokens/internal/tokens"
)

// marketService is named apart so the embedded markets service does not clash with the tokens service
type marketService = markets.Service

const readyTTL = 2 * time.Second

// App is the server's view of the engine
type App struct {
	*tokens.Service
	*marketService
	cfg    config.Config
	status tokens.StatusDeps
	st     *store.Store
	lists  *tokens.Lists
	// Indexers keep the holder index of every chain whose holders come from it, in the order of the config
	Indexers []*index.Indexer
	maps     *holdermap.Builder
	// counters count the activity of the chains that have no stats service from their blocks
	counters []*chainstats.Counter

	mu      sync.Mutex
	readyAt time.Time
	ready   bool
}

var (
	_ server.Service = (*App)(nil)
	_ server.Markets = (*App)(nil)
)

type Deps struct {
	Config  config.Config
	Store   *store.Store
	Clients map[string]*rpc.Client
	Logger  *slog.Logger
}

// New builds every client from the config and registers the gauges in reg
func New(d Deps, reg prometheus.Registerer) (*App, error) {
	cfg := d.Config
	urls := map[string]string{}
	chains := map[string]tokens.Chain{}
	for _, c := range cfg.Chains {
		client, ok := d.Clients[c.ID]
		if !ok {
			return nil, fmt.Errorf("no rpc client for %s", c.ID)
		}
		urls[c.ID] = c.HyperSyncURL
		chains[c.ID] = client
	}
	hs := sources.NewHyperSync(urls, cfg.HyperSyncKey, cfg.HyperSyncPerMinute, cfg.HyperSyncInteractive)
	a := &App{cfg: cfg, st: d.Store}
	idx := map[string]tokens.Index{}
	for _, ic := range cfg.IndexChains() {
		if !hs.Enabled() {
			return nil, errors.New("the index chain needs ENVIO_HYPERSYNC_KEY")
		}
		x := index.New(ic, cfg.IndexOf(ic), d.Store, hs, d.Clients[ic.ID], d.Logger.With("chain", ic.ID))
		a.Indexers = append(a.Indexers, x)
		idx[ic.ID] = x
	}
	blockscout := sources.NewBlockscout(cfg.BlockscoutURL, cfg.BlockscoutPerSecond)
	// every stats host gets its own pacer, the chains that name no stats_url use the Blockscout of the token routes
	stats := map[string]markets.StatsSource{}
	for _, mc := range cfg.Markets.Chains {
		switch {
		case !mc.Activity:
		case mc.ActivitySource == config.ActivityBlocks:
			if !hs.Enabled() {
				return nil, fmt.Errorf("the activity of %s is counted from its blocks and needs ENVIO_HYPERSYNC_KEY", mc.ID)
			}
			ct := chainstats.New(mc, cfg.Markets.BlockStats, hs, d.Store, d.Logger.With("chain", mc.ID))
			a.counters = append(a.counters, ct)
			stats[mc.ID] = ct
		case mc.StatsURL == "":
			stats[mc.ID] = blockscout
		default:
			stats[mc.ID] = sources.NewBlockscout(mc.StatsURL, cfg.BlockscoutPerSecond)
		}
	}
	a.lists = tokens.NewLists(cfg, sources.NewTokenLists(cfg.TokenListTimeout))
	a.maps = holdermap.New(cfg.HolderMap, hs, d.Store, d.Logger)
	a.Service = tokens.New(tokens.Deps{
		Config:     cfg,
		Chains:     chains,
		Blockscout: blockscout,
		Gecko:      sources.NewGecko(cfg.GeckoURL, cfg.GeckoPerMinute),
		Prices:     sources.NewPrices(cfg.PricesURL),
		Logs:       hs,
		Indexes:    idx,
		Store:      d.Store,
		Lists:      a.lists,
		HolderMaps: a.maps,
	})
	a.marketService = markets.New(markets.Deps{
		Config:    cfg.Markets,
		CoinGecko: sources.NewCoinGecko(cfg.Markets.CoinGeckoURL, cfg.Markets.CoinGeckoKey.Reveal(), cfg.Markets.CoinGeckoGap),
		Llama:     sources.NewLlama(cfg.Markets.LlamaURL, cfg.Markets.StablecoinsURL, cfg.Markets.LlamaPerMinute),
		Stats:     stats,
		Logger:    d.Logger,
	})
	paid := func() int64 {
		var n int64
		for _, c := range d.Clients {
			n += c.PaidCalls()
		}
		return n
	}
	a.status = tokens.StatusDeps{Config: cfg, Store: d.Store, Indexers: a.Indexers, HyperSync: hs, PaidCalls: paid}
	reg.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_paid_rpc_calls_total", Help: "JSON-RPC calls the router billed to a paid upstream"},
			func() float64 { return float64(paid()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_hypersync_requests_total", Help: "HyperSync queries sent"},
			func() float64 { return float64(hs.Stats().Requests) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_hypersync_rate_limited_total", Help: "HyperSync queries answered 429"},
			func() float64 { return float64(hs.Stats().RateLimited) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "tokens_holder_map_queued", Help: "Holder maps waiting for their links to be read"},
			func() float64 { return float64(a.maps.Stats().Queued) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_holder_map_built_total", Help: "Holder map links read and stored"},
			func() float64 { return float64(a.maps.Stats().Built) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_holder_map_failed_total", Help: "Holder map link reads that failed"},
			func() float64 { return float64(a.maps.Stats().Failed) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_holder_map_hypersync_requests_total", Help: "HyperSync pages the holder map reads spent"},
			func() float64 { return float64(a.maps.Stats().Requests) }),
	)
	for _, x := range a.Indexers {
		reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{
			Name: "tokens_index_backfill_next_block", Help: "Next block of the running backfill of the chain's index, 0 when none runs",
			ConstLabels: prometheus.Labels{"chain": x.Chain()},
		}, func() float64 {
			if p := x.Current(); p != nil {
				return float64(p.NextBlock)
			}
			return 0
		}))
	}
	return a, nil
}

// Supported is whether the engine serves the chain
func (a *App) Supported(chain string) bool {
	_, ok := a.cfg.Chain(chain)
	return ok
}

func (a *App) Status(ctx context.Context) (tokens.Status, error) {
	return tokens.BuildStatus(ctx, a.status)
}

// Ready is a database ping, kept for two seconds
func (a *App) Ready(ctx context.Context) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if time.Since(a.readyAt) < readyTTL {
		return a.ready
	}
	pctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	a.ready = a.st.Ping(pctx) == nil
	a.readyAt = time.Now()
	return a.ready
}

// Run runs the indexes, the activity counts, the holder map builds and the token list refresh until ctx ends
func (a *App) Run(ctx context.Context) {
	go a.lists.Run(ctx)
	go a.marketService.Run(ctx)
	go a.maps.Run(ctx, a.Service)
	var wg sync.WaitGroup
	for _, x := range a.Indexers {
		wg.Go(func() { x.Run(ctx, a.Decimals(x.Chain())) })
	}
	for _, ct := range a.counters {
		wg.Go(func() { ct.Run(ctx) })
	}
	<-ctx.Done()
	wg.Wait()
}
