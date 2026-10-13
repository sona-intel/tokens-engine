package holdermap_test

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"math/big"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/fixture"
	"tokens/internal/holdermap"
	"tokens/internal/index"
	"tokens/internal/testkit"
)

func mapConfig() config.HolderMap {
	return config.HolderMap{WindowDays: 30, PageBudget: 8, FreshFor: 12 * time.Hour, KeepFor: 168 * time.Hour, QueueMax: 20, RetryFailedAfter: 10 * time.Minute}
}

// recordedNodes are the 100 UNI holders the recorded query asked for, in Blockscout's order
func recordedNodes(t *testing.T) []evm.Address {
	t.Helper()
	var req struct {
		Logs []struct {
			Topics [][]evm.Hash `json:"topics"`
		} `json:"logs"`
	}
	if err := json.Unmarshal(testkit.Read(t, "hypersync/eth_uni_map.p1.request.json"), &req); err != nil {
		t.Fatal(err)
	}
	var out []evm.Address
	for _, topic := range req.Logs[0].Topics[1] {
		a, ok := evm.TopicAddress(topic)
		if !ok {
			t.Fatal("a topic is no address")
		}
		out = append(out, a)
	}
	return out
}

// blocks starts the window where the recorder did and reads the recorded block times, other blocks are 12 s apart from the head
type blocks struct{ t *testing.T }

func (b blocks) FirstBlockAt(_ context.Context, _ string, at time.Time, hi uint64) (uint64, error) {
	_, head := testkit.Header(b.t, "rpc/header.ethereum_26185638.json")
	if hi != fixture.UNIMapHead || head.Sub(at) != 30*24*time.Hour {
		return 0, errors.New("the window does not start 30 days before the head")
	}
	return fixture.UNIMapFrom, nil
}

func (b blocks) BlockTime(_ context.Context, _ string, n uint64) (time.Time, error) {
	switch n {
	case fixture.UNIMapFrom, fixture.UNIMapHead:
		_, at := testkit.Header(b.t, "rpc/header.ethereum_"+big.NewInt(int64(n)).String()+".json")
		return at, nil
	}
	_, head := testkit.Header(b.t, "rpc/header.ethereum_26185638.json")
	return head.Add(-time.Duration(fixture.UNIMapHead-n) * 12 * time.Second), nil
}

type clock struct{ now time.Time }

func (c *clock) get() time.Time { return c.now }

func newBuilder(t *testing.T, cfg config.HolderMap) (*holdermap.Builder, *fixture.MapLogs, *fixture.MapStore, *clock) {
	t.Helper()
	logs := fixture.NewMapLogs(fixture.UNIMapPages(t), fixture.UNIMapHead)
	st := fixture.NewMapStore()
	c := &clock{now: fixture.Now}
	return holdermap.New(cfg, logs, st, slog.New(slog.DiscardHandler)).WithClock(c.get), logs, st, c
}

var uniKey = holdermap.Key{Chain: fixture.Ethereum, Token: fixture.EthUNI, Limit: 100}

// expectedPairs adds up the recorded logs on their own: every transfer between two distinct nodes, per unordered pair
func expectedPairs(t *testing.T, from uint64) map[[2]evm.Address][2]*big.Int {
	t.Helper()
	out := map[[2]evm.Address][2]*big.Int{}
	for _, p := range fixture.UNIMapPages(t) {
		for _, l := range p.Logs {
			m, ok := index.Decode(l, false)
			if !ok || l.Block < from || m.From == m.To {
				continue
			}
			k := [2]evm.Address{m.From, m.To}
			if bytes.Compare(k[0][:], k[1][:]) > 0 {
				k[0], k[1] = k[1], k[0]
			}
			v, ok := out[k]
			if !ok {
				v = [2]*big.Int{new(big.Int), new(big.Int)}
				out[k] = v
			}
			v[0].Add(v[0], big.NewInt(1))
			v[1].Add(v[1], m.Amount)
		}
	}
	return out
}

