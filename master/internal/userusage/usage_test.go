package userusage

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/ptrs"
)

const hour = 3600.0

// utc parses a "2006-01-02 15:04" UTC time.
func utc(t *testing.T, s string) time.Time {
	v, err := time.Parse("2006-01-02 15:04", s)
	require.NoError(t, err)
	return v
}

// at returns a pointer to the UTC time s.
func at(t *testing.T, s string) *time.Time {
	return ptrs.Ptr(utc(t, s))
}

// assigned is a record assigned at start and released at end, if set.
func assigned(t *testing.T, kind Kind, quantity int, start, end string) Record {
	r := Record{Kind: kind, Quantity: quantity, Assigned: at(t, start)}
	if end != "" {
		r.Released, r.Terminal = at(t, end), true
	}
	return r
}

// daily returns the daily usage of the records from the first to the last day, both inclusive.
func daily(t *testing.T, rs []Record, first, last string, asOf time.Time) []Entry {
	return Daily(Intervals(rs, asOf), utc(t, first+" 00:00"), utc(t, last+" 00:00").Add(day))
}

func TestMidnightSplit(t *testing.T) {
	asOf := utc(t, "2026-03-10 00:00")
	rs := []Record{assigned(t, KindGPU, 1, "2026-03-01 20:00", "2026-03-02 04:00")}
	entries := daily(t, rs, "2026-03-01", "2026-03-03", asOf)
	require.Len(t, entries, 3)
	for i, d := range []string{"2026-03-01", "2026-03-02"} {
		require.Equal(t, utc(t, d+" 00:00"), entries[i].Start)
		require.Equal(t, Usage{
			GPU: 4 * hour, GPUActive: 4 * hour, GPUOrAuxActive: 4 * hour, GPUDays: 1, GPUOrAuxDays: 1,
		}, entries[i].Usage)
	}
	require.Equal(t, Usage{}, entries[2].Usage)
}

// The first allocation, assigned at 23:50 and pulling at 00:10, counts from its assignment.
func TestAssignedBeforeMidnightPulledAfter(t *testing.T) {
	r := assigned(t, KindGPU, 1, "2026-03-01 23:50", "2026-03-02 01:00")
	r.Start = at(t, "2026-03-02 00:10")
	entries := daily(t, []Record{r}, "2026-03-01", "2026-03-02", utc(t, "2026-03-03 00:00"))
	require.InDelta(t, 10*60, entries[0].GPU, 1e-6)
	require.InDelta(t, hour, entries[1].GPU, 1e-6)
}

func TestOpenAllocationCutAtAsOf(t *testing.T) {
	asOf := utc(t, "2026-03-02 06:30")
	rs := []Record{
		assigned(t, KindGPU, 2, "2026-03-01 22:00", ""),
		// Released after asOf.
		assigned(t, KindCPU, 1, "2026-03-02 06:00", "2026-03-02 08:00"),
	}
	entries := daily(t, rs, "2026-03-01", "2026-03-02", asOf)
	require.InDelta(t, 2*2*hour, entries[0].GPU, 1e-6)
	require.InDelta(t, 2*6.5*hour, entries[1].GPU, 1e-6)
	require.InDelta(t, 6.5*hour, entries[1].GPUActive, 1e-6)
	require.InDelta(t, 0.5*hour, entries[1].CPU, 1e-6)
}

