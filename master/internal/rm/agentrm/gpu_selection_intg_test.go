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
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// gpuHarness is a resource manager with the priority scheduler and one pool whose one started
// agent has the slots and the GPU topology of a fixture. The pool sets no scheduler of its own: it
// inherits the resource manager's, as the cluster's pools do.
type gpuHarness struct {
	rm    *ResourceManager
	pool  string
	agent *agent
}

func priorityScheduler42(fittingPolicy string, numaPacking *bool) *config.SchedulerConfig {
	return &config.SchedulerConfig{
		Priority:      &config.PrioritySchedulerConfig{DefaultPriority: ptrs.Ptr(42)},
		FittingPolicy: fittingPolicy,
		NUMAPacking:   numaPacking,
	}
}

// ensureTestDB reconnects the database singleton when an earlier test, such as a dynamic pool test
// with a database of its own, left it closed.
func ensureTestDB(t *testing.T) {
	t.Helper()
	if err := db.Bun().PingContext(context.Background()); err != nil {
		db.MustResolveTestPostgres(t)
	}
}

func newGPUHarness(t *testing.T, f topologyFixture, scheduler *config.SchedulerConfig) *gpuHarness {
	t.Helper()
	return newDevicesHarness(t, gpuDeviceList(f.ids...), f.build(), scheduler)
}

// newDevicesHarness is newGPUHarness for an agent with these devices and this topology.
func newDevicesHarness(
	t *testing.T, devices []device.Device, topology *gpuTopology, scheduler *config.SchedulerConfig,
) *gpuHarness {
	t.Helper()
	ensureTestDB(t)
	poolName := "gpus-" + uuid.NewString()[:8]
	poolConfig := config.ResourcePoolConfig{
		PoolName:                 poolName,
		MaxAuxContainersPerAgent: 100,
		AgentReconnectWait:       model.Duration(time.Minute),
	}
	rmConfig := &config.ResourceManagerWithPoolsConfig{
		ResourceManager: &config.ResourceManagerConfig{
			AgentRM: &config.AgentResourceManagerConfig{
				Scheduler:                  scheduler,
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
		Version: "test", ResourcePoolName: poolName, Devices: devices,
	})
	a.agentState.setGPUTopology(topology)
	a.started = true
	a.mu.Unlock()
	require.NoError(t, agentService.agents.Add(a.id, a))
	t.Cleanup(func() {
		a.mu.Lock()
		defer a.mu.Unlock()
		require.NoError(t, a.agentState.delete())
	})
	return &gpuHarness{rm: rm, pool: poolName, agent: a}
}

// request asks the pool for slots for a new command-like task.
func (h *gpuHarness) request(
	t *testing.T, slots int, pref expconf.GPUTopologyPreference,
) *sproto.ResourcesSubscription {
	t.Helper()
	_, sub := h.requestWithID(t, slots, pref)
	return sub
}

// requestWithID is request that also returns the task's allocation ID.
func (h *gpuHarness) requestWithID(
	t *testing.T, slots int, pref expconf.GPUTopologyPreference,
) (model.AllocationID, *sproto.ResourcesSubscription) {
	t.Helper()
	ctx := context.Background()
	taskID := model.TaskID(uuid.NewString())
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID: taskID, TaskType: model.TaskTypeCommand, StartTime: time.Now(),
		LogVersion: model.CurrentTaskLogVersion,
	}))
	allocationID := model.AllocationID(string(taskID) + ".0")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID, TaskID: taskID, Slots: slots, ResourcePool: h.pool,
		StartTime: ptrs.Ptr(time.Now()), State: ptrs.Ptr(model.AllocationStateAssigned),
		Ports: map[string]int{},
	}))
	jobID := model.JobID(taskID)
	require.NoError(t, tasklist.GroupPriorityChangeRegistry.Add(jobID, nil))
	t.Cleanup(func() { _ = tasklist.GroupPriorityChangeRegistry.Delete(jobID) })

	sub, err := h.rm.Allocate(sproto.AllocateRequest{
		TaskID: taskID, AllocationID: allocationID, JobID: jobID, Name: string(allocationID),
		SlotsNeeded: slots, ResourcePool: h.pool, JobSubmissionTime: time.Now(),
		FittingRequirements: sproto.FittingRequirements{SingleAgent: true, GPUTopology: pref},
	})
	require.NoError(t, err)
	t.Cleanup(sub.Close)
	return allocationID, sub
}

