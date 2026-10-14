package markets

import (
	"cmp"
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"tokens/internal/sources"
)

// excludedCategories are the categories DefiLlama itself leaves out of a chain's TVL: custody on exchanges and the chain's own token
var excludedCategories = map[string]bool{"CEX": true, "Chain": true}

// activityLines are the Blockscout stats lines of the activity record, each with the unit conversion that makes its metric explicit
var activityLines = []struct {
	id, metric string
	convert    func(float64) float64
}{
	{"newTxns", "txs", identity},
	{"activeAccounts", "active_accounts", identity},
	{"newAccounts", "new_accounts", identity},
	{"averageGasPrice", "gas_price_gwei", identity},
	{"txnsFee", "fees_eth", identity},
	// the stats service sends the mean gas used over the gas limit as a fraction, 0.5 for half full
	{"networkUtilization", "utilization_pct", func(v float64) float64 { return math.Round(v*1e6) / 1e4 }},
}

func identity(v float64) float64 { return v }

// RefreshTopCoins reads CoinGecko's market list
func (s *Service) RefreshTopCoins(ctx context.Context) error {
	rows, err := s.d.CoinGecko.Markets(ctx)
	if err != nil {
		s.coins.fail(err)
		return err
	}
	if len(rows) == 0 {
		err = &sources.UnavailableError{Source: sourceCoinGecko, Err: errors.New("the market list is empty")}
		s.coins.fail(err)
		return err
	}
	s.coins.set(rows, s.d.Now())
	return nil
}

// RefreshPlatforms reads CoinGecko's contract addresses of the served chains
func (s *Service) RefreshPlatforms(ctx context.Context) error {
	keys := make([]string, 0, len(platformChains))
	for _, p := range platformChains {
		keys = append(keys, p.key)
	}
	m, err := s.d.CoinGecko.Platforms(ctx, keys...)
	if err != nil {
		s.platforms.fail(err)
		return err
	}
	s.platforms.set(m, s.d.Now())
	return nil
}

