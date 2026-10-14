package tokens_test

import (
	"context"
	"encoding/json/v2"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/fixture"
	"tokens/internal/sources"
	"tokens/internal/testkit"
	"tokens/internal/tokens"
)

func search(t *testing.T, svc *tokens.Service, q string, limit int) tokens.Search {
	t.Helper()
	out, err := svc.Search(t.Context(), q, limit)
	if err != nil {
		t.Fatalf("%q: %v", q, err)
	}
	if out.Kind != "token_search" || len(out.Items) > limit {
		t.Fatalf("%q: %+v", q, out)
	}
	return out
}

func sym(it tokens.SearchItem) string {
	if it.Symbol == nil {
		return ""
	}
	return *it.Symbol
}

// verifiedFirst checks that no unverified token comes before a verified one
func verifiedFirst(t *testing.T, out tokens.Search) {
	t.Helper()
	seen := false
	for _, it := range out.Items {
		if !it.Verified {
			seen = true
		} else if seen {
			t.Errorf("verified %s after an unverified token", it.Token)
		}
	}
}

func TestSearchUSDCRealTokensFirst(t *testing.T) {
	svc, _ := fixture.Service(t)
	out := search(t, svc, "usdc", 20)
	if len(out.Items) < 4 {
		t.Fatalf("items %+v", out.Items)
	}
	// both USDC contracts, Ethereum's and Monad's, are the first two and verified
	got := []string{out.Items[0].Token, out.Items[1].Token}
	want := []string{fixture.EthUSDC.String(), fixture.MonUSDC.String()}
	if !slices.Equal(got, want) && !slices.Equal([]string{got[1], got[0]}, want) {
		t.Errorf("first two %v, want %v", got, want)
	}
	for _, it := range out.Items[:2] {
		if !it.Verified || sym(it) != "USDC" || it.Decimals != 6 || it.LogoURL == nil || !strings.HasPrefix(*it.LogoURL, "https://") {
			t.Errorf("item %+v", it)
		}
	}
	verifiedFirst(t, out)
	for _, it := range out.Items {
		if it.LogoURL != nil && !strings.HasPrefix(*it.LogoURL, "https://") {
			t.Errorf("logo %q", *it.LogoURL)
		}
	}
}

// A token that is on no list can carry the name of a real one and the deepest pools, and still comes after every listed token
func TestSearchCopycatsNeverOutrankListedTokens(t *testing.T) {
	svc, _ := fixture.Service(t)
	out := search(t, svc, "pepe", 20)
	first := out.Items[0]
	if first.Token != "0x6982508145454ce325ddbe47a25d4ec3d2311933" || !first.Verified || first.Chain != fixture.Ethereum {
		t.Fatalf("first %+v", first)
	}
	verifiedFirst(t, out)
	var copies int
	for _, it := range out.Items {
		if !it.Verified && strings.EqualFold(sym(it), "pepe") {
			copies++
		}
	}
	if copies == 0 || !slices.Contains(out.Coverage, "token_search_unverified") {
		t.Errorf("%d copies of the name, coverage %v", copies, out.Coverage)
	}
}

func TestSearchMonWrappedMonIsListed(t *testing.T) {
	svc, _ := fixture.Service(t)
	out := search(t, svc, "mon", 20)
	var wmon, native bool
	for _, it := range out.Items {
		if it.Token == fixture.MonWMON.String() {
			wmon = it.Verified && it.Chain == fixture.Monad && sym(it) == "WMON" && it.LogoURL != nil
		}
		if it.Token == "0x0000000000000000000000000000000000000000" {
			native = true
		}
	}
	// the native coin is no ERC-20 contract, so it is in neither the lists nor the results
	if !wmon || native {
		t.Errorf("wmon %v native %v: %+v", wmon, native, out.Items)
	}
	// a symbol that holds the text comes before a name that holds it
	iw, im := -1, -1
	for i, it := range out.Items {
		switch sym(it) {
		case "WMON":
			iw = i
		case "MIM":
			im = i
		}
	}
	if iw < 0 || (im >= 0 && iw > im) {
		t.Errorf("WMON at %d, MIM at %d", iw, im)
	}
	verifiedFirst(t, out)
}

