// Package rpc is a small JSON-RPC 2.0 client for an EVM node or router, read methods only
package rpc

import (
	"bytes"
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

const (
	CodeRevert = 3

	maxAttempts          = 3
	defaultTimeout       = 10 * time.Second
	defaultBackoff       = 250 * time.Millisecond
	defaultMaxResponse   = 32 << 20
	creditsHeader        = "X-ERPC-Credits"
	paidCreditsSourceTag = "drpc:"
	upstreamHeader       = "X-ERPC-Use-Upstream"

	MaxUpstreamFilter = 128
)

var ErrNotFound = errors.New("rpc: not found")

// Error is a JSON-RPC error object, Data holds a string as is and anything else as raw JSON
type Error struct {
	Code    int
	Message string
	Data    string
}

func (e *Error) Error() string { return fmt.Sprintf("rpc error %d: %s", e.Code, e.Message) }

type HTTPError struct {
	Status int
}

func (e *HTTPError) Error() string { return fmt.Sprintf("rpc http status %d", e.Status) }

func IsRevert(err error) bool {
	var rpcErr *Error
	if !errors.As(err, &rpcErr) {
		return false
	}
	return rpcErr.Code == CodeRevert || strings.Contains(strings.ToLower(rpcErr.Message), "execution reverted")
}

/* Client */

type Client struct {
	endpoint    string
	hosts       []string
	http        *http.Client
	timeout     time.Duration
	backoff     time.Duration
	maxResponse int64
	onCredits   func(method, header string, paid bool)
	upstream    string
	pacer       *Pacer
	maxCalls    int

	nextID      atomic.Uint64
	paidCalls   atomic.Int64
	lastCredits atomic.Pointer[string]
}

type Option func(*Client) error

func WithHTTPClient(h *http.Client) Option {
	return func(c *Client) error {
		if h == nil {
			return errors.New("rpc: nil http client")
		}
		c.http = h
		return nil
	}
}

func WithTimeout(d time.Duration) Option {
	return func(c *Client) error {
		if d <= 0 {
			return errors.New("rpc: timeout must be positive")
		}
		c.timeout = d
		return nil
	}
}

func WithBackoff(d time.Duration) Option {
	return func(c *Client) error {
		if d < 0 {
			return errors.New("rpc: backoff must not be negative")
		}
		c.backoff = d
		return nil
	}
}

func WithMaxResponseBytes(n int64) Option {
	return func(c *Client) error {
		if n <= 0 {
			return errors.New("rpc: response limit must be positive")
		}
		c.maxResponse = n
		return nil
	}
}

// WithOnCredits registers a callback for every response that carries the credits header, it may run on any goroutine
func WithOnCredits(fn func(method, header string, paid bool)) Option {
	return func(c *Client) error {
		c.onCredits = fn
		return nil
	}
}

// WithPacer paces every eth_call of the client by the lane of its context
func WithPacer(p *Pacer) Option {
	return func(c *Client) error {
		if p == nil {
			return errors.New("rpc: nil pacer")
		}
		c.pacer = p
		return nil
	}
}

// WithMaxCalls sets the most calls one aggregate3 carries, 1 to DefaultMaxCalls
func WithMaxCalls(n int) Option {
	return func(c *Client) error {
		if n < 1 || n > DefaultMaxCalls {
			return fmt.Errorf("rpc: calls per aggregate3 must be 1 to %d", DefaultMaxCalls)
		}
		c.maxCalls = n
		return nil
	}
}

// WithUpstreamFilter sets the eRPC upstream filter sent on every request, empty sends none
func WithUpstreamFilter(filter string) Option {
	return func(c *Client) error {
		if !ValidUpstreamFilter(filter) {
			return errors.New("rpc: upstream filter must be empty or visible ASCII without spaces, at most 128 bytes")
		}
		c.upstream = filter
		return nil
	}
}

// ValidUpstreamFilter reports whether filter is safe as a header value, which keeps CR, LF and every control byte out
func ValidUpstreamFilter(filter string) bool {
	if len(filter) > MaxUpstreamFilter {
		return false
	}
	for i := range len(filter) {
		if filter[i] <= ' ' || filter[i] >= 0x7f {
			return false
		}
	}
	return true
}

// New never echoes the endpoint in an error because it may hold an API key
func New(endpoint string, opts ...Option) (*Client, error) {
	u, err := url.Parse(endpoint)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return nil, errors.New("rpc: endpoint must be an http or https URL")
	}
	c := &Client{
		endpoint: endpoint,
		hosts:    []string{endpoint, u.Host, u.Hostname()},
		http: &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		}},
		timeout:     defaultTimeout,
		backoff:     defaultBackoff,
		maxResponse: defaultMaxResponse,
		maxCalls:    DefaultMaxCalls,
	}
	for _, opt := range opts {
		if err := opt(c); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func (c *Client) PaidCalls() int64 { return c.paidCalls.Load() }

func (c *Client) LastCredits() string {
	if p := c.lastCredits.Load(); p != nil {
		return *p
	}
	return ""
}

/* Wire */

type envelope struct {
	Result jsontext.Value `json:"result"`
	Error  *wireError     `json:"error"`
}

type wireError struct {
	Code    int            `json:"code"`
	Message string         `json:"message"`
	Data    jsontext.Value `json:"data"`
}

func (w *wireError) toError() *Error {
	e := &Error{Code: w.Code, Message: w.Message}
	if len(w.Data) > 0 && string(w.Data) != "null" {
		var s string
		if err := json.Unmarshal(w.Data, &s); err == nil {
			e.Data = s
		} else {
			e.Data = string(w.Data)
		}
	}
	return e
}

func (c *Client) call(ctx context.Context, method string, params ...any) (jsontext.Value, error) {
	if params == nil {
		params = []any{}
	}
	body, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0",
		"id":      c.nextID.Add(1),
		"method":  method,
		"params":  params,
	})
	if err != nil {
		return nil, fmt.Errorf("rpc %s: %w", method, err)
	}
	var last error
	for attempt := range maxAttempts {
		if attempt > 0 {
			if err := sleep(ctx, c.backoff<<(attempt-1)); err != nil {
				return nil, fmt.Errorf("rpc %s: %w", method, err)
			}
		}
		result, retry, err := c.paced(ctx, method, body)
		if err == nil {
			return result, nil
		}
		if !retry {
			return nil, fmt.Errorf("rpc %s: %w", method, err)
		}
		last = err
	}
	return nil, fmt.Errorf("rpc %s: %w", method, last)
}

