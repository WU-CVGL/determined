//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/docker/docker/api/types/registry"
	"github.com/google/uuid"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/commonv1"
)

const (
	testRegistryUsername = "alice-registry-user"
	testRegistryPassword = "alice-registry-password"
	testRegistryServer   = "registry.example.com"
)

func testRegistryAuth() *registry.AuthConfig {
	return &registry.AuthConfig{
		Username:      testRegistryUsername,
		Password:      testRegistryPassword,
		ServerAddress: testRegistryServer,
	}
}

// registryAuthReader is a user who reads a task or an experiment, and whether they may see its
// registry credentials.
type registryAuthReader struct {
	name     string
	user     model.User
	seesAuth bool
}

// registryAuthReaders returns Bob first, so that the owner's and the admin's later reads also show
// that redacting Bob's response left the task's own config alone.
func registryAuthReaders(owner, bob, admin model.User) []registryAuthReader {
	return []registryAuthReader{
		{"another user", bob, false},
		{"the owner", owner, true},
		{"an admin", admin, true},
	}
}

// requireConfigRegistryAuth checks a config's environment.registry_auth: the credentials for a
// reader who may see them, and no key at all, with the rest of the environment kept, for anyone
// else.
func requireConfigRegistryAuth(t *testing.T, config *structpb.Struct, seesAuth bool) {
	t.Helper()
	require.NotNil(t, config)
	env := config.Fields["environment"].GetStructValue()
	require.NotNil(t, env, "the environment section is kept")
	raw, err := protojson.Marshal(config)
	require.NoError(t, err)
	if !seesAuth {
		require.NotContains(t, env.Fields, "registry_auth")
		require.NotContains(t, string(raw), testRegistryPassword)
		require.NotContains(t, string(raw), testRegistryUsername)
		require.Contains(t, env.Fields, "image", "the rest of the environment is kept")
		return
	}
	auth := env.Fields["registry_auth"].GetStructValue()
	require.NotNil(t, auth, "registry_auth is returned")
	require.Equal(t, testRegistryUsername, auth.Fields["username"].GetStringValue())
	require.Equal(t, testRegistryPassword, auth.Fields["password"].GetStringValue())
	require.Equal(t, testRegistryServer, auth.Fields["serveraddress"].GetStringValue())
}

// registryAuthReadLogs returns the logged admin reads of registry credentials.
func registryAuthReadLogs(entries []*logrus.Entry) []*logrus.Entry {
	var out []*logrus.Entry
	for _, e := range entries {
		if e.Data["credential"] == "registry_auth" {
			out = append(out, e)
		}
	}
	return out
}

// ntscReqWithRegistryAuth is a notebook, TensorBoard, shell or command request of owner whose
// config sets registry_auth.
func ntscReqWithRegistryAuth(owner model.User) *command.CreateGeneric {
	key := "pass"
	spec := tasks.GenericCommandSpec{}
	spec.Base = tasks.TaskSpec{
		Owner:        &model.User{ID: owner.ID, Username: owner.Username},
		TaskID:       string(model.NewTaskID()),
		ExtraEnvVars: map[string]string{},
	}
	spec.CommandID = uuid.New().String()
	spec.Metadata.PrivateKey = &key
	spec.Metadata.PublicKey = &key
	spec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	spec.Config.Description = "task with registry credentials"
	spec.Config.Environment.Image = model.RuntimeItem{
		CPU: testRegistryServer + "/alice/private:1", CUDA: testRegistryServer + "/alice/private:1",
	}
	spec.Config.Environment.RegistryAuth = testRegistryAuth()
	return &command.CreateGeneric{Spec: &spec}
}

