//go:build integration

package agentrm

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task/taskmodel"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// TestGetJobQPlacementAfterRestore: after a master restart, a restored allocation lists the
// devices of its container snapshot at once.
func TestGetJobQPlacementAfterRestore(t *testing.T) {
	database, cleanup := db.MustResolveNewPostgresDatabase(t)
	defer cleanup()
	db.MustMigrateTestPostgres(t, database, "file://../../../static/migrations", "up")
	ctx := context.Background()

	// An agent ran a container with two GPUs of an allocation when the master stopped.
	taskID := model.TaskID(uuid.NewString())
	require.NoError(t, db.AddTask(ctx, &model.Task{
		TaskID: taskID, TaskType: model.TaskTypeCommand, StartTime: time.Now(),
		LogVersion: model.CurrentTaskLogVersion,
	}))
	allocationID := model.AllocationID(uuid.NewString())
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: allocationID, TaskID: taskID, Slots: 2, ResourcePool: "default",
		StartTime: ptrs.Ptr(time.Now()), State: ptrs.Ptr(model.AllocationStateRunning),
	}))
	containerID := cproto.ID(uuid.NewString())
	gpu := func(id device.ID) device.Device {
		return device.Device{ID: id, Brand: "nvidia", UUID: uuid.NewString(), Type: device.CUDA}
	}
	held := []device.Device{gpu(3), gpu(1)}
	_, err := db.Bun().NewInsert().Model(&taskmodel.ResourcesWithState{
		ResourceID: sproto.ResourcesID(containerID), AllocationID: allocationID,
	}).Exec(ctx)
	require.NoError(t, err)
	_, err = db.Bun().NewInsert().Model(&containerSnapshot{
		ResourceID: sproto.ResourcesID(containerID), AgentID: "restored-agent", ID: containerID,
		State: cproto.Running, Devices: held,
	}).Exec(ctx)
	require.NoError(t, err)
	slots := []slotData{{Device: gpu(0), UserEnabled: true}, {Device: gpu(2), UserEnabled: true}}
	for _, d := range held {
		slots = append(slots, slotData{Device: d, UserEnabled: true, ContainerID: &containerID})
	}
	_, err = db.Bun().NewInsert().Model(&agentSnapshot{
		AgentID: "restored-agent", UUID: uuid.NewString(), ResourcePoolName: "default",
		UserEnabled: true, Containers: []cproto.ID{containerID}, Slots: slots,
	}).Exec(ctx)
	require.NoError(t, err)

	// The agent has not reconnected yet: it keeps its containers while the master waits for it.
	rmConfig := testDynamicPoolRMConfig(42)
	rmConfig.ResourcePools[0].AgentReconnectWait = model.Duration(10 * time.Minute)
	masterDefaults := *model.DefaultTaskContainerDefaults()
	restarted, err := New(ctx, database, echo.New(), rmConfig, nil, nil, &masterDefaults)
	require.NoError(t, err)
	defer restarted.stop()

	// The allocation of a running job asks for its resources back.
	jobID := model.JobID(uuid.NewString())
	require.NoError(t, tasklist.GroupPriorityChangeRegistry.Add(jobID, nil))
	defer func() { require.NoError(t, tasklist.GroupPriorityChangeRegistry.Delete(jobID)) }()
	sub, err := restarted.Allocate(sproto.AllocateRequest{
		AllocationID: allocationID, TaskID: taskID, JobID: jobID, Name: "restored",
		SlotsNeeded: 2, ResourcePool: "default", IsUserVisible: true, Restore: true,
		JobSubmissionTime: time.Now(),
	})
	require.NoError(t, err)
	defer sub.Close()
	waitCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for allocated := false; !allocated; {
		ev, err := sub.GetWithContext(waitCtx)
		require.NoError(t, err, "the allocation was not restored")
		_, allocated = ev.(*sproto.ResourcesAllocated)
	}

	jobQ, err := restarted.GetJobQ("default")
	require.NoError(t, err)
	require.Contains(t, jobQ, jobID)
	require.Len(t, jobQ[jobID].Placement, 1)
	require.ElementsMatch(t, []device.ID{1, 3}, jobQ[jobID].Placement[aproto.ID("restored-agent")])
}

// requestJob asks the harness's pool for slots for the one user-visible task of a new job, as a
// command does, and returns the job's ID, the task's allocation ID and its subscription.
func (h *gpuHarness) requestJob(
	t *testing.T, slots int, pref expconf.GPUTopologyPreference,
) (model.JobID, model.AllocationID, *sproto.ResourcesSubscription) {
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
		SlotsNeeded: slots, ResourcePool: h.pool, IsUserVisible: true, JobSubmissionTime: time.Now(),
		FittingRequirements: sproto.FittingRequirements{SingleAgent: true, GPUTopology: pref},
	})
	require.NoError(t, err)
	t.Cleanup(sub.Close)
	return jobID, allocationID, sub
}

// jobQ returns the pool's job queue, each agent's device IDs ascending.
func (h *gpuHarness) jobQ(t *testing.T) map[model.JobID]*sproto.RMJobInfo {
	t.Helper()
	jobQ, err := h.rm.GetJobQ(rm.ResourcePoolName(h.pool))
	require.NoError(t, err)
	for _, info := range jobQ {
		for _, ids := range info.Placement {
			slices.Sort(ids)
		}
	}
	return jobQ
}

