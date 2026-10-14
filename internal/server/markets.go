package server

import (
	"context"
	"errors"
	"net/url"
	"strconv"

	"tokens/internal/markets"
	"tokens/internal/problem"
)

const (
	topCoinsPath = "/internal/v1/markets/top-coins"
	chainPrefix  = "/internal/v1/chains/{chain}"
	defaultCoins = 50
	defaultDays  = 90
)

// Markets is what the chain and market handlers read; a Service that also implements it gets these routes
type Markets interface {
	TopCoins(ctx context.Context, limit int) (markets.TopCoins, error)
	ChainTVL(ctx context.Context, chain string, days int) (markets.ChainTVL, error)
	ChainProtocols(ctx context.Context, chain string, limit int) (markets.ChainProtocols, error)
	ChainStablecoins(ctx context.Context, chain string) (markets.ChainStablecoins, error)
	ChainDexVolume(ctx context.Context, chain string, days int) (markets.ChainDexVolume, error)
	ChainActivity(ctx context.Context, chain string, days int) (markets.ChainActivity, error)
}

var marketRoutes = []struct {
	pattern string
	handler func(*app, Markets) endpoint
}{
	{"GET " + topCoinsPath, (*app).topCoins},
	{"GET " + chainPrefix + "/defi/tvl", (*app).chainTVL},
	{"GET " + chainPrefix + "/defi/protocols", (*app).chainProtocols},
	{"GET " + chainPrefix + "/defi/stablecoins", (*app).chainStablecoins},
	{"GET " + chainPrefix + "/defi/dexs", (*app).chainDexs},
	{"GET " + chainPrefix + "/activity", (*app).chainActivity},
}

// marketChain reads and checks the chain of the path
func marketChain(c *call) (string, error) {
	chain := c.req.PathValue("chain")
	if !caip2.MatchString(chain) {
		return "", invalidID("The chain is not a CAIP-2 identifier.")
	}
	return chain, nil
}

// daysParam reads days from the allowed list, def when absent, and 0 for "all" when all is allowed
func daysParam(q url.Values, def int, all bool, allowed ...int) (int, error) {
	v := q.Get("days")
	if v == "" {
		return def, nil
	}
	if all && v == "all" {
		return 0, nil
	}
	for _, d := range allowed {
		if v == strconv.Itoa(d) {
			return d, nil
		}
	}
	detail := "days is 30, 90 or 365."
	if all {
		detail = "days is 30, 90, 365 or all."
	}
	return 0, invalid(detail)
}

// marketErr turns the markets errors into problems
func marketErr(err error) error {
	switch {
	case errors.Is(err, markets.ErrUnsupportedChain):
		return &httpError{problem.CodeUnsupportedChain, "Only eip155:1, eip155:143 and eip155:5042 have market data."}
	case errors.Is(err, markets.ErrNoActivity):
		return &httpError{problem.CodeUnsupportedChain, "Chain activity comes from Blockscout's stats service, and no keyless source is wired for this chain. Only eip155:1 and eip155:5042 have it."}
	}
	return err
}

func (a *app) topCoins(m Markets) endpoint {
	return func(c *call) (any, error) {
		q, err := parseQuery(c.req, "limit")
		if err != nil {
			return nil, err
		}
		limit, err := intParam(q, "limit", defaultCoins, 1, maxLimit)
		if err != nil {
			return nil, err
		}
		return m.TopCoins(c.ctx, limit)
	}
}

func (a *app) chainTVL(m Markets) endpoint {
	return func(c *call) (any, error) {
		chain, err := marketChain(c)
		if err != nil {
			return nil, err
		}
		q, err := parseQuery(c.req, "days")
		if err != nil {
			return nil, err
		}
		days, err := daysParam(q, defaultDays, true, 30, 90, 365)
		if err != nil {
			return nil, err
		}
		out, err := m.ChainTVL(c.ctx, chain, days)
		return out, marketErr(err)
	}
}

func (a *app) chainProtocols(m Markets) endpoint {
	return func(c *call) (any, error) {
		chain, err := marketChain(c)
		if err != nil {
			return nil, err
		}
		q, err := parseQuery(c.req, "limit")
		if err != nil {
			return nil, err
		}
		limit, err := intParam(q, "limit", 25, 1, maxLimit)
		if err != nil {
			return nil, err
		}
		out, err := m.ChainProtocols(c.ctx, chain, limit)
		return out, marketErr(err)
	}
}

func (a *app) chainStablecoins(m Markets) endpoint {
	return func(c *call) (any, error) {
		chain, err := marketChain(c)
		if err != nil {
			return nil, err
		}
		if _, err := parseQuery(c.req); err != nil {
			return nil, err
		}
		out, err := m.ChainStablecoins(c.ctx, chain)
		return out, marketErr(err)
	}
}

func (a *app) chainDexs(m Markets) endpoint {
	return func(c *call) (any, error) {
		chain, err := marketChain(c)
		if err != nil {
			return nil, err
		}
		q, err := parseQuery(c.req, "days")
		if err != nil {
			return nil, err
		}
		days, err := daysParam(q, defaultDays, false, 30, 90, 365)
		if err != nil {
			return nil, err
		}
		out, err := m.ChainDexVolume(c.ctx, chain, days)
		return out, marketErr(err)
	}
}

func (a *app) chainActivity(m Markets) endpoint {
	return func(c *call) (any, error) {
		chain, err := marketChain(c)
		if err != nil {
			return nil, err
		}
		q, err := parseQuery(c.req, "days")
		if err != nil {
			return nil, err
		}
		days, err := daysParam(q, defaultDays, false, 30, 90, 365)
		if err != nil {
			return nil, err
		}
		out, err := m.ChainActivity(c.ctx, chain, days)
		return out, marketErr(err)
	}
}
