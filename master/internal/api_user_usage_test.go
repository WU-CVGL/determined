package internal

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/devicev1"
	"github.com/determined-ai/determined/proto/pkg/masterv1"
	"github.com/determined-ai/determined/proto/pkg/resourcepoolv1"
)

const (
	usageDaily   = masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_DAILY
	usageMonthly = masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_MONTHLY
	hour         = 3600.0
)

// utc parses a "2006-01-02 15:04" UTC time.
func utc(t *testing.T, s string) time.Time {
	v, err := time.Parse("2006-01-02 15:04", s)
	require.NoError(t, err)
	return v
}

// day parses a "2006-01-02" UTC day.
func day(t *testing.T, s string) time.Time {
	v, err := time.Parse(usageDayLayout, s)
	require.NoError(t, err)
	return v
}

// usageRow is an allocation assigned at start (its QUEUED end) and ended at end, if set.
func usageRow(pool string, slots int, start time.Time, end *time.Time) userAllocationRow {
	state := model.AllocationStateRunning
	if end != nil {
		state = model.AllocationStateTerminated
	}
	return userAllocationRow{
		AllocationID: fmt.Sprintf("a-%s-%d", start, slots),
		ResourcePool: pool,
		Slots:        slots,
		StartTime:    ptrs.Ptr(start.Add(time.Minute)),
		EndTime:      end,
		State:        &state,
		QueuedEnd:    ptrs.Ptr(start),
	}
}

// testPools has a CUDA, a CPU, an unspecified and a Kubernetes pool.
func testPools() map[string]poolKind {
	return poolKinds(&apiv1.GetResourcePoolsResponse{ResourcePools: []*resourcepoolv1.ResourcePool{
		{Name: "gpu", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
			SlotType: devicev1.Type_TYPE_CUDA},
		{Name: "cpu", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
			SlotType: devicev1.Type_TYPE_CPU},
		{Name: "mixed", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_STATIC,
			SlotType: devicev1.Type_TYPE_UNSPECIFIED},
		{Name: "k8s", Type: resourcepoolv1.ResourcePoolType_RESOURCE_POOL_TYPE_K8S,
			SlotType: devicev1.Type_TYPE_CUDA},
	}})
}

// dailyUsage returns the daily entries of rows from the first to the last day, both inclusive.
func dailyUsage(
	t *testing.T, rows []userAllocationRow, first, last string, asOf time.Time,
	pools map[string]poolKind,
) []*apiv1.UserUsageEntry {
	return userUsage(rows, day(t, first), day(t, last), asOf, pools, usageDaily)
}

func TestUserUsageMidnightSplit(t *testing.T) {
	asOf := utc(t, "2026-03-10 00:00")
	rows := []userAllocationRow{
		usageRow("gpu", 1, utc(t, "2026-03-01 20:00"), ptrs.Ptr(utc(t, "2026-03-02 04:00"))),
	}
	entries := dailyUsage(t, rows, "2026-03-01", "2026-03-03", asOf, testPools())
	require.Len(t, entries, 3)
	for i, label := range []string{"2026-03-01", "2026-03-02"} {
		require.Equal(t, label, entries[i].PeriodStart)
		require.InDelta(t, 4*hour, entries[i].GpuSeconds, 1e-6)
		require.InDelta(t, 4*hour, entries[i].ActiveSeconds, 1e-6)
		require.Equal(t, int32(1), entries[i].ActiveDays)
		require.Zero(t, entries[i].CpuSlotSeconds)
		require.Zero(t, *entries[i].AuxContainerSeconds)
	}
	require.Equal(t, "2026-03-03", entries[2].PeriodStart)
	require.Zero(t, entries[2].ActiveSeconds)
	require.Zero(t, entries[2].ActiveDays)
}

