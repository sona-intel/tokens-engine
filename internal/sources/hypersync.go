package sources

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/rand/v2"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
)

const (
	hyperSyncName    = "hypersync"
	hyperSyncTimeout = 90 * time.Second
	maxHyperBody     = 64 << 20
	// maxLogs only guards memory, the server ends a page much sooner on its own scan budget
	maxLogs = 50000

	backgroundAttempts = 30
	// interactiveAttempts lets a visitor's query wait out one or two quota windows, it runs detached from the visitor
	interactiveAttempts = 3
	serverErrorPause    = 10 * time.Second
)

var ErrNoKey = errors.New("hypersync key is not set")

// LogQuery asks for the logs of some contracts with some topic0 values in [From, To)
type LogQuery struct {
	From      uint64
	To        uint64
	Addresses []evm.Address
	Topics0   []evm.Hash
	// Blocks also selects the timestamps of the blocks that hold a log
	Blocks bool
}

type Log struct {
	Address  evm.Address
	Topics   []evm.Hash
	Data     []byte
	Block    uint64
	LogIndex uint64
	TxHash   evm.Hash
	// Time is the block time in Unix seconds, 0 when the query did not select blocks
	Time uint64
}

// Page is one answer, NextBlock is the first block it did not cover
type Page struct {
	Logs          []Log
	NextBlock     uint64
	ArchiveHeight uint64
}

// HyperSyncStats counts requests by outcome, for the status route and metrics
type HyperSyncStats struct {
	Requests    int64
	RateLimited int64
	Failed      int64
}

// HyperSync is the Envio HyperSync client of every chain, its pacers are shared because the key has one quota for all chains
type HyperSync struct {
	urls map[string]string
	key  config.Secret
	hc   *http.Client
	// background work and visitors' requests each keep to their own share of the quota
	pacers [2]*Pacer

	requests    atomic.Int64
	rateLimited atomic.Int64
	failed      atomic.Int64
	sleep       func(context.Context, time.Duration) error
}

// NewHyperSync takes the query URL of each chain by CAIP-2 id and the requests a minute for background work and for visitors
func NewHyperSync(urls map[string]string, key config.Secret, background, interactive float64) *HyperSync {
	h := &HyperSync{urls: urls, key: key, hc: newHTTPClient(hyperSyncTimeout), sleep: sleepCtx}
	// a burst of a minute's share lets the engine send its requests in the first seconds of a quota window,
	// before x402-indexer has spent the window
	h.pacers[Background] = NewPacer(background, max(int(background), 1))
	h.pacers[Interactive] = NewPacer(interactive, max(int(interactive), 1))
	return h
}

func (h *HyperSync) WithHTTPClient(hc *http.Client) *HyperSync {
	h.hc = hc
	return h
}

// WithPacers swaps the pacers and the sleep between retries, for tests
func (h *HyperSync) WithPacers(background, interactive *Pacer, sleep func(context.Context, time.Duration) error) *HyperSync {
	h.pacers[Background], h.pacers[Interactive], h.sleep = background, interactive, sleep
	return h
}

// PausedFor is how long the engine still waits after a 429
func (h *HyperSync) PausedFor() time.Duration {
	return max(h.pacers[Background].PausedFor(), h.pacers[Interactive].PausedFor())
}

// Without these fmt prints the key field raw because it cannot call methods on it
func (h *HyperSync) String() string { return "hypersync" }

func (h *HyperSync) GoString() string { return h.String() }

func (h *HyperSync) Enabled() bool { return h.key.Set() }

