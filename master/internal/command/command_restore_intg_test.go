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
	manager.On("Release", mock.Anything).Return()
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

// A shell that waits for resources when the master stops still waits after the master restarts,
// with its allocation, job, submission time, and priority, and starts when it gets resources.
func TestRestoreQueuedShell(t *testing.T) {
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
	q.Put(&sproto.ResourcesAllocated{
		ID:                shell.allocationID,
		ResourcePool:      shell.Config.Resources.ResourcePool,
		Resources:         map[sproto.ResourcesID]sproto.Resources{rID: &resources},
		JobSubmissionTime: shell.registeredTime,
	})
	require.Eventually(t, started.Load, 5*time.Second, 10*time.Millisecond)
	state, err = task.DefaultService.State(shell.allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStateAssigned, state.State)
}
