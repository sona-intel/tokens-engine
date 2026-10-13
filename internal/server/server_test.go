package server

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers/legacy"
	"github.com/jackc/pgx/v5/pgconn"

	"tokens/internal/evm"
	"tokens/internal/fixture"
	"tokens/internal/problem"
	"tokens/internal/sources"
	"tokens/internal/tokens"
)

const specHost = "http://127.0.0.1:8221"

// svc serves the recorded fixture, and fail replaces every answer with an error
type svc struct {
	*tokens.Service
	f     *fixture.Fakes
	fail  error
	ready bool
}

func (s *svc) Supported(chain string) bool {
	return chain == fixture.Ethereum || chain == fixture.Monad
}

func (s *svc) Ready(context.Context) bool { return s.ready }

func (s *svc) Info(ctx context.Context, chain string, token evm.Address) (tokens.Info, error) {
	if s.fail != nil {
		return tokens.Info{}, s.fail
	}
	return s.Service.Info(ctx, chain, token)
}

func (s *svc) Holders(ctx context.Context, chain string, token evm.Address, limit int, cursor string) (tokens.Holders, error) {
	if s.fail != nil {
		return tokens.Holders{}, s.fail
	}
	return s.Service.Holders(ctx, chain, token, limit, cursor)
}

func (s *svc) Transfers(ctx context.Context, chain string, token evm.Address, limit int) (tokens.Transfers, error) {
	if s.fail != nil {
		return tokens.Transfers{}, s.fail
	}
	return s.Service.Transfers(ctx, chain, token, limit)
}

func (s *svc) Search(ctx context.Context, q string, limit int) (tokens.Search, error) {
	if s.fail != nil {
		return tokens.Search{}, s.fail
	}
	return s.Service.Search(ctx, q, limit)
}

func (s *svc) Status(ctx context.Context) (tokens.Status, error) {
	if s.fail != nil {
		return tokens.Status{}, s.fail
	}
	return tokens.BuildStatus(ctx, tokens.StatusDeps{Config: fixture.Config(), HyperSync: sources.NewHyperSync(nil, "", 3, 2), PaidCalls: func() int64 { return 0 }})
}

func newSvc(t *testing.T, mutate func(*svc)) http.Handler {
	t.Helper()
	return newSvcWith(t, fixture.Service, mutate)
}

func newSvcWith(t *testing.T, build func(testing.TB) (*tokens.Service, *fixture.Fakes), mutate func(*svc)) http.Handler {
	t.Helper()
	service, f := build(t)
	s := &svc{Service: service, f: f, ready: true}
	if mutate != nil {
		mutate(s)
	}
	return New(s, NewMetrics())
}

type specCase struct {
	name, path string
	like       string
	header     []string
	status     int
	malformed  bool
	gone       bool
	mutate     func(*svc)
	// listsFailed serves a service whose curated lists could not load on start
	listsFailed bool
}

var (
	eth      = "/internal/v1/chains/eip155:1/tokens/"
	mon      = "/internal/v1/chains/eip155:143/tokens/"
	usdc     = fixture.EthUSDC.String()
	mkr      = fixture.EthMKR.String()
	monUSDC  = fixture.MonUSDC.String()
	monWMON  = fixture.MonWMON.String()
	eoa      = fixture.EthEOA.String()
	search   = "/internal/v1/tokens/search"
	failWith = func(err error) func(*svc) { return func(s *svc) { s.fail = err } }
)

