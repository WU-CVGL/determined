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
	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

func TestJobQueueUpdatesPreflightEveryOwner(t *testing.T) {
	_, _, ctx := setupAPITest(t, nil)
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
	// The RBAC provider still relies on the existing queue-level permission.
	require.NoError(t, (&job.JobAuthZRBAC{}).CanControlJobQueueUpdate(
		context.Background(), owner, foreignJob))
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
