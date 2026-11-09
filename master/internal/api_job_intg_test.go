//go:build integration
// +build integration

package internal

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/internal/rbac"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
	"github.com/determined-ai/determined/proto/pkg/rbacv1"
)

func TestJobQueueUpdatesPreflightEveryOwner(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	owner := db.RequireMockUser(t, db.SingleDB())
	other := db.RequireMockUser(t, db.SingleDB())
	ownedJob := db.RequireMockJob(t, db.SingleDB(), &owner.ID)
	foreignJob := db.RequireMockJob(t, db.SingleDB(), &other.ID)
	ownerlessJob := db.RequireMockJob(t, db.SingleDB(), nil)
	priority := func(id model.JobID) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_Priority{Priority: 10}}
	}
	resourcePool := func(id model.JobID) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_ResourcePool{ResourcePool: "other"}}
	}
	weight := func(id model.JobID) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_Weight{Weight: 2}}
	}
	authorize := job.AuthZProvider.Get().CanControlJobQueueUpdate
	applied := 0
	apply := func(updates []*jobv1.QueueControl) error {
		applied++
		return nil
	}
	for _, denied := range []*jobv1.QueueControl{priority(foreignJob), resourcePool(foreignJob),
		weight(foreignJob), priority(ownerlessJob)} {
		err := updateJobQueueAuthorized(ctx, owner,
			[]*jobv1.QueueControl{priority(ownedJob), denied}, authorize, apply)
		require.Equal(t, codes.PermissionDenied, status.Code(err))
		require.Zero(t, applied, "the authorized first job must not be mutated")
	}
	require.NoError(t, updateJobQueueAuthorized(ctx, owner,
		[]*jobv1.QueueControl{priority(ownedJob)}, authorize, apply))
	require.Equal(t, 1, applied)
	admin := model.User{ID: other.ID, Admin: true}
	require.NoError(t, updateJobQueueAuthorized(ctx, admin,
		[]*jobv1.QueueControl{priority(ownedJob), resourcePool(ownerlessJob)}, authorize, apply))
	require.Equal(t, 2, applied)

	// Under RBAC, a user without UPDATE_EXPERIMENT in the workspace of another user's experiment
	// may not change its job.
	useRBACForJobQueueTest(t)
	foreignExp := createTestExpWithProjectID(t, api, other, 1)
	err := updateJobQueueAuthorized(ctx, owner, []*jobv1.QueueControl{priority(foreignExp.JobID)},
		job.AuthZProvider.Get().CanControlJobQueueUpdate, apply)
	require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
	require.Equal(t, 2, applied)
}

// useRBACForJobQueueTest makes every authz provider the RBAC one for the rest of the test.
func useRBACForJobQueueTest(t *testing.T) {
	t.Helper()
	previous := config.GetMasterConfig().Security.AuthZ
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "rbac"}
	t.Cleanup(func() { config.GetMasterConfig().Security.AuthZ = previous })
}