func TestIntervals(t *testing.T) {
	asOf := utc(t, "2026-03-01 12:00")
	queued := at(t, "2026-03-01 08:00")
	pulled := at(t, "2026-03-01 09:00")
	ended := at(t, "2026-03-01 10:00")
	cases := []struct {
		name       string
		record     Record
		start, end *time.Time
	}{
		{
			name:   "assigned, then canceled before pulling",
			record: Record{Assigned: queued, Released: pulled, Terminal: true},
			start:  queued, end: pulled,
		},
		{
			name:   "released after its end",
			record: Record{Assigned: queued, Released: pulled, Start: queued, End: ended, Terminal: true},
			start:  queued, end: pulled,
		},
		{
			name:   "running",
			record: Record{Assigned: queued, Start: pulled},
			start:  queued, end: &asOf,
		},
		{name: "ended without a release", record: Record{Assigned: queued, Start: pulled, Terminal: true}},
		{name: "assigned after as_of", record: Record{Assigned: ptrs.Ptr(asOf.Add(time.Hour))}},
		{
			name:   "released after as_of",
			record: Record{Assigned: queued, Released: ptrs.Ptr(asOf.Add(time.Hour)), Terminal: true},
			start:  queued, end: &asOf,
		},
		{name: "released at its assignment", record: Record{Assigned: queued, Released: queued}},

		// Allocations from before metering.
		{
			name:   "legacy: closed at a master start without holding resources",
			record: Record{QueuedEnd: queued, Start: ended, End: ended, Terminal: true},
		},
		{name: "legacy: killed before pulling", record: Record{QueuedEnd: queued, Terminal: true}},
		{
			name:   "legacy: ended without an end time",
			record: Record{QueuedEnd: queued, Start: pulled, Terminal: true},
		},
		{name: "legacy: no QUEUED row and no start time", record: Record{End: ended, Terminal: true}},
		{
			name:   "legacy: the QUEUED end before the start time",
			record: Record{QueuedEnd: queued, Start: pulled, End: ended, Terminal: true},
			start:  queued, end: ended,
		},
		{
			name:   "legacy: no QUEUED row",
			record: Record{Start: pulled, End: ended, Terminal: true},
			start:  pulled, end: ended,
		},
		{
			name:   "legacy: running",
			record: Record{QueuedEnd: queued, Start: pulled},
			start:  queued, end: &asOf,
		},
		{
			name:   "legacy: ended after as_of",
			record: Record{QueuedEnd: queued, End: ptrs.Ptr(asOf.Add(time.Hour)), Terminal: true},
			start:  queued, end: &asOf,
		},
		{name: "legacy: assigned after as_of", record: Record{QueuedEnd: ptrs.Ptr(asOf.Add(time.Hour))}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			c.record.Kind, c.record.Quantity = KindGPU, 1
			intervals := Intervals([]Record{c.record}, asOf)
			if c.start == nil {
				require.Empty(t, intervals)
				return
			}
			require.Equal(t, []Interval{{Start: *c.start, End: *c.end, Kind: KindGPU, Quantity: 1}},
				intervals)
		})
	}

	// Nothing to meter.
	r := Record{Assigned: queued, Released: ended}
	for _, k := range []Record{
		{Kind: KindNone, Quantity: 1}, {Kind: KindGPU}, {Kind: "", Quantity: 1},
	} {
		r.Kind, r.Quantity = k.Kind, k.Quantity
		require.Empty(t, Intervals([]Record{r}, asOf), "%+v", k)
	}
}

// An allocation that ended without a release never accumulates, whatever the time of the read.
func TestTerminalWithoutEnd(t *testing.T) {
	rs := []Record{
		{Kind: KindGPU, Quantity: 1, Terminal: true, Assigned: at(t, "2026-03-01 08:00")},
		{Kind: KindGPU, Quantity: 1, Terminal: true, Start: at(t, "2026-03-01 08:00")},
	}
	for _, asOf := range []string{"2026-03-01 09:00", "2026-04-01 00:00", "2030-01-01 00:00"} {
		require.Empty(t, Intervals(rs, utc(t, asOf)), asOf)
	}
}

