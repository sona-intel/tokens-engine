package config

import (
	"errors"
	"fmt"
	"time"
)

// MarketChain is a served chain that has market data: its DefiLlama name and whether Blockscout's stats service covers it
type MarketChain struct {
	ID        string
	LlamaName string
	Activity  bool
	// StatsURL is the Blockscout instance whose stats service holds the chain's activity lines, empty for blockscout_url
	StatsURL string
	// FeesMetric names the fee series: fees_eth, or fees_usdc where USDC is the gas coin
	FeesMetric string
}

// Markets is the config of the chain and market routes: top coins, DeFi figures and chain activity
type Markets struct {
	CoinGeckoURL string
	// CoinGeckoKey is the optional demo key from COINGECKO_API_KEY
	CoinGeckoKey     Secret
	CoinGeckoGap     time.Duration
	TopCoinsRefresh  time.Duration
	PlatformsRefresh time.Duration
	LlamaURL         string
	StablecoinsURL   string
	LlamaPerMinute   float64
	DefiRefresh      time.Duration
	ActivityRefresh  time.Duration
	Chains           []MarketChain
}

// Chain returns the market chain with this CAIP-2 id
func (m Markets) Chain(id string) (MarketChain, bool) {
	for _, c := range m.Chains {
		if c.ID == id {
			return c, true
		}
	}
	return MarketChain{}, false
}

type marketChainFile struct {
	ID         string `yaml:"id"`
	LlamaName  string `yaml:"llama_name"`
	Activity   bool   `yaml:"activity"`
	StatsURL   string `yaml:"stats_url"`
	FeesMetric string `yaml:"fees_metric"`
}

type marketsFile struct {
	CoinGeckoURL     string            `yaml:"coingecko_url"`
	CoinGeckoGap     time.Duration     `yaml:"coingecko_min_gap"`
	TopCoinsRefresh  time.Duration     `yaml:"top_coins_refresh"`
	PlatformsRefresh time.Duration     `yaml:"platforms_refresh"`
	LlamaURL         string            `yaml:"llama_url"`
	StablecoinsURL   string            `yaml:"stablecoins_url"`
	LlamaPerMinute   float64           `yaml:"llama_per_minute"`
	DefiRefresh      time.Duration     `yaml:"defi_refresh"`
	ActivityRefresh  time.Duration     `yaml:"activity_refresh"`
	Chains           []marketChainFile `yaml:"chains"`
}

func (m marketsFile) build() Markets {
	out := Markets{
		CoinGeckoURL: trimSlash(m.CoinGeckoURL), CoinGeckoGap: m.CoinGeckoGap, TopCoinsRefresh: m.TopCoinsRefresh,
		PlatformsRefresh: m.PlatformsRefresh, LlamaURL: trimSlash(m.LlamaURL), StablecoinsURL: trimSlash(m.StablecoinsURL),
		LlamaPerMinute: m.LlamaPerMinute, DefiRefresh: m.DefiRefresh, ActivityRefresh: m.ActivityRefresh,
	}
	for _, c := range m.Chains {
		mc := MarketChain{ID: c.ID, LlamaName: c.LlamaName, Activity: c.Activity, StatsURL: trimSlash(c.StatsURL), FeesMetric: c.FeesMetric}
		if mc.FeesMetric == "" {
			mc.FeesMetric = "fees_eth"
		}
		out.Chains = append(out.Chains, mc)
	}
	return out
}

func trimSlash(s string) string {
	for s != "" && s[len(s)-1] == '/' {
		s = s[:len(s)-1]
	}
	return s
}

func (m marketsFile) validate(served []chainFile) error {
	switch {
	case !validURL(m.CoinGeckoURL, "https"):
		return errors.New("markets.coingecko_url must be an https URL without credentials")
	case m.CoinGeckoGap < 20*time.Second:
		return fmt.Errorf("markets.coingecko_min_gap must be at least 20s, the pace of the keyless tier, got %s", m.CoinGeckoGap)
	case m.TopCoinsRefresh < time.Minute || m.PlatformsRefresh < time.Hour:
		return errors.New("markets.top_coins_refresh must be at least 1m and platforms_refresh at least 1h")
	case !validURL(m.LlamaURL, "https") || !validURL(m.StablecoinsURL, "https"):
		return errors.New("markets.llama_url and stablecoins_url must be https URLs without credentials")
	case m.LlamaPerMinute <= 0 || m.LlamaPerMinute > 60:
		return fmt.Errorf("markets.llama_per_minute must be above 0 and at most 60, got %v", m.LlamaPerMinute)
	case m.DefiRefresh < 10*time.Minute || m.ActivityRefresh < 10*time.Minute:
		return errors.New("markets.defi_refresh and activity_refresh must be at least 10m")
	case len(m.Chains) == 0:
		return errors.New("markets.chains must list at least one chain")
	}
	seen := map[string]bool{}
	for _, c := range m.Chains {
		known := false
		for _, s := range served {
			known = known || s.ID == c.ID
		}
		switch {
		case !known:
			return fmt.Errorf("markets.chains lists %q, which is not a served chain", c.ID)
		case seen[c.ID] || seen[c.LlamaName]:
			return fmt.Errorf("markets.chains lists %q twice", c.ID)
		case c.LlamaName == "" || len(c.LlamaName) > 64:
			return fmt.Errorf("markets.chains llama_name of %s must be a DefiLlama chain name", c.ID)
		case c.StatsURL != "" && (!c.Activity || !validURL(c.StatsURL, "https")):
			return fmt.Errorf("markets.chains stats_url of %s must be an https URL without credentials and needs activity: true", c.ID)
		case c.FeesMetric != "" && c.FeesMetric != "fees_eth" && c.FeesMetric != "fees_usdc":
			return fmt.Errorf("markets.chains fees_metric of %s must be fees_eth or fees_usdc", c.ID)
		}
		seen[c.ID], seen[c.LlamaName] = true, true
	}
	return nil
}
