package fixture

import (
	"context"
	"errors"
	"slices"
	"sync"
	"testing"
	"time"

	"tokens/internal/evm"
	"tokens/internal/holdermap"
	"tokens/internal/sources"
	"tokens/internal/store"
)

// EthUNI is the token of the recorded holder map
var EthUNI = evm.MustAddress("0x1f9840a85d5af5bf1d1762f925bdaddc4201f984")

// The recorded holder map query of UNI reads [UNIMapFrom, UNIMapHead] in three pages
const (
	UNIMapFrom = 25969639
	UNIMapHead = 26185638
)

// UNIMapPages are the recorded pages of the UNI holder map query, by the block each one starts at
func UNIMapPages(t testing.TB) map[uint64]sources.Page {
	t.Helper()
	return map[uint64]sources.Page{25969639: page(t, "eth_uni_map.p1"), 26039863: page(t, "eth_uni_map.p2"), 26118258: page(t, "eth_uni_map.p3")}
}

/* Store */

// MapStore keeps holder maps in memory as the holder_map table does
type MapStore struct {
	mu   sync.Mutex
	rows map[holdermap.Key]store.HolderMap
	Puts int
}

func NewMapStore() *MapStore { return &MapStore{rows: map[holdermap.Key]store.HolderMap{}} }

func (m *MapStore) HolderMap(_ context.Context, chain string, token evm.Address, limit int) (store.HolderMap, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	r, ok := m.rows[holdermap.Key{Chain: chain, Token: token, Limit: limit}]
	return r, ok, nil
}

func (m *MapStore) PutHolderMap(_ context.Context, r store.HolderMap) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.rows[holdermap.Key{Chain: r.Chain, Token: r.Token, Limit: r.Limit}] = r
	m.Puts++
	return nil
}

func (m *MapStore) PruneHolderMaps(_ context.Context, before time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, r := range m.rows {
		if r.BuiltAt.Before(before) {
			delete(m.rows, k)
			n++
		}
	}
	return n, nil
}

/* HyperSync */

// MapLogs answers a holder map query from the recorded page that holds its first block: the logs of that page from
// the query's first block on whose sender and receiver the query asks for, and the page's next block
type MapLogs struct {
	mu    sync.Mutex
	pages map[uint64]sources.Page
	head  uint64
	Calls int
	Down  bool
	// Queries are the queries asked, in order
	Queries []sources.LogQuery
}

func NewMapLogs(pages map[uint64]sources.Page, head uint64) *MapLogs {
	return &MapLogs{pages: pages, head: head}
}

func (l *MapLogs) SetDown(v bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Down = v
}

func (l *MapLogs) Page(_ context.Context, _ string, q sources.LogQuery, _ sources.Priority) (sources.Page, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.Calls++
	l.Queries = append(l.Queries, q)
	if l.Down {
		return sources.Page{}, &sources.UnavailableError{Source: "hypersync", RetryAfter: time.Millisecond, Err: errors.New("429")}
	}
	var (
		start uint64
		found bool
	)
	for from := range l.pages {
		if from <= q.From && (!found || from > start) {
			start, found = from, true
		}
	}
	if !found || l.pages[start].NextBlock <= q.From {
		return sources.Page{}, errUnrecorded
	}
	rec := l.pages[start]
	out := sources.Page{NextBlock: min(rec.NextBlock, q.To), ArchiveHeight: rec.ArchiveHeight}
	for _, x := range rec.Logs {
		if x.Block < q.From || x.Block >= q.To || !slices.Contains(q.Addresses, x.Address) || len(x.Topics) < 3 ||
			!slices.Contains(q.Topics1, x.Topics[1]) || !slices.Contains(q.Topics2, x.Topics[2]) {
			continue
		}
		out.Logs = append(out.Logs, x)
	}
	return out, nil
}

func (l *MapLogs) Height(context.Context, string) (uint64, error) { return l.head, nil }

/* Blocks */

// mapBlocks starts every window where the recorder started UNI's, and reads block times from the recorded headers
type mapBlocks struct{ c *chain }

func (b mapBlocks) FirstBlockAt(context.Context, string, time.Time, uint64) (uint64, error) {
	return UNIMapFrom, nil
}

func (b mapBlocks) BlockTime(ctx context.Context, _ string, n uint64) (time.Time, error) {
	h, err := b.c.HeaderByNumber(ctx, n)
	return h.Time, err
}

// BuildMaps runs every queued holder map build
func (f *Fakes) BuildMaps(ctx context.Context) {
	for f.Maps.Work(ctx, f.mapBlocks) {
	}
}
