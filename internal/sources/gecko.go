package sources

import (
	"context"
	"errors"
	"net/http"
	"strconv"
	"time"

	"tokens/internal/evm"
)

const (
	geckoName    = "geckoterminal"
	geckoTimeout = 8 * time.Second
)

// Gecko reads holder counts and market caps from GeckoTerminal's keyless public API
type Gecko struct {
	base  string
	hc    *http.Client
	pacer *Pacer
}

func NewGecko(base string, perMinute float64) *Gecko {
	return &Gecko{base: base, hc: newHTTPClient(geckoTimeout), pacer: NewPacer(perMinute, 3)}
}

func (g *Gecko) WithHTTPClient(hc *http.Client) *Gecko {
	g.hc = hc
	return g
}

// GeckoHolders is the holder block of /tokens/{token}/info, Top10Share is a fraction from 0 to 1
type GeckoHolders struct {
	Count       *int64
	Top10Share  *float64
	LastUpdated *time.Time
	// ImageURL is the token's logo when GeckoTerminal has one, as an https URL
	ImageURL *string
}

type geckoInfo struct {
	Data struct {
		Attributes struct {
			ImageURL *string `json:"image_url"`
			Holders  *struct {
				Count        *int64 `json:"count"`
				Distribution *struct {
					Top10 *string `json:"top_10"`
				} `json:"distribution_percentage"`
				LastUpdated *string `json:"last_updated"`
			} `json:"holders"`
		} `json:"attributes"`
	} `json:"data"`
}

// Holders reads /networks/{network}/tokens/{token}/info, ErrNotFound when GeckoTerminal does not know the token
func (g *Gecko) Holders(ctx context.Context, network string, token evm.Address) (GeckoHolders, error) {
	if err := g.wait(ctx); err != nil {
		return GeckoHolders{}, err
	}
	var raw geckoInfo
	if err := g.get(ctx, "/networks/"+network+"/tokens/"+token.String()+"/info", &raw); err != nil {
		return GeckoHolders{}, err
	}
	return raw.convert()
}

func (raw geckoInfo) convert() (GeckoHolders, error) {
	var out GeckoHolders
	if raw.Data.Attributes.ImageURL != nil {
		if u := HTTPSURL(*raw.Data.Attributes.ImageURL); u != "" {
			out.ImageURL = &u
		}
	}
	h := raw.Data.Attributes.Holders
	if h == nil {
		return out, nil
	}
	if h.Count != nil {
		if *h.Count < 0 {
			return GeckoHolders{}, errors.New("geckoterminal: holder count is negative")
		}
		out.Count = h.Count
	}
	if h.Distribution != nil && h.Distribution.Top10 != nil {
		pct, err := strconv.ParseFloat(*h.Distribution.Top10, 64)
		if err != nil || pct < 0 || pct > 100 {
			return GeckoHolders{}, errors.New("geckoterminal: top_10 is not a percentage")
		}
		share := pct / 100
		out.Top10Share = &share
	}
	if h.LastUpdated != nil {
		if t, err := time.Parse(time.RFC3339, *h.LastUpdated); err == nil {
			out.LastUpdated = &t
		}
	}
	return out, nil
}

type geckoToken struct {
	Data struct {
		Attributes struct {
			MarketCapUSD *string `json:"market_cap_usd"`
		} `json:"attributes"`
	} `json:"data"`
}

// MarketCap reads market_cap_usd of /networks/{network}/tokens/{token}, nil when GeckoTerminal reports none
func (g *Gecko) MarketCap(ctx context.Context, network string, token evm.Address) (*float64, error) {
	if err := g.wait(ctx); err != nil {
		return nil, err
	}
	var raw geckoToken
	if err := g.get(ctx, "/networks/"+network+"/tokens/"+token.String(), &raw); err != nil {
		return nil, err
	}
	return raw.convert()
}

func (raw geckoToken) convert() (*float64, error) {
	v := raw.Data.Attributes.MarketCapUSD
	if v == nil {
		return nil, nil
	}
	f, err := strconv.ParseFloat(*v, 64)
	if err != nil || f < 0 {
		return nil, errors.New("geckoterminal: market_cap_usd is not a number")
	}
	if f == 0 {
		return nil, nil
	}
	return &f, nil
}

func (g *Gecko) wait(ctx context.Context) error { return g.waitFor(ctx, Interactive) }

func (g *Gecko) waitFor(ctx context.Context, prio Priority) error {
	if d := g.pacer.PausedFor(); d > 0 {
		return &UnavailableError{Source: geckoName, RetryAfter: d, Err: errors.New("paused after a 429")}
	}
	return g.pacer.Wait(ctx, prio)
}

func (g *Gecko) get(ctx context.Context, path string, out any) error {
	err := getJSON(ctx, g.hc, geckoName, g.base+path, out)
	var ue *UnavailableError
	var se *StatusError
	if errors.As(err, &ue) && errors.As(err, &se) && se.Status == http.StatusTooManyRequests {
		g.pacer.Pause(max(se.RetryAfter, 30*time.Second))
	}
	return err
}
