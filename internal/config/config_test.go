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
	eth, ok := cfg.Chain("eip155:1")
	indexed := cfg.IndexChains()
	if !ok || eth.Holders != HoldersBlockscout || eth.GeckoNetwork != "eth" || eth.Native != nil || len(indexed) != 2 {
		t.Fatalf("chains %+v", cfg.Chains)
	}
	mon, arc := indexed[0], indexed[1]
	if mon.ID != "eip155:143" || mon.WrappedNative != evm.MustAddress("0x3bd359c1119da7da1d913d1c4d2b7c461115433a") || mon.Native != nil || len(mon.Prewarm) != 2 {
		t.Fatalf("monad %+v", mon)
	}
	if cfg.RPCUpstreamFilter != "!drpc-*" || cfg.HyperSyncPerMinute+cfg.HyperSyncInteractive > 15 || cfg.Index.MaxTokens != 50 || cfg.Index.Prewarm != nil {
		t.Errorf("config %+v", cfg)
	}
	// Monad keeps the index defaults, Arc reads USDC from the system emitter and starts its activity window two hours back
	if x := cfg.IndexOf(mon); x.ActivityWindowBlocks != 20000 || len(x.Prewarm) != 2 || x.Prewarm[0] != evm.MustAddress("0x754704bc059f8c67012fed69bc8a327a5aafb603") {
		t.Errorf("monad index %+v", x)
	}
	usdc := evm.MustAddress("0x3600000000000000000000000000000000000000")
	if arc.ID != "eip155:5042" || arc.Name != "arc" || arc.GeckoNetwork != "arc" || !arc.WrappedNative.IsZero() || arc.NativeToken(usdc) == nil ||
		arc.Native.Emitter != evm.MustAddress("0xfffffffffffffffffffffffffffffffffffffffe") || arc.Native.Decimals != 18 {
		t.Fatalf("arc %+v", arc)
	}
	if x := cfg.IndexOf(arc); x.ActivityWindowBlocks != 14400 || len(x.Prewarm) != 2 || x.Prewarm[0] != usdc || x.LagBlocks != cfg.Index.LagBlocks {
		t.Errorf("arc index %+v", x)
	}
	if cfg.HoldersCache != 10*time.Minute || cfg.GeckoCache != 15*time.Minute || cfg.TransfersCache != 15*time.Minute || cfg.TransferWindowBlocks != 7200 {
		t.Errorf("caches %+v", cfg)
	}
	if cfg.HyperSyncKey.Reveal() != "the-key" || fmt.Sprint(cfg.HyperSyncKey) != redacted || strings.Contains(fmt.Sprintf("%#v", cfg), "the-key") {
		t.Error("the key is not kept out of printed output")
	}
	// a chain that names no GeckoTerminal network takes geckoterminal_network
	cfg, err = Load(write(t, strings.Replace(repoConfig(t), "    gecko_network: arc\n", "", 1)))
	if a, _ := cfg.Chain("eip155:5042"); err != nil || a.GeckoNetwork != "monad" {
		t.Errorf("default network %q %v", a.GeckoNetwork, err)
	}
}

