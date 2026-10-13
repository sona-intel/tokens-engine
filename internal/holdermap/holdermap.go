// Package holdermap builds the links of holder maps: transfers of a token between its largest holders over a recent
// window, read from HyperSync in a background job and stored in Postgres
package holdermap

import (
	"bytes"
	"cmp"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"slices"

	"tokens/internal/evm"
	"tokens/internal/index"
	"tokens/internal/sources"
)

// Key names one map: a token and the number of holders it shows
type Key struct {
	Chain string
	Token evm.Address
	Limit int
}

func (k Key) String() string { return fmt.Sprintf("%s/%s/%d", k.Chain, k.Token, k.Limit) }

// Pair is the transfers between two holders in both directions, A is the lower address
type Pair struct {
	A         evm.Address `json:"a"`
	B         evm.Address `json:"b"`
	Transfers int64       `json:"transfers"`
	// Raw is the summed amount as the contract's integer, in decimal
	Raw string `json:"raw"`
}

// Doc is the stored links of one map
type Doc struct {
	// WindowDays is the days the read covered, the configured window unless Cut
	WindowDays float64 `json:"window_days"`
	// Cut is set when the request budget ran out before the window was read
	Cut bool `json:"cut"`
	// Requests is the HyperSync requests the build spent
	Requests int `json:"requests"`
	// Nodes is the number of holders whose transfers were read
	Nodes int    `json:"nodes"`
	Pairs []Pair `json:"pairs"`
}

func (d Doc) Marshal() ([]byte, error) { return json.Marshal(d) }

func ParseDoc(raw []byte) (Doc, error) {
	var d Doc
	if err := json.Unmarshal(raw, &d); err != nil {
		return Doc{}, fmt.Errorf("holder map links: %w", err)
	}
	for _, p := range d.Pairs {
		if _, ok := new(big.Int).SetString(p.Raw, 10); !ok {
			return Doc{}, errors.New("holder map links: an amount is not an integer")
		}
	}
	return d, nil
}

// Topic is an address as an indexed topic, 12 zero bytes and the address
func Topic(a evm.Address) evm.Hash {
	var h evm.Hash
	copy(h[12:], a[:])
	return h
}

type pairKey struct{ a, b evm.Address }

type sum struct {
	n   int64
	raw *big.Int
}

// pairs adds up the transfers between nodes per unordered pair
type pairs struct {
	token evm.Address
	nodes map[evm.Address]bool
	sums  map[pairKey]*sum
}

func newPairs(token evm.Address, nodes []evm.Address) *pairs {
	p := &pairs{token: token, nodes: make(map[evm.Address]bool, len(nodes)), sums: map[pairKey]*sum{}}
	for _, a := range nodes {
		p.nodes[a] = true
	}
	return p
}

// add counts the Transfer logs of the token whose sender and receiver are both nodes, without self transfers and the zero address
func (p *pairs) add(logs []sources.Log, from, to uint64) {
	for _, l := range logs {
		if l.Address != p.token || l.Block < from || l.Block >= to {
			continue
		}
		m, ok := index.Decode(l, false)
		if !ok || !m.Transfer || m.From == m.To || m.From.IsZero() || m.To.IsZero() || !p.nodes[m.From] || !p.nodes[m.To] {
			continue
		}
		k := pairKey{m.From, m.To}
		if bytes.Compare(k.a[:], k.b[:]) > 0 {
			k.a, k.b = k.b, k.a
		}
		s, ok := p.sums[k]
		if !ok {
			s = &sum{raw: new(big.Int)}
			p.sums[k] = s
		}
		s.n++
		s.raw.Add(s.raw, m.Amount)
	}
}

// list orders the pairs by amount, then transfers, then addresses
func (p *pairs) list() []Pair {
	type row struct {
		k pairKey
		s *sum
	}
	rows := make([]row, 0, len(p.sums))
	for k, s := range p.sums {
		rows = append(rows, row{k, s})
	}
	slices.SortFunc(rows, func(x, y row) int {
		return cmp.Or(y.s.raw.Cmp(x.s.raw), cmp.Compare(y.s.n, x.s.n), bytes.Compare(x.k.a[:], y.k.a[:]), bytes.Compare(x.k.b[:], y.k.b[:]))
	})
	out := make([]Pair, len(rows))
	for i, r := range rows {
		out[i] = Pair{A: r.k.a, B: r.k.b, Transfers: r.s.n, Raw: r.s.raw.String()}
	}
	return out
}

// Components groups the nodes that links connect, links are pairs of node indexes. Only groups of two or more
// nodes are returned, each with its members in ascending order, the groups in the order of their first member.
func Components(n int, links [][2]int) [][]int {
	parent := make([]int, n)
	for i := range parent {
		parent[i] = i
	}
	find := func(i int) int {
		for parent[i] != i {
			parent[i] = parent[parent[i]]
			i = parent[i]
		}
		return i
	}
	for _, l := range links {
		a, b := find(l[0]), find(l[1])
		if a == b {
			continue
		}
		if a > b {
			a, b = b, a
		}
		parent[b] = a
	}
	groups := map[int][]int{}
	var roots []int
	for i := range n {
		r := find(i)
		if _, ok := groups[r]; !ok {
			roots = append(roots, r)
		}
		groups[r] = append(groups[r], i)
	}
	var out [][]int
	for _, r := range roots {
		if len(groups[r]) >= 2 {
			out = append(out, groups[r])
		}
	}
	return out
}
