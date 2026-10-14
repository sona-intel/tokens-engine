package index

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/big"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/sources"
	"tokens/internal/store"
)

const (
	heightTTL  = 5 * time.Second
	idlePoll   = 15 * time.Second
	tailPages  = 3
	pruneEvery = time.Hour
	// overPages is how many pages in a row must project past the budget, so one busy day does not end a full backfill
	overPages = 2
	// headWait is the longest a visitor's request waits for the archive height before it answers without it
	headWait = 1500 * time.Millisecond
)

// Logs is the HyperSync client
type Logs interface {
	Page(ctx context.Context, chain string, q sources.LogQuery, prio sources.Priority) (sources.Page, error)
	Height(ctx context.Context, chain string) (uint64, error)
}

// Chain is the JSON-RPC client of the indexed chain
type Chain interface {
	Aggregate3(ctx context.Context, calls []evm.Call3) ([]evm.Result3, error)
}

// Progress describes the backfill that runs now
type Progress struct {
	Token     evm.Address
	Mode      string
	NextBlock uint64
	Head      uint64
	Requests  int
	// Projected is the HyperSync requests a full backfill would take in all, from the pages read so far
	Projected int
	Started   time.Time
}

type Indexer struct {
	chain config.Chain
	cfg   config.Index
	st    *store.Store
	logs  Logs
	rpc   Chain
	log   *slog.Logger
	now   func() time.Time

	wake    chan struct{}
	current atomic.Pointer[Progress]

	mu       sync.Mutex
	head     uint64
	headAt   time.Time
	lastTail time.Time
	tailErr  string
	pruned   time.Time
}

func New(chain config.Chain, cfg config.Index, st *store.Store, logs Logs, client Chain, log *slog.Logger) *Indexer {
	return &Indexer{chain: chain, cfg: cfg, st: st, logs: logs, rpc: client, log: log, now: time.Now, wake: make(chan struct{}, 1)}
}

func (x *Indexer) Chain() string { return x.chain.ID }

// Lookup returns the index row of a token, false when it is not indexed
func (x *Indexer) Lookup(ctx context.Context, token evm.Address) (store.Token, bool, error) {
	return x.st.GetToken(ctx, x.chain.ID, token)
}

// Ensure queues a token for its backfill unless it is indexed, store.ErrFull when the index is full
func (x *Indexer) Ensure(ctx context.Context, token evm.Address, decimals uint8) (store.Token, error) {
	t, created, err := x.st.EnsureToken(ctx, x.chain.ID, token, decimals, x.cfg.MaxTokens)
	if err != nil {
		return store.Token{}, err
	}
	if created {
		x.log.Info("token queued for indexing", "token", token)
		select {
		case x.wake <- struct{}{}:
		default:
		}
	}
	return t, nil
}

// Current is the running backfill, nil when none runs
func (x *Indexer) Current() *Progress { return x.current.Load() }

// Head is the last archive height HyperSync reported, it asks again when the copy is old
func (x *Indexer) Head(ctx context.Context) uint64 {
	// a visitor's request never waits long for the height: the first call to HyperSync can take 10 seconds to connect
	ctx, cancel := context.WithTimeout(ctx, headWait)
	defer cancel()
	h, err := x.height(ctx)
	if err != nil {
		x.mu.Lock()
		defer x.mu.Unlock()
		return x.head
	}
	return h
}

// Tail is when the tail last finished a cycle and the error of the last failed one
func (x *Indexer) Tail() (time.Time, string) {
	x.mu.Lock()
	defer x.mu.Unlock()
	return x.lastTail, x.tailErr
}

func (x *Indexer) height(ctx context.Context) (uint64, error) {
	x.mu.Lock()
	if x.head > 0 && x.now().Sub(x.headAt) < heightTTL {
		h := x.head
		x.mu.Unlock()
		return h, nil
	}
	x.mu.Unlock()
	h, err := x.logs.Height(ctx, x.chain.ID)
	if err != nil {
		return 0, err
	}
	x.mu.Lock()
	defer x.mu.Unlock()
	if h > x.head {
		x.head = h
	}
	x.headAt = x.now()
	return x.head, nil
}

// safeHead is the exclusive end of what may be indexed now, lag blocks behind the archive height
func (x *Indexer) safeHead(ctx context.Context) (uint64, error) {
	h, err := x.height(ctx)
	if err != nil {
		return 0, err
	}
	if h < x.cfg.LagBlocks {
		return 0, nil
	}
	return h - x.cfg.LagBlocks + 1, nil
}

