package rpc

import (
	"context"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"tokens/internal/evm"
)

// DefaultMaxCalls is the most calls one aggregate3 carries, the router's per-upstream budgets cannot take more
const DefaultMaxCalls = 100

type Header struct {
	Number uint64
	Time   time.Time
}

func quantity(n uint64) string { return "0x" + strconv.FormatUint(n, 16) }

func parseQuantity(s string) (uint64, error) {
	digits, ok := strings.CutPrefix(s, "0x")
	if !ok || digits == "" {
		return 0, fmt.Errorf("bad quantity %q", s)
	}
	n, err := strconv.ParseUint(digits, 16, 64)
	if err != nil {
		return 0, fmt.Errorf("bad quantity %q", s)
	}
	return n, nil
}

func decodeInto(raw []byte, out any) error {
	if err := json.Unmarshal(raw, out); err != nil {
		return errors.New("unexpected result shape")
	}
	return nil
}

// Call runs eth_call at the latest block
func (c *Client) Call(ctx context.Context, to evm.Address, data []byte) ([]byte, error) {
	msg := map[string]string{"to": to.String(), "data": "0x" + hex.EncodeToString(data)}
	raw, err := c.call(ctx, "eth_call", msg, "latest")
	if err != nil {
		return nil, err
	}
	var s string
	if err := decodeInto(raw, &s); err != nil {
		return nil, fmt.Errorf("rpc eth_call: %w", err)
	}
	out, err := evm.DecodeHex(s)
	if err != nil {
		return nil, fmt.Errorf("rpc eth_call: %w", err)
	}
	return out, nil
}

// Aggregate3 sends the calls through Multicall3 in batches of the client's maxCalls, every call may fail on its own
func (c *Client) Aggregate3(ctx context.Context, calls []evm.Call3) ([]evm.Result3, error) {
	out := make([]evm.Result3, 0, len(calls))
	for start := 0; start < len(calls); start += c.maxCalls {
		batch := calls[start:min(start+c.maxCalls, len(calls))]
		raw, err := c.Call(ctx, evm.Multicall3, evm.EncodeAggregate3(batch))
		if err != nil {
			return nil, err
		}
		res, err := evm.DecodeAggregate3(raw, len(batch))
		if err != nil {
			return nil, err
		}
		out = append(out, res...)
	}
	return out, nil
}

func (c *Client) BlockNumber(ctx context.Context) (uint64, error) {
	raw, err := c.call(ctx, "eth_blockNumber")
	if err != nil {
		return 0, err
	}
	var s string
	if err := decodeInto(raw, &s); err != nil {
		return 0, fmt.Errorf("rpc eth_blockNumber: %w", err)
	}
	n, err := parseQuantity(s)
	if err != nil {
		return 0, fmt.Errorf("rpc eth_blockNumber: %w", err)
	}
	return n, nil
}

type wireHeader struct {
	Number    string `json:"number"`
	Timestamp string `json:"timestamp"`
}

// HeaderByNumber reads the number and time of one block, ErrNotFound when the node has no such block
func (c *Client) HeaderByNumber(ctx context.Context, number uint64) (Header, error) {
	raw, err := c.call(ctx, "eth_getBlockByNumber", quantity(number), false)
	if err != nil {
		return Header{}, err
	}
	if string(raw) == "null" {
		return Header{}, fmt.Errorf("rpc eth_getBlockByNumber: block %d: %w", number, ErrNotFound)
	}
	var w wireHeader
	if err := decodeInto(raw, &w); err != nil {
		return Header{}, fmt.Errorf("rpc eth_getBlockByNumber: %w", err)
	}
	got, err := parseQuantity(w.Number)
	if err != nil {
		return Header{}, fmt.Errorf("rpc eth_getBlockByNumber: %w", err)
	}
	if got != number {
		return Header{}, fmt.Errorf("rpc eth_getBlockByNumber: node returned block %d for %d", got, number)
	}
	stamp, err := parseQuantity(w.Timestamp)
	if err != nil {
		return Header{}, fmt.Errorf("rpc eth_getBlockByNumber: %w", err)
	}
	return Header{Number: got, Time: time.Unix(int64(stamp), 0).UTC()}, nil
}
