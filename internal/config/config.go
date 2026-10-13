package config

import (
	"errors"
	"fmt"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"go.yaml.in/yaml/v3"

	"tokens/internal/evm"
	"tokens/internal/rpc"
)

const (
	DefaultDir = "/etc/tokens"

	redacted = "[redacted]"

	defaultUpstreamFilter = "!drpc-*"

	HoldersBlockscout = "blockscout"
	HoldersIndex      = "index"
)

type Chain struct {
	ID            string
	Name          string
	RPCURL        string
	HyperSyncURL  string
	Holders       string
	WrappedNative evm.Address
	// GeckoNetwork is the GeckoTerminal network id of the chain, used by the token search
	GeckoNetwork string
}

// TokenList is a curated token list in the Uniswap token list format, the earlier a list the higher its rank
type TokenList struct {
	Name string
	URL  string
}

type Index struct {
	MaxTokens            int
	BackfillBudget       int
	ActivityWindowBlocks uint64
	TailInterval         time.Duration
	LagBlocks            uint64
	WindowDays           int
	TopPerDay            int
	Prewarm              []evm.Address
	RetryFailedAfter     time.Duration
}

type Config struct {
	Chains               []Chain
	RPCUpstreamFilter    string
	BlockscoutURL        string
	BlockscoutPerSecond  float64
	GeckoURL             string
	GeckoNetwork         string
	GeckoPerMinute       float64
	PricesURL            string
	HyperSyncPerMinute   float64
	HyperSyncInteractive float64
	HyperSyncKey         Secret
	MetaCache            time.Duration
	HoldersCache         time.Duration
	GeckoCache           time.Duration
	TransfersCache       time.Duration
	PriceCache           time.Duration
	SearchCache          time.Duration
	TokenLists           []TokenList
	TokenListRefresh     time.Duration
	TokenListTimeout     time.Duration
	TransferWindowBlocks uint64
	TransferMinBlocks    uint64
	TransferPageBudget   int
	Index                Index
}

// Chain returns the chain with this CAIP-2 id
func (c Config) Chain(id string) (Chain, bool) {
	for _, ch := range c.Chains {
		if ch.ID == id {
			return ch, true
		}
	}
	return Chain{}, false
}

// IndexChain returns the one chain whose holders come from the engine's own index
func (c Config) IndexChain() (Chain, bool) {
	for _, ch := range c.Chains {
		if ch.Holders == HoldersIndex {
			return ch, true
		}
	}
	return Chain{}, false
}

// Secret keeps a credential out of logs, formatted output and JSON
type Secret string

func (s Secret) Reveal() string { return string(s) }

func (s Secret) Set() bool { return s != "" }

func (s Secret) String() string {
	if s == "" {
		return ""
	}
	return redacted
}

func (s Secret) GoString() string { return fmt.Sprintf("%q", s.String()) }

func (s Secret) MarshalText() ([]byte, error) { return []byte(s.String()), nil }

func (s Secret) LogValue() slog.Value { return slog.StringValue(s.String()) }

type chainFile struct {
	ID            string `yaml:"id"`
	Name          string `yaml:"name"`
	RPCURL        string `yaml:"rpc_url"`
	HyperSyncURL  string `yaml:"hypersync_url"`
	Holders       string `yaml:"holders"`
	WrappedNative string `yaml:"wrapped_native"`
	GeckoNetwork  string `yaml:"gecko_network"`
}

type tokenListFile struct {
	Name string `yaml:"name"`
	URL  string `yaml:"url"`
}

type indexFile struct {
	MaxTokens            int           `yaml:"max_tokens"`
	BackfillBudget       int           `yaml:"backfill_budget"`
	ActivityWindowBlocks uint64        `yaml:"activity_window_blocks"`
	TailInterval         time.Duration `yaml:"tail_interval"`
	LagBlocks            uint64        `yaml:"lag_blocks"`
	WindowDays           int           `yaml:"window_days"`
	TopPerDay            int           `yaml:"top_per_day"`
	Prewarm              []string      `yaml:"prewarm"`
	RetryFailedAfter     time.Duration `yaml:"retry_failed_after"`
}

