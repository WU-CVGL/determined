//go:build integration
// +build integration

package internal

import (
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/proto/pkg/agentv1"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// node01WithExcludeList is an agent whose exclude list leaves out 81:00.0: slot 3 is at x8 of x16.
func node01WithExcludeList() *agentv1.Agent {
	topo := &agentv1.GpuTopology{DriverVersion: "610.57.04"}
	for _, id := range []int32{0, 1, 2, 3, 5, 6, 7} {
		width := int32(16)
		if id == 3 {
			width = 8
		}
		topo.Gpus = append(topo.Gpus, &agentv1.GpuInfo{
			DeviceId: id, Uuid: "GPU-slot", NumaNode: -1, PcieLinkWidth: width, PcieLinkWidthMax: 16,
		})
	}
	topo.Gpus = append(topo.Gpus, &agentv1.GpuInfo{
		DeviceId: -1, Uuid: "GPU-excluded", PciBusId: "0000:81:00.0", NumaNode: 1,
		PcieLinkWidth: 16, PcieLinkWidthMax: 16, Excluded: true,
	})
	return &agentv1.Agent{Id: "node01", SlotStats: &agentv1.SlotStats{}, GpuTopology: topo}
}

func TestGetAgentExcludedGPUs(t *testing.T) {
	mockRM := MockRM()
	api, _, ctx := setupAPITest(t, nil, mockRM)

	mockRM.On("GetAgent", mock.Anything).Return(
		func(*apiv1.GetAgentRequest) *apiv1.GetAgentResponse {
			return &apiv1.GetAgentResponse{Agent: node01WithExcludeList()}
		}, nil)
	resp, err := api.GetAgent(ctx, &apiv1.GetAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	gpus := resp.Agent.GpuTopology.Gpus
	require.Len(t, gpus, 8)
	for _, g := range gpus[:7] {
		want := agentv1.GpuHealth_GPU_HEALTH_OK
		if g.DeviceId == 3 {
			want = agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX
		}
		require.Equal(t, want, g.Health, "slot %d", g.DeviceId)
	}
	require.True(t, gpus[7].Excluded)
	require.Equal(t, int32(-1), gpus[7].DeviceId)
	require.Equal(t, "0000:81:00.0", gpus[7].PciBusId)
	require.Equal(t, agentv1.GpuHealth_GPU_HEALTH_OK, gpus[7].Health)

	mockRM.On("GetAgents").Return(
		func() *apiv1.GetAgentsResponse {
			return &apiv1.GetAgentsResponse{Agents: []*agentv1.Agent{node01WithExcludeList()}}
		}, nil)
	all, err := api.GetAgents(ctx, &apiv1.GetAgentsRequest{})
	require.NoError(t, err)
	require.Len(t, all.Agents, 1)
	require.Len(t, all.Agents[0].GpuTopology.Gpus, 8)
	require.Equal(t, agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX, all.Agents[0].GpuTopology.Gpus[3].Health)

	// exclude_slots drops the topology with the slots.
	all, err = api.GetAgents(ctx, &apiv1.GetAgentsRequest{ExcludeSlots: true})
	require.NoError(t, err)
	require.Nil(t, all.Agents[0].GpuTopology)
}

// The health is classified in the responses of agent enable and disable too.
func TestEnableDisableAgentClassifiesGPUHealth(t *testing.T) {
	var mockRM mocks.ResourceManager
	api, _, ctx := setupAPITest(t, nil, &mockRM)

	mockRM.On("EnableAgent", mock.Anything).Return(
		func(*apiv1.EnableAgentRequest) *apiv1.EnableAgentResponse {
			return &apiv1.EnableAgentResponse{Agent: node01WithExcludeList()}
		}, nil)
	mockRM.On("DisableAgent", mock.Anything).Return(
		func(*apiv1.DisableAgentRequest) *apiv1.DisableAgentResponse {
			return &apiv1.DisableAgentResponse{Agent: node01WithExcludeList()}
		}, nil)

	enabled, err := api.EnableAgent(ctx, &apiv1.EnableAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Equal(t, agentv1.GpuHealth_GPU_HEALTH_LINK_BELOW_MAX, enabled.Agent.GpuTopology.Gpus[3].Health)
	disabled, err := api.DisableAgent(ctx, &apiv1.DisableAgentRequest{AgentId: "node01"})
	require.NoError(t, err)
	require.Equal(t, agentv1.GpuHealth_GPU_HEALTH_OK, disabled.Agent.GpuTopology.Gpus[0].Health)
}
