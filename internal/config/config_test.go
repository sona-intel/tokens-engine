package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"tokens/internal/evm"
)

func repoConfig(t *testing.T) string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("..", "..", "config", "engine.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func write(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "engine.yaml"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestRepoConfigLoads(t *testing.T) {
	t.Setenv("ENVIO_HYPERSYNC_KEY", " the-key ")
	cfg, err := Load(write(t, repoConfig(t)))
	if err != nil {
		t.Fatal(err)
	}
	eth, ok1 := cfg.Chain("eip155:1")
	mon, ok2 := cfg.IndexChain()
	if !ok1 || !ok2 || eth.Holders != HoldersBlockscout || mon.ID != "eip155:143" || mon.WrappedNative != evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a") {
		t.Fatalf("chains %+v", cfg.Chains)
	}
	if cfg.RPCUpstreamFilter != "!drpc-*" || cfg.HyperSyncPerMinute+cfg.HyperSyncInteractive > 15 || cfg.Index.MaxTokens != 50 || len(cfg.Index.Prewarm) != 2 {
		t.Errorf("config %+v", cfg)
	}
	if cfg.HoldersCache != 10*time.Minute || cfg.GeckoCache != 15*time.Minute || cfg.TransfersCache != 15*time.Minute || cfg.TransferWindowBlocks != 7200 {
		t.Errorf("caches %+v", cfg)
	}
	if cfg.HyperSyncKey.Reveal() != "the-key" || fmt.Sprint(cfg.HyperSyncKey) != redacted || strings.Contains(fmt.Sprintf("%#v", cfg), "the-key") {
		t.Error("the key is not kept out of printed output")
	}
}

func TestEnvironmentWins(t *testing.T) {
	t.Setenv("TOKENS_RPC_MONAD", "http://127.0.0.1:4000/sona/monad")
	t.Setenv("TOKENS_PRICES_URL", "http://127.0.0.1:8210")
	cfg, err := Load(write(t, repoConfig(t)))
	if err != nil {
		t.Fatal(err)
	}
	mon, _ := cfg.Chain("eip155:143")
	if mon.RPCURL != "http://127.0.0.1:4000/sona/monad" || cfg.PricesURL != "http://127.0.0.1:8210" {
		t.Errorf("rpc %s prices %s", mon.RPCURL, cfg.PricesURL)
	}
}

func TestInvalidConfigs(t *testing.T) {
	base := repoConfig(t)
	cases := map[string][2]string{
		"unknown key":          {"blockscout_per_second: 3", "blockscout_per_second: 3\nnope: 1"},
		"quota past the key":   {"hypersync_per_minute: 3", "hypersync_per_minute: 14"},
		"no filter in the url": {"blockscout_url: https://eth.blockscout.com", "blockscout_url: https://user:pw@eth.blockscout.com"},
		"plain http hypersync": {"hypersync_url: https://eth.hypersync.xyz/query", "hypersync_url: http://eth.hypersync.xyz/query"},
		"two index chains":     {"holders: blockscout", "holders: index"},
		"filter with a space":  {`rpc_upstream_filter: "!drpc-*"`, `rpc_upstream_filter: "!drpc *"`},
		"prewarm twice":        {`- "0x3bd359c1119da7da1d913d1c4d2b7c461115433a" # WMON`, `- "0x754704bc059f8c67012fed69bc8a327a5aafb603"`},
		"tail too fast":        {"tail_interval: 60s", "tail_interval: 1s"},
		"window of no days":    {"window_days: 7", "window_days: 0"},
		"bad wrapped native":   {`wrapped_native: "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"`, `wrapped_native: "0x12"`},
		"min above the window": {"transfer_min_blocks: 20", "transfer_min_blocks: 9000"},
		"gecko past its limit": {"geckoterminal_per_minute: 20", "geckoterminal_per_minute: 40"},
	}
	for name, c := range cases {
		if !strings.Contains(base, c[0]) {
			t.Fatalf("%s: the config has no %q", name, c[0])
		}
		if _, err := Load(write(t, strings.Replace(base, c[0], c[1], 1))); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	if _, err := Load(t.TempDir()); err == nil {
		t.Error("a missing file loaded")
	}
}
