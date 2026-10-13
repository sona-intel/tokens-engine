package holdermap

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sync"
	"sync/atomic"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/sources"
	"tokens/internal/store"
)

const (
	StatusReady       = "ready"
	StatusBuilding    = "building"
	StatusUnavailable = "unavailable"

	// ReasonQueueFull and ReasonFailed say why links are unavailable
	ReasonQueueFull = "queue_full"
	ReasonFailed    = "failed"

	// jobTimeout bounds one build, which waits for the quota between its pages
	jobTimeout = 20 * time.Minute
	// pageAttempts is how often a build asks for a page that HyperSync did not give, each ask waits out the quota a few times
	pageAttempts = 3
	pruneEvery   = time.Hour
	// restartShare is the part of the remaining pages a restarted window plans to use, so a window that ends at the head fits
	restartShare = 0.9
)

// Logs is the HyperSync client
type Logs interface {
	Page(ctx context.Context, chain string, q sources.LogQuery, prio sources.Priority) (sources.Page, error)
	Height(ctx context.Context, chain string) (uint64, error)
}

// Store keeps the built links
type Store interface {
	HolderMap(ctx context.Context, chain string, token evm.Address, limit int) (store.HolderMap, bool, error)
	PutHolderMap(ctx context.Context, m store.HolderMap) error
	PruneHolderMaps(ctx context.Context, before time.Time) (int64, error)
}

// Blocks finds the first block of the window and the times of blocks
type Blocks interface {
	FirstBlockAt(ctx context.Context, chain string, t time.Time, hi uint64) (uint64, error)
	BlockTime(ctx context.Context, chain string, n uint64) (time.Time, error)
}

// State is what a call gets for the links of a map
type State struct {
	Status string
	// Reason says why the links are unavailable
	Reason  string
	BuiltAt time.Time
	Doc     Doc
}

type job struct {
	key   Key
	nodes []evm.Address
}

// Stats counts the builds, for metrics
type Stats struct {
	Queued   int
	Running  bool
	Built    int64
	Failed   int64
	Requests int64
}

// Builder queues maps whose links are missing or old and builds them one at a time
type Builder struct {
	cfg  config.HolderMap
	logs Logs
	st   Store
	log  *slog.Logger
	now  func() time.Time

	mu      sync.Mutex
	queue   []job
	pending map[Key]bool
	failed  map[Key]time.Time
	running bool
	wake    chan struct{}

	built    atomic.Int64
	failures atomic.Int64
	requests atomic.Int64
}

func New(cfg config.HolderMap, logs Logs, st Store, log *slog.Logger) *Builder {
	if log == nil {
		log = slog.Default()
	}
	return &Builder{cfg: cfg, logs: logs, st: st, log: log, now: time.Now, pending: map[Key]bool{}, failed: map[Key]time.Time{}, wake: make(chan struct{}, 1)}
}

// WithClock swaps the clock, for tests
func (b *Builder) WithClock(now func() time.Time) *Builder {
	b.now = now
	return b
}

// WindowDays is the configured window
func (b *Builder) WindowDays() int { return b.cfg.WindowDays }

func (b *Builder) Stats() Stats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return Stats{Queued: len(b.queue), Running: b.running, Built: b.built.Load(), Failed: b.failures.Load(), Requests: b.requests.Load()}
}

