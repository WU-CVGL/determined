//go:build integration

package agentrm

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/labstack/echo/v4"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task/taskmodel"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
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
