package sources

import (
	"context"
	"encoding/json/jsontext"
	"encoding/json/v2"
	"errors"
	"io"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"
)

const (
	llamaName    = "defillama"
	llamaTimeout = 90 * time.Second
)

func errNotArray(err error) error {
	if err != nil {
		return err
	}
	return errors.New("the answer is not a JSON array")
}

// Llama reads DefiLlama's keyless APIs: api.llama.fi for TVL, protocols and DEX volume, stablecoins.llama.fi for stablecoins
type Llama struct {
	api         string
	stablecoins string
	hc          *http.Client
	pacer       *Pacer
}

// NewLlama keeps to perMinute requests a minute across both hosts
func NewLlama(api, stablecoins string, perMinute float64) *Llama {
	return &Llama{api: api, stablecoins: stablecoins, hc: newHTTPClient(llamaTimeout), pacer: NewPacer(perMinute, 2)}
}

// WithHTTPClient swaps the client, for tests
func (l *Llama) WithHTTPClient(hc *http.Client) *Llama {
	l.hc = hc
	return l
}

func (l *Llama) get(ctx context.Context, url string, out any) error {
	if err := pausedPacer(ctx, l.pacer, llamaName); err != nil {
		return err
	}
	err := fetchJSON(ctx, l.hc, llamaName, url, nil, out)
	pauseOn429(l.pacer, err, time.Minute)
	return err
}

// TVLPoint is one day of a chain's TVL: the Unix time of the day's start and the dollars
type TVLPoint struct {
	Date int64    `json:"date"`
	TVL  *float64 `json:"tvl"`
}

// ChainTVL reads /v2/historicalChainTvl/{name}, one point a day, the last one the current day so far
func (l *Llama) ChainTVL(ctx context.Context, name string) ([]TVLPoint, error) {
	var out []TVLPoint
	err := l.get(ctx, l.api+"/v2/historicalChainTvl/"+url.PathEscape(name), &out)
	return out, err
}

// ProtocolRow is a protocol's TVL on one chain
type ProtocolRow struct {
	Name     string
	Slug     string
	Category string
	Logo     string
	// TVL is chainTvls[chain]: the protocol's own TVL on the chain, without borrowed, staking or pool2 parts
	TVL float64
	// Change1D and Change7D are the percent changes of the protocol's TVL across all its chains
	Change1D *float64
	Change7D *float64
}

// Protocols reads /protocols (about 9 MB) one protocol at a time and keeps, for each asked chain name, the rows that have TVL on it
func (l *Llama) Protocols(ctx context.Context, names ...string) (map[string][]ProtocolRow, error) {
	if err := pausedPacer(ctx, l.pacer, llamaName); err != nil {
		return nil, err
	}
	out := map[string][]ProtocolRow{}
	err := fetchStream(ctx, l.hc, llamaName, l.api+"/protocols", nil, func(r io.Reader) error {
		dec := jsontext.NewDecoder(r)
		if tok, err := dec.ReadToken(); err != nil || tok.Kind() != '[' {
			return errNotArray(err)
		}
		for dec.PeekKind() != ']' {
			var p struct {
				Name      string             `json:"name"`
				Slug      string             `json:"slug"`
				Category  string             `json:"category"`
				Logo      string             `json:"logo"`
				Change1D  *float64           `json:"change_1d"`
				Change7D  *float64           `json:"change_7d"`
				ChainTVLs map[string]float64 `json:"chainTvls"`
			}
			if err := json.UnmarshalDecode(dec, &p); err != nil {
				return err
			}
			for _, n := range names {
				if v, ok := p.ChainTVLs[n]; ok {
					out[n] = append(out[n], ProtocolRow{Name: p.Name, Slug: p.Slug, Category: p.Category, Logo: p.Logo, TVL: v, Change1D: p.Change1D, Change7D: p.Change7D})
				}
			}
		}
		return nil
	})
	pauseOn429(l.pacer, err, time.Minute)
	if err != nil {
		return nil, err
	}
	return out, nil
}

// Stablecoin is a stablecoin's circulating dollars on one chain, for the USD-pegged ones
type Stablecoin struct {
	Symbol      string
	Name        string
	Circulating float64
}

