//go:build integration
// +build integration

package internal

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/cluster"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// agentWithXIDs is an agent with slots 0 and 1 (GPU-0 and GPU-1) and the excluded GPU-x, all at
// x16 of x16, so only an XID can turn one red.
func agentWithXIDs() *agentv1.Agent {
	topo := &agentv1.GpuTopology{DriverVersion: "610.57.04"}
	for _, id := range []int32{0, 1} {
		topo.Gpus = append(topo.Gpus, &agentv1.GpuInfo{
			DeviceId: id, Uuid: "GPU-" + string('0'+id), NumaNode: 0,
			PcieLinkWidth: 16, PcieLinkWidthMax: 16,
		})
	}
	topo.Gpus = append(topo.Gpus, &agentv1.GpuInfo{
		DeviceId: -1, Uuid: "GPU-x", PciBusId: "0000:81:00.0", NumaNode: 1,
		PcieLinkWidth: 16, PcieLinkWidthMax: 16, Excluded: true,
	})
	return &agentv1.Agent{Id: "node01", SlotStats: &agentv1.SlotStats{}, GpuTopology: topo}
}

func mockAgentsWithXIDs(mockRM *mocks.ResourceManager) {
	mockRM.On("GetAgents").Return(
		func() *apiv1.GetAgentsResponse {
			return &apiv1.GetAgentsResponse{Agents: []*agentv1.Agent{agentWithXIDs()}}
		}, nil)
	mockRM.On("GetAgent", mock.Anything).Return(
		func(*apiv1.GetAgentRequest) *apiv1.GetAgentResponse {
			return &apiv1.GetAgentResponse{Agent: agentWithXIDs()}
		}, nil)
	mockRM.On("EnableAgent", mock.Anything).Return(
		func(*apiv1.EnableAgentRequest) *apiv1.EnableAgentResponse {
			return &apiv1.EnableAgentResponse{Agent: agentWithXIDs()}
		}, nil)
}

func healths(topo *agentv1.GpuTopology) []agentv1.GpuHealth {
	var out []agentv1.GpuHealth
	for _, g := range topo.Gpus {
		out = append(out, g.Health)
	}
	return out
}

const (
	healthOK    = agentv1.GpuHealth_GPU_HEALTH_OK
	healthError = agentv1.GpuHealth_GPU_HEALTH_ERROR
)

func TestGetAgentsRecentXIDs(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)
	prom := newFakeXIDPrometheus(t, xidMatrix)
	api.m.config.Integrations.TaskResources = config.TaskResourcesConfig{
		PrometheusURL: prom.URL, DetCluster: "lab-a",
	}

	// The WebUI's cluster store polls without slots: no query.
	all, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{ExcludeSlots: true})
	require.NoError(t, err)
	require.Nil(t, all.Agents[0].GpuTopology)
	require.Empty(t, prom.got())

	// An enable or disable response never queries; before the first query it has no XIDs.
	enabled, err := api.EnableAgent(ctx, &apiv1.EnableAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_UNSPECIFIED,
		enabled.Agent.GpuTopology.XidQueryStatus)
	require.Equal(t, []agentv1.GpuHealth{healthOK, healthOK, healthOK}, healths(enabled.Agent.GpuTopology))
	require.Empty(t, prom.got())

	one, err := api.GetAgent(ctx, &apiv1.GetAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Len(t, prom.got(), 1)
	require.Equal(t, `max by (gpu_uuid, xid) (max_over_time(DCGM_EXP_XID_ERRORS_COUNT{job="dcgm", `+
		`det_cluster="lab-a", gpu_uuid!="", xid!="", xid!="0", xid!~"13|31|43|45"}[5m])) > 0`,
		prom.got()[0].Get("query"))
	topo := one.Agent.GpuTopology
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_OK, topo.XidQueryStatus)
	require.Empty(t, topo.XidQueryError)
	require.NotNil(t, topo.XidQueriedAt)
	// XID 79 on slot 1 and 94 on the excluded GPU; XID 13 on slot 0 never counts.
	require.Equal(t, []agentv1.GpuHealth{healthOK, healthError, healthError}, healths(topo))
	require.Empty(t, topo.Gpus[0].RecentXids)
	require.Len(t, topo.Gpus[1].RecentXids, 1)
	require.Equal(t, int32(79), topo.Gpus[1].RecentXids[0].Xid)
	require.Equal(t, int64(1791367800), topo.Gpus[1].RecentXids[0].FirstObserved.Seconds)
	require.Equal(t, int64(1791368700), topo.Gpus[1].RecentXids[0].LastObserved.Seconds)
	require.Equal(t, int32(94), topo.Gpus[2].RecentXids[0].Xid)

	// The list with slots uses the cached result.
	all, err = api.GetAgents(ctx, &apiv1.GetAgentsRequest{})
	require.NoError(t, err)
	require.Len(t, prom.got(), 1)
	require.Equal(t, []agentv1.GpuHealth{healthOK, healthError, healthError}, healths(all.Agents[0].GpuTopology))

	// Enable and disable responses carry the last result without querying.
	enabled, err = api.EnableAgent(ctx, &apiv1.EnableAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Equal(t, []agentv1.GpuHealth{healthOK, healthError, healthError}, healths(enabled.Agent.GpuTopology))
	require.Len(t, prom.got(), 1)
}

func TestGetAgentsRecentXIDsNotConfigured(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)

	one, err := api.GetAgent(ctx, &apiv1.GetAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	topo := one.Agent.GpuTopology
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_NOT_CONFIGURED, topo.XidQueryStatus)
	require.Nil(t, topo.XidQueriedAt)
	require.Equal(t, []agentv1.GpuHealth{healthOK, healthOK, healthOK}, healths(topo))
}

func TestGetAgentsRecentXIDsFailed(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)
	prom := newFakeXIDPrometheus(t, "upstream detail")
	prom.respond(http.StatusInternalServerError, "upstream detail")
	api.m.config.Integrations.TaskResources = config.TaskResourcesConfig{
		PrometheusURL: prom.URL, DetCluster: "lab-a",
	}

	all, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{})
	require.NoError(t, err)
	topo := all.Agents[0].GpuTopology
	require.Equal(t, agentv1.GpuXidQueryStatus_GPU_XID_QUERY_STATUS_FAILED, topo.XidQueryStatus)
	require.Equal(t, "request failed", topo.XidQueryError)
	require.NotContains(t, topo.XidQueryError, prom.URL)
	require.NotNil(t, topo.XidQueriedAt)
	// The health is left to the agent's report.
	require.Equal(t, []agentv1.GpuHealth{healthOK, healthOK, healthOK}, healths(topo))

	// The failure is cached too.
	_, err = api.GetAgent(ctx, &apiv1.GetAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Len(t, prom.got(), 1)
}

// Obfuscation comes first: agents a user may not see in detail trigger no query.
func TestGetAgentsRecentXIDsNoQueryWhenObfuscated(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)
	mockAgentsWithXIDs(&mockRM)
	prom := newFakeXIDPrometheus(t, xidMatrix)
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

	all, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{})
	require.NoError(t, err)
	require.Nil(t, all.Agents[0].GpuTopology)
	one, err := api.GetAgent(ctx, &apiv1.GetAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Nil(t, one.Agent.GpuTopology)
	require.Empty(t, prom.got())
}
