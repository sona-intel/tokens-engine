package rpc

import (
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"tokens/internal/evm"
	"tokens/internal/testkit"
)

type rpcRequest struct {
	Method string `json:"method"`
	Params []any  `json:"params"`
}

// router answers like the RPC router: the recorded result for eth_call, a credits header on every answer
func router(t *testing.T, credits string, answer func(rpcRequest) []byte) (*httptest.Server, *atomic.Value) {
	t.Helper()
	var filter atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		filter.Store(r.Header.Get("X-ERPC-Use-Upstream"))
		body, _ := io.ReadAll(r.Body)
		var req rpcRequest
		if err := json.Unmarshal(body, &req); err != nil {
			t.Error(err)
		}
		w.Header().Set("X-ERPC-Credits", credits)
		_, _ = w.Write(answer(req))
	}))
	t.Cleanup(srv.Close)
	return srv, &filter
}

func TestAggregate3OnRecordedAnswer(t *testing.T) {
	srv, filter := router(t, "unknown-monad.xyz:eth_call=1", func(rpcRequest) []byte { return testkit.Read(t, "rpc/meta.monad_usdc.json") })
	c, err := New(srv.URL, WithUpstreamFilter("!drpc-*"))
	if err != nil {
		t.Fatal(err)
	}
	usdc := evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603")
	calls := []evm.Call3{
		{Target: usdc, AllowFailure: true, Data: evm.SelName[:]},
		{Target: usdc, AllowFailure: true, Data: evm.SelSymbol[:]},
		{Target: usdc, AllowFailure: true, Data: evm.SelDecimals[:]},
		{Target: usdc, AllowFailure: true, Data: evm.SelTotalSupply[:]},
	}
	res, err := c.Aggregate3(t.Context(), calls)
	if err != nil || len(res) != 4 {
		t.Fatalf("%d results, %v", len(res), err)
	}
	if sym, _ := evm.Text(res[1].Data); sym != "USDC" {
		t.Errorf("symbol %q", sym)
	}
	if filter.Load() != "!drpc-*" || c.PaidCalls() != 0 {
		t.Errorf("filter %v, paid %d", filter.Load(), c.PaidCalls())
	}
}

