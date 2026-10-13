package index

import (
	"math/big"
	"testing"
	"time"

	"tokens/internal/evm"
	"tokens/internal/sources"
	"tokens/internal/testkit"
)

var (
	monUSDC = evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603")
	monWMON = evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a")
)

func page(t *testing.T, name string) sources.Page {
	t.Helper()
	p, err := sources.ParsePage(testkit.Read(t, "hypersync/"+name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func movesOf(logs []sources.Log, wrapped bool) []Move {
	var out []Move
	for _, l := range logs {
		if m, ok := Decode(l, wrapped); ok {
			out = append(out, m)
		}
	}
	return out
}

func sum(m map[evm.Address]*big.Int) *big.Int {
	out := new(big.Int)
	for _, v := range m {
		out.Add(out, v)
	}
	return out
}

// Transfers move value between holders, so the deltas add up to what was minted less what was burned
func TestDeltasConserveValue(t *testing.T) {
	for _, name := range []string{"monad_usdc", "eth_mkr"} {
		p := page(t, name)
		moves := movesOf(p.Logs, false)
		if len(moves) != len(p.Logs) {
			t.Fatalf("%s: %d of %d logs decode", name, len(moves), len(p.Logs))
		}
		net := new(big.Int)
		for _, m := range moves {
			if m.From.IsZero() {
				net.Add(net, m.Amount)
			}
			if m.To.IsZero() {
				net.Sub(net, m.Amount)
			}
		}
		if got := sum(Deltas(moves)); got.Cmp(net) != 0 {
			t.Errorf("%s: deltas add to %s, mints less burns are %s", name, got, net)
		}
	}
}

func TestWrappedNativeMintsAndBurns(t *testing.T) {
	p := page(t, "monad_wmon")
	plain := movesOf(p.Logs, false)
	wrapped := movesOf(p.Logs, true)
	if len(plain) != 118 || len(wrapped) != 335 {
		t.Fatalf("%d transfers and %d moves, want 118 and 335", len(plain), len(wrapped))
	}
	deposits, withdrawals := new(big.Int), new(big.Int)
	for _, l := range p.Logs {
		v := new(big.Int).SetBytes(l.Data)
		switch l.Topics[0] {
		case evm.TopicDeposit:
			deposits.Add(deposits, v)
		case evm.TopicWithdrawal:
			withdrawals.Add(withdrawals, v)
		}
	}
	want := new(big.Int).Sub(deposits, withdrawals)
	if got := sum(Deltas(wrapped)); got.Cmp(want) != 0 {
		t.Errorf("deltas add to %s, deposits less withdrawals are %s", got, want)
	}
	for _, m := range wrapped {
		if !m.Transfer && !m.From.IsZero() && !m.To.IsZero() {
			t.Fatalf("a deposit or withdrawal names two holders: %+v", m)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	l := page(t, "monad_usdc").Logs[0]
	short := l
	short.Data = l.Data[:31]
	nft := l
	nft.Data = nil
	other := l
	other.Topics = []evm.Hash{evm.MustHash("0x8c5be1e5ebec7d5bd14f71427d1e84f3dd0314c0f7b2291e5b200ac8c7c3b925")}
	dirty := l
	dirty.Topics = append([]evm.Hash(nil), l.Topics...)
	dirty.Topics[1][0] = 1
	for name, x := range map[string]sources.Log{"short data": short, "no data": nft, "approval": other, "bad topic": dirty} {
		if _, ok := Decode(x, true); ok {
			t.Errorf("%s decoded", name)
		}
	}
	dep := page(t, "monad_wmon").Logs
	for _, x := range dep {
		if x.Topics[0] == evm.TopicDeposit {
			if _, ok := Decode(x, false); ok {
				t.Error("a deposit of a token that is no wrapped native coin decoded")
			}
			break
		}
	}
}

func TestTouched(t *testing.T) {
	moves := movesOf(page(t, "monad_wmon").Logs, true)
	got := Touched(moves)
	seen := map[evm.Address]bool{}
	for _, m := range moves {
		for _, a := range []evm.Address{m.From, m.To} {
			if !a.IsZero() {
				seen[a] = true
			}
		}
	}
	if len(got) != len(seen) {
		t.Errorf("%d touched, %d distinct", len(got), len(seen))
	}
	for i := 1; i < len(got); i++ {
		if got[i-1].String() >= got[i].String() {
			t.Fatal("touched is not sorted")
		}
	}
}

func TestLargest(t *testing.T) {
	p := page(t, "monad_usdc")
	var logs []sources.Log
	var moves []Move
	for _, l := range p.Logs {
		if m, ok := Decode(l, false); ok {
			logs, moves = append(logs, l), append(moves, m)
		}
	}
	day := time.Unix(int64(logs[0].Time), 0).UTC().Format(time.DateOnly)
	top := Largest(logs, moves, day, 5)
	if len(top) != 5 {
		t.Fatalf("%d transfers, want 5", len(top))
	}
	for i := 1; i < len(top); i++ {
		if top[i].Amount.Cmp(top[i-1].Amount) > 0 {
			t.Fatal("not largest first")
		}
	}
	for _, m := range moves {
		if m.Amount.Cmp(top[0].Amount) > 0 {
			t.Fatal("a larger transfer was left out")
		}
	}
	next := time.Unix(int64(logs[0].Time), 0).UTC().AddDate(0, 0, 1).Format(time.DateOnly)
	if got := Largest(logs, moves, next, 5); len(got) != 0 {
		t.Errorf("%d transfers before the cutoff day were kept", len(got))
	}
}

func TestCutoffDay(t *testing.T) {
	now := time.Date(2026, 10, 13, 23, 59, 0, 0, time.UTC)
	if got := CutoffDay(now, 7); got != "2026-10-07" {
		t.Errorf("got %s", got)
	}
	if got := CutoffDay(now, 1); got != "2026-10-13" {
		t.Errorf("got %s", got)
	}
}

func TestProject(t *testing.T) {
	if got := project(4, 100, 100, 10); got != 4 {
		t.Errorf("finished: %d", got)
	}
	if got := project(4, 0, 100, 0); got != 4 {
		t.Errorf("no span yet: %d", got)
	}
	if got := project(4, 0, 101, 10); got != 15 {
		t.Errorf("got %d, want 4 spent and 11 left", got)
	}
}