// Stablecoins reads /stablecoins?includePrices=true and keeps, for each asked chain name, the USD-pegged stablecoins
// with a positive peggedUSD circulating amount on it (chainCirculating[name].current.peggedUSD)
func (l *Llama) Stablecoins(ctx context.Context, names ...string) (map[string][]Stablecoin, error) {
	var raw struct {
		Assets []struct {
			Name   string `json:"name"`
			Symbol string `json:"symbol"`
			Chains map[string]struct {
				Current struct {
					USD *float64 `json:"peggedUSD"`
				} `json:"current"`
			} `json:"chainCirculating"`
		} `json:"peggedAssets"`
	}
	if err := l.get(ctx, l.stablecoins+"/stablecoins?includePrices=true", &raw); err != nil {
		return nil, err
	}
	out := map[string][]Stablecoin{}
	for _, a := range raw.Assets {
		for _, n := range names {
			if c, ok := a.Chains[n]; ok && c.Current.USD != nil && *c.Current.USD > 0 {
				out[n] = append(out[n], Stablecoin{Symbol: a.Symbol, Name: a.Name, Circulating: *c.Current.USD})
			}
		}
	}
	return out, nil
}

// StablePoint is a day of a chain's USD-pegged stablecoin supply: totalCirculatingUSD.peggedUSD
type StablePoint struct {
	Date  int64
	Total float64
}

// StablecoinChart reads /stablecoincharts/{name}, one point a day; a day without a peggedUSD figure is left out
func (l *Llama) StablecoinChart(ctx context.Context, name string) ([]StablePoint, error) {
	var raw []struct {
		Date  int64 `json:"date,string"`
		Total struct {
			USD *float64 `json:"peggedUSD"`
		} `json:"totalCirculatingUSD"`
	}
	if err := l.get(ctx, l.stablecoins+"/stablecoincharts/"+url.PathEscape(name), &raw); err != nil {
		return nil, err
	}
	out := make([]StablePoint, 0, len(raw))
	for _, p := range raw {
		if p.Total.USD != nil {
			out = append(out, StablePoint{Date: p.Date, Total: *p.Total.USD})
		}
	}
	return out, nil
}

// DexProtocol is a DEX protocol's volume on the chain
type DexProtocol struct {
	Name string
	Logo string
	V24h *float64
	V7d  *float64
}

// DexOverview is /overview/dexs/{name} without its methodology text
type DexOverview struct {
	Total24h  *float64
	Total7d   *float64
	Total30d  *float64
	Change1d  *float64
	Chart     []DexPoint
	Protocols []DexProtocol
}

// DexPoint is a day of the chain's DEX volume: the Unix time of the day's start and the dollars
type DexPoint struct {
	Date   int64
	Volume float64
}

// DexVolume reads /overview/dexs/{name} with the daily chart and no breakdown
func (l *Llama) DexVolume(ctx context.Context, name string) (DexOverview, error) {
	var raw struct {
		Total24h  *float64    `json:"total24h"`
		Total7d   *float64    `json:"total7d"`
		Total30d  *float64    `json:"total30d"`
		Change1d  *float64    `json:"change_1d"`
		Chart     [][]float64 `json:"totalDataChart"`
		Protocols []struct {
			Name        string   `json:"name"`
			DisplayName string   `json:"displayName"`
			Logo        string   `json:"logo"`
			Total24h    *float64 `json:"total24h"`
			Total7d     *float64 `json:"total7d"`
		} `json:"protocols"`
	}
	q := "?excludeTotalDataChart=false&excludeTotalDataChartBreakdown=true"
	if err := l.get(ctx, l.api+"/overview/dexs/"+url.PathEscape(name)+q, &raw); err != nil {
		return DexOverview{}, err
	}
	out := DexOverview{Total24h: raw.Total24h, Total7d: raw.Total7d, Total30d: raw.Total30d, Change1d: raw.Change1d}
	for _, p := range raw.Chart {
		if len(p) == 2 {
			out.Chart = append(out.Chart, DexPoint{Date: int64(p[0]), Volume: p[1]})
		}
	}
	for _, p := range raw.Protocols {
		name := p.DisplayName
		if name == "" {
			name = p.Name
		}
		out.Protocols = append(out.Protocols, DexProtocol{Name: strings.TrimSpace(name), Logo: p.Logo, V24h: p.Total24h, V7d: p.Total7d})
	}
	slices.SortStableFunc(out.Protocols, func(a, b DexProtocol) int { return compareDesc(a.V24h, b.V24h) })
	return out, nil
}

func compareDesc(a, b *float64) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return 1
	case b == nil:
		return -1
	case *a > *b:
		return -1
	case *a < *b:
		return 1
	}
	return 0
}
