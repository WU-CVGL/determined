//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// continueTestUser is a user with its own agent user group, and a context that authenticates as it.
type continueTestUser struct {
	model.User
	group model.AgentUserGroup
	ctx   context.Context //nolint:containedctx
}

func addContinueTestUser(t *testing.T, admin bool, uid int) continueTestUser {
	t.Helper()
	ctx := context.Background()
	name := uuid.New().String()
	group := model.AgentUserGroup{
		User: fmt.Sprintf("agent-%d", uid), UID: uid,
		Group: fmt.Sprintf("group-%d", uid), GID: uid + 1,
	}
	id, err := user.Add(ctx, &model.User{Username: name, Active: true, Admin: admin}, &group)
	require.NoError(t, err)
	u, err := user.ByID(ctx, id)
	require.NoError(t, err)
	token, err := user.StartSession(ctx, ptrs.Ptr(u.ToUser()))
	require.NoError(t, err)
	return continueTestUser{
		User:  u.ToUser(),
		group: group,
		ctx: metadata.NewIncomingContext(context.Background(),
			metadata.Pairs("x-user-token", "Bearer "+token)),
	}
}

// endedTestExp creates an experiment that owner owns and that has ended, so it can be continued.
func endedTestExp(t *testing.T, api *apiServer, owner continueTestUser) int {
	t.Helper()
	exp := createTestExpWithProjectID(t, api, owner.User, 1)
	_, err := db.Bun().NewUpdate().Table("experiments").
		Set("state = ?", model.CompletedState).
		Where("id = ?", exp.ID).
		Exec(context.Background())
	require.NoError(t, err)
	return exp.ID
}

// requireRunsAs checks that the running experiment and each of its trials run as want: the task
// spec's owner, the user of its session token, and its agent user group. Like the other tests that
// activate experiments against the mock resource manager, it leaves the experiment running: killing
// it releases resources through calls that the mock does not support.
func requireRunsAs(t *testing.T, expID int, want continueTestUser) {
	t.Helper()
	ref, ok := expauth.ExperimentRegistry.Load(expID)
	require.True(t, ok, "experiment %d is not running", expID)
	e, ok := ref.(*internalExperiment)
	require.True(t, ok)

	e.mu.Lock()
	defer e.mu.Unlock()
	require.Equal(t, want.ID, *e.OwnerID, "experiment owner")
	require.Equal(t, want.Username, e.Username, "experiment owner's username")

	specs := map[string]*tasks.TaskSpec{"experiment": e.taskSpec}
	for _, tr := range e.trials {
		specs[fmt.Sprintf("trial %s", tr.taskID)] = tr.taskSpec
	}
	require.Len(t, specs, 2, "the experiment and its one trial")
	for name, spec := range specs {
		require.Equal(t, want.ID, spec.Owner.ID, "%s: task spec owner", name)
		require.Equal(t, want.Username, spec.Owner.Username, "%s: DET_USER", name)

		tokenUser, _, err := user.ByToken(context.Background(), spec.UserSessionToken,
			&model.ExternalSessions{})
		require.NoError(t, err, "%s: session token", name)
		require.Equal(t, want.ID, tokenUser.ID, "%s: user of the session token", name)

		require.NotNil(t, spec.AgentUserGroup, "%s: agent user group", name)
		require.Equal(t, want.group.UID, spec.AgentUserGroup.UID, "%s: uid", name)
		require.Equal(t, want.group.GID, spec.AgentUserGroup.GID, "%s: gid", name)
		require.Equal(t, want.group.User, spec.AgentUserGroup.User, "%s: agent user", name)
		require.Equal(t, want.group.Group, spec.AgentUserGroup.Group, "%s: agent group", name)
	}
}

func requireNotContinued(t *testing.T, expID int) {
	t.Helper()
	_, ok := expauth.ExperimentRegistry.Load(expID)
	require.False(t, ok, "experiment %d is running", expID)
	exp, err := db.ExperimentByID(context.Background(), expID)
	require.NoError(t, err)
	require.Equal(t, model.CompletedState, exp.State)
}

