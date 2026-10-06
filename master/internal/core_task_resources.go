package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/labstack/echo/v4"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/config"
	detcontext "github.com/determined-ai/determined/master/internal/context"
	"github.com/determined-ai/determined/master/internal/db"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/pkg/model"
)

const (
	taskResourceMaxRange  = 7 * 24 * time.Hour
	taskResourceMaxPoints = 1440
	taskResourceMinStep   = 15
	taskResourceTimeout   = 10 * time.Second
	taskResourceMaxBody   = 8 << 20
)

// A single master limits concurrent multi-query dashboard requests.
var taskResourceSlots = make(chan struct{}, 4)

var taskResourceHTTPClient = &http.Client{
	Timeout:       taskResourceTimeout,
	Transport:     &http.Transport{Proxy: nil},
	CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
}

type taskResourceLabels struct {
	AllocationID string `json:"allocation_id,omitempty"`
	Node         string `json:"node,omitempty"`
	GPUUUID      string `json:"gpu_uuid,omitempty"`
	// GPUIndex is the GPU's number in nvidia-smi inside the task's container. It is omitted
	// unless the container's complete GPU set and every bus ID in it are known.
	GPUIndex *int `json:"gpu_index,omitempty"`
	// DCGM's labels: the PCI bus ID, the host's NVML index (nvidia-smi on the node) and model.
	PCIBusID     string `json:"pci_bus_id,omitempty"`
	HostGPUIndex string `json:"host_gpu_index,omitempty"`
	ModelName    string `json:"model_name,omitempty"`
}

type taskResourceSeries struct {
	Metric  string             `json:"metric"`
	Labels  taskResourceLabels `json:"labels"`
	Samples [][2]interface{}   `json:"samples"`
}

type taskResourceWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type taskResourceResponse struct {
	Enabled  bool                  `json:"enabled"`
	Series   []taskResourceSeries  `json:"series"`
	Warnings []taskResourceWarning `json:"warnings"`
}

// taskResourceAllocation is one allocation of a task with the times its resources were held.
// ContainerStart is when the allocation got its resources: the end of its first QUEUED
// task_stats row (a restored allocation records another QUEUED row when the master restarts).
// Every allocation writes that row before it can start, so ContainerStart is null for one that
// never got resources. allocations.start_time is not used: on master start, CloseOpenAllocations
// sets it to the last cluster heartbeat for every allocation that is still queued. Image pulling
// comes after ContainerStart on purpose, since the devices are held while pulling. End is null
// while the allocation has not been released.
type taskResourceAllocation struct {
	AllocationID   string     `json:"allocation_id" bun:"allocation_id"`
	ContainerStart *time.Time `json:"container_start" bun:"container_start"`
	End            *time.Time `json:"end" bun:"end_time"`
}

type taskResourceAllocationsResponse struct {
	Allocations []taskResourceAllocation `json:"allocations"`
}

type taskResourceRange struct {
	Start int64
	End   int64
	Step  int
}

type taskResourceQuery struct {
	Metric string
	Expr   string
}

func (m *Master) getTaskResourcesCapability(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	return c.JSON(http.StatusOK, map[string]bool{
		"enabled": m.config.Integrations.TaskResources.Enabled(),
	})
}

func (m *Master) getTaskResources(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	conf := m.config.Integrations.TaskResources
	if !conf.Enabled() {
		return echo.NewHTTPError(http.StatusNotFound, "task resources are disabled")
	}
	return serveTaskResources(c, conf, m.taskResourceDependencies())
}

func (m *Master) getTaskResourceAllocations(c echo.Context) error {
	c.Response().Header().Set("Cache-Control", "no-store")
	if !m.config.Integrations.TaskResources.Enabled() {
		return echo.NewHTTPError(http.StatusNotFound, "task resources are disabled")
	}
	return serveTaskResourceAllocations(c, m.taskResourceDependencies())
}

