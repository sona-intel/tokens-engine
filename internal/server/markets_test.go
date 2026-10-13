package server

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"

	"tokens/internal/fixture"
	"tokens/internal/markets"
)

// mkSvc serves the token fixture and the recorded market sources
type mkSvc struct {
	*svc
	*markets.Service
}

func newMarketsHandler(t *testing.T, loaded bool) http.Handler {
	t.Helper()
	service, f := fixture.Service(t)
	var m *markets.Service
	if loaded {
		m, _ = fixture.Markets(t)
	} else {
		m, _ = fixture.NewMarkets(t)
	}
	return New(&mkSvc{svc: &svc{Service: service, f: f, ready: true}, Service: m}, NewMetrics())
}

const (
	chainEth = "/internal/v1/chains/eip155:1"
	chainMon = "/internal/v1/chains/eip155:143"
)

type marketCase struct {
	name, path string
	status     int
	unloaded   bool
	header     []string
}

func marketCases() []marketCase {
	return []marketCase{
		{name: "top coins", path: "/internal/v1/markets/top-coins"},
		{name: "top 3 coins", path: "/internal/v1/markets/top-coins?limit=3"},
		{name: "top coins with a limit of 0", path: "/internal/v1/markets/top-coins?limit=0", status: 400},
		{name: "top coins with an unknown parameter", path: "/internal/v1/markets/top-coins?page=2", status: 400},
		{name: "top coins before the first read", path: "/internal/v1/markets/top-coins", status: 503, unloaded: true},
		{name: "top coins past the budget", path: "/internal/v1/markets/top-coins", status: 504, header: []string{"Sona-Deadline-Ms", "0"}},
		{name: "tvl of Monad", path: chainMon + "/defi/tvl"},
		{name: "tvl of Ethereum over all days", path: chainEth + "/defi/tvl?days=all"},
		{name: "tvl over 7 days", path: chainEth + "/defi/tvl?days=7", status: 400},
		{name: "tvl of another chain", path: "/internal/v1/chains/eip155:8453/defi/tvl", status: 404},
		{name: "tvl of no chain", path: "/internal/v1/chains/base/defi/tvl", status: 400},
		{name: "tvl before the first read", path: chainMon + "/defi/tvl", status: 503, unloaded: true},
		{name: "protocols of Ethereum", path: chainEth + "/defi/protocols"},
		{name: "protocols of Monad", path: chainMon + "/defi/protocols?limit=5"},
		{name: "protocols with a limit of 101", path: chainMon + "/defi/protocols?limit=101", status: 400},
		{name: "stablecoins of Monad", path: chainMon + "/defi/stablecoins"},
		{name: "stablecoins of Ethereum", path: chainEth + "/defi/stablecoins"},
		{name: "stablecoins with a parameter", path: chainEth + "/defi/stablecoins?days=30", status: 400},
		{name: "dex volume of Monad", path: chainMon + "/defi/dexs"},
		{name: "dex volume of Ethereum over a year", path: chainEth + "/defi/dexs?days=365"},
		{name: "dex volume over all days", path: chainEth + "/defi/dexs?days=all", status: 400},
		{name: "activity of Ethereum", path: chainEth + "/activity"},
		{name: "activity of Ethereum over 30 days", path: chainEth + "/activity?days=30"},
		{name: "activity of Monad", path: chainMon + "/activity", status: 404},
		{name: "activity of another chain", path: "/internal/v1/chains/eip155:10/activity", status: 404},
	}
}

func TestMarketRoutesFollowTheSpec(t *testing.T) {
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range marketCases() {
		t.Run(c.name, func(t *testing.T) {
			h := newMarketsHandler(t, !c.unloaded)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, specHost+c.path, nil)
			for i := 0; i+1 < len(c.header); i += 2 {
				req.Header.Set(c.header[i], c.header[i+1])
			}
			route, params, err := router.FindRoute(req)
			if err != nil {
				t.Fatal(err)
			}
			in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if want := cmp.Or(c.status, http.StatusOK); rec.Code != want {
				t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body)
			}
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in, Status: rec.Code, Header: rec.Header(),
				Options: &openapi3filter.Options{IncludeResponseStatus: true},
			}
			if err := openapi3filter.ValidateResponse(t.Context(), out.SetBodyBytes(rec.Body.Bytes())); err != nil {
				t.Fatalf("response: %v\n%s", err, rec.Body)
			}
			checkHeaders(t, route.Operation.OperationID, rec)
			checkDeclared(t, route.Operation, rec)
		})
	}
}

func TestSpecMarketCoverageCodes(t *testing.T) {
	doc := loadSpec(t)
	var spec, code []string
	for _, v := range doc.Components.Schemas["MarketCoverageCode"].Value.Enum {
		spec = append(spec, v.(string))
	}
	for _, n := range markets.AllNotes {
		code = append(code, n.Code)
	}
	if !slices.Equal(spec, code) {
		t.Errorf("spec %v, code %v", spec, code)
	}
}

func TestMarketProblems(t *testing.T) {
	h := newMarketsHandler(t, true)
	get := func(path string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, specHost+path, nil))
		var body map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		return rec.Code, body
	}
	code, body := get(chainMon + "/activity")
	if code != http.StatusNotFound || body["code"] != "unsupported_chain" || body["detail"] == "" {
		t.Errorf("monad activity: %d %v", code, body)
	}
	code, body = get("/internal/v1/chains/eip155:10/defi/tvl")
	if code != http.StatusNotFound || body["code"] != "unsupported_chain" {
		t.Errorf("tvl: %d %v", code, body)
	}
	code, body = get(chainEth + "/defi/tvl?days=7")
	if code != http.StatusBadRequest || body["code"] != "invalid_query" {
		t.Errorf("days: %d %v", code, body)
	}
}

func TestMarketRoutesNeedMarkets(t *testing.T) {
	h := newSvc(t, nil)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, specHost+"/internal/v1/markets/top-coins", nil))
	if rec.Code != http.StatusNotFound {
		t.Errorf("status %d", rec.Code)
	}
}
