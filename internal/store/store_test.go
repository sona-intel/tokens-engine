package store_test

import (
	"errors"
	"math/big"
	"testing"
	"time"

	"tokens/internal/evm"
	"tokens/internal/store"
	"tokens/internal/testkit"
)

const monad = "eip155:143"

func addr(n byte) evm.Address {
	var a evm.Address
	a[19] = n
	return a
}

func TestCommitDeltasAndBalances(t *testing.T) {
	db := testkit.NewDB(t)
	st, ctx := db.Store, t.Context()
	tok, created, err := st.EnsureToken(ctx, monad, addr(100), 6, 10)
	if err != nil || !created || tok.State != store.StateQueued {
		t.Fatalf("%+v %v %v", tok, created, err)
	}
	if err := st.StartBackfill(ctx, tok.ID, store.ModeFull, 0, true); err != nil {
		t.Fatal(err)
	}
	b := store.Batch{TokenID: tok.ID, FromBlock: 0, NextBlock: 10, Requests: 1, Deltas: map[evm.Address]*big.Int{
		addr(1): big.NewInt(500), addr(2): big.NewInt(500), addr(3): big.NewInt(200), addr(5): big.NewInt(99), addr(6): big.NewInt(1000),
	}}
	if err := st.Commit(ctx, b); err != nil {
		t.Fatal(err)
	}
	// a repeated batch for the old cursor changes nothing
	if err := st.Commit(ctx, b); !errors.Is(err, store.ErrMoved) {
		t.Fatalf("a stale batch: %v", err)
	}
	// addr(3) sends everything to addr(1), so it leaves the holders
	if err := st.Commit(ctx, store.Batch{TokenID: tok.ID, FromBlock: 10, NextBlock: 20, Requests: 1, Deltas: map[evm.Address]*big.Int{
		addr(3): big.NewInt(-200), addr(1): big.NewInt(200),
	}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ := st.GetToken(ctx, monad, addr(100))
	if got.NextBlock != 20 || got.Holders != 4 || got.Requests != 2 || got.State != store.StateIndexing {
		t.Fatalf("token %+v", got)
	}
	// numeric order: 1000, 700, 500, 99, which as text would be 99, 700, 500, 1000
	rows, err := st.Holders(ctx, tok.ID, 2, nil)
	if err != nil || len(rows) != 2 || rows[0].Holder != addr(6) || rows[1].Holder != addr(1) || rows[1].Balance.Int64() != 700 {
		t.Fatalf("first page %+v %v", rows, err)
	}
	rows, _ = st.Holders(ctx, tok.ID, 5, &store.HolderCursor{Balance: rows[1].Balance, Holder: rows[1].Holder})
	if len(rows) != 2 || rows[0].Holder != addr(2) || rows[1].Holder != addr(5) {
		t.Fatalf("second page %+v", rows)
	}
	// absolute balances replace, and a zero removes the holder
	if err := st.Commit(ctx, store.Batch{TokenID: tok.ID, FromBlock: 20, NextBlock: 30, Balances: map[evm.Address]*big.Int{
		addr(1): big.NewInt(5), addr(2): big.NewInt(0), addr(4): big.NewInt(9), addr(5): big.NewInt(0), addr(6): big.NewInt(0),
	}}); err != nil {
		t.Fatal(err)
	}
	got, _, _ = st.GetToken(ctx, monad, addr(100))
	rows, _ = st.Holders(ctx, tok.ID, 10, nil)
	if got.Holders != 2 || len(rows) != 2 || rows[0].Holder != addr(4) || rows[1].Balance.Int64() != 5 {
		t.Fatalf("after balances %+v %+v", got, rows)
	}
	if err := st.MarkReady(ctx, tok.ID); err != nil {
		t.Fatal(err)
	}
	ready, err := st.ReadyTokens(ctx, monad)
	if err != nil || len(ready) != 1 || ready[0].BackfillS == nil || ready[0].ReadyAt == nil {
		t.Fatalf("ready %+v %v", ready, err)
	}
	if sz, err := st.Sizes(ctx); err != nil || sz.Total <= 0 {
		t.Errorf("sizes %+v %v", sz, err)
	}
}

func TestLargeTransfersPerDay(t *testing.T) {
	db := testkit.NewDB(t)
	st, ctx := db.Store, t.Context()
	tok, _, _ := st.EnsureToken(ctx, monad, addr(100), 6, 10)
	day1 := time.Date(2026, 10, 12, 10, 0, 0, 0, time.UTC)
	day2 := day1.AddDate(0, 0, 1)
	var trs []store.Transfer
	for i := range 6 {
		at := day1
		if i >= 3 {
			at = day2
		}
		// 1, 10, 100 on day 1 and 1000, 10000, 100000 on day 2, which sort differently as text
		amount := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(i)), nil)
		trs = append(trs, store.Transfer{Block: uint64(100 + i), LogIndex: 1, Time: at, From: addr(1), To: addr(2), Amount: amount})
	}
	if err := st.Commit(ctx, store.Batch{TokenID: tok.ID, FromBlock: 0, NextBlock: 200, Transfers: trs, CutoffDay: "2026-10-12", TopPerDay: 2}); err != nil {
		t.Fatal(err)
	}
	got, err := st.TopTransfers(ctx, tok.ID, "2026-10-12", 10)
	if err != nil || len(got) != 4 {
		t.Fatalf("%d transfers, want 2 a day: %v", len(got), err)
	}
	if got[0].Amount.Int64() != 100000 || got[1].Amount.Int64() != 10000 || got[2].Amount.Int64() != 100 || got[3].Amount.Int64() != 10 {
		t.Errorf("order %v ... %v", got[0].Amount, got[3].Amount)
	}
	if err := st.PruneTransfers(ctx, "2026-10-13"); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.TopTransfers(ctx, tok.ID, "2026-10-01", 10); len(got) != 2 {
		t.Errorf("%d transfers after the prune, want day 2's", len(got))
	}
}