func TestNTSCConfigRegistryAuthOnlyForOwnerOrAdmin(t *testing.T) {
	// Basic authorization lets every user see every task.
	api, admin, adminCtx := setupAPITest(t, nil)
	require.True(t, admin.Admin)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	api.m.rm.(*mocks.ResourceManager).On("Release", mock.Anything).Return()
	audits := credentialReadLogs(t)

	alice := db.RequireMockUser(t, api.m.db)
	bob := db.RequireMockUser(t, api.m.db)

	type ntscKind struct {
		name   string
		launch func(req *command.CreateGeneric) (string, error)
		get    func(ctx context.Context, id string) (*structpb.Struct, error)
		kill   func(ctx context.Context, id string) error
	}
	launchGeneric := func(taskType model.TaskType, jobType model.JobType) func(
		*command.CreateGeneric,
	) (string, error) {
		return func(req *command.CreateGeneric) (string, error) {
			c, err := command.DefaultCmdService.LaunchGenericCommand(taskType, jobType, req)
			if err != nil {
				return "", err
			}
			return c.ToV1Command().Id, nil
		}
	}
	kinds := []ntscKind{
		{
			name:   "command",
			launch: launchGeneric(model.TaskTypeCommand, model.JobTypeCommand),
			get: func(ctx context.Context, id string) (*structpb.Struct, error) {
				resp, err := api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: id})
				if err != nil {
					return nil, err
				}
				return resp.Config, nil
			},
			kill: func(ctx context.Context, id string) error {
				_, err := api.KillCommand(ctx, &apiv1.KillCommandRequest{CommandId: id})
				return err
			},
		},
		{
			name: "notebook",
			launch: func(req *command.CreateGeneric) (string, error) {
				c, err := command.DefaultCmdService.LaunchNotebookCommand(req, req.Spec.Base.Owner)
				if err != nil {
					return "", err
				}
				return c.ToV1Notebook().Id, nil
			},
			get: func(ctx context.Context, id string) (*structpb.Struct, error) {
				resp, err := api.GetNotebook(ctx, &apiv1.GetNotebookRequest{NotebookId: id})
				if err != nil {
					return nil, err
				}
				return resp.Config, nil
			},
			kill: func(ctx context.Context, id string) error {
				_, err := api.KillNotebook(ctx, &apiv1.KillNotebookRequest{NotebookId: id})
				return err
			},
		},
		{
			name:   "shell",
			launch: launchGeneric(model.TaskTypeShell, model.JobTypeShell),
			get: func(ctx context.Context, id string) (*structpb.Struct, error) {
				resp, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: id})
				if err != nil {
					return nil, err
				}
				return resp.Config, nil
			},
			kill: func(ctx context.Context, id string) error {
				_, err := api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: id})
				return err
			},
		},
		{
			name:   "tensorboard",
			launch: launchGeneric(model.TaskTypeTensorboard, model.JobTypeTensorboard),
			get: func(ctx context.Context, id string) (*structpb.Struct, error) {
				resp, err := api.GetTensorboard(ctx,
					&apiv1.GetTensorboardRequest{TensorboardId: id})
				if err != nil {
					return nil, err
				}
				return resp.Config, nil
			},
			kill: func(ctx context.Context, id string) error {
				_, err := api.KillTensorboard(ctx,
					&apiv1.KillTensorboardRequest{TensorboardId: id})
				return err
			},
		},
	}

	for _, kind := range kinds {
		t.Run(kind.name, func(t *testing.T) {
			taskID, err := kind.launch(ntscReqWithRegistryAuth(alice))
			require.NoError(t, err)
			before := len(registryAuthReadLogs(audits()))

			var ownerConfig, otherConfig *structpb.Struct
			for _, reader := range registryAuthReaders(alice, bob, admin) {
				config, err := kind.get(ntscUserCtx(t, reader.user), taskID)
				require.NoError(t, err, reader.name)
				requireConfigRegistryAuth(t, config, reader.seesAuth)
				switch reader.name {
				case "the owner":
					ownerConfig = config
				case "another user":
					otherConfig = config
				}
			}

			// The other user gets the owner's config, less registry_auth.
			want := proto.Clone(ownerConfig).(*structpb.Struct)
			delete(want.Fields["environment"].GetStructValue().Fields, "registry_auth")
			require.True(t, proto.Equal(want, otherConfig),
				"another user's config differs from the owner's in more than registry_auth")

			// Only the admin's read is logged, and the admin's kill is not a read.
			logged := registryAuthReadLogs(audits())[before:]
			require.Len(t, logged, 1)
			require.Equal(t, logrus.Fields{
				"user": admin.Username, "user_id": admin.ID, "owner_id": int32(alice.ID),
				"task_id": taskID, "credential": "registry_auth",
			}, logged[0].Data)
			require.NoError(t, kind.kill(adminCtx, taskID))
			require.Len(t, registryAuthReadLogs(audits())[before:], 1)
		})
	}
}

