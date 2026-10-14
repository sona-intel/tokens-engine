package tokens

import (
	"context"
	"errors"
	"math/big"
	"testing"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/rpc"
)

// searchBlock is checked against a chain of evenly and unevenly spaced blocks, the test needs no recorded data
func TestSearchBlock(t *testing.T) {
	base := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	timeOf := func(n uint64) time.Time {
		// 0.4 s blocks, with a 10 minute stall at block 500,000
		d := time.Duration(n) * 400 * time.Millisecond
		if n >= 500000 {
			d += 10 * time.Minute
		}
		return base.Add(d).Truncate(time.Second)
	}
	calls := 0
	header := func(_ context.Context, n uint64) (rpc.Header, error) {
		calls++
		return rpc.Header{Number: n, Time: timeOf(n)}, nil
	}
	for _, target := range []time.Time{base.Add(time.Hour), base.Add(55*time.Hour + 5*time.Minute), timeOf(500000).Add(-time.Minute), base.Add(100 * time.Hour)} {
		calls = 0
		got, err := searchBlock(t.Context(), header, target, 1000000)
		if err != nil {
			t.Fatal(err)
		}
		if got <= 1 || got > 1000000 {
			t.Fatalf("%s: block %d", target, got)
		}
		if timeOf(got).Before(target) || got > 1 && !timeOf(got-1).Before(target) && got != 1000000 {
			t.Errorf("%s: block %d at %s is not the first at or after it", target, got, timeOf(got))
		}
		if calls > 40 {
			t.Errorf("%s: %d header reads", target, calls)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := cursor{Page: map[string]string{"value": "100000000090000", "items_count": "50"}, Offset: 3}
	got, err := decodeCursor(*c.encode())
	if err != nil || got.Offset != 3 || got.Page["value"] != "100000000090000" {
		t.Fatalf("got %+v %v", got, err)
	}
	for _, bad := range []string{"!", "e30x", "eyJ4IjoxfQ", "eyJvIjo1MX0", "eyJvIjotMX0"} {
		if _, err := decodeCursor(bad); err == nil {
			t.Errorf("%q decoded", bad)
		}
	}
}

func TestRatioAndTimes(t *testing.T) {
	supply, _ := new(big.Int).SetString("48224833163808684", 10)
	part, _ := new(big.Int).SetString("4312358755535047", 10)
	if r := ratio(part, supply); r == nil || *r != 0.08942195281 {
		t.Errorf("share %v", r)
	}
	if ratio(part, big.NewInt(0)) != nil {
		t.Error("a share of a zero supply")
	}
	if v := times(supply, 6, 1); v == nil || *v != 48224833163.8 {
		t.Errorf("fdv %v", *v)
	}
	if times(supply, 6, 0) != nil {
		t.Error("an fdv without a price")
	}
}

type laneChain struct{ lane rpc.Lane }

func (c *laneChain) Aggregate3(ctx context.Context, calls []evm.Call3) ([]evm.Result3, error) {
	c.lane = rpc.LaneOf(ctx)
	return make([]evm.Result3, len(calls)), nil
}

func (*laneChain) HeaderByNumber(context.Context, uint64) (rpc.Header, error) {
	return rpc.Header{}, errors.New("unused")
}

func (*laneChain) BlockNumber(context.Context) (uint64, error) { return 0, errors.New("unused") }

// the pre-warm read is joined by a visitor's ensure-token, so it must not wait in the background queue
func TestPrewarmMetaReadUsesVisitorLane(t *testing.T) {
	chain := &laneChain{lane: rpc.Background}
	s := New(Deps{
		Config: config.Config{Chains: []config.Chain{{ID: "eip155:143"}}, MetaCache: time.Minute},
		Chains: map[string]Chain{"eip155:143": chain},
	})
	_, _ = s.Decimals("eip155:143")(t.Context(), evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603"))
	if chain.lane != rpc.Visitor {
		t.Fatal("the pre-warm read ran in the background lane")
	}
}
