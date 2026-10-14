package tokens

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"math/big"
	"net/url"
	"strconv"
	"sync"
	"time"

	"tokens/internal/cache"
	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/index"
	"tokens/internal/rpc"
	"tokens/internal/sources"
	"tokens/internal/store"
)

const (
	notTokenTTL  = 10 * time.Minute
	notFoundTTL  = 15 * time.Minute
	headerTTL    = time.Hour
	fetchTimeout = 60 * time.Second
	cacheEntries = 4096
)

// Chain is the JSON-RPC client of one chain
type Chain interface {
	Aggregate3(ctx context.Context, calls []evm.Call3) ([]evm.Result3, error)
	HeaderByNumber(ctx context.Context, number uint64) (rpc.Header, error)
	BlockNumber(ctx context.Context) (uint64, error)
}

type Blockscout interface {
	Token(ctx context.Context, token evm.Address) (sources.BlockscoutToken, error)
	Holders(ctx context.Context, token evm.Address, page url.Values) (sources.BlockscoutHolders, error)
}

type Gecko interface {
	Holders(ctx context.Context, network string, token evm.Address) (sources.GeckoHolders, error)
	MarketCap(ctx context.Context, network string, token evm.Address) (*float64, error)
	Search(ctx context.Context, network, query string) ([]sources.GeckoHit, error)
}

type Prices interface {
	Current(ctx context.Context, chain string, token evm.Address) (*float64, error)
}

type Logs interface {
	Page(ctx context.Context, chain string, q sources.LogQuery, prio sources.Priority) (sources.Page, error)
	Height(ctx context.Context, chain string) (uint64, error)
	PausedFor() time.Duration
}

// Index is the holder index of one chain
type Index interface {
	Chain() string
	Lookup(ctx context.Context, token evm.Address) (store.Token, bool, error)
	Ensure(ctx context.Context, token evm.Address, decimals uint8) (store.Token, error)
	Head(ctx context.Context) uint64
}

// IndexStore reads what the index holds
type IndexStore interface {
	Holders(ctx context.Context, id int32, limit int, after *store.HolderCursor) ([]store.Balance, error)
	TopTransfers(ctx context.Context, id int32, fromDay string, limit int) ([]store.Transfer, error)
	IndexedTokens(ctx context.Context, chain string) ([]evm.Address, error)
}

type Deps struct {
	Config     config.Config
	Chains     map[string]Chain
	Blockscout Blockscout
	Gecko      Gecko
	Prices     Prices
	Logs       Logs
	// Indexes are the holder indexes by chain, one for every chain whose holders come from the index
	Indexes map[string]Index
	Store   IndexStore
	// Lists are the curated token lists, nil for none
	Lists *Lists
	// HolderMaps builds the links of Ethereum holder maps, nil leaves links unavailable
	HolderMaps HolderMaps
	Now        func() time.Time
}

// Meta is what the token contract itself answers
type Meta struct {
	Name        *string
	Symbol      *string
	Decimals    uint8
	TotalSupply *big.Int
}

type Service struct {
	d   Deps
	now func() time.Time

	meta      *cache.Cache[Meta]
	bsToken   *cache.Cache[sources.BlockscoutToken]
	bsHolders *cache.Cache[sources.BlockscoutHolders]
	gtHolders *cache.Cache[sources.GeckoHolders]
	gtCap     *cache.Cache[*float64]
	price     *cache.Cache[*float64]
	windows   *cache.Cache[ethWindow]
	// lastGood keeps the newest good window of each token past its freshness, for when HyperSync fails
	lastGood *cache.Cache[goodWindow]
	headers  *cache.Cache[rpc.Header]
	blockAt  *cache.Cache[uint64]
	gtSearch *cache.Cache[[]sources.GeckoHit]
	idxMeta  *cache.Cache[Meta]
	natives  *cache.Cache[[]*big.Int]

	// learned is the transfer window that fit the HyperSync budget last time, per Ethereum token
	learned sync.Map
}

