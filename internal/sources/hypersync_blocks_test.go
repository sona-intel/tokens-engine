package sources

import (
	"encoding/json/v2"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"tokens/internal/evm"
	"tokens/internal/testkit"
)

var monSystem = evm.MustAddress("0x6f49a8f621353f12378d0046e7d7e4b9b249dc9e")

func TestBlockQueryBodyMatchesRecording(t *testing.T) {
	body, err := BlockQuery{From: 113178240, To: 113178280}.body()
	if err != nil {
		t.Fatal(err)
	}
	var got, want any
	if err := errors.Join(json.Unmarshal(body, &got), json.Unmarshal(testkit.Read(t, "hypersync/monad_blocks.request.json"), &want)); err != nil {
		t.Fatal(err)
	}
	if mustJSONAny(got) != mustJSONAny(want) {
		t.Errorf("got %s\nwant %s", mustJSONAny(got), mustJSONAny(want))
	}
}

func TestBlocksReadsTheRecordedMonadPage(t *testing.T) {
	fake := &hsServer{statuses: []int{http.StatusOK}, body: testkit.Read(t, "hypersync/monad_blocks.json")}
	srv := httptest.NewServer(fake)
	defer srv.Close()
	h := NewHyperSync(map[string]string{"eip155:143": srv.URL + "/query"}, "secret-key", 600, 600)
	page, err := h.Blocks(t.Context(), "eip155:143", BlockQuery{From: 113178240, To: 113178280}, Background)
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Blocks) != 40 || len(page.Txs) != 596 || page.NextBlock != 113178280 || page.ArchiveHeight != 113400091 {
		t.Fatalf("%d blocks, %d txs, next %d, archive %d", len(page.Blocks), len(page.Txs), page.NextBlock, page.ArchiveHeight)
	}
	first, last := page.Blocks[0], page.Blocks[39]
	if first.Number != 113178240 || first.Time != 1791935994 || first.GasUsed != 0x71f798 || first.GasLimit != 0x08f0d180 || last.Time != 1791936005 {
		t.Errorf("first %+v last %+v", first, last)
	}
	system := map[uint64]int{}
	for _, tx := range page.Txs {
		if tx.From == monSystem {
			system[tx.Block]++
			if tx.GasPrice != 0 {
				t.Errorf("system transaction of block %d costs %d", tx.Block, tx.GasPrice)
			}
		}
	}
	if len(system) != 40 {
		t.Errorf("%d blocks hold a system transaction", len(system))
	}
}

func TestBlocksRejectsAPageWithAGap(t *testing.T) {
	page, err := ParseBlockPage(testkit.Read(t, "hypersync/monad_blocks.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := page.covers(113178240); err != nil {
		t.Fatal(err)
	}
	gap := page
	gap.Blocks = append(append([]Block{}, page.Blocks[:10]...), page.Blocks[11:]...)
	short := page
	short.NextBlock++
	stray := page
	stray.Txs = append(append([]Tx{}, page.Txs...), Tx{Block: 113178280})
	for name, p := range map[string]BlockPage{"a missing block": gap, "a page that ends past its blocks": short, "a transaction past the page": stray} {
		if err := p.covers(113178240); err == nil {
			t.Errorf("%s passes", name)
		}
	}
	if err := page.covers(113178241); err == nil {
		t.Error("a page that starts before the query passes")
	}
}
