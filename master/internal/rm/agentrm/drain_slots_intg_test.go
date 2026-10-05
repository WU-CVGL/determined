//go:build integration
// +build integration

package agentrm

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

const (
	// drainPendingWait covers several scheduler ticks (actionCoolDown) in which a request must
	// stay pending.
	drainPendingWait = 4 * actionCoolDown
	// drainScheduleWait bounds the wait for a request that must be scheduled.
	drainScheduleWait = 10 * actionCoolDown
)

// drainHarness is a resource manager with one pool and one started agent with one GPU slot. Its
// scheduler only runs when something notifies the pool, as in the master.
type drainHarness struct {
	rm    *ResourceManager
	pool  string
	agent *agent
}

func newDrainHarness(t *testing.T) *drainHarness {
	t.Helper()
	poolName := "drain-" + uuid.NewString()[:8]
	poolConfig := config.ResourcePoolConfig{
		PoolName:                 poolName,
		MaxAuxContainersPerAgent: 100,
		AgentReconnectWait:       model.Duration(time.Minute),
	}
	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{
			AgentRM: &config.AgentResourceManagerConfig{
				Scheduler: &config.SchedulerConfig{
					FairShare:     &config.FairShareSchedulerConfig{},
					FittingPolicy: best,
				},
				DefaultComputeResourcePool: poolName,
				DefaultAuxResourcePool:     poolName,
			},
		},
		ResourcePools: []config.ResourcePoolConfig{poolConfig},
	}
	registry, err := newPoolRegistry(rmConfig.ResourcePools)
	require.NoError(t, err)
	agentService, agentUpdates := newAgentService(registry, &aproto.MasterSetAgentOptions{}, false)
	rm, err := newAgentResourceManager(nil, rmConfig, nil, agentService, agentUpdates, registry)
	require.NoError(t, err)
	t.Cleanup(rm.stop)

	a := newAgent(
		aproto.ID(poolName+"-agent"), agentUpdates, poolName, &poolConfig,
		&aproto.MasterSetAgentOptions{}, nil, func() {},
	)
	a.mu.Lock()
	a.agentStarted(&aproto.AgentStarted{
		Version:          "test",
		ResourcePoolName: poolName,
		Devices: []device.Device{{
			ID: 0, Brand: "nvda", UUID: "GPU-" + uuid.NewString(), Type: device.CUDA,
		}},
	})
	a.started = true
	a.mu.Unlock()
	require.NoError(t, agentService.agents.Add(a.id, a))
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		require.NoError(t, a.agentState.delete())
	})

	return &drainHarness{rm: rm, pool: poolName, agent: a}
}

// request asks the pool for one slot for a new task.
func (h *drainHarness) request(t *testing.T) (sproto.AllocateRequest, *sproto.ResourcesSubscription) {
	t.Helper()
	ctx := context.Background()
	taskID := model.TaskID(uuid.NewString())
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID:     taskID,
		TaskType:   model.TaskTypeCommand,
		StartTime:  time.Now(),
		LogVersion: model.CurrentTaskLogVersion,
	}))
	allocationID := model.AllocationID(string(taskID) + ".0")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID,
		TaskID:       taskID,
		Slots:        1,
		ResourcePool: h.pool,
		StartTime:    ptrs.Ptr(time.Now()),
		State:        ptrs.Ptr(model.AllocationStateAssigned),
		Ports:        map[string]int{},
	}))

	// The job's scheduling group lives while the job is registered, as for a command.
	jobID := model.JobID(taskID)
	require.NoError(t, tasklist.GroupPriorityChangeRegistry.Add(jobID, nil))
	t.Cleanup(func() { _ = tasklist.GroupPriorityChangeRegistry.Delete(jobID) })

	req := sproto.AllocateRequest{
		TaskID:            taskID,
		AllocationID:      allocationID,
		JobID:             jobID,
		Name:              string(allocationID),
		SlotsNeeded:       1,
		ResourcePool:      h.pool,
		JobSubmissionTime: time.Now(),
		Preemption:        sproto.PreemptionConfig{Preemptible: true},
	}
	sub, err := h.rm.Allocate(req)
	require.NoError(t, err)
	t.Cleanup(sub.Close)
	return req, sub
}

func (h *drainHarness) release(req sproto.AllocateRequest) {
	h.rm.Release(sproto.ResourcesReleased{AllocationID: req.AllocationID, ResourcePool: h.pool})
}

func (h *drainHarness) disableSlot(t *testing.T, drain bool) {
	t.Helper()
	_, err := h.rm.DisableSlot(&apiv1.DisableSlotRequest{
		AgentId: string(h.agent.id), SlotId: "0", Drain: drain,
	})
	require.NoError(t, err)
}

func (h *drainHarness) enableSlot(t *testing.T) {
	t.Helper()
	_, err := h.rm.EnableSlot(&apiv1.EnableSlotRequest{AgentId: string(h.agent.id), SlotId: "0"})
	require.NoError(t, err)
}