func TestPaidCreditsAreCounted(t *testing.T) {
	srv, _ := router(t, "drpc:eth_call=20", func(rpcRequest) []byte { return testkit.Read(t, "rpc/meta.monad_usdc.json") })
	var seen atomic.Int32
	c, err := New(srv.URL, WithOnCredits(func(_, _ string, paid bool) {
		if paid {
			seen.Add(1)
		}
	}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Call(t.Context(), evm.Multicall3, []byte{1}); err != nil {
		t.Fatal(err)
	}
	if c.PaidCalls() != 1 || seen.Load() != 1 || !strings.HasPrefix(c.LastCredits(), "drpc:") {
		t.Errorf("paid %d, seen %d", c.PaidCalls(), seen.Load())
	}
}

func TestAggregate3Batches(t *testing.T) {
	var batches atomic.Int32
	srv, _ := router(t, "", func(req rpcRequest) []byte {
		batches.Add(1)
		msg := req.Params[0].(map[string]any)
		data, _ := evm.DecodeHex(msg["data"].(string))
		// count the calls of the batch from the array length word and answer each with a failure
		n := int(data[4+63])
		if data[4+62] != 0 {
			n += int(data[4+62]) << 8
		}
		var out strings.Builder
		out.WriteString("0x" + word(32) + word(uint64(n)))
		for i := range n {
			out.WriteString(word(uint64(32*n + 96*i)))
		}
		for range n {
			out.WriteString(word(0) + word(64) + word(0))
		}
		b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": out.String()})
		return b
	})
	c, _ := New(srv.URL)
	calls := make([]evm.Call3, DefaultMaxCalls+5)
	res, err := c.Aggregate3(t.Context(), calls)
	if err != nil || len(res) != len(calls) || batches.Load() != 2 {
		t.Fatalf("%d results in %d batches: %v", len(res), batches.Load(), err)
	}
}

func TestAggregate3SplitsAtTheConfiguredSize(t *testing.T) {
	for _, tc := range []struct {
		opts    []Option
		calls   int
		batches int32
	}{
		{nil, 100, 1},
		{nil, 101, 2},
		{nil, 250, 3},
		{[]Option{WithMaxCalls(40)}, 100, 3},
	} {
		var batches atomic.Int32
		srv, _ := router(t, "", func(req rpcRequest) []byte {
			batches.Add(1)
			msg := req.Params[0].(map[string]any)
			data, _ := evm.DecodeHex(msg["data"].(string))
			n := int(data[4+63]) + int(data[4+62])<<8
			if n > tc.calls || n > DefaultMaxCalls {
				t.Errorf("a batch of %d calls", n)
			}
			var out strings.Builder
			out.WriteString("0x" + word(32) + word(uint64(n)))
			for i := range n {
				out.WriteString(word(uint64(32*n + 96*i)))
			}
			for range n {
				out.WriteString(word(0) + word(64) + word(0))
			}
			b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "result": out.String()})
			return b
		})
		c, _ := New(srv.URL, tc.opts...)
		if res, err := c.Aggregate3(t.Context(), make([]evm.Call3, tc.calls)); err != nil || len(res) != tc.calls || batches.Load() != tc.batches {
			t.Errorf("%d calls: %d results in %d batches (want %d): %v", tc.calls, len(res), batches.Load(), tc.batches, err)
		}
	}
	if _, err := New("http://x", WithMaxCalls(0)); err == nil {
		t.Error("0 calls accepted")
	}
	if _, err := New("http://x", WithMaxCalls(DefaultMaxCalls+1)); err == nil {
		t.Error("more than the default accepted")
	}
}

func word(n uint64) string {
	s := strings.Repeat("0", 64) + strconvHex(n)
	return s[len(s)-64:]
}

func strconvHex(n uint64) string {
	const digits = "0123456789abcdef"
	if n == 0 {
		return "0"
	}
	var b []byte
	for ; n > 0; n /= 16 {
		b = append([]byte{digits[n%16]}, b...)
	}
	return string(b)
}

func TestHeaderOnRecordedBlock(t *testing.T) {
	srv, _ := router(t, "", func(rpcRequest) []byte { return testkit.Read(t, "rpc/header.monad_113000000.json") })
	c, _ := New(srv.URL)
	h, err := c.HeaderByNumber(t.Context(), 113000000)
	n, at := testkit.Header(t, "rpc/header.monad_113000000.json")
	if err != nil || h.Number != n || !h.Time.Equal(at) {
		t.Fatalf("header %+v %v", h, err)
	}
	if _, err := c.HeaderByNumber(t.Context(), 5); err == nil {
		t.Error("a header for another block was accepted")
	}
	null, _ := router(t, "", func(rpcRequest) []byte { return []byte(`{"jsonrpc":"2.0","id":1,"result":null}`) })
	c2, _ := New(null.URL)
	if _, err := c2.HeaderByNumber(t.Context(), 1); !errors.Is(err, ErrNotFound) {
		t.Errorf("null block: %v", err)
	}
}

func TestEndpointNeverInErrors(t *testing.T) {
	c, err := New("http://127.0.0.1:1/sona/secret-token")
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.BlockNumber(t.Context())
	if err == nil || strings.Contains(err.Error(), "secret-token") || strings.Contains(err.Error(), "127.0.0.1:1") {
		t.Errorf("error %v", err)
	}
	if _, err := New("ftp://x"); err == nil {
		t.Error("an ftp endpoint was accepted")
	}
	if _, err := New("http://x", WithUpstreamFilter("a b")); err == nil {
		t.Error("a filter with a space was accepted")
	}
}