// Links answers the stored links when they are fresh, and otherwise queues a build of the map over these nodes.
// It never waits for HyperSync.
func (b *Builder) Links(ctx context.Context, key Key, nodes []evm.Address) (State, error) {
	row, ok, err := b.st.HolderMap(ctx, key.Chain, key.Token, key.Limit)
	if err != nil {
		return State{}, err
	}
	now := b.now()
	if ok && now.Sub(row.BuiltAt) < b.cfg.FreshFor {
		doc, err := ParseDoc(row.Links)
		if err != nil {
			return State{}, err
		}
		return State{Status: StatusReady, BuiltAt: row.BuiltAt, Doc: doc}, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pending[key] {
		return State{Status: StatusBuilding}, nil
	}
	for k, at := range b.failed {
		if now.Sub(at) >= b.cfg.RetryFailedAfter {
			delete(b.failed, k)
		}
	}
	if _, ok := b.failed[key]; ok {
		return State{Status: StatusUnavailable, Reason: ReasonFailed}, nil
	}
	if len(b.queue) >= b.cfg.QueueMax {
		return State{Status: StatusUnavailable, Reason: ReasonQueueFull}, nil
	}
	b.queue = append(b.queue, job{key: key, nodes: nodes})
	b.pending[key] = true
	select {
	case b.wake <- struct{}{}:
	default:
	}
	return State{Status: StatusBuilding}, nil
}

// Run builds the queued maps one at a time and prunes old ones until ctx ends
func (b *Builder) Run(ctx context.Context, blocks Blocks) {
	b.prune(ctx)
	prune := time.NewTicker(pruneEvery)
	defer prune.Stop()
	for {
		for b.Work(ctx, blocks) {
		}
		select {
		case <-ctx.Done():
			return
		case <-prune.C:
			b.prune(ctx)
		case <-b.wake:
		}
	}
}

func (b *Builder) prune(ctx context.Context) {
	n, err := b.st.PruneHolderMaps(ctx, b.now().Add(-b.cfg.KeepFor))
	switch {
	case err != nil && ctx.Err() == nil:
		b.log.Error("prune holder maps", "error", err)
	case n > 0:
		b.log.Info("holder maps pruned", "rows", n)
	}
}

// Work builds the oldest queued map and reports whether there was one
func (b *Builder) Work(ctx context.Context, blocks Blocks) bool {
	b.mu.Lock()
	if len(b.queue) == 0 || ctx.Err() != nil {
		b.mu.Unlock()
		return false
	}
	j := b.queue[0]
	b.queue = b.queue[1:]
	b.running = true
	b.mu.Unlock()

	jctx, cancel := context.WithTimeout(ctx, jobTimeout)
	err := b.build(jctx, j, blocks)
	cancel()

	b.mu.Lock()
	defer b.mu.Unlock()
	b.running = false
	delete(b.pending, j.key)
	if err != nil {
		b.failures.Add(1)
		b.failed[j.key] = b.now()
		if ctx.Err() == nil {
			b.log.Error("holder map build failed", "map", j.key, "error", err)
		}
	} else {
		b.built.Add(1)
	}
	return true
}

func (b *Builder) build(ctx context.Context, j job, blocks Blocks) error {
	started := b.now()
	chain := j.key.Chain
	// a call between the last build's store and its end may have queued the map again
	if row, ok, err := b.st.HolderMap(ctx, chain, j.key.Token, j.key.Limit); err == nil && ok && started.Sub(row.BuiltAt) < b.cfg.FreshFor {
		return nil
	}
	head, err := b.logs.Height(ctx, chain)
	if err != nil {
		return fmt.Errorf("archive height: %w", err)
	}
	headAt, err := blocks.BlockTime(ctx, chain, head)
	if err != nil {
		return fmt.Errorf("time of block %d: %w", head, err)
	}
	from, err := blocks.FirstBlockAt(ctx, chain, headAt.Add(-time.Duration(b.cfg.WindowDays)*24*time.Hour), head)
	if err != nil {
		return fmt.Errorf("first block of the window: %w", err)
	}
	r, err := b.read(ctx, j, from, head+1)
	if err != nil {
		return err
	}
	doc := Doc{WindowDays: float64(b.cfg.WindowDays), Cut: r.cut, Requests: r.pages, Nodes: len(j.nodes), Pairs: r.pairs.list()}
	if r.cut {
		a, err := blocks.BlockTime(ctx, chain, r.from)
		if err != nil {
			return fmt.Errorf("time of block %d: %w", r.from, err)
		}
		z, err := blocks.BlockTime(ctx, chain, r.to)
		if err != nil {
			return fmt.Errorf("time of block %d: %w", r.to, err)
		}
		doc.WindowDays = math.Round(z.Sub(a).Hours()/24*10) / 10
	}
	raw, err := doc.Marshal()
	if err != nil {
		return err
	}
	builtAt := b.now()
	if err := b.st.PutHolderMap(ctx, store.HolderMap{Chain: chain, Token: j.key.Token, Limit: j.key.Limit, BuiltAt: builtAt, FromBlock: r.from, ToBlock: r.to, Links: raw}); err != nil {
		return err
	}
	b.log.Info("holder map built", "map", j.key, "requests", r.pages, "from_block", r.from, "to_block", r.to, "window_days", doc.WindowDays,
		"cut", r.cut, "pairs", len(doc.Pairs), "seconds", int(builtAt.Sub(started).Seconds()))
	return nil
}

type reading struct {
	from, to uint64
	pages    int
	cut      bool
	pairs    *pairs
}

// read pages [from, end) within the page budget. When the first page shows that the budget cannot reach the head,
// it starts again later, so the window it keeps ends at the head; when the budget still runs out it keeps what it read.
func (b *Builder) read(ctx context.Context, j job, from, end uint64) (reading, error) {
	topics := make([]evm.Hash, len(j.nodes))
	for i, a := range j.nodes {
		topics[i] = Topic(a)
	}
	q := sources.LogQuery{Addresses: []evm.Address{j.key.Token}, Topics0: []evm.Hash{evm.TopicTransfer}, Topics1: topics, Topics2: topics}
	budget := b.cfg.PageBudget
	r := reading{from: from, pairs: newPairs(j.key.Token, j.nodes)}
	next := from
	for next < end && r.pages < budget {
		q.From, q.To = next, end
		page, err := b.page(ctx, j.key.Chain, q)
		if err != nil {
			return reading{}, err
		}
		r.pages++
		b.requests.Add(1)
		if page.NextBlock <= next {
			return reading{}, errors.New("hypersync made no progress")
		}
		reached := min(page.NextBlock, end)
		r.pairs.add(page.Logs, next, reached)
		next = reached
		left := uint64(budget - r.pages)
		if r.pages == 1 && next < end && left > 0 {
			if plan := uint64(float64((next-from)*left) * restartShare); plan > 0 && plan < end-next {
				r.from, next, r.cut = end-plan, end-plan, true
				r.pairs = newPairs(j.key.Token, j.nodes)
			}
		}
	}
	if next < end {
		r.cut = true
	}
	r.to = next - 1
	return r, nil
}

// page asks again when HyperSync stays unavailable, the client itself waits for the quota between asks
func (b *Builder) page(ctx context.Context, chain string, q sources.LogQuery) (sources.Page, error) {
	for attempt := 1; ; attempt++ {
		p, err := b.logs.Page(ctx, chain, q, sources.Job)
		var ue *sources.UnavailableError
		if err == nil || !errors.As(err, &ue) || attempt >= pageAttempts {
			return p, err
		}
		b.log.Warn("holder map page unavailable, asking again", "token", q.Addresses[0], "from_block", q.From, "error", err)
		t := time.NewTimer(min(ue.RetryAfter, time.Minute))
		select {
		case <-ctx.Done():
			t.Stop()
			return sources.Page{}, ctx.Err()
		case <-t.C:
		}
	}
}
