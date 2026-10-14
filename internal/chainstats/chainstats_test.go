package chainstats

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"strconv"
	"strings"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/markets"
	"tokens/internal/sources"
	"tokens/internal/store"
	"tokens/internal/testkit"
)

const (
	monad    = "eip155:143"
	pageFrom = 113178240
	pageNext = 113178280
)

var monSystem = evm.MustAddress("0x6f49a8f621353f12378d0046e7d7e4b9b249dc9e")

// recordedPage is the HyperSync answer of blocks 113,178,240 to 113,178,279, twenty on each side of midnight
func recordedPage(t *testing.T) sources.BlockPage {
	t.Helper()
	page, err := sources.ParseBlockPage(testkit.Read(t, "hypersync/monad_blocks.json"))
	if err != nil {
		t.Fatal(err)
	}
	return page
}

// pageSource answers each query with the part of the recorded page from its From, at most size blocks
type pageSource struct {
	page    sources.BlockPage
	size    int
	height  uint64
	queries []sources.BlockQuery
}

func (s *pageSource) Height(context.Context, string) (uint64, error) { return s.height, nil }

func (s *pageSource) Blocks(_ context.Context, _ string, q sources.BlockQuery, prio sources.Priority) (sources.BlockPage, error) {
	if prio != sources.Background {
		return sources.BlockPage{}, errors.New("the count asked outside the background lane")
	}
	s.queries = append(s.queries, q)
	start := int(q.From - pageFrom)
	end := min(start+s.size, len(s.page.Blocks), int(q.To-pageFrom))
	out := sources.BlockPage{Blocks: s.page.Blocks[start:end], NextBlock: pageFrom + uint64(end)}
	for _, tx := range s.page.Txs {
		if tx.Block >= q.From && tx.Block < out.NextBlock {
			out.Txs = append(out.Txs, tx)
		}
	}
	return out, nil
}

// memStore keeps the counted days in memory, with the same cursor rule as the database
type memStore struct {
	cur   *store.StatsCursor
	days  map[string]store.StatsDay
	adds  int
	moved bool
}

func (m *memStore) StatsCursor(context.Context, string) (store.StatsCursor, bool, error) {
	if m.cur == nil {
		return store.StatsCursor{}, false, nil
	}
	return *m.cur, true, nil
}

func (m *memStore) AddStats(_ context.Context, _ string, from, next uint64, first time.Time, days []store.StatsDay) error {
	if m.moved || m.cur != nil && m.cur.Next != from {
		return store.ErrStatsMoved
	}
	if m.cur == nil {
		m.cur = &store.StatsCursor{First: first}
	}
	m.cur.Next = next
	m.adds++
	sum := func(a, b string) string {
		x, _ := strconv.ParseUint(a, 10, 64)
		y, _ := strconv.ParseUint(b, 10, 64)
		return strconv.FormatUint(x+y, 10)
	}
	for _, d := range days {
		old, ok := m.days[d.Day]
		if !ok {
			old = store.StatsDay{Day: d.Day, GasUsed: "0", GasLimit: "0", GasPriceWei: "0"}
		}
		m.days[d.Day] = store.StatsDay{
			Day: d.Day, Blocks: old.Blocks + d.Blocks, Txs: old.Txs + d.Txs, GasUsed: sum(old.GasUsed, d.GasUsed),
			GasLimit: sum(old.GasLimit, d.GasLimit), GasPriceWei: sum(old.GasPriceWei, d.GasPriceWei),
		}
	}
	return nil
}

func (m *memStore) StatsDays(_ context.Context, _ string, from, to string) ([]store.StatsDay, error) {
	var out []store.StatsDay
	for _, day := range []string{"2026-10-13", "2026-10-14"} {
		if d, ok := m.days[day]; ok && day >= from && day <= to {
			out = append(out, d)
		}
	}
	return out, nil
}

