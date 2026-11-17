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

// gpuMemoryMatrix is a result at 1794920240 for GPU-b (listed first) and GPU-a. GPU-a used 0 MiB,
// sampled 14.258 s before the query; GPU-b has no total, and neither has reserved.
const gpuMemoryMatrix = `{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"__name__":"DCGM_FI_DEV_FB_USED","gpu_uuid":"GPU-b","det_gpu_memory_field":"used"},"values":[[1794920240,"11534"]]},
{"metric":{"gpu_uuid":"GPU-b","det_gpu_memory_field":"used_at"},"values":[[1794920240,"1794920230.5"]]},
{"metric":{"gpu_uuid":"GPU-b","det_gpu_memory_field":"free"},"values":[[1794920240,"37600"]]},
{"metric":{"gpu_uuid":"GPU-b","det_gpu_memory_field":"free_at"},"values":[[1794920240,"1794920230.5"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_memory_field":"total"},"values":[[1794920240,"49140"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_memory_field":"total_at"},"values":[[1794920240,"1794920225.742"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_memory_field":"used"},"values":[[1794920240,"0"]]},
{"metric":{"gpu_uuid":"GPU-a","det_gpu_memory_field":"used_at"},"values":[[1794920240,"1794920225.742"]]}
]}}`

func gpuMemoryConfig(url string) config.TaskResourcesConfig {
	return config.TaskResourcesConfig{PrometheusURL: url, DetCluster: "lab-a"}
}

func TestReadGPUMemoryQueriesOnceAtOneTime(t *testing.T) {
	prom := newFakeXIDPrometheus(t, gpuMemoryMatrix)
	now := time.Unix(1794920240, 400_000_000)

	memory, err := readGPUMemory(context.Background(), gpuMemoryConfig(prom.URL), now)
	require.NoError(t, err)

	queries := prom.got()
	require.Len(t, queries, 1)
	q := queries[0]
	require.Equal(t, "1794920240", q.Get("start"))
	require.Equal(t, q.Get("start"), q.Get("end"))
	require.Equal(t, "1", q.Get("step"))
	for _, metric := range []string{
		"DCGM_FI_DEV_FB_TOTAL", "DCGM_FI_DEV_FB_USED", "DCGM_FI_DEV_FB_FREE", "DCGM_FI_DEV_FB_RESERVED",
	} {
		selector := metric + `{job="dcgm",det_cluster="lab-a",gpu_uuid!=""}`
		require.Equal(t, 2, strings.Count(q.Get("query"), selector), metric)
		require.Contains(t, q.Get("query"), "timestamp("+selector+")")
	}

	a, b := memory["GPU-a"], memory["GPU-b"]
	require.Equal(t, int64(49140)<<20, a.Total.Bytes)
	require.Equal(t, int64(0), a.Used.Bytes)
	// The sample's own time, not the query's.
	require.Equal(t, time.UnixMilli(1794920225742).UTC(), a.Used.SampledAt.AsTime())
	require.Nil(t, a.Free)
	require.Nil(t, a.Reserved)
	require.Nil(t, b.Total)
	require.Equal(t, int64(11534)<<20, b.Used.Bytes)
	require.Equal(t, int64(37600)<<20, b.Free.Bytes)
	require.Equal(t, time.UnixMilli(1794920230500).UTC(), b.Free.SampledAt.AsTime())
}

func TestReadGPUMemoryFailures(t *testing.T) {
	ctx := context.Background()
	_, err := readGPUMemory(ctx, config.TaskResourcesConfig{}, time.Now())
	require.Equal(t, codes.FailedPrecondition, status.Code(err))

	prom := newFakeXIDPrometheus(t, gpuMemoryMatrix)
	prom.respond(http.StatusInternalServerError, `{"status":"error"}`)
	_, err = readGPUMemory(ctx, gpuMemoryConfig(prom.URL), time.Now())
	require.Equal(t, codes.Unavailable, status.Code(err))
	require.NotContains(t, err.Error(), prom.URL)

	prom.respond(http.StatusOK, `{"status":"success","data":{"resultType":"matrix","result":[`)
	_, err = readGPUMemory(ctx, gpuMemoryConfig(prom.URL), time.Now())
	require.Equal(t, codes.Unavailable, status.Code(err))
}