type file struct {
	Chains               []chainFile     `yaml:"chains"`
	RPCUpstreamFilter    *string         `yaml:"rpc_upstream_filter"`
	BlockscoutURL        string          `yaml:"blockscout_url"`
	BlockscoutPerSecond  float64         `yaml:"blockscout_per_second"`
	GeckoURL             string          `yaml:"geckoterminal_url"`
	GeckoNetwork         string          `yaml:"geckoterminal_network"`
	GeckoPerMinute       float64         `yaml:"geckoterminal_per_minute"`
	PricesURL            string          `yaml:"prices_url"`
	HyperSyncPerMinute   float64         `yaml:"hypersync_per_minute"`
	HyperSyncInteractive float64         `yaml:"hypersync_interactive_per_minute"`
	MetaCache            time.Duration   `yaml:"meta_cache"`
	HoldersCache         time.Duration   `yaml:"holders_cache"`
	GeckoCache           time.Duration   `yaml:"geckoterminal_cache"`
	TransfersCache       time.Duration   `yaml:"transfers_cache"`
	PriceCache           time.Duration   `yaml:"price_cache"`
	SearchCache          time.Duration   `yaml:"search_cache"`
	TokenLists           []tokenListFile `yaml:"token_lists"`
	TokenListRefresh     time.Duration   `yaml:"token_list_refresh"`
	TokenListTimeout     time.Duration   `yaml:"token_list_timeout"`
	TransferWindowBlocks uint64          `yaml:"transfer_window_blocks"`
	TransferMinBlocks    uint64          `yaml:"transfer_min_blocks"`
	TransferPageBudget   int             `yaml:"transfer_page_budget"`
	Index                indexFile       `yaml:"index"`
}

var (
	caip2    = regexp.MustCompile(`^eip155:[0-9]{1,10}$`)
	namePat  = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	networks = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)
)

func Dir() string {
	if dir := os.Getenv("TOKENS_CONFIG_DIR"); dir != "" {
		return dir
	}
	return DefaultDir
}

// Load reads engine.yaml, where TOKENS_RPC_<NAME> and TOKENS_PRICES_URL from the environment win over the file
func Load(dir string) (Config, error) {
	path := filepath.Join(dir, "engine.yaml")
	f, err := os.Open(path)
	if err != nil {
		return Config{}, err
	}
	defer f.Close()
	var raw file
	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)
	if err := dec.Decode(&raw); err != nil {
		return Config{}, fmt.Errorf("engine config %s: %w", path, err)
	}
	for i := range raw.Chains {
		if v := os.Getenv("TOKENS_RPC_" + strings.ToUpper(raw.Chains[i].Name)); v != "" {
			raw.Chains[i].RPCURL = v
		}
	}
	if v := os.Getenv("TOKENS_PRICES_URL"); v != "" {
		raw.PricesURL = v
	}
	if raw.RPCUpstreamFilter == nil {
		raw.RPCUpstreamFilter = new(defaultUpstreamFilter)
	}
	cfg, err := raw.build()
	if err != nil {
		return Config{}, fmt.Errorf("engine config %s: %w", path, err)
	}
	cfg.HyperSyncKey = Secret(strings.TrimSpace(os.Getenv("ENVIO_HYPERSYNC_KEY")))
	return cfg, nil
}

