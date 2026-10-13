package store

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5"

	"tokens/internal/evm"
)

const (
	StateQueued   = "queued"
	StateIndexing = "indexing"
	StateReady    = "ready"
	StateFailed   = "failed"

	ModeFull     = "full"
	ModeActivity = "activity"
)

// ErrFull is a new token past the configured number of indexed tokens
var ErrFull = errors.New("the index holds the most tokens it may")

type Token struct {
	ID          int32
	Chain       string
	Token       evm.Address
	State       string
	Mode        string
	Decimals    uint8
	StartBlock  uint64
	NextBlock   uint64
	Holders     int64
	Requests    int
	RequestedAt time.Time
	StartedAt   *time.Time
	ReadyAt     *time.Time
	BackfillS   *float64
	LastError   string
}

const tokenCols = `id, chain, token, state, mode, decimals, start_block, next_block, holders, requests, requested_at, started_at, ready_at, backfill_s, coalesce(last_error, '')`

func scanToken(row pgx.Row) (Token, error) {
	var (
		t          Token
		addr       []byte
		dec        int16
		start, nxt int64
	)
	err := row.Scan(&t.ID, &t.Chain, &addr, &t.State, &t.Mode, &dec, &start, &nxt, &t.Holders, &t.Requests, &t.RequestedAt,
		&t.StartedAt, &t.ReadyAt, &t.BackfillS, &t.LastError)
	if err != nil {
		return Token{}, err
	}
	if len(addr) != len(t.Token) {
		return Token{}, errors.New("stored token address is not 20 bytes")
	}
	t.Token = evm.Address(addr)
	t.Decimals, t.StartBlock, t.NextBlock = uint8(dec), u64(start), u64(nxt)
	return t, nil
}

// GetToken returns the row of a token, false when it is not indexed
func (s *Store) GetToken(ctx context.Context, chain string, token evm.Address) (Token, bool, error) {
	t, err := scanToken(s.Pool.QueryRow(ctx, `select `+tokenCols+` from indexed_tokens where chain = $1 and token = $2`, chain, token[:]))
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("read token: %w", err)
	}
	return t, true, nil
}

// EnsureToken queues a token unless it is indexed already, and answers ErrFull when maxTokens rows exist
func (s *Store) EnsureToken(ctx context.Context, chain string, token evm.Address, decimals uint8, maxTokens int) (Token, bool, error) {
	var (
		out     Token
		created bool
	)
	err := pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if _, err := tx.Exec(ctx, `select pg_advisory_xact_lock(hashtext('tokens_ensure:' || current_schema()))`); err != nil {
			return err
		}
		t, err := scanToken(tx.QueryRow(ctx, `select `+tokenCols+` from indexed_tokens where chain = $1 and token = $2`, chain, token[:]))
		if err == nil {
			out = t
			return nil
		}
		if !errors.Is(err, pgx.ErrNoRows) {
			return err
		}
		var n int
		if err := tx.QueryRow(ctx, `select count(*) from indexed_tokens where chain = $1`, chain).Scan(&n); err != nil {
			return err
		}
		if n >= maxTokens {
			return ErrFull
		}
		out, err = scanToken(tx.QueryRow(ctx, `insert into indexed_tokens (chain, token, state, decimals) values ($1, $2, 'queued', $3) returning `+tokenCols,
			chain, token[:], int16(decimals)))
		created = err == nil
		return err
	})
	if errors.Is(err, ErrFull) {
		return Token{}, false, ErrFull
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("ensure token: %w", err)
	}
	return out, created, nil
}

// NextJob picks a backfill to run: an interrupted one first, then the oldest queued token, then a failed one due for a retry
func (s *Store) NextJob(ctx context.Context, chain string, retryFailedAfter time.Duration) (Token, bool, error) {
	t, err := scanToken(s.Pool.QueryRow(ctx, `select `+tokenCols+` from indexed_tokens
		where chain = $1 and (state in ('indexing', 'queued') or (state = 'failed' and requested_at < now() - make_interval(secs => $2)))
		order by case state when 'indexing' then 0 when 'queued' then 1 else 2 end, requested_at, id limit 1`,
		chain, retryFailedAfter.Seconds()))
	if errors.Is(err, pgx.ErrNoRows) {
		return Token{}, false, nil
	}
	if err != nil {
		return Token{}, false, fmt.Errorf("next job: %w", err)
	}
	return t, true, nil
}

