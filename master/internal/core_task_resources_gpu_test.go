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

const taskResourceGPULookupPrefix = `group by (det_cluster,gpu_uuid,pci_bus_id,node) ` +
	`(DCGM_FI_DEV_GPU_UTIL{job="dcgm",`

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

func gpuSet(allocationID string, uuids ...string) model.AcceleratorData {
	return model.AcceleratorData{AllocationID: model.AllocationID(allocationID), AcceleratorUuids: uuids}
}

func gpuInfos(node string, busIDs map[string]string) map[string]taskResourceGPUInfo {
	gpus := map[string]taskResourceGPUInfo{}
	for uuid, busID := range busIDs {
		gpus[uuid] = taskResourceGPUInfo{busID: busID, node: node}
	}
	return gpus
}

func mergeGPUInfos(parts ...map[string]taskResourceGPUInfo) map[string]taskResourceGPUInfo {
	gpus := map[string]taskResourceGPUInfo{}
	for _, part := range parts {
		for uuid, info := range part {
			gpus[uuid] = info
		}
	}
	return gpus
}

func gpuIndexes(indexes map[taskResourceGPUKey]int) map[string]int {
	out := map[string]int{}
	for key, index := range indexes {
		out[key.allocationID+"/"+key.gpuUUID] = index
	}
	return out
}

func TestPCIBusIDKeyNormalisesCaseAndDomain(t *testing.T) {
	for _, tc := range []struct{ a, b string }{
		{"00000000:01:00.0", "0000:01:00.0"},
		{"00000000:0A:00.0", "0000:0a:00.0"},
		{" 00000000:81:00.0 ", "00000000:81:00.0"},
	} {
		a, okA := pciBusIDKey(tc.a)
		b, okB := pciBusIDKey(tc.b)
		require.True(t, okA && okB, "%q %q", tc.a, tc.b)
		require.Equal(t, a, b)
	}
	ordered := []string{
		"0000:09:00.0", "00000000:0A:00.0", "0000:0b:00.0", "00000000:81:00.0",
		"0000:FF:1f.7", "00000001:01:00.0",
	}
	for i := 1; i < len(ordered); i++ {
		prev, _ := pciBusIDKey(ordered[i-1])
		next, _ := pciBusIDKey(ordered[i])
		require.Less(t, prev, next, "%s < %s", ordered[i-1], ordered[i])
	}
	for _, bad := range []string{"", "01:00.0", "GPU-1", "00000000:01:00.8", "000000000:01:00.0"} {
		_, ok := pciBusIDKey(bad)
		require.False(t, ok, bad)
	}
}