// awaitAllocated returns the allocation's resources, or nil if none arrive within wait.
func awaitAllocated(
	t *testing.T, sub *sproto.ResourcesSubscription, wait time.Duration,
) *containerResources {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		event, err := sub.GetWithContext(ctx)
		if err != nil {
			return nil
		}
		if allocated, ok := event.(*sproto.ResourcesAllocated); ok {
			require.Len(t, allocated.Resources, 1)
			for _, r := range allocated.Resources {
				return r.(*containerResources)
			}
		}
	}
}

func TestDrainedSlotStaysUnallocatableAfterItsTaskExits(t *testing.T) {
	h := newDrainHarness(t)

	first, firstSub := h.request(t)
	resources := awaitAllocated(t, firstSub, drainScheduleWait)
	require.NotNil(t, resources, "the first task was not scheduled")

	// The agent starts the first task's container.
	container := cproto.Container{
		ID: resources.containerID, State: cproto.Running, Devices: resources.devices,
	}
	h.agent.mu.Lock()
	err := h.agent.agentState.startContainer(sproto.StartTaskContainer{
		AllocationID:   first.AllocationID,
		StartContainer: aproto.StartContainer{Container: container},
	})
	h.agent.mu.Unlock()
	require.NoError(t, err)

	h.disableSlot(t, true)
	_, secondSub := h.request(t)
	require.Nil(t, awaitAllocated(t, secondSub, drainPendingWait),
		"a task was scheduled on a slot in use")

	// The first task exits: the agent reports its container terminated, and the task releases
	// its resources.
	container.State = cproto.Terminated
	h.agent.mu.Lock()
	h.agent.containerStateChanged(aproto.ContainerStateChanged{
		Container: container, ContainerStopped: &aproto.ContainerStopped{},
	})
	h.agent.mu.Unlock()
	h.release(first)

	require.Nil(t, awaitAllocated(t, secondSub, drainPendingWait),
		"a task was scheduled on the drained slot after the slot's task exited")

	h.enableSlot(t)
	require.NotNil(t, awaitAllocated(t, secondSub, drainScheduleWait),
		"the pending task was not scheduled after the slot was enabled")
}

func TestDrainedSlotStaysUnallocatableAfterItsReservationIsCanceled(t *testing.T) {
	h := newDrainHarness(t)

	// The first task gets the slot but its container never starts.
	first, firstSub := h.request(t)
	require.NotNil(t, awaitAllocated(t, firstSub, drainScheduleWait),
		"the first task was not scheduled")

	h.disableSlot(t, true)
	_, secondSub := h.request(t)
	require.Nil(t, awaitAllocated(t, secondSub, drainPendingWait),
		"a task was scheduled on a reserved slot")

	h.release(first)
	require.Nil(t, awaitAllocated(t, secondSub, drainPendingWait),
		"a task was scheduled on the drained slot after its reservation was canceled")

	h.enableSlot(t)
	require.NotNil(t, awaitAllocated(t, secondSub, drainScheduleWait),
		"the pending task was not scheduled after the slot was enabled")
}

func TestEnablingASlotSchedulesPendingWork(t *testing.T) {
	for _, tc := range []struct {
		name  string
		drain bool
	}{
		{name: "disabled slot", drain: false},
		{name: "drained idle slot", drain: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newDrainHarness(t)

			h.disableSlot(t, tc.drain)
			_, sub := h.request(t)
			require.Nil(t, awaitAllocated(t, sub, drainPendingWait),
				"a task was scheduled on a slot that is not enabled")

			// Nothing else happens in the pool: only the slot change can start the pending task.
			h.enableSlot(t)
			require.NotNil(t, awaitAllocated(t, sub, drainScheduleWait),
				"the pending task was not scheduled after the slot was enabled")
		})
	}
}

// When an agent reconnects, its slots can be drained (a slot patch, or the agent's own drain
// applied again) before the master clears the containers the agent did not reattach. A drained
// slot whose container is cleared there must not become free.
func TestDrainedSlotStaysUnallocatableWhenItsContainerIsNotRecovered(t *testing.T) {
	state, _ := newSlotsAgentState(t, 2)
	state.resourcePoolName = "drain-pool"
	t.Cleanup(func() { require.NoError(t, state.delete()) })

	cid, used, other := runOnOneSlot(t, state)
	drainSlot(t, state, used)
	requireOnlyAllocatable(t, state, other)
	require.Equal(t, 2, state.numSlots())

	require.NoError(t, state.clearUnlessRecovered(map[cproto.ID]aproto.ContainerReattachAck{}))

	require.Nil(t, state.slotStates[used].containerID)
	require.NotContains(t, state.containerState, cid)
	requireOnlyAllocatable(t, state, other)
	require.Equal(t, 1, state.numSlots())
}
