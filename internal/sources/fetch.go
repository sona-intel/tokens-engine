package sources

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"
)

// maxStreamBody bounds an answer that is decoded while it is read
const maxStreamBody = 64 << 20

// fetchStream sends a GET and hands the body to decode while it is read, so a large answer is never held whole.
// A 404 is ErrNotFound, other failures are UnavailableError. headers may be nil.
func fetchStream(ctx context.Context, hc *http.Client, source, url string, headers map[string]string, decode func(io.Reader) error) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return fmt.Errorf("%s: request is malformed", source)
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
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
	if err := decode(io.LimitReader(resp.Body, maxStreamBody)); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return &UnavailableError{Source: source, Err: fmt.Errorf("decode: %w", err)}
	}
	return nil
}

// fetchJSON reads a small GET answer into out, with extra headers
func fetchJSON(ctx context.Context, hc *http.Client, source, url string, headers map[string]string, out any) error {
	return fetchStream(ctx, hc, source, url, headers, func(r io.Reader) error {
		body, err := readBody(r, maxRESTBody)
		if err != nil {
			return err
		}
		return json.Unmarshal(body, out)
	})
}

// pausedPacer waits for a slot of a background reader, and refuses at once while a source asked for a pause
func pausedPacer(ctx context.Context, p *Pacer, source string) error {
	if d := p.PausedFor(); d > 0 {
		return &UnavailableError{Source: source, RetryAfter: d, Err: errors.New("paused after a 429")}
	}
	return p.Wait(ctx, Background)
}

// pauseOn429 stops the pacer for what the source asked, at least minimum
func pauseOn429(p *Pacer, err error, minimum time.Duration) {
	var se *StatusError
	if errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		p.Pause(max(se.RetryAfter, minimum))
	}
}
