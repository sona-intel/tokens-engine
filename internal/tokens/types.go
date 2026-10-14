// Package tokens builds the token records the API serves, from the chain, Blockscout, GeckoTerminal, HyperSync, the prices service and the index
package tokens

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"time"
)

var (
	// ErrNotToken is an address that answers neither decimals nor totalSupply, so it is no ERC-20
	ErrNotToken = errors.New("the address is not an ERC-20 token")
	// ErrUnsupportedChain is a chain the engine does not serve
	ErrUnsupportedChain = errors.New("the chain is not served")
	// ErrBadCursor is a cursor this engine did not make
	ErrBadCursor = errors.New("the cursor is malformed")
)

// Source names, as they appear in sources and source
const (
	SourceRPC        = "rpc"
	SourcePrices     = "prices"
	SourceBlockscout = "blockscout"
	SourceGecko      = "geckoterminal"
	SourceHyperSync  = "hypersync"
	SourceIndex      = "sona_index"
)

// Amount is a token quantity: the integer the contract holds, its decimals and the decimal value
type Amount struct {
	Raw      string `json:"raw"`
	Decimals int    `json:"decimals"`
	Value    string `json:"value"`
}

func NewAmount(raw *big.Int, decimals uint8) Amount {
	return Amount{Raw: raw.String(), Decimals: int(decimals), Value: FormatUnits(raw, decimals)}
}

// FormatUnits places the decimal point, exact and without trailing zeros
func FormatUnits(raw *big.Int, decimals uint8) string {
	neg := raw.Sign() < 0
	digits := new(big.Int).Abs(raw).String()
	d := int(decimals)
	if d > 0 {
		if len(digits) <= d {
			digits = strings.Repeat("0", d-len(digits)+1) + digits
		}
		whole, frac := digits[:len(digits)-d], strings.TrimRight(digits[len(digits)-d:], "0")
		digits = whole
		if frac != "" {
			digits += "." + frac
		}
	}
	if neg {
		return "-" + digits
	}
	return digits
}

// ratio is num/den as a float rounded to 10 significant digits, nil when den is not positive
func ratio(num, den *big.Int) *float64 {
	if den == nil || den.Sign() <= 0 || num == nil {
		return nil
	}
	f, _ := new(big.Rat).SetFrac(num, den).Float64()
	f = round(f, 10)
	return &f
}

// round keeps n significant digits so a record does not carry float noise
func round(f float64, n int) float64 {
	if f == 0 || math.IsInf(f, 0) || math.IsNaN(f) {
		return f
	}
	scale := math.Pow(10, float64(n-1-int(math.Floor(math.Log10(math.Abs(f))))))
	return math.Round(f*scale) / scale
}

// times multiplies a token amount by a dollar price
func times(raw *big.Int, decimals uint8, usd float64) *float64 {
	if raw == nil || usd <= 0 || math.IsInf(usd, 0) || math.IsNaN(usd) {
		return nil
	}
	v := new(big.Float).SetInt(raw)
	v.Quo(v, new(big.Float).SetInt(new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(decimals)), nil)))
	v.Mul(v, big.NewFloat(usd))
	f, _ := v.Float64()
	if math.IsInf(f, 0) {
		return nil
	}
	f = round(f, 12)
	return &f
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

/* Records */

// Info is the token_info record
type Info struct {
	Kind         string   `json:"kind"`
	Chain        string   `json:"chain"`
	Token        string   `json:"token"`
	Name         *string  `json:"name"`
	Symbol       *string  `json:"symbol"`
	Decimals     int      `json:"decimals"`
	TotalSupply  Amount   `json:"total_supply"`
	PriceUSD     *float64 `json:"price_usd"`
	FDVUSD       *float64 `json:"fdv_usd"`
	MarketCapUSD *float64 `json:"market_cap_usd"`
	HolderCount  *int64   `json:"holder_count"`
	Top10Share   *float64 `json:"top10_share"`
	LogoURL      *string  `json:"logo_url"`
	Sources      []string `json:"sources"`
	Coverage     []string `json:"coverage"`
}

// IndexState tells how far the engine's own index of a token is
type IndexState struct {
	State        string  `json:"state"`
	Mode         *string `json:"mode"`
	StartBlock   *uint64 `json:"start_block"`
	IndexedBlock *uint64 `json:"indexed_block"`
	HeadBlock    *uint64 `json:"head_block"`
}

const (
	IndexReady       = "ready"
	IndexIndexing    = "indexing"
	IndexQueued      = "queued"
	IndexUnavailable = "unavailable"
)

type Holder struct {
	Address    string   `json:"address"`
	Balance    Amount   `json:"balance"`
	Share      *float64 `json:"share"`
	IsContract *bool    `json:"is_contract"`
	Label      *string  `json:"label"`
}

// Holders is the token_holders record
type Holders struct {
	Kind        string      `json:"kind"`
	Chain       string      `json:"chain"`
	Token       string      `json:"token"`
	Items       []Holder    `json:"items"`
	NextCursor  *string     `json:"next_cursor"`
	HolderCount *int64      `json:"holder_count"`
	Source      string      `json:"source"`
	Index       *IndexState `json:"index"`
	Coverage    []string    `json:"coverage"`
}