func New(d Deps) *Service {
	if d.Now == nil {
		d.Now = time.Now
	}
	return &Service{
		d: d, now: d.Now,
		meta:      cache.New[Meta](cacheEntries, fetchTimeout),
		bsToken:   cache.New[sources.BlockscoutToken](cacheEntries, fetchTimeout),
		bsHolders: cache.New[sources.BlockscoutHolders](cacheEntries, fetchTimeout),
		gtHolders: cache.New[sources.GeckoHolders](cacheEntries, fetchTimeout),
		gtCap:     cache.New[*float64](cacheEntries, fetchTimeout),
		price:     cache.New[*float64](cacheEntries, fetchTimeout),
		windows:   cache.New[ethWindow](1024, 2*fetchTimeout),
		lastGood:  cache.New[goodWindow](1024, fetchTimeout),
		headers:   cache.New[rpc.Header](cacheEntries, fetchTimeout),
		blockAt:   cache.New[uint64](64, fetchTimeout),
		gtSearch:  cache.New[[]sources.GeckoHit](512, searchFetch),
		idxMeta:   cache.New[Meta](cacheEntries, fetchTimeout),
		natives:   cache.New[[]*big.Int](512, fetchTimeout),
	}
}

func (s *Service) chain(id string) (config.Chain, Chain, error) {
	cfg, ok := s.d.Config.Chain(id)
	if !ok {
		return config.Chain{}, nil, ErrUnsupportedChain
	}
	client, ok := s.d.Chains[id]
	if !ok {
		return config.Chain{}, nil, ErrUnsupportedChain
	}
	return cfg, client, nil
}

// ttlOK keeps successes for d, a not-found answer for notFoundTTL and drops failures
func ttlOK[V any](d time.Duration) cache.TTL[V] {
	return func(_ V, err error) time.Duration {
		switch {
		case err == nil:
			return d
		case errors.Is(err, sources.ErrNotFound):
			return notFoundTTL
		}
		return 0
	}
}

/* Metadata */

// Meta reads name, symbol, decimals and totalSupply in one Multicall3 call, ErrNotToken when decimals or totalSupply do not answer
func (s *Service) Meta(ctx context.Context, chain string, token evm.Address) (Meta, error) {
	_, client, err := s.chain(chain)
	if err != nil {
		return Meta{}, err
	}
	ttl := func(_ Meta, err error) time.Duration {
		switch {
		case err == nil:
			return s.d.Config.MetaCache
		case errors.Is(err, ErrNotToken):
			return notTokenTTL
		}
		return 0
	}
	return s.meta.Get(ctx, chain+"/"+token.String(), ttl, func(ctx context.Context) (Meta, error) {
		return readMeta(ctx, client, token)
	})
}

func readMeta(ctx context.Context, client Chain, token evm.Address) (Meta, error) {
	sels := [][4]byte{evm.SelName, evm.SelSymbol, evm.SelDecimals, evm.SelTotalSupply}
	calls := make([]evm.Call3, len(sels))
	for i, sel := range sels {
		calls[i] = evm.Call3{Target: token, AllowFailure: true, Data: sel[:]}
	}
	res, err := client.Aggregate3(ctx, calls)
	if err != nil {
		return Meta{}, err
	}
	return decodeMeta(res)
}

func decodeMeta(res []evm.Result3) (Meta, error) {
	if len(res) != 4 || !res[2].Success || !res[3].Success {
		return Meta{}, ErrNotToken
	}
	dec, err := evm.Decimals(res[2].Data)
	if err != nil {
		return Meta{}, ErrNotToken
	}
	supply, err := evm.Uint256(res[3].Data)
	if err != nil {
		return Meta{}, ErrNotToken
	}
	m := Meta{Decimals: dec, TotalSupply: supply}
	if res[0].Success {
		if v, err := evm.Text(res[0].Data); err == nil && v != "" {
			m.Name = &v
		}
	}
	if res[1].Success {
		if v, err := evm.Text(res[1].Data); err == nil && v != "" {
			m.Symbol = &v
		}
	}
	return m, nil
}

// Decimals is for the index pre-warm, which needs the decimals of a token before any request names it
func (s *Service) Decimals(chain string) func(context.Context, evm.Address) (uint8, error) {
	return func(ctx context.Context, token evm.Address) (uint8, error) {
		m, err := s.Meta(ctx, chain, token)
		return m.Decimals, err
	}
}

/* Shared reads */

func (s *Service) currentPrice(ctx context.Context, chain string, token evm.Address) (*float64, error) {
	return s.price.Get(ctx, chain+"/"+token.String(), ttlOK[*float64](s.d.Config.PriceCache), func(ctx context.Context) (*float64, error) {
		return s.d.Prices.Current(ctx, chain, token)
	})
}