func TestUserUsageOpenAllocationCutAtAsOf(t *testing.T) {
	asOf := utc(t, "2026-03-02 06:30")
	rows := []userAllocationRow{usageRow("gpu", 2, utc(t, "2026-03-01 22:00"), nil)}
	coverage := day(t, "2026-01-01")
	rangeStart, rangeEnd, err := parseUsageRange("2026-03-01", "2026-03-05", usageDaily)
	require.NoError(t, err)
	from, to, ok := usageEntryBounds(rangeStart, rangeEnd, &coverage, asOf, usageDaily)
	require.True(t, ok)
	entries := userUsage(rows, from, to, asOf, testPools(), usageDaily)

	require.Len(t, entries, 2, "days after as_of are absent")
	require.Equal(t, "2026-03-01", entries[0].PeriodStart)
	require.InDelta(t, 2*2*hour, entries[0].GpuSeconds, 1e-6)
	require.Equal(t, "2026-03-02", entries[1].PeriodStart)
	require.InDelta(t, 2*6.5*hour, entries[1].GpuSeconds, 1e-6)
	require.InDelta(t, 6.5*hour, entries[1].ActiveSeconds, 1e-6)
}

func TestUserUsageAux(t *testing.T) {
	asOf := utc(t, "2026-03-10 00:00")
	start, end := utc(t, "2026-03-01 10:00"), ptrs.Ptr(utc(t, "2026-03-01 13:00"))

	entries := dailyUsage(t, []userAllocationRow{usageRow("gpu", 0, start, end)},
		"2026-03-01", "2026-03-01", asOf, testPools())
	require.Len(t, entries, 1)
	require.Zero(t, entries[0].GpuSeconds)
	require.Zero(t, entries[0].CpuSlotSeconds)
	require.InDelta(t, 3*hour, *entries[0].AuxContainerSeconds, 1e-6)
	require.InDelta(t, 3*hour, entries[0].ActiveSeconds, 1e-6)

	// A zero-slot allocation in a Kubernetes pool counts nothing.
	entries = dailyUsage(t, []userAllocationRow{usageRow("k8s", 0, start, end)},
		"2026-03-01", "2026-03-01", asOf, testPools())
	require.Len(t, entries, 1)
	require.Zero(t, *entries[0].AuxContainerSeconds)
	require.Zero(t, entries[0].ActiveSeconds)
	require.Zero(t, entries[0].ActiveDays)

	// Aux is unset when every pool is Kubernetes.
	k8sOnly := map[string]poolKind{"k8s": {k8s: true}}
	entries = dailyUsage(t, []userAllocationRow{
		usageRow("k8s", 0, start, end), usageRow("k8s", 2, start, end),
	}, "2026-03-01", "2026-03-01", asOf, k8sOnly)
	require.Len(t, entries, 1)
	require.Nil(t, entries[0].AuxContainerSeconds)
	require.InDelta(t, 2*3*hour, entries[0].GpuSeconds, 1e-6)
	require.InDelta(t, 3*hour, entries[0].ActiveSeconds, 1e-6)
}

func TestUserUsagePoolSlotType(t *testing.T) {
	asOf := utc(t, "2026-03-10 00:00")
	start, end := utc(t, "2026-03-01 10:00"), ptrs.Ptr(utc(t, "2026-03-01 11:00"))
	cases := []struct {
		pool          string
		gpus, cpuSlot float64
	}{
		{"cpu", 0, 3 * hour},
		{"gpu", 3 * hour, 0},
		{"mixed", 3 * hour, 0},
		{"removed", 3 * hour, 0},
		{"k8s", 3 * hour, 0},
	}
	for _, c := range cases {
		t.Run(c.pool, func(t *testing.T) {
			entries := dailyUsage(t, []userAllocationRow{usageRow(c.pool, 3, start, end)},
				"2026-03-01", "2026-03-01", asOf, testPools())
			require.Len(t, entries, 1)
			require.InDelta(t, c.gpus, entries[0].GpuSeconds, 1e-6)
			require.InDelta(t, c.cpuSlot, entries[0].CpuSlotSeconds, 1e-6)
			require.InDelta(t, hour, entries[0].ActiveSeconds, 1e-6)
		})
	}
}

