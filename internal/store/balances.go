package store

import (
	"context"
	"errors"
	"fmt"
	"math/big"
	"time"

	"github.com/jackc/pgx/v5"

	"tokens/internal/evm"
)

// Transfer is one large transfer kept for the recent window
type Transfer struct {
	Block    uint64
	LogIndex uint64
	Tx       evm.Hash
	Time     time.Time
	From     evm.Address
	To       evm.Address
	Amount   *big.Int
}

// Day is the UTC day of the transfer
func (t Transfer) Day() string { return t.Time.UTC().Format(time.DateOnly) }

// Batch is what one page of logs changes for one token, applied in one transaction
type Batch struct {
	TokenID int32
	// Deltas are added to the stored balances, the full mode
	Deltas map[evm.Address]*big.Int
	// Balances replace the stored balances, the activity mode reads them with balanceOf
	Balances  map[evm.Address]*big.Int
	Transfers []Transfer
	NextBlock uint64
	Requests  int
	// FromBlock guards the cursor: the batch applies only while the token still stands at this block
	FromBlock uint64
	// CutoffDay drops transfers of older days, TopPerDay keeps that many per day
	CutoffDay string
	TopPerDay int
}

// ErrMoved is a batch for a token whose cursor moved since the batch was read
var ErrMoved = errors.New("the token's cursor moved")

// Commit applies one batch, or nothing when the token is no longer at FromBlock
func (s *Store) Commit(ctx context.Context, b Batch) error {
	from, err := i64(b.FromBlock)
	if err != nil {
		return err
	}
	next, err := i64(b.NextBlock)
	if err != nil {
		return err
	}
	err = pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var at int64
		if err := tx.QueryRow(ctx, `select next_block from indexed_tokens where id = $1 for update`, b.TokenID).Scan(&at); err != nil {
			return err
		}
		if at != from {
			return ErrMoved
		}
		if err := addDeltas(ctx, tx, b.TokenID, b.Deltas); err != nil {
			return err
		}
		if err := setBalances(ctx, tx, b.TokenID, b.Balances); err != nil {
			return err
		}
		if err := keepTransfers(ctx, tx, b); err != nil {
			return err
		}
		_, err := tx.Exec(ctx, `update indexed_tokens set next_block = $2, requests = requests + $3,
			holders = (select count(*) from balances where token_id = $1) where id = $1`, b.TokenID, next, b.Requests)
		return err
	})
	if errors.Is(err, ErrMoved) {
		return ErrMoved
	}
	if err != nil {
		return fmt.Errorf("commit token %d: %w", b.TokenID, err)
	}
	return nil
}

func split(m map[evm.Address]*big.Int) (holders [][]byte, values []string, zeros [][]byte) {
	for a, v := range m {
		if v.Sign() == 0 {
			zeros = append(zeros, a[:])
			continue
		}
		holders = append(holders, a[:])
		values = append(values, v.String())
	}
	return holders, values, zeros
}

func addDeltas(ctx context.Context, tx pgx.Tx, id int32, deltas map[evm.Address]*big.Int) error {
	holders, values, _ := split(deltas)
	if len(holders) == 0 {
		return nil
	}
	if _, err := tx.Exec(ctx, `insert into balances (token_id, holder, balance)
		select $1, h, d from unnest($2::bytea[], $3::numeric[]) as t (h, d)
		on conflict (token_id, holder) do update set balance = balances.balance + excluded.balance`, id, holders, values); err != nil {
		return fmt.Errorf("add balances: %w", err)
	}
	if _, err := tx.Exec(ctx, `delete from balances where token_id = $1 and holder = any($2::bytea[]) and balance = 0`, id, holders); err != nil {
		return fmt.Errorf("drop empty balances: %w", err)
	}
	return nil
}

func setBalances(ctx context.Context, tx pgx.Tx, id int32, balances map[evm.Address]*big.Int) error {
	holders, values, zeros := split(balances)
	if len(holders) > 0 {
		if _, err := tx.Exec(ctx, `insert into balances (token_id, holder, balance)
			select $1, h, d from unnest($2::bytea[], $3::numeric[]) as t (h, d)
			on conflict (token_id, holder) do update set balance = excluded.balance`, id, holders, values); err != nil {
			return fmt.Errorf("set balances: %w", err)
		}
	}
	if len(zeros) > 0 {
		if _, err := tx.Exec(ctx, `delete from balances where token_id = $1 and holder = any($2::bytea[])`, id, zeros); err != nil {
			return fmt.Errorf("drop empty balances: %w", err)
		}
	}
	return nil
}

