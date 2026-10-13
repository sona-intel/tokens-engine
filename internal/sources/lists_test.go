package sources

import (
	"encoding/json/v2"
	"strings"
	"testing"

	"tokens/internal/testkit"
)

func TestHTTPSURL(t *testing.T) {
	cases := map[string]string{
		"https://assets.coingecko.com/coins/images/6319/large/USDC.png?1769615602": "https://assets.coingecko.com/coins/images/6319/large/USDC.png?1769615602",
		"  https://example.com/a.svg ":                                             "https://example.com/a.svg",
		"http://example.com/a.png":                                                 "",
		"ipfs://bafy/logo.png":                                                     "",
		"data:image/png;base64,AAAA":                                               "",
		"javascript:alert(1)":                                                      "",
		"//example.com/a.png":                                                      "",
		"missing_small.png":                                                        "",
		"https://":                                                                 "",
		"https://user:pw@example.com/":                                             "",
		"https://example.com/a b.png":                                              "",
		"https://example.com/\x00":                                                 "",
		"https://example.com/" + strings.Repeat("a", 600):                          "",
		"": "",
	}
	for in, want := range cases {
		if got := HTTPSURL(in); got != want {
			t.Errorf("%.40q: got %q, want %q", in, got, want)
		}
	}
}

func TestCleanText(t *testing.T) {
	if got := CleanText("US\x00D\u202e Co\u200bin\n"); got != "USD Coin" {
		t.Errorf("got %q", got)
	}
	if got := CleanText("a\xffb"); got != "ab" {
		t.Errorf("got %q", got)
	}
	long := strings.Repeat("é", 100)
	if got := CleanText(long); len(got) > 128 || !strings.HasPrefix(long, got) {
		t.Errorf("len %d", len(got))
	}
}

func TestTokenListsRecorded(t *testing.T) {
	srv := recorded(t, map[string]string{
		"/uniswap": "lists/uniswap.json", "/monad": "lists/monad.json", "/gone": "status:500",
	})
	l := NewTokenLists(5e9)
	ctx := t.Context()

	uni, err := l.Fetch(ctx, srv.URL+"/uniswap")
	if err != nil || len(uni) == 0 {
		t.Fatalf("uniswap: %d %v", len(uni), err)
	}
	var found bool
	for _, tok := range uni {
		if tok.ChainID == 1 && tok.Address == ethUSDC {
			found = tok.Symbol == "USDC" && tok.Decimals == 6 && strings.HasPrefix(tok.LogoURL, "https://")
		}
	}
	if !found {
		t.Error("Ethereum USDC is not in the Uniswap list")
	}

	mon, err := l.Fetch(ctx, srv.URL+"/monad")
	if err != nil {
		t.Fatal(err)
	}
	// the list holds the native coin MON at the zero address, which is no token and is dropped
	var raw struct {
		Tokens []struct {
			Address string `json:"address"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(testkit.Read(t, "lists/monad.json"), &raw); err != nil {
		t.Fatal(err)
	}
	if len(mon) != len(raw.Tokens)-1 {
		t.Errorf("%d tokens of %d", len(mon), len(raw.Tokens))
	}
	for _, tok := range mon {
		if tok.ChainID != 143 || tok.LogoURL == "" || tok.Symbol == "" {
			t.Errorf("entry %+v", tok)
		}
	}

	if _, err := l.Fetch(ctx, srv.URL+"/gone"); err == nil {
		t.Error("a 500 is no list")
	}
	if _, err := l.Fetch(ctx, srv.URL+"/missing"); err == nil {
		t.Error("an unknown list is an error")
	}
}

func TestTokenListDropsBadEntries(t *testing.T) {
	body := `{"tokens":[
		{"chainId":1,"address":"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48","name":"USDC","symbol":"USDC","decimals":6,"logoURI":"http://insecure.example/logo.png"},
		{"chainId":1,"address":"0xnothex","name":"Bad","symbol":"BAD","decimals":6},
		{"chainId":1,"address":"0x0000000000000000000000000000000000000000","name":"Native","symbol":"ETH","decimals":18},
		{"chainId":1,"address":"0x9f8f72aa9304c8b593d555f12ef6589cc3a579a2","name":"Maker","symbol":"MKR","decimals":300},
		{"chainId":1,"address":"0x6982508145454ce325ddbe47a25d4ec3d2311933","name":"Pepe","symbol":"","decimals":18}
	]}`
	srv := serveBody(t, body)
	out, err := NewTokenLists(5e9).Fetch(t.Context(), srv)
	if err != nil || len(out) != 1 || out[0].Symbol != "USDC" || out[0].LogoURL != "" {
		t.Fatalf("%+v %v", out, err)
	}
	if _, err := NewTokenLists(5e9).Fetch(t.Context(), serveBody(t, `{"tokens":[]}`)); err == nil {
		t.Error("an empty list must fail so that the last copy is kept")
	}
}
