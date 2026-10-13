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

	"tokens/internal/config"
	"tokens/internal/index"
	"tokens/internal/rpc"
	"tokens/internal/server"
	"tokens/internal/sources"
	"tokens/internal/store"
	"tokens/internal/tokens"
)

const readyTTL = 2 * time.Second

// App is the server's view of the engine
type App struct {
	*tokens.Service
	cfg     config.Config
	status  tokens.StatusDeps
	st      *store.Store
	lists   *tokens.Lists
	Indexer *index.Indexer

	mu      sync.Mutex
	readyAt time.Time
	ready   bool
}

var _ server.Service = (*App)(nil)

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
	if ic, ok := cfg.IndexChain(); ok {
		if !hs.Enabled() {
			return nil, errors.New("the index chain needs ENVIO_HYPERSYNC_KEY")
		}
		a.Indexer = index.New(ic, cfg.Index, d.Store, hs, d.Clients[ic.ID], d.Logger)
	}
	var idx tokens.Index
	if a.Indexer != nil {
		idx = a.Indexer
	}
	a.lists = tokens.NewLists(cfg, sources.NewTokenLists(cfg.TokenListTimeout))
	a.Service = tokens.New(tokens.Deps{
		Config:     cfg,
		Chains:     chains,
		Blockscout: sources.NewBlockscout(cfg.BlockscoutURL, cfg.BlockscoutPerSecond),
		Gecko:      sources.NewGecko(cfg.GeckoURL, cfg.GeckoNetwork, cfg.GeckoPerMinute),
		Prices:     sources.NewPrices(cfg.PricesURL),
		Logs:       hs,
		Index:      idx,
		Store:      d.Store,
		Lists:      a.lists,
	})
	paid := func() int64 {
		var n int64
		for _, c := range d.Clients {
			n += c.PaidCalls()
		}
		return n
	}
	a.status = tokens.StatusDeps{Config: cfg, Store: d.Store, Indexer: a.Indexer, HyperSync: hs, PaidCalls: paid}
	reg.MustRegister(
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_paid_rpc_calls_total", Help: "JSON-RPC calls the router billed to a paid upstream"},
			func() float64 { return float64(paid()) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_hypersync_requests_total", Help: "HyperSync queries sent"},
			func() float64 { return float64(hs.Stats().Requests) }),
		prometheus.NewCounterFunc(prometheus.CounterOpts{Name: "tokens_hypersync_rate_limited_total", Help: "HyperSync queries answered 429"},
			func() float64 { return float64(hs.Stats().RateLimited) }),
		prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: "tokens_index_backfill_next_block", Help: "Next block of the running backfill, 0 when none runs"},
			func() float64 {
				if a.Indexer == nil {
					return 0
				}
				if p := a.Indexer.Current(); p != nil {
					return float64(p.NextBlock)
				}
				return 0
			}),
	)
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

// Run runs the index and the token list refresh until ctx ends
func (a *App) Run(ctx context.Context) {
	go a.lists.Run(ctx)
	if a.Indexer == nil {
		<-ctx.Done()
		return
	}
	a.Indexer.Run(ctx, a.Decimals(a.Indexer.Chain()))
}
