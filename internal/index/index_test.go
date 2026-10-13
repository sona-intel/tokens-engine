package index

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"math/big"
	"slices"
	"sync"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/sources"
	"tokens/internal/store"
	"tokens/internal/testkit"
)

const monad = "eip155:143"

// fakeLogs answers from recorded pages like the server would: the logs of the asked contracts and topics in
// [From, To), at most step blocks a page when step is set
type fakeLogs struct {
	mu    sync.Mutex
	logs  []sources.Log
	head  uint64
	step  uint64
	calls int
}

func (f *fakeLogs) Page(_ context.Context, _ string, q sources.LogQuery, _ sources.Priority) (sources.Page, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	end := q.To
	if f.step > 0 && q.From > 0 {
		end = min(end, q.From+f.step)
	}
	if q.From == 0 && f.step > 0 {
		// the first page jumps over the empty start of the chain like HyperSync does
		end = min(end, f.logs[0].Block+f.step)
	}
	out := sources.Page{NextBlock: end, ArchiveHeight: f.head}
	for _, l := range f.logs {
		if l.Block >= q.From && l.Block < end && slices.Contains(q.Addresses, l.Address) && slices.Contains(q.Topics0, l.Topics[0]) {
			out.Logs = append(out.Logs, l)
		}
	}
	return out, nil
}

func (f *fakeLogs) Height(context.Context, string) (uint64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.head, nil
}

// failingChain answers every balanceOf with a failure, so no test needs a balance that was not recorded
type failingChain struct{ asked int }

func (c *failingChain) Aggregate3(_ context.Context, calls []evm.Call3) ([]evm.Result3, error) {
	c.asked += len(calls)
	return make([]evm.Result3, len(calls)), nil
}

