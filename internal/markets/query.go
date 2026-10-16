package markets

import (
	"context"
	"slices"
	"strings"
	"time"

	"tokens/internal/config"
	"tokens/internal/sources"
)

// TopCoins answers the first limit coins of the last market list
func (s *Service) TopCoins(_ context.Context, limit int) (TopCoins, error) {
	rows, at, err, ok := s.coins.get()
	if !ok {
		return TopCoins{}, unavailable(sourceCoinGecko, err)
	}
	cov := []Note{}
	if s.stale(at, s.d.Config.TopCoinsRefresh) {
		cov = append(cov, CovTopCoinsStale)
	}
	plat, pat, _, pok := s.platforms.get()
	if !pok || s.stale(pat, s.d.Config.PlatformsRefresh) {
		cov = append(cov, CovTokenMapMissing)
	}
	out := TopCoins{Kind: KindTopCoins, Items: []TopCoin{}}
	var newest time.Time
	for i, r := range rows {
		if t, err := time.Parse(time.RFC3339, r.LastUpdated); err == nil && t.After(newest) {
			newest = t
		}
		if r.ID == "" || r.CurrentPrice == nil || r.MarketCap == nil || len(out.Items) >= limit {
			continue
		}
		rank := i + 1
		if r.Rank != nil {
			rank = *r.Rank
		}
		c := TopCoin{
			Rank: rank, ID: r.ID, Symbol: strings.ToUpper(sources.CleanText(r.Symbol)), Name: sources.CleanText(r.Name),
			PriceUSD: *r.CurrentPrice, MarketCapUSD: *r.MarketCap, Change24hPct: pct(r.Change24h), Tokens: coinTokens(plat[r.ID]),
		}
		if u := sources.HTTPSURL(r.Image); u != "" {
			c.LogoURL = &u
		}
		out.Items = append(out.Items, c)
	}
	if newest.IsZero() {
		newest = at
	}
	out.AsOf = stamp(newest)
	out.Coverage = codes(cov...)
	return out, nil
}

// ChainTVL answers the last days days of a chain's TVL, all of them when days is 0
func (s *Service) ChainTVL(_ context.Context, chain string, days int) (ChainTVL, error) {
	c, err := s.chain(chain)
	if err != nil {
		return ChainTVL{}, err
	}
	raw, at, lerr, ok := c.tvl.get()
	if !ok {
		return ChainTVL{}, unavailable(sourceDefiLlama, lerr)
	}
	pts := make([]sources.TVLPoint, 0, len(raw))
	for _, p := range raw {
		if p.TVL != nil {
			pts = append(pts, p)
		}
	}
	slices.SortFunc(pts, func(a, b sources.TVLPoint) int { return int(a.Date - b.Date) })
	if len(pts) == 0 {
		return ChainTVL{}, unavailable(sourceDefiLlama, nil)
	}
	last := pts[len(pts)-1]
	at1d, at7d := valueAt(pts, last.Date-86400), valueAt(pts, last.Date-7*86400)
	out := ChainTVL{Kind: KindChainTVL, Chain: chain, CurrentUSD: *last.TVL, Items: []Point{}}
	out.Change1dPct, out.Change7dPct = change(*last.TVL, at1d), change(*last.TVL, at7d)
	for _, p := range pts {
		if days == 0 || p.Date > last.Date-int64(days)*86400 {
			out.Items = append(out.Items, Point{Day: day(p.Date), TVL: *p.TVL})
		}
	}
	out.Coverage = codes(s.defiNotes(at, CovLastDayPartial)...)
	return out, nil
}

// valueAt is the TVL of the point at exactly this time, 0 when the history has none
func valueAt(pts []sources.TVLPoint, t int64) float64 {
	for _, p := range slices.Backward(pts) {
		if p.Date == t {
			return *p.TVL
		}
		if p.Date < t {
			break
		}
	}
	return 0
}