func (f file) build() (Config, error) {
	if err := f.validate(); err != nil {
		return Config{}, err
	}
	cfg := Config{
		RPCUpstreamFilter:    *f.RPCUpstreamFilter,
		BlockscoutURL:        strings.TrimRight(f.BlockscoutURL, "/"),
		BlockscoutPerSecond:  f.BlockscoutPerSecond,
		GeckoURL:             strings.TrimRight(f.GeckoURL, "/"),
		GeckoNetwork:         f.GeckoNetwork,
		GeckoPerMinute:       f.GeckoPerMinute,
		PricesURL:            strings.TrimRight(f.PricesURL, "/"),
		HyperSyncPerMinute:   f.HyperSyncPerMinute,
		HyperSyncInteractive: f.HyperSyncInteractive,
		MetaCache:            f.MetaCache,
		HoldersCache:         f.HoldersCache,
		GeckoCache:           f.GeckoCache,
		TransfersCache:       f.TransfersCache,
		PriceCache:           f.PriceCache,
		SearchCache:          f.SearchCache,
		TokenListRefresh:     f.TokenListRefresh,
		TokenListTimeout:     f.TokenListTimeout,
		TransferWindowBlocks: f.TransferWindowBlocks,
		TransferMinBlocks:    f.TransferMinBlocks,
		TransferPageBudget:   f.TransferPageBudget,
		Index: Index{
			MaxTokens:            f.Index.MaxTokens,
			BackfillBudget:       f.Index.BackfillBudget,
			ActivityWindowBlocks: f.Index.ActivityWindowBlocks,
			TailInterval:         f.Index.TailInterval,
			LagBlocks:            f.Index.LagBlocks,
			WindowDays:           f.Index.WindowDays,
			TopPerDay:            f.Index.TopPerDay,
			RetryFailedAfter:     f.Index.RetryFailedAfter,
		},
	}
	for _, c := range f.Chains {
		ch := Chain{ID: c.ID, Name: c.Name, RPCURL: c.RPCURL, HyperSyncURL: c.HyperSyncURL, Holders: c.Holders, GeckoNetwork: c.GeckoNetwork}
		if c.WrappedNative != "" {
			ch.WrappedNative = evm.MustAddress(c.WrappedNative)
		}
		cfg.Chains = append(cfg.Chains, ch)
	}
	for _, l := range f.TokenLists {
		cfg.TokenLists = append(cfg.TokenLists, TokenList(l))
	}
	for _, p := range f.Index.Prewarm {
		cfg.Index.Prewarm = append(cfg.Index.Prewarm, evm.MustAddress(p))
	}
	return cfg, nil
}

func (f file) validate() error {
	if err := f.validateChains(); err != nil {
		return err
	}
	switch {
	case !rpc.ValidUpstreamFilter(*f.RPCUpstreamFilter):
		return fmt.Errorf("rpc_upstream_filter must be empty or visible ASCII without spaces, at most %d bytes", rpc.MaxUpstreamFilter)
	case !validURL(f.BlockscoutURL, "https"):
		return errors.New("blockscout_url must be an https URL without credentials")
	case f.BlockscoutPerSecond <= 0 || f.BlockscoutPerSecond > 10:
		return fmt.Errorf("blockscout_per_second must be above 0 and at most 10, got %v", f.BlockscoutPerSecond)
	case !validURL(f.GeckoURL, "https"):
		return errors.New("geckoterminal_url must be an https URL without credentials")
	case !networks.MatchString(f.GeckoNetwork):
		return fmt.Errorf("geckoterminal_network %q is not a network id", f.GeckoNetwork)
	case f.GeckoPerMinute <= 0 || f.GeckoPerMinute > 30:
		return fmt.Errorf("geckoterminal_per_minute must be above 0 and at most 30, got %v", f.GeckoPerMinute)
	case !validURL(f.PricesURL, "http", "https"):
		return errors.New("prices_url (or TOKENS_PRICES_URL) must be an http or https URL without credentials")
	case f.HyperSyncPerMinute <= 0 || f.HyperSyncInteractive <= 0 || f.HyperSyncPerMinute+f.HyperSyncInteractive > 15:
		return fmt.Errorf("hypersync_per_minute and hypersync_interactive_per_minute must be above 0 and add up to at most 15, the whole quota of the shared key, got %v and %v",
			f.HyperSyncPerMinute, f.HyperSyncInteractive)
	case f.MetaCache <= 0 || f.HoldersCache <= 0 || f.GeckoCache <= 0 || f.TransfersCache <= 0 || f.PriceCache <= 0:
		return errors.New("meta_cache, holders_cache, geckoterminal_cache, transfers_cache and price_cache must be positive")
	case f.SearchCache <= 0:
		return errors.New("search_cache must be positive")
	case f.TokenListRefresh < time.Minute || f.TokenListTimeout < time.Second || f.TokenListTimeout > time.Minute:
		return errors.New("token_list_refresh must be at least 1m and token_list_timeout 1s to 1m")
	case f.TransferWindowBlocks < 1 || f.TransferMinBlocks < 1 || f.TransferMinBlocks > f.TransferWindowBlocks:
		return errors.New("transfer_window_blocks and transfer_min_blocks must be positive, with the minimum not above the window")
	case f.TransferPageBudget < 1 || f.TransferPageBudget > 10:
		return fmt.Errorf("transfer_page_budget must be 1 to 10, got %d", f.TransferPageBudget)
	}
	if err := f.validateLists(); err != nil {
		return err
	}
	return f.Index.validate()
}

