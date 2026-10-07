package internal

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/gpuhealth"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
)

// fakeXIDPrometheus answers range queries with one XID series per GPU and code, and records the
// queries it got.
type fakeXIDPrometheus struct {
	*httptest.Server
	mu      sync.Mutex
	queries []url.Values
	status  int
	body    string
}

// got returns the queries so far.
func (f *fakeXIDPrometheus) got() []url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]url.Values(nil), f.queries...)
}

func (f *fakeXIDPrometheus) respond(status int, body string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.status, f.body = status, body
}

func newFakeXIDPrometheus(t *testing.T, body string) *fakeXIDPrometheus {
	f := &fakeXIDPrometheus{status: http.StatusOK, body: body}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		if r.URL.Path != "/api/v1/query_range" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		f.queries = append(f.queries, r.URL.Query())
		w.WriteHeader(f.status)
		_, _ = w.Write([]byte(f.body))
	}))
	t.Cleanup(f.Close)
	return f
}

// xidMatrix is a range query result: GPU-1 has XID 79 at two steps and XID 13, which the query
// leaves out, so it must never count; GPU-x (an excluded GPU) has XID 94 at one step.
const xidMatrix = `{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"gpu_uuid":"GPU-1","xid":"79"},"values":[[1791367800,"3"],[1791368700,"1"]]},
{"metric":{"gpu_uuid":"GPU-0","xid":"13"},"values":[[1791367800,"2"]]},
{"metric":{"gpu_uuid":"GPU-x","xid":"94"},"values":[[1791368100,"1"]]}]}}`

func TestGPUXIDQueryOverHTTP(t *testing.T) {
	prom := newFakeXIDPrometheus(t, xidMatrix)
	query := gpuXIDQuery(config.TaskResourcesConfig{PrometheusURL: prom.URL, DetCluster: "lab-a"})
	require.NotNil(t, query)

	now := time.Date(2026, 10, 7, 12, 27, 13, 0, time.UTC)
	start, end := gpuhealth.XIDRange(now)
	series, err := query(context.Background(), gpuhealth.XIDQuery("lab-a"), start, end, gpuhealth.XIDStep)
	require.NoError(t, err)
	require.Len(t, prom.got(), 1)
	q := prom.got()[0]
	require.Equal(t, gpuhealth.XIDQuery("lab-a"), q.Get("query"))
	require.Equal(t, "300", q.Get("step"))
	require.Equal(t, "1791376200", q.Get("end"), "12:30:00 UTC, the step at or after now")
	require.Equal(t, "1791289800", q.Get("start"), "24 hours before the end")

	require.Len(t, series, 3)
	require.Equal(t, map[string]string{"gpu_uuid": "GPU-1", "xid": "79"}, series[0].Labels)
	require.Equal(t, []gpuhealth.Sample{
		{Time: time.Unix(1791367800, 0), Value: 3}, {Time: time.Unix(1791368700, 0), Value: 1},
	}, series[0].Samples)

	// Through the cache: XID 13 never counts.
	cache := gpuhealth.NewXIDCache("lab-a", query)
	xids := cache.Get(context.Background())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, xids.Status)
	require.Equal(t, map[string][]gpuhealth.XID{
		"GPU-1": {{Code: 79, FirstObserved: time.Unix(1791367800, 0), LastObserved: time.Unix(1791368700, 0)}},
		"GPU-x": {{Code: 94, FirstObserved: time.Unix(1791368100, 0), LastObserved: time.Unix(1791368100, 0)}},
	}, xids.ByUUID)

	// A failed request never shows the URL or the response.
	prom.respond(http.StatusServiceUnavailable, "secret upstream detail")
	_, err = query(context.Background(), gpuhealth.XIDQuery("lab-a"), start, end, gpuhealth.XIDStep)
	require.Error(t, err)

	require.Nil(t, gpuXIDQuery(config.TaskResourcesConfig{}))
}

// The master builds one cache, from integrations.task_resources.
func TestMasterGPUXIDs(t *testing.T) {
	m := &Master{config: &config.Config{}}
	cache := m.gpuXIDs()
	require.Same(t, cache, m.gpuXIDs())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED,
		m.gpuXIDs().Get(context.Background()).Status)

	prom := newFakeXIDPrometheus(t, xidMatrix)
	m = &Master{config: &config.Config{}}
	m.config.Integrations.TaskResources = config.TaskResourcesConfig{PrometheusURL: prom.URL, DetCluster: "lab-a"}
	xids := m.gpuXIDs().Get(context.Background())
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, xids.Status)
	require.Len(t, prom.got(), 1)
	require.Contains(t, prom.got()[0].Get("query"), `det_cluster="lab-a"`)
}
