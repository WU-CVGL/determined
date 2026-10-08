//go:build integration
// +build integration

package agentrm

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// addGPUAgent adds a started agent with the slots and the GPU topology of a fixture to the
// harness's pool.
func (h *gpuHarness) addGPUAgent(t *testing.T, id aproto.ID, f topologyFixture) *agent {
	t.Helper()
	poolConfig := config.ResourcePoolConfig{
		PoolName:                 h.pool,
		MaxAuxContainersPerAgent: 100,
		AgentReconnectWait:       model.Duration(time.Minute),
	}
	a := newAgent(
		id, h.rm.agentService.agentUpdates, h.pool, &poolConfig, &aproto.MasterSetAgentOptions{}, nil, func() {},
	)
	a.mu.Lock()
	a.agentStarted(&aproto.AgentStarted{
		Version: "test", ResourcePoolName: h.pool, Devices: gpuDeviceList(f.ids...),
	})
	a.agentState.setGPUTopology(f.build())
	a.started = true
	a.mu.Unlock()
	require.NoError(t, h.rm.agentService.agents.Add(a.id, a))
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		require.NoError(t, a.agentState.delete())
	})
	return a
}

func disableSlots(t *testing.T, a *agent, ids ...device.ID) {
	t.Helper()
	for _, id := range ids {
		_, err := a.PatchSlotState(patchSlotState{id: id, enabled: ptrs.Ptr(false)})
		require.NoError(t, err)
	}
}

func TestLiveSoftAgentChoice(t *testing.T) {
	// The check after deploy, through the resource manager, on two agents of the cluster's layout:
	// node03 with slots 0, 1 and 4 disabled (free 2,3 | 5,6,7) and node04 with slot 0 disabled
	// (free 1,2,3 | 4,5,6,7). Under best with packing, with the priority and the fair-share
	// scheduler, a plain 4-GPU task gets node03 {2,5,6,7}, and soft and strong get node04
	// {4,5,6,7}. With numa_packing false, soft gets node03, as a plain task does.
	//
	// Under worst, node03 runs a strong 4-GPU task on one NUMA node (4 of 8 GPUs free, all on the
	// other node), and node04 with slots 0 and 4 disabled has 6 of 6 free (1,2,3 | 5,6,7): soft
	// gets node04, which WorstFit picks, and its best set there.
	type placement struct {
		agent string
		gpus  []int
	}
	fairShare := &config.SchedulerConfig{FairShare: &config.FairShareSchedulerConfig{}, FittingPolicy: best}
	for name, c := range map[string]struct {
		scheduler *config.SchedulerConfig
		want      map[expconf.GPUTopologyPreference]placement
	}{
		"priority": {priorityScheduler42(best, nil), map[expconf.GPUTopologyPreference]placement{
			"":                        {"node03", []int{2, 5, 6, 7}},
			expconf.GPUTopologySoft:   {"node04", []int{4, 5, 6, 7}},
			expconf.GPUTopologyStrong: {"node04", []int{4, 5, 6, 7}},
		}},
		"fair share": {fairShare, map[expconf.GPUTopologyPreference]placement{
			"":                      {"node03", []int{2, 5, 6, 7}},
			expconf.GPUTopologySoft: {"node04", []int{4, 5, 6, 7}},
		}},
		"numa_packing false": {priorityScheduler42(best, ptrs.Ptr(false)), map[expconf.GPUTopologyPreference]placement{
			expconf.GPUTopologySoft: {"node03", []int{2, 5, 6, 7}},
		}},
	} {
		layout := clusterNode(node02IDs)
		h := newGPUHarness(t, layout, c.scheduler)
		node04 := h.addGPUAgent(t, aproto.ID(h.pool+"-node04"), layout)
		names := map[aproto.ID]string{h.agent.id: "node03", node04.id: "node04"}
		disableSlots(t, h.agent, 0, 1, 4)
		disableSlots(t, node04, 0)

		for pref, want := range c.want {
			id, sub := h.requestWithID(t, 4, pref)
			resources := awaitAllocated(t, sub, drainScheduleWait)
			require.NotNil(t, resources, "%s, %q", name, pref)
			got := placement{names[resources.agent.id], deviceIDs(resources.devices)}
			require.Equal(t, want, got, "%s, %q", name, pref)
			if pref == expconf.GPUTopologySoft {
				pair := "SYS"
				if got.agent == "node04" {
					pair = "NODE"
				}
				require.Equal(t, "GPU topology preference: agent "+string(resources.agent.id)+", slots "+
					idList(resources.devices)+"; worst pair "+pair+", P2P usable",
					awaitTaskLog(t, sub, drainScheduleWait), name)
			}
			h.release(id)
		}
	}

	layout := clusterNode(node02IDs)
	h := newGPUHarness(t, layout, priorityScheduler42(worst, nil))
	node04 := h.addGPUAgent(t, aproto.ID(h.pool+"-node04"), layout)
	disableSlots(t, node04, 0, 4)
	strongID, strongSub := h.requestWithID(t, 4, expconf.GPUTopologyStrong)
	held := awaitAllocated(t, strongSub, drainScheduleWait)
	require.NotNil(t, held)
	require.Equal(t, h.agent.id, held.agent.id)
	id, sub := h.requestWithID(t, 4, expconf.GPUTopologySoft)
	resources := awaitAllocated(t, sub, drainScheduleWait)
	require.NotNil(t, resources)
	require.Equal(t, node04.id, resources.agent.id)
	require.Equal(t, []int{1, 2, 3, 5}, deviceIDs(resources.devices))
	require.Equal(t, "GPU topology preference: agent "+string(node04.id)+", slots 1,2,3,5; worst pair SYS, "+
		"P2P usable", awaitTaskLog(t, sub, drainScheduleWait))
	h.release(id)
	h.release(strongID)
}
