// Package markets holds the chain and market data the engine serves beside its token records:
// the largest coins, the DeFi figures of a chain and its on-chain activity
package markets

import (
	"errors"
	"math"
	"time"
)

var (
	// ErrUnsupportedChain is a chain without market data
	ErrUnsupportedChain = errors.New("the chain has no market data")
	// ErrNoActivity is a chain whose activity statistics are not wired
	ErrNoActivity = errors.New("the chain has no activity statistics")
)

// Record kinds
const (
	KindTopCoins       = "top_coins"
	KindChainTVL       = "chain_tvl"
	KindChainProtocols = "chain_protocols"
	KindChainStable    = "chain_stablecoins"
	KindChainDex       = "chain_dex_volume"
	KindChainActivity  = "chain_activity"
)

// Note is one limit with its sentence
type Note struct {
	Code string `json:"code"`
	Note string `json:"note"`
}

// The coverage codes of these records
var (
	CovTopCoinsStale   = Note{"market_top_coins_stale", "CoinGecko's market list could not be refreshed for over 15 minutes, so the prices are older than 5 minutes."}
	CovTokenMapMissing = Note{"market_token_map_unavailable", "CoinGecko's list of contract addresses has not loaded, or could not be refreshed for over 3 days, so tokens may be empty or out of date."}
	CovDefiStale       = Note{"chain_defi_stale", "DefiLlama could not be refreshed for over 3 hours, so the figures are older than an hour."}
	CovActivityStale   = Note{"chain_activity_stale", "Blockscout's stats service could not be refreshed for over 3 hours, so the figures are older than an hour."}
	CovLastDayPartial  = Note{"chain_series_last_day_partial", "The last item is the current UTC day, still in progress, so it is lower than a full day and moves between reads."}
	CovNotAdditive     = Note{"chain_protocols_not_additive", "Each tvl_usd is a protocol's own TVL on the chain. DefiLlama's chain TVL leaves out double counting between protocols, so the rows do not add up to it."}
	CovChangeGlobal    = Note{"chain_protocol_change_all_chains", "change_1d_pct and change_7d_pct are DefiLlama's changes of the protocol's TVL on all its chains, not on this chain."}
)

// AllNotes is every coverage code of these records
var AllNotes = []Note{CovTopCoinsStale, CovTokenMapMissing, CovDefiStale, CovActivityStale, CovLastDayPartial, CovNotAdditive, CovChangeGlobal}

func codes(ns ...Note) []string {
	out := make([]string, 0, len(ns))
	for _, n := range ns {
		out = append(out, n.Code)
	}
	return out
}

// CoinToken is a coin's contract on a served chain
type CoinToken struct {
	Chain   string `json:"chain"`
	Address string `json:"address"`
}

// TopCoin is one coin of the top_coins record
type TopCoin struct {
	Rank         int         `json:"rank"`
	ID           string      `json:"id"`
	Symbol       string      `json:"symbol"`
	Name         string      `json:"name"`
	LogoURL      *string     `json:"logo_url"`
	PriceUSD     float64     `json:"price_usd"`
	MarketCapUSD float64     `json:"market_cap_usd"`
	Change24hPct *float64    `json:"change_24h_pct"`
	Tokens       []CoinToken `json:"tokens"`
}

// TopCoins is the top_coins record
type TopCoins struct {
	Kind     string    `json:"kind"`
	AsOf     string    `json:"as_of"`
	Items    []TopCoin `json:"items"`
	Coverage []string  `json:"coverage"`
}

// Point is a day of a chain series
type Point struct {
	Day string  `json:"day"`
	TVL float64 `json:"tvl_usd"`
}

// ChainTVL is the chain_tvl record
type ChainTVL struct {
	Kind        string   `json:"kind"`
	Chain       string   `json:"chain"`
	CurrentUSD  float64  `json:"current_usd"`
	Change1dPct *float64 `json:"change_1d_pct"`
	Change7dPct *float64 `json:"change_7d_pct"`
	Items       []Point  `json:"items"`
	Coverage    []string `json:"coverage"`
}

