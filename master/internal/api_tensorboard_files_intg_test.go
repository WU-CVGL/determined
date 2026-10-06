//go:build integration
// +build integration

package internal

import (
	"context"
	"strconv"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	authz2 "github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks/allocationmocks"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// mockGCAllocations stands in for the allocation service until the test ends. Each allocation
// that it is asked to start ends at once.
func mockGCAllocations(t *testing.T) *allocationmocks.AllocationService {
	as := &allocationmocks.AllocationService{}
	as.On("StartAllocation", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		args.Get(5).(func(*task.AllocationExited))(&task.AllocationExited{
			FinalState: task.AllocationState{State: model.AllocationStateTerminated},
		})
	})
	prev := task.DefaultService
	task.DefaultService = as
	t.Cleanup(func() { task.DefaultService = prev })
	return as
}

// checkpointGCTasks counts the checkpoint GC tasks started for the experiment.
func checkpointGCTasks(t *testing.T, exp *model.Experiment) int {
	n, err := db.Bun().NewSelect().Table("tasks").
		Where("job_id = ? AND task_type = ?", exp.JobID, model.TaskTypeCheckpointGC).
		Count(context.TODO())
	require.NoError(t, err)
	return n
}

// requireGCTasksStarted runs call and checks that it starts n checkpoint GC tasks for exp.
func requireGCTasksStarted(
	t *testing.T, as *allocationmocks.AllocationService, exp *model.Experiment, n int, call func(),
) {
	tasksBefore, allocationsBefore := checkpointGCTasks(t, exp), len(as.Calls)
	call()
	require.Equal(t, n, checkpointGCTasks(t, exp)-tasksBefore, "checkpoint GC tasks started")
	require.Equal(t, n, len(as.Calls)-allocationsBefore, "allocations started")
}

func TestDeleteTensorboardFilesOwnerOrAdmin(t *testing.T) {
	api, admin, _ := setupAPITest(t, nil)
	owner := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)
	exp := createTestExp(t, api, owner)
	allocations := mockGCAllocations(t)
	deleteAs := func(u model.User, expID int) error {
		_, err := api.DeleteTensorboardFiles(ntscUserCtx(t, u),
			&apiv1.DeleteTensorboardFilesRequest{ExperimentId: int32(expID)})
		return err
	}

	// Every user can view the experiment under basic authz, but only its owner or an admin may
	// delete its files.
	t.Run("another user", func(t *testing.T) {
		requireGCTasksStarted(t, allocations, exp, 0, func() {
			err := deleteAs(other, exp.ID)
			require.Equal(t, codes.PermissionDenied, status.Code(err), err)
		})
	})

	t.Run("missing experiment", func(t *testing.T) {
		err := deleteAs(other, -999)
		require.Equal(t, apiPkg.NotFoundErrs("experiment", "-999", true), err)
	})

	for name, u := range map[string]model.User{"owner": owner, "admin": admin} {
		t.Run(name, func(t *testing.T) {
			requireGCTasksStarted(t, allocations, exp, 1, func() {
				require.NoError(t, deleteAs(u, exp.ID))
			})
		})
	}
}

func TestAuthZDeleteTensorboardFiles(t *testing.T) {
	api, authZExp, _, curUser, ctx := setupExpAuthTest(t, nil)
	exp := createTestExp(t, api, curUser)
	allocations := mockGCAllocations(t)
	mockUserArg := mock.MatchedBy(func(u model.User) bool {
		return u.ID == curUser.ID
	})
	mockExpArg := mock.MatchedBy(func(e *model.Experiment) bool {
		return e.ID == exp.ID
	})
	req := &apiv1.DeleteTensorboardFilesRequest{ExperimentId: int32(exp.ID)}

	t.Run("cannot view", func(t *testing.T) {
		// The same answer as for a missing experiment.
		authZExp.On("CanGetExperiment", mock.Anything, mockUserArg, mockExpArg).
			Return(authz2.PermissionDeniedError{}).Once()
		requireGCTasksStarted(t, allocations, exp, 0, func() {
			_, err := api.DeleteTensorboardFiles(ctx, req)
			require.Equal(t, apiPkg.NotFoundErrs("experiment", strconv.Itoa(exp.ID), true), err)
		})
	})

	t.Run("can view, cannot edit", func(t *testing.T) {
		authZExp.On("CanGetExperiment", mock.Anything, mockUserArg, mockExpArg).Return(nil).Once()
		authZExp.On("CanEditExperiment", mock.Anything, mockUserArg, mockExpArg).
			Return(authz2.PermissionDeniedError{}).Once()
		requireGCTasksStarted(t, allocations, exp, 0, func() {
			_, err := api.DeleteTensorboardFiles(ctx, req)
			require.Equal(t, codes.PermissionDenied, status.Code(err), err)
		})
	})

	t.Run("can edit", func(t *testing.T) {
		authZExp.On("CanGetExperiment", mock.Anything, mockUserArg, mockExpArg).Return(nil).Once()
		authZExp.On("CanEditExperiment", mock.Anything, mockUserArg, mockExpArg).Return(nil).Once()
		requireGCTasksStarted(t, allocations, exp, 1, func() {
			_, err := api.DeleteTensorboardFiles(ctx, req)
			require.NoError(t, err)
		})
	})
}