func cases() []specCase {
	return []specCase{
		{name: "healthz", path: "/healthz"},
		{name: "readyz", path: "/readyz"},
		{name: "readyz with the database down", path: "/readyz", status: http.StatusServiceUnavailable, mutate: func(s *svc) { s.ready = false }},
		{name: "status", path: "/internal/v1/status"},
		{name: "info of USDC on Ethereum", path: eth + usdc},
		{name: "info of MKR in mixed case", path: eth + "0x9F8F72AA9304c8B593d555F12eF6589cC3A579A2"},
		{name: "info of USDC on Monad", path: mon + monUSDC},
		{name: "info of WMON with its sources down", path: mon + monWMON, mutate: func(s *svc) { s.f.PricesDown, s.f.GeckoDown = true, true }},
		{name: "holders on Ethereum", path: eth + usdc + "/holders"},
		{name: "holders on Ethereum across two pages", path: eth + usdc + "/holders?limit=100"},
		{name: "holders on Monad from the index", path: mon + monUSDC + "/holders?limit=5"},
		{name: "holders on Monad of a queued token", path: mon + monWMON + "/holders"},
		{name: "holders on Monad of a full index", path: mon + monWMON + "/holders", mutate: func(s *svc) { s.f.IndexFull = true }},
		{name: "holders on Monad while indexing", path: mon + monUSDC + "/holders", mutate: func(s *svc) { s.f.IndexState = "indexing" }},
		{name: "transfers on Ethereum", path: eth + mkr + "/transfers?limit=3"},
		{name: "transfers on Monad", path: mon + monUSDC + "/transfers"},
		{name: "transfers on Monad of a queued token", path: mon + monWMON + "/transfers?limit=100"},
		{name: "info of MKR with a logo from Blockscout", path: eth + mkr},
		{name: "search for usdc", path: search + "?q=usdc"},
		{name: "search for mon", path: search + "?q=mon&limit=5"},
		{name: "search for wmon", path: search + "?q=wmon&limit=20"},
		{name: "search for pepe", path: search + "?q=pepe&limit=3"},
		{name: "search with spaces and capitals", path: search + "?q=%20USDC%20"},
		{name: "search that matches nothing", path: search + "?q=zz"},
		{name: "search with GeckoTerminal down", path: search + "?q=usdc", mutate: func(s *svc) { s.f.GeckoDown = true }},
		{name: "search with the curated lists unavailable", path: search + "?q=usdc", listsFailed: true},

		{name: "limit zero", path: eth + usdc + "/holders?limit=0", status: http.StatusBadRequest, malformed: true},
		{name: "limit 101", path: mon + monUSDC + "/transfers?limit=101", status: http.StatusBadRequest, malformed: true},
		{name: "cursor with a bad character", path: eth + usdc + "/holders?cursor=a.b", status: http.StatusBadRequest, malformed: true},
		{name: "cursor this engine did not make", path: eth + usdc + "/holders?cursor=eyJ4IjoxfQ", status: http.StatusBadRequest},
		{name: "unknown parameter", path: eth + usdc + "?foo=1", status: http.StatusBadRequest},
		{name: "repeated parameter", path: eth + usdc + "/holders?limit=1&limit=2", status: http.StatusBadRequest},
		{name: "search with one character", path: search + "?q=z", status: http.StatusBadRequest, malformed: true},
		{name: "search with only spaces", path: search + "?q=%20%20%20", status: http.StatusBadRequest},
		{name: "search with 65 characters", path: search + "?q=" + strings.Repeat("a", 65), status: http.StatusBadRequest, malformed: true},
		{name: "search without q", path: search, status: http.StatusBadRequest, malformed: true},
		{name: "search with limit 21", path: search + "?q=usdc&limit=21", status: http.StatusBadRequest, malformed: true},
		{name: "search with limit zero", path: search + "?q=usdc&limit=0", status: http.StatusBadRequest, malformed: true},
		{name: "search with an unknown parameter", path: search + "?q=usdc&chain=eip155:1", status: http.StatusBadRequest},
		{name: "search with a control character", path: search + "?q=us%00dc", status: http.StatusBadRequest},
		{name: "search past its deadline", path: search + "?q=usdc", header: []string{"Sona-Deadline-Ms", "0"}, status: http.StatusGatewayTimeout},
		{name: "token that is no address", path: eth + "usdc", status: http.StatusBadRequest, malformed: true},
		{name: "chain that is no CAIP-2 id", path: "/internal/v1/chains/ethereum/tokens/" + usdc, status: http.StatusBadRequest},

		{name: "another chain", path: "/internal/v1/chains/eip155:8453/tokens/" + usdc, status: http.StatusNotFound},
		{name: "an EOA", path: eth + eoa, status: http.StatusNotFound},
		{name: "holders of an EOA", path: eth + eoa + "/holders", status: http.StatusNotFound},
		{name: "unknown route", path: "/nope", like: eth + usdc, status: http.StatusNotFound},

		{name: "transfers while the HyperSync quota is spent", path: eth + usdc + "/transfers", status: http.StatusServiceUnavailable, mutate: func(s *svc) { s.f.HyperDown = true }},
		{name: "holders with the database down", path: mon + monUSDC + "/holders", status: http.StatusServiceUnavailable, mutate: failWith(errors.New("dial tcp: connection refused"))},
		{name: "status with the database down", path: "/internal/v1/status", status: http.StatusServiceUnavailable, mutate: failWith(errors.New("dial tcp: connection refused"))},
		{name: "info past its deadline", path: eth + usdc, header: []string{"Sona-Deadline-Ms", "0"}, status: http.StatusGatewayTimeout},
		{name: "info that times out", path: eth + usdc, status: http.StatusGatewayTimeout, mutate: failWith(fmt.Errorf("read: %w", context.DeadlineExceeded))},
		{name: "info of a client that left", path: eth + usdc, status: statusClientClosed, gone: true, mutate: failWith(context.Canceled)},
		{name: "holders the database rejects", path: mon + monUSDC + "/holders", status: http.StatusInternalServerError, mutate: failWith(&pgconn.PgError{Code: "42703"})},
	}
}

