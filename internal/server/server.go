package server

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"math"
	"math/rand/v2"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgconn"

	"tokens/internal/evm"
	"tokens/internal/problem"
	"tokens/internal/sources"
	"tokens/internal/tokens"
)

const (
	// nginx's code for a client that closed the request before the answer
	statusClientClosed = 499
	prefix             = "/internal/v1/chains/{chain}/tokens/{token}"

	searchPath      = "/internal/v1/tokens/search"
	defaultSearch   = 8
	maxSearch       = 20
	minQuery        = 2
	maxQuery        = 64
	defaultLimit    = 25
	maxLimit        = 100
	defaultMapLimit = 100
	minMapLimit     = 50
	maxMapLimit     = 250
	maxCursor       = 512
	defaultDeadline = 5000 * time.Millisecond
	maxDeadline     = time.Hour
)

var (
	caip2         = regexp.MustCompile(`^[-a-z0-9]{3,8}:[-_a-zA-Z0-9]{1,32}$`)
	digitsPattern = regexp.MustCompile(`^[0-9]{1,9}$`)
	cursorPattern = regexp.MustCompile(`^[A-Za-z0-9_-]+$`)
)

// Service is what the handlers read
type Service interface {
	Supported(chain string) bool
	Info(ctx context.Context, chain string, token evm.Address) (tokens.Info, error)
	Holders(ctx context.Context, chain string, token evm.Address, limit int, cursor string) (tokens.Holders, error)
	Transfers(ctx context.Context, chain string, token evm.Address, limit int) (tokens.Transfers, error)
	HolderMap(ctx context.Context, chain string, token evm.Address, limit int) (tokens.HolderMap, error)
	Search(ctx context.Context, q string, limit int) (tokens.Search, error)
	Status(ctx context.Context) (tokens.Status, error)
	Ready(ctx context.Context) bool
}

type app struct {
	svc Service
}

var routes = []struct {
	pattern string
	handler func(*app) endpoint
}{
	{"GET " + prefix, (*app).info},
	{"GET " + prefix + "/holders", (*app).holders},
	{"GET " + prefix + "/transfers", (*app).transfers},
	{"GET " + prefix + "/holder-map", (*app).holderMap},
}

func New(svc Service, m *Metrics) http.Handler {
	a := &app{svc: svc}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(http.ResponseWriter, *http.Request) {})
	mux.HandleFunc("GET /readyz", func(w http.ResponseWriter, req *http.Request) {
		if !svc.Ready(req.Context()) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}
	})
	for _, rt := range routes {
		mux.HandleFunc(rt.pattern, a.serve(rt.handler(a), true))
	}
	mux.HandleFunc("GET "+searchPath, a.serve(a.search(), false))
	if mk, ok := svc.(Markets); ok {
		for _, rt := range marketRoutes {
			mux.HandleFunc(rt.pattern, a.serve(rt.handler(a, mk), false))
		}
	}
	mux.HandleFunc("GET /internal/v1/status", a.serve(a.status(), false))
	mux.HandleFunc("/", func(w http.ResponseWriter, req *http.Request) {
		problem.Write(w, problem.New(problem.CodeNotFound, "No such route.", req.URL.EscapedPath()))
	})
	return instrument(m, mux)
}

func marshal(v any) []byte {
	// AllowInvalidUTF8 leaves no encode error to handle
	b, _ := json.Marshal(v, jsontext.AllowInvalidUTF8(true))
	return b
}

/* Middleware */

func instrument(m *Metrics, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.URL.Path == "/healthz" || req.URL.Path == "/readyz" {
			next.ServeHTTP(w, req)
			return
		}
		start := time.Now()
		w.Header().Set("X-Request-Id", requestID(req.Header.Get("traceparent")))
		sw := &statusWriter{ResponseWriter: w}
		next.ServeHTTP(sw, req)
		route := req.Pattern
		if route == "" || route == "/" {
			route = "unmatched"
		}
		m.observe(route, sw.code(), time.Since(start).Seconds())
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(b []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(b)
}

func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) code() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func requestID(traceparent string) string {
	if len(traceparent) == 55 && traceparent[:3] == "00-" && traceparent[35] == '-' && traceparent[52] == '-' &&
		isLowerHex(traceparent[3:35]) && isLowerHex(traceparent[36:52]) && isLowerHex(traceparent[53:]) &&
		strings.Trim(traceparent[3:35], "0") != "" && strings.Trim(traceparent[36:52], "0") != "" {
		return traceparent[3:35]
	}
	var b [16]byte
	binary.BigEndian.PutUint64(b[:8], rand.Uint64())
	binary.BigEndian.PutUint64(b[8:], rand.Uint64())
	return hex.EncodeToString(b[:])
}

