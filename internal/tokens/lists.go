package tokens

import (
	"context"
	"log/slog"
	"strconv"
	"strings"
	"sync"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/sources"
)

// retryFailedLists is how soon a list that has never loaded is read again
const retryFailedLists = 5 * time.Minute

// ListFetcher reads one token list
type ListFetcher interface {
	Fetch(ctx context.Context, url string) ([]sources.ListToken, error)
}

// Curated is a token on a curated list
type Curated struct {
	Chain    string
	Token    evm.Address
	Name     string
	Symbol   string
	Decimals uint8
	// Logo is an https URL or empty
	Logo string
	// Rank is the position of the first list that holds the token, 0 for the first list
	Rank int

	nameLower, symbolLower string
}

type listKey struct {
	chain string
	token evm.Address
}

// Lists holds the curated token lists of the config, loaded on start and again every refresh.
// A list that fails to load keeps the copy it had.
type Lists struct {
	specs   []config.TokenList
	fetch   ListFetcher
	chains  map[uint64]string
	refresh time.Duration
	timeout time.Duration

	mu     sync.RWMutex
	copies [][]sources.ListToken
	loaded []bool
	byKey  map[listKey]*Curated
	all    []*Curated
}

// NewLists reads the lists of the config for the chains it serves
func NewLists(cfg config.Config, fetch ListFetcher) *Lists {
	chains := map[uint64]string{}
	for _, c := range cfg.Chains {
		if id, ok := strings.CutPrefix(c.ID, "eip155:"); ok {
			if n, err := strconv.ParseUint(id, 10, 64); err == nil {
				chains[n] = c.ID
			}
		}
	}
	return &Lists{
		specs: cfg.TokenLists, fetch: fetch, chains: chains, refresh: cfg.TokenListRefresh, timeout: cfg.TokenListTimeout,
		copies: make([][]sources.ListToken, len(cfg.TokenLists)), loaded: make([]bool, len(cfg.TokenLists)),
		byKey: map[listKey]*Curated{},
	}
}

// Load reads every list once, in parallel, and returns how many failed
func (l *Lists) Load(ctx context.Context) int {
	if l == nil {
		return 0
	}
	type result struct {
		tokens []sources.ListToken
		err    error
	}
	results := make([]result, len(l.specs))
	var wg sync.WaitGroup
	for i, spec := range l.specs {
		wg.Go(func() {
			lctx, cancel := context.WithTimeout(ctx, l.timeout)
			defer cancel()
			results[i].tokens, results[i].err = l.fetch.Fetch(lctx, spec.URL)
		})
	}
	wg.Wait()
	failed := 0
	l.mu.Lock()
	defer l.mu.Unlock()
	for i, r := range results {
		if r.err != nil {
			failed++
			slog.Warn("token list not loaded, keeping the last copy", "list", l.specs[i].Name, "kept", l.loaded[i], "error", r.err)
			continue
		}
		l.copies[i], l.loaded[i] = r.tokens, true
		slog.Info("token list loaded", "list", l.specs[i].Name, "tokens", len(r.tokens))
	}
	l.rebuildLocked()
	return failed
}

// Run loads the lists and reads them again every refresh, sooner while one has never loaded, until ctx ends
func (l *Lists) Run(ctx context.Context) {
	if l == nil {
		return
	}
	for {
		failed := l.Load(ctx)
		wait := l.refresh
		if failed > 0 && !l.Complete() {
			wait = min(wait, retryFailedLists)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Complete is whether every list has loaded at least once
func (l *Lists) Complete() bool {
	if l == nil {
		return true
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	for _, ok := range l.loaded {
		if !ok {
			return false
		}
	}
	return true
}

// rebuildLocked merges the copies: the first list that holds a token gives its name and rank, a later one only a missing logo
func (l *Lists) rebuildLocked() {
	byKey := map[listKey]*Curated{}
	var all []*Curated
	for rank, tokens := range l.copies {
		for _, t := range tokens {
			chain, ok := l.chains[t.ChainID]
			if !ok {
				continue
			}
			k := listKey{chain, t.Address}
			if c, ok := byKey[k]; ok {
				if c.Logo == "" {
					c.Logo = t.LogoURL
				}
				continue
			}
			c := &Curated{
				Chain: chain, Token: t.Address, Name: t.Name, Symbol: t.Symbol, Decimals: t.Decimals, Logo: t.LogoURL, Rank: rank,
				nameLower: strings.ToLower(t.Name), symbolLower: strings.ToLower(t.Symbol),
			}
			byKey[k] = c
			all = append(all, c)
		}
	}
	l.byKey, l.all = byKey, all
}

// Logo is the logo of a token on a list, empty when no list gives one
func (l *Lists) Logo(chain string, token evm.Address) string {
	if l == nil {
		return ""
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	if c, ok := l.byKey[listKey{chain, token}]; ok {
		return c.Logo
	}
	return ""
}

// Get returns the entry of a token
func (l *Lists) Get(chain string, token evm.Address) (*Curated, bool) {
	if l == nil {
		return nil, false
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	c, ok := l.byKey[listKey{chain, token}]
	return c, ok
}

// Snapshot returns every curated token. The entries are not changed after a rebuild replaces them.
func (l *Lists) Snapshot() []*Curated {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	defer l.mu.RUnlock()
	return l.all
}
