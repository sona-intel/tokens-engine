package holdermap

import (
	"math/big"
	"testing"

	"tokens/internal/evm"
	"tokens/internal/sources"
)

// The recordings hold no self transfer, no transfer with the zero address and no other token, so these logs are made
// here; the test needs no recorded data
func TestPairsSkipSelfZeroAndOthers(t *testing.T) {
	token := evm.MustAddress("0x1f9840a85d5af5bf1d1762f925bdaddc4201f984")
	a := evm.MustAddress("0x5a52e96bacdabb82fd05763e25335261b270efcb")
	b := evm.MustAddress("0x28c6c06298d514db089934071355e5743bf21d60")
	outsider := evm.MustAddress("0x000000000000000000000000000000000000dead")
	var zero evm.Address
	transfer := func(block uint64, tok, from, to evm.Address, amount int64) sources.Log {
		data := make([]byte, 32)
		big.NewInt(amount).FillBytes(data)
		return sources.Log{Address: tok, Block: block, Topics: []evm.Hash{evm.TopicTransfer, Topic(from), Topic(to)}, Data: data}
	}
	p := newPairs(token, []evm.Address{a, b, zero})
	p.add([]sources.Log{
		transfer(10, token, a, b, 5),
		transfer(11, token, b, a, 7),
		transfer(12, token, a, a, 100),
		transfer(13, token, zero, a, 100),
		transfer(14, token, a, outsider, 100),
		transfer(15, outsider, a, b, 100),
		transfer(30, token, a, b, 100),
	}, 10, 30)
	got := p.list()
	// a is above b, so the pair is (b, a) with both directions summed
	if len(got) != 1 || got[0].A != b || got[0].B != a || got[0].Transfers != 2 || got[0].Raw != "12" {
		t.Fatalf("pairs %+v", got)
	}
}

func TestDocRoundTrip(t *testing.T) {
	d := Doc{WindowDays: 8.8, Cut: true, Requests: 2, Nodes: 100, Pairs: []Pair{{A: addr(1), B: addr(2), Transfers: 3, Raw: "4324434389000000000000000"}}}
	raw, err := d.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseDoc(raw)
	if err != nil || back.WindowDays != 8.8 || !back.Cut || len(back.Pairs) != 1 || back.Pairs[0] != d.Pairs[0] {
		t.Fatalf("%s: %+v %v", raw, back, err)
	}
	if _, err := ParseDoc([]byte(`{"pairs":[{"a":"0x0000000000000000000000000000000000000001","b":"0x0000000000000000000000000000000000000002","transfers":1,"raw":"1.5"}]}`)); err == nil {
		t.Error("a fractional amount was read")
	}
}

func addr(n byte) evm.Address {
	var a evm.Address
	a[19] = n
	return a
}
