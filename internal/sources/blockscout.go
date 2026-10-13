package sources

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"net/url"
	"strconv"
	"time"

	"tokens/internal/evm"
)

const (
	blockscoutName    = "blockscout"
	blockscoutTimeout = 8 * time.Second
	// BlockscoutPage is the number of holders Blockscout gives per page
	BlockscoutPage = 50
)

// Blockscout reads token records and holder pages from a keyless Blockscout instance
type Blockscout struct {
	base  string
	hc    *http.Client
	pacer *Pacer
}

func NewBlockscout(base string, perSecond float64) *Blockscout {
	return &Blockscout{base: base, hc: newHTTPClient(blockscoutTimeout), pacer: NewPacer(perSecond*60, 3)}
}

// WithHTTPClient swaps the client, for tests
func (b *Blockscout) WithHTTPClient(hc *http.Client) *Blockscout {
	b.hc = hc
	return b
}

type BlockscoutToken struct {
	HoldersCount         *int64
	CirculatingMarketCap *float64
	TotalSupply          *big.Int
	Type                 string
}

type bsToken struct {
	HoldersCount         *string `json:"holders_count"`
	CirculatingMarketCap *string `json:"circulating_market_cap"`
	TotalSupply          *string `json:"total_supply"`
	Type                 string  `json:"type"`
}

// Token reads /api/v2/tokens/{token}
func (b *Blockscout) Token(ctx context.Context, token evm.Address) (BlockscoutToken, error) {
	if err := b.pacer.Wait(ctx, Interactive); err != nil {
		return BlockscoutToken{}, err
	}
	var raw bsToken
	if err := getJSON(ctx, b.hc, blockscoutName, b.base+"/api/v2/tokens/"+token.String(), &raw); err != nil {
		return BlockscoutToken{}, err
	}
	return raw.convert()
}

func (raw bsToken) convert() (BlockscoutToken, error) {
	out := BlockscoutToken{Type: raw.Type}
	if raw.HoldersCount != nil {
		n, err := strconv.ParseInt(*raw.HoldersCount, 10, 64)
		if err != nil || n < 0 {
			return BlockscoutToken{}, errors.New("blockscout: holders_count is not a count")
		}
		out.HoldersCount = &n
	}
	if raw.CirculatingMarketCap != nil {
		f, err := strconv.ParseFloat(*raw.CirculatingMarketCap, 64)
		if err != nil || f < 0 {
			return BlockscoutToken{}, errors.New("blockscout: circulating_market_cap is not a number")
		}
		if f > 0 {
			out.CirculatingMarketCap = &f
		}
	}
	if raw.TotalSupply != nil {
		v, ok := new(big.Int).SetString(*raw.TotalSupply, 10)
		if !ok || v.Sign() < 0 {
			return BlockscoutToken{}, errors.New("blockscout: total_supply is not an integer")
		}
		out.TotalSupply = v
	}
	return out, nil
}

type BlockscoutHolder struct {
	Address    evm.Address
	Value      *big.Int
	IsContract *bool
	Label      string
}

// BlockscoutPage is one page of holders, Next is nil on the last page
type BlockscoutHolders struct {
	Items []BlockscoutHolder
	Next  url.Values
}

type bsHolders struct {
	Items []struct {
		Address struct {
			Hash       string  `json:"hash"`
			IsContract *bool   `json:"is_contract"`
			Name       *string `json:"name"`
			Metadata   *struct {
				Tags []struct {
					Name    string `json:"name"`
					TagType string `json:"tagType"`
				} `json:"tags"`
			} `json:"metadata"`
		} `json:"address"`
		Value string `json:"value"`
	} `json:"items"`
	NextPageParams jsontext.Value `json:"next_page_params"`
}

// Holders reads one page of /api/v2/tokens/{token}/holders, page is the next_page_params of the page before or nil for the first
func (b *Blockscout) Holders(ctx context.Context, token evm.Address, page url.Values) (BlockscoutHolders, error) {
	if err := b.pacer.Wait(ctx, Interactive); err != nil {
		return BlockscoutHolders{}, err
	}
	u := b.base + "/api/v2/tokens/" + token.String() + "/holders"
	if len(page) > 0 {
		u += "?" + page.Encode()
	}
	var raw bsHolders
	if err := getJSON(ctx, b.hc, blockscoutName, u, &raw); err != nil {
		return BlockscoutHolders{}, err
	}
	return raw.convert()
}

func (raw bsHolders) convert() (BlockscoutHolders, error) {
	out := BlockscoutHolders{Items: make([]BlockscoutHolder, 0, len(raw.Items))}
	for i, it := range raw.Items {
		addr, err := evm.ParseAddress(it.Address.Hash)
		if err != nil {
			return BlockscoutHolders{}, fmt.Errorf("blockscout: holder %d: %w", i, err)
		}
		v, ok := new(big.Int).SetString(it.Value, 10)
		if !ok || v.Sign() < 0 {
			return BlockscoutHolders{}, fmt.Errorf("blockscout: holder %d: value is not an integer", i)
		}
		h := BlockscoutHolder{Address: addr, Value: v, IsContract: it.Address.IsContract}
		h.Label = label(it.Address.Name, it.Address.Metadata)
		out.Items = append(out.Items, h)
	}
	next, err := pageParams(raw.NextPageParams)
	if err != nil {
		return BlockscoutHolders{}, err
	}
	out.Next = next
	return out, nil
}

// label prefers the first name tag of the metadata, then the verified contract name
func label(name *string, meta *struct {
	Tags []struct {
		Name    string `json:"name"`
		TagType string `json:"tagType"`
	} `json:"tags"`
},
) string {
	if meta != nil {
		for _, t := range meta.Tags {
			if t.TagType == "name" && t.Name != "" {
				return clip(t.Name)
			}
		}
	}
	if name != nil {
		return clip(*name)
	}
	return ""
}

func clip(s string) string {
	r := []rune(s)
	if len(r) > 80 {
		r = r[:80]
	}
	return string(r)
}

// pageParams flattens next_page_params into query values, null is the last page
func pageParams(raw jsontext.Value) (url.Values, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var m map[string]jsontext.Value
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, errors.New("blockscout: next_page_params is not an object")
	}
	out := url.Values{}
	for k, v := range m {
		switch v.Kind() {
		case '"':
			var s string
			if err := json.Unmarshal(v, &s); err != nil {
				return nil, fmt.Errorf("blockscout: next_page_params.%s: %w", k, err)
			}
			out.Set(k, s)
		case '0':
			out.Set(k, string(v))
		case 'n':
		default:
			return nil, fmt.Errorf("blockscout: next_page_params.%s is neither a string nor a number", k)
		}
	}
	return out, nil
}