func (m *Master) taskResourceDependencies() taskResourceDependencies {
	conf := m.config.Integrations.TaskResources
	return taskResourceDependencies{
		authorize: func(ctx context.Context, user model.User, taskID string) error {
			_, _, err := (&apiServer{m: m}).canDoActionsOnTaskForUser(
				ctx, model.TaskID(taskID), user,
				expauth.AuthZProvider.Get().CanGetExperimentArtifacts,
			)
			return err
		},
		allocationBelongs: func(ctx context.Context, taskID, allocationID string) (bool, error) {
			return db.Bun().NewSelect().Table("allocations").
				Where("task_id = ? AND allocation_id = ?", taskID, allocationID).Exists(ctx)
		},
		query: func(ctx context.Context, expr string, r taskResourceRange) ([]prometheusTaskSeries, error) {
			return queryTaskPrometheus(ctx, conf.PrometheusURL, expr, r)
		},
		allocations: queryTaskResourceAllocations,
		gpuSets:     queryTaskResourceGPUSets,
	}
}

type taskResourceDependencies struct {
	authorize         func(context.Context, model.User, string) error
	allocationBelongs func(context.Context, string, string) (bool, error)
	query             func(context.Context, string, taskResourceRange) ([]prometheusTaskSeries, error)
	allocations       func(context.Context, string) ([]taskResourceAllocation, error)
	// gpuSets reads the recorded GPU sets of a task's allocations; nil skips GPU numbering.
	gpuSets func(context.Context, string, []string) ([]model.AcceleratorData, error)
}

// queryTaskResourceAllocations reads a task's allocations and their container start in one query.
func queryTaskResourceAllocations(ctx context.Context, taskID string) ([]taskResourceAllocation, error) {
	allocations := []taskResourceAllocation{}
	err := db.Bun().NewRaw(`
SELECT a.allocation_id, q.queued_end AS container_start, a.end_time
FROM allocations a
LEFT JOIN (
	SELECT ts.allocation_id, min(ts.end_time) AS queued_end
	FROM task_stats ts
	JOIN allocations qa ON qa.allocation_id = ts.allocation_id
	WHERE qa.task_id = ? AND ts.event_type = 'QUEUED'
	GROUP BY ts.allocation_id
) q ON q.allocation_id = a.allocation_id
WHERE a.task_id = ?
ORDER BY container_start ASC NULLS LAST, a.allocation_id ASC`, taskID, taskID).Scan(ctx, &allocations)
	if err != nil {
		return nil, fmt.Errorf("reading task resource allocations: %w", err)
	}
	return allocations, nil
}

// serveTaskResourceAllocations lists a task's allocations for the resources view. Like the
// resources endpoint it authorizes the task before reading anything else.
func serveTaskResourceAllocations(c echo.Context, deps taskResourceDependencies) error {
	user := c.(*detcontext.DetContext).MustGetUser()
	ctx := c.Request().Context()
	taskID := c.Param("task_id")
	if err := authorizeTaskResources(ctx, user, taskID, deps); err != nil {
		return err
	}
	if len(c.QueryParams()) > 0 {
		return echo.NewHTTPError(http.StatusBadRequest, "unsupported or repeated query parameter")
	}
	allocations, err := deps.allocations(ctx, taskID)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, taskResourceAllocationsResponse{Allocations: allocations})
}

func serveTaskResources(c echo.Context, conf config.TaskResourcesConfig, deps taskResourceDependencies) error {
	user := c.(*detcontext.DetContext).MustGetUser()
	resp, err := collectTaskResources(c.Request().Context(), user, c.Param("task_id"),
		c.QueryParams(), conf, deps)
	if err != nil {
		return err
	}
	return c.JSON(http.StatusOK, resp)
}