// Run queues the pre-warm tokens, then runs the backfill worker and the tail until ctx ends
func (x *Indexer) Run(ctx context.Context, decimals func(context.Context, evm.Address) (uint8, error)) {
	var wg sync.WaitGroup
	// the first connection to HyperSync is slow, so open it before a visitor needs the height
	wg.Go(func() { _, _ = x.height(ctx) })
	wg.Go(func() { x.prewarm(ctx, decimals) })
	wg.Go(func() { x.worker(ctx) })
	wg.Go(func() { x.tailLoop(ctx) })
	wg.Wait()
}

func (x *Indexer) prewarm(ctx context.Context, decimals func(context.Context, evm.Address) (uint8, error)) {
	for _, token := range x.cfg.Prewarm {
		for wait := 5 * time.Second; ; wait = min(2*wait, 5*time.Minute) {
			d, err := decimals(ctx, token)
			if err == nil {
				_, err = x.Ensure(ctx, token, d)
			}
			if err == nil || errors.Is(err, store.ErrFull) {
				break
			}
			if ctx.Err() != nil {
				return
			}
			x.log.Warn("pre-warm token not queued yet", "token", token, "error", err)
			if sleep(ctx, wait) != nil {
				return
			}
		}
	}
}

func sleep(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

/* Backfill */

func (x *Indexer) worker(ctx context.Context) {
	for ctx.Err() == nil {
		job, ok, err := x.st.NextJob(ctx, x.chain.ID, x.cfg.RetryFailedAfter)
		if err != nil {
			x.log.Error("read the next backfill", "error", err)
		}
		if !ok || err != nil {
			select {
			case <-ctx.Done():
				return
			case <-x.wake:
			case <-time.After(idlePoll):
			}
			continue
		}
		if err := x.backfill(ctx, job); err != nil && ctx.Err() == nil {
			x.log.Error("backfill failed", "token", job.Token, "error", err)
			if err := x.st.MarkFailed(ctx, job.ID, err.Error()); err != nil {
				x.log.Error("mark backfill failed", "token", job.Token, "error", err)
			}
		}
		x.current.Store(nil)
	}
}

func (x *Indexer) backfill(ctx context.Context, t store.Token) error {
	mode, next := t.Mode, t.NextBlock
	if t.State == store.StateQueued {
		mode, next = store.ModeFull, 0
	}
	if err := x.st.StartBackfill(ctx, t.ID, mode, next, t.State == store.StateQueued); err != nil {
		return err
	}
	started := x.now()
	if t.StartedAt != nil {
		started = *t.StartedAt
	}
	f := x.feed(t)
	requests := t.Requests
	// span is the blocks the latest full page covered, over counts the pages in a row whose span projects past the budget
	var (
		span uint64
		over int
	)
	x.log.Info("backfill started", "token", t.Token, "mode", mode, "from_block", next)
	for {
		head, err := x.safeHead(ctx)
		if err != nil {
			return fmt.Errorf("archive height: %w", err)
		}
		progress := &Progress{Token: t.Token, Mode: mode, NextBlock: next, Head: head, Requests: requests, Started: started}
		if mode == store.ModeFull {
			progress.Projected = project(requests, next, head, span)
		}
		x.current.Store(progress)
		if next >= head {
			if err := x.st.MarkReady(ctx, t.ID); err != nil {
				return err
			}
			x.log.Info("backfill done", "token", t.Token, "mode", mode, "next_block", next, "requests", requests, "seconds", int(x.now().Sub(started).Seconds()))
			return nil
		}
		if mode == store.ModeFull && over >= overPages {
			start := head - min(head, x.cfg.ActivityWindowBlocks)
			x.log.Info("full backfill over budget, following recent activity", "token", t.Token, "projected_requests", progress.Projected,
				"budget", x.cfg.BackfillBudget, "spent", requests, "from_block", start)
			if err := x.st.StartBackfill(ctx, t.ID, store.ModeActivity, start, true); err != nil {
				return err
			}
			mode, next, span, over = store.ModeActivity, start, 0, 0
			continue
		}
		page, err := x.logs.Page(ctx, x.chain.ID, sources.LogQuery{From: next, To: head, Addresses: []evm.Address{f.Source}, Topics0: Topics(f.Wrapped), Blocks: true}, sources.Background)
		if err != nil {
			return err
		}
		requests++
		end := min(max(page.NextBlock, next), head)
		if end == next {
			return errors.New("hypersync made no progress")
		}
		if err := x.apply(ctx, t, mode, f, page.Logs, next, end, 1); err != nil {
			return err
		}
		if end < head && len(page.Logs) > 0 {
			span = end - next
			if project(requests, end, head, span) > x.cfg.BackfillBudget {
				over++
			} else {
				over = 0
			}
		}
		next = end
	}
}

// project is the requests a full backfill takes in all, when what is left goes at span blocks a page
func project(spent int, next, head, span uint64) int {
	if span == 0 || next >= head {
		return spent
	}
	left := (head - next + span - 1) / span
	return spent + int(min(left, 1<<30))
}

// feed is where the index reads the moves of a token of this chain
func (x *Indexer) feed(t store.Token) Feed { return FeedOf(x.chain, t.Token, t.Decimals) }

// apply turns the logs of [from, end) into one batch for one token and commits it
func (x *Indexer) apply(ctx context.Context, t store.Token, mode string, f Feed, logs []sources.Log, from, end uint64, requests int) error {
	kept, moves := f.Moves(logs, from, end)
	b := store.Batch{
		TokenID: t.ID, NextBlock: end, FromBlock: from, Requests: requests,
		CutoffDay: CutoffDay(x.now(), x.cfg.WindowDays), TopPerDay: x.cfg.TopPerDay,
	}
	b.Transfers = Largest(kept, moves, b.CutoffDay, x.cfg.TopPerDay)
	if mode == store.ModeFull {
		b.Deltas = Deltas(moves)
	} else {
		balances, err := x.balances(ctx, f, Touched(moves))
		if err != nil {
			return err
		}
		b.Balances = balances
	}
	return x.st.Commit(ctx, b)
}

// balances reads the balance of every holder at the latest block, a holder whose call fails is left out
func (x *Indexer) balances(ctx context.Context, f Feed, holders []evm.Address) (map[evm.Address]*big.Int, error) {
	out := make(map[evm.Address]*big.Int, len(holders))
	if len(holders) == 0 {
		return out, nil
	}
	calls := make([]evm.Call3, len(holders))
	for i, h := range holders {
		calls[i] = f.BalanceCall(h)
	}
	res, err := x.rpc.Aggregate3(ctx, calls)
	if err != nil {
		return nil, fmt.Errorf("balanceOf: %w", err)
	}
	for i, r := range res {
		if v, ok := f.Balance(r); ok {
			out[holders[i]] = v
		}
	}
	return out, nil
}

/* Tail */

func (x *Indexer) tailLoop(ctx context.Context) {
	t := time.NewTicker(x.cfg.TailInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		err := x.tail(ctx)
		x.mu.Lock()
		if err == nil {
			x.lastTail, x.tailErr = x.now(), ""
		} else if ctx.Err() == nil {
			x.tailErr = err.Error()
		}
		x.mu.Unlock()
		if err != nil && ctx.Err() == nil {
			x.log.Warn("tail cycle failed", "error", err)
		}
		if x.now().Sub(x.pruned) > pruneEvery {
			if err := x.st.PruneTransfers(ctx, CutoffDay(x.now(), x.cfg.WindowDays)); err != nil && ctx.Err() == nil {
				x.log.Warn("prune transfers", "error", err)
			} else {
				x.pruned = x.now()
			}
		}
	}
}

// tail brings every ready token to the archive height with one query for all of them
func (x *Indexer) tail(ctx context.Context) error {
	for range tailPages {
		tokens, err := x.st.ReadyTokens(ctx, x.chain.ID)
		if err != nil || len(tokens) == 0 {
			return err
		}
		head, err := x.safeHead(ctx)
		if err != nil {
			return fmt.Errorf("archive height: %w", err)
		}
		from := slices.MinFunc(tokens, func(a, b store.Token) int { return cmp.Compare(a.NextBlock, b.NextBlock) }).NextBlock
		if from >= head {
			return nil
		}
		addrs := make([]evm.Address, len(tokens))
		anyWrapped := false
		for i, t := range tokens {
			f := x.feed(t)
			addrs[i] = f.Source
			anyWrapped = anyWrapped || f.Wrapped
		}
		page, err := x.logs.Page(ctx, x.chain.ID, sources.LogQuery{From: from, To: head, Addresses: addrs, Topics0: Topics(anyWrapped), Blocks: true}, sources.Background)
		if err != nil {
			return err
		}
		end := min(max(page.NextBlock, from), head)
		for _, t := range tokens {
			if t.NextBlock >= end {
				continue
			}
			err := x.apply(ctx, t, t.Mode, x.feed(t), page.Logs, t.NextBlock, end, 0)
			if errors.Is(err, store.ErrMoved) {
				continue
			}
			if err != nil {
				return fmt.Errorf("token %s: %w", t.Token, err)
			}
		}
		if end >= head {
			return nil
		}
	}
	return nil
}