func isLowerHex(s string) bool {
	for i := range len(s) {
		if c := s[i]; (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}

/* Serving */

type call struct {
	ctx   context.Context
	req   *http.Request
	chain string
	token evm.Address
}

type endpoint func(c *call) (any, error)

// serve checks the budget, and the chain and token of a token route, before the handler runs
func (a *app) serve(h endpoint, tokenRoute bool) http.HandlerFunc {
	return func(w http.ResponseWriter, req *http.Request) {
		budget := deadline(req.Header)
		if budget <= 0 {
			a.fail(w, req, context.DeadlineExceeded)
			return
		}
		c := &call{req: req}
		if tokenRoute {
			if err := a.checkPath(c); err != nil {
				a.fail(w, req, err)
				return
			}
		}
		ctx, cancel := context.WithTimeout(req.Context(), budget)
		defer cancel()
		c.ctx = ctx
		body, err := h(c)
		if err != nil {
			a.fail(w, req, err)
			return
		}
		raw := marshal(body)
		hdr := w.Header()
		hdr.Set("Content-Type", "application/json")
		hdr.Set("Content-Length", strconv.Itoa(len(raw)))
		_, _ = w.Write(raw)
	}
}

func (a *app) checkPath(c *call) error {
	chain := c.req.PathValue("chain")
	if !caip2.MatchString(chain) {
		return invalidID("The chain is not a CAIP-2 identifier.")
	}
	if !a.svc.Supported(chain) {
		return &httpError{problem.CodeUnsupportedChain, "Only eip155:1, eip155:143 and eip155:5042 are served."}
	}
	token, err := evm.ParseAddress(c.req.PathValue("token"))
	if err != nil {
		return invalidID("A token is 0x and 40 hex digits.")
	}
	c.chain, c.token = chain, token
	return nil
}

func (a *app) info() endpoint {
	return func(c *call) (any, error) {
		if _, err := parseQuery(c.req); err != nil {
			return nil, err
		}
		return a.svc.Info(c.ctx, c.chain, c.token)
	}
}

func (a *app) holders() endpoint {
	return func(c *call) (any, error) {
		q, err := parseQuery(c.req, "limit", "cursor")
		if err != nil {
			return nil, err
		}
		limit, err := intParam(q, "limit", defaultLimit, 1, maxLimit)
		if err != nil {
			return nil, err
		}
		cur := q.Get("cursor")
		if len(cur) > maxCursor || (cur != "" && !cursorPattern.MatchString(cur)) {
			return nil, invalid("cursor is the next_cursor of a previous page.")
		}
		return a.svc.Holders(c.ctx, c.chain, c.token, limit, cur)
	}
}

func (a *app) transfers() endpoint {
	return func(c *call) (any, error) {
		q, err := parseQuery(c.req, "limit")
		if err != nil {
			return nil, err
		}
		limit, err := intParam(q, "limit", defaultLimit, 1, maxLimit)
		if err != nil {
			return nil, err
		}
		return a.svc.Transfers(c.ctx, c.chain, c.token, limit)
	}
}

func (a *app) holderMap() endpoint {
	return func(c *call) (any, error) {
		q, err := parseQuery(c.req, "limit")
		if err != nil {
			return nil, err
		}
		limit, err := intParam(q, "limit", defaultMapLimit, minMapLimit, maxMapLimit)
		if err != nil {
			return nil, err
		}
		return a.svc.HolderMap(c.ctx, c.chain, c.token, limit)
	}
}

func (a *app) search() endpoint {
	return func(c *call) (any, error) {
		q, err := parseQuery(c.req, "q", "limit")
		if err != nil {
			return nil, err
		}
		term := strings.ToLower(strings.TrimSpace(q.Get("q")))
		if n := utf8.RuneCountInString(term); n < minQuery || n > maxQuery || strings.ContainsFunc(term, unicode.IsControl) {
			return nil, invalid("q is 2 to 64 characters once trimmed.")
		}
		limit, err := intParam(q, "limit", defaultSearch, 1, maxSearch)
		if err != nil {
			return nil, err
		}
		return a.svc.Search(c.ctx, term, limit)
	}
}

func (a *app) status() endpoint {
	return func(c *call) (any, error) {
		if _, err := parseQuery(c.req); err != nil {
			return nil, err
		}
		return a.svc.Status(c.ctx)
	}
}

/* Parameters */

type httpError struct{ code, detail string }

func (e *httpError) Error() string { return e.code + ": " + e.detail }

// invalid is for a query parameter and invalidID for a path segment
func invalid(detail string) error { return &httpError{problem.CodeInvalidQuery, detail} }

func invalidID(detail string) error { return &httpError{problem.CodeInvalidID, detail} }

func parseQuery(req *http.Request, allowed ...string) (url.Values, error) {
	q, err := url.ParseQuery(req.URL.RawQuery)
	if err != nil {
		return nil, invalid("The query string is malformed.")
	}
	for k, v := range q {
		if !slices.Contains(allowed, k) {
			return nil, invalid("Unknown parameter " + strconv.Quote(k[:min(len(k), 40)]) + ".")
		}
		if len(v) != 1 || v[0] == "" {
			return nil, invalid("Parameter " + k + " takes exactly one non-empty value.")
		}
	}
	return q, nil
}

func intParam(q url.Values, name string, def, lo, hi int) (int, error) {
	v := q.Get(name)
	if v == "" {
		return def, nil
	}
	n, err := strconv.Atoi(v)
	if !digitsPattern.MatchString(v) || err != nil || n < lo || n > hi {
		return 0, invalid(name + " is a whole number from " + strconv.Itoa(lo) + " to " + strconv.Itoa(hi) + ".")
	}
	return n, nil
}

func deadline(h http.Header) time.Duration {
	ms, err := strconv.ParseInt(h.Get("Sona-Deadline-Ms"), 10, 64)
	if err != nil {
		return defaultDeadline
	}
	limit := int64(maxDeadline / time.Millisecond)
	return time.Duration(max(min(ms, limit), -limit)) * time.Millisecond
}

/* Errors */

func (a *app) fail(w http.ResponseWriter, req *http.Request, err error) {
	instance := req.URL.EscapedPath()
	var (
		he *httpError
		ue *sources.UnavailableError
		p  problem.Problem
	)
	switch {
	case errors.As(err, &he):
		p = problem.New(he.code, he.detail, instance)
	case errors.Is(err, tokens.ErrUnsupportedChain):
		p = problem.New(problem.CodeUnsupportedChain, "Only eip155:1, eip155:143 and eip155:5042 are served.", instance)
	case errors.Is(err, tokens.ErrNotToken):
		p = problem.New(problem.CodeNotFound, "This address is not an ERC-20 token: it answers neither decimals nor totalSupply.", instance)
	case errors.Is(err, tokens.ErrBadCursor):
		p = problem.New(problem.CodeInvalidQuery, "cursor is the next_cursor of a previous page.", instance)
	case errors.Is(err, context.DeadlineExceeded):
		p = problem.New(problem.CodeDeadline, "The deadline passed before the data sources answered.", instance)
		a.logDeadline(w, req, err)
	case errors.Is(err, context.Canceled) && req.Context().Err() != nil:
		w.WriteHeader(statusClientClosed)
		return
	case errors.As(err, &ue):
		p = problem.Unavailable("The data source "+ue.Source+" did not answer.", instance, int(math.Ceil(ue.RetryAfter.Seconds())))
		a.log(w, req, err)
	case unreachable(err):
		p = problem.New(problem.CodeUpstream, "A data source did not answer.", instance)
		a.log(w, req, err)
	default:
		p = problem.New(problem.CodeInternal, "The request failed.", instance)
		a.log(w, req, err)
	}
	problem.Write(w, p)
}

func (*app) log(w http.ResponseWriter, req *http.Request, err error) {
	slog.ErrorContext(req.Context(), "request failed", "request_id", w.Header().Get("X-Request-Id"), "route", req.Pattern, "error", err)
}

// logDeadline keeps the error level for the default budget, since only a budget the caller chose is the caller's own doing
func (a *app) logDeadline(w http.ResponseWriter, req *http.Request, err error) {
	if req.Header.Get("Sona-Deadline-Ms") == "" {
		a.log(w, req, err)
		return
	}
	slog.WarnContext(req.Context(), "request past the budget of the caller", "request_id", w.Header().Get("X-Request-Id"), "route", req.Pattern, "error", err)
}

// unreachable is true for every error that is not a statement the database rejected
func unreachable(err error) bool {
	var pg *pgconn.PgError
	if !errors.As(err, &pg) {
		return true
	}
	switch pg.Code[:min(2, len(pg.Code))] {
	case "08", "53", "57":
		return true
	}
	return false
}