// collectTaskResources is shared by the UI endpoint and the v1 API. It checks task
// authorization before reading any query parameter or consulting Prometheus.
func collectTaskResources(ctx context.Context, user model.User, taskID string, params url.Values,
	conf config.TaskResourcesConfig, deps taskResourceDependencies,
) (taskResourceResponse, error) {
	// Authorize before inspecting query parameters, allocation ownership, or Prometheus.
	if err := authorizeTaskResources(ctx, user, taskID, deps); err != nil {
		return taskResourceResponse{}, err
	}
	r, allocationID, err := parseTaskResourceRange(params, time.Now())
	if err != nil {
		return taskResourceResponse{}, echo.NewHTTPError(http.StatusBadRequest, err.Error())
	}
	if allocationID != "" {
		belongs, err := deps.allocationBelongs(ctx, taskID, allocationID)
		if err != nil {
			return taskResourceResponse{}, err
		}
		if !belongs {
			return taskResourceResponse{}, echo.NewHTTPError(http.StatusNotFound, "allocation not found")
		}
	}
	ctx, cancel := context.WithTimeout(ctx, taskResourceTimeout)
	defer cancel()
	select {
	case taskResourceSlots <- struct{}{}:
		defer func() { <-taskResourceSlots }()
	case <-ctx.Done():
		return taskResourceResponse{}, echo.NewHTTPError(http.StatusServiceUnavailable, "task resource query capacity exhausted")
	}

	resp := taskResourceResponse{Enabled: true, Series: []taskResourceSeries{},
		Warnings: []taskResourceWarning{}}
	gpus := map[string]taskResourceGPUInfo{}
	for _, q := range taskResourceQueries(conf.DetCluster, taskID, allocationID) {
		results, err := deps.query(ctx, q.Expr, r)
		if err != nil {
			// Never return a Prometheus response body or configured URL to browsers or logs.
			return taskResourceResponse{}, echo.NewHTTPError(http.StatusBadGateway, "task resource metrics are unavailable")
		}
		for _, result := range results {
			if result.Metric["task_id"] != taskID ||
				result.Metric["det_cluster"] != conf.DetCluster {
				return taskResourceResponse{}, echo.NewHTTPError(http.StatusBadGateway, "task resource metrics are invalid")
			}
			if allocationID != "" && result.Metric["allocation_id"] != allocationID {
				return taskResourceResponse{}, echo.NewHTTPError(http.StatusBadGateway, "task resource metrics are invalid")
			}
			series := taskResourceSeries{Metric: q.Metric, Labels: taskResourceLabels{
				AllocationID: result.Metric["allocation_id"], Node: result.Metric["node"],
				GPUUUID: result.Metric["gpu_uuid"],
			}, Samples: result.Samples}
			if strings.HasPrefix(q.Metric, "gpu_") {
				// The GPU queries keep DCGM's own labels from the left-hand side of the join.
				series.Labels.PCIBusID = result.Metric["pci_bus_id"]
				series.Labels.HostGPUIndex = result.Metric["gpu"]
				series.Labels.ModelName = result.Metric["modelName"]
				addTaskResourceGPUInfo(gpus, result.Metric)
			}
			resp.Series = append(resp.Series, series)
		}
	}
	setTaskResourceGPUIndexes(ctx, conf.DetCluster, taskID, r, resp.Series, gpus, deps)
	sort.SliceStable(resp.Series, func(i, j int) bool {
		return taskResourceSeriesLess(resp.Series[i], resp.Series[j])
	})
	resp.Warnings = taskResourceWarnings(resp.Series)
	return resp, nil
}

// taskResourceSeriesLess orders series by metric, allocation, node and then GPUs as numbered
// in the container, so that a legend lists GPU 0, GPU 1 and so on.
func taskResourceSeriesLess(a, b taskResourceSeries) bool {
	if a.Metric != b.Metric {
		return a.Metric < b.Metric
	}
	if a.Labels.AllocationID != b.Labels.AllocationID {
		return a.Labels.AllocationID < b.Labels.AllocationID
	}
	if a.Labels.Node != b.Labels.Node {
		return a.Labels.Node < b.Labels.Node
	}
	ai, bi := a.Labels.GPUIndex, b.Labels.GPUIndex
	if (ai == nil) != (bi == nil) {
		return ai != nil
	}
	if ai != nil && *ai != *bi {
		return *ai < *bi
	}
	return a.Labels.GPUUUID < b.Labels.GPUUUID
}