func TestRPCPacingDefaults(t *testing.T) {
	base := repoConfig(t)
	cfg, err := Load(write(t, base))
	if err != nil || cfg.MulticallMaxCalls != 100 || cfg.RPCBackgroundPerSecond != 2 {
		t.Fatalf("repo config: %+v %v", cfg, err)
	}
	stripped := strings.NewReplacer("multicall_max_calls: 100\n", "", "rpc_background_per_second: 2\n", "").Replace(base)
	cfg, err = Load(write(t, stripped))
	if err != nil || cfg.MulticallMaxCalls != 100 || cfg.RPCBackgroundPerSecond != 2 {
		t.Fatalf("defaults: %+v %v", cfg, err)
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
		"unknown key":           {"blockscout_per_second: 3", "blockscout_per_second: 3\nnope: 1"},
		"quota past the key":    {"hypersync_per_minute: 3", "hypersync_per_minute: 14"},
		"no filter in the url":  {"blockscout_url: https://eth.blockscout.com", "blockscout_url: https://user:pw@eth.blockscout.com"},
		"plain http hypersync":  {"hypersync_url: https://eth.hypersync.xyz/query", "hypersync_url: http://eth.hypersync.xyz/query"},
		"unknown holders":       {"holders: blockscout", "holders: explorer"},
		"native on blockscout":  {"    holders: blockscout\n", "    holders: blockscout\n    native: {token: \"0x3600000000000000000000000000000000000000\", emitter: \"0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE\", decimals: 18}\n"},
		"native emits itself":   {`emitter: "0xffffFFFfFFffffffffffffffFfFFFfffFFFfFFfE"`, `emitter: "0x3600000000000000000000000000000000000000"`},
		"native of 0 decimals":  {"      decimals: 18", "      decimals: 0"},
		"prewarm on blockscout": {"    holders: blockscout\n", "    holders: blockscout\n    prewarm: [\"0xa0b86991c6218b36c1d19d4a2e9eb0ce3606eb48\"]\n"},
		"bad gecko network":     {"gecko_network: arc", "gecko_network: Arc!"},
		"filter with a space":   {`rpc_upstream_filter: "!drpc-*"`, `rpc_upstream_filter: "!drpc *"`},
		"prewarm twice":         {`- "0x3bd359c1119da7da1d913d1c4d2b7c461115433a" # WMON`, `- "0x754704bc059f8c67012fed69bc8a327a5aafb603"`},
		"tail too fast":         {"tail_interval: 60s", "tail_interval: 1s"},
		"window of no days":     {"window_days: 7", "window_days: 0"},
		"bad wrapped native":    {`wrapped_native: "0xc02aaa39b223fe8d0a0e5c4f27ead9083c756cc2"`, `wrapped_native: "0x12"`},
		"min above the window":  {"transfer_min_blocks: 20", "transfer_min_blocks: 9000"},
		"batch of 300":          {"multicall_max_calls: 100", "multicall_max_calls: 300"},
		"negative batch":        {"multicall_max_calls: 100", "multicall_max_calls: -1"},
		"background too fast":   {"rpc_background_per_second: 2", "rpc_background_per_second: 5"},
		"background negative":   {"rpc_background_per_second: 2", "rpc_background_per_second: -1"},
		"gecko past its limit":  {"geckoterminal_per_minute: 20", "geckoterminal_per_minute: 40"},
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

func TestHolderMapConfig(t *testing.T) {
	cfg, err := Load(write(t, repoConfig(t)))
	if err != nil {
		t.Fatal(err)
	}
	h := cfg.HolderMap
	if h.WindowDays != 30 || h.PageBudget != 8 || h.FreshFor != 12*time.Hour || h.KeepFor != 7*24*time.Hour || h.QueueMax != 20 || h.RetryFailedAfter != 10*time.Minute {
		t.Fatalf("holder map %+v", h)
	}
	base := repoConfig(t)
	for _, c := range [][2]string{
		{"  window_days: 30\n  # at most", "  window_days: 0\n  # at most"},
		{"page_budget: 8", "page_budget: 21"},
		{"fresh_for: 12h", "fresh_for: 300h"},
		{"queue_max: 20", "queue_max: 0"},
		{"keep_for: 168h\n  queue_max: 20\n  retry_failed_after: 10m", "keep_for: 168h\n  queue_max: 20\n  retry_failed_after: 1s"},
		{"holder_map:\n", "holder_map:\n  colour: red\n"},
	} {
		if !strings.Contains(base, c[0]) {
			t.Fatalf("the repo config lacks %q", c[0])
		}
		if _, err := Load(write(t, strings.Replace(base, c[0], c[1], 1))); err == nil {
			t.Errorf("%q was accepted", c[1])
		}
	}
}
