package internal

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/userusage"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/masterv1"
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

// usageRow is an allocation of a GPU assigned at start and released at end, if set.
func usageRow(start time.Time, end *time.Time) userAllocationRow {
	state := model.AllocationStateRunning
	if end != nil {
		state = model.AllocationStateTerminated
	}
	return userAllocationRow{
		State:        &state,
		MeteredKind:  string(userusage.KindGPU),
		Quantity:     1,
		AssignedTime: &start,
		ReleasedTime: end,
	}
}

func TestUserUsageOpenAllocationCutAtAsOf(t *testing.T) {
	asOf := utc(t, "2026-03-02 06:30")
	rows := []userAllocationRow{usageRow(utc(t, "2026-03-01 22:00"), nil)}
	coverage := day(t, "2026-01-01")
	rangeStart, rangeEnd, err := parseUsageRange("2026-03-01", "2026-03-05", usageDaily)
	require.NoError(t, err)
	from, to, ok := usageEntryBounds(rangeStart, rangeEnd, &coverage, asOf, usageDaily)
	require.True(t, ok)
	entries := userUsage(rows, from, to, asOf, usageDaily)

	require.Len(t, entries, 2, "days after as_of are absent")
	require.Equal(t, "2026-03-01", entries[0].PeriodStart)
	require.InDelta(t, 2*hour, entries[0].GpuSeconds, 1e-6)
	require.Equal(t, "2026-03-02", entries[1].PeriodStart)
	require.InDelta(t, 6.5*hour, entries[1].GpuSeconds, 1e-6)
	require.InDelta(t, 6.5*hour, entries[1].GpuActiveSeconds, 1e-6)
	require.InDelta(t, 6.5*hour, entries[1].GpuOrAuxActiveSeconds, 1e-6)
	require.Equal(t, int32(1), entries[1].GpuActiveDays)
}

func TestUserUsageMonthlyEntries(t *testing.T) {
	asOf := utc(t, "2026-01-01 00:00")
	rows := []userAllocationRow{
		usageRow(utc(t, "2024-01-31 12:00"), ptrs.Ptr(utc(t, "2024-03-01 06:00"))),
	}
	entries := userUsage(rows, day(t, "2024-01-01"), day(t, "2024-04-01"), asOf, usageMonthly)
	require.Len(t, entries, 4)
	for i, want := range []struct {
		label string
		hours float64
		days  int32
	}{{"2024-01", 12, 1}, {"2024-02", 29 * 24, 29}, {"2024-03", 6, 1}, {"2024-04", 0, 0}} {
		require.Equal(t, want.label, entries[i].PeriodStart)
		require.InDelta(t, want.hours*hour, entries[i].GpuSeconds, 1e-6, want.label)
		require.Equal(t, want.days, entries[i].GpuActiveDays, want.label)
		require.Equal(t, want.days, entries[i].GpuOrAuxActiveDays, want.label)
		require.Zero(t, entries[i].AuxActiveDays, want.label)
	}
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

	entries := userUsage(nil, from, to, asOf, usageDaily)
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
	entries = userUsage(nil, from, to, asOf, usageMonthly)
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

// k8sSummary is the summary of a Kubernetes job holding n devices of a type.
func k8sSummary(typ device.Type, n int) sproto.ResourcesSummary {
	return sproto.ResourcesSummary{
		ResourcesType: sproto.ResourcesTypeK8sJob,
		AgentDevices:  map[aproto.ID][]device.Device{"pods": devices(typ, n)},
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

	k8sPools := map[string]bool{"gpu": false, "cpu": false, "k8s": true}
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
		{
			"4 cuda", "gpu",
			[]sproto.ResourcesSummary{containerSummary("a1", devices(device.CUDA, 4)...)},
			ptrs.Ptr(agentrm(4, 0, 0)),
		},
		{
			"2 rocm", "gpu",
			[]sproto.ResourcesSummary{containerSummary("a1", devices(device.ROCM, 2)...)},
			ptrs.Ptr(agentrm(2, 0, 0)),
		},
		{
			"1 cpu", "cpu",
			[]sproto.ResourcesSummary{containerSummary("a1", devices(device.CPU, 1)...)},
			ptrs.Ptr(agentrm(0, 1, 0)),
		},
		{
			"cuda and cpu: the gpus", "gpu",
			[]sproto.ResourcesSummary{containerSummary("a1",
				append(devices(device.CUDA, 1), device.Device{ID: 7, Type: device.CPU})...)},
			ptrs.Ptr(agentrm(1, 0, 0)),
		},
		{
			"unknown device type: not counted", "removed",
			[]sproto.ResourcesSummary{containerSummary("a1", devices("tpu", 2)...)},
			nil,
		},
		{"0 devices", "gpu", []sproto.ResourcesSummary{containerSummary("a1")}, ptrs.Ptr(agentrm(0, 0, 1))},
		{
			"0 devices in an unreported pool", "removed",
			[]sproto.ResourcesSummary{containerSummary("a1")}, ptrs.Ptr(agentrm(0, 0, 1)),
		},
		{"distributed", "gpu", distributed, ptrs.Ptr(agentrm(8, 0, 0))},
		{"distributed after one container is released", "gpu", distributed[1:], ptrs.Ptr(agentrm(4, 0, 0))},
		{"queued", "gpu", nil, nil},
		{"k8s cuda", "k8s", []sproto.ResourcesSummary{k8sSummary(device.CUDA, 8)}, ptrs.Ptr(k8s(8, 0))},
		{"k8s cpu", "k8s", []sproto.ResourcesSummary{k8sSummary(device.CPU, 8)}, ptrs.Ptr(k8s(0, 8))},
		{
			"k8s job in an unreported pool", "removed",
			[]sproto.ResourcesSummary{k8sSummary(device.CUDA, 2)}, ptrs.Ptr(k8s(2, 0)),
		},
		{"k8s without devices", "k8s", []sproto.ResourcesSummary{k8sSummary(device.CUDA, 0)}, nil},
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
			got := currentResources(summaries, map[model.TaskID]bool{"mine": true}, k8sPools)
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
		"t4.1": {TaskID: "t4", ResourcePool: "k8s", Resources: []sproto.ResourcesSummary{
			k8sSummary(device.CUDA, 1),
		}},
	}
	owned := map[model.TaskID]bool{"t1": true, "t2": true, "t3": true, "t4": true}
	got := currentResources(summaries, owned, map[string]bool{"k8s": true})
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
