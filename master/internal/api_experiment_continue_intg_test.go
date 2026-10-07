//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/db"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/projectv1"
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

// codeTestConfig gives an experiment a description, hyperparameters, data, an image, registry
// credentials, environment variables, a pod spec and a bind mount, so that a continue that sends
// the whole config back, as the WebUI does, merges each of them.
const codeTestConfig = `
description: the owner's run
hyperparameters:
  model_name: mnist
  batch_size: 64
  lr: {type: const, val: 0.1}
data:
  url: https://example.com/owner.tar
environment:
  image: owner/image:1
  registry_auth:
    username: owner
    password: owner-password
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
	return endedTestExpWithConfig(t, api, owner, codeTestConfig)
}

// endedTestExpWithConfig is endedTestExp with config, a YAML experiment config, merged into
// minExpConfig.
func endedTestExpWithConfig(
	t *testing.T, api *apiServer, owner continueTestUser, config string,
) int {
	t.Helper()
	return endedTestExpInProject(t, api, owner, model.DefaultProjectID, config)
}

// endedTestExpInProject is endedTestExpWithConfig in project projectID.
func endedTestExpInProject(
	t *testing.T, api *apiServer, owner continueTestUser, projectID int, config string,
) int {
	t.Helper()
	cfg, err := expconf.ParseAnyExperimentConfigYAML([]byte(config))
	require.NoError(t, err)
	activeConfig := schemas.WithDefaults(schemas.Merge(cfg, minExpConfig))
	return endTestExp(t,
		createTestExpWithActiveConfig(t, api, owner.User, projectID, activeConfig).ID)
}

// resumeOverride returns the override config that Resume Current Trial in the WebUI sends: the whole
// config that GetExperiment returns to the user of ctx, without its workspace and project, and with
// "Fork of" before its description.
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
	if description, ok := config["description"].(string); ok && description != "" {
		config["description"] = "Fork of " + description
	}
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

// requireInProject checks that the running experiment is in project projectID: in the database, in
// the master, and in the workspace and project names that its tasks get.
func requireInProject(t *testing.T, expID, projectID int, workspaceName, projectName string) {
	t.Helper()
	stored, err := db.ExperimentByID(context.Background(), expID)
	require.NoError(t, err)
	require.Equal(t, projectID, stored.ProjectID, "project in the database")

	ref, ok := expauth.ExperimentRegistry.Load(expID)
	require.True(t, ok, "experiment %d is not running", expID)
	e, ok := ref.(*internalExperiment)
	require.True(t, ok)
	e.mu.Lock()
	defer e.mu.Unlock()
	require.Equal(t, projectID, e.ProjectID, "project of the running experiment")
	require.Equal(t, workspaceName, e.taskSpec.Workspace, "workspace its tasks get")
	require.Equal(t, projectName, e.taskSpec.Project, "project its tasks get")
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

	t.Run("an administrator may not change the hyperparameters", func(t *testing.T) {
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(admin.ctx, &apiv1.ContinueExperimentRequest{
			Id:             int32(expID),
			OverrideConfig: "hyperparameters: {model_name: other-model}",
		})
		require.Equal(t, codes.PermissionDenied, status.Code(err), err)
		require.ErrorContains(t, err, owner.Username)
		require.ErrorContains(t, err, "hyperparameters")
		requireNotContinued(t, expID)
	})

	t.Run("the owner may change the hyperparameters", func(t *testing.T) {
		expID := endedTestExp(t, api, owner)
		_, err := api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{
			Id:             int32(expID),
			OverrideConfig: "hyperparameters: {model_name: other-model}",
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
		active, err := api.m.db.ActiveExperimentConfig(expID)
		require.NoError(t, err)
		require.Equal(t, "other-model",
			active.Hyperparameters()["model_name"].RawConstHyperparameter.RawVal)
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

	// The experiment was moved out of Uncategorized, which its config still names.
	t.Run("project access is checked on the experiment's own project", func(t *testing.T) {
		wsID, _ := db.RequireMockWorkspaceID(t, api.m.db, "")
		projID, _ := db.RequireMockProjectID(t, api.m.db, wsID, false)
		isProject := mock.MatchedBy(func(p *projectv1.Project) bool { return p.Id == int32(projID) })
		for _, c := range []struct {
			name   string
			uid    int
			access error
		}{
			{"allowed", 47100, nil},
			{"refused", 47200, authz.PermissionDeniedError{}},
		} {
			t.Run(c.name, func(t *testing.T) {
				u := addContinueTestUser(t, false, c.uid)
				expID := endedTestExpInProject(t, api, owner, projID,
					"{workspace: Uncategorized, project: Uncategorized}")
				isExp := mock.MatchedBy(func(e *model.Experiment) bool { return e.ID == expID })
				authZExp.On("CanGetExperiment", mock.Anything, isUser(u), isExp).Return(nil)
				authZExp.On("CanEditExperiment", mock.Anything, isUser(u), isExp).Return(nil)
				authZExp.On("CanGetExperimentArtifacts", mock.Anything, isUser(u), isExp).Return(nil)
				projectAuthZ.On("CanGetProject", mock.Anything, isUser(u), isProject).Return(c.access)
				projectAuthZ.On("CanGetProject", mock.Anything, isUser(u), mock.Anything).
					Return(fmt.Errorf("checked on another project")).Maybe()

				_, err := api.ContinueExperiment(u.ctx, &apiv1.ContinueExperimentRequest{
					Id: int32(expID),
				})
				projectAuthZ.AssertCalled(t, "CanGetProject", mock.Anything, isUser(u), isProject)
				if c.access == nil {
					require.NoError(t, err)
					requireRunsAs(t, expID, owner)
				} else {
					require.Equal(t, codes.NotFound, status.Code(err), err)
					requireNotContinued(t, expID)
				}
			})
		}
	})

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

// A continue keeps the experiment in its own project, not the workspace and project its config
// names: none when it was created with a project ID, the old ones when it was moved. That project's
// workspace gives its tasks their agent user group and its invariant config.
func TestContinueExperimentKeepsItsProject(t *testing.T) {
	api, _, _ := setupAPITest(t, nil)
	ctx := context.Background()
	owner := addContinueTestUser(t, false, 50000)

	// A workspace with an agent user group and an invariant config, and a project in it.
	wsID, wsName := db.RequireMockWorkspaceID(t, api.m.db, "")
	wsGroup := model.AgentUserGroup{User: "ws-agent", UID: 50100, Group: "ws-group", GID: 50101}
	_, err := db.Bun().NewUpdate().Table("workspaces").
		Set("uid = ?", wsGroup.UID).Set("user_ = ?", wsGroup.User).
		Set("gid = ?", wsGroup.GID).Set("group_ = ?", wsGroup.Group).
		Where("id = ?", wsID).
		Exec(ctx)
	require.NoError(t, err)
	require.NoError(t, configpolicy.SetTaskConfigPolicies(ctx, &model.TaskConfigPolicies{
		WorkspaceID: &wsID, WorkloadType: model.ExperimentType, LastUpdatedBy: owner.ID,
		InvariantConfig: ptrs.Ptr(`{"max_restarts": 3}`),
	}))
	projID, projName := db.RequireMockProjectID(t, api.m.db, wsID, false)
	inWorkspace := owner
	inWorkspace.group = wsGroup

	for _, c := range []struct{ name, config string }{
		{"created with a project ID: its config names no project", "{}"},
		{
			"moved out of Uncategorized: its config names Uncategorized",
			"{workspace: Uncategorized, project: Uncategorized}",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			expID := endedTestExpInProject(t, api, owner, projID, c.config)
			_, err := api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{
				Id: int32(expID),
			})
			require.NoError(t, err)
			requireRunsAs(t, expID, inWorkspace)
			requireInProject(t, expID, projID, wsName, projName)
			active, err := api.m.db.ActiveExperimentConfig(expID)
			require.NoError(t, err)
			require.Equal(t, 3, active.MaxRestarts(), "the workspace's invariant config")
		})
	}

	// Uncategorized (ID 1) is bound as any other project, not read as "no project".
	t.Run("moved to Uncategorized: its config names the project it left", func(t *testing.T) {
		expID := endedTestExpInProject(t, api, owner, model.DefaultProjectID,
			fmt.Sprintf("{workspace: %q, project: %q}", wsName, projName))
		_, err := api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.NoError(t, err)
		requireRunsAs(t, expID, owner)
		requireInProject(t, expID, model.DefaultProjectID, model.DefaultWorkspaceName, "Uncategorized")
		active, err := api.m.db.ActiveExperimentConfig(expID)
		require.NoError(t, err)
		require.NotEqual(t, 3, active.MaxRestarts(), "the other workspace's invariant config")
	})

	// The start-time weight check reads the same workspace as the invariant merge; otherwise the
	// continue fails and leaves the experiment in ERROR.
	t.Run("moved between workspaces that enforce different weights", func(t *testing.T) {
		weightProject := func(weight int) (int, string, string) {
			wsID, wsName := db.RequireMockWorkspaceID(t, api.m.db, "")
			require.NoError(t, configpolicy.SetTaskConfigPolicies(ctx, &model.TaskConfigPolicies{
				WorkspaceID: &wsID, WorkloadType: model.ExperimentType, LastUpdatedBy: owner.ID,
				InvariantConfig: ptrs.Ptr(fmt.Sprintf(`{"resources": {"weight": %d}}`, weight)),
			}))
			projID, projName := db.RequireMockProjectID(t, api.m.db, wsID, false)
			return projID, wsName, projName
		}
		_, fromWS, fromProj := weightProject(2)
		toProjID, toWS, toProj := weightProject(3)

		expID := endedTestExpInProject(t, api, owner, toProjID,
			fmt.Sprintf("{workspace: %q, project: %q, resources: {weight: 2}}", fromWS, fromProj))
		_, err := api.ContinueExperiment(owner.ctx, &apiv1.ContinueExperimentRequest{
			Id: int32(expID),
		})
		require.NoError(t, err)
		requireInProject(t, expID, toProjID, toWS, toProj)
		stored, err := db.ExperimentByID(ctx, expID)
		require.NoError(t, err)
		require.Equal(t, model.ActiveState, stored.State)
		active, err := api.m.db.ActiveExperimentConfig(expID)
		require.NoError(t, err)
		require.InDelta(t, 3.0, active.Resources().Weight(), 0, "the workspace's invariant weight")
	})
}

// A user who may continue another user's experiment, under RBAC or as an administrator, may change
// only the fields that bound its training (continueNonOwnerFields), because its trials run as its
// owner. Everything else, code or not, stays as the owner set it.
func TestContinueExperimentNonOwnerChangesOnlyOperationalFields(t *testing.T) {
	api, authZExp, projectAuthZ, _, _ := setupExpAuthTest(t, nil)
	owner := addContinueTestUser(t, false, 48000)
	actor := addContinueTestUser(t, false, 49000)

	isActor := mock.MatchedBy(func(m model.User) bool { return m.ID == actor.ID })
	isOwner := mock.MatchedBy(func(m model.User) bool { return m.ID == owner.ID })
	isOwners := mock.MatchedBy(func(e *model.Experiment) bool {
		return e.OwnerID != nil && *e.OwnerID == owner.ID
	})
	// The mocks are shared with other tests; these expectations match only this test's users.
	authZExp.On("CanGetExperiment", mock.Anything, isActor, isOwners).Return(nil)
	authZExp.On("CanEditExperiment", mock.Anything, isActor, isOwners).Return(nil)
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, isActor, isOwners).Return(nil)
	projectAuthZ.On("CanGetProject", mock.Anything, isActor, mock.Anything).Return(nil)
	// The owner only reads the config, for the round trip with registry credentials.
	authZExp.On("CanGetExperiment", mock.Anything, isOwner, isOwners).Return(nil)
	authZExp.On("CanGetExperimentArtifacts", mock.Anything, isOwner, isOwners).Return(nil).Maybe()

	// Representative refusals; continueOwnerOnlyChanges has the full matrix in a unit test.
	refused := []struct{ name, field, override string }{
		{"entrypoint", "entrypoint", "entrypoint: echo changed"},
		{"environment variable value", "environment", "environment: {environment_variables: [A=2]}"},
		{"bind mount", "bind_mounts", "bind_mounts: [{host_path: /home/other, container_path: /x}]"},
		{
			"checkpoint storage", "checkpoint_storage",
			"checkpoint_storage: {type: shared_fs, host_path: /home/other}",
		},
		{"hyperparameter value", "hyperparameters", "hyperparameters: {model_name: other-model}"},
		{"data", "data", "data: {url: https://example.com/other.tar}"},
		{"resources.resource_pool", "resources", "resources: {resource_pool: other}"},
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
			require.ErrorContains(t, err, "only they may change "+c.field+" when")
			require.ErrorContains(t, err, owner.Username)
			require.ErrorContains(t, err, "fork the experiment")
			requireNotContinued(t, expID)
			require.Equal(t, sessions, sessionCount(t, owner))
		})
	}

	// GetExperiment leaves environment.registry_auth out for anyone but the owner and
	// administrators (redactExperimentRegistryAuth), so their WebUI sends the config without it,
	// and the owner's or an administrator's WebUI sends it back.
	for _, reader := range []struct {
		name              string
		ctx               context.Context //nolint:containedctx
		sendsRegistryAuth bool
	}{
		{"as the user who continues sees it", actor.ctx, false},
		{"with registry credentials", owner.ctx, true},
	} {
		t.Run("allowed: the whole config, as Resume Current Trial sends it, "+reader.name,
			func(t *testing.T) {
				expID := endedCodeTestExp(t, api, owner)
				override := resumeOverride(reader.ctx, t, api, expID)
				require.Equal(t, reader.sendsRegistryAuth, strings.Contains(override, "owner-password"))
				_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
					Id:             int32(expID),
					OverrideConfig: override,
				})
				require.NoError(t, err)
				requireRunsAs(t, expID, owner)
				active, err := api.m.db.ActiveExperimentConfig(expID)
				require.NoError(t, err)
				require.Equal(t, "owner-password", active.Environment().RegistryAuth().Password)
				require.Equal(t, "Fork of the owner's run", *active.Description())
			})
	}

	t.Run("allowed: every allowed field at once, with other values repeated as they are",
		func(t *testing.T) {
			expID := endedCodeTestExp(t, api, owner)
			_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
				Id: int32(expID),
				OverrideConfig: `
