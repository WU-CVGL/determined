//go:build integration
// +build integration

package internal

import (
	"net/url"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// gpuMemoryMatrixForAgent has memory readings for GPU-0 and the excluded GPU-x, none for GPU-1.
const gpuMemoryMatrixForAgent = `{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"gpu_uuid":"GPU-0","det_gpu_reading":"memory_used"},"values":[[1794920240,"1024"]]},
{"metric":{"gpu_uuid":"GPU-0","det_gpu_reading":"memory_used_at"},"values":[[1794920240,"1794920230"]]},
{"metric":{"gpu_uuid":"GPU-x","det_gpu_reading":"memory_free"},"values":[[1794920240,"0"]]},
{"metric":{"gpu_uuid":"GPU-x","det_gpu_reading":"memory_free_at"},"values":[[1794920240,"1794920231"]]}
]}}`

// gpuReadingsMatrixForAgent adds GPU-1's power draw to gpuMemoryMatrixForAgent.
const gpuReadingsMatrixForAgent = `{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"gpu_uuid":"GPU-0","det_gpu_reading":"memory_used"},"values":[[1794920240,"1024"]]},
{"metric":{"gpu_uuid":"GPU-0","det_gpu_reading":"memory_used_at"},"values":[[1794920240,"1794920230"]]},
{"metric":{"gpu_uuid":"GPU-1","det_gpu_reading":"power_draw"},"values":[[1794920240,"97.25"]]},
{"metric":{"gpu_uuid":"GPU-1","det_gpu_reading":"power_draw_at"},"values":[[1794920240,"1794920232"]]}
]}}`

func memoryQueries(prom *fakeXIDPrometheus) []url.Values {
	var out []url.Values
	for _, q := range prom.got() {
		if strings.Contains(q.Get("query"), gpuReadingLabel) {
			out = append(out, q)
		}
	}
	return out
}

func TestGetAgentsGPUMemory(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)
	prom := newFakeXIDPrometheus(t, gpuMemoryMatrixForAgent)
	api.m.config.Integrations.TaskResources = config.TaskResourcesConfig{
		PrometheusURL: prom.URL, DetCluster: "lab-a",
	}

	// Without the flag nothing is read and no GPU has memory.
	plain, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{})
	require.NoError(t, err)
	require.Nil(t, plain.GpuMemoryQueriedAt)
	require.Nil(t, plain.Agents[0].GpuTopology.Gpus[0].Memory)
	require.Empty(t, memoryQueries(prom))

	all, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuMemory: true})
	require.NoError(t, err)
	require.NotNil(t, all.GpuMemoryQueriedAt)
	require.Len(t, memoryQueries(prom), 1)
	gpus := all.Agents[0].GpuTopology.Gpus
	require.Equal(t, int64(1024)<<20, gpus[0].Memory.Used.Bytes)
	require.Equal(t, int64(1794920230), gpus[0].Memory.Used.SampledAt.Seconds)
	require.Nil(t, gpus[0].Memory.Total)
	// A GPU without samples has memory with null readings.
	require.Equal(t, &agentv1.GpuMemory{}, gpus[1].Memory)
	require.Equal(t, int64(0), gpus[2].Memory.Free.Bytes)

	require.Nil(t, all.GpuPowerQueriedAt)
	require.Nil(t, gpus[0].Power)

	_, err = api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuMemory: true, ExcludeSlots: true})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	_, err = api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuPower: true, ExcludeSlots: true})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Len(t, memoryQueries(prom), 1)
}

func TestGetAgentsGPUMemoryAndPowerInOneQuery(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)
	prom := newFakeXIDPrometheus(t, gpuReadingsMatrixForAgent)
	api.m.config.Integrations.TaskResources = config.TaskResourcesConfig{
		PrometheusURL: prom.URL, DetCluster: "lab-a",
	}

	all, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{
		IncludeGpuMemory: true, IncludeGpuPower: true,
	})
	require.NoError(t, err)
	require.Len(t, memoryQueries(prom), 1)
	require.NotNil(t, all.GpuMemoryQueriedAt)
	require.NotNil(t, all.GpuPowerQueriedAt)
	gpus := all.Agents[0].GpuTopology.Gpus
	require.Equal(t, int64(1024)<<20, gpus[0].Memory.Used.Bytes)
	require.Equal(t, &agentv1.GpuPower{}, gpus[0].Power)
	require.Equal(t, 97.25, gpus[1].Power.Draw.Watts)
	require.Equal(t, int64(1794920232), gpus[1].Power.Draw.SampledAt.Seconds)
	require.Equal(t, &agentv1.GpuMemory{}, gpus[1].Memory)

	power, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuPower: true})
	require.Equal(t, codes.Internal, status.Code(err), "a memory series answers a power query")
	require.Nil(t, power)
}

func TestGetAgentsGPUMemoryNotConfigured(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)

	_, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuMemory: true})
	require.Equal(t, codes.FailedPrecondition, status.Code(err))
}

func TestGetAgentsGPUMemoryNeedsAgentDetails(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)
	prom := newFakeXIDPrometheus(t, gpuMemoryMatrixForAgent)
	api.m.config.Integrations.TaskResources = config.TaskResourcesConfig{
		PrometheusURL: prom.URL, DetCluster: "lab-a",
	}
	if !denySensitiveAgentInfoRegistered {
		cluster.AuthZProvider.RegisterOverride(denySensitiveAgentInfoAuthZ, &denySensitiveAgentInfo{})
		denySensitiveAgentInfoRegistered = true
	}
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{
		Type: denySensitiveAgentInfoAuthZ, FallbackType: ptrs.Ptr(config.BasicAuthZType),
	}
	t.Cleanup(func() {
		config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: config.BasicAuthZType}
	})

	_, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuMemory: true})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuPower: true})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Empty(t, prom.got())
}