func TestSearchWMONIsFirstOnItsChain(t *testing.T) {
	svc, _ := fixture.Service(t)
	out := search(t, svc, "wmon", 3)
	if out.Items[0].Token != fixture.MonWMON.String() || !out.Items[0].Verified || len(out.Items) != 3 {
		t.Errorf("items %+v", out.Items)
	}
}

func TestSearchNothingAndLimit(t *testing.T) {
	svc, _ := fixture.Service(t)
	none := search(t, svc, "zz", 8)
	if none.Items == nil || len(none.Items) != 0 || len(none.Coverage) != 0 {
		t.Errorf("zz: %+v", none)
	}
	if out := search(t, svc, "usdc", 1); len(out.Items) != 1 {
		t.Errorf("limit 1 gave %d", len(out.Items))
	}
}

func TestSearchMatchOrderBySymbolThenName(t *testing.T) {
	svc, _ := fixture.Service(t)
	out := search(t, svc, "usd", 20)
	// verified tokens whose symbol starts with the query come before verified tokens that only hold it in the name
	nameOnly := false
	for _, it := range out.Items {
		if !it.Verified {
			break
		}
		starts := strings.HasPrefix(strings.ToLower(sym(it)), "usd")
		if nameOnly && starts {
			t.Errorf("%s (a symbol prefix) after a looser match", sym(it))
		}
		if !starts {
			nameOnly = true
		}
	}
}

func TestSearchWithGeckoDown(t *testing.T) {
	svc, f := fixture.Service(t)
	f.GeckoDown = true
	out := search(t, svc, "usdc", 8)
	if !slices.Contains(out.Coverage, "token_search_long_tail_unavailable") || len(out.Items) < 2 || !out.Items[0].Verified || out.Items[0].Token != fixture.EthUSDC.String() && out.Items[0].Token != fixture.MonUSDC.String() {
		t.Errorf("%+v", out)
	}
	for _, it := range out.Items {
		if !it.Verified {
			t.Errorf("unverified %+v with GeckoTerminal down", it)
		}
	}
}

// With no list at all, the long tail and the Monad index are all there is, and nothing is verified
func TestSearchUncuratedFindsTheIndexedToken(t *testing.T) {
	svc, f := fixture.Uncurated(t)
	f.GeckoDown = true
	out := search(t, svc, "usdc", 8)
	if len(out.Items) != 1 || out.Items[0].Token != fixture.MonUSDC.String() || out.Items[0].Verified || out.Items[0].LogoURL != nil {
		t.Fatalf("items %+v", out.Items)
	}
	if it := out.Items[0]; *it.Symbol != "USDC" || *it.Name != "USDC" || it.Decimals != 6 || it.Chain != fixture.Monad {
		t.Errorf("item %+v", it)
	}
	if !slices.Equal(out.Coverage, []string{"token_search_long_tail_unavailable", "token_search_unverified"}) {
		t.Errorf("coverage %v", out.Coverage)
	}

	f.GeckoDown = false
	svc, _ = fixture.Uncurated(t)
	out = search(t, svc, "usdc", 20)
	for _, it := range out.Items {
		if it.Verified {
			t.Errorf("verified %+v without a list", it)
		}
	}
	// the indexed USDC is also in GeckoTerminal's answer, once, with its logo
	var n int
	for _, it := range out.Items {
		if it.Token == fixture.MonUSDC.String() {
			n++
			if it.LogoURL == nil || !strings.HasPrefix(*it.LogoURL, "https://") {
				t.Errorf("logo %v", it.LogoURL)
			}
		}
	}
	if n != 1 {
		t.Errorf("USDC on Monad %d times", n)
	}
}

// No list holds an Arc token, so the USDC of the Arc index is found unverified, after the listed tokens of the other chains
func TestSearchFindsTheArcIndex(t *testing.T) {
	svc, f := fixture.WithArc(t)
	f.GeckoDown = true
	out := search(t, svc, "usdc", 20)
	verifiedFirst(t, out)
	i := slices.IndexFunc(out.Items, func(it tokens.SearchItem) bool {
		return it.Chain == fixture.Arc && it.Token == fixture.ArcUSDC.String()
	})
	if i < 0 || out.Items[i].Verified || sym(out.Items[i]) != "USDC" || out.Items[i].Decimals != 6 {
		t.Fatalf("items %+v", out.Items)
	}
	if !slices.Equal(out.Coverage, []string{"token_search_long_tail_unavailable", "token_search_unverified"}) {
		t.Errorf("coverage %v", out.Coverage)
	}
}