func TestUserUsageIntervalGuards(t *testing.T) {
	asOf := utc(t, "2026-03-01 12:00")
	queued := utc(t, "2026-03-01 08:00")
	pulled := utc(t, "2026-03-01 09:00")
	ended := utc(t, "2026-03-01 10:00")
	terminated := model.AllocationStateTerminated
	running := model.AllocationStateRunning
	cases := []struct {
		name  string
		row   userAllocationRow
		start time.Time
		end   time.Time
		none  bool
	}{
		{
			name: "closed at a master start without holding resources",
			row: userAllocationRow{
				Slots: 1, QueuedEnd: &queued, StartTime: &ended, EndTime: &ended, State: &terminated,
			},
			none: true,
		},
		{
			name: "killed before pulling",
			row:  userAllocationRow{Slots: 1, QueuedEnd: &queued, State: &terminated},
			none: true,
		},
		{
			name:  "in finalize's write window",
			row:   userAllocationRow{Slots: 1, QueuedEnd: &queued, StartTime: &pulled, State: &terminated},
			start: queued,
			end:   asOf,
		},
		{
			name: "no QUEUED row and no start time",
			row:  userAllocationRow{Slots: 1, EndTime: &ended, State: &terminated},
			none: true,
		},
		{
			name:  "the QUEUED end before the start time",
			row:   userAllocationRow{Slots: 1, QueuedEnd: &queued, StartTime: &pulled, EndTime: &ended},
			start: queued,
			end:   ended,
		},
		{
			name:  "no QUEUED row",
			row:   userAllocationRow{Slots: 1, StartTime: &pulled, EndTime: &ended, State: &terminated},
			start: pulled,
			end:   ended,
		},
		{
			name:  "running",
			row:   userAllocationRow{Slots: 1, QueuedEnd: &queued, StartTime: &pulled, State: &running},
			start: queued,
			end:   asOf,
		},
		{
			name:  "ended after as_of",
			row:   userAllocationRow{Slots: 1, QueuedEnd: &queued, EndTime: ptrs.Ptr(asOf.Add(time.Hour))},
			start: queued,
			end:   asOf,
		},
		{
			name: "assigned after as_of",
			row:  userAllocationRow{Slots: 1, QueuedEnd: ptrs.Ptr(asOf.Add(time.Hour)), State: &running},
			none: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			intervals := usageIntervals([]userAllocationRow{c.row}, asOf, testPools())
			if c.none {
				require.Empty(t, intervals)
				return
			}
			require.Len(t, intervals, 1)
			require.Equal(t, c.start, intervals[0].start)
			require.Equal(t, c.end, intervals[0].end)
		})
	}
}

func TestUserUsageUnion(t *testing.T) {
	asOf := utc(t, "2026-03-10 00:00")
	rows := []userAllocationRow{
		usageRow("gpu", 1, utc(t, "2026-03-01 10:00"), ptrs.Ptr(utc(t, "2026-03-01 12:00"))),
		usageRow("gpu", 1, utc(t, "2026-03-01 11:00"), ptrs.Ptr(utc(t, "2026-03-01 13:00"))),
	}
	entries := dailyUsage(t, rows, "2026-03-01", "2026-03-01", asOf, testPools())
	require.Len(t, entries, 1)
	require.InDelta(t, 4*hour, entries[0].GpuSeconds, 1e-6)
	require.InDelta(t, 3*hour, entries[0].ActiveSeconds, 1e-6)

	require.Zero(t, unionSeconds(nil))
	require.InDelta(t, 3*hour, unionSeconds([]timeSpan{
		{utc(t, "2026-03-01 12:00"), utc(t, "2026-03-01 13:00")},
		{utc(t, "2026-03-01 08:00"), utc(t, "2026-03-01 09:00")},
		{utc(t, "2026-03-01 08:30"), utc(t, "2026-03-01 08:45")},
		{utc(t, "2026-03-01 09:00"), utc(t, "2026-03-01 10:00")},
	}), 1e-6)
}

