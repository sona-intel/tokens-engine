package tokens

import (
	"context"
	"encoding/base64"
	"encoding/json/v2"
	"errors"
	"math/big"
	"net/url"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/sources"
	"tokens/internal/store"
)

const maxCursor = 512

/* Cursors */

// cursor is opaque to callers: base64url of a small JSON object
type cursor struct {
	// Page and Offset walk Blockscout's pages
	Page   map[string]string `json:"p,omitzero"`
	Offset int               `json:"o,omitzero"`
	// Balance and Holder are the last row of an index page
	Balance string `json:"b,omitzero"`
	Holder  string `json:"h,omitzero"`
}

func (c cursor) encode() *string {
	b, _ := json.Marshal(c)
	s := base64.RawURLEncoding.EncodeToString(b)
	return &s
}

func decodeCursor(s string) (cursor, error) {
	if s == "" {
		return cursor{}, nil
	}
	if len(s) > maxCursor {
		return cursor{}, ErrBadCursor
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return cursor{}, ErrBadCursor
	}
	var c cursor
	if err := json.Unmarshal(raw, &c, json.RejectUnknownMembers(true)); err != nil || c.Offset < 0 || c.Offset > sources.BlockscoutPage {
		return cursor{}, ErrBadCursor
	}
	return c, nil
}

func (c cursor) values() url.Values {
	if len(c.Page) == 0 {
		return nil
	}
	v := url.Values{}
	for k, x := range c.Page {
		v.Set(k, x)
	}
	return v
}

func pageMap(v url.Values) map[string]string {
	if len(v) == 0 {
		return nil
	}
	m := make(map[string]string, len(v))
	for k := range v {
		m[k] = v.Get(k)
	}
	return m
}

/* Holders */

// Holders pages the holders of a token, largest first
func (s *Service) Holders(ctx context.Context, chain string, token evm.Address, limit int, cur string) (Holders, error) {
	cfg, client, err := s.chain(chain)
	if err != nil {
		return Holders{}, err
	}
	c, err := decodeCursor(cur)
	if err != nil {
		return Holders{}, err
	}
	meta, err := s.Meta(ctx, chain, token)
	if err != nil {
		return Holders{}, err
	}
	out := Holders{Kind: "token_holders", Chain: chain, Token: token.String(), Items: []Holder{}}
	if cfg.Holders == config.HoldersBlockscout {
		if c.Balance != "" || c.Holder != "" {
			return Holders{}, ErrBadCursor
		}
		err = s.blockscoutPage(ctx, &out, token, meta, limit, c)
	} else {
		if len(c.Page) > 0 || c.Offset > 0 {
			return Holders{}, ErrBadCursor
		}
		err = s.indexPage(ctx, &out, cfg, client, token, meta, limit, c)
	}
	if err != nil {
		return Holders{}, err
	}
	return out, nil
}

func (s *Service) blockscoutPage(ctx context.Context, out *Holders, token evm.Address, meta Meta, limit int, c cursor) error {
	out.Source = SourceBlockscout
	notes := []Note{CovHoldersPartial}
	var err error
	if out.Items, out.NextCursor, err = s.blockscoutList(ctx, token, meta, limit, c); err != nil {
		return err
	}
	t, err := s.blockscoutToken(ctx, token)
	switch {
	case err == nil:
		out.HolderCount = t.HoldersCount
	case errors.Is(err, sources.ErrNotFound):
	case ctx.Err() != nil:
		return ctx.Err()
	default:
		notes = append(notes, CovSourceDown)
	}
	out.Coverage = codes(ordered(notes)...)
	return nil
}

// blockscoutList reads up to limit holders from the cursor on, across Blockscout's pages, and the cursor after them
func (s *Service) blockscoutList(ctx context.Context, token evm.Address, meta Meta, limit int, c cursor) ([]Holder, *string, error) {
	items := []Holder{}
	var next *string
	page, off := c.values(), c.Offset
	for len(items) < limit {
		p, err := s.blockscoutHolders(ctx, token, page)
		if errors.Is(err, sources.ErrNotFound) {
			break
		}
		if err != nil {
			return nil, nil, err
		}
		if off >= len(p.Items) {
			break
		}
		take := min(limit-len(items), len(p.Items)-off)
		for _, h := range p.Items[off : off+take] {
			item := Holder{Address: h.Address.String(), Balance: NewAmount(h.Value, meta.Decimals), Share: ratio(h.Value, meta.TotalSupply), IsContract: h.IsContract}
			if h.Label != "" {
				item.Label = &h.Label
			}
			items = append(items, item)
		}
		off += take
		switch {
		case off < len(p.Items):
			next = cursor{Page: pageMap(page), Offset: off}.encode()
		case p.Next != nil:
			next = cursor{Page: pageMap(p.Next)}.encode()
		default:
			next = nil
		}
		if next == nil || len(items) == limit {
			break
		}
		page, off = p.Next, 0
	}
	return items, next, nil
}

