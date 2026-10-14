// Package chainstats counts the daily activity of a chain that has no stats service from its blocks and transactions
package chainstats

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"math/bits"
	"slices"
	"strconv"
	"time"

	"tokens/internal/config"
	"tokens/internal/evm"
	"tokens/internal/markets"
	"tokens/internal/sources"
	"tokens/internal/store"
)

const (
	// probeSpan is how far apart the two blocks are whose times give a backfill its blocks a second
	probeSpan = 1_000_000
	// startMargin starts a backfill this much earlier than the block rate says, so its first whole day is whole
	startMargin = 1.05
)

// Source is the HyperSync client, its background lane shared with every other background job of the engine
type Source interface {
	Blocks(ctx context.Context, chain string, q sources.BlockQuery, prio sources.Priority) (sources.BlockPage, error)
	Height(ctx context.Context, chain string) (uint64, error)
}

// Store keeps the counted days and where the count stands
type Store interface {
	StatsCursor(ctx context.Context, chain string) (store.StatsCursor, bool, error)
	AddStats(ctx context.Context, chain string, from, next uint64, first time.Time, days []store.StatsDay) error
	StatsDays(ctx context.Context, chain, from, to string) ([]store.StatsDay, error)
}

// Counter counts one chain's blocks into days and answers its activity lines
type Counter struct {
	chain  string
	cfg    config.BlockStats
	system map[evm.Address]bool
	src    Source
	st     Store
	log    *slog.Logger
}

// New counts the chain mc on the source's background lane into st
func New(mc config.MarketChain, cfg config.BlockStats, src Source, st Store, log *slog.Logger) *Counter {
	system := map[evm.Address]bool{}
	for _, a := range mc.SystemSenders {
		system[a] = true
	}
	return &Counter{chain: mc.ID, cfg: cfg, system: system, src: src, st: st, log: log}
}

// Chain is the CAIP-2 id of the counted chain
func (c *Counter) Chain() string { return c.chain }