func TestUserUsageMonthly(t *testing.T) {
	asOf := utc(t, "2026-01-01 00:00")
	rows := []userAllocationRow{
		// Leap February, and January and March around it.
		usageRow("gpu", 2, utc(t, "2024-01-31 12:00"), ptrs.Ptr(utc(t, "2024-03-01 06:00"))),
		// April has 30 days.
		usageRow("cpu", 1, utc(t, "2024-04-30 23:00"), ptrs.Ptr(utc(t, "2024-05-01 01:00"))),
		usageRow("gpu", 0, utc(t, "2024-04-10 00:00"), ptrs.Ptr(utc(t, "2024-04-12 00:00"))),
	}
	pools := testPools()
	months := userUsage(rows, day(t, "2024-01-01"), day(t, "2024-05-01"), asOf, pools, usageMonthly)
	days := userUsage(rows, day(t, "2024-01-01"), day(t, "2024-05-31"), asOf, pools, usageDaily)
	require.Len(t, days, 31+29+31+30+31)
	require.Len(t, months, 5)

	type want struct {
		label                 string
		gpu, cpu, aux, active float64
		activeDays            int32
	}
	wants := []want{
		{"2024-01", 2 * 12 * hour, 0, 0, 12 * hour, 1},
		{"2024-02", 2 * 29 * 24 * hour, 0, 0, 29 * 24 * hour, 29},
		{"2024-03", 2 * 6 * hour, 0, 0, 6 * hour, 1},
		{"2024-04", 0, hour, 48 * hour, 49 * hour, 3},
		{"2024-05", 0, hour, 0, hour, 1},
	}
	for i, w := range wants {
		m := months[i]
		require.Equal(t, w.label, m.PeriodStart)
		require.InDelta(t, w.gpu, m.GpuSeconds, 1e-6, w.label)
		require.InDelta(t, w.cpu, m.CpuSlotSeconds, 1e-6, w.label)
		require.InDelta(t, w.aux, *m.AuxContainerSeconds, 1e-6, w.label)
		require.InDelta(t, w.active, m.ActiveSeconds, 1e-6, w.label)
		require.Equal(t, w.activeDays, m.ActiveDays, w.label)

		// A month is the sum of its days.
		var gpu, cpu, aux, active float64
		var activeDays int32
		for _, d := range days {
			if d.PeriodStart[:7] == w.label {
				gpu += d.GpuSeconds
				cpu += d.CpuSlotSeconds
				aux += *d.AuxContainerSeconds
				active += d.ActiveSeconds
				activeDays += d.ActiveDays
			}
		}
		require.InDelta(t, gpu, m.GpuSeconds, 1e-6, w.label)
		require.InDelta(t, cpu, m.CpuSlotSeconds, 1e-6, w.label)
		require.InDelta(t, aux, *m.AuxContainerSeconds, 1e-6, w.label)
		require.InDelta(t, active, m.ActiveSeconds, 1e-6, w.label)
		require.Equal(t, activeDays, m.ActiveDays, w.label)
	}
	require.Equal(t, "2024-02-29", days[31+28].PeriodStart)
	require.InDelta(t, 2*24*hour, days[31+28].GpuSeconds, 1e-6)
}

func TestUserUsageRange(t *testing.T) {
	invalid := func(t *testing.T, start, end string, period masterv1.ResourceAllocationAggregationPeriod) {
		_, _, err := parseUsageRange(start, end, period)
		require.Equal(t, codes.InvalidArgument, status.Code(err), "%s..%s: %v", start, end, err)
	}

	start, end, err := parseUsageRange("2026-01-01", "2027-02-04", usageDaily)
	require.NoError(t, err)
	require.Equal(t, day(t, "2026-01-01"), start)
	require.Equal(t, day(t, "2027-02-05"), end)
	require.Equal(t, userUsageMaxDays, int(end.Sub(start)/usageDay))
	invalid(t, "2026-01-01", "2027-02-05", usageDaily)

	start, end, err = parseUsageRange("2024-01", "2026-12", usageMonthly)
	require.NoError(t, err)
	require.Equal(t, day(t, "2024-01-01"), start)
	require.Equal(t, day(t, "2027-01-01"), end)
	invalid(t, "2024-01", "2027-01", usageMonthly)

	start, end, err = parseUsageRange("2026-03-05", "2026-03-05", usageDaily)
	require.NoError(t, err)
	require.Equal(t, usageDay, end.Sub(start))

	invalid(t, "2026-03-06", "2026-03-05", usageDaily)
	invalid(t, "2026-04", "2026-03", usageMonthly)
	invalid(t, "2026-03", "2026-03-05", usageDaily)
	invalid(t, "2026-03-05", "2026-3-6", usageDaily)
	invalid(t, "2026-03-05", "2026-03", usageMonthly)
	invalid(t, "2026-03", "2026-04",
		masterv1.ResourceAllocationAggregationPeriod_RESOURCE_ALLOCATION_AGGREGATION_PERIOD_UNSPECIFIED)
}

