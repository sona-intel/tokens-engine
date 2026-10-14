package tokens

import (
	"context"
	"errors"
	"slices"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/index"
	"tokens/internal/rpc"
	"tokens/internal/sources"
	"tokens/internal/store"
)

// MaxTransfers is the most transfers a window keeps, the limit of the route
const MaxTransfers = 100

// pauseMargin is the time a read takes after the HyperSync pause ends
const pauseMargin = 5 * time.Second

// learnedTTL is how long a shorter window that a busy token needed is reused before the default is tried again
const learnedTTL = 6 * time.Hour

type learnedWindow struct {
	blocks uint64
	at     time.Time
}

// goodWindow is a window that was served, with the block times its answer gave
type goodWindow struct {
	w      ethWindow
	window Window
}

// olderKeep is how long a good window is kept past its freshness for a failing source
const olderKeep = 6 * time.Hour

// ethWindow is the largest transfers of one token over the recent blocks, as fetched from HyperSync
type ethWindow struct {
	From, To  uint64
	Transfers []store.Transfer
	Cut       bool
	ReadAt    time.Time
}

// Transfers returns the largest transfers of a token over a recent window
func (s *Service) Transfers(ctx context.Context, chain string, token evm.Address, limit int) (Transfers, error) {
	cfg, client, err := s.chain(chain)
	if err != nil {
		return Transfers{}, err
	}
	meta, err := s.Meta(ctx, chain, token)
	if err != nil {
		return Transfers{}, err
	}
	out := Transfers{Kind: "token_transfers", Chain: chain, Token: token.String(), Items: []Transfer{}}
	if cfg.Holders == config.HoldersBlockscout {
		err = s.hyperSyncWindow(ctx, &out, cfg, client, token, meta, limit)
	} else {
		err = s.indexWindow(ctx, &out, cfg, client, token, meta, limit)
	}
	if err != nil {
		return Transfers{}, err
	}
	return out, nil
}

func items(ts []store.Transfer, decimals uint8, limit int) []Transfer {
	out := make([]Transfer, 0, min(len(ts), limit))
	for _, t := range ts[:min(len(ts), limit)] {
		out = append(out, Transfer{
			Tx: t.Tx.String(), LogIndex: t.LogIndex, Block: t.Block, Time: stamp(t.Time),
			From: t.From.String(), To: t.To.String(), Amount: NewAmount(t.Amount, decimals),
		})
	}
	return out
}

func (s *Service) window(ctx context.Context, chain string, client Chain, from, to uint64) (*Window, error) {
	a, err := s.header(ctx, chain, client, from)
	if err != nil {
		return nil, err
	}
	b, err := s.header(ctx, chain, client, to)
	if err != nil {
		return nil, err
	}
	return &Window{FromBlock: from, ToBlock: to, FromTime: stamp(a.Time), ToTime: stamp(b.Time)}, nil
}

/* Ethereum: a window read from HyperSync */

func (s *Service) hyperSyncWindow(ctx context.Context, out *Transfers, cfg config.Chain, client Chain, token evm.Address, meta Meta, limit int) error {
	out.Source = SourceHyperSync
	ttl := func(w ethWindow, err error) time.Duration {
		if err != nil {
			return 0
		}
		if len(w.Transfers) == 0 && w.Cut {
			// a short window of a busy token that shows nothing is more likely a bad read than an idle token
			return time.Minute
		}
		return s.d.Config.TransfersCache
	}
	key := cfg.ID + "/" + token.String()
	fetch := func(ctx context.Context) (ethWindow, error) { return s.fetchWindow(ctx, cfg, token) }
	older := func() (goodWindow, bool) {
		g, _, ok := s.lastGood.Peek(key)
		return g, ok && s.now().Sub(g.w.ReadAt) <= olderKeep+s.d.Config.TransfersCache
	}
	serve := func(g goodWindow, notes ...Note) error {
		out.Window = &g.window
		out.Items = items(g.w.Transfers, meta.Decimals, limit)
		if g.w.Cut {
			notes = append([]Note{CovWindowCut}, notes...)
		}
		out.Coverage = codes(notes...)
		return nil
	}
	if _, _, ok := s.windows.Peek(key); !ok {
		// the quota is spent until the pause ends: start the read for the next caller, then answer with the last good
		// window, or tell this caller when to come back
		if pause := s.d.Logs.PausedFor(); pause > 0 {
			if dl, ok := ctx.Deadline(); ok && time.Until(dl) < pause+pauseMargin {
				go func() { _, _ = s.windows.Get(context.WithoutCancel(ctx), key, ttl, fetch) }()
				if g, ok := older(); ok {
					return serve(g, CovWindowOlder)
				}
				return &sources.UnavailableError{Source: SourceHyperSync, RetryAfter: pause + pauseMargin, Err: errors.New("the HyperSync quota is spent until the next window")}
			}
		}
	}
	w, err := s.windows.Get(ctx, key, ttl, fetch)
	if err != nil {
		if g, ok := older(); ok {
			return serve(g, CovWindowOlder)
		}
		return err
	}
	g := goodWindow{w: w}
	if prev, _, ok := s.lastGood.Peek(key); ok && prev.w.ReadAt.Equal(w.ReadAt) {
		g.window = prev.window
	} else {
		win, err := s.window(ctx, cfg.ID, client, w.From, w.To)
		if err != nil {
			return err
		}
		g.window = *win
		s.lastGood.Set(key, g, olderKeep+s.d.Config.TransfersCache)
	}
	return serve(g)
}

