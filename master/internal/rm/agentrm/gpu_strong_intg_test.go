//go:build integration
// +build integration

package agentrm

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// wireTopology is what an agent with the fixture's GPUs reports in its AgentStarted.
func wireTopology(f topologyFixture) *aproto.GPUTopology {
	g := f.build()
	wire := &aproto.GPUTopology{}
	for _, id := range f.ids {
		wire.GPUs = append(wire.GPUs, g.gpus[device.ID(id)])
	}
	for key, p := range g.pairs {
		wire.Links = append(wire.Links, aproto.GPULink{
			UUIDA: gpuDevice(int(key.a)).UUID, UUIDB: gpuDevice(int(key.b)).UUID, Level: p.level,
			NVLinks: p.nvlinks, P2PAToB: p.p2pAToB, P2PBToA: p.p2pBToA,
		})
	}
	return wire
}

// awaitInvalidRequest returns the cause of the next InvalidResourcesRequestError of the
// allocation, and fails the test if none arrives.
func awaitInvalidRequest(t *testing.T, sub *sproto.ResourcesSubscription, wait time.Duration) error {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		event, err := sub.GetWithContext(ctx)
		require.NoError(t, err, "no InvalidResourcesRequestError")
		if invalid, ok := event.(*sproto.InvalidResourcesRequestError); ok {
			return invalid.Cause
		}
	}
}

func strongWaitingLine(pool string, n string) string {
	return "GPU topology preference strong: waiting until one NUMA node of an agent in pool " + pool +
		" has " + n + " free GPUs"
}

func TestLiveStrongWaitsForOneNUMANode(t *testing.T) {
	// The check after deploy on an idle 8-GPU node, through the resource manager: five 1-GPU tasks
	// take slots 0-4, and those on 1-3 end. A strong 4-GPU task waits with one task-log line while
	// a plain one starts across the two NUMA nodes, and starts on NUMA node 1 once the plain one
	// and the task on slot 4 have ended. Strong 5 is refused at submit.
	h := newGPUHarness(t, node02, priorityScheduler42(best, nil))
	tasks := map[int]model.AllocationID{}
	for want := 0; want < 5; want++ {
		id, sub := h.requestWithID(t, 1, "")
		resources := awaitAllocated(t, sub, drainScheduleWait)
		require.NotNil(t, resources)
		require.Equal(t, []int{want}, deviceIDs(resources.devices))
		tasks[want] = id
	}
	for _, slot := range []int{1, 2, 3} {
		h.release(tasks[slot])
	}

	_, strong := h.requestWithID(t, 4, expconf.GPUTopologyStrong)
	require.Equal(t, strongWaitingLine(h.pool, "4"), awaitTaskLog(t, strong, drainScheduleWait))
	plainID, plain := h.requestWithID(t, 4, "")
	resources := awaitAllocated(t, plain, drainScheduleWait)
	require.NotNil(t, resources, "a plain task starts across NUMA nodes")
	nodes := map[int]bool{}
	for _, d := range resources.devices {
		nodes[int(d.ID)/4] = true
	}
	require.Len(t, nodes, 2)
	require.Empty(t, awaitTaskLog(t, strong, drainPendingWait), "one waiting line per task")

	_, err := h.rm.ValidateResources(sproto.ValidateResourcesRequest{
		ResourcePool: h.pool, Slots: 5, GPUTopology: expconf.GPUTopologyStrong,
	})
	require.EqualError(t, err, "no NUMA node in pool "+h.pool+" has 5 slots; use soft")

	h.release(plainID)
	require.Nil(t, awaitAllocated(t, strong, drainPendingWait))
	h.release(tasks[4])
	resources = awaitAllocated(t, strong, drainScheduleWait)
	require.NotNil(t, resources)
	require.Equal(t, []int{4, 5, 6, 7}, deviceIDs(resources.devices))
	require.Equal(t, "GPU topology preference: agent "+string(h.agent.id)+", slots 4,5,6,7; "+
		"worst pair NODE, P2P not usable", awaitTaskLog(t, strong, drainScheduleWait))
}

