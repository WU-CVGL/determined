//go:build integration
// +build integration

package command

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"golang.org/x/exp/slices"

	internaldb "github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// restoreTestRM is a resource manager that hands every allocation the same event queue.
func restoreTestRM() (*mocks.ResourceManager, *queue.Queue[sproto.ResourcesEvent]) {
	var manager mocks.ResourceManager
	q := queue.New[sproto.ResourcesEvent]()
	manager.On("Allocate", mock.Anything).Return(sproto.NewAllocationSubscription(q, func() {}), nil)
	manager.On("Release", mock.Anything).Return().Run(func(args mock.Arguments) {
		if args[0].(sproto.ResourcesReleased).ResourcesID == nil {
			q.Put(sproto.ResourcesReleasedEvent{})
		}
	})
	manager.On("SetGroupPriority", mock.Anything).Return(nil)
	manager.On("SmallerValueIsHigherPriority").Return(true, nil)
	return &manager, q
}

// stoppingMasterAllocations is the allocation service of masters that stop: their allocations do
// not exit, so the allocations' exit callbacks never run.
type stoppingMasterAllocations struct{ task.AllocationService }

func (s stoppingMasterAllocations) StartAllocation(
	logCtx logger.Context, req sproto.AllocateRequest, database internaldb.DB,
	manager rm.ResourceManager, specifier tasks.TaskSpecifier, _ func(*task.AllocationExited),
) error {
	return s.AllocationService.StartAllocation(
		logCtx, req, database, manager, specifier, func(*task.AllocationExited) {},
	)
}

