package internal

import (
	"context"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

// gpuReadingsMatrix is a result at 1794920240 for GPU-b (listed first) and GPU-a. GPU-a used 0 MiB,
// sampled 14.258 s before the query; GPU-b has no total, and neither has reserved. GPU-a draws
// 221.4 W under a 250 W limit whose default is 300 W; neither GPU has an enforced limit.
const gpuReadingsMatrix = `{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"__name__":"DCGM_FI_DEV_FB_USED","gpu_uuid":"GPU-b","det_gpu_reading":"memory_used"},"values":[[1794920240,"11534"]]},
{"metric":{"gpu_uuid":"GPU-b","det_gpu_reading":"memory_used_at"},"values":[[1794920240,"1794920230.5"]]},
{"metric":{"gpu_uuid":"GPU-b","det_gpu_reading":"memory_free"},"values":[[1794920240,"37600"]]},
{"metric":{"gpu_uuid":"GPU-b","det_gpu_reading":"memory_free_at"},"values":[[1794920240,"1794920230.5"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"memory_total"},"values":[[1794920240,"49140"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"memory_total_at"},"values":[[1794920240,"1794920225.742"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"memory_used"},"values":[[1794920240,"0"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"memory_used_at"},"values":[[1794920240,"1794920225.742"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"power_draw"},"values":[[1794920240,"221.4"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"power_draw_at"},"values":[[1794920240,"1794920225.742"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"power_limit"},"values":[[1794920240,"250"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"power_limit_at"},"values":[[1794920240,"1794920225.742"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"power_default_limit"},"values":[[1794920240,"300"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_reading":"power_default_limit_at"},"values":[[1794920240,"1794920225.742"]]}
]}}`

func gpuReadingsConfig(url string) config.TaskResourcesConfig {
	return config.TaskResourcesConfig{PrometheusURL: url, DetCluster: "lab-a"}
}

var (
	bothGroups   = gpuReadingGroups{memory: true, power: true}
	memoryGroups = gpuReadingGroups{memory: true}
	powerGroups  = gpuReadingGroups{power: true}
)

var gpuMemoryMetrics = []string{
	"DCGM_FI_DEV_FB_TOTAL", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_RESERVED",
}

var gpuPowerMetrics = []string{
	"DCGM_FI_DEV_POWER_USAGE", "DCGM_FI_DEV_POWER_MGMT_LIMIT", "DCGM_FI_DEV_POWER_MGMT_LIMIT_DEF",
	"DCGM_FI_DEV_ENFORCED_POWER_LIMIT",
}

func selectorOf(metric string) string {
	return metric + `{job="dcgm",det_cluster="lab-a",gpu_uuid!=""}`
}

func TestReadGPUReadingsQueriesOnceAtOneTime(t *testing.T) {
	prom := newFakeXIDPrometheus(t, gpuReadingsMatrix)
	now := time.Unix(1794920240, 400_000_000)

	readings, err := readGPUReadings(context.Background(), gpuReadingsConfig(prom.URL), now, bothGroups)
	require.NoError(t, err)

	// Memory and power together are one query.
	queries := prom.got()
	require.Len(t, queries, 1)
	q := queries[0]
	require.Equal(t, "1794920240", q.Get("start"))
	require.Equal(t, q.Get("start"), q.Get("end"))
	require.Equal(t, "1", q.Get("step"))
	for _, metric := range append(append([]string{}, gpuMemoryMetrics...), gpuPowerMetrics...) {
		require.Equal(t, 2, strings.Count(q.Get("query"), selectorOf(metric)), metric)
		require.Contains(t, q.Get("query"), "timestamp("+selectorOf(metric)+")")
	}

	a, b := readings["GPU-a"], readings["GPU-b"]
	require.Equal(t, int64(49140)<<20, a.memory.Total.Bytes)
	require.Equal(t, int64(0), a.memory.Used.Bytes)
	// The sample's own time, not the query's.
	require.Equal(t, time.UnixMilli(1794920225742).UTC(), a.memory.Used.SampledAt.AsTime())
	require.Nil(t, a.memory.Free)
	require.Nil(t, a.memory.Reserved)
	require.Nil(t, b.memory.Total)
	require.Equal(t, int64(11534)<<20, b.memory.Used.Bytes)
	require.Equal(t, int64(37600)<<20, b.memory.Free.Bytes)
	require.Equal(t, time.UnixMilli(1794920230500).UTC(), b.memory.Free.SampledAt.AsTime())

	// Watts keep their fraction.
	require.InDelta(t, 221.4, a.power.Draw.Watts, 1e-9)
	require.Equal(t, 250.0, a.power.Limit.Watts)
	require.Equal(t, 300.0, a.power.DefaultLimit.Watts)
	require.Nil(t, a.power.EnforcedLimit)
	require.Equal(t, time.UnixMilli(1794920225742).UTC(), a.power.Draw.SampledAt.AsTime())
	require.Nil(t, b.power)
}

func TestGPUReadingsQueryHasOnlyTheRequestedGroups(t *testing.T) {
	memory := gpuReadingsQuery("lab-a", memoryGroups)
	power := gpuReadingsQuery("lab-a", powerGroups)
	for _, metric := range gpuMemoryMetrics {
		require.Contains(t, memory, selectorOf(metric))
		require.NotContains(t, power, selectorOf(metric))
	}
	for _, metric := range gpuPowerMetrics {
		require.Contains(t, power, selectorOf(metric))
		require.NotContains(t, memory, selectorOf(metric))
	}
}

func TestReadGPUReadingsFailures(t *testing.T) {
	ctx := context.Background()
	_, err := readGPUReadings(ctx, config.TaskResourcesConfig{}, time.Now(), memoryGroups)
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	prom := newFakeXIDPrometheus(t, gpuReadingsMatrix)
	prom.respond(http.StatusInternalServerError, `{"status":"error"}`)
	_, err = readGPUReadings(ctx, gpuReadingsConfig(prom.URL), time.Now(), powerGroups)
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.NotContains(t, err.Error(), prom.URL)

	prom.respond(http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[`)
	_, err = readGPUReadings(ctx, gpuReadingsConfig(prom.URL), time.Now(), memoryGroups)
	require.Equal(t, codes.Unavailable, status.Code(err))

	// A power series in a memory-only result is invalid.
	prom.respond(http.StatusOK, gpuReadingsMatrix)
	_, err = readGPUReadings(ctx, gpuReadingsConfig(prom.URL), time.Now(), memoryGroups)
	require.Equal(t, codes.Internal, status.Code(err))
}

func readingSeries(uuid, tag string, value interface{}) prometheusTaskSeries {
	return prometheusTaskSeries{
		Metric:  map[string]string{"gpu_uuid": uuid, gpuReadingLabel: tag},
		Samples: [][2]interface{}{{float64(1794920240), value}},
	}
}

func TestParseGPUReadingsRejectsInvalidSamples(t *testing.T) {
	at := readingSeries("GPU-a", "memory_used_at", float64(1794920230))
	used := readingSeries("GPU-a", "memory_used", float64(10))
	drawAt := readingSeries("GPU-a", "power_draw_at", float64(1794920230))
	_, err := parseGPUReadings([]prometheusTaskSeries{used, at}, bothGroups)
	require.NoError(t, err)

	cases := map[string][]prometheusTaskSeries{
		"duplicate value":  {used, at, used},
		"duplicate time":   {used, at, at},
		"value no time":    {used},
		"time no value":    {at},
		"negative MiB":     {readingSeries("GPU-a", "memory_used", float64(-1)), at},
		"fraction of MiB":  {readingSeries("GPU-a", "memory_used", 1.5), at},
		"too many MiB":     {readingSeries("GPU-a", "memory_used", float64(1<<25)), at},
		"negative watts":   {readingSeries("GPU-a", "power_draw", -0.5), drawAt},
		"too many watts":   {readingSeries("GPU-a", "power_draw", 10000.5), drawAt},
		"not a number":     {readingSeries("GPU-a", "memory_used", nil), at},
		"no uuid":          {readingSeries("", "memory_used", float64(1)), readingSeries("", "memory_used_at", 1.0)},
		"unknown field":    {readingSeries("GPU-a", "clock", float64(1)), at},
		"two samples":      {{Metric: used.Metric, Samples: [][2]interface{}{{1.0, 1.0}, {2.0, 1.0}}}, at},
		"no samples":       {{Metric: used.Metric}, at},
		"unknown time tag": {used, readingSeries("GPU-a", "clock_at", float64(1))},
	}
	for name, series := range cases {
		_, err := parseGPUReadings(series, bothGroups)
		require.Equal(t, codes.Internal, status.Code(err), name)
	}
}

func TestApplyGPUReadingsByUUID(t *testing.T) {
	agents := func() []*agentv1.Agent {
		return []*agentv1.Agent{
			{Id: "a", GpuTopology: &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{
				{Uuid: "GPU-b"}, {Uuid: "GPU-a"}, {Uuid: "GPU-unsampled", Excluded: true},
			}}},
			{Id: "cpu"},
		}
	}
	readings := map[string]gpuReadings{
		"GPU-a": {
			memory: &agentv1.GpuMemory{Used: &agentv1.GpuMemoryReading{Bytes: 1}},
			power:  &agentv1.GpuPower{Draw: &agentv1.GpuPowerReading{Watts: 1.5}},
		},
		"GPU-b":    {memory: &agentv1.GpuMemory{Used: &agentv1.GpuMemoryReading{Bytes: 2}}},
		"GPU-gone": {memory: &agentv1.GpuMemory{Used: &agentv1.GpuMemoryReading{Bytes: 3}}},
	}

	both := agents()
	require.NoError(t, applyGPUReadings(both, readings, bothGroups))
	gpus := both[0].GpuTopology.Gpus
	require.Equal(t, int64(2), gpus[0].Memory.Used.Bytes)
	require.Equal(t, &agentv1.GpuPower{}, gpus[0].Power)
	require.Equal(t, int64(1), gpus[1].Memory.Used.Bytes)
	require.Equal(t, 1.5, gpus[1].Power.Draw.Watts)
	require.Equal(t, &agentv1.GpuMemory{}, gpus[2].Memory)
	require.Equal(t, &agentv1.GpuPower{}, gpus[2].Power)

	// A group that was not requested stays nil.
	memoryOnly := agents()
	require.NoError(t, applyGPUReadings(memoryOnly, readings, memoryGroups))
	require.Nil(t, memoryOnly[0].GpuTopology.Gpus[1].Power)

	none := []*agentv1.Agent{{GpuTopology: &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{{Uuid: "GPU-z"}}}}}
	require.Equal(t, codes.Unavailable, status.Code(applyGPUReadings(none, readings, memoryGroups)))
	noPower := map[string]gpuReadings{"GPU-a": {memory: readings["GPU-a"].memory}}
	err := applyGPUReadings(agents(), noPower, bothGroups)
	require.Equal(t, errGPUPowerNoSamples, err)
	require.NoError(t, applyGPUReadings([]*agentv1.Agent{{Id: "cpu"}}, nil, bothGroups))
}
