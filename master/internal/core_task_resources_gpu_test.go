package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/pkg/model"
)

func gpuSeries(allocationID, node string, uuids ...string) []taskResourceSeries {
	series := make([]taskResourceSeries, 0, len(uuids))
	for _, uuid := range uuids {
		series = append(series, taskResourceSeries{
			Metric: "gpu_utilization_percent",
			Labels: taskResourceLabels{AllocationID: allocationID, Node: node, GPUUUID: uuid},
		})
	}
	return series
}

func concatSeries(parts ...[]taskResourceSeries) []taskResourceSeries {
	series := []taskResourceSeries{}
	for _, part := range parts {
		series = append(series, part...)
	}
	return series
}

// gpuSet is one container's recorded GPU list; slots are its allocation's.
func gpuSet(allocationID, containerID string, slots int, uuids ...string) taskResourceGPUSet {
	return taskResourceGPUSet{
		AllocationID: allocationID, ContainerID: containerID, Slots: slots, UUIDs: uuids,
	}
}

func gpuIndexes(indexes map[taskResourceGPUKey]int) map[string]int {
	out := map[string]int{}
	for key, index := range indexes {
		out[key.allocationID+"/"+key.gpuUUID] = index
	}
	return out
}

func TestTaskResourceGPUIndexes(t *testing.T) {
	for _, tc := range []struct {
		name   string
		series []taskResourceSeries
		sets   []taskResourceGPUSet
		want   map[string]int
	}{
		{
			name:   "numbered by the recorded order, not by UUID",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b", "GPU-c"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 3, "GPU-b", "GPU-a", "GPU-c")},
			want:   map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1, "t.1/GPU-c": 2},
		},
		{
			// nvidia-smi showed GPU-a, GPU-b and GPU-c, but the line of GPU-b was skipped.
			name:   "a partial list is not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-c"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 3, "GPU-a", "GPU-c")},
			want:   map[string]int{},
		},
		{
			name:   "a list with more GPUs than the slots is not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 1, "GPU-a", "GPU-b")},
			want:   map[string]int{},
		},
		{
			name: "each node of a multi-node allocation is numbered from zero",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-e", "GPU-f")),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a"), gpuSet("t.1", "c2", 4, "GPU-f", "GPU-e"),
			},
			want: map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1, "t.1/GPU-f": 0, "t.1/GPU-e": 1},
		},
		{
			name: "one partial list leaves every node of the allocation unnumbered",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-f")),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a"), gpuSet("t.1", "c2", 4, "GPU-f"),
			},
			want: map[string]int{},
		},
		{
			name: "a multi-node allocation is not numbered before every container recorded its list",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-e", "GPU-f")),
			sets: []taskResourceGPUSet{gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a")},
			want: map[string]int{},
		},
		{
			name: "two allocations on the same GPU are numbered separately",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.2", "node-a", "GPU-a", "GPU-c")),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a"), gpuSet("t.2", "c2", 2, "GPU-a", "GPU-c"),
			},
			want: map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1, "t.2/GPU-a": 0, "t.2/GPU-c": 1},
		},
		{
			name:   "GPUs of the list without a series still count",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-c"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a", "GPU-d", "GPU-c")},
			want:   map[string]int{"t.1/GPU-a": 1, "t.1/GPU-c": 3},
		},
		{
			name:   "no recorded list leaves the GPUs unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			want:   map[string]int{},
		},
		{
			name:   "a series GPU missing from the recorded list leaves the node unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b", "GPU-c"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a")},
			want:   map[string]int{},
		},
		{
			name:   "another allocation's list does not number this allocation",
			series: gpuSeries("t.2", "node-a", "GPU-a", "GPU-b"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a")},
			want:   map[string]int{},
		},
		{
			name:   "a GPU in the lists of two containers of one allocation is ambiguous",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a"), gpuSet("t.1", "c2", 4, "GPU-a", "GPU-c"),
			},
			want: map[string]int{},
		},
		{
			// GPU-a counts twice, so the lists add up to the slots with only three GPUs.
			name:   "a GPU in the lists of two containers leaves the other GPUs unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-b"),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a"), gpuSet("t.1", "c2", 4, "GPU-a", "GPU-c"),
			},
			want: map[string]int{},
		},
		{
			name:   "series of one node from the lists of two containers are not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b", "GPU-c", "GPU-d"),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-a", "GPU-b"), gpuSet("t.1", "c2", 4, "GPU-c", "GPU-d"),
			},
			want: map[string]int{},
		},
		{
			name:   "repeated identical rows of one container are one list",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a"), gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a"),
			},
			want: map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1},
		},
		{
			name:   "rows of one container with the same GPUs in different orders contradict each other",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a"), gpuSet("t.1", "c1", 2, "GPU-a", "GPU-b"),
			},
			want: map[string]int{},
		},
		{
			name:   "a list that names a GPU twice is not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   []taskResourceGPUSet{gpuSet("t.1", "c1", 3, "GPU-b", "GPU-a", "GPU-a")},
			want:   map[string]int{},
		},
		{
			// The count matches, but one of the lists it sums is not trustworthy.
			name: "a broken list on one node leaves every node of the allocation unnumbered",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-e")),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a"), gpuSet("t.1", "c2", 4, "GPU-e", "GPU-e"),
			},
			want: map[string]int{},
		},
		{
			name: "a container with two different rows leaves every node of the allocation unnumbered",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-e", "GPU-f")),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 4, "GPU-b", "GPU-a"), gpuSet("t.1", "c2", 4, "GPU-e", "GPU-f"),
				gpuSet("t.1", "c2", 4, "GPU-f", "GPU-e"),
			},
			want: map[string]int{},
		},
		{
			// GPU-e is reported on node-b by another allocation.
			name: "a list spanning two nodes is not numbered",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a"),
				gpuSeries("t.2", "node-b", "GPU-e")),
			sets: []taskResourceGPUSet{
				gpuSet("t.1", "c1", 2, "GPU-a", "GPU-e"), gpuSet("t.2", "c2", 1, "GPU-e"),
			},
			want: map[string]int{"t.2/GPU-e": 0},
		},
		{
			name: "a GPU reported on two nodes is not numbered",
			series: concatSeries(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-a")),
			sets: []taskResourceGPUSet{gpuSet("t.1", "c1", 2, "GPU-b", "GPU-a")},
			want: map[string]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, gpuIndexes(taskResourceGPUIndexes(tc.series, tc.sets)))
		})
	}
}

