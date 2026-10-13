// Package sources holds the clients of the outside data sources: Blockscout, GeckoTerminal, HyperSync and the prices service
package sources

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	userAgent   = "sona-tokens-engine/1"
	maxRESTBody = 8 << 20
)

var (
	// ErrNotFound is a 404 from a source, which knows nothing of the token
	ErrNotFound = errors.New("source has no such record")

	errTooLarge = errors.New("response body is too large")
)

// UnavailableError is a source that did not answer, RetryAfter is when it may answer again
type UnavailableError struct {
	Source     string
	RetryAfter time.Duration
	Err        error
}

func (e *UnavailableError) Error() string {
	return e.Source + " is unavailable: " + e.Err.Error()
}

func (e *UnavailableError) Unwrap() error { return e.Err }

// StatusError keeps the body out because a server can echo request headers
type StatusError struct {
	Status     int
	RetryAfter time.Duration
}

func (e *StatusError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("upstream answered HTTP %d, retry after %s", e.Status, e.RetryAfter)
	}
	return fmt.Sprintf("upstream answered HTTP %d", e.Status)
}

// noRedirect turns a 3xx into a status error so a header never travels to another URL
func noRedirect(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }

func newHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{Timeout: timeout, CheckRedirect: noRedirect}
}

func statusError(resp *http.Response) *StatusError {
	return &StatusError{Status: resp.StatusCode, RetryAfter: retryAfter(resp.Header, time.Now())}
}

// retryAfter reads Retry-After, or x-ratelimit-reset in seconds as HyperSync sends it
func retryAfter(h http.Header, now time.Time) time.Duration {
	if v := strings.TrimSpace(h.Get("Retry-After")); v != "" {
		if secs, err := strconv.ParseUint(v, 10, 32); err == nil {
			return time.Duration(secs) * time.Second
		}
		if at, err := http.ParseTime(v); err == nil && at.After(now) {
			return at.Sub(now)
		}
	}
	if v := strings.TrimSpace(h.Get("X-Ratelimit-Reset")); v != "" {
		if secs, err := strconv.ParseUint(v, 10, 32); err == nil && secs <= 3600 {
			return time.Duration(secs) * time.Second
		}
	}
	return 0
}

func readBody(r io.Reader, limit int64) ([]byte, error) {
	body, err := io.ReadAll(io.LimitReader(r, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(body)) > limit {
		return nil, errTooLarge
	}
	return body, nil
}

// getJSON reads a GET answer into out, a 404 is ErrNotFound and other failures are UnavailableError
func getJSON(ctx context.Context, hc *http.Client, source, url string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%s: request is malformed", source)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := hc.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &UnavailableError{Source: source, Err: err}
	}
	defer resp.Body.Close()
	switch {
	case resp.StatusCode == http.StatusNotFound:
		return ErrNotFound
	case resp.StatusCode != http.StatusOK:
		se := statusError(resp)
		return &UnavailableError{Source: source, RetryAfter: se.RetryAfter, Err: se}
	}
	body, err := readBody(resp.Body, maxRESTBody)
	if err != nil {
		return &UnavailableError{Source: source, Err: err}
	}
	if err := json.Unmarshal(body, out); err != nil {
		return &UnavailableError{Source: source, Err: fmt.Errorf("decode: %w", err)}
	}
	return nil
}