func TestJobsAndCap(t *testing.T) {
	db := testkit.NewDB(t)
	st, ctx := db.Store, t.Context()
	a, _, _ := st.EnsureToken(ctx, monad, addr(1), 6, 2)
	b, _, _ := st.EnsureToken(ctx, monad, addr(2), 6, 2)
	if _, _, err := st.EnsureToken(ctx, monad, addr(3), 6, 2); !errors.Is(err, store.ErrFull) {
		t.Fatalf("third token: %v", err)
	}
	if again, created, err := st.EnsureToken(ctx, monad, addr(1), 6, 2); err != nil || created || again.ID != a.ID {
		t.Fatalf("existing token: %+v %v %v", again, created, err)
	}
	if err := st.StartBackfill(ctx, b.ID, store.ModeFull, 0, true); err != nil {
		t.Fatal(err)
	}
	// an interrupted backfill goes first
	if job, ok, err := st.NextJob(ctx, monad, time.Hour); err != nil || !ok || job.ID != b.ID {
		t.Fatalf("job %+v %v %v", job, ok, err)
	}
	if err := st.MarkFailed(ctx, b.ID, "boom"); err != nil {
		t.Fatal(err)
	}
	if job, _, _ := st.NextJob(ctx, monad, time.Hour); job.ID != a.ID {
		t.Fatalf("job %d, want the queued token", job.ID)
	}
	if err := st.MarkReady(ctx, a.ID); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := st.NextJob(ctx, monad, time.Hour); ok {
		t.Error("a failed token was retried before its wait")
	}
	if job, ok, _ := st.NextJob(ctx, monad, time.Nanosecond); !ok || job.ID != b.ID || job.LastError != "boom" {
		t.Errorf("retry %+v %v", job, ok)
	}
}

func TestIndexedTokens(t *testing.T) {
	db := testkit.NewDB(t)
	st, ctx := db.Store, t.Context()
	if got, err := st.IndexedTokens(ctx, monad); err != nil || len(got) != 0 {
		t.Fatalf("empty index: %v %v", got, err)
	}
	for _, n := range []byte{7, 3} {
		if _, _, err := st.EnsureToken(ctx, monad, addr(n), 6, 10); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.EnsureToken(ctx, "eip155:1", addr(9), 6, 10); err != nil {
		t.Fatal(err)
	}
	got, err := st.IndexedTokens(ctx, monad)
	if err != nil || len(got) != 2 || got[0] != addr(7) || got[1] != addr(3) {
		t.Errorf("tokens %v %v", got, err)
	}
}
