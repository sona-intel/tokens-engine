// Package index keeps the Monad holder index: a backfill per token from HyperSync, then one tail for every ready token
package index

import (
	"cmp"
	"math/big"
	"slices"
	"time"

	"tokens/internal/evm"
	"tokens/internal/sources"
	"tokens/internal/store"
)

// Move is one balance change read from a log, From or To is zero for a mint or a burn
type Move struct {
	From, To evm.Address
	Amount   *big.Int
	// Transfer is false for the Deposit and Withdrawal of a wrapped native coin
	Transfer bool
}

// Decode reads a Transfer, or a Deposit or Withdrawal when wrapped is set, and false for anything else
func Decode(l sources.Log, wrapped bool) (Move, bool) {
	if len(l.Topics) == 0 {
		return Move{}, false
	}
	switch l.Topics[0] {
	case evm.TopicTransfer:
		switch {
		case len(l.Topics) >= 3 && len(l.Data) == 32:
			from, ok1 := evm.TopicAddress(l.Topics[1])
			to, ok2 := evm.TopicAddress(l.Topics[2])
			if !ok1 || !ok2 {
				return Move{}, false
			}
			return Move{From: from, To: to, Amount: new(big.Int).SetBytes(l.Data), Transfer: true}, true
		case len(l.Topics) == 1 && len(l.Data) == 96:
			// a token that indexes neither address keeps all three values in the data
			from, ok1 := evm.TopicAddress(evm.Hash(l.Data[:32]))
			to, ok2 := evm.TopicAddress(evm.Hash(l.Data[32:64]))
			if !ok1 || !ok2 {
				return Move{}, false
			}
			return Move{From: from, To: to, Amount: new(big.Int).SetBytes(l.Data[64:]), Transfer: true}, true
		}
	case evm.TopicDeposit, evm.TopicWithdrawal:
		if !wrapped || len(l.Topics) < 2 || len(l.Data) != 32 {
			return Move{}, false
		}
		who, ok := evm.TopicAddress(l.Topics[1])
		if !ok {
			return Move{}, false
		}
		amount := new(big.Int).SetBytes(l.Data)
		if l.Topics[0] == evm.TopicDeposit {
			return Move{To: who, Amount: amount}, true
		}
		return Move{From: who, Amount: amount}, true
	}
	return Move{}, false
}

// Topics are the topic0 values a token's index reads
func Topics(wrapped bool) []evm.Hash {
	if wrapped {
		return []evm.Hash{evm.TopicTransfer, evm.TopicDeposit, evm.TopicWithdrawal}
	}
	return []evm.Hash{evm.TopicTransfer}
}

// Deltas adds up the balance changes of the moves, the zero address is not a holder
func Deltas(moves []Move) map[evm.Address]*big.Int {
	out := map[evm.Address]*big.Int{}
	add := func(a evm.Address, v *big.Int, sign int) {
		if a.IsZero() || v.Sign() == 0 {
			return
		}
		d, ok := out[a]
		if !ok {
			d = new(big.Int)
			out[a] = d
		}
		if sign > 0 {
			d.Add(d, v)
		} else {
			d.Sub(d, v)
		}
	}
	for _, m := range moves {
		add(m.From, m.Amount, -1)
		add(m.To, m.Amount, +1)
	}
	return out
}

// Touched is every non-zero address that a move names
func Touched(moves []Move) []evm.Address {
	seen := map[evm.Address]bool{}
	var out []evm.Address
	for _, m := range moves {
		for _, a := range []evm.Address{m.From, m.To} {
			if !a.IsZero() && !seen[a] {
				seen[a] = true
				out = append(out, a)
			}
		}
	}
	slices.SortFunc(out, func(a, b evm.Address) int { return cmp.Compare(a.String(), b.String()) })
	return out
}

// Largest keeps, per UTC day from cutoffDay on, the perDay largest transfers of the logs
func Largest(logs []sources.Log, moves []Move, cutoffDay string, perDay int) []store.Transfer {
	byDay := map[string][]store.Transfer{}
	for i, m := range moves {
		l := logs[i]
		if !m.Transfer || m.Amount.Sign() == 0 || l.Time == 0 {
			continue
		}
		t := store.Transfer{
			Block: l.Block, LogIndex: l.LogIndex, Tx: l.TxHash, Time: time.Unix(int64(l.Time), 0).UTC(),
			From: m.From, To: m.To, Amount: m.Amount,
		}
		if day := t.Day(); day >= cutoffDay {
			byDay[day] = append(byDay[day], t)
		}
	}
	var out []store.Transfer
	for _, ts := range byDay {
		slices.SortFunc(ts, CompareTransfers)
		out = append(out, ts[:min(len(ts), perDay)]...)
	}
	slices.SortFunc(out, CompareTransfers)
	return out
}

// CompareTransfers orders the largest first, then the newest
func CompareTransfers(a, b store.Transfer) int {
	return cmp.Or(b.Amount.Cmp(a.Amount), cmp.Compare(b.Block, a.Block), cmp.Compare(b.LogIndex, a.LogIndex))
}

// CutoffDay is the first UTC day of a window of days that ends today
func CutoffDay(now time.Time, days int) string {
	return now.UTC().AddDate(0, 0, -(days - 1)).Format(time.DateOnly)
}