func (h *HyperSync) Stats() HyperSyncStats {
	return HyperSyncStats{Requests: h.requests.Load(), RateLimited: h.rateLimited.Load(), Failed: h.failed.Load()}
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

/* Requests */

type hsRequest struct {
	FromBlock      uint64        `json:"from_block"`
	ToBlock        uint64        `json:"to_block"`
	Logs           []hsLogFilter `json:"logs"`
	FieldSelection hsFields      `json:"field_selection"`
	MaxNumLogs     int           `json:"max_num_logs"`
}

type hsLogFilter struct {
	Address []string   `json:"address"`
	Topics  [][]string `json:"topics"`
}

type hsFields struct {
	Log   []string `json:"log"`
	Block []string `json:"block,omitempty"`
}

var logFields = []string{"block_number", "log_index", "transaction_hash", "address", "data", "topic0", "topic1", "topic2"}

func (q LogQuery) body() ([]byte, error) {
	addrs := make([]string, len(q.Addresses))
	for i, a := range q.Addresses {
		addrs[i] = a.String()
	}
	topics := make([]string, len(q.Topics0))
	for i, t := range q.Topics0 {
		topics[i] = t.String()
	}
	fields := hsFields{Log: logFields}
	if q.Blocks {
		fields.Block = []string{"number", "timestamp"}
	}
	return json.Marshal(hsRequest{
		FromBlock:      q.From,
		ToBlock:        q.To,
		Logs:           []hsLogFilter{{Address: addrs, Topics: [][]string{topics}}},
		FieldSelection: fields,
		MaxNumLogs:     maxLogs,
	})
}

// Page sends one query and waits out a 429, up to interactiveAttempts or backgroundAttempts tries
func (h *HyperSync) Page(ctx context.Context, chain string, q LogQuery, prio Priority) (Page, error) {
	if !h.key.Set() {
		return Page{}, ErrNoKey
	}
	endpoint, ok := h.urls[chain]
	if !ok {
		return Page{}, fmt.Errorf("hypersync: no endpoint for %s", chain)
	}
	if q.From >= q.To {
		return Page{NextBlock: q.From}, nil
	}
	if len(q.Addresses) == 0 || len(q.Topics0) == 0 {
		return Page{}, errors.New("hypersync: a query needs addresses and topics")
	}
	body, err := q.body()
	if err != nil {
		return Page{}, fmt.Errorf("hypersync request: %w", err)
	}
	attempts := backgroundAttempts
	if prio == Interactive {
		attempts = interactiveAttempts
	}
	for attempt := 1; ; attempt++ {
		if err := h.pacers[prio].Wait(ctx, prio); err != nil {
			return Page{}, err
		}
		h.requests.Add(1)
		raw, hdr, err := h.post(ctx, endpoint, body)
		h.noteQuota(hdr)
		if err == nil {
			return ParsePage(raw)
		}
		var se *StatusError
		if !errors.As(err, &se) {
			h.failed.Add(1)
			if ctx.Err() != nil {
				return Page{}, ctx.Err()
			}
			if attempt >= attempts {
				return Page{}, &UnavailableError{Source: hyperSyncName, RetryAfter: 5 * time.Second, Err: err}
			}
			if err := h.sleep(ctx, serverErrorPause); err != nil {
				return Page{}, err
			}
			continue
		}
		wait := se.RetryAfter
		switch {
		case se.Status == http.StatusTooManyRequests:
			h.rateLimited.Add(1)
			wait = max(wait, time.Second)
		case se.Status >= 500:
			h.failed.Add(1)
			wait = max(wait, serverErrorPause)
		default:
			h.failed.Add(1)
			return Page{}, fmt.Errorf("hypersync: %w", se)
		}
		if attempt >= attempts {
			return Page{}, &UnavailableError{Source: hyperSyncName, RetryAfter: wait, Err: se}
		}
		if se.Status >= 500 {
			if err := h.sleep(ctx, wait); err != nil {
				return Page{}, err
			}
		}
	}
}

// noteQuota pauses both pacers until the quota window resets when a response says the window is spent.
// The key's quota is a fixed window shared with x402-indexer, which spends it within seconds of each reset,
// so a request that waits for the reset is the one that gets through.
func (h *HyperSync) noteQuota(hdr http.Header) {
	if hdr == nil {
		return
	}
	remaining, err1 := strconv.ParseInt(firstValue(hdr.Get("X-Ratelimit-Remaining")), 10, 64)
	cost, err2 := strconv.ParseInt(firstValue(hdr.Get("X-Ratelimit-Cost")), 10, 64)
	if err1 != nil || remaining > 0 && (err2 != nil || remaining >= cost) {
		return
	}
	reset := 5 * time.Second
	if secs, err := strconv.ParseUint(firstValue(hdr.Get("X-Ratelimit-Reset")), 10, 32); err == nil && secs <= 3600 {
		reset = time.Duration(secs) * time.Second
	}
	// the header counts whole seconds, so the window may reset up to a second later than it says
	wait := reset + time.Second + time.Duration(rand.Int64N(int64(500*time.Millisecond)))
	h.pacers[Background].Pause(wait)
	h.pacers[Interactive].Pause(wait)
}

// firstValue reads the first entry of a header such as "15000, 15000;w=60"
func firstValue(v string) string {
	v, _, _ = strings.Cut(v, ",")
	return strings.TrimSpace(v)
}

func (h *HyperSync) post(ctx context.Context, endpoint string, body []byte) ([]byte, http.Header, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, nil, errors.New("hypersync request is malformed")
	}
	req.Header.Set("Authorization", "Bearer "+h.key.Reveal())
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", userAgent)
	resp, err := h.hc.Do(req)
	if err != nil {
		return nil, nil, fmt.Errorf("hypersync: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, resp.Header, statusError(resp)
	}
	raw, err := readBody(resp.Body, maxHyperBody)
	if err != nil {
		return nil, resp.Header, fmt.Errorf("hypersync: %w", err)
	}
	return raw, resp.Header, nil
}

// Height reads the archive height, which the server answers without spending the request quota
func (h *HyperSync) Height(ctx context.Context, chain string) (uint64, error) {
	if !h.key.Set() {
		return 0, ErrNoKey
	}
	endpoint, ok := h.urls[chain]
	if !ok {
		return 0, fmt.Errorf("hypersync: no endpoint for %s", chain)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimSuffix(endpoint, "/query")+"/height", nil)
	if err != nil {
		return 0, errors.New("hypersync request is malformed")
	}
	req.Header.Set("Authorization", "Bearer "+h.key.Reveal())
	req.Header.Set("User-Agent", userAgent)
	resp, err := h.hc.Do(req)
	if err != nil {
		return 0, fmt.Errorf("hypersync height: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("hypersync height: %w", statusError(resp))
	}
	raw, err := readBody(resp.Body, 1<<10)
	if err != nil {
		return 0, fmt.Errorf("hypersync height: %w", err)
	}
	var out struct {
		Height *quantity `json:"height"`
	}
	if err := json.Unmarshal(raw, &out); err != nil || out.Height == nil {
		return 0, errors.New("hypersync height: answer has no height")
	}
	return uint64(*out.Height), nil
}

/* Responses */

type hsResponse struct {
	Data []struct {
		Logs   []hsLog   `json:"logs"`
		Blocks []hsBlock `json:"blocks"`
	} `json:"data"`
	ArchiveHeight *quantity `json:"archive_height"`
	NextBlock     *quantity `json:"next_block"`
}

type hsLog struct {
	BlockNumber *quantity `json:"block_number"`
	LogIndex    *quantity `json:"log_index"`
	TxHash      string    `json:"transaction_hash"`
	Address     string    `json:"address"`
	Data        string    `json:"data"`
	Topic0      *string   `json:"topic0"`
	Topic1      *string   `json:"topic1"`
	Topic2      *string   `json:"topic2"`
}

type hsBlock struct {
	Number    *quantity `json:"number"`
	Timestamp *quantity `json:"timestamp"`
}

// ParsePage decodes one /query answer
func ParsePage(raw []byte) (Page, error) {
	var resp hsResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return Page{}, fmt.Errorf("hypersync: decode response: %w", err)
	}
	if resp.NextBlock == nil {
		return Page{}, errors.New("hypersync: response has no next_block")
	}
	stamps := map[uint64]uint64{}
	n := 0
	for _, item := range resp.Data {
		n += len(item.Logs)
		for _, b := range item.Blocks {
			if b.Number != nil && b.Timestamp != nil {
				stamps[uint64(*b.Number)] = uint64(*b.Timestamp)
			}
		}
	}
	page := Page{NextBlock: uint64(*resp.NextBlock), Logs: make([]Log, 0, n)}
	if resp.ArchiveHeight != nil {
		page.ArchiveHeight = uint64(*resp.ArchiveHeight)
	}
	for _, item := range resp.Data {
		for _, l := range item.Logs {
			log, err := l.log(stamps)
			if err != nil {
				return Page{}, fmt.Errorf("hypersync: %w", err)
			}
			page.Logs = append(page.Logs, log)
		}
	}
	slices.SortFunc(page.Logs, func(a, b Log) int {
		return cmp.Or(cmp.Compare(a.Block, b.Block), cmp.Compare(a.LogIndex, b.LogIndex))
	})
	return page, nil
}

func (l hsLog) log(stamps map[uint64]uint64) (Log, error) {
	if l.BlockNumber == nil || l.LogIndex == nil || l.Topic0 == nil {
		return Log{}, errors.New("log lacks block_number, log_index or topic0")
	}
	out := Log{Block: uint64(*l.BlockNumber), LogIndex: uint64(*l.LogIndex), Time: stamps[uint64(*l.BlockNumber)]}
	var err error
	if out.TxHash, err = evm.ParseHash(l.TxHash); err != nil {
		return Log{}, fmt.Errorf("log %d/%d: transaction_hash: %w", out.Block, out.LogIndex, err)
	}
	if out.Address, err = evm.ParseAddress(l.Address); err != nil {
		return Log{}, fmt.Errorf("log %d/%d: address: %w", out.Block, out.LogIndex, err)
	}
	if out.Data, err = evm.DecodeHex(l.Data); err != nil {
		return Log{}, fmt.Errorf("log %d/%d: data: %w", out.Block, out.LogIndex, err)
	}
	for _, t := range []*string{l.Topic0, l.Topic1, l.Topic2} {
		if t == nil {
			break
		}
		topic, err := evm.ParseHash(*t)
		if err != nil {
			return Log{}, fmt.Errorf("log %d/%d: topic: %w", out.Block, out.LogIndex, err)
		}
		out.Topics = append(out.Topics, topic)
	}
	return out, nil
}

// HyperSync mixes JSON numbers, decimal strings and 0x hex for the same kind of field
type quantity uint64

func (q *quantity) UnmarshalJSON(b []byte) error {
	s := string(b)
	if len(s) >= 2 && s[0] == '"' && s[len(s)-1] == '"' {
		s = s[1 : len(s)-1]
	}
	base := 10
	if digits, ok := strings.CutPrefix(s, "0x"); ok {
		s, base = digits, 16
	}
	n, err := strconv.ParseUint(s, base, 64)
	if err != nil {
		return errors.New("not an unsigned number")
	}
	*q = quantity(n)
	return nil
}