func TestBuildReadsTheWindow(t *testing.T) {
	b, logs, st, _ := newBuilder(t, mapConfig())
	nodes := recordedNodes(t)
	if s, err := b.Links(t.Context(), uniKey, nodes); err != nil || s.Status != holdermap.StatusBuilding || logs.Calls != 0 {
		t.Fatalf("first call %+v %v, %d HyperSync calls", s, err, logs.Calls)
	}
	if !b.Work(t.Context(), blocks{t}) || b.Work(t.Context(), blocks{t}) {
		t.Fatal("the queue did not hold exactly one map")
	}
	if logs.Calls != 3 {
		t.Fatalf("%d pages, the recording has 3", logs.Calls)
	}
	for i, q := range logs.Queries {
		if q.To != fixture.UNIMapHead+1 || len(q.Topics1) != 100 || len(q.Topics2) != 100 || q.Topics1[0] != holdermap.Topic(nodes[0]) || q.Topics0[0] != evm.TopicTransfer {
			t.Fatalf("query %d %+v", i, q)
		}
	}
	row, ok, _ := st.HolderMap(t.Context(), uniKey.Chain, uniKey.Token, 100)
	if !ok || row.FromBlock != fixture.UNIMapFrom || row.ToBlock != fixture.UNIMapHead || !row.BuiltAt.Equal(fixture.Now) {
		t.Fatalf("row %+v", row)
	}
	s, err := b.Links(t.Context(), uniKey, nodes)
	if err != nil || s.Status != holdermap.StatusReady || s.Doc.WindowDays != 30 || s.Doc.Cut || s.Doc.Requests != 3 || s.Doc.Nodes != 100 {
		t.Fatalf("links %+v %v", s, err)
	}
	want := expectedPairs(t, fixture.UNIMapFrom)
	if len(s.Doc.Pairs) != len(want) || len(want) != 6 {
		t.Fatalf("%d pairs, want %d", len(s.Doc.Pairs), len(want))
	}
	for i, p := range s.Doc.Pairs {
		w := want[[2]evm.Address{p.A, p.B}]
		if w[0] == nil || w[0].Int64() != p.Transfers || w[1].String() != p.Raw || bytes.Compare(p.A[:], p.B[:]) >= 0 {
			t.Errorf("pair %d %+v, want %v", i, p, w)
		}
		if i > 0 {
			prev, _ := new(big.Int).SetString(s.Doc.Pairs[i-1].Raw, 10)
			cur, _ := new(big.Int).SetString(p.Raw, 10)
			if cur.Cmp(prev) > 0 {
				t.Error("pairs are not largest first")
			}
		}
	}
	// the Uniswap v4 pool manager and the Universal Router swap UNI back and forth
	busy := want[[2]evm.Address{evm.MustAddress("0x000000000004444c5dc75cb358380d2e3de08a90"), evm.MustAddress("0x1d42064fc4beb5f8aaf85f4617ae8b3b5b8bd801")}]
	if busy[0] == nil || busy[0].Int64() != 91 {
		t.Errorf("pool manager and router %v", busy)
	}
}