// restartWithQueuedShell launches a shell that waits for resources, stops the master, and
// restores the shell in a new master as the master start does, up to closing the open
// allocations with the last cluster heartbeat at heartbeat.
func restartWithQueuedShell(t *testing.T, heartbeat time.Time) (
	*Command, *mocks.ResourceManager, *queue.Queue[sproto.ResourcesEvent],
) {
	// The restore picks up every command in the database that has not ended.
	newDB, dropDB := internaldb.MustResolveNewPostgresDatabase(t)
	t.Cleanup(func() {
		internaldb.MustResolveTestPostgres(t) // the package's other tests use its database
		dropDB()
	})
	internaldb.MustMigrateTestPostgres(t, newDB, "file://../../static/migrations")
	pgDB := setupTest(t)
	allocations := task.DefaultService
	task.DefaultService = stoppingMasterAllocations{allocations}
	t.Cleanup(func() { task.DefaultService = allocations })
	firstRM, _ := restoreTestRM()
	cs, err := NewService(pgDB, firstRM)
	require.NoError(t, err)
	SetDefaultService(cs)

	req := CreateMockGenericReq(t, pgDB)
	req.Spec.Config.Resources.Priority = ptrs.Ptr(7)
	shell, err := cs.LaunchGenericCommand(model.TaskTypeShell, model.JobTypeShell, req)
	require.NoError(t, err)
	state, err := task.DefaultService.State(shell.allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStatePending, state.State)

	// The master stops: the allocation is left as it is. A new master starts with new services.
	require.NoError(t, task.DefaultService.Detach(shell.allocationID))
	require.Eventually(t, func() bool {
		return !slices.Contains(task.DefaultService.GetAllAllocationIDs(), shell.allocationID)
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, tasklist.GroupPriorityChangeRegistry.Delete(shell.jobID))
	_, err = pgDB.GetOrCreateClusterID("")
	require.NoError(t, err)
	require.NoError(t, pgDB.UpdateClusterHeartBeat(heartbeat))

	restartedRM, q := restoreTestRM()
	cs, err = NewService(pgDB, restartedRM)
	require.NoError(t, err)
	SetDefaultService(cs)
	require.NoError(t, cs.RestoreAllCommands(context.Background()))
	restored := cs.commands[shell.taskID]
	require.NotNil(t, restored)
	t.Cleanup(func() {
		_ = task.DefaultService.Detach(shell.allocationID)
		_ = tasklist.GroupPriorityChangeRegistry.Delete(shell.jobID)
	})
	require.NoError(t, internaldb.CloseOpenAllocations(
		context.Background(), task.DefaultService.GetAllAllocationIDs(),
	))

	restartedRM.AssertCalled(t, "SetGroupPriority", sproto.SetGroupPriority{
		Priority: 7, ResourcePool: shell.Config.Resources.ResourcePool, JobID: shell.jobID,
	})
	restartedRM.AssertCalled(t, "Allocate", mock.MatchedBy(func(req sproto.AllocateRequest) bool {
		return req.AllocationID == shell.allocationID && req.JobID == shell.jobID &&
			req.JobSubmissionTime.Equal(shell.registeredTime) && !req.Restore
	}))
	state, err = task.DefaultService.State(shell.allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStatePending, state.State)
	require.Equal(t, taskv1.State_STATE_QUEUED, restored.ToV1Shell().State)
	row, err := internaldb.AllocationByID(context.Background(), shell.allocationID)
	require.NoError(t, err)
	require.Nil(t, row.StartTime)
	require.Nil(t, row.EndTime)
	return restored, restartedRM, q
}

// A shell that waits for resources when the master stops still waits after the master restarts,
// with its allocation, job, submission time, and priority, and starts when it gets resources.
func TestRestoreQueuedShell(t *testing.T) {
	shell, _, q := restartWithQueuedShell(t, time.Now().UTC().Add(-time.Hour))

	// It gets resources: it starts on them.
	var started atomic.Bool
	rID := sproto.ResourcesID(cproto.NewID())
	var resources mocks.Resources
	resources.On("Summary").Return(sproto.ResourcesSummary{
		AllocationID:  shell.allocationID,
		ResourcesID:   rID,
		ResourcesType: sproto.ResourcesTypeDockerContainer,
		AgentDevices:  map[aproto.ID][]device.Device{"agent": nil},
	})
	resources.On("Start", mock.Anything, mock.Anything, mock.Anything).Return(nil).
		Run(func(mock.Arguments) { started.Store(true) })
	resources.On("Kill", mock.Anything).Return()
	before := time.Now().UTC().Add(-time.Second)
	q.Put(&sproto.ResourcesAllocated{
		ID:                shell.allocationID,
		ResourcePool:      shell.Config.Resources.ResourcePool,
		Resources:         map[sproto.ResourcesID]sproto.Resources{rID: &resources},
		JobSubmissionTime: shell.registeredTime,
	})
	require.Eventually(t, started.Load, 5*time.Second, 10*time.Millisecond)
	state, err := task.DefaultService.State(shell.allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStateAssigned, state.State)

	// Its start is when its container starts.
	q.Put(&sproto.ResourcesStateChanged{ResourcesID: rID, ResourcesState: sproto.Pulling})
	var row *model.Allocation
	require.Eventually(t, func() bool {
		row, err = internaldb.AllocationByID(context.Background(), shell.allocationID)
		require.NoError(t, err)
		return row.StartTime != nil
	}, 5*time.Second, 10*time.Millisecond)
	require.True(t, row.StartTime.After(before), "start %s", row.StartTime)
}

// A shell killed while it waits for resources after a master restart records no usage.
func TestRestoreQueuedShellKilled(t *testing.T) {
	shell, _, _ := restartWithQueuedShell(t, time.Now().UTC().Add(-time.Hour))

	require.NoError(t, task.DefaultService.Signal(shell.allocationID, task.KillAllocation, "killed"))
	require.Eventually(t, func() bool {
		return !slices.Contains(task.DefaultService.GetAllAllocationIDs(), shell.allocationID)
	}, 5*time.Second, 10*time.Millisecond)
	row, err := internaldb.AllocationByID(context.Background(), shell.allocationID)
	require.NoError(t, err)
	require.Nil(t, row.StartTime)
	require.Nil(t, row.EndTime)

	// The next master start closes it without a duration.
	heartbeat := time.Now().UTC().Truncate(time.Millisecond)
	require.NoError(t, internaldb.SingleDB().UpdateClusterHeartBeat(heartbeat))
	require.NoError(t, internaldb.CloseOpenAllocations(context.Background(), nil))
	row, err = internaldb.AllocationByID(context.Background(), shell.allocationID)
	require.NoError(t, err)
	require.NotNil(t, row.StartTime)
	require.NotNil(t, row.EndTime)
	require.True(t, heartbeat.Equal(*row.StartTime), "start %s", row.StartTime)
	require.True(t, heartbeat.Equal(*row.EndTime), "end %s", row.EndTime)
}