func memorySeries(uuid, tag string, value interface{}) prometheusTaskSeries {
	return prometheusTaskSeries{
		Metric:  map[string]string{"gpu_uuid": uuid, gpuMemoryFieldLabel: tag},
		Samples: [][2]interface{}{{float64(1794920240), value}},
	}
}

func TestParseGPUMemoryRejectsInvalidSamples(t *testing.T) {
	valid := []prometheusTaskSeries{
		memorySeries("GPU-a", "used", float64(10)), memorySeries("GPU-a", "used_at", float64(1794920230)),
	}
	_, err := parseGPUMemory(valid)
	require.NoError(t, err)

	cases := map[string][]prometheusTaskSeries{
		"duplicate value":  append(append([]prometheusTaskSeries{}, valid...), valid[0]),
		"duplicate time":   append(append([]prometheusTaskSeries{}, valid...), valid[1]),
		"value no time":    valid[:1],
		"time no value":    valid[1:],
		"negative":         {memorySeries("GPU-a", "used", float64(-1)), valid[1]},
		"fraction of MiB":  {memorySeries("GPU-a", "used", 1.5), valid[1]},
		"too large":        {memorySeries("GPU-a", "used", float64(1<<25)), valid[1]},
		"not a number":     {memorySeries("GPU-a", "used", nil), valid[1]},
		"no uuid":          {memorySeries("", "used", float64(1)), memorySeries("", "used_at", float64(1))},
		"unknown field":    {memorySeries("GPU-a", "clock", float64(1)), valid[1]},
		"two samples":      {{Metric: valid[0].Metric, Samples: [][2]interface{}{{1.0, 1.0}, {2.0, 1.0}}}, valid[1]},
		"no samples":       {{Metric: valid[0].Metric}, valid[1]},
		"unknown time tag": {valid[0], memorySeries("GPU-a", "clock_at", float64(1))},
	}
	for name, series := range cases {
		_, err := parseGPUMemory(series)
		require.Equal(t, codes.Internal, status.Code(err), name)
	}
}

func TestApplyGPUMemoryByUUID(t *testing.T) {
	agents := []*agentv1.Agent{
		{Id: "a", GpuTopology: &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{
			{Uuid: "GPU-b"}, {Uuid: "GPU-a"}, {Uuid: "GPU-unsampled", Excluded: true},
		}}},
		{Id: "cpu"},
	}
	memory := map[string]*agentv1.GpuMemory{
		"GPU-a":    {Used: &agentv1.GpuMemoryReading{Bytes: 1}},
		"GPU-b":    {Used: &agentv1.GpuMemoryReading{Bytes: 2}},
		"GPU-gone": {Used: &agentv1.GpuMemoryReading{Bytes: 3}},
	}
	require.NoError(t, applyGPUMemory(agents, memory))
	gpus := agents[0].GpuTopology.Gpus
	require.Equal(t, int64(2), gpus[0].Memory.Used.Bytes)
	require.Equal(t, int64(1), gpus[1].Memory.Used.Bytes)
	require.NotNil(t, gpus[2].Memory)
	require.Nil(t, gpus[2].Memory.Used)

	none := []*agentv1.Agent{{GpuTopology: &agentv1.GpuTopology{Gpus: []*agentv1.GpuInfo{{Uuid: "GPU-z"}}}}}
	require.Equal(t, codes.Unavailable, status.Code(applyGPUMemory(none, memory)))
	require.NoError(t, applyGPUMemory([]*agentv1.Agent{{Id: "cpu"}}, nil))
}