// Run counts up to the head, then waits Poll between reads, until ctx ends
func (c *Counter) Run(ctx context.Context) {
	for {
		// a failed page is asked again only after Poll, so a page that keeps failing costs no more than the count at the head
		if err := c.CatchUp(ctx); err != nil {
			if ctx.Err() != nil {
				return
			}
			c.log.WarnContext(ctx, "chain stats count failed, it resumes from its cursor", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(c.cfg.Poll):
		}
	}
}

// CatchUp counts every block from the cursor to the archive height less LagBlocks
func (c *Counter) CatchUp(ctx context.Context) error {
	height, err := c.src.Height(ctx, c.chain)
	if err != nil {
		return err
	}
	if height < c.cfg.LagBlocks {
		return nil
	}
	head := height - c.cfg.LagBlocks
	cur, ok, err := c.st.StatsCursor(ctx, c.chain)
	if err != nil {
		return err
	}
	from := cur.Next
	if !ok {
		if from, err = c.start(ctx, head); err != nil {
			return err
		}
	}
	for from < head {
		page, err := c.src.Blocks(ctx, c.chain, sources.BlockQuery{From: from, To: head}, sources.Background)
		if err != nil {
			return err
		}
		if page.NextBlock <= from {
			return fmt.Errorf("chain stats: the page from %d did not move", from)
		}
		days, err := c.days(page)
		if err != nil {
			return err
		}
		first := unix(page.Blocks[0].Time)
		if err := c.st.AddStats(ctx, c.chain, from, page.NextBlock, first, days); err != nil {
			return err
		}
		from = page.NextBlock
	}
	return nil
}

// start is a block below the head, or with BackfillDays a block before the midnight that many days ago by the block rate below the head
func (c *Counter) start(ctx context.Context, head uint64) (uint64, error) {
	if c.cfg.BackfillDays == 0 || head <= probeSpan {
		return head - 1, nil
	}
	timeAt := func(n uint64) (uint64, error) {
		page, err := c.src.Blocks(ctx, c.chain, sources.BlockQuery{From: n, To: n + 1}, sources.Background)
		if err != nil {
			return 0, err
		}
		if len(page.Blocks) == 0 {
			return 0, fmt.Errorf("chain stats: block %d has no header", n)
		}
		return page.Blocks[0].Time, nil
	}
	top, err := timeAt(head - 1)
	if err != nil {
		return 0, err
	}
	low, err := timeAt(head - 1 - probeSpan)
	if err != nil {
		return 0, err
	}
	if top <= low {
		return 0, errors.New("chain stats: the probed block times do not rise")
	}
	day := unix(top).Truncate(24*time.Hour).AddDate(0, 0, -c.cfg.BackfillDays)
	back := float64(top-uint64(day.Unix())) * probeSpan / float64(top-low) * startMargin
	if back >= float64(head) {
		return 0, nil
	}
	return head - uint64(back), nil
}

// days sums one page into its UTC days, leaving out the system senders' transactions
func (c *Counter) days(page sources.BlockPage) ([]store.StatsDay, error) {
	type sums struct{ blocks, txs, gasUsed, gasLimit, gasPrice uint64 }
	byDay := map[string]*sums{}
	dayOf := map[uint64]string{}
	add := func(a *uint64, v uint64) bool {
		var carry uint64
		*a, carry = bits.Add64(*a, v, 0)
		return carry == 0
	}
	for _, b := range page.Blocks {
		day := unix(b.Time).Format(time.DateOnly)
		dayOf[b.Number] = day
		s := byDay[day]
		if s == nil {
			s = &sums{}
			byDay[day] = s
		}
		s.blocks++
		if !add(&s.gasUsed, b.GasUsed) || !add(&s.gasLimit, b.GasLimit) {
			return nil, fmt.Errorf("chain stats: the gas of %s overflows a page", day)
		}
	}
	for _, t := range page.Txs {
		if c.system[t.From] {
			continue
		}
		s := byDay[dayOf[t.Block]]
		if s == nil {
			return nil, fmt.Errorf("chain stats: the transaction of block %d has no block in the page", t.Block)
		}
		s.txs++
		if !add(&s.gasPrice, t.GasPrice) {
			return nil, fmt.Errorf("chain stats: the gas prices of %s overflow a page", dayOf[t.Block])
		}
	}
	out := make([]store.StatsDay, 0, len(byDay))
	for day, s := range byDay {
		out = append(out, store.StatsDay{
			Day: day, Blocks: s.blocks, Txs: s.txs, GasUsed: strconv.FormatUint(s.gasUsed, 10),
			GasLimit: strconv.FormatUint(s.gasLimit, 10), GasPriceWei: strconv.FormatUint(s.gasPrice, 10),
		})
	}
	slices.SortFunc(out, func(a, b store.StatsDay) int { return cmp.Compare(a.Day, b.Day) })
	return out, nil
}

func unix(sec uint64) time.Time { return time.Unix(int64(sec), 0).UTC() }

// lines are the Blockscout stats ids a count from blocks answers, each with its value from a counted day
var lines = map[string]func(txs, gasUsed, gasLimit, gasPrice float64) float64{
	"newTxns": func(txs, _, _, _ float64) float64 { return txs },
	// the mean gas price of the day's transactions in gwei, as Blockscout's averageGasPrice
	"averageGasPrice": func(txs, _, _, gasPrice float64) float64 {
		if txs == 0 {
			return 0
		}
		return gasPrice / txs / 1e9
	},
	// the day's gas used over its gas limit as a fraction, as Blockscout's networkUtilization
	"networkUtilization": func(_, gasUsed, gasLimit, _ float64) float64 {
		if gasLimit == 0 {
			return 0
		}
		return gasUsed / gasLimit
	},
}

// StatsLine answers a line of the activity record from the counted days, markets.ErrNoStatsLine for a line blocks do not give
func (c *Counter) StatsLine(ctx context.Context, id, from, to string) ([]sources.StatPoint, error) {
	value, ok := lines[id]
	if !ok {
		return nil, markets.ErrNoStatsLine
	}
	days, err := c.st.StatsDays(ctx, c.chain, from, to)
	if err != nil {
		return nil, err
	}
	out := make([]sources.StatPoint, 0, len(days))
	for _, d := range days {
		var nums [3]float64
		for i, v := range []string{d.GasUsed, d.GasLimit, d.GasPriceWei} {
			if nums[i], err = strconv.ParseFloat(v, 64); err != nil {
				return nil, fmt.Errorf("chain stats of %s: %w", d.Day, err)
			}
		}
		out = append(out, sources.StatPoint{Day: d.Day, Value: value(float64(d.Txs), nums[0], nums[1], nums[2])})
	}
	return out, nil
}