func TestSearchListsNotLoaded(t *testing.T) {
	svc, _ := fixture.ListsFailed(t)
	out := search(t, svc, "usdc", 8)
	if !slices.Contains(out.Coverage, "token_search_list_unavailable") {
		t.Errorf("coverage %v", out.Coverage)
	}
	for _, it := range out.Items {
		if it.Verified {
			t.Errorf("verified %+v with no list loaded", it)
		}
	}
}

/* Lists */

type fakeFetcher struct {
	byURL map[string][]sources.ListToken
	down  map[string]bool
}

func (f *fakeFetcher) Fetch(_ context.Context, url string) ([]sources.ListToken, error) {
	if f.down[url] {
		return nil, &sources.UnavailableError{Source: "token_list", Err: errors.New("down")}
	}
	return f.byURL[url], nil
}

func TestListsKeepTheLastGoodCopy(t *testing.T) {
	cfg := fixture.Config()
	cfg.TokenLists = []config.TokenList{{Name: "first", URL: "https://a.example/list"}, {Name: "second", URL: "https://b.example/list"}}
	usdc := sources.ListToken{ChainID: 1, Address: fixture.EthUSDC, Name: "USD Coin", Symbol: "USDC", Decimals: 6}
	withLogo := usdc
	withLogo.LogoURL = "https://b.example/usdc.png"
	otherChain := sources.ListToken{ChainID: 8453, Address: fixture.MonUSDC, Name: "Base", Symbol: "BASE", Decimals: 6}
	ff := &fakeFetcher{byURL: map[string][]sources.ListToken{
		"https://a.example/list": {usdc},
		"https://b.example/list": {withLogo, otherChain},
	}, down: map[string]bool{"https://b.example/list": true}}
	l := tokens.NewLists(cfg, ff)

	if l.Complete() {
		t.Error("complete before any load")
	}
	if failed := l.Load(t.Context()); failed != 1 || l.Complete() || len(l.Snapshot()) != 1 {
		t.Fatalf("failed %d complete %v tokens %d", failed, l.Complete(), len(l.Snapshot()))
	}
	if l.Logo(fixture.Ethereum, fixture.EthUSDC) != "" {
		t.Error("a logo from a list that never loaded")
	}

	ff.down = map[string]bool{}
	if failed := l.Load(t.Context()); failed != 0 || !l.Complete() {
		t.Fatalf("failed %d", failed)
	}
	// the first list names the token, the second only adds the logo, and a chain the engine does not serve is dropped
	c, ok := l.Get(fixture.Ethereum, fixture.EthUSDC)
	if !ok || c.Rank != 0 || l.Logo(fixture.Ethereum, fixture.EthUSDC) != "https://b.example/usdc.png" || len(l.Snapshot()) != 1 {
		t.Errorf("%+v %v", c, ok)
	}

	// both lists go down: every copy stays
	ff.down = map[string]bool{"https://a.example/list": true, "https://b.example/list": true}
	if failed := l.Load(t.Context()); failed != 2 || !l.Complete() || len(l.Snapshot()) != 1 || l.Logo(fixture.Ethereum, fixture.EthUSDC) == "" {
		t.Errorf("failed %d, tokens %d", failed, len(l.Snapshot()))
	}
}

func TestListsRunRefreshes(t *testing.T) {
	cfg := fixture.Config()
	cfg.TokenLists = []config.TokenList{{Name: "first", URL: "https://a.example/list"}}
	cfg.TokenListRefresh = 20 * time.Millisecond
	ff := &countingFetcher{}
	l := tokens.NewLists(cfg, ff)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() { l.Run(ctx); close(done) }()
	deadline := time.After(2 * time.Second)
	for ff.count() < 3 {
		select {
		case <-deadline:
			t.Fatalf("%d loads", ff.count())
		case <-time.After(5 * time.Millisecond):
		}
	}
	cancel()
	<-done
}

type countingFetcher struct {
	mu sync.Mutex
	n  int
}

