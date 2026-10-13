package sources

import (
	"context"
	"errors"
	"net/http"
	"time"

	"tokens/internal/evm"
)

const (
	pricesName    = "prices"
	pricesTimeout = 6 * time.Second
)

// Prices reads the current price of a coin from Sona's own prices service
type Prices struct {
	base string
	hc   *http.Client
}

func NewPrices(base string) *Prices {
	return &Prices{base: base, hc: newHTTPClient(pricesTimeout)}
}

func (p *Prices) WithHTTPClient(hc *http.Client) *Prices {
	p.hc = hc
	return p
}

type pricesBody struct {
	Current *struct {
		USD *float64 `json:"usd"`
	} `json:"current"`
}

// Current is the price in US dollars, nil when the prices service knows no price for the coin
func (p *Prices) Current(ctx context.Context, chain string, token evm.Address) (*float64, error) {
	var raw pricesBody
	err := getJSON(ctx, p.hc, pricesName, p.base+"/internal/v1/chains/"+chain+"/tokens/"+token.String()+"/prices?range=1d", &raw)
	if errors.Is(err, ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if raw.Current == nil || raw.Current.USD == nil || *raw.Current.USD <= 0 {
		return nil, nil
	}
	v := *raw.Current.USD
	return &v, nil
}