func TestActiveModes(t *testing.T) {
	asOf := utc(t, "2026-04-01 00:00")
	rs := []Record{
		// 10:00-12:00 and 11:00-13:00 GPUs; 12:30-14:00 an aux container; CPU and unclassified
		// slots outside them, which are never active.
		assigned(t, KindGPU, 1, "2026-03-01 10:00", "2026-03-01 12:00"),
		assigned(t, KindGPU, 2, "2026-03-01 11:00", "2026-03-01 13:00"),
		assigned(t, KindAux, 1, "2026-03-01 12:30", "2026-03-01 14:00"),
		assigned(t, KindCPU, 4, "2026-03-01 15:00", "2026-03-01 16:00"),
		{
			Kind: KindUnclassified, Quantity: 3, QueuedEnd: at(t, "2026-03-01 17:00"),
			End: at(t, "2026-03-01 18:00"), Terminal: true,
		},
		// Another day: an aux container only.
		assigned(t, KindAux, 1, "2026-03-03 10:00", "2026-03-03 11:00"),
		// Another day: CPU only.
		assigned(t, KindCPU, 1, "2026-03-04 10:00", "2026-03-04 11:00"),
	}
	entries := daily(t, rs, "2026-03-01", "2026-03-04", asOf)
	require.Equal(t, Usage{
		GPU: 2*hour + 2*2*hour, CPU: 4 * hour, Aux: 1.5 * hour, Unclassified: 3 * hour,
		GPUActive: 3 * hour, AuxActive: 1.5 * hour, GPUOrAuxActive: 4 * hour,
		GPUDays: 1, AuxDays: 1, GPUOrAuxDays: 1,
	}, entries[0].Usage)
	require.Equal(t, Usage{}, entries[1].Usage)
	require.Equal(t, Usage{
		Aux: hour, AuxActive: hour, GPUOrAuxActive: hour, AuxDays: 1, GPUOrAuxDays: 1,
	}, entries[2].Usage)
	require.Equal(t, Usage{CPU: hour}, entries[3].Usage)

	months := Monthly(Intervals(rs, asOf), utc(t, "2026-03-01 00:00"), utc(t, "2026-04-01 00:00"))
	require.Len(t, months, 1)
	require.Equal(t, 1, months[0].GPUDays)
	require.Equal(t, 2, months[0].AuxDays)
	require.Equal(t, 2, months[0].GPUOrAuxDays)
	require.InDelta(t, 5*hour, months[0].GPUOrAuxActive, 1e-6)
}

func TestUnionSeconds(t *testing.T) {
	require.Zero(t, unionSeconds(nil))
	require.InDelta(t, 3*hour, unionSeconds([]span{
		{utc(t, "2026-03-01 12:00"), utc(t, "2026-03-01 13:00")},
		{utc(t, "2026-03-01 08:00"), utc(t, "2026-03-01 09:00")},
		{utc(t, "2026-03-01 08:30"), utc(t, "2026-03-01 08:45")},
		{utc(t, "2026-03-01 09:00"), utc(t, "2026-03-01 10:00")},
	}), 1e-6)
}

func TestMonthly(t *testing.T) {
	asOf := utc(t, "2026-01-01 00:00")
	rs := []Record{
		// Leap February, and January and March around it.
		assigned(t, KindGPU, 2, "2024-01-31 12:00", "2024-03-01 06:00"),
		// April has 30 days.
		assigned(t, KindCPU, 1, "2024-04-30 23:00", "2024-05-01 01:00"),
		assigned(t, KindAux, 1, "2024-04-10 00:00", "2024-04-12 00:00"),
	}
	ivs := Intervals(rs, asOf)
	from, to := utc(t, "2024-01-01 00:00"), utc(t, "2024-06-01 00:00")
	months := Monthly(ivs, from, to)
	days := Daily(ivs, from, to)
	require.Len(t, days, 31+29+31+30+31)
	require.Len(t, months, 5)

	wants := []Usage{
		{GPU: 2 * 12 * hour, GPUActive: 12 * hour, GPUOrAuxActive: 12 * hour, GPUDays: 1, GPUOrAuxDays: 1},
		{
			GPU: 2 * 29 * 24 * hour, GPUActive: 29 * 24 * hour, GPUOrAuxActive: 29 * 24 * hour,
			GPUDays: 29, GPUOrAuxDays: 29,
		},
		{GPU: 2 * 6 * hour, GPUActive: 6 * hour, GPUOrAuxActive: 6 * hour, GPUDays: 1, GPUOrAuxDays: 1},
		{CPU: hour, Aux: 48 * hour, AuxActive: 48 * hour, GPUOrAuxActive: 48 * hour, AuxDays: 2, GPUOrAuxDays: 2},
		{CPU: hour},
	}
	for i, w := range wants {
		m := months[i]
		require.Equal(t, time.Date(2024, time.Month(i+1), 1, 0, 0, 0, 0, time.UTC), m.Start)
		require.Equal(t, w, m.Usage, m.Start)

		// A month is the sum of its days.
		var sum Usage
		for _, d := range days {
			if d.Start.Month() == m.Start.Month() {
				sum.add(d.Usage)
			}
		}
		require.Equal(t, sum, m.Usage, m.Start)
	}
	require.Equal(t, utc(t, "2024-02-29 00:00"), days[31+28].Start)
	require.InDelta(t, 2*24*hour, days[31+28].GPU, 1e-6)
}