func (s *Service) blockscoutToken(ctx context.Context, token evm.Address) (sources.BlockscoutToken, error) {
	return s.bsToken.Get(ctx, token.String(), ttlOK[sources.BlockscoutToken](s.d.Config.HoldersCache), func(ctx context.Context) (sources.BlockscoutToken, error) {
		return s.d.Blockscout.Token(ctx, token)
	})
}

func (s *Service) blockscoutHolders(ctx context.Context, token evm.Address, page url.Values) (sources.BlockscoutHolders, error) {
	return s.bsHolders.Get(ctx, token.String()+"?"+page.Encode(), ttlOK[sources.BlockscoutHolders](s.d.Config.HoldersCache), func(ctx context.Context) (sources.BlockscoutHolders, error) {
		return s.d.Blockscout.Holders(ctx, token, page)
	})
}

func (s *Service) geckoHolders(ctx context.Context, network string, token evm.Address) (sources.GeckoHolders, error) {
	return s.gtHolders.Get(ctx, network+"/"+token.String(), ttlOK[sources.GeckoHolders](s.d.Config.GeckoCache), func(ctx context.Context) (sources.GeckoHolders, error) {
		return s.d.Gecko.Holders(ctx, network, token)
	})
}

func (s *Service) geckoCap(ctx context.Context, network string, token evm.Address) (*float64, error) {
	return s.gtCap.Get(ctx, network+"/"+token.String(), ttlOK[*float64](s.d.Config.GeckoCache), func(ctx context.Context) (*float64, error) {
		return s.d.Gecko.MarketCap(ctx, network, token)
	})
}

// nativeBalances swaps the stored balances of a native coin's ERC-20 view for its native balances in the token's decimals
func (s *Service) nativeBalances(ctx context.Context, cfg config.Chain, client Chain, token evm.Address, decimals uint8, rows []store.Balance) ([]store.Balance, error) {
	if cfg.NativeToken(token) == nil || len(rows) == 0 {
		return rows, nil
	}
	f := index.FeedOf(cfg, token, decimals)
	sum := sha256.New()
	for _, r := range rows {
		sum.Write(r.Holder[:])
	}
	key := cfg.ID + "/" + token.String() + "/" + hex.EncodeToString(sum.Sum(nil))
	// gas moves the coin without a log, so a balance summed from logs or read at the last move can be off
	vals, err := s.natives.Get(ctx, key, ttlOK[[]*big.Int](s.d.Config.MetaCache), func(ctx context.Context) ([]*big.Int, error) {
		calls := make([]evm.Call3, len(rows))
		for i, r := range rows {
			calls[i] = f.BalanceCall(r.Holder)
		}
		res, err := client.Aggregate3(ctx, calls)
		if err != nil {
			return nil, err
		}
		out := make([]*big.Int, len(rows))
		for i, r := range res {
			v, ok := f.Balance(r)
			if !ok {
				return nil, fmt.Errorf("getEthBalance of %s did not answer", rows[i].Holder)
			}
			out[i] = v
		}
		return out, nil
	})
	if err != nil {
		return nil, err
	}
	out := make([]store.Balance, len(rows))
	for i, r := range rows {
		out[i] = store.Balance{Holder: r.Holder, Balance: vals[i]}
	}
	return out, nil
}

func (s *Service) header(ctx context.Context, chain string, client Chain, n uint64) (rpc.Header, error) {
	key := chain + "/" + strconv.FormatUint(n, 10)
	return s.headers.Get(ctx, key, ttlOK[rpc.Header](headerTTL), func(ctx context.Context) (rpc.Header, error) {
		return client.HeaderByNumber(ctx, n)
	})
}

/* Token info */

