package store_test

import (
	"errors"
	"slices"
	"testing"
	"time"

	"tokens/internal/store"
	"tokens/internal/testkit"
)

func TestChainStatsAddPagesAndKeepTheFirstDayOut(t *testing.T) {
	st := testkit.NewDB(t).Store
	ctx := t.Context()
	if _, ok, err := st.StatsCursor(ctx, monad); err != nil || ok {
		t.Fatalf("a cursor before the first page: %v %v", ok, err)
	}
	// the first block counted is late on 2026-10-13, so that day began before the count
	first := time.Date(2026, 10, 13, 23, 59, 54, 0, time.UTC)
	// more than fits in 64 bits after two pages, as a day of gas prices in wei can be
	wei := "18000000000000000000"
	page1 := []store.StatsDay{
		{Day: "2026-10-13", Blocks: 20, Txs: 300, GasUsed: "150000000", GasLimit: "3000000000", GasPriceWei: wei},
		{Day: "2026-10-14", Blocks: 5, Txs: 70, GasUsed: "40000000", GasLimit: "750000000", GasPriceWei: wei},
	}
	if err := st.AddStats(ctx, monad, 100, 125, first, page1); err != nil {
		t.Fatal(err)
	}
	page2 := []store.StatsDay{{Day: "2026-10-14", Blocks: 15, Txs: 186, GasUsed: "60000000", GasLimit: "2250000000", GasPriceWei: wei}}
	if err := st.AddStats(ctx, monad, 125, 140, first, page2); err != nil {
		t.Fatal(err)
	}
	if err := st.AddStats(ctx, monad, 125, 140, first, page2); !errors.Is(err, store.ErrStatsMoved) {
		t.Fatalf("a page counted twice: %v", err)
	}
	cur, ok, err := st.StatsCursor(ctx, monad)
	if err != nil || !ok || cur.Next != 140 || !cur.First.Equal(first) || time.Since(cur.Updated).Abs() > time.Minute {
		t.Fatalf("cursor %+v %v %v", cur, ok, err)
	}
	days, err := st.StatsDays(ctx, monad, "2026-10-01", "2026-10-31")
	if err != nil {
		t.Fatal(err)
	}
	wantDays := []store.StatsDay{{Day: "2026-10-14", Blocks: 20, Txs: 256, GasUsed: "100000000", GasLimit: "3000000000", GasPriceWei: "36000000000000000000"}}
	if !slices.Equal(days, wantDays) {
		t.Errorf("days %+v", days)
	}
	if days, err := st.StatsDays(ctx, monad, "2026-10-15", "2026-10-31"); err != nil || len(days) != 0 {
		t.Errorf("days after the count %+v %v", days, err)
	}
	if days, err := st.StatsDays(ctx, "eip155:5042", "2026-10-01", "2026-10-31"); err != nil || len(days) != 0 {
		t.Errorf("another chain's days %+v %v", days, err)
	}
}
