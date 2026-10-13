package sources

import (
	"context"
	"math"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"tokens/internal/evm"
)

// GeckoHit is a token that GeckoTerminal found in the pools of a search
type GeckoHit struct {
	Address  evm.Address
	Name     string
	Symbol   string
	Decimals uint8
	// ImageURL is an https URL or empty
	ImageURL string
	// ReserveUSD is the sum of reserve_in_usd over the found pools the token is in
	ReserveUSD float64
}

type geckoSearch struct {
	Data []struct {
		Attributes struct {
			ReserveUSD *string `json:"reserve_in_usd"`
		} `json:"attributes"`
		Relationships struct {
			Base  geckoRel `json:"base_token"`
			Quote geckoRel `json:"quote_token"`
		} `json:"relationships"`
	} `json:"data"`
	Included []struct {
		ID         string `json:"id"`
		Type       string `json:"type"`
		Attributes struct {
			Address  string  `json:"address"`
			Name     *string `json:"name"`
			Symbol   *string `json:"symbol"`
			Decimals *int    `json:"decimals"`
			ImageURL *string `json:"image_url"`
		} `json:"attributes"`
	} `json:"included"`
}

type geckoRel struct {
	Data *struct {
		ID string `json:"id"`
	} `json:"data"`
}

// Search reads /search/pools for a network with the base and quote tokens of each pool included, and returns each
// token once with the liquidity of the pools it is in, deepest first. The pacer lets a search wait behind token pages.
func (g *Gecko) Search(ctx context.Context, network, query string) ([]GeckoHit, error) {
	if err := g.waitFor(ctx, Background); err != nil {
		return nil, err
	}
	v := url.Values{"query": {query}, "network": {network}, "include": {"base_token,quote_token"}}
	var raw geckoSearch
	if err := g.get(ctx, "/search/pools?"+v.Encode(), &raw); err != nil {
		return nil, err
	}
	return raw.convert(), nil
}

func (raw geckoSearch) convert() []GeckoHit {
	tokens := map[string]GeckoHit{}
	for _, inc := range raw.Included {
		a := inc.Attributes
		addr, err := evm.ParseAddress(a.Address)
		// the pools of the native coin name it with the zero address, which is no ERC-20 contract
		if inc.Type != "token" || err != nil || addr == (evm.Address{}) || a.Symbol == nil || a.Decimals == nil || *a.Decimals < 0 || *a.Decimals > 255 {
			continue
		}
		symbol := CleanText(*a.Symbol)
		if symbol == "" {
			continue
		}
		hit := GeckoHit{Address: addr, Symbol: symbol, Decimals: uint8(*a.Decimals)}
		if a.Name != nil {
			hit.Name = CleanText(*a.Name)
		}
		if a.ImageURL != nil {
			hit.ImageURL = HTTPSURL(*a.ImageURL)
		}
		tokens[inc.ID] = hit
	}
	sums := map[string]float64{}
	for _, p := range raw.Data {
		var reserve float64
		if r := p.Attributes.ReserveUSD; r != nil {
			if f, err := strconv.ParseFloat(strings.TrimSpace(*r), 64); err == nil && f > 0 && !math.IsInf(f, 0) {
				reserve = f
			}
		}
		for _, rel := range []geckoRel{p.Relationships.Base, p.Relationships.Quote} {
			if rel.Data != nil {
				sums[rel.Data.ID] += reserve
			}
		}
	}
	out := make([]GeckoHit, 0, len(sums))
	for id, sum := range sums {
		if hit, ok := tokens[id]; ok {
			hit.ReserveUSD = sum
			out = append(out, hit)
		}
	}
	slices.SortFunc(out, func(a, b GeckoHit) int {
		switch {
		case a.ReserveUSD > b.ReserveUSD:
			return -1
		case a.ReserveUSD < b.ReserveUSD:
			return 1
		}
		return strings.Compare(a.Address.String(), b.Address.String())
	})
	return out
}
