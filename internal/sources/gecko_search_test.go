package sources

import (
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"

	"tokens/internal/evm"
	"tokens/internal/testkit"
)

func TestGeckoSearchRecorded(t *testing.T) {
	const q = "/search/pools?include=base_token%2Cquote_token&network="
	srv := recorded(t, map[string]string{
		q + "monad&query=usdc": "gecko/search.monad.usdc.json",
		q + "eth&query=mon":    "gecko/search.eth.mon.json",
		q + "eth&query=wmon":   "gecko/search.eth.wmon.json",
	})
	g := NewGecko(srv.URL, 6000)
	ctx := t.Context()

	hits, err := g.Search(ctx, "monad", "usdc")
	if err != nil || len(hits) == 0 {
		t.Fatalf("%d %v", len(hits), err)
	}
	// deepest liquidity first, each token once
	seen := map[string]bool{}
	for i, h := range hits {
		if seen[h.Address.String()] {
			t.Errorf("%s twice", h.Address)
		}
		seen[h.Address.String()] = true
		if i > 0 && h.ReserveUSD > hits[i-1].ReserveUSD {
			t.Errorf("hit %d is deeper than the one before", i)
		}
		if h.Symbol == "" || (h.ImageURL != "" && !strings.HasPrefix(h.ImageURL, "https://")) {
			t.Errorf("hit %+v", h)
		}
	}
	// the sum of the reserves of USDC's pools is read from the recording
	var raw geckoSearch
	if err := json.Unmarshal(testkit.Read(t, "gecko/search.monad.usdc.json"), &raw); err != nil {
		t.Fatal(err)
	}
	want := 0.0
	for _, p := range raw.Data {
		if p.Relationships.Base.Data != nil && p.Relationships.Base.Data.ID == "monad_"+monUSDC.String() {
			want += parse(t, *p.Attributes.ReserveUSD)
		}
		if p.Relationships.Quote.Data != nil && p.Relationships.Quote.Data.ID == "monad_"+monUSDC.String() {
			want += parse(t, *p.Attributes.ReserveUSD)
		}
	}
	for _, h := range hits {
		if h.Address == monUSDC && (h.ReserveUSD != want || want <= 0 || h.Decimals != 6) {
			t.Errorf("USDC reserve %v, want %v", h.ReserveUSD, want)
		}
	}

	// the recording of "mon" on Ethereum holds a token whose image is "missing_small.png", which is no https URL
	mon, err := g.Search(ctx, "eth", "mon")
	if err != nil {
		t.Fatal(err)
	}
	var missing int
	for _, h := range mon {
		if h.ImageURL != "" && !strings.HasPrefix(h.ImageURL, "https://") {
			t.Errorf("image %q", h.ImageURL)
		}
		if h.ImageURL == "" {
			missing++
		}
	}
	if missing == 0 {
		t.Error("no token without an image in the recording")
	}

	if _, err := g.Search(ctx, "eth", "nothing-recorded"); err == nil || !strings.Contains(err.Error(), "no such record") {
		t.Errorf("unrecorded search: %v", err)
	}
}

func parse(t *testing.T, s string) float64 {
	t.Helper()
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func TestLogosRecorded(t *testing.T) {
	bs := recorded(t, map[string]string{
		"/api/v2/tokens/" + ethUSDC.String(): "blockscout/token.usdc.json",
		"/api/v2/tokens/" + ethMKR.String():  "blockscout/token.mkr.json",
	})
	gt := recorded(t, map[string]string{
		"/networks/monad/tokens/" + monUSDC.String() + "/info": "gecko/info.usdc.json",
		"/networks/monad/tokens/" + monWMON.String() + "/info": "gecko/info.wmon.json",
	})
	ctx := t.Context()
	b := NewBlockscout(bs.URL, 100)
	for _, c := range []struct{ token, file string }{{ethUSDC.String(), "blockscout/token.usdc.json"}, {ethMKR.String(), "blockscout/token.mkr.json"}} {
		tok, err := b.Token(ctx, mustAddr(t, c.token))
		want := recordedField(t, c.file, "icon_url")
		if err != nil || tok.IconURL == nil || *tok.IconURL != want || !strings.HasPrefix(want, "https://") {
			t.Errorf("%s: %v %v, want %s", c.token, tok.IconURL, err, want)
		}
	}
	g := NewGecko(gt.URL, 6000)
	for _, c := range []struct{ token, file string }{{monUSDC.String(), "gecko/info.usdc.json"}, {monWMON.String(), "gecko/info.wmon.json"}} {
		h, err := g.Holders(ctx, "monad", mustAddr(t, c.token))
		want := recordedField(t, c.file, "image_url")
		if err != nil || h.ImageURL == nil || *h.ImageURL != want || !strings.HasPrefix(want, "https://") {
			t.Errorf("%s: %v %v, want %s", c.token, h.ImageURL, err, want)
		}
	}

	// the same answers with the logo turned into an address that is not https
	insecure := strings.Replace(string(testkit.Read(t, "blockscout/token.mkr.json")), recordedField(t, "blockscout/token.mkr.json", "icon_url"), "http://assets.example/mkr.png", 1)
	tok, err := NewBlockscout(serveBody(t, insecure), 100).Token(ctx, ethMKR)
	if err != nil || tok.IconURL != nil {
		t.Errorf("an http icon was kept: %v %v", tok.IconURL, err)
	}
	insecure = strings.Replace(string(testkit.Read(t, "gecko/info.usdc.json")), recordedField(t, "gecko/info.usdc.json", "image_url"), "javascript:alert(1)", 1)
	h, err := NewGecko(serveBody(t, insecure), 6000).Holders(ctx, "monad", monUSDC)
	if err != nil || h.ImageURL != nil || h.Count == nil {
		t.Errorf("a javascript image was kept: %v %v", h.ImageURL, err)
	}
}

// recordedField finds the first string member of that name anywhere in a recorded answer
func recordedField(t *testing.T, file, name string) string {
	t.Helper()
	var doc any
	if err := json.Unmarshal(testkit.Read(t, file), &doc); err != nil {
		t.Fatal(err)
	}
	var find func(any) string
	find = func(v any) string {
		switch x := v.(type) {
		case map[string]any:
			if s, ok := x[name].(string); ok {
				return s
			}
			for _, c := range x {
				if s := find(c); s != "" {
					return s
				}
			}
		case []any:
			for _, c := range x {
				if s := find(c); s != "" {
					return s
				}
			}
		}
		return ""
	}
	out := find(doc)
	if out == "" {
		t.Fatalf("%s has no %s", file, name)
	}
	return out
}

func mustAddr(t *testing.T, s string) evm.Address {
	t.Helper()
	a, err := evm.ParseAddress(s)
	if err != nil {
		t.Fatal(err)
	}
	return a
}