func TestGenericTaskConfigRegistryAuthOnlyForOwnerOrAdmin(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	audits := credentialReadLogs(t)
	alice := db.RequireMockUser(t, api.m.db)
	bob := db.RequireMockUser(t, api.m.db)

	taskID := addGenericTaskForAuthZTest(adminCtx, t, alice, model.DefaultWorkspaceID, nil,
		model.TaskStateActive)
	stored, err := json.Marshal(map[string]any{
		"description": "generic task with registry credentials",
		"entrypoint":  []string{"python3", "train.py"},
		"environment": map[string]any{
			"image":         map[string]any{"cpu": testRegistryServer + "/alice/private:1"},
			"registry_auth": testRegistryAuth(),
		},
	})
	require.NoError(t, err)
	_, err = db.Bun().NewUpdate().Table("tasks").
		Set("config = ?", string(stored)).
		Where("task_id = ?", taskID).
		Exec(adminCtx)
	require.NoError(t, err)

	for _, reader := range registryAuthReaders(alice, bob, admin) {
		resp, err := api.GetGenericTaskConfig(ntscUserCtx(t, reader.user),
			&apiv1.GetGenericTaskConfigRequest{TaskId: taskID.String()})
		require.NoError(t, err, reader.name)
		config := &structpb.Struct{}
		require.NoError(t, protojson.Unmarshal([]byte(resp.Config), config))
		requireConfigRegistryAuth(t, config, reader.seesAuth)
		if reader.seesAuth {
			// The stored config, as Postgres formats JSONB.
			require.JSONEq(t, string(stored), resp.Config, reader.name)
			continue
		}
		require.Equal(t, "generic task with registry credentials",
			config.Fields["description"].GetStringValue())
		require.Len(t, config.Fields["entrypoint"].GetListValue().GetValues(), 2)
	}

	logged := registryAuthReadLogs(audits())
	require.Len(t, logged, 1)
	require.Equal(t, logrus.Fields{
		"user": admin.Username, "user_id": admin.ID, "owner_id": int32(alice.ID),
		"task_id": taskID.String(), "credential": "registry_auth",
	}, logged[0].Data)
}

func TestExperimentConfigRegistryAuthOnlyForOwnerOrAdmin(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	require.True(t, admin.Admin)
	alice := db.RequireMockUser(t, api.m.db)
	bob := db.RequireMockUser(t, api.m.db)
	_, projectID := createProjectAndWorkspace(adminCtx, t, api)

	image := testRegistryServer + "/alice/train:1"
	name := "alice-registry-auth"
	conf := expconf.ExperimentConfig{ //nolint:exhaustruct
		RawName: expconf.Name{RawString: &name},
		RawEnvironment: &expconf.EnvironmentConfigV0{ //nolint:exhaustruct
			RawImage: &expconf.EnvironmentImageMapV0{
				RawCPU: &image, RawCUDA: &image, RawROCM: &image,
			},
			RawRegistryAuth: testRegistryAuth(),
		},
	}
	exp := createTestExpWithActiveConfig(t, api, alice, projectID,
		schemas.WithDefaults(schemas.Merge(minExpConfig, conf)))
	// What the owner submitted, as YAML.
	originalConfig := "name: " + name + "\n" +
		"environment:\n" +
		"  image: " + image + "\n" +
		"  registry_auth:\n" +
		"    username: " + testRegistryUsername + "\n" +
		"    password: " + testRegistryPassword + "\n" +
		"    serveraddress: " + testRegistryServer + "\n"
	_, err := db.Bun().NewUpdate().Table("experiments").
		Set("original_config = ?", originalConfig).
		Where("id = ?", exp.ID).
		Exec(adminCtx)
	require.NoError(t, err)

	pid := int32(projectID)
	for _, reader := range registryAuthReaders(alice, bob, admin) {
		ctx := ntscUserCtx(t, reader.user)
		got, err := api.GetExperiment(ctx,
			&apiv1.GetExperimentRequest{ExperimentId: int32(exp.ID)})
		require.NoError(t, err, reader.name)
		requireConfigRegistryAuth(t, got.Config, reader.seesAuth)
		requireConfigRegistryAuth(t, got.Experiment.Config, reader.seesAuth) //nolint:staticcheck
		if reader.seesAuth {
			require.Equal(t, originalConfig, got.Experiment.OriginalConfig, reader.name)
		} else {
			require.NotContains(t, got.Experiment.OriginalConfig, testRegistryPassword)
			require.NotContains(t, got.Experiment.OriginalConfig, testRegistryUsername)
			original := &structpb.Struct{}
			require.NoError(t,
				protojson.Unmarshal([]byte(got.Experiment.OriginalConfig), original))
			requireConfigRegistryAuth(t, original, false)
			require.Equal(t, name, original.Fields["name"].GetStringValue())
		}

		list, err := api.GetExperiments(ctx, &apiv1.GetExperimentsRequest{
			ExperimentIdFilter: &commonv1.Int32FieldFilter{Incl: []int32{int32(exp.ID)}},
		})
		require.NoError(t, err, reader.name)
		require.Len(t, list.Experiments, 1, reader.name)
		requireConfigRegistryAuth(t, list.Experiments[0].Config, //nolint:staticcheck
			reader.seesAuth)

		search, err := api.SearchExperiments(ctx,
			&apiv1.SearchExperimentsRequest{ProjectId: &pid})
		require.NoError(t, err, reader.name)
		require.Len(t, search.Experiments, 1, reader.name)
		requireConfigRegistryAuth(t, search.Experiments[0].Experiment.Config, //nolint:staticcheck
			reader.seesAuth)
	}
}
