//go:build integration
// +build integration

package internal

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// stoppingMasterAllocations is the allocation service of masters that stop: their allocations do
// not exit, so the allocations' exit callbacks never run.
type stoppingMasterAllocations struct{ task.AllocationService }

func (s stoppingMasterAllocations) StartAllocation(
	logCtx logger.Context, req sproto.AllocateRequest, database db.DB,
	manager rm.ResourceManager, specifier tasks.TaskSpecifier, _ func(*task.AllocationExited),
) error {
	return s.AllocationService.StartAllocation(
		logCtx, req, database, manager, specifier, func(*task.AllocationExited) {},
	)
}

// A generic task that waits for resources when the master stops still waits after the master
// restarts, with its allocation, job, submission time, and priority, and starts when it gets
// resources.
func TestRestoreQueuedGenericTask(t *testing.T) {
	// The restore picks up every generic task in the database that has not ended.
	pgDB, dropDB := db.MustResolveNewPostgresDatabase(t)
	t.Cleanup(dropDB)
	db.MustMigrateTestPostgres(t, pgDB, "file://../static/migrations")
	require.NoError(t, etc.SetRootPath("../static/srv"))
	api, _, ctx := setupAPITest(t, pgDB)
	allocations := task.DefaultService
	task.DefaultService = stoppingMasterAllocations{allocations}
	t.Cleanup(func() { task.DefaultService = allocations })

	resp, err := api.CreateGenericTask(ctx, &apiv1.CreateGenericTaskRequest{
		Config: "entrypoint: [\"true\"]\nresources:\n  slots: 1\n",
	})
	require.NoError(t, err)
	taskID := model.TaskID(resp.TaskId)
	var snapshot command.CommandSnapshot
	require.NoError(t, db.Bun().NewSelect().Model(&snapshot).Where("task_id = ?", taskID).Scan(ctx))
	allocationID := snapshot.AllocationID
	jobID := snapshot.GenericTaskSpec.JobID
	t.Cleanup(func() {
		if slices.Contains(task.DefaultService.GetAllAllocationIDs(), allocationID) {
			_ = task.DefaultService.Detach(allocationID)
			require.Eventually(t, func() bool {
				return !slices.Contains(task.DefaultService.GetAllAllocationIDs(), allocationID)
			}, 5*time.Second, 10*time.Millisecond)
		}
		unregisterGenericTaskJob(jobID, allocationID)
	})
	state, err := task.DefaultService.State(allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStatePending, state.State)

	// The master stops: the allocation is left as it is. A new master starts with new services.
	require.NoError(t, task.DefaultService.Detach(allocationID))
	require.Eventually(t, func() bool {
		return !slices.Contains(task.DefaultService.GetAllAllocationIDs(), allocationID)
	}, 5*time.Second, 10*time.Millisecond)
	unregisterGenericTaskJob(jobID, allocationID)

	restartedRM := MockRM()
	restartedRM.On("Release", mock.Anything).Return()
	api.m.rm = restartedRM
	require.NoError(t, api.m.restoreGenericTasks(context.Background()))

	resourcesConfig := snapshot.GenericTaskSpec.GenericTaskConfig.Resources
	restartedRM.AssertCalled(t, "SetGroupPriority", sproto.SetGroupPriority{
		Priority: *resourcesConfig.Priority(), ResourcePool: resourcesConfig.ResourcePool(), JobID: jobID,
	})
	restartedRM.AssertCalled(t, "Allocate", mock.MatchedBy(func(req sproto.AllocateRequest) bool {
		return req.AllocationID == allocationID && req.JobID == jobID &&
			req.JobSubmissionTime.Equal(snapshot.RegisteredTime) && !req.Restore
	}))
	state, err = task.DefaultService.State(allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStatePending, state.State)

	// It gets resources: it starts on them.
	var started atomic.Bool
	rID := sproto.ResourcesID(cproto.NewID())
	var resources mocks.Resources
	resources.On("Summary").Return(sproto.ResourcesSummary{
		AllocationID:  allocationID,
		ResourcesID:   rID,
		ResourcesType: sproto.ResourcesTypeDockerContainer,
		AgentDevices:  map[aproto.ID][]device.Device{"agent": nil},
	})
	resources.On("Start", mock.Anything, mock.Anything, mock.Anything).Return(nil).
		Run(func(mock.Arguments) { started.Store(true) })
	resources.On("Kill", mock.Anything).Return()
	rmevents.Publish(allocationID, &sproto.ResourcesAllocated{
		ID:                allocationID,
		ResourcePool:      resourcesConfig.ResourcePool(),
		Resources:         map[sproto.ResourcesID]sproto.Resources{rID: &resources},
		JobSubmissionTime: snapshot.RegisteredTime,
	})
	require.Eventually(t, started.Load, 5*time.Second, 10*time.Millisecond)
	state, err = task.DefaultService.State(allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStateAssigned, state.State)
}