func loadSpec(t testing.TB) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func TestSpecIsValid(t *testing.T) {
	doc := loadSpec(t)
	if err := doc.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	if doc.OpenAPI != "3.1.2" || len(doc.Servers) != 1 || doc.Servers[0].URL != specHost {
		t.Fatalf("openapi %q servers %v", doc.OpenAPI, doc.Servers)
	}
}

func TestSpecIsSelfContained(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "openapi.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	refs := regexp.MustCompile(`\$ref"?:\s*["']?([^"'\s,}]+)`).FindAllStringSubmatch(string(raw), -1)
	if len(refs) == 0 {
		t.Fatal("the file holds no $ref")
	}
	for _, m := range refs {
		if !strings.HasPrefix(m[1], "#/") {
			t.Errorf("$ref %s leaves the file", m[1])
		}
	}
}

func TestSpecOperationsAreTheRoutes(t *testing.T) {
	doc := loadSpec(t)
	var documented, served []string
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented = append(documented, method+" "+path)
		}
	}
	served = append(served, "GET /healthz", "GET /readyz", "GET /internal/v1/status", "GET "+searchPath)
	for _, rt := range routes {
		served = append(served, rt.pattern)
	}
	for _, rt := range marketRoutes {
		served = append(served, rt.pattern)
	}
	slices.Sort(documented)
	slices.Sort(served)
	if !slices.Equal(documented, served) {
		t.Fatalf("spec has %q, routes are %q", documented, served)
	}
}

// The coverage codes of the spec are the ones the service can state
func TestSpecCoverageCodes(t *testing.T) {
	doc := loadSpec(t)
	var spec []string
	for _, v := range doc.Components.Schemas["CoverageCode"].Value.Enum {
		spec = append(spec, v.(string))
	}
	var code []string
	for _, n := range tokens.AllNotes {
		code = append(code, n.Code)
	}
	if !slices.Equal(spec, code) {
		t.Errorf("spec %v, code %v", spec, code)
	}
}

func TestSpecValidatesResponses(t *testing.T) {
	doc := loadSpec(t)
	router, err := legacy.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cases() {
		t.Run(c.name, func(t *testing.T) {
			build := fixture.Service
			if c.listsFailed {
				build = fixture.ListsFailed
			}
			h := newSvcWith(t, build, c.mutate)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			if c.gone {
				cancel()
			}
			req := httptest.NewRequestWithContext(ctx, http.MethodGet, specHost+c.path, nil)
			for i := 0; i+1 < len(c.header); i += 2 {
				req.Header.Set(c.header[i], c.header[i+1])
			}
			lookup := req
			if c.like != "" {
				lookup = httptest.NewRequestWithContext(t.Context(), http.MethodGet, specHost+c.like, nil)
			}
			route, params, err := router.FindRoute(lookup)
			if err != nil {
				t.Fatal(err)
			}
			in := &openapi3filter.RequestValidationInput{Request: req, PathParams: params, Route: route}
			if c.like == "" {
				if err := openapi3filter.ValidateRequest(t.Context(), in); (err != nil) != c.malformed {
					t.Fatalf("malformed %v, request: %v", c.malformed, err)
				}
			}
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, req)
			if want := cmp.Or(c.status, http.StatusOK); rec.Code != want {
				t.Fatalf("status %d, want %d: %s", rec.Code, want, rec.Body)
			}
			out := &openapi3filter.ResponseValidationInput{
				RequestValidationInput: in,
				Status:                 rec.Code,
				Header:                 rec.Header(),
				Options:                &openapi3filter.Options{IncludeResponseStatus: true},
			}
			if err := openapi3filter.ValidateResponse(t.Context(), out.SetBodyBytes(rec.Body.Bytes())); err != nil {
				t.Fatalf("response: %v\n%s", err, rec.Body)
			}
			checkHeaders(t, route.Operation.OperationID, rec)
			checkDeclared(t, route.Operation, rec)
		})
	}
}

