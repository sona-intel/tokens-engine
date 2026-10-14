package sources

import (
	"cmp"
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"slices"

	"tokens/internal/evm"
)

// maxTxs only guards memory, the server ends a page much sooner on its own scan budget
const maxTxs = 50000

// BlockQuery asks for every block in [From, To) with its gas, and every transaction's block, sender and gas price
type BlockQuery struct {
	From uint64
	To   uint64
}

// Block is one block of a BlockPage, Time in Unix seconds
type Block struct {
	Number   uint64
	Time     uint64
	GasUsed  uint64
	GasLimit uint64
}

// Tx is one transaction of a BlockPage, GasPrice in wei
type Tx struct {
	Block    uint64
	From     evm.Address
	GasPrice uint64
}

// BlockPage is one answer of a BlockQuery, every block from the query's From up to NextBlock with their transactions
type BlockPage struct {
	Blocks        []Block
	Txs           []Tx
	NextBlock     uint64
	ArchiveHeight uint64
}

type hsBlockRequest struct {
	FromBlock          uint64        `json:"from_block"`
	ToBlock            uint64        `json:"to_block"`
	IncludeAllBlocks   bool          `json:"include_all_blocks"`
	Transactions       []struct{}    `json:"transactions"`
	FieldSelection     hsBlockFields `json:"field_selection"`
	MaxNumTransactions int           `json:"max_num_transactions"`
}

type hsBlockFields struct {
	Block       []string `json:"block"`
	Transaction []string `json:"transaction"`
}

func (q BlockQuery) body() ([]byte, error) {
	// one empty selection matches every transaction, and include_all_blocks keeps the blocks without one
	return json.Marshal(hsBlockRequest{
		FromBlock:        q.From,
		ToBlock:          q.To,
		IncludeAllBlocks: true,
		Transactions:     []struct{}{{}},
		FieldSelection: hsBlockFields{
			Block:       []string{"number", "timestamp", "gas_used", "gas_limit"},
			Transaction: []string{"block_number", "from", "gas_price"},
		},
		MaxNumTransactions: maxTxs,
	})
}

// Blocks sends one block query and checks that the answer holds every block it covers
func (h *HyperSync) Blocks(ctx context.Context, chain string, q BlockQuery, prio Priority) (BlockPage, error) {
	if !h.key.Set() {
		return BlockPage{}, ErrNoKey
	}
	endpoint, ok := h.urls[chain]
	if !ok {
		return BlockPage{}, fmt.Errorf("hypersync: no endpoint for %s", chain)
	}
	if q.From >= q.To {
		return BlockPage{NextBlock: q.From}, nil
	}
	body, err := q.body()
	if err != nil {
		return BlockPage{}, fmt.Errorf("hypersync request: %w", err)
	}
	raw, err := h.send(ctx, endpoint, body, prio)
	if err != nil {
		return BlockPage{}, err
	}
	page, err := ParseBlockPage(raw)
	if err != nil {
		return BlockPage{}, err
	}
	if err := page.covers(q.From); err != nil {
		return BlockPage{}, fmt.Errorf("hypersync: %w", err)
	}
	return page, nil
}

// covers checks that the blocks run without a gap from from to NextBlock and that every transaction is in one of them
func (p BlockPage) covers(from uint64) error {
	if p.NextBlock < from || uint64(len(p.Blocks)) != p.NextBlock-from {
		return fmt.Errorf("page from %d to %d holds %d blocks", from, p.NextBlock, len(p.Blocks))
	}
	for i, b := range p.Blocks {
		if b.Number != from+uint64(i) {
			return fmt.Errorf("page from %d lacks block %d", from, from+uint64(i))
		}
	}
	for _, t := range p.Txs {
		if t.Block < from || t.Block >= p.NextBlock {
			return fmt.Errorf("transaction of block %d is outside the page from %d to %d", t.Block, from, p.NextBlock)
		}
	}
	return nil
}

type hsBlockResponse struct {
	Data []struct {
		Blocks       []hsGasBlock `json:"blocks"`
		Transactions []hsTx       `json:"transactions"`
	} `json:"data"`
	ArchiveHeight *quantity `json:"archive_height"`
	NextBlock     *quantity `json:"next_block"`
}

type hsGasBlock struct {
	Number    *quantity `json:"number"`
	Timestamp *quantity `json:"timestamp"`
	GasUsed   *quantity `json:"gas_used"`
	GasLimit  *quantity `json:"gas_limit"`
}

type hsTx struct {
	BlockNumber *quantity `json:"block_number"`
	From        string    `json:"from"`
	GasPrice    *quantity `json:"gas_price"`
}

// ParseBlockPage decodes one answer of a block query, blocks in order
func ParseBlockPage(raw []byte) (BlockPage, error) {
	var resp hsBlockResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		return BlockPage{}, fmt.Errorf("hypersync: decode response: %w", err)
	}
	if resp.NextBlock == nil {
		return BlockPage{}, errors.New("hypersync: response has no next_block")
	}
	page := BlockPage{NextBlock: uint64(*resp.NextBlock)}
	if resp.ArchiveHeight != nil {
		page.ArchiveHeight = uint64(*resp.ArchiveHeight)
	}
	for _, item := range resp.Data {
		for _, b := range item.Blocks {
			if b.Number == nil || b.Timestamp == nil || b.GasUsed == nil || b.GasLimit == nil {
				return BlockPage{}, errors.New("hypersync: block lacks number, timestamp, gas_used or gas_limit")
			}
			page.Blocks = append(page.Blocks, Block{Number: uint64(*b.Number), Time: uint64(*b.Timestamp), GasUsed: uint64(*b.GasUsed), GasLimit: uint64(*b.GasLimit)})
		}
		for _, t := range item.Transactions {
			if t.BlockNumber == nil || t.GasPrice == nil {
				return BlockPage{}, errors.New("hypersync: transaction lacks block_number or gas_price")
			}
			from, err := evm.ParseAddress(t.From)
			if err != nil {
				return BlockPage{}, fmt.Errorf("hypersync: transaction of block %d: from: %w", uint64(*t.BlockNumber), err)
			}
			page.Txs = append(page.Txs, Tx{Block: uint64(*t.BlockNumber), From: from, GasPrice: uint64(*t.GasPrice)})
		}
	}
	slices.SortFunc(page.Blocks, func(a, b Block) int { return cmp.Compare(a.Number, b.Number) })
	return page, nil
}
