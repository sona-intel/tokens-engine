package markets

import (
	"fmt"
	"testing"

	"tokens/internal/sources"
)

func TestBuildStableTotalCountsEveryCoin(t *testing.T) {
	var list []sources.Stablecoin
	for i := range topLists + 5 {
		list = append(list, sources.Stablecoin{Symbol: fmt.Sprint("S", i), Circulating: 100})
	}
	all := float64(len(list)) * 100
	for _, tc := range []struct {
		name  string
		chart float64
		want  float64
	}{
		{"chart behind the coins", 1000, all},
		{"chart ahead of the coins", all + 50, all + 50},
	} {
		got := buildStable(list, []sources.StablePoint{{Date: 1, Total: 1}, {Date: 2, Total: tc.chart}})
		if got.total != tc.want || len(got.items) != topLists || got.history[1].TotalUSD != tc.chart {
			t.Errorf("%s: total %v items %d history %+v", tc.name, got.total, len(got.items), got.history)
		}
	}
}