func newIndexer(t *testing.T, logs *fakeLogs, chain Chain, cfg config.Index) (*Indexer, *store.Store) {
	t.Helper()
	db := testkit.NewDB(t)
	ch := config.Chain{ID: monad, Name: "monad", WrappedNative: monWMON}
	x := New(ch, cfg, db.Store, logs, chain, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// the recorded blocks are from 2026-10-13, so the transfer window ends that day
	x.now = func() time.Time { return time.Date(2026, 10, 13, 18, 0, 0, 0, time.UTC) }
	return x, db.Store
}

func indexCfg() config.Index {
	return config.Index{MaxTokens: 3, BackfillBudget: 120, ActivityWindowBlocks: 20000, TailInterval: time.Minute, LagBlocks: 1, WindowDays: 7, TopPerDay: 10, RetryFailedAfter: time.Minute}
}

func balancesOf(t *testing.T, st *store.Store, id int32) map[evm.Address]*big.Int {
	t.Helper()
	rows, err := st.Holders(t.Context(), id, 10000, nil)
	if err != nil {
		t.Fatal(err)
	}
	out := map[evm.Address]*big.Int{}
	for _, r := range rows {
		out[r.Holder] = r.Balance
	}
	return out
}

func TestFullBackfillOfRecordedLogs(t *testing.T) {
	p := page(t, "monad_wmon")
	logs := &fakeLogs{logs: p.Logs, head: p.NextBlock}
	x, st := newIndexer(t, logs, &failingChain{}, indexCfg())
	ctx := t.Context()
	tok, err := x.Ensure(ctx, monWMON, 18)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.backfill(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, ok, err := x.Lookup(ctx, monWMON)
	if err != nil || !ok {
		t.Fatal(err)
	}
	// the safe head is one block behind the archive height
	if got.State != store.StateReady || got.Mode != store.ModeFull || got.NextBlock != p.NextBlock || got.Requests != 1 || got.BackfillS == nil {
		t.Fatalf("token %+v", got)
	}
	want := Deltas(movesOf(p.Logs, true))
	for a, v := range want {
		if v.Sign() == 0 {
			delete(want, a)
		}
	}
	have := balancesOf(t, st, got.ID)
	if len(have) != len(want) || got.Holders != int64(len(want)) {
		t.Fatalf("%d balances, %d holders, want %d", len(have), got.Holders, len(want))
	}
	for a, v := range want {
		if have[a] == nil || have[a].Cmp(v) != 0 {
			t.Fatalf("%s holds %v, want %s", a, have[a], v)
		}
	}
	// the stored page is ranked by balance
	rows, _ := st.Holders(ctx, got.ID, 5, nil)
	next, _ := st.Holders(ctx, got.ID, 5, &store.HolderCursor{Balance: rows[4].Balance, Holder: rows[4].Holder})
	if len(next) != 5 || next[0].Balance.Cmp(rows[4].Balance) > 0 {
		t.Error("the second page does not follow the first")
	}
	tr, err := st.TopTransfers(ctx, got.ID, "2026-10-07", 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(tr) != 10 {
		t.Fatalf("%d transfers kept, want the top 10 of the one day", len(tr))
	}
	for _, x := range tr {
		if x.Time.Format(time.DateOnly) != "2026-10-13" {
			t.Fatalf("transfer of %s", x.Time)
		}
	}
}

func TestBackfillSwitchesToActivity(t *testing.T) {
	p := page(t, "monad_wmon")
	logs := &fakeLogs{logs: p.Logs, head: p.NextBlock, step: 50}
	chain := &failingChain{}
	cfg := indexCfg()
	cfg.BackfillBudget, cfg.ActivityWindowBlocks = 2, 150
	x, st := newIndexer(t, logs, chain, cfg)
	ctx := t.Context()
	tok, err := x.Ensure(ctx, monWMON, 18)
	if err != nil {
		t.Fatal(err)
	}
	if err := x.backfill(ctx, tok); err != nil {
		t.Fatal(err)
	}
	got, _, _ := x.Lookup(ctx, monWMON)
	// with a lag of one block the safe head is the archive height itself
	start := p.NextBlock - 150
	if got.State != store.StateReady || got.Mode != store.ModeActivity || got.StartBlock != start || got.NextBlock != p.NextBlock {
		t.Fatalf("token %+v, want activity from %d", got, start)
	}
	// activity mode asks balanceOf for every address the window touched, the fake fails them all
	if chain.asked == 0 || got.Holders != 0 || len(balancesOf(t, st, got.ID)) != 0 {
		t.Errorf("asked %d balances, %d holders", chain.asked, got.Holders)
	}
	tr, _ := st.TopTransfers(ctx, got.ID, "2026-10-07", 100)
	for _, x := range tr {
		if x.Block < start {
			t.Fatalf("a transfer from block %d is before the activity start %d", x.Block, start)
		}
	}
}

func TestTailCatchesUpEveryReadyToken(t *testing.T) {
	usdc := page(t, "monad_usdc")
	logs := &fakeLogs{logs: usdc.Logs, head: usdc.NextBlock}
	x, st := newIndexer(t, logs, &failingChain{}, indexCfg())
	ctx := t.Context()
	tok, err := x.Ensure(ctx, monUSDC, 6)
	if err != nil {
		t.Fatal(err)
	}
	// a token whose backfill ended at the first recorded block
	if err := st.StartBackfill(ctx, tok.ID, store.ModeFull, usdc.Logs[0].Block, true); err != nil {
		t.Fatal(err)
	}
	if err := st.MarkReady(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	if err := x.tail(ctx); err != nil {
		t.Fatal(err)
	}
	got, _, _ := x.Lookup(ctx, monUSDC)
	if got.NextBlock != usdc.NextBlock || logs.calls != 1 {
		t.Fatalf("next block %d after %d calls", got.NextBlock, logs.calls)
	}
	want := Deltas(movesOf(usdc.Logs, false))
	have := balancesOf(t, st, got.ID)
	n := 0
	for a, v := range want {
		if v.Sign() == 0 {
			continue
		}
		n++
		if have[a] == nil || have[a].Cmp(v) != 0 {
			t.Fatalf("%s holds %v, want %s", a, have[a], v)
		}
	}
	if len(have) != n {
		t.Errorf("%d balances, want %d", len(have), n)
	}
	// a second cycle at the same head reads nothing
	if err := x.tail(ctx); err != nil || logs.calls != 1 {
		t.Errorf("second cycle: %v after %d calls", err, logs.calls)
	}
}

func TestEnsureCapsTheIndex(t *testing.T) {
	x, _ := newIndexer(t, &fakeLogs{head: 10}, &failingChain{}, indexCfg())
	ctx := t.Context()
	for i := range 3 {
		var a evm.Address
		a[19] = byte(i + 1)
		if _, err := x.Ensure(ctx, a, 18); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := x.Ensure(ctx, monUSDC, 6); !errors.Is(err, store.ErrFull) {
		t.Errorf("fourth token: %v", err)
	}
	var a evm.Address
	a[19] = 1
	if _, err := x.Ensure(ctx, a, 18); err != nil {
		t.Errorf("a token already indexed: %v", err)
	}
}