func TestBuildKeepsTheHeadWhenTheBudgetIsShort(t *testing.T) {
	cfg := mapConfig()
	cfg.PageBudget = 2
	b, logs, st, _ := newBuilder(t, cfg)
	nodes := recordedNodes(t)
	_, _ = b.Links(t.Context(), uniKey, nodes)
	b.Work(t.Context(), blocks{t})
	// the first page covered 70,224 blocks, so the one page left plans 90% of that before the head
	if logs.Calls != 2 || logs.Queries[1].From != fixture.UNIMapHead+1-63201 {
		t.Fatalf("%d pages, second from %d", logs.Calls, logs.Queries[len(logs.Queries)-1].From)
	}
	row, _, _ := st.HolderMap(t.Context(), uniKey.Chain, uniKey.Token, 100)
	s, _ := b.Links(t.Context(), uniKey, nodes)
	if row.FromBlock != fixture.UNIMapHead+1-63201 || row.ToBlock != fixture.UNIMapHead || !s.Doc.Cut || s.Doc.Requests != 2 || s.Doc.WindowDays != 8.8 {
		t.Fatalf("row %d to %d, doc %+v", row.FromBlock, row.ToBlock, s.Doc)
	}
	if want := expectedPairs(t, row.FromBlock); len(s.Doc.Pairs) != len(want) {
		t.Errorf("%d pairs, want %d", len(s.Doc.Pairs), len(want))
	}

	cfg.PageBudget = 1
	b, logs, st, _ = newBuilder(t, cfg)
	_, _ = b.Links(t.Context(), uniKey, nodes)
	b.Work(t.Context(), blocks{t})
	row, _, _ = st.HolderMap(t.Context(), uniKey.Chain, uniKey.Token, 100)
	s, _ = b.Links(t.Context(), uniKey, nodes)
	// no page is left to start again, so the map keeps the first page
	if logs.Calls != 1 || row.FromBlock != fixture.UNIMapFrom || row.ToBlock != 26039862 || !s.Doc.Cut || s.Doc.Requests != 1 || s.Doc.WindowDays <= 0 || s.Doc.WindowDays >= 30 {
		t.Fatalf("%d pages, row %d to %d, doc %+v", logs.Calls, row.FromBlock, row.ToBlock, s.Doc)
	}
}

func TestLinksQueueRefreshAndFailure(t *testing.T) {
	cfg := mapConfig()
	cfg.QueueMax = 1
	b, logs, st, c := newBuilder(t, cfg)
	ctx := t.Context()
	nodes := recordedNodes(t)
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusBuilding {
		t.Fatalf("first %+v", s)
	}
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusBuilding || b.Stats().Queued != 1 {
		t.Fatalf("the same map queued twice %+v %+v", s, b.Stats())
	}
	other := uniKey
	other.Limit = 50
	if s, _ := b.Links(ctx, other, nodes[:50]); s.Status != holdermap.StatusUnavailable || s.Reason != holdermap.ReasonQueueFull {
		t.Fatalf("past the queue %+v", s)
	}
	b.Work(ctx, blocks{t})
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusReady || !s.BuiltAt.Equal(fixture.Now) {
		t.Fatalf("after the build %+v", s)
	}

	c.now = c.now.Add(12 * time.Hour)
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusBuilding {
		t.Fatalf("12 hours later %+v", s)
	}
	logs.SetDown(true)
	calls := logs.Calls
	b.Work(ctx, blocks{t})
	if logs.Calls != calls+3 || b.Stats().Failed != 1 {
		t.Fatalf("a failed page was asked %d times, stats %+v", logs.Calls-calls, b.Stats())
	}
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusUnavailable || s.Reason != holdermap.ReasonFailed {
		t.Fatalf("after the failure %+v", s)
	}
	logs.SetDown(false)
	c.now = c.now.Add(10 * time.Minute)
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusBuilding {
		t.Fatalf("10 minutes after the failure %+v", s)
	}
	b.Work(ctx, blocks{t})
	if s, _ := b.Links(ctx, uniKey, nodes); s.Status != holdermap.StatusReady || !s.BuiltAt.Equal(c.now) {
		t.Fatalf("rebuilt %+v", s)
	}

	// Run prunes the maps built more than 7 days ago before it waits for work
	c.now = c.now.Add(7*24*time.Hour + time.Second)
	done, cancel := context.WithCancel(ctx)
	cancel()
	b.Run(done, blocks{t})
	if _, ok, _ := st.HolderMap(ctx, uniKey.Chain, uniKey.Token, 100); ok {
		t.Error("an 8 day old map was kept")
	}
}

func TestComponents(t *testing.T) {
	got := holdermap.Components(7, [][2]int{{5, 1}, {1, 3}, {4, 6}, {3, 5}})
	if len(got) != 2 || len(got[0]) != 3 || got[0][0] != 1 || got[0][2] != 5 || len(got[1]) != 2 || got[1][0] != 4 {
		t.Fatalf("components %v", got)
	}
	if got := holdermap.Components(3, nil); len(got) != 0 {
		t.Errorf("no links %v", got)
	}
}