func TestUserUsageEntryBounds(t *testing.T) {
	asOf := utc(t, "2026-03-20 15:00")
	coverage := day(t, "2026-03-10")

	rangeStart, rangeEnd, err := parseUsageRange("2026-03-01", "2026-03-31", usageDaily)
	require.NoError(t, err)
	from, to, ok := usageEntryBounds(rangeStart, rangeEnd, &coverage, asOf, usageDaily)
	require.True(t, ok)
	require.Equal(t, coverage, from)
	require.Equal(t, day(t, "2026-03-20"), to)

	entries := userUsage(nil, from, to, asOf, testPools(), usageDaily)
	require.Len(t, entries, 11)
	require.Equal(t, "2026-03-10", entries[0].PeriodStart)
	require.Equal(t, "2026-03-20", entries[10].PeriodStart)

	_, _, ok = usageEntryBounds(rangeStart, rangeEnd, nil, asOf, usageDaily)
	require.False(t, ok, "no records")
	late := day(t, "2026-04-02")
	_, _, ok = usageEntryBounds(rangeStart, rangeEnd, &late, asOf, usageDaily)
	require.False(t, ok, "records start after the range")
	_, _, ok = usageEntryBounds(rangeStart, rangeEnd, &coverage, utc(t, "2026-02-28 23:00"), usageDaily)
	require.False(t, ok, "the range starts after as_of")

	rangeStart, rangeEnd, err = parseUsageRange("2025-01", "2026-12", usageMonthly)
	require.NoError(t, err)
	from, to, ok = usageEntryBounds(rangeStart, rangeEnd, &coverage, asOf, usageMonthly)
	require.True(t, ok)
	require.Equal(t, day(t, "2026-03-01"), from)
	require.Equal(t, day(t, "2026-03-01"), to)
	entries = userUsage(nil, from, to, asOf, testPools(), usageMonthly)
	require.Len(t, entries, 1)
	require.Equal(t, "2026-03", entries[0].PeriodStart)
}

// containerSummary is the summary of one agentrm container holding devices.
func containerSummary(agent string, devices ...device.Device) sproto.ResourcesSummary {
	return sproto.ResourcesSummary{
		ResourcesType: sproto.ResourcesTypeDockerContainer,
		AgentDevices:  map[aproto.ID][]device.Device{aproto.ID(agent): devices},
	}
}

// k8sSummary is the summary of a Kubernetes job holding n typeless devices.
func k8sSummary(n int) sproto.ResourcesSummary {
	return sproto.ResourcesSummary{
		ResourcesType: sproto.ResourcesTypeK8sJob,
		AgentDevices:  map[aproto.ID][]device.Device{"pods": make([]device.Device, n)},
	}
}

// devices returns n devices of a type.
func devices(typ device.Type, n int) []device.Device {
	result := make([]device.Device, n)
	for i := range result {
		result[i] = device.Device{ID: device.ID(i), Type: typ}
	}
	return result
}

