package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"tokens/internal/evm"
)

// HolderMap is the stored links of one holder map, Links is a JSON document the holdermap package writes and reads
type HolderMap struct {
	Chain     string
	Token     evm.Address
	Limit     int
	BuiltAt   time.Time
	FromBlock uint64
	ToBlock   uint64
	Links     []byte
}

// HolderMap returns the stored map of a token and number of holders, false when none is stored
func (s *Store) HolderMap(ctx context.Context, chain string, token evm.Address, limit int) (HolderMap, bool, error) {
	out := HolderMap{Chain: chain, Token: token, Limit: limit}
	var (
		from, to int64
		links    string
	)
	err := s.Pool.QueryRow(ctx, `select built_at, window_from_block, window_to_block, links::text from holder_map
		where chain_id = $1 and token = $2 and "limit" = $3`, chain, token[:], limit).Scan(&out.BuiltAt, &from, &to, &links)
	if errors.Is(err, pgx.ErrNoRows) {
		return HolderMap{}, false, nil
	}
	if err != nil {
		return HolderMap{}, false, fmt.Errorf("read holder map: %w", err)
	}
	out.FromBlock, out.ToBlock, out.Links = u64(from), u64(to), []byte(links)
	return out, true, nil
}

// PutHolderMap stores a built map in place of the one before
func (s *Store) PutHolderMap(ctx context.Context, m HolderMap) error {
	from, err := i64(m.FromBlock)
	if err != nil {
		return err
	}
	to, err := i64(m.ToBlock)
	if err != nil {
		return err
	}
	_, err = s.Pool.Exec(ctx, `insert into holder_map (chain_id, token, "limit", built_at, window_from_block, window_to_block, links)
		values ($1, $2, $3, $4, $5, $6, $7::jsonb)
		on conflict (chain_id, token, "limit") do update set built_at = excluded.built_at,
			window_from_block = excluded.window_from_block, window_to_block = excluded.window_to_block, links = excluded.links`,
		m.Chain, m.Token[:], m.Limit, m.BuiltAt, from, to, string(m.Links))
	if err != nil {
		return fmt.Errorf("store holder map: %w", err)
	}
	return nil
}

// PruneHolderMaps deletes the maps built before the given time and returns how many it deleted
func (s *Store) PruneHolderMaps(ctx context.Context, before time.Time) (int64, error) {
	tag, err := s.Pool.Exec(ctx, `delete from holder_map where built_at < $1`, before)
	if err != nil {
		return 0, fmt.Errorf("prune holder maps: %w", err)
	}
	return tag.RowsAffected(), nil
}