// each runs fn for every chain and joins the failures, a chain that fails keeps its copy
func (s *Service) each(fn func(*chainSlots) error) error {
	var errs []error
	for _, c := range s.d.Config.Chains {
		if err := fn(s.chains[c.ID]); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}

// RefreshTVL reads each chain's TVL history
func (s *Service) RefreshTVL(ctx context.Context) error {
	return s.each(func(c *chainSlots) error {
		pts, err := s.d.Llama.ChainTVL(ctx, c.cfg.LlamaName)
		if err == nil && len(pts) == 0 {
			err = &sources.UnavailableError{Source: sourceDefiLlama, Err: errors.New("the TVL history is empty")}
		}
		if err != nil {
			c.tvl.fail(err)
			return err
		}
		c.tvl.set(pts, s.d.Now())
		return nil
	})
}

// RefreshProtocols reads DefiLlama's protocol list once for every chain
func (s *Service) RefreshProtocols(ctx context.Context) error {
	names := make([]string, 0, len(s.chains))
	for _, c := range s.d.Config.Chains {
		names = append(names, c.LlamaName)
	}
	rows, err := s.d.Llama.Protocols(ctx, names...)
	if err != nil {
		for _, c := range s.chains {
			c.protocols.fail(err)
		}
		return err
	}
	now := s.d.Now()
	for _, c := range s.chains {
		c.protocols.set(buildProtocols(rows[c.cfg.LlamaName]), now)
	}
	return nil
}

func buildProtocols(rows []sources.ProtocolRow) protocolSet {
	var items []Protocol
	cats := map[string]*Category{}
	for _, r := range rows {
		if excludedCategories[r.Category] || r.TVL <= 0 || math.IsNaN(r.TVL) || math.IsInf(r.TVL, 0) {
			continue
		}
		p := Protocol{Name: sources.CleanText(r.Name), Slug: r.Slug, Category: sources.CleanText(r.Category), TVLUSD: cents(r.TVL), Change1dPct: pct(r.Change1D), Change7dPct: pct(r.Change7D)}
		if u := sources.HTTPSURL(r.Logo); u != "" {
			p.LogoURL = &u
		}
		items = append(items, p)
		c := cats[p.Category]
		if c == nil {
			c = &Category{Category: p.Category}
			cats[p.Category] = c
		}
		c.TVLUSD = cents(c.TVLUSD + p.TVLUSD)
		c.Protocols++
	}
	slices.SortFunc(items, func(a, b Protocol) int {
		return cmp.Or(cmp.Compare(b.TVLUSD, a.TVLUSD), cmp.Compare(a.Slug, b.Slug))
	})
	out := protocolSet{rows: items[:min(len(items), keepProtocols)], categories: make([]Category, 0, len(cats))}
	for _, c := range cats {
		out.categories = append(out.categories, *c)
	}
	slices.SortFunc(out.categories, func(a, b Category) int {
		return cmp.Or(cmp.Compare(b.TVLUSD, a.TVLUSD), cmp.Compare(a.Category, b.Category))
	})
	return out
}

// RefreshStablecoins reads the stablecoin list once and each chain's supply history
func (s *Service) RefreshStablecoins(ctx context.Context) error {
	names := make([]string, 0, len(s.chains))
	for _, c := range s.d.Config.Chains {
		names = append(names, c.LlamaName)
	}
	list, err := s.d.Llama.Stablecoins(ctx, names...)
	if err != nil {
		for _, c := range s.chains {
			c.stable.fail(err)
		}
		return err
	}
	return s.each(func(c *chainSlots) error {
		chart, err := s.d.Llama.StablecoinChart(ctx, c.cfg.LlamaName)
		if err == nil && len(chart) == 0 {
			err = &sources.UnavailableError{Source: sourceDefiLlama, Err: errors.New("the stablecoin history is empty")}
		}
		if err != nil {
			c.stable.fail(err)
			return err
		}
		c.stable.set(buildStable(list[c.cfg.LlamaName], chart), s.d.Now())
		return nil
	})
}

func buildStable(list []sources.Stablecoin, chart []sources.StablePoint) stableSet {
	items := make([]Stablecoin, 0, len(list))
	for _, c := range list {
		items = append(items, Stablecoin{Symbol: sources.CleanText(c.Symbol), Name: sources.CleanText(c.Name), CirculatingUSD: cents(c.Circulating)})
	}
	slices.SortFunc(items, func(a, b Stablecoin) int {
		return cmp.Or(cmp.Compare(b.CirculatingUSD, a.CirculatingUSD), cmp.Compare(a.Symbol, b.Symbol))
	})
	chart = slices.Clone(chart)
	slices.SortFunc(chart, func(a, b sources.StablePoint) int { return cmp.Compare(a.Date, b.Date) })
	chart = chart[max(0, len(chart)-historyDays):]
	out := stableSet{items: items[:min(len(items), topLists)], history: make([]StablePoint, 0, len(chart))}
	for _, p := range chart {
		out.history = append(out.history, StablePoint{Day: day(p.Date), TotalUSD: p.Total})
	}
	return out
}

// RefreshDex reads each chain's DEX volume
func (s *Service) RefreshDex(ctx context.Context) error {
	return s.each(func(c *chainSlots) error {
		o, err := s.d.Llama.DexVolume(ctx, c.cfg.LlamaName)
		if err == nil && len(o.Chart) == 0 {
			err = &sources.UnavailableError{Source: sourceDefiLlama, Err: errors.New("the DEX volume chart is empty")}
		}
		if err != nil {
			c.dex.fail(err)
			return err
		}
		c.dex.set(buildDex(o), s.d.Now())
		return nil
	})
}

func buildDex(o sources.DexOverview) dexSet {
	chart := slices.Clone(o.Chart)
	slices.SortFunc(chart, func(a, b sources.DexPoint) int { return cmp.Compare(a.Date, b.Date) })
	chart = chart[max(0, len(chart)-historyDays):]
	out := dexSet{total24h: o.Total24h, total7d: o.Total7d, total30d: o.Total30d, change1d: pct(o.Change1d), chart: make([]VolumePoint, 0, len(chart))}
	for _, p := range chart {
		out.chart = append(out.chart, VolumePoint{Day: day(p.Date), VolumeUSD: cents(p.Volume)})
	}
	for _, p := range o.Protocols {
		if p.V24h == nil || *p.V24h <= 0 || len(out.protocols) == topLists {
			continue
		}
		d := DexProtocol{Name: sources.CleanText(p.Name), Volume24hUSD: *p.V24h, Volume7dUSD: p.V7d}
		if u := sources.HTTPSURL(p.Logo); u != "" {
			d.LogoURL = &u
		}
		out.protocols = append(out.protocols, d)
	}
	return out
}

// RefreshActivity reads the stats lines of each chain that has them, a year of daily points ending today
func (s *Service) RefreshActivity(ctx context.Context) error {
	if s.d.Blockscout == nil {
		return nil
	}
	return s.each(func(c *chainSlots) error {
		if !c.cfg.Activity {
			return nil
		}
		today := s.d.Now().UTC()
		from, to := today.AddDate(0, 0, -(historyDays-1)).Format(time.DateOnly), today.Format(time.DateOnly)
		series := make([]Series, 0, len(activityLines))
		for _, l := range activityLines {
			pts, err := s.d.Blockscout.StatsLine(ctx, l.id, from, to)
			if err != nil {
				c.activity.fail(err)
				return err
			}
			items := make([]ValuePoint, 0, len(pts))
			for _, p := range pts {
				items = append(items, ValuePoint{Day: p.Day, Value: l.convert(p.Value)})
			}
			series = append(series, Series{Metric: l.metric, Items: items})
		}
		c.activity.set(series, s.d.Now())
		return nil
	})
}

// platformChains are the CoinGecko platform keys of the served chains, in the order tokens are listed
var platformChains = []struct{ key, chain string }{
	{"ethereum", "eip155:1"},
	{"monad", "eip155:143"},
	{"arc", "eip155:5042"},
}

func coinTokens(platforms map[string]string) []CoinToken {
	out := []CoinToken{}
	for _, p := range platformChains {
		a, ok := cleanAddress(platforms[p.key])
		if ok {
			out = append(out, CoinToken{Chain: p.chain, Address: a})
		}
	}
	return out
}

// cleanAddress lowercases a 0x address of 40 hex digits, and refuses anything else
func cleanAddress(s string) (string, bool) {
	s = strings.ToLower(strings.TrimSpace(s))
	if len(s) != 42 || s[:2] != "0x" {
		return "", false
	}
	for _, r := range s[2:] {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return "", false
		}
	}
	return s, true
}