// paced waits for a slot of the caller's lane before an eth_call attempt and holds it until the attempt is over
func (c *Client) paced(ctx context.Context, method string, body []byte) (jsontext.Value, bool, error) {
	if c.pacer == nil || method != "eth_call" {
		return c.attempt(ctx, method, body)
	}
	release, err := c.pacer.Acquire(ctx)
	if err != nil {
		return nil, false, err
	}
	defer release()
	return c.attempt(ctx, method, body)
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

func (c *Client) attempt(parent context.Context, method string, body []byte) (jsontext.Value, bool, error) {
	ctx, cancel := context.WithTimeout(parent, c.timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, false, errors.New("cannot build request")
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "sona-tokens-engine")
	if c.upstream != "" {
		req.Header.Set(upstreamHeader, c.upstream)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, false, c.scrub(err)
	}
	defer resp.Body.Close()
	c.noteCredits(method, resp.Header.Get(creditsHeader))

	raw, err := io.ReadAll(io.LimitReader(resp.Body, c.maxResponse+1))
	if err != nil {
		return nil, false, c.scrub(err)
	}
	if int64(len(raw)) > c.maxResponse {
		return nil, false, fmt.Errorf("response is over the limit of %d bytes", c.maxResponse)
	}

	var env envelope
	parseErr := json.Unmarshal(raw, &env)
	transient := resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500
	switch {
	case parseErr == nil && env.Error != nil:
		return nil, transient, env.Error.toError()
	case resp.StatusCode != http.StatusOK:
		return nil, transient, &HTTPError{Status: resp.StatusCode}
	case parseErr != nil:
		return nil, false, errors.New("response is not a JSON-RPC envelope")
	case len(env.Result) == 0:
		return nil, false, errors.New("response has neither result nor error")
	}
	return env.Result, false, nil
}

// scrubbed keeps the error chain for errors.Is while its text never names the endpoint
type scrubbed struct {
	msg string
	err error
}

func (e *scrubbed) Error() string { return e.msg }

func (e *scrubbed) Unwrap() error { return e.err }

// scrub removes the endpoint from net/http errors because a URL or host may carry an API key
func (c *Client) scrub(err error) error {
	if ue, ok := errors.AsType[*url.Error](err); ok {
		err = ue.Err
	}
	msg := err.Error()
	for _, h := range c.hosts {
		if h != "" {
			msg = strings.ReplaceAll(msg, h, "[endpoint]")
		}
	}
	return &scrubbed{msg: msg, err: err}
}

func (c *Client) noteCredits(method, header string) {
	if header == "" {
		return
	}
	c.lastCredits.Store(&header)
	paid := namesPaidSource(header)
	if paid {
		c.paidCalls.Add(1)
	}
	if c.onCredits != nil {
		c.onCredits(method, header, paid)
	}
}

// namesPaidSource matches entries that start with the drpc source tag
func namesPaidSource(header string) bool {
	for entry := range strings.FieldsFuncSeq(header, func(r rune) bool { return r == ';' || r == ',' }) {
		if strings.HasPrefix(strings.ToLower(strings.TrimSpace(entry)), paidCreditsSourceTag) {
			return true
		}
	}
	return false
}