// Under RBAC, the Editor role in a workspace allows priority, weight and pool updates of a
// colleague's jobs in that workspace only, and a batch with a job outside it applies nothing.
func TestUpdateJobQueueRBACChecksEachJobWorkspace(t *testing.T) {
	api, admin, ctx := setupAPITest(t, nil)
	open := accessTestPool(t, "open", admin, false)
	useRBACForJobQueueTest(t)
	colleague := db.RequireMockUser(t, db.SingleDB())
	editor := db.RequireMockUser(t, db.SingleDB())
	wsA, _ := db.RequireMockWorkspaceID(t, db.SingleDB(), "")
	wsB, _ := db.RequireMockWorkspaceID(t, db.SingleDB(), "")
	projectA, _ := db.RequireMockProjectID(t, db.SingleDB(), wsA, false)
	projectB, _ := db.RequireMockProjectID(t, db.SingleDB(), wsB, false)
	var editorRole int32
	require.NoError(t, db.Bun().NewRaw("SELECT id FROM roles WHERE role_name = 'Editor'").
		Scan(ctx, &editorRole))
	require.NoError(t, rbac.AddRoleAssignments(ctx, nil, []*rbacv1.UserRoleAssignment{{
		UserId: int32(editor.ID),
		RoleAssignment: &rbacv1.RoleAssignment{
			Role: &rbacv1.Role{RoleId: editorRole}, ScopeWorkspaceId: ptrs.Ptr(int32(wsA)),
		},
	}}))
	genericJob := func(workspaceID int) model.JobID {
		taskID := addGenericTaskForAuthZTest(ctx, t, colleague, workspaceID, nil, model.TaskStateActive)
		task, err := db.TaskByID(ctx, taskID)
		require.NoError(t, err)
		return *task.JobID
	}
	expA := createTestExpWithProjectID(t, api, colleague, projectA).JobID
	expB := createTestExpWithProjectID(t, api, colleague, projectB).JobID
	genericA, genericB := genericJob(wsA), genericJob(wsB)

	priority := func(id model.JobID) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_Priority{Priority: 10}}
	}
	weight := func(id model.JobID) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_Weight{Weight: 2}}
	}
	move := func(id model.JobID) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_ResourcePool{ResourcePool: open}}
	}
	authorize := job.AuthZProvider.Get().CanControlJobQueueUpdate
	var applied [][]*jobv1.QueueControl
	apply := func(updates []*jobv1.QueueControl) error {
		applied = append(applied, updates)
		return nil
	}

	for _, action := range []func(model.JobID) *jobv1.QueueControl{priority, weight, move} {
		for _, denied := range []model.JobID{expB, genericB} {
			err := updateJobQueueAuthorized(ctx, editor,
				[]*jobv1.QueueControl{action(expA), action(genericA), action(denied)}, authorize, apply)
			require.Equal(t, codes.PermissionDenied, status.Code(err), "%v", err)
			require.Empty(t, applied, "no update of a refused batch is applied")
		}
	}
	require.NoError(t, updateJobQueueAuthorized(ctx, editor, []*jobv1.QueueControl{
		priority(expA), weight(expA), move(expA), priority(genericA), weight(genericA), move(genericA),
	}, authorize, apply))
	require.Len(t, applied, 1)
}

// Moving a job to another pool is an admission into the target pool; priority and weight changes
// manage accepted work and are not checked.
func TestUpdateJobQueuePoolMoveChecksTarget(t *testing.T) {
	_, admin, ctx := setupAPITest(t, nil)
	owner := db.RequireMockUser(t, db.SingleDB())
	ownedJob := db.RequireMockJob(t, db.SingleDB(), &owner.ID)
	restricted := accessTestPool(t, "restricted", admin, true)
	open := accessTestPool(t, "open", admin, false)
	move := func(pool string) *jobv1.QueueControl {
		return &jobv1.QueueControl{
			JobId: ownedJob.String(), Action: &jobv1.QueueControl_ResourcePool{ResourcePool: pool},
		}
	}
	priority := &jobv1.QueueControl{
		JobId: ownedJob.String(), Action: &jobv1.QueueControl_Priority{Priority: 10},
	}
	weight := &jobv1.QueueControl{
		JobId: ownedJob.String(), Action: &jobv1.QueueControl_Weight{Weight: 2},
	}
	authorize := job.AuthZProvider.Get().CanControlJobQueueUpdate
	var applied [][]*jobv1.QueueControl
	apply := func(updates []*jobv1.QueueControl) error {
		applied = append(applied, updates)
		return nil
	}

	err := updateJobQueueAuthorized(ctx, owner,
		[]*jobv1.QueueControl{priority, move(restricted)}, authorize, apply)
	requirePoolDenied(t, err, owner, restricted)
	require.Empty(t, applied, "no update of a refused batch is applied")

	err = updateJobQueueAuthorized(ctx, owner, []*jobv1.QueueControl{priority, move("")}, authorize, apply)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Equal(t, "moving a job to another resource pool requires the target pool name",
		status.Convert(err).Message())
	require.Empty(t, applied)

	// Priority and weight updates are not checked, whatever pools are restricted.
	require.NoError(t, updateJobQueueAuthorized(ctx, owner,
		[]*jobv1.QueueControl{priority, weight}, authorize, apply))
	require.NoError(t, updateJobQueueAuthorized(ctx, owner,
		[]*jobv1.QueueControl{move(open)}, authorize, apply))
	require.NoError(t, updateJobQueueAuthorized(ctx, admin,
		[]*jobv1.QueueControl{move(restricted)}, authorize, apply))
	require.Len(t, applied, 3)

	_, err = poolaccess.Grant(ctx, restricted, []model.UserID{owner.ID}, admin.ID)
	require.NoError(t, err)
	require.NoError(t, updateJobQueueAuthorized(ctx, owner,
		[]*jobv1.QueueControl{move(restricted)}, authorize, apply))
	require.Len(t, applied, 4)
}