func (f file) validateLists() error {
	seen := map[string]bool{}
	for _, l := range f.TokenLists {
		switch {
		case !namePat.MatchString(l.Name) || seen[l.Name]:
			return fmt.Errorf("token list name %q must be lowercase letters, digits and dashes, and unique", l.Name)
		case !validURL(l.URL, "https"):
			return fmt.Errorf("url of token list %s must be an https URL without credentials", l.Name)
		}
		seen[l.Name] = true
	}
	return nil
}

func (f file) validateChains() error {
	if len(f.Chains) == 0 {
		return errors.New("chains must list at least one chain")
	}
	seen := map[string]bool{}
	indexed := 0
	for _, c := range f.Chains {
		switch {
		case !caip2.MatchString(c.ID):
			return fmt.Errorf("chain id %q is not an eip155 CAIP-2 id", c.ID)
		case seen[c.ID] || seen[c.Name]:
			return fmt.Errorf("chain %q is listed twice", c.ID)
		case !namePat.MatchString(c.Name):
			return fmt.Errorf("chain name %q must be lowercase letters, digits and dashes", c.Name)
		case !validURL(c.RPCURL, "http", "https"):
			return fmt.Errorf("rpc_url of %s (or TOKENS_RPC_%s) must be an http or https URL without credentials", c.ID, strings.ToUpper(c.Name))
		case !validURL(c.HyperSyncURL, "https"):
			return fmt.Errorf("hypersync_url of %s must be an https URL without credentials", c.ID)
		case !networks.MatchString(c.GeckoNetwork):
			return fmt.Errorf("gecko_network of %s is not a network id", c.ID)
		case c.Holders != HoldersBlockscout && c.Holders != HoldersIndex:
			return fmt.Errorf("holders of %s must be %s or %s", c.ID, HoldersBlockscout, HoldersIndex)
		}
		if c.WrappedNative != "" {
			if _, err := evm.ParseAddress(c.WrappedNative); err != nil {
				return fmt.Errorf("wrapped_native of %s: %w", c.ID, err)
			}
		}
		if c.Holders == HoldersIndex {
			indexed++
		}
		seen[c.ID], seen[c.Name] = true, true
	}
	if indexed > 1 {
		return errors.New("only one chain can take its holders from the index")
	}
	return nil
}

func (x indexFile) validate() error {
	switch {
	case x.MaxTokens < 1 || x.MaxTokens > 1000:
		return fmt.Errorf("index.max_tokens must be 1 to 1000, got %d", x.MaxTokens)
	case x.BackfillBudget < 1:
		return fmt.Errorf("index.backfill_budget must be at least 1, got %d", x.BackfillBudget)
	case x.ActivityWindowBlocks < 1:
		return errors.New("index.activity_window_blocks must be positive")
	case x.TailInterval < 5*time.Second:
		return fmt.Errorf("index.tail_interval must be at least 5s, got %s", x.TailInterval)
	case x.WindowDays < 1 || x.WindowDays > 31:
		return fmt.Errorf("index.window_days must be 1 to 31, got %d", x.WindowDays)
	case x.TopPerDay < 1 || x.TopPerDay > 1000:
		return fmt.Errorf("index.top_per_day must be 1 to 1000, got %d", x.TopPerDay)
	case x.RetryFailedAfter <= 0:
		return errors.New("index.retry_failed_after must be positive")
	case len(x.Prewarm) > x.MaxTokens:
		return errors.New("index.prewarm lists more tokens than index.max_tokens")
	}
	seen := map[evm.Address]bool{}
	for _, p := range x.Prewarm {
		a, err := evm.ParseAddress(p)
		if err != nil {
			return fmt.Errorf("index.prewarm: %w", err)
		}
		if seen[a] {
			return fmt.Errorf("index.prewarm lists %s twice", a)
		}
		seen[a] = true
	}
	return nil
}

// the value stays out of the error text because a URL can carry a token
func validURL(raw string, schemes ...string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return false
	}
	return slices.Contains(schemes, u.Scheme)
}