// heldBy returns the IDs of the live agent's devices that the container holds, ascending.
func heldBy(a *agent, containerID cproto.ID) []device.ID {
	a.mu.Lock()
	defer a.mu.Unlock()
	var ids []device.ID
	for d, holder := range a.agentState.Devices {
		if holder != nil && *holder == containerID {
			ids = append(ids, d.ID)
		}
	}
	slices.Sort(ids)
	return ids
}

// TestLivePlacementUnderNUMAPacking: through the resource manager, under best with NUMA packing,
// the job queue lists the devices that each job's reservation took on the live agent. Five 1-GPU
// jobs take slots 0-4, and those on 1-3 end. A strong 4-GPU job waits and lists none while a plain
// 4-GPU job starts across the two NUMA nodes; once the plain job and the job on slot 4 end, the
// strong job lists NUMA node 1.
func TestLivePlacementUnderNUMAPacking(t *testing.T) {
	h := newGPUHarness(t, node02, priorityScheduler42(best, nil))
	jobs := map[int]model.JobID{}
	allocations := map[int]model.AllocationID{}
	for want := 0; want < 5; want++ {
		jobID, id, sub := h.requestJob(t, 1, "")
		resources := awaitAllocated(t, sub, drainScheduleWait)
		require.NotNil(t, resources)
		require.Equal(t, []int{want}, deviceIDs(resources.devices))
		jobs[want], allocations[want] = jobID, id
	}
	for _, slot := range []int{1, 2, 3} {
		h.release(allocations[slot])
	}

	strongJob, _, strong := h.requestJob(t, 4, expconf.GPUTopologyStrong)
	require.Equal(t, strongWaitingLine(h.pool, "4"), awaitTaskLog(t, strong, drainScheduleWait))
	plainJob, plainID, sub := h.requestJob(t, 4, "")
	plain := awaitAllocated(t, sub, drainScheduleWait)
	require.NotNil(t, plain)

	jobQ := h.jobQ(t)
	require.Len(t, jobQ, 4)
	for _, slot := range []int{0, 4} {
		require.Equal(t, map[aproto.ID][]device.ID{h.agent.id: {device.ID(slot)}}, jobQ[jobs[slot]].Placement)
	}
	held := heldBy(h.agent, plain.containerID)
	require.Len(t, held, 4)
	require.Equal(t, map[aproto.ID][]device.ID{h.agent.id: held}, jobQ[plainJob].Placement)
	require.Contains(t, jobQ, strongJob)
	require.Equal(t, sproto.SchedulingStateQueued, jobQ[strongJob].State)
	require.Nil(t, jobQ[strongJob].Placement)

	h.release(plainID)
	h.release(allocations[4])
	resources := awaitAllocated(t, strong, drainScheduleWait)
	require.NotNil(t, resources)
	jobQ = h.jobQ(t)
	require.Equal(t, map[aproto.ID][]device.ID{h.agent.id: {4, 5, 6, 7}}, jobQ[strongJob].Placement)
	require.Equal(t, heldBy(h.agent, resources.containerID), jobQ[strongJob].Placement[h.agent.id])
}

// TestLivePlacementOfSoftAgentChoice: through the resource manager, on the layout of
// TestLiveSoftAgentChoice (node03 with slots 0, 1 and 4 disabled, node04 with slot 0 disabled), a
// soft 4-GPU job lists NUMA node 1 of node04, where the agent choice put it, and a plain 4-GPU job
// then lists node03, with both schedulers.
func TestLivePlacementOfSoftAgentChoice(t *testing.T) {
	fairShare := &config.SchedulerConfig{FairShare: &config.FairShareSchedulerConfig{}, FittingPolicy: best}
	for name, scheduler := range map[string]*config.SchedulerConfig{
		"priority":   priorityScheduler42(best, nil),
		"fair share": fairShare,
	} {
		layout := clusterNode(node02IDs)
		h := newGPUHarness(t, layout, scheduler)
		node04 := h.addGPUAgent(t, aproto.ID(h.pool+"-node04"), layout)
		disableSlots(t, h.agent, 0, 1, 4)
		disableSlots(t, node04, 0)

		softJob, _, sub := h.requestJob(t, 4, expconf.GPUTopologySoft)
		soft := awaitAllocated(t, sub, drainScheduleWait)
		require.NotNil(t, soft, name)
		plainJob, _, sub := h.requestJob(t, 4, "")
		plain := awaitAllocated(t, sub, drainScheduleWait)
		require.NotNil(t, plain, name)

		jobQ := h.jobQ(t)
		require.Equal(t, map[aproto.ID][]device.ID{node04.id: {4, 5, 6, 7}}, jobQ[softJob].Placement, name)
		require.Equal(t, heldBy(node04, soft.containerID), jobQ[softJob].Placement[node04.id], name)
		require.Equal(t, map[aproto.ID][]device.ID{h.agent.id: {2, 5, 6, 7}}, jobQ[plainJob].Placement, name)
		require.Equal(t, heldBy(h.agent, plain.containerID), jobQ[plainJob].Placement[h.agent.id], name)
	}
}