func authorizeTaskResources(ctx context.Context, user model.User, taskID string,
	deps taskResourceDependencies,
) error {
	if taskID == "" {
		return echo.NewHTTPError(http.StatusBadRequest, "task_id is required")
	}
	if err := deps.authorize(ctx, user, taskID); err != nil {
		if code := grpcTaskResourcesAuthCode(err); code != 0 {
			return echo.NewHTTPError(code, api.NotFoundErrMsg("task", taskID))
		}
		return err
	}
	return nil
}

func grpcTaskResourcesAuthCode(err error) int {
	// Both a missing task and a denied task are 404 to avoid revealing task existence.
	if status.Code(err) == codes.NotFound || status.Code(err) == codes.PermissionDenied ||
		authz.IsPermissionDenied(err) {
		return http.StatusNotFound
	}
	return 0
}

func parseTaskResourceRange(q url.Values, now time.Time) (taskResourceRange, string, error) {
	for key, values := range q {
		if key != "start" && key != "end" && key != "step" && key != "allocation_id" || len(values) != 1 {
			return taskResourceRange{}, "", fmt.Errorf("unsupported or repeated query parameter")
		}
	}
	parse := func(key string) (int64, error) {
		if q.Get(key) == "" {
			return 0, fmt.Errorf("%s is required", key)
		}
		v, err := strconv.ParseInt(q.Get(key), 10, 64)
		if err != nil {
			return 0, fmt.Errorf("%s must be an integer", key)
		}
		return v, nil
	}
	start, err := parse("start")
	if err != nil {
		return taskResourceRange{}, "", err
	}
	end, err := parse("end")
	if err != nil {
		return taskResourceRange{}, "", err
	}
	step64, err := parse("step")
	if err != nil {
		return taskResourceRange{}, "", err
	}
	if start < 0 || end <= start || end-start > int64(taskResourceMaxRange.Seconds()) ||
		end > now.Unix()+60 || step64 < taskResourceMinStep || step64 > 86400 ||
		(end-start)/step64+1 > taskResourceMaxPoints {
		return taskResourceRange{}, "", fmt.Errorf("time range or step exceeds task resource limits")
	}
	return taskResourceRange{Start: start, End: end, Step: int(step64)}, q.Get("allocation_id"), nil
}

func promLabel(s string) string { return strconv.Quote(s) }

func taskResourceQueries(cluster, taskID, allocationID string) []taskResourceQuery {
	clusterLabel := "det_cluster=" + promLabel(cluster)
	owner := clusterLabel + ",task_id=" + promLabel(taskID)
	if allocationID != "" {
		owner += ",allocation_id=" + promLabel(allocationID)
	}
	raw := clusterLabel
	runtime := "det:runtime_task:info{" + owner + "}"
	gpu := "det:gpu_task:info{" + owner + "}"
	mapRuntime := " * on (det_cluster,container_runtime_id) group_left (task_id,allocation_id) " + runtime
	mapGPU := " * on (det_cluster,gpu_uuid) group_left (task_id,allocation_id) " + gpu
	mem := func(name string) string {
		metric := name + `{job="cadvisor",` + raw + `,container_runtime_id!=""}`
		return "sum by (det_cluster,task_id,allocation_id,node) ((" + metric +
			" and on (det_cluster,container_runtime_id) (count by (det_cluster,container_runtime_id) (" +
			metric + ") == 1))" + mapRuntime + ")"
	}
	gpuMetric := func(name string) string {
		return name + `{job="dcgm",` + raw + `,gpu_uuid!=""}`
	}
	gpuValue := func(name string, maximum int) string {
		metric := gpuMetric(name)
		base := "((" + metric + " >= 0) and on (det_cluster,gpu_uuid) (" +
			metric + " <= " + strconv.Itoa(maximum) + "))"
		return "(" + base + " and on (det_cluster,gpu_uuid) (count by (det_cluster,gpu_uuid) (" +
			metric + ") == 1))" + mapGPU
	}
	cpu := `container_cpu_usage_seconds_total{job="cadvisor",` + raw + `,container_runtime_id!="",cpu=~"^(total)?$"}`
	return []taskResourceQuery{
		{"allocation_active", "det:allocation_task:info{" + owner + "}"},
		{"cpu_cores", "sum by (det_cluster,task_id,allocation_id,node) ((rate(" + cpu + "[1m]) and on (det_cluster,container_runtime_id) (count by (det_cluster,container_runtime_id) (" + cpu + ") == 1))" + mapRuntime + ")"},
		{"memory_working_set_bytes", mem("container_memory_working_set_bytes")},
		{"memory_rss_bytes", mem("container_memory_rss")},
		{"gpu_utilization_percent", gpuValue("DCGM_FI_DEV_GPU_UTIL", 100)},
		{"gpu_memory_used_bytes", gpuValue("DCGM_FI_DEV_FB_USED", 1000000) + " * 1024 * 1024"},
		{"gpu_power_watts", gpuValue("DCGM_FI_DEV_POWER_USAGE", 10000)},
		{"gpu_temperature_celsius", gpuValue("DCGM_FI_DEV_GPU_TEMP", 150)},
	}
}