func (s *Service) indexPage(ctx context.Context, out *Holders, cfg config.Chain, client Chain, token evm.Address, meta Meta, limit int, c cursor) error {
	out.Source = SourceIndex
	var after *store.HolderCursor
	if c.Balance != "" || c.Holder != "" {
		b, ok := new(big.Int).SetString(c.Balance, 10)
		h, err := evm.ParseAddress(c.Holder)
		if !ok || err != nil {
			return ErrBadCursor
		}
		after = &store.HolderCursor{Balance: b, Holder: h}
	}
	t, state, err := s.ensureIndexed(ctx, cfg.ID, token, meta.Decimals)
	if err != nil {
		return err
	}
	out.Index = state
	var notes []Note
	switch state.State {
	case IndexUnavailable:
		notes = append(notes, CovIndexFull)
	case IndexQueued, IndexIndexing:
		notes = append(notes, CovIndexPartial)
	}
	if t != nil {
		if t.Mode == store.ModeActivity {
			notes = append(notes, CovFromActivity)
		} else {
			notes = append(notes, CovFromTransfers)
		}
		rows, err := s.d.Store.Holders(ctx, t.ID, limit+1, after)
		if err != nil {
			return err
		}
		if len(rows) > limit {
			last := rows[limit-1]
			out.NextCursor = cursor{Balance: last.Balance.String(), Holder: last.Holder.String()}.encode()
			rows = rows[:limit]
		}
		if cfg.NativeToken(token) != nil {
			notes = append(notes, CovNativeCoin)
			// the cursor above holds the stored balance, the order of the pages follows it
			if rows, err = s.nativeBalances(ctx, cfg, client, token, meta.Decimals, rows); err != nil {
				return err
			}
		}
		for _, r := range rows {
			out.Items = append(out.Items, Holder{Address: r.Holder.String(), Balance: NewAmount(r.Balance, meta.Decimals), Share: ratio(r.Balance, meta.TotalSupply)})
		}
		if t.State == store.StateReady && t.Mode == store.ModeFull {
			out.HolderCount = &t.Holders
		}
	}
	if out.HolderCount == nil {
		h, err := s.geckoHolders(ctx, cfg.GeckoNetwork, token)
		switch {
		case err == nil:
			if h.Count != nil {
				out.HolderCount = h.Count
				notes = append(notes, CovGeckoHolders)
			}
		case errors.Is(err, sources.ErrNotFound):
		case ctx.Err() != nil:
			return ctx.Err()
		default:
			notes = append(notes, CovSourceDown)
		}
	}
	out.Coverage = codes(ordered(notes)...)
	return nil
}

// ensureIndexed queues the token on its first request and describes its index, the row is nil when the index cannot take it
func (s *Service) ensureIndexed(ctx context.Context, chain string, token evm.Address, decimals uint8) (*store.Token, *IndexState, error) {
	idx := s.d.Indexes[chain]
	head := idx.Head(ctx)
	t, err := idx.Ensure(ctx, token, decimals)
	if errors.Is(err, store.ErrFull) {
		st := &IndexState{State: IndexUnavailable}
		if head > 0 {
			st.HeadBlock = &head
		}
		return nil, st, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &t, indexState(t, head), nil
}

func indexState(t store.Token, head uint64) *IndexState {
	st := &IndexState{}
	switch t.State {
	case store.StateReady:
		st.State = IndexReady
	case store.StateIndexing:
		st.State = IndexIndexing
	case store.StateQueued:
		st.State = IndexQueued
	default:
		st.State = IndexUnavailable
	}
	if t.State != store.StateQueued {
		mode, start := t.Mode, t.StartBlock
		st.Mode, st.StartBlock = &mode, &start
		if t.NextBlock > t.StartBlock {
			indexed := t.NextBlock - 1
			st.IndexedBlock = &indexed
		}
	}
	if head > 0 {
		st.HeadBlock = &head
	}
	return st
}
