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

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/aproto"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// restoreTestRM is a resource manager that hands every allocation the same event queue.
func restoreTestRM() (*mocks.ResourceManager, *queue.Queue[sproto.ResourcesEvent]) {
	var rm mocks.ResourceManager
	q := queue.New[sproto.ResourcesEvent]()
	rm.On("Allocate", mock.Anything).Return(sproto.NewAllocationSubscription(q, func() {}), nil)
	rm.On("Release", mock.Anything).Return()
	rm.On("SetGroupPriority", mock.Anything).Return(nil)
	rm.On("SmallerValueIsHigherPriority").Return(true, nil)
	return &rm, q
}

// A shell that waits for resources when the master stops still waits after the master restarts,
// with its allocation, job, and submission time, and starts when it gets resources.
func TestRestoreQueuedShell(t *testing.T) {
	pgDB := setupTest(t)
	rm, _ := restoreTestRM()
	cs, err := NewService(pgDB, rm)
	require.NoError(t, err)
	SetDefaultService(cs)

	shell, err := cs.LaunchGenericCommand(model.TaskTypeShell, model.JobTypeShell, CreateMockGenericReq(t, pgDB))
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

	rm, q := restoreTestRM()
	cs, err = NewService(pgDB, rm)
	require.NoError(t, err)
	SetDefaultService(cs)
	require.NoError(t, cs.RestoreAllCommands(context.Background()))
	restored := cs.commands[shell.taskID]
	require.NotNil(t, restored)
	t.Cleanup(func() { _ = task.DefaultService.Detach(shell.allocationID) })

	rm.AssertCalled(t, "Allocate", mock.MatchedBy(func(req sproto.AllocateRequest) bool {
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
		ID:           shell.allocationID,
		ResourcePool: shell.Config.Resources.ResourcePool,
		Resources:    map[sproto.ResourcesID]sproto.Resources{rID: &resources},
	})
	require.Eventually(t, started.Load, 5*time.Second, 10*time.Millisecond)
	state, err = task.DefaultService.State(shell.allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStateAssigned, state.State)
}