func TestTaskResourceGPUIndexes(t *testing.T) {
	nodeA := gpuInfos("node-a", map[string]string{
		"GPU-a": "00000000:25:00.0", "GPU-b": "00000000:01:00.0", "GPU-c": "00000000:81:00.0",
		"GPU-d": "00000000:41:00.0",
	})
	nodeB := gpuInfos("node-b", map[string]string{
		"GPU-e": "00000000:C1:00.0", "GPU-f": "00000000:01:00.0",
	})
	for _, tc := range []struct {
		name   string
		series []taskResourceSeries
		sets   []model.AcceleratorData
		gpus   map[string]taskResourceGPUInfo
		want   map[string]int
	}{
		{
			name:   "ranked by PCI bus ID, not by UUID, when the recorded order agrees",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b", "GPU-c"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a", "GPU-c")},
			gpus:   nodeA,
			want:   map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1, "t.1/GPU-c": 2},
		},
		{
			// The container recorded GPU-c as its GPU 0, but GPU-c has the highest bus ID.
			name:   "a recorded order that disagrees with the bus-ID order leaves the node unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b", "GPU-c"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-c", "GPU-a", "GPU-b")},
			gpus:   nodeA,
			want:   map[string]int{},
		},
		{
			name:   "case and domain width do not change the order",
			series: gpuSeries("t.1", "node-a", "GPU-x", "GPU-y", "GPU-z"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-z", "GPU-y", "GPU-x")},
			gpus: gpuInfos("node-a", map[string]string{
				"GPU-x": "00000000:0B:00.0", "GPU-y": "0000:0a:00.0", "GPU-z": "00000000:09:00.0",
			}),
			want: map[string]int{"t.1/GPU-z": 0, "t.1/GPU-y": 1, "t.1/GPU-x": 2},
		},
		{
			name: "each node of an allocation is numbered from zero",
			series: append(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.1", "node-b", "GPU-e", "GPU-f")...),
			sets: []model.AcceleratorData{
				gpuSet("t.1", "GPU-b", "GPU-a"), gpuSet("t.1", "GPU-f", "GPU-e"),
			},
			gpus: mergeGPUInfos(nodeA, nodeB),
			want: map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1, "t.1/GPU-f": 0, "t.1/GPU-e": 1},
		},
		{
			name: "two allocations on the same GPU are numbered separately",
			series: append(gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
				gpuSeries("t.2", "node-a", "GPU-a", "GPU-c")...),
			sets: []model.AcceleratorData{
				gpuSet("t.1", "GPU-b", "GPU-a"), gpuSet("t.2", "GPU-a", "GPU-c"),
			},
			gpus: nodeA,
			want: map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1, "t.2/GPU-a": 0, "t.2/GPU-c": 1},
		},
		{
			name:   "a GPU of the set without a bus ID leaves the node unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a")},
			gpus: mergeGPUInfos(nodeA, gpuInfos("node-a", map[string]string{
				"GPU-b": "",
			})),
			want: map[string]int{},
		},
		{
			name:   "a GPU of the set without any DCGM labels leaves the node unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a", "GPU-unseen")},
			gpus:   nodeA,
			want:   map[string]int{},
		},
		{
			name:   "GPUs of the set without a series still count",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-c"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a", "GPU-d", "GPU-c")},
			gpus:   nodeA,
			want:   map[string]int{"t.1/GPU-a": 1, "t.1/GPU-c": 3},
		},
		{
			name:   "no recorded set leaves the GPUs unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   nil,
			gpus:   nodeA,
			want:   map[string]int{},
		},
		{
			name:   "a series GPU missing from the recorded set leaves the node unnumbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b", "GPU-c"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a")},
			gpus:   nodeA,
			want:   map[string]int{},
		},
		{
			name:   "another allocation's set does not number this allocation",
			series: gpuSeries("t.2", "node-a", "GPU-a", "GPU-b"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a")},
			gpus:   nodeA,
			want:   map[string]int{},
		},
		{
			name:   "a GPU in two different sets of one allocation is ambiguous",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets: []model.AcceleratorData{
				gpuSet("t.1", "GPU-b", "GPU-a"), gpuSet("t.1", "GPU-a", "GPU-c"),
			},
			gpus: nodeA,
			want: map[string]int{},
		},
		{
			name:   "repeated identical rows of one container are one set",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets: []model.AcceleratorData{
				gpuSet("t.1", "GPU-b", "GPU-a"), gpuSet("t.1", "GPU-b", "GPU-a"),
			},
			gpus: nodeA,
			want: map[string]int{"t.1/GPU-b": 0, "t.1/GPU-a": 1},
		},
		{
			name:   "rows of one allocation with the same GPUs in different orders contradict each other",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets: []model.AcceleratorData{
				gpuSet("t.1", "GPU-b", "GPU-a"), gpuSet("t.1", "GPU-a", "GPU-b"),
			},
			gpus: nodeA,
			want: map[string]int{},
		},
		{
			name:   "a row that lists a GPU twice is not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a", "GPU-a")},
			gpus:   nodeA,
			want:   map[string]int{},
		},
		{
			name:   "a set spanning two nodes is not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-a", "GPU-e")},
			gpus:   mergeGPUInfos(nodeA, nodeB),
			want:   map[string]int{},
		},
		{
			name:   "two GPUs of one set with the same bus ID are not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-a", "GPU-b")},
			gpus: gpuInfos("node-a", map[string]string{
				"GPU-a": "00000000:01:00.0", "GPU-b": "0000:01:00.0",
			}),
			want: map[string]int{},
		},
		{
			name:   "a GPU reported with two bus IDs is not numbered",
			series: gpuSeries("t.1", "node-a", "GPU-a", "GPU-b"),
			sets:   []model.AcceleratorData{gpuSet("t.1", "GPU-b", "GPU-a")},
			gpus: mergeGPUInfos(nodeA, map[string]taskResourceGPUInfo{
				"GPU-a": {busID: "00000000:25:00.0", node: "node-a", conflict: true},
			}),
			want: map[string]int{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, gpuIndexes(taskResourceGPUIndexes(tc.series, tc.sets, tc.gpus)))
		})
	}
}