// dcgmResult is a GPU query result with the DCGM labels the join keeps.
func dcgmResult(now int64, uuid, busID, hostIndex string) prometheusTaskSeries {
	return prometheusTaskSeries{Metric: map[string]string{
		"det_cluster": "cvgl", "task_id": "task.1", "allocation_id": "task.1.1",
		"node": "cvgl-node02.lan", "gpu_uuid": uuid, "UUID": uuid, "pci_bus_id": busID,
		"gpu": hostIndex, "modelName": "NVIDIA GeForce RTX 4090", "device": "nvidia" + hostIndex,
	}, Samples: [][2]interface{}{{float64(now), float64(50)}}}
}

func TestTaskResourcesGPULabelsAndIndexes(t *testing.T) {
	now := time.Now().Unix()
	var queries []string
	var setRequests [][]string
	deps := func(sets ...taskResourceGPUSet) taskResourceDependencies {
		return taskResourceDependencies{
			authorize:         func(context.Context, model.User, string) error { return nil },
			allocationBelongs: func(context.Context, string, string) (bool, error) { return true, nil },
			query: func(_ context.Context, expr string, _ taskResourceRange) ([]prometheusTaskSeries, error) {
				queries = append(queries, expr)
				if !strings.Contains(expr, "DCGM_FI_DEV_GPU_UTIL") {
					return nil, nil
				}
				return []prometheusTaskSeries{
					dcgmResult(now, "GPU-a", "00000000:81:00.0", "6"),
					dcgmResult(now, "GPU-b", "00000000:25:00.0", "2"),
				}, nil
			},
			gpuSets: func(_ context.Context, taskID string, allocationIDs []string) ([]taskResourceGPUSet, error) {
				require.Equal(t, "task.1", taskID)
				setRequests = append(setRequests, allocationIDs)
				return sets, nil
			},
		}
	}
	type labels map[string]json.RawMessage
	serve := func(deps taskResourceDependencies) []labels {
		queries, setRequests = nil, nil
		c, rec := taskResourceTestContext(t, fmt.Sprintf("start=%d&end=%d&step=15", now-60, now))
		require.NoError(t, serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"}, deps))
		require.Equal(t, http.StatusOK, rec.Code)
		// Only the fixed queries: the GPU number needs no other Prometheus query.
		require.Len(t, queries, len(taskResourceQueries("cvgl", "task.1", "")))
		require.Equal(t, [][]string{{"task.1.1"}}, setRequests)
		var response struct {
			Series []struct {
				Metric string `json:"metric"`
				Labels labels `json:"labels"`
			} `json:"series"`
		}
		require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
		require.Len(t, response.Series, 2)
		require.Equal(t, "gpu_utilization_percent", response.Series[0].Metric)
		return []labels{response.Series[0].Labels, response.Series[1].Labels}
	}

	// GPU-c has no series in the range but still takes position 0.
	got := serve(deps(gpuSet("task.1.1", "c1", 3, "GPU-c", "GPU-b", "GPU-a")))
	// The series are in container order, not UUID order.
	require.JSONEq(t, `{"allocation_id":"task.1.1","node":"cvgl-node02.lan","gpu_uuid":"GPU-b",
		"gpu_index":1,"pci_bus_id":"00000000:25:00.0","host_gpu_index":"2",
		"model_name":"NVIDIA GeForce RTX 4090"}`, mustJSON(t, got[0]))
	require.Equal(t, json.RawMessage("2"), got[1]["gpu_index"])
	require.Equal(t, json.RawMessage(`"GPU-a"`), got[1]["gpu_uuid"])

	// A list shorter than the allocation's slots leaves the GPUs unnumbered; the DCGM labels stay.
	got = serve(deps(gpuSet("task.1.1", "c1", 3, "GPU-b", "GPU-a")))
	for _, l := range got {
		require.NotContains(t, l, "gpu_index")
		require.Contains(t, l, "pci_bus_id")
		require.Contains(t, l, "host_gpu_index")
		require.Contains(t, l, "model_name")
	}
}

func mustJSON(t *testing.T, v interface{}) string {
	t.Helper()
	out, err := json.Marshal(v)
	require.NoError(t, err)
	return string(out)
}

func TestTaskResourcesGPUSetsFailureOrDenial(t *testing.T) {
	now := time.Now().Unix()
	query := func(_ context.Context, expr string, _ taskResourceRange) ([]prometheusTaskSeries, error) {
		if strings.Contains(expr, "det:gpu_task:info") {
			return []prometheusTaskSeries{dcgmResult(now, "GPU-25", "00000000:25:00.0", "2")}, nil
		}
		return nil, nil
	}

	// A denied request reads no GPU set.
	read := false
	c, _ := taskResourceTestContext(t, fmt.Sprintf("start=%d&end=%d&step=15", now-60, now))
	err := serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"}, taskResourceDependencies{
		authorize: func(context.Context, model.User, string) error {
			return status.Error(codes.PermissionDenied, "denied")
		},
		query: query,
		gpuSets: func(context.Context, string, []string) ([]taskResourceGPUSet, error) {
			read = true
			return nil, nil
		},
	})
	require.Error(t, err)
	require.False(t, read)

	// An unreadable GPU set keeps the response and the UUID labels.
	c, rec := taskResourceTestContext(t, fmt.Sprintf("start=%d&end=%d&step=15", now-60, now))
	err = serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"}, taskResourceDependencies{
		authorize:         func(context.Context, model.User, string) error { return nil },
		allocationBelongs: func(context.Context, string, string) (bool, error) { return true, nil },
		query:             query,
		gpuSets: func(context.Context, string, []string) ([]taskResourceGPUSet, error) {
			return nil, fmt.Errorf("database is down")
		},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"gpu_uuid":"GPU-25"`)
	require.NotContains(t, rec.Body.String(), `"gpu_index"`)
	require.NotContains(t, rec.Body.String(), "database is down")
}

func TestTaskResourceLabelsOmitUnknownGPUIndex(t *testing.T) {
	zero := 0
	require.Contains(t, mustJSON(t, taskResourceLabels{GPUUUID: "GPU-a", GPUIndex: &zero}), `"gpu_index":0`)
	require.NotContains(t, mustJSON(t, taskResourceLabels{GPUUUID: "GPU-a"}), `"gpu_index"`)
}
