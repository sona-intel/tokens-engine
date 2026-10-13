package tokens

import (
	"context"
	"time"

	"tokens/internal/config"
	"tokens/internal/index"
	"tokens/internal/sources"
	"tokens/internal/store"
)

// StatusDeps are the parts the status route reports on
type StatusDeps struct {
	Config    config.Config
	Store     *store.Store
	Indexer   *index.Indexer
	HyperSync *sources.HyperSync
	PaidCalls func() int64
	Now       func() time.Time
}

type ChainStatus struct {
	Chain   string `json:"chain"`
	Name    string `json:"name"`
	Holders string `json:"holders"`
}

type HyperSyncStatus struct {
	Enabled              bool    `json:"enabled"`
	BackgroundPerMinute  float64 `json:"background_per_minute"`
	InteractivePerMinute float64 `json:"interactive_per_minute"`
	Requests             int64   `json:"requests"`
	RateLimited          int64   `json:"rate_limited"`
	Failed               int64   `json:"failed"`
	PausedS              int64   `json:"paused_s"`
}

type BackfillStatus struct {
	Token             string `json:"token"`
	Mode              string `json:"mode"`
	NextBlock         uint64 `json:"next_block"`
	HeadBlock         uint64 `json:"head_block"`
	Requests          int    `json:"requests"`
	ProjectedRequests *int   `json:"projected_requests"`
	StartedAt         string `json:"started_at"`
}

type IndexedToken struct {
	Token        string   `json:"token"`
	State        string   `json:"state"`
	Mode         string   `json:"mode"`
	StartBlock   uint64   `json:"start_block"`
	IndexedBlock *uint64  `json:"indexed_block"`
	Holders      int64    `json:"holders"`
	Requests     int      `json:"requests"`
	BackfillS    *float64 `json:"backfill_s"`
	LastError    *string  `json:"last_error"`
}

type IndexStatus struct {
	Chain          string          `json:"chain"`
	HeadBlock      *uint64         `json:"head_block"`
	MaxTokens      int             `json:"max_tokens"`
	BackfillBudget int             `json:"backfill_budget"`
	Counts         map[string]int  `json:"counts"`
	DiskBytes      int64           `json:"disk_bytes"`
	Backfill       *BackfillStatus `json:"backfill"`
	TailAt         *string         `json:"tail_at"`
	TailError      *string         `json:"tail_error"`
	Tokens         []IndexedToken  `json:"tokens"`
}

type Status struct {
	Kind         string          `json:"kind"`
	Chains       []ChainStatus   `json:"chains"`
	PaidRPCCalls int64           `json:"paid_rpc_calls"`
	HyperSync    HyperSyncStatus `json:"hypersync"`
	Index        *IndexStatus    `json:"index"`
	Coverage     []Note          `json:"coverage"`
}

// BuildStatus reads the state of the engine, the index part needs the database
func BuildStatus(ctx context.Context, d StatusDeps) (Status, error) {
	out := Status{Kind: "tokens_status", Coverage: AllNotes, PaidRPCCalls: d.PaidCalls()}
	for _, c := range d.Config.Chains {
		out.Chains = append(out.Chains, ChainStatus{Chain: c.ID, Name: c.Name, Holders: c.Holders})
	}
	hs := d.HyperSync.Stats()
	out.HyperSync = HyperSyncStatus{
		Enabled: d.HyperSync.Enabled(), BackgroundPerMinute: d.Config.HyperSyncPerMinute, InteractivePerMinute: d.Config.HyperSyncInteractive,
		Requests: hs.Requests, RateLimited: hs.RateLimited, Failed: hs.Failed, PausedS: int64(d.HyperSync.PausedFor().Seconds()),
	}
	if d.Indexer == nil {
		return out, nil
	}
	ix := &IndexStatus{
		Chain: d.Indexer.Chain(), MaxTokens: d.Config.Index.MaxTokens, BackfillBudget: d.Config.Index.BackfillBudget,
		Counts: map[string]int{store.StateQueued: 0, store.StateIndexing: 0, store.StateReady: 0, store.StateFailed: 0}, Tokens: []IndexedToken{},
	}
	if head := d.Indexer.Head(ctx); head > 0 {
		ix.HeadBlock = &head
	}
	rows, err := d.Store.Tokens(ctx, d.Indexer.Chain())
	if err != nil {
		return Status{}, err
	}
	for _, t := range rows {
		ix.Counts[t.State]++
		it := IndexedToken{Token: t.Token.String(), State: t.State, Mode: t.Mode, StartBlock: t.StartBlock, Holders: t.Holders, Requests: t.Requests, BackfillS: t.BackfillS}
		if t.NextBlock > t.StartBlock {
			n := t.NextBlock - 1
			it.IndexedBlock = &n
		}
		if t.LastError != "" {
			e := t.LastError
			it.LastError = &e
		}
		ix.Tokens = append(ix.Tokens, it)
	}
	sizes, err := d.Store.Sizes(ctx)
	if err != nil {
		return Status{}, err
	}
	ix.DiskBytes = sizes.Total
	if p := d.Indexer.Current(); p != nil {
		b := &BackfillStatus{Token: p.Token.String(), Mode: p.Mode, NextBlock: p.NextBlock, HeadBlock: p.Head, Requests: p.Requests, StartedAt: stamp(p.Started)}
		if p.Mode == store.ModeFull && p.Projected > 0 {
			n := p.Projected
			b.ProjectedRequests = &n
		}
		ix.Backfill = b
	}
	if at, e := d.Indexer.Tail(); !at.IsZero() || e != "" {
		if !at.IsZero() {
			s := stamp(at)
			ix.TailAt = &s
		}
		if e != "" {
			ix.TailError = &e
		}
	}
	out.Index = ix
	return out, nil
}

// compile-time check that the indexer serves the service
var _ Index = (*index.Indexer)(nil)