func keepTransfers(ctx context.Context, tx pgx.Tx, b Batch) error {
	if len(b.Transfers) > 0 {
		var (
			days, amounts           []string
			blocks, indexes         []int64
			txs, senders, receivers [][]byte
			times                   []time.Time
		)
		for _, t := range b.Transfers {
			blk, err := i64(t.Block)
			if err != nil {
				return err
			}
			idx, err := i64(t.LogIndex)
			if err != nil {
				return err
			}
			days, amounts = append(days, t.Day()), append(amounts, t.Amount.String())
			blocks, indexes, times = append(blocks, blk), append(indexes, idx), append(times, t.Time)
			txs, senders, receivers = append(txs, t.Tx[:]), append(senders, t.From[:]), append(receivers, t.To[:])
		}
		if _, err := tx.Exec(ctx, `insert into large_transfers (token_id, day, block, log_index, tx, ts, sender, recipient, amount)
			select $1, d, b, i, x, t, s, r, a from unnest($2::date[], $3::bigint[], $4::int[], $5::bytea[], $6::timestamptz[], $7::bytea[], $8::bytea[], $9::numeric[])
			as u (d, b, i, x, t, s, r, a) on conflict do nothing`,
			b.TokenID, days, blocks, indexes, txs, times, senders, receivers, amounts); err != nil {
			return fmt.Errorf("keep transfers: %w", err)
		}
		if _, err := tx.Exec(ctx, `delete from large_transfers lt using (
				select block, log_index from (
					select block, log_index, row_number() over (partition by day order by amount desc, block desc, log_index desc) as rn
					from large_transfers where token_id = $1 and day = any($2::date[])
				) r where rn > $3
			) d where lt.token_id = $1 and lt.block = d.block and lt.log_index = d.log_index`, b.TokenID, days, b.TopPerDay); err != nil {
			return fmt.Errorf("trim transfers: %w", err)
		}
	}
	if b.CutoffDay != "" {
		if _, err := tx.Exec(ctx, `delete from large_transfers where token_id = $1 and day < $2::date`, b.TokenID, b.CutoffDay); err != nil {
			return fmt.Errorf("drop old transfers: %w", err)
		}
	}
	return nil
}

// Balance is one holder of a token
type Balance struct {
	Holder  evm.Address
	Balance *big.Int
}

// HolderCursor is the last row of a page, the next page starts after it
type HolderCursor struct {
	Balance *big.Int
	Holder  evm.Address
}

// Holders pages the holders by balance, largest first and ties by address
func (s *Store) Holders(ctx context.Context, id int32, limit int, after *HolderCursor) ([]Balance, error) {
	var (
		rows pgx.Rows
		err  error
	)
	if after == nil {
		// the text cast gets its own name, so the order is numeric and not by the text
		rows, err = s.Pool.Query(ctx, `select holder, balance::text as amount from balances where token_id = $1
			order by balances.balance desc, holder limit $2`, id, limit)
	} else {
		rows, err = s.Pool.Query(ctx, `select holder, balance::text as amount from balances where token_id = $1
			and (balance < $2::numeric or (balance = $2::numeric and holder > $3))
			order by balances.balance desc, holder limit $4`, id, after.Balance.String(), after.Holder[:], limit)
	}
	if err != nil {
		return nil, fmt.Errorf("holders: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Balance, error) {
		var (
			h []byte
			v string
		)
		if err := r.Scan(&h, &v); err != nil {
			return Balance{}, err
		}
		n, ok := new(big.Int).SetString(v, 10)
		if !ok || len(h) != 20 {
			return Balance{}, errors.New("stored balance is malformed")
		}
		return Balance{Holder: evm.Address(h), Balance: n}, nil
	})
	if err != nil {
		return nil, fmt.Errorf("holders: %w", err)
	}
	return out, nil
}

// TopTransfers returns the largest kept transfers from fromDay on
func (s *Store) TopTransfers(ctx context.Context, id int32, fromDay string, limit int) ([]Transfer, error) {
	rows, err := s.Pool.Query(ctx, `select block, log_index, tx, ts, sender, recipient, amount::text as value from large_transfers
		where token_id = $1 and day >= $2::date order by large_transfers.amount desc, block desc, log_index desc limit $3`, id, fromDay, limit)
	if err != nil {
		return nil, fmt.Errorf("transfers: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Transfer, error) {
		var (
			blk, idx       int64
			tx, from, to   []byte
			ts             time.Time
			amount         string
			t              Transfer
			ok             bool
			badLengthOrNum = errors.New("stored transfer is malformed")
		)
		if err := r.Scan(&blk, &idx, &tx, &ts, &from, &to, &amount); err != nil {
			return Transfer{}, err
		}
		if len(tx) != 32 || len(from) != 20 || len(to) != 20 {
			return Transfer{}, badLengthOrNum
		}
		if t.Amount, ok = new(big.Int).SetString(amount, 10); !ok {
			return Transfer{}, badLengthOrNum
		}
		t.Block, t.LogIndex, t.Tx, t.Time = u64(blk), u64(idx), evm.Hash(tx), ts.UTC()
		t.From, t.To = evm.Address(from), evm.Address(to)
		return t, nil
	})
	if err != nil {
		return nil, fmt.Errorf("transfers: %w", err)
	}
	return out, nil
}

// PruneTransfers drops the transfers of days before cutoffDay for every token
func (s *Store) PruneTransfers(ctx context.Context, cutoffDay string) error {
	if _, err := s.Pool.Exec(ctx, `delete from large_transfers where day < $1::date`, cutoffDay); err != nil {
		return fmt.Errorf("prune transfers: %w", err)
	}
	return nil
}