func TestAddTaskResourceGPUInfoMarksConflicts(t *testing.T) {
	gpus := map[string]taskResourceGPUInfo{}
	addTaskResourceGPUInfo(gpus, map[string]string{"gpu_uuid": "GPU-a", "pci_bus_id": "00000000:01:00.0", "node": "n"})
	addTaskResourceGPUInfo(gpus, map[string]string{"gpu_uuid": "GPU-a", "pci_bus_id": "0000:01:00.0", "node": "n"})
	require.False(t, gpus["GPU-a"].conflict)
	addTaskResourceGPUInfo(gpus, map[string]string{"gpu_uuid": "GPU-a", "pci_bus_id": "00000000:02:00.0", "node": "n"})
	require.True(t, gpus["GPU-a"].conflict)
	addTaskResourceGPUInfo(gpus, map[string]string{"gpu_uuid": "GPU-a", "pci_bus_id": "00000000:01:00.0", "node": "n"})
	require.True(t, gpus["GPU-a"].conflict, "a conflict is never forgotten")
	addTaskResourceGPUInfo(gpus, map[string]string{"gpu_uuid": "GPU-b", "pci_bus_id": "00000000:01:00.0", "node": "n"})
	addTaskResourceGPUInfo(gpus, map[string]string{"gpu_uuid": "GPU-b", "pci_bus_id": "00000000:01:00.0", "node": "m"})
	require.True(t, gpus["GPU-b"].conflict)
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
	var lookups []string
	var setRequests [][]string
	deps := func(lookupErr error) taskResourceDependencies {
		return taskResourceDependencies{
			authorize:         func(context.Context, model.User, string) error { return nil },
			allocationBelongs: func(context.Context, string, string) (bool, error) { return true, nil },
			query: func(_ context.Context, expr string, _ taskResourceRange) ([]prometheusTaskSeries, error) {
				if strings.HasPrefix(expr, "group by (det_cluster,gpu_uuid,pci_bus_id,node)") {
					lookups = append(lookups, expr)
					if lookupErr != nil {
						return nil, lookupErr
					}
					// GPU-c has no series in the range; another cluster's answer is ignored.
					hidden := dcgmResult(now, "GPU-c", "00000000:01:00.0", "0")
					other := dcgmResult(now, "GPU-c", "00000000:F1:00.0", "0")
					other.Metric["det_cluster"] = "other"
					return []prometheusTaskSeries{hidden, other}, nil
				}
				if !strings.Contains(expr, "DCGM_FI_DEV_GPU_UTIL") {
					return nil, nil
				}
				return []prometheusTaskSeries{
					dcgmResult(now, "GPU-a", "00000000:81:00.0", "6"),
					dcgmResult(now, "GPU-b", "00000000:25:00.0", "2"),
				}, nil
			},
			gpuSets: func(_ context.Context, taskID string, allocationIDs []string) ([]model.AcceleratorData, error) {
				require.Equal(t, "task.1", taskID)
				setRequests = append(setRequests, allocationIDs)
				return []model.AcceleratorData{gpuSet("task.1.1", "GPU-c", "GPU-b", "GPU-a")}, nil
			},
		}
	}

	c, rec := taskResourceTestContext(t, fmt.Sprintf("start=%d&end=%d&step=15", now-60, now))
	require.NoError(t, serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"}, deps(nil)))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Equal(t, [][]string{{"task.1.1"}}, setRequests)
	require.Len(t, lookups, 1)
	require.Equal(t, taskResourceGPULookupPrefix+`det_cluster="cvgl",gpu_uuid=~"GPU-c"})`, lookups[0])
	var response struct {
		Series []struct {
			Metric string                     `json:"metric"`
			Labels map[string]json.RawMessage `json:"labels"`
		} `json:"series"`
	}
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &response))
	require.Len(t, response.Series, 2)
	first, second := response.Series[0].Labels, response.Series[1].Labels
	require.Equal(t, "gpu_utilization_percent", response.Series[0].Metric)
	// The series are in container order, not UUID order.
	require.JSONEq(t, `{"allocation_id":"task.1.1","node":"cvgl-node02.lan","gpu_uuid":"GPU-b",
		"gpu_index":1,"pci_bus_id":"00000000:25:00.0","host_gpu_index":"2",
		"model_name":"NVIDIA GeForce RTX 4090"}`, mustJSON(t, first))
	require.Equal(t, json.RawMessage("2"), second["gpu_index"])
	require.Equal(t, json.RawMessage(`"GPU-a"`), second["gpu_uuid"])

	// A failed lookup leaves the GPUs unnumbered but keeps the response.
	lookups, setRequests = nil, nil
	c, rec = taskResourceTestContext(t, fmt.Sprintf("start=%d&end=%d&step=15", now-60, now))
	require.NoError(t, serveTaskResources(c, config.TaskResourcesConfig{DetCluster: "cvgl"},
		deps(fmt.Errorf("prometheus down"))))
	require.Equal(t, http.StatusOK, rec.Code)
	require.Len(t, lookups, 1)
	require.NotContains(t, rec.Body.String(), `"gpu_index"`)
	require.Contains(t, rec.Body.String(), `"pci_bus_id":"00000000:25:00.0"`)
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
		gpuSets: func(context.Context, string, []string) ([]model.AcceleratorData, error) {
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
		gpuSets: func(context.Context, string, []string) ([]model.AcceleratorData, error) {
			return nil, fmt.Errorf("database is down")
		},
	})
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, rec.Code)
	require.Contains(t, rec.Body.String(), `"gpu_uuid":"GPU-25"`)
	require.NotContains(t, rec.Body.String(), `"gpu_index"`)
	require.NotContains(t, rec.Body.String(), "database is down")
}

func TestTaskResourceGPUInfoQueryQuotesUUIDs(t *testing.T) {
	expr := taskResourceGPUInfoQuery(`cv"gl`, []string{"GPU-1", "GPU-2"})
	require.Equal(t, taskResourceGPULookupPrefix+`det_cluster="cv\"gl",gpu_uuid=~"GPU-1|GPU-2"})`, expr)
	require.False(t, taskResourceGPUUUIDPattern.MatchString(`GPU-1"}or up{x="`))
	require.False(t, taskResourceGPUUUIDPattern.MatchString("GPU-1|.*"))
	zero := 0
	require.Contains(t, mustJSON(t, taskResourceLabels{GPUUUID: "GPU-a", GPUIndex: &zero}), `"gpu_index":0`)
	require.NotContains(t, mustJSON(t, taskResourceLabels{GPUUUID: "GPU-a"}), `"gpu_index"`)
}
