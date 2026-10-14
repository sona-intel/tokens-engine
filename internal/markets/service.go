package markets

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"

	"tokens/internal/config"
	"tokens/internal/sources"
)

const (
	// staleAfter is how many refresh intervals a copy may miss before an answer says it is stale
	staleAfter = 3
	// minRetry and maxRetry bound the wait before a failed refresh is tried again
	minRetry = time.Minute
	maxRetry = 10 * time.Minute
	// keepProtocols is the most protocol rows held per chain, the route serves at most 100
	keepProtocols = 100
	// topLists is the length of the stablecoin and DEX protocol lists
	topLists = 15
	// historyDays is the stablecoin history and the DEX chart kept
	historyDays = 365
)

const (
	sourceCoinGecko  = "coingecko"
	sourceDefiLlama  = "defillama"
	sourceBlockscout = "blockscout"
)

// slot holds the last good copy of one source answer and when it was read
type slot[T any] struct {
	mu  sync.RWMutex
	v   T
	at  time.Time
	err error
}

func (s *slot[T]) set(v T, at time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.v, s.at, s.err = v, at, nil
}

// fail records why the last refresh failed and keeps the copy
func (s *slot[T]) fail(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.err = err
}

// get returns the copy, when it was read and whether there is one; err is the last refresh failure when there is no copy
func (s *slot[T]) get() (v T, at time.Time, err error, ok bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.v, s.at, s.err, !s.at.IsZero()
}

type protocolSet struct {
	rows       []Protocol
	categories []Category
}

type stableSet struct {
	items   []Stablecoin
	history []StablePoint
}

type dexSet struct {
	total24h, total7d, total30d, change1d *float64
	chart                                 []VolumePoint
	protocols                             []DexProtocol
}

type chainSlots struct {
	cfg       config.MarketChain
	tvl       slot[[]sources.TVLPoint]
	protocols slot[protocolSet]
	stable    slot[stableSet]
	dex       slot[dexSet]
	activity  slot[[]Series]
}

// StatsSource reads a daily activity line by its Blockscout stats id, Blockscout's own service or the engine's count of a chain's blocks
type StatsSource interface {
	StatsLine(ctx context.Context, id, from, to string) ([]sources.StatPoint, error)
}

// CountedSource is a stats source that counts the chain itself, CountedTo is the time of the newest block it counted, zero before any
type CountedSource interface {
	StatsSource
	CountedTo(ctx context.Context) (time.Time, error)
}

// Deps are the sources and the clock of the service
type Deps struct {
	Config    config.Markets
	CoinGecko *sources.CoinGecko
	Llama     *sources.Llama
	// Stats is the stats source of each chain with activity, by chain id
	Stats  map[string]StatsSource
	Now    func() time.Time
	Logger *slog.Logger
}

// Service keeps the market data in memory and refreshes it in the background
type Service struct {
	d         Deps
	coins     slot[[]sources.CoinMarket]
	platforms slot[map[string]map[string]string]
	chains    map[string]*chainSlots
}

// New builds the service, which serves nothing until Run or the Refresh methods have loaded a copy
func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	if d.Logger == nil {
		d.Logger = slog.Default()
	}
	s := &Service{d: d, chains: map[string]*chainSlots{}}
	for _, c := range d.Config.Chains {
		s.chains[c.ID] = &chainSlots{cfg: c}
	}
	return s
}

// Supports is whether the chain has market data
func (s *Service) Supports(chain string) bool {
	_, ok := s.chains[chain]
	return ok
}

func (s *Service) chain(id string) (*chainSlots, error) {
	c, ok := s.chains[id]
	if !ok {
		return nil, ErrUnsupportedChain
	}
	return c, nil
}

func (s *Service) stale(at time.Time, every time.Duration) bool {
	return s.d.Now().Sub(at) > staleAfter*every
}

// unavailable is the error of a source whose first read has not succeeded yet
func unavailable(source string, err error) error {
	if err == nil {
		err = errors.New("the first read has not finished")
	}
	if ue, ok := errors.AsType[*sources.UnavailableError](err); ok {
		return ue
	}
	return &sources.UnavailableError{Source: source, RetryAfter: 30 * time.Second, Err: err}
}

// Run reads every source and keeps them fresh until ctx ends
func (s *Service) Run(ctx context.Context) {
	cfg := s.d.Config
	var wg sync.WaitGroup
	start := func(name string, every time.Duration, fn func(context.Context) error) {
		wg.Go(func() { s.loop(ctx, name, every, fn) })
	}
	start("top coins", cfg.TopCoinsRefresh, s.RefreshTopCoins)
	start("coin platforms", cfg.PlatformsRefresh, s.RefreshPlatforms)
	start("chain tvl", cfg.DefiRefresh, s.RefreshTVL)
	start("chain protocols", cfg.DefiRefresh, s.RefreshProtocols)
	start("chain stablecoins", cfg.DefiRefresh, s.RefreshStablecoins)
	start("chain dex volume", cfg.DefiRefresh, s.RefreshDex)
	start("chain activity", cfg.ActivityRefresh, s.RefreshActivity)
	wg.Wait()
}

func (s *Service) loop(ctx context.Context, name string, every time.Duration, fn func(context.Context) error) {
	for {
		wait := every
		if err := fn(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			s.d.Logger.WarnContext(ctx, "market refresh failed, the last copy stays", "what", name, "error", err)
			wait = retryIn(every, err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func retryIn(every time.Duration, err error) time.Duration {
	wait := min(max(every/10, minRetry), maxRetry)
	if ue, ok := errors.AsType[*sources.UnavailableError](err); ok {
		wait = max(wait, ue.RetryAfter)
	}
	return min(wait, every)
}
