package sources

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"io"
	"net/http"
	"strings"
	"time"
)

const (
	coingeckoName    = "coingecko"
	coingeckoTimeout = 45 * time.Second
	coingeckoKeyHdr  = "x-cg-demo-api-key"
)

// CoinGecko reads the market list and the platform list of CoinGecko's public API, at a pace the keyless tier allows
type CoinGecko struct {
	base  string
	key   string
	hc    *http.Client
	pacer *Pacer
}

// NewCoinGecko paces requests at one every gap. key is the optional demo key, sent only to base
func NewCoinGecko(base, key string, gap time.Duration) *CoinGecko {
	return &CoinGecko{base: base, key: key, hc: newHTTPClient(coingeckoTimeout), pacer: NewPacer(float64(time.Minute)/float64(gap), 1)}
}

// WithHTTPClient swaps the client, for tests
func (c *CoinGecko) WithHTTPClient(hc *http.Client) *CoinGecko {
	c.hc = hc
	return c
}

func (c *CoinGecko) headers() map[string]string {
	if c.key == "" {
		return nil
	}
	return map[string]string{coingeckoKeyHdr: c.key}
}

// CoinMarket is one row of /coins/markets
type CoinMarket struct {
	ID           string   `json:"id"`
	Symbol       string   `json:"symbol"`
	Name         string   `json:"name"`
	Image        string   `json:"image"`
	CurrentPrice *float64 `json:"current_price"`
	MarketCap    *float64 `json:"market_cap"`
	Rank         *int     `json:"market_cap_rank"`
	Change24h    *float64 `json:"price_change_percentage_24h"`
	LastUpdated  string   `json:"last_updated"`
}

// Markets reads /coins/markets: the 100 largest coins by market cap in USD with their 24 hour change
func (c *CoinGecko) Markets(ctx context.Context) ([]CoinMarket, error) {
	if err := pausedPacer(ctx, c.pacer, coingeckoName); err != nil {
		return nil, err
	}
	var out []CoinMarket
	err := fetchJSON(ctx, c.hc, coingeckoName, c.base+"/coins/markets?vs_currency=usd&order=market_cap_desc&per_page=100&page=1&price_change_percentage=24h", c.headers(), &out)
	pauseOn429(c.pacer, err, time.Minute)
	return out, err
}

// Platforms reads /coins/list?include_platform=true, about 4 MB, element by element, and keeps only the asked platform keys
// of the coins that have at least one of them: coin id -> platform key -> contract address as sent
func (c *CoinGecko) Platforms(ctx context.Context, keys ...string) (map[string]map[string]string, error) {
	if err := pausedPacer(ctx, c.pacer, coingeckoName); err != nil {
		return nil, err
	}
	out := map[string]map[string]string{}
	err := fetchStream(ctx, c.hc, coingeckoName, c.base+"/coins/list?include_platform=true", c.headers(), func(r io.Reader) error {
		dec := jsontext.NewDecoder(r)
		if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '[' {
			return errNotArray(err)
		}
		for dec.PeekKind() != ']' {
			var el struct {
				ID        string            `json:"id"`
				Platforms map[string]string `json:"platforms"`
			}
			if err := json.UnmarshalDecode(dec, &el); err != nil {
				return err
			}
			var kept map[string]string
			for _, k := range keys {
				if a := strings.TrimSpace(el.Platforms[k]); a != "" {
					if kept == nil {
						kept = make(map[string]string, len(keys))
					}
					kept[k] = a
				}
			}
			if kept != nil {
				out[el.ID] = kept
			}
		}
		return nil
	})
	pauseOn429(c.pacer, err, time.Minute)
	if err != nil {
		return nil, err
	}
	return out, nil
}