func TestContinueExperimentKeepsOwnerIdentity(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)
	owner := addContinueTestUser(t, false, 41000)
	admin := addContinueTestUser(t, true, 42000)

	t.Run("an administrator continues another user's experiment", func(t *testing.T) {
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(admin.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
	})

	t.Run("the owner continues their own experiment", func(t *testing.T) {
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
	})

	t.Run("a user who may not edit the experiment is refused", func(t *testing.T) {
		other := addContinueTestUser(t, false, 43000)
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(other.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.Equal(t, codes.PermissionDenied, status.Code(err), err)
		requireNotContinued(t, expID)
	})

	t.Run("an experiment of a deactivated user is not continued", func(t *testing.T) {
		gone := addContinueTestUser(t, false, 44000)
		expID := endedTestExp(t, api, gone)
		gone.Active = false
		require.NoError(t, user.Update(context.Background(), &gone.User, []string{"active"}, nil))

		_, err := api.ContinueExperiment(admin.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), err)
		require.ErrorContains(t, err, gone.Username)
		require.ErrorContains(t, err, "deactivated")
		requireNotContinued(t, expID)
	})

	t.Run("with external sessions, another user's experiment is not continued", func(t *testing.T) {
		ext := &config.GetMasterConfig().InternalConfig.ExternalSessions
		loginURI := ext.LoginURI
		ext.LoginURI = "https://login.example.com"
		t.Cleanup(func() { ext.LoginURI = loginURI })

		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(admin.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.Equal(t, codes.FailedPrecondition, status.Code(err), err)
		require.ErrorContains(t, err, "with external sessions")
		requireNotContinued(t, expID)
	})
}

// Under RBAC, a user who is not an administrator may continue another user's experiment when they
// may edit it. Access is checked for them, and the experiment still runs as its owner.
func TestContinueExperimentChecksActorRunsAsOwner(t *testing.T) {
	api, authZExp, projectAuthZ, _, _ := setupExpAuthTest(t, nil)
	owner := addContinueTestUser(t, false, 45000)
	actor := addContinueTestUser(t, false, 46000)
	expID := endedTestExp(t, api, owner)

	isUser := func(u continueTestUser) any {
		return mock.MatchedBy(func(m model.User) bool { return m.ID == u.ID })
	}
	isExp := mock.MatchedBy(func(e *model.Experiment) bool { return e.ID == expID })
	// The mocks are shared with other tests; these expectations match only this test's users.
	authZExp.On("CanGetExperiment", mock.Anything, isUser(actor), isExp).Return(nil)
	authZExp.On("CanEditExperiment", mock.Anything, isUser(actor), isExp).Return(nil)
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, isUser(actor), isExp).Return(nil)
	projectAuthZ.On("CanGetProject", mock.Anything, isUser(actor), mock.Anything).Return(nil)
	// The owner may not see the project, which does not matter: access is checked for the actor.
	projectAuthZ.On("CanGetProject", mock.Anything, isUser(owner), mock.Anything).
		Return(fmt.Errorf("the owner may not see the project")).Maybe()

	_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
		Id: int32(expID),
	})
	require.NoError(t, err)
	authZExp.AssertCalled(t, "CanEditExperiment", mock.Anything, isUser(actor), isExp)
	projectAuthZ.AssertCalled(t, "CanGetProject", mock.Anything, isUser(actor), mock.Anything)
	requireRunsAs(t, expID, owner)

	t.Run("a user who may not edit the experiment is refused", func(t *testing.T) {
		refused := addContinueTestUser(t, false, 47000)
		expID := endedTestExp(t, api, owner)
		isExp := mock.MatchedBy(func(e *model.Experiment) bool { return e.ID == expID })
		authZExp.On("CanGetExperiment", mock.Anything, isUser(refused), isExp).Return(nil)
		authZExp.On("CanEditExperiment", mock.Anything, isUser(refused), isExp).
			Return(fmt.Errorf("may not edit"))

		_, err := api.ContinueExperiment(refused.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.Equal(t, codes.PermissionDenied, status.Code(err), err)
		requireNotContinued(t, expID)
	})
}
