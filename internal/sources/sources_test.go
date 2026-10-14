package sources

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"tokens/internal/evm"
	"tokens/internal/testkit"
)

var (
	ethUSDC  = evm.MustAddress("0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48")
	ethMKR   = evm.MustAddress("0x9f8f72aa9304c8b593d555f12ef6589cc3a579a2")
	ethEOA   = evm.MustAddress("0x37305b1cd40574e4c5ce33f8e8306be057fd7341")
	monUSDC  = evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603")
	monWMON  = evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a")
	deadAddr = evm.MustAddress("0x000000000000000000000000000000000000dead")
	ethUNI   = evm.MustAddress("0x1f9840a85d5af5bf1d1762f925bdaddc4201f984")
	arcUSDC  = evm.MustAddress("0x3600000000000000000000000000000000000000")
)

// recorded serves testdata files by request path and query, a missing route answers 404 with an empty JSON object
func recorded(t *testing.T, routes map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.URL.Path
		if r.URL.RawQuery != "" {
			key += "?" + r.URL.RawQuery
		}
		file, ok := routes[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{}`))
			return
		}
		if strings.HasPrefix(file, "status:") {
			code := map[string]int{"status:404": 404, "status:429": 429, "status:500": 500}[file]
			w.Header().Set("Retry-After", "7")
			w.WriteHeader(code)
			return
		}
		_, _ = w.Write(testkit.Read(t, file))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestBlockscoutRecorded(t *testing.T) {
	var raw struct {
		Next jsontext.Value `json:"next_page_params"`
	}
	if err := json.Unmarshal(testkit.Read(t, "blockscout/holders.usdc.page1.json"), &raw); err != nil {
		t.Fatal(err)
	}
	q, err := pageParams(raw.Next)
	if err != nil {
		t.Fatal(err)
	}
	srv := recorded(t, map[string]string{
		"/api/v2/tokens/" + ethUSDC.String():                            "blockscout/token.usdc.json",
		"/api/v2/tokens/" + ethMKR.String():                             "blockscout/token.mkr.json",
		"/api/v2/tokens/" + ethUSDC.String() + "/holders":               "blockscout/holders.usdc.page1.json",
		"/api/v2/tokens/" + ethUSDC.String() + "/holders?" + q.Encode(): "blockscout/holders.usdc.page2.json",
		"/api/v2/tokens/" + ethEOA.String():                             "status:404",
		"/api/v2/tokens/" + deadAddr.String():                           "status:429",
	})
	b := NewBlockscout(srv.URL, 100)
	ctx := t.Context()

	tok, err := b.Token(ctx, ethUSDC)
	if err != nil {
		t.Fatal(err)
	}
	if tok.HoldersCount == nil || *tok.HoldersCount != 9485913 || tok.CirculatingMarketCap == nil || *tok.CirculatingMarketCap < 7e10 || tok.Type != "ERC-20" || tok.TotalSupply.Sign() <= 0 {
		t.Errorf("USDC token %+v", tok)
	}
	mkr, err := b.Token(ctx, ethMKR)
	if err != nil || mkr.HoldersCount == nil || *mkr.HoldersCount <= 0 {
		t.Errorf("MKR token %+v %v", mkr, err)
	}
	if _, err := b.Token(ctx, ethEOA); !errors.Is(err, ErrNotFound) {
		t.Errorf("EOA: %v", err)
	}
	var ue *UnavailableError
	if _, err := b.Token(ctx, deadAddr); !errors.As(err, &ue) || ue.RetryAfter != 7*time.Second {
		t.Errorf("429: %v", err)
	}

	p1, err := b.Holders(ctx, ethUSDC, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != BlockscoutPage || p1.Next == nil {
		t.Fatalf("page 1: %d items, next %v", len(p1.Items), p1.Next)
	}
	first := p1.Items[0]
	if first.Address != ethEOA || first.IsContract == nil || *first.IsContract || first.Label != "Sky: PSM" || first.Value.Sign() <= 0 {
		t.Errorf("first holder %+v", first)
	}
	for i := 1; i < len(p1.Items); i++ {
		if p1.Items[i].Value.Cmp(p1.Items[i-1].Value) > 0 {
			t.Fatalf("holder %d is larger than the one before", i)
		}
	}
	if p1.Next.Get("items_count") != "50" || p1.Next.Get("address_hash") == "" || p1.Next.Get("value") == "" {
		t.Errorf("next params %v", p1.Next)
	}
	p2, err := b.Holders(ctx, ethUSDC, p1.Next)
	if err != nil {
		t.Fatal(err)
	}
	if len(p2.Items) != BlockscoutPage || p2.Items[0].Value.Cmp(p1.Items[49].Value) > 0 {
		t.Errorf("page 2 does not follow page 1")
	}
}

func TestPageParams(t *testing.T) {
	v, err := pageParams([]byte(`{"value":"100000000090000","address_hash":"0x2f9e","items_count":50,"x":null}`))
	if err != nil || v.Get("value") != "100000000090000" || v.Get("items_count") != "50" || v.Has("x") {
		t.Errorf("got %v, %v", v, err)
	}
	if v, err := pageParams([]byte(`null`)); err != nil || v != nil {
		t.Errorf("null: %v %v", v, err)
	}
	if _, err := pageParams([]byte(`{"a":[1]}`)); err == nil {
		t.Error("an array value was accepted")
	}
}

func TestGeckoRecorded(t *testing.T) {
	srv := recorded(t, map[string]string{
		"/networks/monad/tokens/" + monUSDC.String() + "/info":  "gecko/info.usdc.json",
		"/networks/monad/tokens/" + monUSDC.String():            "gecko/token.usdc.json",
		"/networks/monad/tokens/" + monWMON.String() + "/info":  "gecko/info.wmon.json",
		"/networks/monad/tokens/" + deadAddr.String() + "/info": "gecko/info.unknown.json",
		"/networks/arc/tokens/" + arcUSDC.String() + "/info":    "gecko/info.arc_usdc.json",
		"/networks/arc/tokens/" + arcUSDC.String():              "gecko/token.arc_usdc.json",
	})
	// the recorded unknown token is a real 404 body, served here with status 200 by the table, so route it as 404
	g := NewGecko(srv.URL, 600)
	ctx := t.Context()
	h, err := g.Holders(ctx, "monad", monUSDC)
	if err != nil {
		t.Fatal(err)
	}
	if h.Count == nil || *h.Count != 76440 || h.Top10Share == nil || *h.Top10Share != 0.772433 || h.LastUpdated == nil {
		t.Errorf("USDC holders %+v", h)
	}
	w, err := g.Holders(ctx, "monad", monWMON)
	// GeckoTerminal sends no distribution for WMON, only the count
	if err != nil || w.Count == nil || *w.Count != 91458 || w.Top10Share != nil {
		t.Errorf("WMON holders %+v %v", w, err)
	}
	mc, err := g.MarketCap(ctx, "monad", monUSDC)
	if err != nil || mc == nil || *mc < 7e10 {
		t.Errorf("USDC market cap %v %v", mc, err)
	}
	// one client reads every chain, each call names its network
	a, err := g.Holders(ctx, "arc", arcUSDC)
	if err != nil || a.Count == nil || *a.Count != 37270 || a.Top10Share == nil || *a.Top10Share != 0.397904 {
		t.Errorf("Arc USDC holders %+v %v", a, err)
	}
	if mc, err := g.MarketCap(ctx, "arc", arcUSDC); err != nil || mc == nil || *mc < 7e10 {
		t.Errorf("Arc USDC market cap %v %v", mc, err)
	}
	// GeckoTerminal's real answer for an unknown token
	if !strings.Contains(string(testkit.Read(t, "gecko/info.unknown.json")), "Not Found") {
		t.Error("the recorded unknown answer is not GeckoTerminal's 404 body")
	}
	notFound := recorded(t, map[string]string{})
	if _, err := NewGecko(notFound.URL, 600).Holders(ctx, "monad", deadAddr); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown token: %v", err)
	}
}

func TestGeckoPausesAfter429(t *testing.T) {
	srv := recorded(t, map[string]string{"/networks/monad/tokens/" + monUSDC.String() + "/info": "status:429"})
	g := NewGecko(srv.URL, 600)
	var ue *UnavailableError
	if _, err := g.Holders(t.Context(), "monad", monUSDC); !errors.As(err, &ue) {
		t.Fatalf("429: %v", err)
	}
	// the next call does not reach GeckoTerminal while the pause lasts
	if _, err := g.Holders(t.Context(), "monad", monUSDC); !errors.As(err, &ue) || ue.RetryAfter < 25*time.Second {
		t.Errorf("paused call: %v", err)
	}
}

func TestPricesRecorded(t *testing.T) {
	srv := recorded(t, map[string]string{
		"/internal/v1/chains/eip155:143/tokens/" + monUSDC.String() + "/prices?range=1d": "prices/monad_usdc.json",
		"/internal/v1/chains/eip155:1/tokens/" + deadAddr.String() + "/prices?range=1d":  "status:404",
		"/internal/v1/chains/eip155:1/tokens/" + ethUSDC.String() + "/prices?range=1d":   "status:500",
	})
	p := NewPrices(srv.URL)
	v, err := p.Current(t.Context(), "eip155:143", monUSDC)
	if err != nil || v == nil || *v < 0.9 || *v > 1.1 {
		t.Errorf("USDC price %v %v", v, err)
	}
	if v, err := p.Current(t.Context(), "eip155:1", deadAddr); err != nil || v != nil {
		t.Errorf("unknown coin %v %v", v, err)
	}
	var ue *UnavailableError
	if _, err := p.Current(t.Context(), "eip155:1", ethUSDC); !errors.As(err, &ue) {
		t.Errorf("server error: %v", err)
	}
}

/* HyperSync */

func TestParseRecordedPages(t *testing.T) {
	cases := []struct {
		file   string
		token  evm.Address
		logs   int
		next   uint64
		topics int
	}{
		{"monad_usdc", monUSDC, 39, 113000040, 1},
		{"monad_wmon", monWMON, 335, 113000300, 3},
		{"eth_mkr", ethMKR, 49, 26181000, 1},
		{"eth_uni_map.p1", ethUNI, 34, 26039863, 1},
		{"eth_uni_map.p2", ethUNI, 71, 26118258, 1},
		{"eth_uni_map.p3", ethUNI, 15, 26185639, 1},
	}
	for _, c := range cases {
		page, err := ParsePage(testkit.Read(t, "hypersync/"+c.file+".json"))
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if len(page.Logs) != c.logs || page.NextBlock != c.next || page.ArchiveHeight < c.next {
			t.Errorf("%s: %d logs, next %d, archive %d", c.file, len(page.Logs), page.NextBlock, page.ArchiveHeight)
		}
		seen := map[evm.Hash]bool{}
		for i, l := range page.Logs {
			// the holder map query selects no block times
			if l.Address != c.token || (l.Time == 0) != strings.HasPrefix(c.file, "eth_uni_map") || len(l.Topics) == 0 {
				t.Fatalf("%s: log %d %+v", c.file, i, l)
			}
			if i > 0 && (l.Block < page.Logs[i-1].Block || l.Block == page.Logs[i-1].Block && l.LogIndex <= page.Logs[i-1].LogIndex) {
				t.Fatalf("%s: logs are not in order at %d", c.file, i)
			}
			seen[l.Topics[0]] = true
		}
		if len(seen) != c.topics {
			t.Errorf("%s: %d topic0 values", c.file, len(seen))
		}
	}
}

// The engine's query body must be the one the recordings were made with
func TestQueryBodyMatchesRecording(t *testing.T) {
	holders := uniHolders(t)
	cases := []struct {
		file string
		q    LogQuery
	}{
		{"monad_usdc", LogQuery{From: 113000000, To: 113000040, Addresses: []evm.Address{monUSDC}, Topics0: []evm.Hash{evm.TopicTransfer}, Blocks: true}},
		{"monad_wmon", LogQuery{From: 113000000, To: 113000300, Addresses: []evm.Address{monWMON}, Topics0: []evm.Hash{evm.TopicTransfer, evm.TopicDeposit, evm.TopicWithdrawal}, Blocks: true}},
		{"eth_mkr", LogQuery{From: 26180000, To: 26181000, Addresses: []evm.Address{ethMKR}, Topics0: []evm.Hash{evm.TopicTransfer}, Blocks: true}},
		{"eth_uni_map.p1", LogQuery{From: 25969639, To: 26185639, Addresses: []evm.Address{ethUNI}, Topics0: []evm.Hash{evm.TopicTransfer}, Topics1: holders, Topics2: holders}},
		{"eth_uni_map.p2", LogQuery{From: 26039863, To: 26185639, Addresses: []evm.Address{ethUNI}, Topics0: []evm.Hash{evm.TopicTransfer}, Topics1: holders, Topics2: holders}},
	}
	for _, c := range cases {
		if len(c.q.Topics1) > 0 && len(c.q.Topics1) != 100 {
			t.Fatalf("%s: %d holders", c.file, len(c.q.Topics1))
		}
		body, err := c.q.body()
		if err != nil {
			t.Fatal(err)
		}
		var got, want any
		if err := errors.Join(json.Unmarshal(body, &got), json.Unmarshal(testkit.Read(t, "hypersync/"+c.file+".request.json"), &want)); err != nil {
			t.Fatal(err)
		}
		if mustJSONAny(got) != mustJSONAny(want) {
			t.Errorf("%s:\n got %s\nwant %s", c.file, mustJSONAny(got), mustJSONAny(want))
		}
	}
}

// uniHolders are the 100 UNI holders of the recorded Blockscout pages as topics, in Blockscout's order
func uniHolders(t *testing.T) []evm.Hash {
	t.Helper()
	var out []evm.Hash
	for _, f := range []string{"holders.uni.page1.json", "holders.uni.page2.json"} {
		var raw bsHolders
		if err := json.Unmarshal(testkit.Read(t, "blockscout/"+f), &raw); err != nil {
			t.Fatal(err)
		}
		p, err := raw.convert()
		if err != nil {
			t.Fatal(err)
		}
		for _, h := range p.Items {
			var topic evm.Hash
			copy(topic[12:], h.Address[:])
			out = append(out, topic)
		}
	}
	return out
}

func mustJSONAny(v any) string {
	b, _ := json.Marshal(v, json.Deterministic(true))
	return string(b)
}

type hsServer struct {
	mu       sync.Mutex
	calls    int
	statuses []int
	headers  []map[string]string
	body     []byte
	auth     string
}

func (s *hsServer) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/height" {
		_, _ = w.Write([]byte(`{"height":113080000}`))
		return
	}
	s.auth = r.Header.Get("Authorization")
	i := s.calls
	s.calls++
	if i < len(s.headers) {
		for k, v := range s.headers[i] {
			w.Header().Set(k, v)
		}
	}
	if i < len(s.statuses) && s.statuses[i] != http.StatusOK {
		w.WriteHeader(s.statuses[i])
		return
	}
	_, _ = w.Write(s.body)
}

func noSleep(context.Context, time.Duration) error { return nil }

func TestHyperSyncPageWaitsOutTheQuota(t *testing.T) {
	fake := &hsServer{
		statuses: []int{http.StatusTooManyRequests, http.StatusOK},
		headers:  []map[string]string{{"X-Ratelimit-Remaining": "0", "X-Ratelimit-Cost": "1000", "X-Ratelimit-Reset": "0"}, {"X-Ratelimit-Remaining": "8000", "X-Ratelimit-Cost": "1000"}},
		body:     testkit.Read(t, "hypersync/monad_usdc.json"),
	}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	h := NewHyperSync(map[string]string{"eip155:143": srv.URL + "/query"}, "secret-key", 600, 600)
	start := time.Now()
	page, err := h.Page(t.Context(), "eip155:143", LogQuery{From: 113000000, To: 113000040, Addresses: []evm.Address{monUSDC}, Topics0: []evm.Hash{evm.TopicTransfer}, Blocks: true}, Background)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Logs) != 39 || fake.calls != 2 || fake.auth != "Bearer secret-key" {
		t.Errorf("%d logs after %d calls, auth %q", len(page.Logs), fake.calls, fake.auth)
	}
	// a spent window pauses until a second past its reset
	if waited := time.Since(start); waited < time.Second {
		t.Errorf("retried after %s, before the window reset", waited)
	}
	if st := h.Stats(); st.Requests != 2 || st.RateLimited != 1 {
		t.Errorf("stats %+v", st)
	}
	height, err := h.Height(t.Context(), "eip155:143")
	if err != nil || height != 113080000 {
		t.Errorf("height %d %v", height, err)
	}
	if strings.Contains(h.String()+h.GoString(), "secret") {
		t.Error("the key leaks through fmt")
	}
}

func TestHyperSyncInteractiveGivesUp(t *testing.T) {
	fake := &hsServer{statuses: []int{500, 500, 500, 500}}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	h := NewHyperSync(map[string]string{"eip155:1": srv.URL + "/query"}, "k", 600, 600).WithPacers(NewPacer(600, 5), NewPacer(600, 5), noSleep)
	_, err := h.Page(t.Context(), "eip155:1", LogQuery{From: 1, To: 2, Addresses: []evm.Address{ethMKR}, Topics0: []evm.Hash{evm.TopicTransfer}}, Interactive)
	var ue *UnavailableError
	if !errors.As(err, &ue) || fake.calls != interactiveAttempts {
		t.Errorf("err %v after %d calls", err, fake.calls)
	}
	if _, err := h.Page(t.Context(), "eip155:999", LogQuery{From: 1, To: 2, Addresses: []evm.Address{ethMKR}, Topics0: []evm.Hash{evm.TopicTransfer}}, Interactive); err == nil {
		t.Error("a chain without an endpoint was queried")
	}
	if p, err := h.Page(t.Context(), "eip155:1", LogQuery{From: 5, To: 5, Addresses: []evm.Address{ethMKR}, Topics0: []evm.Hash{evm.TopicTransfer}}, Interactive); err != nil || p.NextBlock != 5 {
		t.Errorf("empty range %+v %v", p, err)
	}
	if _, err := NewHyperSync(nil, "", 1, 1).Page(t.Context(), "eip155:1", LogQuery{}, Background); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: %v", err)
	}
}

func TestNoteQuota(t *testing.T) {
	h := NewHyperSync(nil, "k", 60, 60)
	h.noteQuota(http.Header{"X-Ratelimit-Remaining": {"8000"}, "X-Ratelimit-Cost": {"1000"}, "X-Ratelimit-Reset": {"30"}})
	if h.PausedFor() != 0 {
		t.Error("paused with quota left")
	}
	h.noteQuota(http.Header{"X-Ratelimit-Remaining": {"0"}, "X-Ratelimit-Cost": {"1000"}, "X-Ratelimit-Reset": {"30"}})
	if d := h.PausedFor(); d < 30*time.Second || d > 32*time.Second {
		t.Errorf("paused for %s", d)
	}
	if firstValue("15000, 15000;w=60") != "15000" {
		t.Error("first value")
	}
}

func TestQuantity(t *testing.T) {
	for in, want := range map[string]uint64{`12`: 12, `"12"`: 12, `"0x6acdf3ab"`: 0x6acdf3ab} {
		var q quantity
		if err := q.UnmarshalJSON([]byte(in)); err != nil || uint64(q) != want {
			t.Errorf("%s: %d %v", in, q, err)
		}
	}
	var q quantity
	if err := q.UnmarshalJSON([]byte(`"-1"`)); err == nil {
		t.Error("a negative quantity parsed")
	}
}

func TestRetryAfter(t *testing.T) {
	now := time.Now()
	if d := retryAfter(http.Header{"Retry-After": {"12"}}, now); d != 12*time.Second {
		t.Errorf("seconds: %s", d)
	}
	if d := retryAfter(http.Header{"X-Ratelimit-Reset": {"58"}}, now); d != 58*time.Second {
		t.Errorf("reset: %s", d)
	}
	if d := retryAfter(http.Header{"Retry-After": {now.Add(time.Minute).UTC().Format(http.TimeFormat)}}, now); d < 58*time.Second {
		t.Errorf("date: %s", d)
	}
	if d := retryAfter(http.Header{}, now); d != 0 {
		t.Errorf("none: %s", d)
	}
}

// serveBody serves one JSON body at any path
func serveBody(t *testing.T, body string) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte(body)) }))
	t.Cleanup(srv.Close)
	return srv.URL
}
