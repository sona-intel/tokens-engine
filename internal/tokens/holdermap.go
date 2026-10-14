package tokens

import (
	"context"
	"math/big"
	"slices"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/holdermap"
	"tokens/internal/store"
)

// HolderMaps answers the links of a map from the stored builds and queues the missing ones, it never waits for HyperSync
type HolderMaps interface {
	Links(ctx context.Context, key holdermap.Key, nodes []evm.Address) (holdermap.State, error)
	WindowDays() int
}

const (
	NodeContract = "contract"
	NodeWallet   = "wallet"
	NodeUnknown  = "unknown"
)

// MapNode is one holder of the map
type MapNode struct {
	Address string   `json:"address"`
	Balance string   `json:"balance"`
	Share   *float64 `json:"share"`
	Label   *string  `json:"label"`
	Kind    string   `json:"kind"`
	Cluster *int     `json:"cluster"`
}

// MapLink is the transfers between two nodes in both directions, From is the lower address
type MapLink struct {
	From      string `json:"from"`
	To        string `json:"to"`
	Transfers int64  `json:"transfers"`
	Amount    string `json:"amount"`
}

type MapLinks struct {
	Status     string    `json:"status"`
	WindowDays float64   `json:"window_days"`
	BuiltAt    *string   `json:"built_at"`
	Items      []MapLink `json:"items"`
}

type MapCluster struct {
	ID    int     `json:"id"`
	Size  int     `json:"size"`
	Share float64 `json:"share"`
}

// HolderMap is the holder_map record
type HolderMap struct {
	Kind     string       `json:"kind"`
	Chain    string       `json:"chain"`
	Token    string       `json:"token"`
	Nodes    []MapNode    `json:"nodes"`
	Links    MapLinks     `json:"links"`
	Clusters []MapCluster `json:"clusters"`
	Coverage []string     `json:"coverage"`
}

// HolderMap answers the largest holders of a token with the links between them that a background build found
func (s *Service) HolderMap(ctx context.Context, chain string, token evm.Address, limit int) (HolderMap, error) {
	cfg, client, err := s.chain(chain)
	if err != nil {
		return HolderMap{}, err
	}
	meta, err := s.Meta(ctx, chain, token)
	if err != nil {
		return HolderMap{}, err
	}
	out := HolderMap{
		Kind: "holder_map", Chain: chain, Token: token.String(), Nodes: []MapNode{}, Clusters: []MapCluster{},
		Links: MapLinks{Status: holdermap.StatusUnavailable, Items: []MapLink{}},
	}
	if s.d.HolderMaps != nil {
		out.Links.WindowDays = float64(s.d.HolderMaps.WindowDays())
	}
	var (
		holders []Holder
		raws    []*big.Int
		notes   []Note
	)
	if cfg.Holders == config.HoldersBlockscout {
		if holders, _, err = s.blockscoutList(ctx, token, meta, limit, cursor{}); err != nil {
			return HolderMap{}, err
		}
		notes = append(notes, CovHoldersPartial)
	} else {
		if holders, err = s.fullIndexHolders(ctx, cfg, client, token, meta, limit, &notes); err != nil {
			return HolderMap{}, err
		}
		notes = append(notes, CovMapLinksEthereumOnly)
	}
	addrs := make([]evm.Address, len(holders))
	for i, h := range holders {
		addrs[i] = evm.MustAddress(h.Address)
		raw, _ := new(big.Int).SetString(h.Balance.Raw, 10)
		raws = append(raws, raw)
		kind := NodeUnknown
		if h.IsContract != nil {
			kind = NodeWallet
			if *h.IsContract {
				kind = NodeContract
			}
		}
		out.Nodes = append(out.Nodes, MapNode{Address: h.Address, Balance: h.Balance.Value, Share: h.Share, Label: h.Label, Kind: kind})
	}
	if cfg.Holders == config.HoldersBlockscout {
		if err := s.mapLinks(ctx, &out, holdermap.Key{Chain: chain, Token: token, Limit: limit}, addrs, raws, meta, &notes); err != nil {
			return HolderMap{}, err
		}
	}
	out.Coverage = codes(ordered(notes)...)
	return out, nil
}