type prometheusTaskSeries struct {
	Metric  map[string]string
	Samples [][2]interface{}
}

func queryTaskPrometheus(ctx context.Context, baseURL, expr string, r taskResourceRange) ([]prometheusTaskSeries, error) {
	u, err := url.Parse(baseURL)
	if err != nil {
		return nil, err
	}
	u.Path = "/api/v1/query_range"
	q := url.Values{"query": {expr}, "start": {strconv.FormatInt(r.Start, 10)},
		"end": {strconv.FormatInt(r.End, 10)}, "step": {strconv.Itoa(r.Step)}}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, err
	}
	resp, err := taskResourceHTTPClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("prometheus query failed")
	}
	var raw struct {
		Status string `json:"status"`
		Data   struct {
			ResultType string `json:"resultType"`
			Result     []struct {
				Metric map[string]string   `json:"metric"`
				Values [][]json.RawMessage `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, taskResourceMaxBody)).Decode(&raw); err != nil {
		return nil, err
	}
	if raw.Status != "success" || raw.Data.ResultType != "matrix" {
		return nil, fmt.Errorf("invalid prometheus matrix response")
	}
	series := make([]prometheusTaskSeries, 0, len(raw.Data.Result))
	for _, result := range raw.Data.Result {
		item := prometheusTaskSeries{Metric: result.Metric, Samples: make([][2]interface{}, 0, len(result.Values))}
		for _, sample := range result.Values {
			if len(sample) != 2 {
				return nil, fmt.Errorf("invalid prometheus sample")
			}
			var stamp float64
			var value string
			if err := json.Unmarshal(sample[0], &stamp); err != nil {
				return nil, err
			}
			if err := json.Unmarshal(sample[1], &value); err != nil {
				return nil, err
			}
			if math.IsNaN(stamp) || math.IsInf(stamp, 0) {
				return nil, fmt.Errorf("invalid prometheus timestamp")
			}
			v, err := strconv.ParseFloat(value, 64)
			var point interface{}
			if err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) {
				point = v
			}
			item.Samples = append(item.Samples, [2]interface{}{stamp, point})
		}
		series = append(series, item)
	}
	return series, nil
}

func taskResourceWarnings(series []taskResourceSeries) []taskResourceWarning {
	warnings := []taskResourceWarning{}
	rssSeen, rssNonzero, gpuSeen := false, false, false
	for _, s := range series {
		if s.Metric == "memory_rss_bytes" {
			for _, point := range s.Samples {
				if v, ok := point[1].(float64); ok {
					rssSeen = true
					if v != 0 {
						rssNonzero = true
					}
				}
			}
		}
		if strings.HasPrefix(s.Metric, "gpu_") {
			gpuSeen = true
		}
	}
	if rssSeen && !rssNonzero {
		warnings = append(warnings, taskResourceWarning{"rss_unverified", "RSS is reported as zero throughout this range; verify cAdvisor support before interpreting it as zero use."})
	}
	if gpuSeen {
		warnings = append(warnings, taskResourceWarning{"gpu_full_device", "GPU metrics describe the whole assigned device, which may include other processes."})
	}
	return warnings
}
