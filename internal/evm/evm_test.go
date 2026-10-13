package evm_test

import (
	"bytes"
	"errors"
	"math/big"
	"strings"
	"testing"

	"tokens/internal/evm"
	"tokens/internal/testkit"
)

var metaCalls = [][4]byte{evm.SelName, evm.SelSymbol, evm.SelDecimals, evm.SelTotalSupply}

func calls(token evm.Address) []evm.Call3 {
	out := make([]evm.Call3, len(metaCalls))
	for i, sel := range metaCalls {
		out[i] = evm.Call3{Target: token, AllowFailure: true, Data: sel[:]}
	}
	return out
}

// The recorder built its calldata in Python, so equal bytes mean two encoders agree
func TestEncodeAggregate3MatchesRecordedCalldata(t *testing.T) {
	for name, token := range map[string]string{
		"eth_usdc":   "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48",
		"eth_mkr":    "0x9f8f72aa9304c8b593d555f12ef6589cc3a579a2",
		"monad_usdc": "0x754704bc059f8c67012fed69bc8a327a5aafb603",
	} {
		got := evm.EncodeAggregate3(calls(evm.MustAddress(token)))
		want := testkit.CallData(t, "rpc/meta."+name+".request.json")
		if !bytes.Equal(got, want) {
			t.Errorf("%s: calldata differs\n got %x\nwant %x", name, got, want)
		}
	}
	holders := []evm.Address{evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a"), evm.MustAddress("0x000000000000000000000000000000000000dead")}
	usdc := evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603")
	bal := []evm.Call3{
		{Target: usdc, AllowFailure: true, Data: evm.BalanceOfCall(holders[0])},
		{Target: usdc, AllowFailure: true, Data: evm.BalanceOfCall(holders[1])},
		{Target: usdc, AllowFailure: true, Data: []byte{0xde, 0xad, 0xbe, 0xef}},
	}
	if got, want := evm.EncodeAggregate3(bal), testkit.CallData(t, "rpc/balanceof.monad_usdc.request.json"); !bytes.Equal(got, want) {
		t.Errorf("balanceOf calldata differs\n got %x\nwant %x", got, want)
	}
}

func TestDecodeRecordedMetadata(t *testing.T) {
	cases := []struct {
		file, name, symbol string
		decimals           uint8
	}{
		{"eth_usdc", "USD Coin", "USDC", 6},
		{"eth_mkr", "Maker", "MKR", 18},
		{"monad_usdc", "USDC", "USDC", 6},
		{"monad_wmon", "Wrapped MON", "WMON", 18},
	}
	for _, c := range cases {
		res, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/meta."+c.file+".json"), 4)
		if err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		for i, r := range res {
			if !r.Success {
				t.Fatalf("%s: call %d failed", c.file, i)
			}
		}
		name, err1 := evm.Text(res[0].Data)
		symbol, err2 := evm.Text(res[1].Data)
		dec, err3 := evm.Decimals(res[2].Data)
		supply, err4 := evm.Uint256(res[3].Data)
		if err := errors.Join(err1, err2, err3, err4); err != nil {
			t.Fatalf("%s: %v", c.file, err)
		}
		if name != c.name || symbol != c.symbol || dec != c.decimals || supply.Sign() <= 0 {
			t.Errorf("%s: got %q %q %d %s", c.file, name, symbol, dec, supply)
		}
	}
	// MKR answers bytes32, not an ABI string
	res, _ := evm.DecodeAggregate3(testkit.Result(t, "rpc/meta.eth_mkr.json"), 4)
	if len(res[1].Data) != 32 {
		t.Errorf("MKR symbol is %d bytes, the recording should hold a bytes32", len(res[1].Data))
	}
}

// An account without code answers every call with success and no data
func TestDecodeRecordedEOA(t *testing.T) {
	res, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/meta.eth_eoa.json"), 4)
	if err != nil {
		t.Fatal(err)
	}
	for i, r := range res {
		if !r.Success || len(r.Data) != 0 {
			t.Fatalf("call %d: success %v with %d bytes", i, r.Success, len(r.Data))
		}
	}
	if _, err := evm.Decimals(res[2].Data); !errors.Is(err, evm.ErrShort) {
		t.Errorf("decimals of an EOA: %v", err)
	}
	if _, err := evm.Uint256(res[3].Data); !errors.Is(err, evm.ErrShort) {
		t.Errorf("totalSupply of an EOA: %v", err)
	}
}

func TestDecodeRecordedBalanceOf(t *testing.T) {
	res, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/balanceof.monad_usdc.json"), 3)
	if err != nil {
		t.Fatal(err)
	}
	if !res[0].Success || !res[1].Success || res[2].Success {
		t.Fatalf("success flags %v %v %v, want true true false", res[0].Success, res[1].Success, res[2].Success)
	}
	for i := range 2 {
		if _, err := evm.Uint256(res[i].Data); err != nil {
			t.Errorf("balance %d: %v", i, err)
		}
	}
	if _, err := evm.DecodeAggregate3(testkit.Result(t, "rpc/balanceof.monad_usdc.json"), 2); err == nil {
		t.Error("a wrong result count must fail")
	}
}

func TestDecodeAggregate3RejectsTruncatedData(t *testing.T) {
	raw := testkit.Result(t, "rpc/meta.eth_usdc.json")
	for _, n := range []int{0, 31, 64, len(raw) / 2, len(raw) - 1} {
		if _, err := evm.DecodeAggregate3(raw[:n], 4); err == nil {
			t.Errorf("%d of %d bytes decoded", n, len(raw))
		}
	}
}

func TestText(t *testing.T) {
	word := func(s string) []byte { b := make([]byte, 32); copy(b, s); return b }
	abiString := func(s string) []byte {
		out := make([]byte, 64)
		out[31] = 32
		out[63] = byte(len(s))
		padded := make([]byte, (len(s)+31)/32*32)
		copy(padded, s)
		return append(out, padded...)
	}
	cases := []struct {
		data []byte
		want string
		ok   bool
	}{
		{word("MKR"), "MKR", true},
		{abiString("Wrapped MON"), "Wrapped MON", true},
		{abiString("A\x00B\x07C\n"), "ABC", true},
		{abiString(strings.Repeat("x", 200)), strings.Repeat("x", evm.MaxText), true},
		{abiString("\xff\xfeok"), "ok", true},
		{[]byte{1, 2, 3}, "", false},
		{append(make([]byte, 31), 0x40, 0, 0), "", false},
	}
	for i, c := range cases {
		got, err := evm.Text(c.data)
		if (err == nil) != c.ok || got != c.want {
			t.Errorf("case %d: got %q, %v", i, got, err)
		}
	}
	// a length that runs past the data
	bad := abiString("abc")
	bad[63] = 200
	if _, err := evm.Text(bad); err == nil {
		t.Error("a string longer than its data decoded")
	}
}

func TestDecimals(t *testing.T) {
	w := make([]byte, 32)
	w[31] = 18
	if d, err := evm.Decimals(w); err != nil || d != 18 {
		t.Errorf("got %d, %v", d, err)
	}
	w[30] = 1
	if _, err := evm.Decimals(w); err == nil {
		t.Error("decimals of 274 accepted")
	}
}

func TestAddressesAndTopics(t *testing.T) {
	a, err := evm.ParseAddress("0xA0b86991c6218b36c1d19D4a2e9Eb0cE3606eB48")
	if err != nil || a.String() != "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48" {
		t.Fatalf("got %s, %v", a, err)
	}
	for _, bad := range []string{"", "0x", "a0b86991c6218b36c1d19d4a2e9eb0ce3606eb48", "0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb4", "0xg0b86991c6218b36c1d19d4a2e9eb0ce3606eb48"} {
		if _, err := evm.ParseAddress(bad); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
	var topic evm.Hash
	copy(topic[12:], a[:])
	if got, ok := evm.TopicAddress(topic); !ok || got != a {
		t.Errorf("topic address %s %v", got, ok)
	}
	topic[0] = 1
	if _, ok := evm.TopicAddress(topic); ok {
		t.Error("a topic with high bytes set read as an address")
	}
	call := evm.BalanceOfCall(a)
	if len(call) != 36 || !bytes.Equal(call[16:], a[:]) || new(big.Int).SetBytes(call[4:16]).Sign() != 0 {
		t.Errorf("balanceOf calldata %x", call)
	}
}
