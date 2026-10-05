//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
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
	return endTestExp(t, createTestExpWithProjectID(t, api, owner.User, 1).ID)
}

func endTestExp(t *testing.T, expID int) int {
	t.Helper()
	_, err := db.Bun().NewUpdate().Table("experiments").
		Set("state = ?", model.CompletedState).
		Where("id = ?", expID).
		Exec(context.Background())
	require.NoError(t, err)
	return expID
}

// codeTestConfig gives an experiment an image, environment variables, a pod spec and a bind mount,
// so that a continue that sends the whole config back, as the WebUI does, merges each of them.
const codeTestConfig = `
environment:
  image: owner/image:1
  environment_variables:
    - A=1
    - B=2
  pod_spec:
    metadata:
      labels:
        team: owner
bind_mounts:
  - host_path: /data
    container_path: /data
`

// endedCodeTestExp is endedTestExp with codeTestConfig.
func endedCodeTestExp(t *testing.T, api *apiServer, owner continueTestUser) int {
	t.Helper()
	cfg, err := expconf.ParseAnyExperimentConfigYAML([]byte(codeTestConfig))
	require.NoError(t, err)
	activeConfig := schemas.WithDefaults(schemas.Merge(cfg, minExpConfig))
	return endTestExp(t, createTestExpWithActiveConfig(t, api, owner.User, 1, activeConfig).ID)
}

// resumeOverride returns the override config that Resume Current Trial in the WebUI sends: the whole
// config that GetExperiment returns, without its workspace and project.
func resumeOverride(ctx context.Context, t *testing.T, api *apiServer, expID int) string {
	t.Helper()
	resp, err := api.GetExperiment(ctx, &apiv1.GetExperimentRequest{ExperimentId: int32(expID)})
	require.NoError(t, err)
	raw, err := protojson.Marshal(resp.Config)
	require.NoError(t, err)
	var config map[string]any
	require.NoError(t, json.Unmarshal(raw, &config))
	delete(config, "workspace")
	delete(config, "project")
	override, err := json.Marshal(config)
	require.NoError(t, err)
	return string(override)
}

func sessionCount(t *testing.T, u continueTestUser) int {
	t.Helper()
	n, err := db.Bun().NewSelect().Table("user_sessions").
		Where("user_id = ?", u.ID).
		Count(context.Background())
	require.NoError(t, err)
	return n
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

	t.Run("an administrator may not change the code it runs", func(t *testing.T) {
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(admin.ctx, &apiv1.ContinueExperimentRequest{
			Id:             int32(expID),
			OverrideConfig: "entrypoint: echo changed",
		})
		require.Equal(t, codes.PermissionDenied, status.Code(err), err)
		require.ErrorContains(t, err, owner.Username)
		require.ErrorContains(t, err, "entrypoint")
		requireNotContinued(t, expID)
	})

	t.Run("the owner may change the code", func(t *testing.T) {
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{
			Id:             int32(expID),
			OverrideConfig: "entrypoint: echo changed",
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
		active, err := api.m.db.ActiveExperimentConfig(expID)
		require.NoError(t, err)
		require.Equal(t, "echo changed", active.Entrypoint().RawEntrypoint)
	})

	t.Run("a continue that fails leaves the owner no new session", func(t *testing.T) {
		// Not ended, so the continue fails after the session for its tasks is made.
		expID := createTestExpWithProjectID(t, api, owner.User, 1).ID
		before := sessionCount(t, owner)
		_, err := api.ContinueExperiment(admin.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.ErrorContains(t, err, "non terminal state")
		require.Equal(t, before, sessionCount(t, owner))
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

// A user who may continue another user's experiment, under RBAC or as an administrator, may not
// change the code its trials run or what they run it with, because they run as its owner.
func TestContinueExperimentOnlyOwnerChangesCode(t *testing.T) {
	api, authZExp, projectAuthZ, _, _ := setupExpAuthTest(t, nil)
	owner := addContinueTestUser(t, false, 48000)
	actor := addContinueTestUser(t, false, 49000)

	isActor := mock.MatchedBy(func(m model.User) bool { return m.ID == actor.ID })
	isOwners := mock.MatchedBy(func(e *model.Experiment) bool {
		return e.OwnerID != nil && *e.OwnerID == owner.ID
	})
	// The mocks are shared with other tests; these expectations match only this test's users.
	authZExp.On("CanGetExperiment", mock.Anything, isActor, isOwners).Return(nil)
	authZExp.On("CanEditExperiment", mock.Anything, isActor, isOwners).Return(nil)
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, isActor, isOwners).Return(nil)
	projectAuthZ.On("CanGetProject", mock.Anything, isActor, mock.Anything).Return(nil)

	refused := []struct{ name, field, override string }{
		{"entrypoint", "entrypoint", "entrypoint: echo changed"},
		{"image", "environment", "environment: {image: other/image:1}"},
		{"new environment variable", "environment",
			"environment: {environment_variables: [LD_PRELOAD=/tmp/x.so]}"},
		{"environment variable value", "environment", "environment: {environment_variables: [A=2]}"},
		{"pod spec", "environment",
			"environment: {pod_spec: {spec: {initContainers: [{name: x, image: other/image:1}]}}}"},
		{"bind mount", "bind_mounts", "bind_mounts: [{host_path: /home/other, container_path: /x}]"},
		{"checkpoint storage", "checkpoint_storage",
			"checkpoint_storage: {type: shared_fs, host_path: /home/other}"},
		{"warm start checkpoint", "searcher.source_checkpoint_uuid",
			"searcher: {name: single, metric: loss, max_length: {batches: 10}, " +
				"source_checkpoint_uuid: 7e0bad9e-8c1b-4f4e-9d2a-3a0f1f6b0c01}"},
		{"warm start trial", "searcher.source_trial_id",
			"searcher: {name: single, metric: loss, max_length: {batches: 10}, source_trial_id: 1}"},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			expID := endedCodeTestExp(t, api, owner)
			sessions := sessionCount(t, owner)
			_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
				Id:             int32(expID),
				OverrideConfig: c.override,
			})
			require.Equal(t, codes.PermissionDenied, status.Code(err), err)
			require.ErrorContains(t, err, c.field)
			require.ErrorContains(t, err, owner.Username)
			requireNotContinued(t, expID)
			require.Equal(t, sessions, sessionCount(t, owner))
		})
	}

	t.Run("allowed: the whole config, as Resume Current Trial sends it", func(t *testing.T) {
		expID := endedCodeTestExp(t, api, owner)
		_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
			Id:             int32(expID),
			OverrideConfig: resumeOverride(actor.ctx, t, api, expID),
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
	})

	t.Run("allowed: fields that are not code", func(t *testing.T) {
		expID := endedCodeTestExp(t, api, owner)
		_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
			OverrideConfig: `
description: changed
max_restarts: 7
searcher: {name: single, metric: loss, max_length: {batches: 20}}
environment: {environment_variables: [B=2]}
checkpoint_storage: {type: shared_fs, host_path: /, save_trial_latest: 3}
`,
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
		active, err := api.m.db.ActiveExperimentConfig(expID)
		require.NoError(t, err)
		require.Equal(t, 7, active.MaxRestarts())
		require.Equal(t, 3, active.CheckpointStorage().SaveTrialLatest())
	})
}
