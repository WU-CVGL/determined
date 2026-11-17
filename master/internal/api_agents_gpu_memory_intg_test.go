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

// gpuMemoryMatrixForAgent has readings for GPU-0 and the excluded GPU-x, none for GPU-1.
const gpuMemoryMatrixForAgent = `{"status":"success","data":{"resultType":"matrix","result":[
{"metric":{"gpu_uuid":"GPU-0","det_gpu_memory_field":"used"},"values":[[1794920240,"1024"]]},
{"metric":{"gpu_uuid":"GPU-0","det_gpu_memory_field":"used_at"},"values":[[1794920240,"1794920230"]]},
{"metric":{"gpu_uuid":"GPU-x","det_gpu_memory_field":"free"},"values":[[1794920240,"0"]]},
{"metric":{"gpu_uuid":"GPU-x","det_gpu_memory_field":"free_at"},"values":[[1794920240,"1794920231"]]}
]}}`

func memoryQueries(prom *fakeXIDPrometheus) []url.Values {
	var out []url.Values
	for _, q := range prom.got() {
		if strings.Contains(q.Get("query"), "DCGM_FI_DEV_FB_") {
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

	_, err = api.GetAgents(ctx, &apiv1.GetAgentsRequest{IncludeGpuMemory: true, ExcludeSlots: true})
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Len(t, memoryQueries(prom), 1)
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
	require.Empty(t, prom.got())
}