name: renamed
description: changed
labels: [added]
max_restarts: 7
searcher: {name: single, metric: loss, max_length: {batches: 20}}
hyperparameters: {model_name: mnist}
environment: {environment_variables: [B=2]}
checkpoint_storage:
  {type: shared_fs, host_path: /, save_experiment_best: 2, save_trial_best: 3, save_trial_latest: 4}
`,
			})
			require.NoError(t, err)
			requireRunsAs(t, expID, owner)
			active, err := api.m.db.ActiveExperimentConfig(expID)
			require.NoError(t, err)
			require.Equal(t, "renamed", active.Name().String())
			require.Equal(t, "changed", *active.Description())
			require.True(t, active.Labels()["added"])
			require.Equal(t, 7, active.MaxRestarts())
			require.Equal(t, uint64(20), active.Searcher().RawSingleConfig.MaxLength().Units)
			require.Equal(t, 2, active.CheckpointStorage().SaveExperimentBest())
			require.Equal(t, 3, active.CheckpointStorage().SaveTrialBest())
			require.Equal(t, 4, active.CheckpointStorage().SaveTrialLatest())
			require.Equal(t, "mnist",
				active.Hyperparameters()["model_name"].RawConstHyperparameter.RawVal)
		})

	// GetExperiment shows every value under data.secrets as "********" to everyone
	// (authz.ObfuscateExperiments), so Resume Current Trial sends the placeholder back, which is a
	// change to data. A continue without an override config, as det experiment continue sends it
	// without --config, keeps the secrets.
	t.Run("data.secrets: Resume Current Trial is refused, a continue without a config is not",
		func(t *testing.T) {
			expID := endedTestExpWithConfig(t, api, owner,
				"data: {url: https://example.com/owner.tar, secrets: {token: owner-token}}")
			sessions := sessionCount(t, owner)
			override := resumeOverride(actor.ctx, t, api, expID)
			require.NotContains(t, override, "owner-token")
			_, err := api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
				Id:             int32(expID),
				OverrideConfig: override,
			})
			require.Equal(t, codes.PermissionDenied, status.Code(err), err)
			require.ErrorContains(t, err, "only they may change data when")
			requireNotContinued(t, expID)
			require.Equal(t, sessions, sessionCount(t, owner))

			_, err = api.ContinueExperiment(actor.ctx, &apiv1.ContinueExperimentRequest{
				Id:             int32(expID),
				OverrideConfig: "{}\n",
			})
			require.NoError(t, err)
			requireRunsAs(t, expID, owner)
			active, err := api.m.db.ActiveExperimentConfig(expID)
			require.NoError(t, err)
			require.Equal(t, map[string]any{"token": "owner-token"}, active.Data()["secrets"])
		})
}