func TestLiveStrongAfterAgentStartedAgain(t *testing.T) {
	// As after a master restart: the agent, restored from its snapshot, has no topology until its
	// AgentStarted. Strong tasks wait, and strong 5 is accepted at submit. When the AgentStarted
	// arrives, the pool runs a pass: the 4-GPU task starts and the 5-GPU one fails, with a cause
	// that ends a trial without restarts.
	h := newGPUHarness(t, node02, priorityScheduler42(best, nil))
	h.agent.mu.Lock()
	h.agent.agentState.setGPUTopology(nil)
	h.agent.mu.Unlock()

	_, err := h.rm.ValidateResources(sproto.ValidateResourcesRequest{
		ResourcePool: h.pool, Slots: 5, GPUTopology: expconf.GPUTopologyStrong,
	})
	require.NoError(t, err)
	four := h.request(t, 4, expconf.GPUTopologyStrong)
	five := h.request(t, 5, expconf.GPUTopologyStrong)
	require.Equal(t, strongWaitingLine(h.pool, "4"), awaitTaskLog(t, four, drainScheduleWait))
	require.Equal(t, strongWaitingLine(h.pool, "5"), awaitTaskLog(t, five, drainScheduleWait))
	require.Nil(t, awaitAllocated(t, four, drainPendingWait))

	h.agent.HandleIncomingWebsocketMessage(&aproto.MasterMessage{AgentStarted: &aproto.AgentStarted{
		Version: "test", ResourcePoolName: h.pool, Devices: gpuDeviceList(node02IDs...),
		GPUTopology: wireTopology(node02),
	}})
	resources := awaitAllocated(t, four, drainScheduleWait)
	require.NotNil(t, resources, "the AgentStarted of a started agent reschedules")
	require.Equal(t, []int{0, 1, 2, 3}, deviceIDs(resources.devices))
	cause := awaitInvalidRequest(t, five, drainScheduleWait)
	require.EqualError(t, cause, "invalid resources request: no NUMA node in pool "+h.pool+
		" has 5 slots; use soft")
	require.True(t, sproto.IsUnrecoverableSystemError(cause))
}

func TestValidateStrongAtSubmit(t *testing.T) {
	validate := func(h *gpuHarness, slots int, pref expconf.GPUTopologyPreference, singleNode bool) error {
		_, err := h.rm.ValidateResources(sproto.ValidateResourcesRequest{
			ResourcePool: h.pool, Slots: slots, GPUTopology: pref, IsSingleNode: singleNode,
		})
		return err
	}
	strong := expconf.GPUTopologyStrong

	h := newGPUHarness(t, node02, priorityScheduler42(best, nil))
	require.NoError(t, validate(h, 4, strong, true))
	require.NoError(t, validate(h, 1, strong, true))
	require.NoError(t, validate(h, 5, expconf.GPUTopologySoft, true))
	require.NoError(t, validate(h, 5, "", false))
	// Strong uses one agent, whatever is_single_node says.
	for _, singleNode := range []bool{true, false} {
		require.EqualError(t, validate(h, 5, strong, singleNode),
			"no NUMA node in pool "+h.pool+" has 5 slots; use soft")
	}
	// The single-node check refuses it with the cause of strong.
	require.EqualError(t, validate(h, 9, strong, false), "no NUMA node in pool "+h.pool+" has 9 slots; use soft")
	require.EqualError(t, validate(h, 9, "", true), "request unfulfillable, please try requesting less slots")

	// Disabled slots count.
	for _, id := range []device.ID{0, 1} {
		_, err := h.agent.PatchSlotState(patchSlotState{id: id, enabled: ptrs.Ptr(false)})
		require.NoError(t, err)
	}
	require.NoError(t, validate(h, 4, strong, true))

	// A CPU pool, and an agent with an unknown topology.
	cpus := []device.Device{{ID: 0, Type: device.CPU}, {ID: 1, Type: device.CPU}}
	h = newDevicesHarness(t, cpus, &gpuTopology{gpus: map[device.ID]aproto.GPUInfo{}}, priorityScheduler42(best, nil))
	require.EqualError(t, validate(h, 2, strong, true), "no agent in pool "+h.pool+" reports NUMA nodes; use soft")
	h = newDevicesHarness(t, gpuDeviceList(node02IDs...),
		&gpuTopology{unknownReason: "agent 0.40.0 does not report GPU topology"}, priorityScheduler42(best, nil))
	require.EqualError(t, validate(h, 2, strong, true), "no agent in pool "+h.pool+" reports NUMA nodes; use soft")

	// A pool without agents: the single-node check refuses strong, as it refuses is_single_node.
	h = newGPUHarness(t, node02, priorityScheduler42(best, nil))
	require.NoError(t, h.rm.agentService.agents.Delete(h.agent.id))
	require.EqualError(t, validate(h, 2, strong, false), "no agent in pool "+h.pool+" reports NUMA nodes; use soft")
	require.EqualError(t, validate(h, 2, "", true), "request unfulfillable, please try requesting less slots")
	require.NoError(t, validate(h, 2, expconf.GPUTopologySoft, false))
	require.NoError(t, validate(h, 1, strong, false))
}