// A generic task's invalid priority or weight in a job queue update is the caller's error (HTTP
// 400), as at creation. A failure of the master, and the refusals of other job types, keep their
// status.
func TestUpdateJobQueueRefusesInvalidGenericTaskUpdatesAsClientErrors(t *testing.T) {
	mockRM := MockRM()
	mockRM.On("SetGroupPriority", mock.MatchedBy(func(msg sproto.SetGroupPriority) bool {
		return msg.Priority == 60
	})).Return(errors.New("resource manager unavailable"))
	// The mock answers with the first expectation that matches.
	calls := mockRM.ExpectedCalls
	mockRM.ExpectedCalls = append([]*mock.Call{calls[len(calls)-1]}, calls[:len(calls)-1]...)
	api, owner, ctx := setupAPITest(t, nil, mockRM)
	_, jobID, _ := addGenericTaskJobForTest(ctx, t, api, owner, "queued")

	admin, err := user.ByUsername(ctx, "admin")
	require.NoError(t, err)
	require.NoError(t, configpolicy.SetTaskConfigPolicies(ctx, &model.TaskConfigPolicies{
		WorkloadType: model.NTSCType, LastUpdatedBy: admin.ID,
		Constraints: ptrs.Ptr(`{"priority_limit": 42}`),
	}))
	t.Cleanup(func() {
		require.NoError(t, configpolicy.DeleteConfigPolicies(context.Background(), nil, model.NTSCType))
	})

	update := func(updates ...*jobv1.QueueControl) error {
		_, err := api.UpdateJobQueue(ctx, &apiv1.UpdateJobQueueRequest{Updates: updates})
		return err
	}
	priority := func(id model.JobID, priority int32) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_Priority{Priority: priority}}
	}
	weight := func(id model.JobID, weight float32) *jobv1.QueueControl {
		return &jobv1.QueueControl{JobId: id.String(), Action: &jobv1.QueueControl_Weight{Weight: weight}}
	}

	// A smaller value is a higher priority with the mock, so 1 is beyond the limit of 42.
	for _, invalid := range []*jobv1.QueueControl{
		priority(jobID, 0), priority(jobID, 100), priority(jobID, 1), weight(jobID, 0), weight(jobID, -1),
	} {
		require.Equal(t, codes.InvalidArgument, status.Code(update(invalid)), "%v", invalid)
	}
	require.Equal(t, codes.InvalidArgument, status.Code(update(priority(jobID, 0), weight(jobID, 0))))
	require.NoError(t, update(priority(jobID, 50), weight(jobID, 2)))

	err = update(priority(jobID, 60))
	require.ErrorContains(t, err, "resource manager unavailable")
	require.Equal(t, codes.Unknown, status.Code(err))

	// A command refuses an invalid priority with an error without a status, as before.
	commandJobID := db.RequireMockJob(t, db.SingleDB(), &owner.ID)
	jobservice.DefaultService.RegisterJob(commandJobID, &command.Command{})
	err = update(priority(commandJobID, 0))
	require.ErrorContains(t, err, "priority must be between 1 and 99")
	require.Equal(t, codes.Unknown, status.Code(err))
	require.Equal(t, codes.Unknown, status.Code(update(priority(commandJobID, 0), priority(jobID, 0))))
}