// fetchWindow reads [head-W+1, head] in at most TransferPageBudget pages, and shortens W for a token too busy for that
func (s *Service) fetchWindow(ctx context.Context, cfg config.Chain, token evm.Address) (ethWindow, error) {
	head, err := s.d.Logs.Height(ctx, cfg.ID)
	if err != nil {
		return ethWindow{}, &sources.UnavailableError{Source: "hypersync", RetryAfter: 5 * time.Second, Err: err}
	}
	size := s.d.Config.TransferWindowBlocks
	cut := false
	if v, ok := s.learned.Load(cfg.ID + "/" + token.String()); ok {
		if l := v.(learnedWindow); s.now().Sub(l.at) < learnedTTL {
			size, cut = l.blocks, true
		}
	}
	end := head + 1
	budget := s.d.Config.TransferPageBudget
	// without a learned window, one page first shows how busy the token is
	attempt := budget
	if !cut {
		attempt = 1
	}
	for {
		from := end - min(size, end)
		logs, next, pages, err := s.scan(ctx, cfg.ID, token, from, end, min(attempt, budget))
		if err != nil {
			return ethWindow{}, err
		}
		budget -= pages
		if next >= end || budget <= 0 {
			if next < end {
				// out of pages: report the part that was read, its end is where the reading stopped
				end = next
				cut = true
			}
			for _, l := range logs {
				if l.Time == 0 {
					// a log without its block time means the answer was incomplete: an empty window would look like data
					return ethWindow{}, &sources.UnavailableError{Source: SourceHyperSync, RetryAfter: 5 * time.Second, Err: errors.New("hypersync answered logs without block times")}
				}
			}
			return ethWindow{From: from, To: end - 1, Transfers: largest(logs, token), Cut: cut, ReadAt: s.now()}, nil
		}
		// too busy: shrink the window to what the pages left can read, judged by the blocks one page covered
		perPage := max((next-from)/uint64(max(pages, 1)), 1)
		size = max(min(perPage*uint64(budget), size/2), s.d.Config.TransferMinBlocks)
		cut, attempt = true, budget
		s.learned.Store(cfg.ID+"/"+token.String(), learnedWindow{blocks: size, at: s.now()})
	}
}

// scan reads [from, to) in at most budget pages and returns the logs, the first block not read and the pages used
func (s *Service) scan(ctx context.Context, chain string, token evm.Address, from, to uint64, budget int) ([]sources.Log, uint64, int, error) {
	var logs []sources.Log
	next, pages := from, 0
	for next < to && pages < budget {
		page, err := s.d.Logs.Page(ctx, chain, sources.LogQuery{From: next, To: to, Addresses: []evm.Address{token}, Topics0: index.Topics(false), Blocks: true}, sources.Interactive)
		if err != nil {
			return nil, 0, 0, err
		}
		pages++
		logs = append(logs, page.Logs...)
		if page.NextBlock <= next {
			return nil, 0, 0, errors.New("hypersync made no progress")
		}
		next = min(page.NextBlock, to)
	}
	return logs, next, pages, nil
}

