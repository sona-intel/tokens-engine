package sources

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"time"
)

// StatPoint is one day of a Blockscout stats line
type StatPoint struct {
	Day   string
	Value float64
}

// StatsLine reads a daily line of Blockscout's stats service (/stats-service/api/v1/lines/{id}) from the day from to the day to,
// as YYYY-MM-DD, at the background rate of the Blockscout pacer
func (b *Blockscout) StatsLine(ctx context.Context, id, from, to string) ([]StatPoint, error) {
	if err := pausedPacer(ctx, b.pacer, blockscoutName); err != nil {
		return nil, err
	}
	var raw struct {
		Chart []struct {
			Date  string `json:"date"`
			Value string `json:"value"`
		} `json:"chart"`
	}
	u := b.base + "/stats-service/api/v1/lines/" + url.PathEscape(id) + "?from=" + url.QueryEscape(from) + "&to=" + url.QueryEscape(to)
	if err := getJSON(ctx, b.hc, blockscoutName, u, &raw); err != nil {
		return nil, err
	}
	out := make([]StatPoint, 0, len(raw.Chart))
	for _, p := range raw.Chart {
		if _, err := time.Parse(time.DateOnly, p.Date); err != nil {
			return nil, &UnavailableError{Source: blockscoutName, Err: fmt.Errorf("stats line %s: bad date %q", id, p.Date)}
		}
		v, err := strconv.ParseFloat(p.Value, 64)
		if err != nil {
			return nil, &UnavailableError{Source: blockscoutName, Err: fmt.Errorf("stats line %s: bad value on %s", id, p.Date)}
		}
		out = append(out, StatPoint{Day: p.Date, Value: v})
	}
	return out, nil
}
