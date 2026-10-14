package tokens

import (
	"cmp"
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"time"

	"tokens/internal/evm"
	"tokens/internal/sources"
)

const (
	// longTailBudget is the most a search waits for GeckoTerminal; the read goes on and fills the cache for the next search
	longTailBudget = 3 * time.Second
	searchFetch    = 10 * time.Second
	indexMetaTTL   = time.Hour
	indexMetaPar   = 8
)

// Match classes, best first: a symbol that starts with the query, a name that starts with it, then a symbol or a name that holds it
const (
	matchSymbolPrefix = iota
	matchNamePrefix
	matchSymbolContains
	matchNameContains
	matchNone
)

// SearchItem is one token of a search
type SearchItem struct {
	Chain    string  `json:"chain"`
	Token    string  `json:"token"`
	Name     *string `json:"name"`
	Symbol   *string `json:"symbol"`
	Decimals int     `json:"decimals"`
	// Verified is true for the tokens on a curated list
	Verified bool    `json:"verified"`
	LogoURL  *string `json:"logo_url"`
}

// Search is the token_search record
type Search struct {
	Kind     string       `json:"kind"`
	Items    []SearchItem `json:"items"`
	Coverage []string     `json:"coverage"`
}

type candidate struct {
	item     SearchItem
	name     string
	symbol   string
	verified bool
	rank     int
	class    int
	exact    bool
	reserve  float64
}

// classify says how q, already trimmed and lowercased, matches a token
func classify(q, symbol, name, addr string) (class int, exact bool) {
	switch {
	case symbol == q:
		return matchSymbolPrefix, true
	case addr == q:
		return matchSymbolPrefix, true
	case strings.HasPrefix(symbol, q):
		return matchSymbolPrefix, false
	case strings.HasPrefix(name, q):
		return matchNamePrefix, false
	case strings.Contains(symbol, q):
		return matchSymbolContains, false
	case strings.Contains(name, q):
		return matchNameContains, false
	}
	return matchNone, false
}

func textPtr(s string) *string {
	if s == "" {
		return nil
	}
	return &s
}

// Search finds tokens on every served chain whose symbol or name matches q (trimmed and lowercased by the caller).
// Tokens on a curated list come first and are verified. The rest follow by the liquidity GeckoTerminal reports,
// and are never verified, so a copy of a name never outranks the real token.
func (s *Service) Search(ctx context.Context, q string, limit int) (Search, error) {
	cands := map[listKey]*candidate{}
	for _, c := range s.d.Lists.Snapshot() {
		if class, exact := classify(q, c.symbolLower, c.nameLower, c.Token.String()); class != matchNone {
			cands[listKey{c.Chain, c.Token}] = &candidate{
				item: SearchItem{
					Chain: c.Chain, Token: c.Token.String(), Name: textPtr(c.Name), Symbol: textPtr(c.Symbol),
					Decimals: int(c.Decimals), Verified: true, LogoURL: textPtr(c.Logo),
				},
				name: c.nameLower, symbol: c.symbolLower, verified: true, rank: c.Rank, class: class, exact: exact,
			}
		}
	}

	type held struct {
		addrs []evm.Address
		metas []Meta
	}
	var (
		wg         sync.WaitGroup
		mu         sync.Mutex
		longTail   bool
		hitsByChan = map[string][]sources.GeckoHit{}
		indexed    = map[string]held{}
	)
	for _, cfg := range s.d.Config.Chains {
		if cfg.GeckoNetwork == "" {
			continue
		}
		wg.Go(func() {
			hits, err := s.geckoSearch(ctx, cfg.GeckoNetwork, q)
			mu.Lock()
			defer mu.Unlock()
			if err != nil {
				longTail = true
				return
			}
			hitsByChan[cfg.ID] = hits
		})
	}
	for _, cfg := range s.d.Config.Chains {
		if _, ok := s.d.Indexes[cfg.ID]; !ok || s.d.Store == nil {
			continue
		}
		wg.Go(func() {
			addrs, metas := s.indexedMetas(ctx, cfg.ID)
			mu.Lock()
			defer mu.Unlock()
			indexed[cfg.ID] = held{addrs, metas}
		})
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return Search{}, err
	}

	for chain, h := range indexed {
		for i, m := range h.metas {
			addr := h.addrs[i]
			key := listKey{chain, addr}
			if _, ok := cands[key]; ok {
				continue
			}
			var name, symbol string
			if m.Name != nil {
				name = *m.Name
			}
			if m.Symbol != nil {
				symbol = *m.Symbol
			}
			sl, nl := strings.ToLower(symbol), strings.ToLower(name)
			if class, exact := classify(q, sl, nl, addr.String()); class != matchNone {
				cands[key] = &candidate{
					item: SearchItem{Chain: chain, Token: addr.String(), Name: m.Name, Symbol: m.Symbol, Decimals: int(m.Decimals)},
					name: nl, symbol: sl, class: class, exact: exact,
				}
			}
		}
	}
	for chain, hits := range hitsByChan {
		for _, h := range hits {
			key := listKey{chain, h.Address}
			if c, ok := cands[key]; ok {
				c.reserve += h.ReserveUSD
				if c.item.LogoURL == nil {
					c.item.LogoURL = textPtr(h.ImageURL)
				}
				continue
			}
			sl, nl := strings.ToLower(h.Symbol), strings.ToLower(h.Name)
			if class, exact := classify(q, sl, nl, h.Address.String()); class != matchNone {
				cands[key] = &candidate{
					item: SearchItem{
						Chain: chain, Token: h.Address.String(), Name: textPtr(h.Name), Symbol: textPtr(h.Symbol),
						Decimals: int(h.Decimals), LogoURL: textPtr(h.ImageURL),
					},
					name: nl, symbol: sl, class: class, exact: exact, reserve: h.ReserveUSD,
				}
			}
		}
	}

	ranked := make([]*candidate, 0, len(cands))
	for _, c := range cands {
		ranked = append(ranked, c)
	}
	slices.SortFunc(ranked, compareCandidates)
	out := Search{Kind: "token_search", Items: make([]SearchItem, 0, min(limit, len(ranked)))}
	unverified := false
	for _, c := range ranked[:min(limit, len(ranked))] {
		out.Items = append(out.Items, c.item)
		unverified = unverified || !c.verified
	}
	var notes []Note
	if !s.d.Lists.Complete() {
		notes = append(notes, CovSearchListDown)
	}
	if longTail {
		notes = append(notes, CovSearchLongTailDown)
	}
	if unverified {
		notes = append(notes, CovSearchUnverified)
	}
	out.Coverage = codes(ordered(notes)...)
	return out, nil
}