func (c *countingFetcher) Fetch(context.Context, string) ([]sources.ListToken, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.n++
	return []sources.ListToken{{ChainID: 1, Address: fixture.EthUSDC, Name: "USD Coin", Symbol: "USDC", Decimals: 6}}, nil
}

func (c *countingFetcher) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

/* Logos */

func recordedLogo(t *testing.T, file string, match func(map[string]any) bool, field string) string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(testkit.Read(t, file), &doc); err != nil {
		t.Fatal(err)
	}
	var out string
	var walk func(any)
	walk = func(v any) {
		switch x := v.(type) {
		case map[string]any:
			if out == "" && match(x) {
				out, _ = x[field].(string)
			}
			for _, c := range x {
				walk(c)
			}
		case []any:
			for _, c := range x {
				walk(c)
			}
		}
	}
	walk(doc)
	if out == "" {
		t.Fatalf("%s has no %s", file, field)
	}
	return out
}

func TestInfoLogoOrder(t *testing.T) {
	listed := func(chainID float64, addr evm.Address) string {
		return recordedLogo(t, "lists/uniswap.json", func(m map[string]any) bool {
			a, _ := m["address"].(string)
			return m["chainId"] == chainID && strings.EqualFold(a, addr.String())
		}, "logoURI")
	}
	svc, _ := fixture.Service(t)
	ctx := t.Context()

	// 1. the curated list, though Blockscout and GeckoTerminal have a logo too
	eth, err := svc.Info(ctx, fixture.Ethereum, fixture.EthUSDC)
	if err != nil || eth.LogoURL == nil || *eth.LogoURL != listed(1, fixture.EthUSDC) {
		t.Errorf("Ethereum USDC %v %v", eth.LogoURL, err)
	}
	mon, err := svc.Info(ctx, fixture.Monad, fixture.MonUSDC)
	if err != nil || mon.LogoURL == nil || *mon.LogoURL != listed(143, fixture.MonUSDC) {
		t.Errorf("Monad USDC %v %v", mon.LogoURL, err)
	}
	// the Monad list gives WMON its logo, GeckoTerminal's differs
	wmon, err := svc.Info(ctx, fixture.Monad, fixture.MonWMON)
	monadList := recordedLogo(t, "lists/monad.json", func(m map[string]any) bool { return m["symbol"] == "WMON" }, "logoURI")
	if err != nil || wmon.LogoURL == nil || *wmon.LogoURL != monadList {
		t.Errorf("WMON %v %v", wmon.LogoURL, err)
	}

	// 2. Blockscout's icon_url for an Ethereum token on no list
	mkr, err := svc.Info(ctx, fixture.Ethereum, fixture.EthMKR)
	icon := recordedLogo(t, "blockscout/token.mkr.json", func(m map[string]any) bool { _, ok := m["icon_url"]; return ok }, "icon_url")
	if err != nil || mkr.LogoURL == nil || *mkr.LogoURL != icon {
		t.Errorf("MKR %v %v", mkr.LogoURL, err)
	}

	// 3. GeckoTerminal's image_url for a Monad token on no list, and null when it has none
	bare, f := fixture.Uncurated(t)
	got, err := bare.Info(ctx, fixture.Monad, fixture.MonWMON)
	image := recordedLogo(t, "gecko/info.wmon.json", func(m map[string]any) bool { _, ok := m["image_url"]; return ok }, "image_url")
	if err != nil || got.LogoURL == nil || *got.LogoURL != image {
		t.Errorf("uncurated WMON %v %v", got.LogoURL, err)
	}
	bareEth, err := bare.Info(ctx, fixture.Ethereum, fixture.EthUSDC)
	usdcIcon := recordedLogo(t, "blockscout/token.usdc.json", func(m map[string]any) bool { _, ok := m["icon_url"]; return ok }, "icon_url")
	if err != nil || bareEth.LogoURL == nil || *bareEth.LogoURL != usdcIcon {
		t.Errorf("uncurated Ethereum USDC %v %v", bareEth.LogoURL, err)
	}
	f.GeckoDown = true
	none, err := bare.Info(ctx, fixture.Monad, fixture.MonUSDC)
	if err != nil || none.LogoURL != nil {
		t.Errorf("a logo from nowhere: %v %v", none.LogoURL, err)
	}
}