// fullIndexHolders is the index's list when the index of the token is complete, nil otherwise; the call queues a new token like the holder route
func (s *Service) fullIndexHolders(ctx context.Context, cfg config.Chain, client Chain, token evm.Address, meta Meta, limit int, notes *[]Note) ([]Holder, error) {
	t, state, err := s.ensureIndexed(ctx, cfg.ID, token, meta.Decimals)
	if err != nil {
		return nil, err
	}
	if t == nil || state.State != IndexReady || t.Mode != store.ModeFull {
		if state.State == IndexUnavailable {
			*notes = append(*notes, CovIndexFull)
		} else {
			*notes = append(*notes, CovMapIndexIncomplete)
		}
		return nil, nil
	}
	*notes = append(*notes, CovFromTransfers)
	rows, err := s.d.Store.Holders(ctx, t.ID, limit, nil)
	if err != nil {
		return nil, err
	}
	if cfg.NativeToken(token) != nil {
		*notes = append(*notes, CovNativeCoin)
		if rows, err = s.nativeBalances(ctx, cfg, client, token, meta.Decimals, rows); err != nil {
			return nil, err
		}
	}
	out := make([]Holder, 0, len(rows))
	for _, r := range rows {
		out = append(out, Holder{Address: r.Holder.String(), Balance: NewAmount(r.Balance, meta.Decimals), Share: ratio(r.Balance, meta.TotalSupply)})
	}
	return out, nil
}

// mapLinks fills the links and clusters of an Ethereum map
func (s *Service) mapLinks(ctx context.Context, out *HolderMap, key holdermap.Key, addrs []evm.Address, raws []*big.Int, meta Meta, notes *[]Note) error {
	if s.d.HolderMaps == nil {
		return nil
	}
	// fewer than two holders have no pair to link, so nothing is read
	if len(addrs) < 2 {
		out.Links.Status = holdermap.StatusReady
		at := stamp(s.now())
		out.Links.BuiltAt = &at
		return nil
	}
	st, err := s.d.HolderMaps.Links(ctx, key, addrs)
	if err != nil {
		return err
	}
	out.Links.Status = st.Status
	switch st.Status {
	case holdermap.StatusUnavailable:
		if st.Reason == holdermap.ReasonQueueFull {
			*notes = append(*notes, CovMapQueueFull)
		} else {
			*notes = append(*notes, CovMapLinksFailed)
		}
		return nil
	case holdermap.StatusBuilding:
		return nil
	}
	at := stamp(st.BuiltAt)
	out.Links.BuiltAt = &at
	out.Links.WindowDays = st.Doc.WindowDays
	if st.Doc.Cut {
		*notes = append(*notes, CovMapWindowCut)
	}
	pos := make(map[evm.Address]int, len(addrs))
	for i, a := range addrs {
		pos[a] = i
	}
	var edges [][2]int
	for _, p := range st.Doc.Pairs {
		a, okA := pos[p.A]
		b, okB := pos[p.B]
		// a holder that left the list since the build has no node to link
		if !okA || !okB {
			continue
		}
		amount, _ := new(big.Int).SetString(p.Raw, 10)
		out.Links.Items = append(out.Links.Items, MapLink{From: p.A.String(), To: p.B.String(), Transfers: p.Transfers, Amount: FormatUnits(amount, meta.Decimals)})
		edges = append(edges, [2]int{a, b})
	}
	type group struct {
		members []int
		share   float64
	}
	var groups []group
	for _, members := range holdermap.Components(len(addrs), edges) {
		sum := new(big.Int)
		for _, i := range members {
			sum.Add(sum, raws[i])
		}
		g := group{members: members}
		if r := ratio(sum, meta.TotalSupply); r != nil {
			g.share = *r
		}
		groups = append(groups, g)
	}
	slices.SortStableFunc(groups, func(x, y group) int {
		switch {
		case x.share > y.share:
			return -1
		case x.share < y.share:
			return 1
		}
		return len(y.members) - len(x.members)
	})
	for i, g := range groups {
		id := i + 1
		out.Clusters = append(out.Clusters, MapCluster{ID: id, Size: len(g.members), Share: g.share})
		for _, m := range g.members {
			out.Nodes[m].Cluster = &id
		}
	}
	return nil
}

// FirstBlockAt is the first block at or after t, for the holder map builds
func (s *Service) FirstBlockAt(ctx context.Context, chain string, t time.Time, hi uint64) (uint64, error) {
	_, client, err := s.chain(chain)
	if err != nil {
		return 0, err
	}
	return s.firstBlockAt(ctx, chain, client, t, hi)
}

// BlockTime is the time of a block, for the holder map builds
func (s *Service) BlockTime(ctx context.Context, chain string, n uint64) (time.Time, error) {
	_, client, err := s.chain(chain)
	if err != nil {
		return time.Time{}, err
	}
	h, err := s.header(ctx, chain, client, n)
	return h.Time, err
}
