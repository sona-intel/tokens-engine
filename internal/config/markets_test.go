package config

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

func TestMarketsConfig(t *testing.T) {
	t.Setenv("COINGECKO_API_KEY", " cg-demo ")
	cfg, err := Load(write(t, repoConfig(t)))
	if err != nil {
		t.Fatal(err)
	}
	m := cfg.Markets
	if m.CoinGeckoGap != 20*time.Second || m.TopCoinsRefresh != 5*time.Minute || m.DefiRefresh != time.Hour || len(m.Chains) != 3 {
		t.Fatalf("markets %+v", m)
	}
	if c, ok := m.Chain("eip155:143"); !ok || c.LlamaName != "Monad" || !c.Activity || c.ActivitySource != ActivityBlocks ||
		len(c.SystemSenders) != 1 || c.SystemSenders[0].String() != "0x6f49a8f621353f12378d0046e7d7e4b9b249dc9e" {
		t.Errorf("monad %+v", c)
	}
	if b := m.BlockStats; b.Poll != 10*time.Minute || b.BackfillDays != 0 || b.LagBlocks != 10 || !m.CountsBlocks() {
		t.Errorf("block stats %+v", b)
	}
	if c, _ := m.Chain("eip155:5042"); c.ActivitySource != "" || len(c.SystemSenders) != 0 {
		t.Errorf("arc counts its blocks %+v", c)
	}
	if c, ok := m.Chain("eip155:1"); !ok || c.LlamaName != "Ethereum" || !c.Activity {
		t.Errorf("ethereum %+v", c)
	}
	if c, ok := m.Chain("eip155:5042"); !ok || c.LlamaName != "Arc" || !c.Activity || c.StatsURL != "https://explorer.arc.io" || c.FeesMetric != "fees_usdc" {
		t.Errorf("arc %+v", c)
	}
	if m.CoinGeckoKey.Reveal() != "cg-demo" || fmt.Sprint(m.CoinGeckoKey) != redacted || strings.Contains(fmt.Sprintf("%#v", cfg), "cg-demo") {
		t.Error("the CoinGecko key is not kept out of printed output")
	}
	t.Setenv("COINGECKO_API_KEY", "")
	cfg, err = Load(write(t, repoConfig(t)))
	if err != nil || cfg.Markets.CoinGeckoKey.Set() {
		t.Errorf("no key: %v %v", err, cfg.Markets.CoinGeckoKey.Set())
	}
}

func TestMarketsConfigRefusals(t *testing.T) {
	base := repoConfig(t)
	for _, c := range [][2]string{
		{"coingecko_min_gap: 20s", "coingecko_min_gap: 5s"},
		{"coingecko_url: https://api.coingecko.com/api/v3", "coingecko_url: http://api.coingecko.com/api/v3"},
		{"defi_refresh: 1h", "defi_refresh: 1m"},
		{"llama_per_minute: 30", "llama_per_minute: 0"},
		{"    - id: \"eip155:1\"\n      llama_name: Ethereum", "    - id: \"eip155:9\"\n      llama_name: Ethereum"},
		{"llama_name: Monad", "llama_name: Ethereum"},
		{"activity_source: blocks", "activity_source: rpc"},
		{"      llama_name: Monad\n      activity: true", "      llama_name: Monad\n      activity: false"},
		{"system_senders: [\"0x6f49a8f621353f12378d0046e7d7e4b9b249dc9e\"]", "system_senders: [\"0x6f49\"]"},
		{"      activity_source: blocks\n      # the sender of the one system transaction every Monad block opens with\n      system_senders: [\"0x6f49a8f621353f12378d0046e7d7e4b9b249dc9e\"]\n", ""},
		{"poll: 10m", "poll: 30s"},
		{"backfill_days: 0", "backfill_days: 366"},
		{"backfill_days: 0", "backfill_days: -1"},
		{"lag_blocks: 10\n  chains", "lag_blocks: 0\n  chains"},
		{"      activity_source: blocks\n", "      stats_url: https://explorer.arc.io\n      activity_source: blocks\n"},
		{"    - id: \"eip155:5042\"\n      llama_name: Arc\n", "    - id: \"eip155:5042\"\n      llama_name: Arc\n      system_senders: [\"0x6f49a8f621353f12378d0046e7d7e4b9b249dc9e\"]\n"},
	} {
		if !strings.Contains(base, c[0]) {
			t.Fatalf("the repo config lacks %q", c[0])
		}
		if _, err := Load(write(t, strings.Replace(base, c[0], c[1], 1))); err == nil {
			t.Errorf("%q was accepted", c[1])
		}
	}
}