type Window struct {
	FromBlock uint64 `json:"from_block"`
	ToBlock   uint64 `json:"to_block"`
	FromTime  string `json:"from_time"`
	ToTime    string `json:"to_time"`
}

type Transfer struct {
	Tx       string `json:"tx"`
	LogIndex uint64 `json:"log_index"`
	Block    uint64 `json:"block"`
	Time     string `json:"time"`
	From     string `json:"from"`
	To       string `json:"to"`
	Amount   Amount `json:"amount"`
}

// Transfers is the token_transfers record
type Transfers struct {
	Kind     string      `json:"kind"`
	Chain    string      `json:"chain"`
	Token    string      `json:"token"`
	Window   *Window     `json:"window"`
	Items    []Transfer  `json:"items"`
	Source   string      `json:"source"`
	Index    *IndexState `json:"index"`
	Coverage []string    `json:"coverage"`
}

/* Coverage */

// Note is one limit with its sentence, the status lists them all
type Note struct {
	Code string `json:"code"`
	Note string `json:"note"`
}

var (
	CovNoPrice            = Note{"token_price_unknown", "The prices service has no price for this token, so price_usd and fdv_usd are null."}
	CovSourceDown         = Note{"token_source_unavailable", "A data source did not answer, so the fields it feeds are null. sources lists the ones that answered."}
	CovMultichainCap      = Note{"token_market_cap_multichain", "The provider's market cap is above this chain's fully diluted value, so it counts the token's supply on other chains too."}
	CovIndexPartial       = Note{"token_index_partial", "The index of this token is still being built, so the list holds what is indexed so far, up to index.indexed_block."}
	CovIndexFull          = Note{"token_index_unavailable", "The index cannot take this token now (it holds the most tokens it may, or its backfill failed and waits for a retry), so the list is empty."}
	CovFromActivity       = Note{"token_holders_from_activity", "The token's full history is too long to index on the free HyperSync quota, so the index follows the addresses active since index.start_block and reads their balances with balanceOf. A holder that has not moved the token since then is missing."}
	CovFromTransfers      = Note{"token_balances_from_transfers", "Balances are the sum of the token's Transfer events (and Deposit and Withdrawal for wrapped MON). A token that changes balances without events, such as a rebasing token, drifts from balanceOf."}
	CovWindowCut          = Note{"token_transfer_window_cut", "The token is busy, so the window is shorter than the default to stay within the HyperSync quota. window gives its blocks and times."}
	CovWindowOlder        = Note{"token_transfer_window_older", "HyperSync did not answer, so the transfers are the last window that was read, up to 6 hours and 15 minutes ago. window.to_time is the time it ends; it is not the latest day."}
	CovGeckoHolders       = Note{"token_holder_count_provider", "holder_count and top10_share come from GeckoTerminal, which updates them on its own schedule."}
	CovSearchListDown     = Note{"token_search_list_unavailable", "A curated token list has not loaded yet, so verified tokens from it may be missing from the search."}
	CovSearchLongTailDown = Note{"token_search_long_tail_unavailable", "GeckoTerminal did not answer in time for a chain, so tokens that are on no curated list may be missing from the search."}
	CovSearchUnverified   = Note{"token_search_unverified", "Tokens with verified false are on no curated list. Anyone can deploy a token with any name, so check the address. They are ranked by the pool liquidity GeckoTerminal reports, or not at all when it reports none."}
	CovHoldersPartial     = Note{"token_holders_ranked_by_provider", "Holders, their order and holder_count come from Blockscout's index, read when the page was fetched and kept for up to 10 minutes."}

	CovMapIndexIncomplete   = Note{"token_holder_map_index_incomplete", "The index of this token is not complete: its backfill has not finished, or it follows only the addresses active since its start block. A map of part of the holders would mislead, so nodes is empty."}
	CovMapLinksEthereumOnly = Note{"token_holder_map_links_ethereum_only", "Links between holders are read from HyperSync on Ethereum only, so links is unavailable on this chain."}
	CovMapWindowCut         = Note{"token_holder_map_window_cut", "The token is busy, so the HyperSync request budget of one map read fewer days than the default window. links.window_days gives the days read."}
	CovMapQueueFull         = Note{"token_holder_map_queue_full", "Too many maps wait for their links, so this one was not queued and links is unavailable. Ask again in a few minutes."}
	CovMapLinksFailed       = Note{"token_holder_map_links_failed", "The last read of the links failed because HyperSync did not answer, so links is unavailable until the map is read again 10 minutes after the failure."}
)

// AllNotes is every coverage code the engine can state, in the order the status lists them
var AllNotes = []Note{
	CovNoPrice, CovSourceDown, CovMultichainCap, CovIndexPartial, CovIndexFull, CovFromActivity, CovFromTransfers, CovWindowCut, CovWindowOlder, CovGeckoHolders, CovHoldersPartial,
	CovSearchListDown, CovSearchLongTailDown, CovSearchUnverified,
	CovMapIndexIncomplete, CovMapLinksEthereumOnly, CovMapWindowCut, CovMapQueueFull, CovMapLinksFailed,
}

func codes(ns ...Note) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Code)
	}
	return out
}
