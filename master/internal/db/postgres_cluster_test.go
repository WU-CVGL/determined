package db

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAggregationStart(t *testing.T) {
	day := func(d int) *time.Time {
		v := time.Date(2015, 3, d, 0, 0, 0, 0, time.UTC)
		return &v
	}
	cases := []struct {
		name      string
		lastDate  *time.Time
		firstDate *time.Time
		want      *time.Time
	}{
		{"nothing aggregated, no allocations", nil, nil, nil},
		{"nothing aggregated", nil, day(2), day(2)},
		{"no allocation to recompute", day(5), nil, day(6)},
		{"allocation open since an aggregated day", day(5), day(3), day(3)},
		{"allocation started on the last day", day(5), day(5), day(5)},
		{"allocation started after the last day", day(5), day(7), day(6)},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got, ok := aggregationStart(c.lastDate, c.firstDate)
			if c.want == nil {
				require.False(t, ok)
				return
			}
			require.True(t, ok)
			require.Equal(t, *c.want, got)
		})
	}
}