func TestCurrentResources(t *testing.T) {
	type counts struct {
		gpus, cpuSlots int32
		aux            *int32
	}
	agentrm := func(gpus, cpuSlots, aux int32) counts { return counts{gpus, cpuSlots, &aux} }
	k8s := func(gpus, cpuSlots int32) counts { return counts{gpus, cpuSlots, nil} }

	pools := testPools()
	pools["k8s-cpu"] = poolKind{k8s: true, cpu: true}
	distributed := []sproto.ResourcesSummary{
		containerSummary("a1", devices(device.CUDA, 4)...),
		containerSummary("a2", devices(device.CUDA, 4)...),
	}
	cases := []struct {
		name      string
		pool      string
		resources []sproto.ResourcesSummary
		want      *counts
	}{
		{"4 cuda", "gpu", []sproto.ResourcesSummary{containerSummary("a1", devices(device.CUDA, 4)...)},
			ptrs.Ptr(agentrm(4, 0, 0))},
		{"2 rocm", "gpu", []sproto.ResourcesSummary{containerSummary("a1", devices(device.ROCM, 2)...)},
			ptrs.Ptr(agentrm(2, 0, 0))},
		{"1 cpu", "cpu", []sproto.ResourcesSummary{containerSummary("a1", devices(device.CPU, 1)...)},
			ptrs.Ptr(agentrm(0, 1, 0))},
		{"typeless in a CPU pool", "cpu",
			[]sproto.ResourcesSummary{containerSummary("a1", devices(device.ZeroSlot, 2)...)},
			ptrs.Ptr(agentrm(0, 2, 0))},
		{"typeless in an unknown pool", "removed",
			[]sproto.ResourcesSummary{containerSummary("a1", devices(device.ZeroSlot, 2)...)},
			ptrs.Ptr(agentrm(2, 0, 0))},
		{"0 devices", "gpu", []sproto.ResourcesSummary{containerSummary("a1")}, ptrs.Ptr(agentrm(0, 0, 1))},
		{"distributed", "gpu", distributed, ptrs.Ptr(agentrm(8, 0, 0))},
		{"distributed after one container is released", "gpu", distributed[1:], ptrs.Ptr(agentrm(4, 0, 0))},
		{"queued", "gpu", nil, nil},
		{"k8s in a CUDA pool", "k8s", []sproto.ResourcesSummary{k8sSummary(8)}, ptrs.Ptr(k8s(8, 0))},
		{"k8s in a CPU pool", "k8s-cpu", []sproto.ResourcesSummary{k8sSummary(8)}, ptrs.Ptr(k8s(0, 8))},
		{"k8s without devices", "k8s", []sproto.ResourcesSummary{k8sSummary(0)}, nil},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			summaries := map[model.AllocationID]sproto.AllocationSummary{
				"mine.1": {TaskID: "mine", AllocationID: "mine.1", ResourcePool: c.pool, Resources: c.resources},
				"other.1": {
					TaskID: "other", AllocationID: "other.1", ResourcePool: c.pool,
					Resources: []sproto.ResourcesSummary{containerSummary("a3", devices(device.CUDA, 1)...)},
				},
			}
			got := currentResources(summaries, map[model.TaskID]bool{"mine": true}, pools)
			if c.want == nil {
				require.Empty(t, got)
				return
			}
			require.Len(t, got, 1)
			require.Equal(t, c.pool, got[0].ResourcePool)
			require.Equal(t, c.want.gpus, got[0].Counts.Gpus)
			require.Equal(t, c.want.cpuSlots, got[0].Counts.CpuSlots)
			require.Equal(t, c.want.aux, got[0].Counts.AuxContainers)
		})
	}
}

func TestCurrentResourcesByPool(t *testing.T) {
	summaries := map[model.AllocationID]sproto.AllocationSummary{
		"t1.1": {TaskID: "t1", ResourcePool: "gpu", Resources: []sproto.ResourcesSummary{
			containerSummary("a1", devices(device.CUDA, 2)...),
		}},
		"t2.1": {TaskID: "t2", ResourcePool: "gpu", Resources: []sproto.ResourcesSummary{containerSummary("a1")}},
		"t3.1": {TaskID: "t3", ResourcePool: "cpu", Resources: []sproto.ResourcesSummary{
			containerSummary("c1", devices(device.CPU, 3)...),
		}},
		"t4.1": {TaskID: "t4", ResourcePool: "k8s", Resources: []sproto.ResourcesSummary{k8sSummary(1)}},
	}
	owned := map[model.TaskID]bool{"t1": true, "t2": true, "t3": true, "t4": true}
	got := currentResources(summaries, owned, testPools())
	require.Len(t, got, 3)
	require.Equal(t, "cpu", got[0].ResourcePool)
	require.Equal(t, int32(3), got[0].Counts.CpuSlots)
	require.Equal(t, int32(0), *got[0].Counts.AuxContainers)
	require.Equal(t, "gpu", got[1].ResourcePool)
	require.Equal(t, int32(2), got[1].Counts.Gpus)
	require.Equal(t, int32(1), *got[1].Counts.AuxContainers)
	require.Equal(t, "k8s", got[2].ResourcePool)
	require.Equal(t, int32(1), got[2].Counts.Gpus)
	require.Nil(t, got[2].Counts.AuxContainers)
}