// compareCandidates puts curated tokens first, then the better match, an exact symbol, the earlier list, the deeper
// liquidity, and last the shorter symbol and the address so the order never varies
func compareCandidates(a, b *candidate) int {
	if a.verified != b.verified {
		if a.verified {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(a.class, b.class); c != 0 {
		return c
	}
	if a.exact != b.exact {
		if a.exact {
			return -1
		}
		return 1
	}
	if c := cmp.Compare(a.rank, b.rank); c != 0 {
		return c
	}
	if c := cmp.Compare(b.reserve, a.reserve); c != 0 {
		return c
	}
	if c := cmp.Compare(len(a.symbol), len(b.symbol)); c != 0 {
		return c
	}
	return cmp.Or(strings.Compare(a.item.Chain, b.item.Chain), strings.Compare(a.item.Token, b.item.Token))
}

// geckoSearch reads GeckoTerminal's pool search, kept for the search cache time. A search that GeckoTerminal does not
// answer in time or at all is an error here, and the caller still answers with the curated tokens.
func (s *Service) geckoSearch(ctx context.Context, network, q string) ([]sources.GeckoHit, error) {
	ttl := func(_ []sources.GeckoHit, err error) time.Duration {
		if err == nil || errors.Is(err, sources.ErrNotFound) {
			return s.d.Config.SearchCache
		}
		return 0
	}
	sctx, cancel := context.WithTimeout(ctx, longTailBudget)
	defer cancel()
	hits, err := s.gtSearch.Get(sctx, network+"/"+q, ttl, func(ctx context.Context) ([]sources.GeckoHit, error) {
		return s.d.Gecko.Search(ctx, network, q)
	})
	if errors.Is(err, sources.ErrNotFound) {
		return nil, nil
	}
	return hits, err
}

// indexedMetas reads name and symbol of the tokens a chain's index holds, which GeckoTerminal may not know
func (s *Service) indexedMetas(ctx context.Context, chain string) ([]evm.Address, []Meta) {
	addrs, err := s.d.Store.IndexedTokens(ctx, chain)
	if err != nil {
		return nil, nil
	}
	metas := make([]Meta, len(addrs))
	ok := make([]bool, len(addrs))
	sem := make(chan struct{}, indexMetaPar)
	var wg sync.WaitGroup
	for i, a := range addrs {
		wg.Go(func() {
			sem <- struct{}{}
			defer func() { <-sem }()
			m, err := s.idxMeta.Get(ctx, chain+"/"+a.String(), ttlOK[Meta](indexMetaTTL), func(ctx context.Context) (Meta, error) {
				return s.Meta(ctx, chain, a)
			})
			metas[i], ok[i] = m, err == nil
		})
	}
	wg.Wait()
	var outA []evm.Address
	var outM []Meta
	for i := range addrs {
		if ok[i] {
			outA, outM = append(outA, addrs[i]), append(outM, metas[i])
		}
	}
	return outA, outM
}