// Info builds the token_info record. Only the contract read is required; every other source that fails leaves its fields null
func (s *Service) Info(ctx context.Context, chain string, token evm.Address) (Info, error) {
	cfg, _, err := s.chain(chain)
	if err != nil {
		return Info{}, err
	}
	meta, err := s.Meta(ctx, chain, token)
	if err != nil {
		return Info{}, err
	}
	out := Info{
		Kind: "token_info", Chain: chain, Token: token.String(), Name: meta.Name, Symbol: meta.Symbol,
		Decimals: int(meta.Decimals), TotalSupply: NewAmount(meta.TotalSupply, meta.Decimals), Sources: []string{SourceRPC},
	}

	var (
		wg                   sync.WaitGroup
		price, mcap          *float64
		priceErr, holdersErr error
		mcapErr              error
		count                *int64
		top10                *float64
		indexCount           *int64
		providerLogo         *string
	)
	wg.Go(func() { price, priceErr = s.currentPrice(ctx, chain, token) })
	if cfg.Holders == config.HoldersBlockscout {
		wg.Go(func() {
			t, err := s.blockscoutToken(ctx, token)
			if errors.Is(err, sources.ErrNotFound) {
				err = nil
			}
			mcapErr = err
			if err == nil {
				mcap, count, providerLogo = t.CirculatingMarketCap, t.HoldersCount, t.IconURL
			}
		})
		wg.Go(func() {
			page, err := s.blockscoutHolders(ctx, token, nil)
			if errors.Is(err, sources.ErrNotFound) {
				err = nil
			}
			holdersErr = err
			if err == nil {
				top10 = topShare(page.Items, meta.TotalSupply, 10)
			}
		})
	} else {
		wg.Go(func() {
			h, err := s.geckoHolders(ctx, cfg.GeckoNetwork, token)
			if errors.Is(err, sources.ErrNotFound) {
				err = nil
			}
			holdersErr = err
			count, top10, providerLogo = h.Count, h.Top10Share, h.ImageURL
		})
		wg.Go(func() {
			v, err := s.geckoCap(ctx, cfg.GeckoNetwork, token)
			if errors.Is(err, sources.ErrNotFound) {
				err = nil
			}
			mcap, mcapErr = v, err
		})
		wg.Go(func() {
			if t, ok, err := s.d.Indexes[chain].Lookup(ctx, token); err == nil && ok && t.State == store.StateReady && t.Mode == store.ModeFull {
				indexCount = &t.Holders
			}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Info{}, err
	}

	out.LogoURL = providerLogo
	if logo := s.d.Lists.Logo(chain, token); logo != "" {
		out.LogoURL = &logo
	}
	var notes []Note
	if priceErr == nil {
		out.Sources = append(out.Sources, SourcePrices)
		out.PriceUSD = price
		if price != nil {
			out.FDVUSD = times(meta.TotalSupply, meta.Decimals, *price)
		} else {
			notes = append(notes, CovNoPrice)
		}
	}
	provider := SourceBlockscout
	if cfg.Holders == config.HoldersIndex {
		provider = SourceGecko
	}
	if holdersErr == nil || mcapErr == nil {
		out.Sources = append(out.Sources, provider)
	}
	if mcapErr == nil {
		out.MarketCapUSD = mcap
	}
	// Blockscout gives the count with the market cap, GeckoTerminal with the distribution
	countOK := holdersErr == nil
	if cfg.Holders == config.HoldersBlockscout {
		countOK = mcapErr == nil
	}
	if countOK {
		out.HolderCount = count
	}
	if holdersErr == nil {
		out.Top10Share = top10
	}
	if out.HolderCount == nil && indexCount != nil {
		out.HolderCount = indexCount
		out.Sources = append(out.Sources, SourceIndex)
	}
	if out.MarketCapUSD != nil && out.FDVUSD != nil && *out.MarketCapUSD > *out.FDVUSD*1.01 {
		notes = append(notes, CovMultichainCap)
	}
	if cfg.Holders == config.HoldersIndex && holdersErr == nil && (out.HolderCount != nil || out.Top10Share != nil) {
		notes = append(notes, CovGeckoHolders)
	}
	if priceErr != nil || holdersErr != nil || mcapErr != nil {
		notes = append(notes, CovSourceDown)
	}
	out.Coverage = codes(ordered(notes)...)
	return out, nil
}

// topShare is the part of the supply that the n largest holders of the page hold
func topShare(items []sources.BlockscoutHolder, supply *big.Int, n int) *float64 {
	if len(items) == 0 {
		return nil
	}
	sum := new(big.Int)
	for _, it := range items[:min(n, len(items))] {
		sum.Add(sum, it.Value)
	}
	return ratio(sum, supply)
}

// ordered sorts notes into the order of AllNotes and drops repeats
func ordered(ns []Note) []Note {
	out := make([]Note, 0, len(ns))
	for _, n := range AllNotes {
		for _, m := range ns {
			if m.Code == n.Code {
				out = append(out, n)
				break
			}
		}
	}
	return out
}