func checkHeaders(t *testing.T, operation string, rec *httptest.ResponseRecorder) {
	t.Helper()
	h := rec.Header()
	probe := operation == "getHealth" || operation == "getReady"
	if got := h.Get("X-Request-Id") != ""; got == probe {
		t.Errorf("X-Request-Id present %v on %s", got, operation)
	}
	if rec.Code >= 400 && !probe && rec.Code != statusClientClosed {
		var p problem.Problem
		if err := json.Unmarshal(rec.Body.Bytes(), &p); err != nil || h.Get("Cache-Control") != "no-store" {
			t.Errorf("problem %v, Cache-Control %q", err, h.Get("Cache-Control"))
		}
		if rec.Code == http.StatusServiceUnavailable && (h.Get("Retry-After") == "" || p.RetryAfterS < 1) {
			t.Errorf("503 without a retry hint: %s", rec.Body)
		}
	}
	if rec.Code == statusClientClosed && rec.Body.Len() != 0 {
		t.Errorf("body %q", rec.Body)
	}
}

func checkDeclared(t *testing.T, op *openapi3.Operation, rec *httptest.ResponseRecorder) {
	t.Helper()
	ref := cmp.Or(op.Responses.Status(rec.Code), op.Responses.Default())
	if ref == nil {
		t.Fatalf("%d is not declared", rec.Code)
	}
	declared := map[string]bool{}
	for name := range ref.Value.Headers {
		declared[http.CanonicalHeaderKey(name)] = true
	}
	for name := range rec.Header() {
		if !declared[name] && name != "Content-Type" && name != "Content-Length" {
			t.Errorf("%d sends %s, which the spec does not declare", rec.Code, name)
		}
	}
}

func TestHoldersCursorWalksBothChains(t *testing.T) {
	h := newSvc(t, nil)
	for _, base := range []string{eth + usdc, mon + monUSDC} {
		seen := map[string]bool{}
		path := base + "/holders?limit=7"
		for range 5 {
			rec := httptest.NewRecorder()
			h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, specHost+path, nil))
			var body tokens.Holders
			if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusOK {
				t.Fatalf("%s: %d %v", path, rec.Code, err)
			}
			for _, it := range body.Items {
				if seen[it.Address] {
					t.Fatalf("%s: %s appears twice", base, it.Address)
				}
				seen[it.Address] = true
			}
			if body.NextCursor == nil {
				break
			}
			path = base + "/holders?limit=7&cursor=" + *body.NextCursor
		}
		if len(seen) < 12 {
			t.Errorf("%s: %d holders over the pages", base, len(seen))
		}
	}
}

func TestDeadlineHeader(t *testing.T) {
	cases := map[string]int64{"": 5000, "abc": 5000, "250": 250, "-5": -5, "99999999999": 3600000}
	for v, want := range cases {
		h := http.Header{}
		if v != "" {
			h.Set("Sona-Deadline-Ms", v)
		}
		if got := deadline(h).Milliseconds(); got != want {
			t.Errorf("%q: %d", v, got)
		}
	}
}

func TestRequestID(t *testing.T) {
	if got := requestID("00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"); got != "4bf92f3577b34da6a3ce929d0e0e4736" {
		t.Errorf("got %s", got)
	}
	if got := requestID("00-00000000000000000000000000000000-00f067aa0ba902b7-01"); len(got) != 32 || got == strings.Repeat("0", 32) {
		t.Errorf("got %s", got)
	}
}
