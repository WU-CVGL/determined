package userusage

import (
	"sort"
	"time"
)

const day = 24 * time.Hour

// Record is one allocation as stored. An allocation from before metering has no Assigned time and
// is metered from QueuedEnd, the end of its first QUEUED row, or Start, to End.
type Record struct {
	Kind Kind
	// Quantity is the number of units of Kind held.
	Quantity int
	// Terminal is set when the allocation has ended.
	Terminal bool
	// Assigned and Released are when the resources were assigned and released.
	Assigned, Released *time.Time
	// QueuedEnd, Start and End are the times of an allocation from before metering.
	QueuedEnd, Start, End *time.Time
}

// Interval is the time an allocation held Quantity units of Kind.
type Interval struct {
	Start, End time.Time
	Kind       Kind
	Quantity   float64
}

// Intervals builds at most one interval per record, cut at asOf. A record without a release counts
// until asOf while it runs, and not at all once it has ended. Allocations from before metering are
// also skipped when their start time equals their end time (closed at a master start without
// resources). Records of KindNone, without a quantity or without a length give no interval. The
// master's reads skip the same records in SQL (meteredSQL); the two must agree.
func Intervals(rs []Record, asOf time.Time) []Interval {
	asOf = asOf.UTC()
	intervals := make([]Interval, 0, len(rs))
	for _, r := range rs {
		if r.Quantity <= 0 || r.Kind == KindNone || r.Kind == "" {
			continue
		}
		start, end, ok := r.held(asOf)
		if !ok {
			continue
		}
		intervals = append(intervals, Interval{
			Start: start, End: end, Kind: r.Kind, Quantity: float64(r.Quantity),
		})
	}
	return intervals
}

// held returns when the record's resources were held, up to asOf.
func (r Record) held(asOf time.Time) (start, end time.Time, ok bool) {
	var until *time.Time
	if r.Assigned != nil {
		start, until = r.Assigned.UTC(), r.Released
	} else {
		switch {
		case r.QueuedEnd != nil:
			start = r.QueuedEnd.UTC()
		case r.Start != nil:
			start = r.Start.UTC()
		default:
			return start, end, false
		}
		if r.Start != nil && r.End != nil && r.Start.Equal(*r.End) {
			return start, end, false
		}
		until = r.End
	}
	switch {
	case until != nil:
		end = minTime(until.UTC(), asOf)
	case r.Terminal:
		return start, end, false
	default:
		end = asOf
	}
	return start, end, end.After(start)
}

// Usage is the usage in one period. GPU, CPU, Aux and Unclassified are unit-seconds; the active
// times are the seconds with any GPU, any aux container, or either held; the days count the days
// with any.
type Usage struct {
	GPU, CPU, Aux, Unclassified          float64
	GPUActive, AuxActive, GPUOrAuxActive float64
	GPUDays, AuxDays, GPUOrAuxDays       int
}

// add adds o to u.
func (u *Usage) add(o Usage) {
	u.GPU += o.GPU
	u.CPU += o.CPU
	u.Aux += o.Aux
	u.Unclassified += o.Unclassified
	u.GPUActive += o.GPUActive
	u.AuxActive += o.AuxActive
	u.GPUOrAuxActive += o.GPUOrAuxActive
	u.GPUDays += o.GPUDays
	u.AuxDays += o.AuxDays
	u.GPUOrAuxDays += o.GPUOrAuxDays
}

// Entry is the usage of the period that begins at Start.
type Entry struct {
	Start time.Time
	Usage
}

// span is a half-open time range.
type span struct {
	start, end time.Time
}

// Daily returns one entry per UTC day in [from, to), both UTC midnights.
func Daily(ivs []Interval, from, to time.Time) []Entry {
	from, to = from.UTC(), to.UTC()
	n := int(to.Sub(from) / day)
	if n <= 0 {
		return nil
	}
	entries := make([]Entry, n)
	gpu, aux := make([][]span, n), make([][]span, n)
	for i := range entries {
		entries[i].Start = from.Add(time.Duration(i) * day)
	}
	for _, iv := range ivs {
		start, end := maxTime(iv.Start, from), minTime(iv.End, to)
		if !end.After(start) {
			continue
		}
		for i := int(start.Sub(from) / day); i < n; i++ {
			dayStart := entries[i].Start
			s, e := maxTime(start, dayStart), minTime(end, dayStart.Add(day))
			if !e.After(s) {
				break
			}
			unitSeconds := iv.Quantity * e.Sub(s).Seconds()
			u := &entries[i].Usage
			switch iv.Kind {
			case KindGPU:
				u.GPU += unitSeconds
				gpu[i] = append(gpu[i], span{s, e})
			case KindCPU:
				u.CPU += unitSeconds
			case KindAux:
				u.Aux += unitSeconds
				aux[i] = append(aux[i], span{s, e})
			case KindUnclassified:
				u.Unclassified += unitSeconds
			}
		}
	}
	for i := range entries {
		u := &entries[i].Usage
		u.GPUActive = unionSeconds(gpu[i])
		u.AuxActive = unionSeconds(aux[i])
		u.GPUOrAuxActive = unionSeconds(append(gpu[i], aux[i]...))
		u.GPUDays = activeDay(u.GPUActive)
		u.AuxDays = activeDay(u.AuxActive)
		u.GPUOrAuxDays = activeDay(u.GPUOrAuxActive)
	}
	return entries
}

// Monthly returns one entry per UTC month in [from, to), both first UTC midnights of a month: the
// sum of the month's days.
func Monthly(ivs []Interval, from, to time.Time) []Entry {
	var entries []Entry
	for _, d := range Daily(ivs, from, to) {
		if n := len(entries); n == 0 || entries[n-1].Start.Month() != d.Start.Month() ||
			entries[n-1].Start.Year() != d.Start.Year() {
			entries = append(entries, Entry{
				Start: time.Date(d.Start.Year(), d.Start.Month(), 1, 0, 0, 0, 0, time.UTC),
			})
		}
		entries[len(entries)-1].add(d.Usage)
	}
	return entries
}

// activeDay returns 1 for a day with active seconds, else 0.
func activeDay(seconds float64) int {
	if seconds > 0 {
		return 1
	}
	return 0
}

// unionSeconds returns the length of the union of the spans. It reorders spans.
func unionSeconds(spans []span) float64 {
	if len(spans) == 0 {
		return 0
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].start.Before(spans[j].start) })
	var total time.Duration
	cur := spans[0]
	for _, s := range spans[1:] {
		if s.start.After(cur.end) {
			total += cur.end.Sub(cur.start)
			cur = s
		} else if s.end.After(cur.end) {
			cur.end = s.end
		}
	}
	total += cur.end.Sub(cur.start)
	return total.Seconds()
}

// minTime returns the earlier of a and b.
func minTime(a, b time.Time) time.Time {
	if b.Before(a) {
		return b
	}
	return a
}

// maxTime returns the later of a and b.
func maxTime(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}