// largest keeps the MaxTransfers largest transfers of the logs
func largest(logs []sources.Log, token evm.Address) []store.Transfer {
	var out []store.Transfer
	for _, l := range logs {
		if l.Address != token || l.Time == 0 {
			continue
		}
		m, ok := index.Decode(l, false)
		if !ok || m.Amount.Sign() == 0 {
			continue
		}
		out = append(out, store.Transfer{
			Block: l.Block, LogIndex: l.LogIndex, Tx: l.TxHash, Time: time.Unix(int64(l.Time), 0).UTC(),
			From: m.From, To: m.To, Amount: m.Amount,
		})
	}
	slices.SortFunc(out, index.CompareTransfers)
	return out[:min(len(out), MaxTransfers)]
}

/* Index chains: the index keeps the largest transfers of each day */

func (s *Service) indexWindow(ctx context.Context, out *Transfers, cfg config.Chain, client Chain, token evm.Address, meta Meta, limit int) error {
	out.Source = SourceIndex
	t, state, err := s.ensureIndexed(ctx, cfg.ID, token, meta.Decimals)
	if err != nil {
		return err
	}
	out.Index = state
	var notes []Note
	switch state.State {
	case IndexUnavailable:
		notes = append(notes, CovIndexFull)
	case IndexQueued, IndexIndexing:
		notes = append(notes, CovIndexPartial)
	}
	if t == nil || state.IndexedBlock == nil {
		out.Coverage = codes(notes...)
		return nil
	}
	if t.Mode == store.ModeActivity {
		notes = append(notes, CovFromActivity)
	}
	if cfg.NativeToken(token) != nil {
		notes = append(notes, CovNativeCoin)
	}
	cutoffDay := index.CutoffDay(s.now(), s.d.Config.Index.WindowDays)
	rows, err := s.d.Store.TopTransfers(ctx, t.ID, cutoffDay, limit)
	if err != nil {
		return err
	}
	out.Items = items(rows, meta.Decimals, limit)
	cutoff, _ := time.Parse(time.DateOnly, cutoffDay)
	from := min(t.StartBlock, *state.IndexedBlock)
	start, err := s.header(ctx, cfg.ID, client, from)
	if err != nil {
		return err
	}
	// an index that starts before the window looks up the window's first block
	if start.Time.Before(cutoff) {
		first, err := s.firstBlockAt(ctx, cfg.ID, client, cutoff, *state.IndexedBlock)
		if err != nil {
			return err
		}
		from = min(max(first, t.StartBlock), *state.IndexedBlock)
	}
	if out.Window, err = s.window(ctx, cfg.ID, client, from, *state.IndexedBlock); err != nil {
		return err
	}
	out.Coverage = codes(ordered(notes)...)
	return nil
}

// firstBlockAt is the first block at or after t, found with an interpolation search over block times below hi
func (s *Service) firstBlockAt(ctx context.Context, chain string, client Chain, t time.Time, hi uint64) (uint64, error) {
	key := chain + "/" + t.UTC().Format(time.RFC3339)
	return s.blockAt.Get(ctx, key, ttlOK[uint64](24*time.Hour), func(ctx context.Context) (uint64, error) {
		return searchBlock(ctx, func(ctx context.Context, n uint64) (rpc.Header, error) { return s.header(ctx, chain, client, n) }, t, hi)
	})
}

func searchBlock(ctx context.Context, header func(context.Context, uint64) (rpc.Header, error), t time.Time, hi uint64) (uint64, error) {
	top, err := header(ctx, hi)
	if err != nil {
		return 0, err
	}
	if !top.Time.After(t) {
		return hi, nil
	}
	lo := uint64(1)
	bottom, err := header(ctx, lo)
	if err != nil {
		return 0, err
	}
	if !bottom.Time.Before(t) {
		return lo, nil
	}
	// invariant: block lo is before t and block hi is at or after t
	for i := 0; hi-lo > 1; i++ {
		var mid uint64
		span := top.Time.Sub(bottom.Time)
		if i < 6 && span > 0 {
			frac := float64(t.Sub(bottom.Time)) / float64(span)
			mid = lo + uint64(frac*float64(hi-lo))
			mid = min(max(mid, lo+1), hi-1)
		} else {
			mid = lo + (hi-lo)/2
		}
		h, err := header(ctx, mid)
		if err != nil {
			return 0, err
		}
		if h.Time.Before(t) {
			lo, bottom = mid, h
		} else {
			hi, top = mid, h
		}
	}
	return hi, nil
}