// StartBackfill marks a token as indexing in the given mode, and starts it over at start when reset is set
func (s *Store) StartBackfill(ctx context.Context, id int32, mode string, start uint64, reset bool) error {
	from, err := i64(start)
	if err != nil {
		return err
	}
	return pgx.BeginFunc(ctx, s.Pool, func(tx pgx.Tx) error {
		if reset {
			if _, err := tx.Exec(ctx, `delete from balances where token_id = $1`, id); err != nil {
				return fmt.Errorf("reset balances: %w", err)
			}
			if _, err := tx.Exec(ctx, `delete from large_transfers where token_id = $1`, id); err != nil {
				return fmt.Errorf("reset transfers: %w", err)
			}
			_, err := tx.Exec(ctx, `update indexed_tokens set state = 'indexing', mode = $2, start_block = $3, next_block = $3, holders = 0,
				started_at = coalesce(started_at, now()), last_error = null where id = $1`, id, mode, from)
			return err
		}
		_, err := tx.Exec(ctx, `update indexed_tokens set state = 'indexing', started_at = coalesce(started_at, now()), last_error = null where id = $1`, id)
		return err
	})
}

// MarkReady ends a backfill, the time is from the first start to now
func (s *Store) MarkReady(ctx context.Context, id int32) error {
	_, err := s.Pool.Exec(ctx, `update indexed_tokens set state = 'ready', ready_at = now(),
		backfill_s = extract(epoch from now() - started_at), last_error = null where id = $1`, id)
	if err != nil {
		return fmt.Errorf("mark ready: %w", err)
	}
	return nil
}

// MarkFailed parks a token until a retry, requested_at restarts the wait
func (s *Store) MarkFailed(ctx context.Context, id int32, cause string) error {
	_, err := s.Pool.Exec(ctx, `update indexed_tokens set state = 'failed', requested_at = now(), last_error = left($2, 300) where id = $1`, id, cause)
	if err != nil {
		return fmt.Errorf("mark failed: %w", err)
	}
	return nil
}

// ReadyTokens lists the tokens the tail keeps current
func (s *Store) ReadyTokens(ctx context.Context, chain string) ([]Token, error) {
	rows, err := s.Pool.Query(ctx, `select `+tokenCols+` from indexed_tokens where chain = $1 and state = 'ready' order by id`, chain)
	if err != nil {
		return nil, fmt.Errorf("ready tokens: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Token, error) { return scanToken(r) })
	if err != nil {
		return nil, fmt.Errorf("ready tokens: %w", err)
	}
	return out, nil
}

// Tokens lists every indexed token, oldest first
func (s *Store) Tokens(ctx context.Context, chain string) ([]Token, error) {
	rows, err := s.Pool.Query(ctx, `select `+tokenCols+` from indexed_tokens where chain = $1 order by id`, chain)
	if err != nil {
		return nil, fmt.Errorf("tokens: %w", err)
	}
	out, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (Token, error) { return scanToken(r) })
	if err != nil {
		return nil, fmt.Errorf("tokens: %w", err)
	}
	return out, nil
}

// Sizes is the disk the index uses, tables with their indexes
type Sizes struct {
	Balances  int64
	Transfers int64
	Total     int64
}

func (s *Store) Sizes(ctx context.Context) (Sizes, error) {
	var out Sizes
	err := s.Pool.QueryRow(ctx, `select pg_total_relation_size('balances'), pg_total_relation_size('large_transfers'),
		pg_total_relation_size('balances') + pg_total_relation_size('large_transfers') + pg_total_relation_size('indexed_tokens')`).
		Scan(&out.Balances, &out.Transfers, &out.Total)
	if err != nil {
		return Sizes{}, fmt.Errorf("sizes: %w", err)
	}
	return out, nil
}

// IndexedTokens lists the tokens of a chain the index holds, oldest first
func (s *Store) IndexedTokens(ctx context.Context, chain string) ([]evm.Address, error) {
	rows, err := s.Pool.Query(ctx, `select token from indexed_tokens where chain = $1 order by id`, chain)
	if err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	defer rows.Close()
	var out []evm.Address
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, fmt.Errorf("list tokens: %w", err)
		}
		if len(b) != len(evm.Address{}) {
			return nil, errors.New("stored token address is not 20 bytes")
		}
		out = append(out, evm.Address(b))
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("list tokens: %w", err)
	}
	return out, nil
}