// Protocol is a protocol of the chain_protocols record
type Protocol struct {
	Name        string   `json:"name"`
	Slug        string   `json:"slug"`
	Category    string   `json:"category"`
	TVLUSD      float64  `json:"tvl_usd"`
	Change1dPct *float64 `json:"change_1d_pct"`
	Change7dPct *float64 `json:"change_7d_pct"`
	LogoURL     *string  `json:"logo_url"`
}

// Category is a category total of the chain_protocols record
type Category struct {
	Category  string  `json:"category"`
	TVLUSD    float64 `json:"tvl_usd"`
	Protocols int     `json:"protocols"`
}

// ChainProtocols is the chain_protocols record
type ChainProtocols struct {
	Kind       string     `json:"kind"`
	Chain      string     `json:"chain"`
	Items      []Protocol `json:"items"`
	Categories []Category `json:"categories"`
	Coverage   []string   `json:"coverage"`
}

// Stablecoin is a stablecoin of the chain_stablecoins record
type Stablecoin struct {
	Symbol         string  `json:"symbol"`
	Name           string  `json:"name"`
	CirculatingUSD float64 `json:"circulating_usd"`
}

// StablePoint is a day of the stablecoin supply
type StablePoint struct {
	Day      string  `json:"day"`
	TotalUSD float64 `json:"total_usd"`
}

// ChainStablecoins is the chain_stablecoins record
type ChainStablecoins struct {
	Kind     string        `json:"kind"`
	Chain    string        `json:"chain"`
	TotalUSD float64       `json:"total_usd"`
	Items    []Stablecoin  `json:"items"`
	History  []StablePoint `json:"history"`
	Coverage []string      `json:"coverage"`
}

// VolumePoint is a day of DEX volume
type VolumePoint struct {
	Day       string  `json:"day"`
	VolumeUSD float64 `json:"volume_usd"`
}

// DexProtocol is a DEX protocol of the chain_dex_volume record
type DexProtocol struct {
	Name         string   `json:"name"`
	Volume24hUSD float64  `json:"volume_24h_usd"`
	Volume7dUSD  *float64 `json:"volume_7d_usd"`
	LogoURL      *string  `json:"logo_url"`
}

// ChainDexVolume is the chain_dex_volume record
type ChainDexVolume struct {
	Kind        string        `json:"kind"`
	Chain       string        `json:"chain"`
	Total24hUSD *float64      `json:"total_24h_usd"`
	Total7dUSD  *float64      `json:"total_7d_usd"`
	Total30dUSD *float64      `json:"total_30d_usd"`
	Change1dPct *float64      `json:"change_1d_pct"`
	Items       []VolumePoint `json:"items"`
	Protocols   []DexProtocol `json:"protocols"`
	Coverage    []string      `json:"coverage"`
}

// ValuePoint is a day of an activity series
type ValuePoint struct {
	Day   string  `json:"day"`
	Value float64 `json:"value"`
}

// Series is one metric of the chain_activity record
type Series struct {
	Metric string       `json:"metric"`
	Items  []ValuePoint `json:"items"`
}

// ChainActivity is the chain_activity record
type ChainActivity struct {
	Kind     string   `json:"kind"`
	Chain    string   `json:"chain"`
	Series   []Series `json:"series"`
	Coverage []string `json:"coverage"`
}

func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

func day(unix int64) string { return time.Unix(unix, 0).UTC().Format(time.DateOnly) }

func pct(p *float64) *float64 {
	if p == nil || math.IsInf(*p, 0) || math.IsNaN(*p) {
		return nil
	}
	v := math.Round(*p*100) / 100
	return &v
}

// change is the percent change from old to now, nil when old is not positive
func change(now, old float64) *float64 {
	if old <= 0 {
		return nil
	}
	v := (now/old - 1) * 100
	return pct(&v)
}
