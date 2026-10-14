package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"
)

// ErrStatsMoved is a page that no longer starts at the chain's cursor, because another writer counted it first
var ErrStatsMoved = errors.New("the chain stats cursor moved")

// StatsCursor is where the count of a chain stands, First is the time of the first block it counted
type StatsCursor struct {
	Next  uint64
	First time.Time
}

// StatsDay is the count of one chain and UTC day, the gas figures as decimal strings
type StatsDay struct {
	Day         string
	Blocks      uint64
	Txs         uint64
	GasUsed     string
	GasLimit    string
	GasPriceWei string
}

// StatsCursor returns where the count of a chain stands, false before its first page
func (s *Store) StatsCursor(ctx context.Context, chain string) (StatsCursor, bool, error) {
	var (
		next  int64
		first time.Time
	)
	err := s.Pool.QueryRow(ctx, `select next_block, first_time from chain_stats_cursor where chain_id = $1`, chain).Scan(&next, &first)
	if errors.Is(err, pgx.ErrNoRows) {
		return StatsCursor{}, false, nil
	}
	if err != nil {
		return StatsCursor{}, false, fmt.Errorf("read chain stats cursor: %w", err)
	}
	return StatsCursor{Next: u64(next), First: first.UTC()}, true, nil
}

// AddStats adds the days of a page that began at from and moves the cursor to next, a chain's first page also records first, the time of block from
func (s *Store) AddStats(ctx context.Context, chain string, from, next uint64, first time.Time, days []StatsDay) error {
	fromI, err := i64(from)
	if err != nil {
		return err
	}
	nextI, err := i64(next)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		var at int64
		err := tx.QueryRow(ctx, `select next_block from chain_stats_cursor where chain_id = $1 for update`, chain).Scan(&at)
		switch {
		case errors.Is(err, pgx.ErrNoRows):
			_, err = tx.Exec(ctx, `insert into chain_stats_cursor (chain_id, next_block, first_time, updated_at) values ($1, $2, $3, now())`,
				chain, nextI, first.UTC())
		case err != nil:
		case at != fromI:
			return fmt.Errorf("%w: it is at %d, the page began at %d", ErrStatsMoved, at, from)
		default:
			_, err = tx.Exec(ctx, `update chain_stats_cursor set next_block = $2, updated_at = now() where chain_id = $1`, chain, nextI)
		}
		if err != nil {
			return fmt.Errorf("move chain stats cursor: %w", err)
		}
		for _, d := range days {
			blocks, err := i64(d.Blocks)
			if err != nil {
				return err
			}
			txs, err := i64(d.Txs)
			if err != nil {
				return err
			}
			_, err = tx.Exec(ctx, `insert into chain_stats_day (chain_id, day, blocks, gas_used, gas_limit, txs, gas_price_wei)
				values ($1, $2::date, $3, $4::numeric, $5::numeric, $6, $7::numeric)
				on conflict (chain_id, day) do update set blocks = chain_stats_day.blocks + excluded.blocks,
					gas_used = chain_stats_day.gas_used + excluded.gas_used, gas_limit = chain_stats_day.gas_limit + excluded.gas_limit,
					txs = chain_stats_day.txs + excluded.txs, gas_price_wei = chain_stats_day.gas_price_wei + excluded.gas_price_wei`,
				chain, d.Day, blocks, d.GasUsed, d.GasLimit, txs, d.GasPriceWei)
			if err != nil {
				return fmt.Errorf("add chain stats of %s: %w", d.Day, err)
			}
		}
		return nil
	})
}

// StatsDays returns a chain's counted days from the day from to the day to in order, without the day of the first block counted, which began before the count did
func (s *Store) StatsDays(ctx context.Context, chain, from, to string) ([]StatsDay, error) {
	rows, err := s.Pool.Query(ctx, `select to_char(d.day, 'YYYY-MM-DD'), d.blocks, d.txs, d.gas_used::text, d.gas_limit::text, d.gas_price_wei::text
		from chain_stats_day d join chain_stats_cursor c on c.chain_id = d.chain_id
		where d.chain_id = $1 and d.day between $2::date and $3::date and d.day > (c.first_time at time zone 'UTC')::date
		order by d.day`, chain, from, to)
	if err != nil {
		return nil, fmt.Errorf("read chain stats: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (StatsDay, error) {
		var (
			d           StatsDay
			blocks, txs int64
		)
		err := r.Scan(&d.Day, &blocks, &txs, &d.GasUsed, &d.GasLimit, &d.GasPriceWei)
		d.Blocks, d.Txs = u64(blocks), u64(txs)
		return d, err
	})
	if err != nil {
		return nil, fmt.Errorf("read chain stats: %w", err)
	}
	return out, nil
}