func counter(src Source, st Store, backfill int) *Counter {
	mc := config.MarketChain{ID: monad, ActivitySource: config.ActivityBlocks, SystemSenders: []evm.Address{monSystem}}
	cfg := config.BlockStats{Poll: 10 * time.Minute, BackfillDays: backfill, LagBlocks: 10}
	return New(mc, cfg, src, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

// want sums the recording itself, day by day, without the system sender's transactions
func want(t *testing.T) map[string]store.StatsDay {
	t.Helper()
	var raw struct {
		Data []struct {
			Blocks []struct {
				Number    int64  `json:"number"`
				Timestamp string `json:"timestamp"`
				GasUsed   string `json:"gas_used"`
				GasLimit  string `json:"gas_limit"`
			} `json:"blocks"`
			Transactions []struct {
				BlockNumber int64  `json:"block_number"`
				From        string `json:"from"`
				GasPrice    string `json:"gas_price"`
			} `json:"transactions"`
		} `json:"data"`
	}
	if err := json.Unmarshal(testkit.Read(t, "hypersync/monad_blocks.json"), &raw); err != nil {
		t.Fatal(err)
	}
	hex := func(s string) uint64 {
		n, err := strconv.ParseUint(strings.TrimPrefix(s, "0x"), 16, 64)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	type sums struct{ blocks, txs, used, limit, price uint64 }
	by, dayOf := map[string]*sums{}, map[uint64]string{}
	for _, item := range raw.Data {
		for _, b := range item.Blocks {
			day := time.Unix(int64(hex(b.Timestamp)), 0).UTC().Format(time.DateOnly)
			dayOf[uint64(b.Number)] = day
			if by[day] == nil {
				by[day] = &sums{}
			}
			by[day].blocks++
			by[day].used += hex(b.GasUsed)
			by[day].limit += hex(b.GasLimit)
		}
	}
	for _, item := range raw.Data {
		for _, tx := range item.Transactions {
			if strings.EqualFold(tx.From, monSystem.String()) {
				continue
			}
			s := by[dayOf[uint64(tx.BlockNumber)]]
			s.txs++
			s.price += hex(tx.GasPrice)
		}
	}
	out := map[string]store.StatsDay{}
	for day, s := range by {
		out[day] = store.StatsDay{
			Day: day, Blocks: s.blocks, Txs: s.txs, GasUsed: strconv.FormatUint(s.used, 10),
			GasLimit: strconv.FormatUint(s.limit, 10), GasPriceWei: strconv.FormatUint(s.price, 10),
		}
	}
	return out
}

func TestCatchUpCountsTheRecordedPageIntoItsTwoDays(t *testing.T) {
	src := &pageSource{page: recordedPage(t), size: 40, height: pageNext + 10}
	st := &memStore{cur: &store.StatsCursor{Next: pageFrom}, days: map[string]store.StatsDay{}}
	if err := counter(src, st, 0).CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	exp := want(t)
	if len(exp) != 2 || exp["2026-10-13"].Blocks != 20 || exp["2026-10-14"].Blocks != 20 {
		t.Fatalf("the recording holds %+v", exp)
	}
	for day, w := range exp {
		if got := st.days[day]; got != w {
			t.Errorf("%s: got %+v, want %+v", day, got, w)
		}
	}
	// 596 transactions, one system transaction in each of the 40 blocks
	if n := exp["2026-10-13"].Txs + exp["2026-10-14"].Txs; n != 556 {
		t.Errorf("%d transactions without the system ones", n)
	}
	if st.cur.Next != pageNext || len(src.queries) != 1 || src.queries[0] != (sources.BlockQuery{From: pageFrom, To: pageNext}) {
		t.Errorf("cursor %d after %v", st.cur.Next, src.queries)
	}
}

func TestCatchUpGivesTheSameDaysOverShortPages(t *testing.T) {
	src := &pageSource{page: recordedPage(t), size: 7, height: pageNext + 10}
	st := &memStore{cur: &store.StatsCursor{Next: pageFrom}, days: map[string]store.StatsDay{}}
	if err := counter(src, st, 0).CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	for day, w := range want(t) {
		if got := st.days[day]; got != w {
			t.Errorf("%s: got %+v, want %+v", day, got, w)
		}
	}
	if st.adds != 6 || st.cur.Next != pageNext {
		t.Errorf("%d pages, cursor %d", st.adds, st.cur.Next)
	}
}

func TestCatchUpStopsWhenAnotherWriterMovedTheCursor(t *testing.T) {
	src := &pageSource{page: recordedPage(t), size: 40, height: pageNext + 10}
	st := &memStore{cur: &store.StatsCursor{Next: pageFrom}, days: map[string]store.StatsDay{}, moved: true}
	if err := counter(src, st, 0).CatchUp(t.Context()); !errors.Is(err, store.ErrStatsMoved) {
		t.Fatalf("error %v", err)
	}
	if len(st.days) != 0 || st.cur.Next != pageFrom {
		t.Errorf("counted %v, cursor %d", st.days, st.cur.Next)
	}
}

func TestAFirstCountStartsOneBlockBelowTheHead(t *testing.T) {
	src := &pageSource{page: recordedPage(t), size: 40, height: pageNext + 10}
	st := &memStore{days: map[string]store.StatsDay{}}
	if err := counter(src, st, 0).CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	last := src.page.Blocks[39]
	if len(src.queries) != 1 || src.queries[0].From != pageNext-1 || st.cur.Next != pageNext || !st.cur.First.Equal(time.Unix(int64(last.Time), 0)) {
		t.Errorf("queries %v, cursor %+v", src.queries, st.cur)
	}
}

// rateSource answers one-block queries with blocks every 0.3 seconds, the probes of a backfill start
type rateSource struct {
	head, headTime uint64
	queries        []uint64
}

func (s *rateSource) Height(context.Context, string) (uint64, error) { return s.head + 10, nil }

func (s *rateSource) Blocks(_ context.Context, _ string, q sources.BlockQuery, _ sources.Priority) (sources.BlockPage, error) {
	s.queries = append(s.queries, q.From)
	at := s.headTime - (s.head-q.From)*3/10
	return sources.BlockPage{Blocks: []sources.Block{{Number: q.From, Time: at}}, NextBlock: q.From + 1}, nil
}

func TestABackfillStartsBeforeTheMidnightItAsksFor(t *testing.T) {
	// the head at 17h UTC on 2026-10-14, so 3 days back is midnight of 2026-10-11, 320,400 seconds or 1,068,000 blocks earlier
	src := &rateSource{head: 113400000, headTime: 1791997200}
	c := counter(src, &memStore{days: map[string]store.StatsDay{}}, 3)
	from, err := c.start(t.Context(), src.head)
	if err != nil {
		t.Fatal(err)
	}
	midnight := src.head - 1068000
	if from >= midnight || from < midnight-60000 || len(src.queries) != 2 {
		t.Errorf("start %d, the midnight block is %d, probes %v", from, midnight, src.queries)
	}
}

func TestStatsLineAnswersThreeLines(t *testing.T) {
	st := &memStore{cur: &store.StatsCursor{Next: pageFrom}, days: map[string]store.StatsDay{}}
	c := counter(&pageSource{page: recordedPage(t), size: 40, height: pageNext + 10}, st, 0)
	if err := c.CatchUp(t.Context()); err != nil {
		t.Fatal(err)
	}
	w := want(t)["2026-10-14"]
	used, _ := strconv.ParseFloat(w.GasUsed, 64)
	limit, _ := strconv.ParseFloat(w.GasLimit, 64)
	price, _ := strconv.ParseFloat(w.GasPriceWei, 64)
	for id, value := range map[string]float64{"newTxns": float64(w.Txs), "averageGasPrice": price / float64(w.Txs) / 1e9, "networkUtilization": used / limit} {
		pts, err := c.StatsLine(t.Context(), id, "2026-10-14", "2026-10-14")
		if err != nil || len(pts) != 1 || pts[0] != (sources.StatPoint{Day: "2026-10-14", Value: value}) {
			t.Errorf("%s: %v %v, want %v", id, pts, err, value)
		}
	}
	for _, id := range []string{"activeAccounts", "newAccounts", "txnsFee"} {
		if _, err := c.StatsLine(t.Context(), id, "2026-10-14", "2026-10-14"); !errors.Is(err, markets.ErrNoStatsLine) {
			t.Errorf("%s: %v", id, err)
		}
	}
}