func (s *Service) defiNotes(at time.Time, always ...Note) []Note {
	var out []Note
	if s.stale(at, s.d.Config.DefiRefresh) {
		out = append(out, CovDefiStale)
	}
	return append(out, always...)
}

// ChainProtocols answers the largest limit protocols of a chain
func (s *Service) ChainProtocols(_ context.Context, chain string, limit int) (ChainProtocols, error) {
	c, err := s.chain(chain)
	if err != nil {
		return ChainProtocols{}, err
	}
	set, at, lerr, ok := c.protocols.get()
	if !ok {
		return ChainProtocols{}, unavailable(sourceDefiLlama, lerr)
	}
	return ChainProtocols{
		Kind: KindChainProtocols, Chain: chain, Items: slices.Clone(set.rows[:min(limit, len(set.rows))]), Categories: slices.Clone(set.categories),
		Coverage: codes(s.defiNotes(at, CovNotAdditive, CovChangeGlobal)...),
	}, nil
}

// ChainStablecoins answers a chain's stablecoin supply
func (s *Service) ChainStablecoins(_ context.Context, chain string) (ChainStablecoins, error) {
	c, err := s.chain(chain)
	if err != nil {
		return ChainStablecoins{}, err
	}
	set, at, lerr, ok := c.stable.get()
	if !ok {
		return ChainStablecoins{}, unavailable(sourceDefiLlama, lerr)
	}
	return ChainStablecoins{Kind: KindChainStable, Chain: chain, TotalUSD: set.total, Items: slices.Clone(set.items), History: slices.Clone(set.history), Coverage: codes(s.defiNotes(at, CovLastDayPartial)...)}, nil
}

// ChainDexVolume answers a chain's DEX volume over the last days days
func (s *Service) ChainDexVolume(_ context.Context, chain string, days int) (ChainDexVolume, error) {
	c, err := s.chain(chain)
	if err != nil {
		return ChainDexVolume{}, err
	}
	set, at, lerr, ok := c.dex.get()
	if !ok {
		return ChainDexVolume{}, unavailable(sourceDefiLlama, lerr)
	}
	items := set.chart[max(0, len(set.chart)-days):]
	return ChainDexVolume{
		Kind: KindChainDex, Chain: chain, Total24hUSD: set.total24h, Total7dUSD: set.total7d, Total30dUSD: set.total30d, Change1dPct: set.change1d,
		Items: slices.Clone(items), Protocols: slices.Clone(set.protocols), Coverage: codes(s.defiNotes(at, CovLastDayPartial)...),
	}, nil
}

// ChainActivity answers a chain's activity series over the last days days
func (s *Service) ChainActivity(_ context.Context, chain string, days int) (ChainActivity, error) {
	c, err := s.chain(chain)
	if err != nil {
		return ChainActivity{}, err
	}
	if !c.cfg.Activity {
		return ChainActivity{}, ErrNoActivity
	}
	all, at, lerr, ok := c.activity.get()
	if !ok {
		if c.cfg.ActivitySource == config.ActivityBlocks {
			return ChainActivity{}, unavailable(sourceBlockCount, lerr)
		}
		return ChainActivity{}, unavailable(sourceBlockscout, lerr)
	}
	cutoff := s.d.Now().UTC().AddDate(0, 0, -(days - 1)).Format(time.DateOnly)
	out := ChainActivity{Kind: KindChainActivity, Chain: chain, Series: make([]Series, 0, len(all))}
	for _, sr := range all {
		items := []ValuePoint{}
		for _, p := range sr.Items {
			if p.Day >= cutoff {
				items = append(items, p)
			}
		}
		out.Series = append(out.Series, Series{Metric: sr.Metric, Items: items})
	}
	notes := []Note{}
	if s.stale(at, s.d.Config.ActivityRefresh) {
		notes = append(notes, CovActivityStale)
	}
	if c.cfg.ActivitySource == config.ActivityBlocks {
		notes = append(notes, CovActivityFromBlocks)
	}
	out.Coverage = codes(append(notes, CovLastDayPartial)...)
	return out, nil
}