// release ends a task: the pool releases its resources.
func (h *gpuHarness) release(id model.AllocationID) {
	h.rm.Release(sproto.ResourcesReleased{AllocationID: id, ResourcePool: h.pool})
}

// allocate requests slots and returns the slot IDs the task gets.
func (h *gpuHarness) allocate(t *testing.T, slots int, pref expconf.GPUTopologyPreference) []int {
	t.Helper()
	resources := awaitAllocated(t, h.request(t, slots, pref), drainScheduleWait)
	require.NotNil(t, resources, "a %d-slot task was not scheduled", slots)
	return deviceIDs(resources.devices)
}

// awaitTaskLog returns the next task-log line of the allocation, or "" if none arrives.
func awaitTaskLog(t *testing.T, sub *sproto.ResourcesSubscription, wait time.Duration) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	for {
		event, err := sub.GetWithContext(ctx)
		if err != nil {
			return ""
		}
		if log, ok := event.(*sproto.ContainerLog); ok {
			return log.Message()
		}
	}
}

func TestLiveReservationsPackByNUMA(t *testing.T) {
	// Eight 1-slot tasks through the resource manager get slots 0..7 in order; map order does so
	// with probability 8!/8^8.
	h := newGPUHarness(t, node02, priorityScheduler42(best, nil))
	for want := 0; want < 8; want++ {
		require.Equal(t, []int{want}, h.allocate(t, 1, ""))
	}

	h = newGPUHarness(t, node01, priorityScheduler42(best, nil))
	var order []int
	for range node01IDs {
		order = append(order, h.allocate(t, 1, "")...)
	}
	require.Equal(t, []int{5, 6, 7, 0, 1, 2, 3}, order)
}

func TestLiveReservationsWithoutPacking(t *testing.T) {
	// worst and numa_packing false keep map order: only the counts are certain.
	for name, scheduler := range map[string]*config.SchedulerConfig{
		"worst":            priorityScheduler42(worst, nil),
		"numa_packing off": priorityScheduler42(best, ptrs.Ptr(false)),
	} {
		h := newGPUHarness(t, node02, scheduler)
		seen := map[int]bool{}
		for i := 0; i < 8; i++ {
			got := h.allocate(t, 1, "")
			require.Len(t, got, 1, name)
			require.False(t, seen[got[0]], name)
			seen[got[0]] = true
		}
	}
}

func TestLiveReservationPrefersGPUTopology(t *testing.T) {
	// g292 with the stock driver: plain 2-slot tasks get the lowest IDs, a soft one GPUs on two
	// switches, and the task log says why.
	h := newGPUHarness(t, g292(p2pNotOK), priorityScheduler42(best, nil))
	require.Equal(t, []int{0, 1}, h.allocate(t, 2, ""))
	sub := h.request(t, 2, expconf.GPUTopologySoft)
	resources := awaitAllocated(t, sub, drainScheduleWait)
	require.NotNil(t, resources)
	require.Equal(t, []int{2, 4}, deviceIDs(resources.devices))
	require.Equal(t,
		"GPU topology preference: agent "+string(h.agent.id)+", slots 2,4; worst pair NODE, P2P not usable",
		awaitTaskLog(t, sub, drainScheduleWait))

	// A topology the master has not received yet: the task log says so.
	h = newGPUHarness(t, node02, priorityScheduler42(best, nil))
	h.agent.mu.Lock()
	h.agent.agentState.setGPUTopology(nil)
	h.agent.mu.Unlock()
	sub = h.request(t, 2, expconf.GPUTopologySoft)
	resources = awaitAllocated(t, sub, drainScheduleWait)
	require.NotNil(t, resources)
	require.Equal(t, []int{0, 1}, deviceIDs(resources.devices))
	require.Equal(t, "GPU topology preference: agent "+string(h.agent.id)+" not ranked (topology "+
		"unknown: not reported since the master started); slots chosen as for tasks without it",
		awaitTaskLog(t, sub, drainScheduleWait))

	// A plain task gets no line.
	sub = h.request(t, 2, "")
	require.NotNil(t, awaitAllocated(t, sub, drainScheduleWait))
	require.Empty(t, awaitTaskLog(t, sub, drainPendingWait))
}
