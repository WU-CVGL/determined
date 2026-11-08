//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"strconv"
	"strings"
	"testing"

	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	expauth "github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// The inline credentials of the S3 experiments of these tests. No response, error or log line may
// hold them.
const (
	tbSourcesAccessKey      = "SENTINEL-ACCESS-KEY"
	tbSourcesSecretKey      = "SENTINEL-SECRET-KEY"
	tbSourcesOtherSecretKey = "SENTINEL-OTHER-SECRET-KEY"
	tbSourcesClusterID      = "tb-sources-cluster"
)

// tbSourcesSentinels are the credentials that must not leak.
var tbSourcesSentinels = []string{tbSourcesAccessKey, tbSourcesSecretKey, tbSourcesOtherSecretKey}

// createTestExpWithStorage creates an experiment of owner whose checkpoint storage is storage in
// place of minExpConfig's.
func createTestExpWithStorage(
	t *testing.T, api *apiServer, owner model.User, storage expconf.CheckpointStorageConfig,
) *model.Experiment {
	t.Helper()
	conf := minExpConfig
	conf.RawCheckpointStorage = &storage
	conf.RawName = expconf.Name{RawString: ptrs.Ptr("tb-sources")}
	exp := createTestExpWithActiveConfig(t, api, owner, 1, schemas.WithDefaults(conf))
	stored, err := api.m.db.LegacyExperimentConfigByID(exp.ID)
	require.NoError(t, err)
	require.Equal(t,
		fmt.Sprintf("%T", storage.GetUnionMember()),
		fmt.Sprintf("%T", stored.CheckpointStorage.GetUnionMember()))
	return exp
}

// tbSourcesS3 returns S3 checkpoint storage in bucket, with the given keys when they are set.
func tbSourcesS3(bucket, accessKey, secretKey string) expconf.CheckpointStorageConfig {
	s3 := &expconf.S3Config{
		RawBucket:      ptrs.Ptr(bucket),
		RawPrefix:      ptrs.Ptr("runs"),
		RawEndpointURL: ptrs.Ptr("https://minio.example.com:9000"),
	}
	if accessKey != "" {
		s3.RawAccessKey = ptrs.Ptr(accessKey)
	}
	if secretKey != "" {
		s3.RawSecretKey = ptrs.Ptr(secretKey)
	}
	return expconf.CheckpointStorageConfig{RawS3Config: s3}
}

// tbSourcesSharedFS returns shared_fs checkpoint storage at hostPath.
func tbSourcesSharedFS(hostPath string) expconf.CheckpointStorageConfig {
	return expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr(hostPath)},
	}
}

// tbSourcesIDs returns ids as a config's list.
func tbSourcesIDs(ids ...int) []any {
	out := make([]any, 0, len(ids))
	for _, id := range ids {
		out = append(out, float64(id))
	}
	return out
}

// tbSourcesConfig returns the config of a TensorBoard-viewing shell: ssh disabled, the
// tensorboard service with the given options, and the given more services and top-level fields.
func tbSourcesConfig(
	t *testing.T, tensorBoard map[string]any, services map[string]any, fields map[string]any,
) *structpb.Struct {
	t.Helper()
	all := map[string]any{
		model.ServiceSSH:         map[string]any{"enabled": false},
		model.ServiceTensorBoard: tensorBoard,
	}
	for name, s := range services {
		all[name] = s
	}
	raw := map[string]any{"services": all}
	for key, value := range fields {
		raw[key] = value
	}
	cfg, err := structpb.NewStruct(raw)
	require.NoError(t, err)
	return cfg
}

// tbSourcesExperiments returns the tensorboard options that read the given experiments.
func tbSourcesExperiments(ids ...int) map[string]any {
	return map[string]any{"experiment_ids": tbSourcesIDs(ids...)}
}

// tbSourcesLogs captures every log line, at every level, until the test ends.
func tbSourcesLogs(t *testing.T) *logrustest.Hook {
	logger := logrus.StandardLogger()
	old := logrus.LevelHooks{}
	for level, hooks := range logger.Hooks {
		old[level] = append([]logrus.Hook(nil), hooks...)
	}
	oldLevel := logger.GetLevel()
	hook := &logrustest.Hook{}
	logger.AddHook(hook)
	logger.SetLevel(logrus.TraceLevel)
	t.Cleanup(func() {
		logger.ReplaceHooks(old)
		logger.SetLevel(oldLevel)
	})
	return hook
}

// requireNoSentinel checks that what holds none of the S3 experiments' credentials.
func requireNoSentinel(t *testing.T, name, what string) {
	t.Helper()
	for _, s := range tbSourcesSentinels {
		require.NotContains(t, what, s, name)
	}
}

// requireNoSentinelInLogs checks that no captured log line holds the credentials.
func requireNoSentinelInLogs(t *testing.T, hook *logrustest.Hook) {
	t.Helper()
	require.NotEmpty(t, hook.AllEntries(), "no log line was captured")
	for _, e := range hook.AllEntries() {
		line, err := e.String()
		require.NoError(t, err)
		requireNoSentinel(t, "log", line+fmt.Sprint(e.Data))
	}
}

// requireRefused checks that err has code and exactly message.
func requireRefused(t *testing.T, err error, code codes.Code, message string) {
	t.Helper()
	require.Equal(t, code, status.Code(err), "%v", err)
	require.Equal(t, message, status.Convert(err).Message())
}

// requireInvalidConfig checks that err is the refusal of a config by the rule of message, which
// the config's validation reports among its errors.
func requireInvalidConfig(t *testing.T, err error, message string) {
	t.Helper()
	require.Equal(t, codes.InvalidArgument, status.Code(err), "%v", err)
	require.Contains(t, status.Convert(err).Message(), "error found at root: "+message)
}

// tbSourcesFile returns the item at path of the spec's files.
func tbSourcesFile(t *testing.T, spec tasks.GenericCommandSpec, path string) archive.Item {
	t.Helper()
	for _, item := range spec.AdditionalFiles {
		if item.Path == path {
			return item
		}
	}
	require.FailNow(t, "missing file", "%s is not in the task's files", path)
	return archive.Item{}
}

// tbSourcesFileContent decodes the spec's sources file.
func tbSourcesFileContent(t *testing.T, spec tasks.GenericCommandSpec) tensorBoardSourcesFileContent {
	t.Helper()
	item := tbSourcesFile(t, spec, tensorBoardSourcesFile)
	require.Equal(t, 0o600, int(item.FileMode))
	var content tensorBoardSourcesFileContent
	require.NoError(t, json.Unmarshal(item.Content, &content))
	return content
}

// tbSourcesAuthZ is the basic experiment authz with experiments that a user cannot see, and
// experiments whose artifacts a user cannot get.
type tbSourcesAuthZ struct {
	expauth.ExperimentAuthZBasic
	hidden, noArtifacts map[int]bool
}

func (a *tbSourcesAuthZ) CanGetExperiment(
	_ context.Context, _ model.User, e *model.Experiment,
) error {
	if a.hidden[e.ID] {
		return authz.PermissionDeniedError{}
	}
	return nil
}

func (a *tbSourcesAuthZ) CanGetExperimentArtifacts(
	_ context.Context, _ model.User, e *model.Experiment,
) error {
	if a.noArtifacts[e.ID] {
		return authz.PermissionDeniedError{}
	}
	return nil
}

// selectAuthZTypeForTest selects authZType, with basic authz for the modules that have no
// implementation of it, until the test ends.
func selectAuthZTypeForTest(t *testing.T, authZType string) {
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{
		Type: authZType, FallbackType: ptrs.Ptr(config.BasicAuthZType),
	}
	t.Cleanup(func() {
		config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: config.BasicAuthZType}
	})
}

// useTBSourcesAuthZ selects impl for experiments and basic authz for everything else until the
// test ends.
func useTBSourcesAuthZ(t *testing.T, impl *tbSourcesAuthZ) {
	const authZType = "tb-sources-experiments"
	expauth.AuthZProvider.RegisterOverride(authZType, impl)
	selectAuthZTypeForTest(t, authZType)
}

// A shell whose tensorboard service reads experiments and trials, with ssh disabled, runs
// TensorBoard as its primary service, on zero slots, without sshd, SSH keys or default editors. Its
// sources are checked against the launcher's permissions and one checkpoint storage, and its
// config, metadata, files, environment and mounts hold the canonical sources. The preview refuses
// what the launch refuses. No response, error or log line holds a storage credential.
func TestLaunchShellTensorBoardSources(t *testing.T) {
	api, admin, adminCtx := setupPreviewTest(t)
	rm := api.m.rm.(*mocks.ResourceManager)
	prevClusterID := api.m.taskSpec.ClusterID
	api.m.taskSpec.ClusterID = tbSourcesClusterID
	t.Cleanup(func() { api.m.taskSpec.ClusterID = prevClusterID })
	logs := tbSourcesLogs(t)

	alice := db.RequireMockUser(t, api.m.db)
	bob := db.RequireMockUser(t, api.m.db)
	aliceCtx, bobCtx := ntscUserCtx(t, alice), ntscUserCtx(t, bob)

	// Experiments on the same shared_fs storage, as minExpConfig's, and their trials.
	fs1 := createTestExp(t, api, alice)
	fs2 := createTestExp(t, api, alice)
	fs3 := createTestExp(t, api, bob)
	fs1Trial := db.RequireMockTrialID(t, api.m.db, fs1)
	fs3Trial := db.RequireMockTrialID(t, api.m.db, fs3)
	fsOtherHost := createTestExpWithStorage(t, api, alice, tbSourcesSharedFS("/srv/other"))
	// Experiments on S3: with alice's keys, the same with other save_* options, the same bucket with
	// other keys, and a bucket without keys.
	s3Keys := createTestExpWithStorage(t, api, alice,
		tbSourcesS3("tb-bucket", tbSourcesAccessKey, tbSourcesSecretKey))
	s3KeysTrial := db.RequireMockTrialID(t, api.m.db, s3Keys)
	s3KeysSaveStorage := tbSourcesS3("tb-bucket", tbSourcesAccessKey, tbSourcesSecretKey)
	s3KeysSaveStorage.RawSaveExperimentBest = ptrs.Ptr(5)
	s3KeysSaveStorage.RawSaveTrialLatest = ptrs.Ptr(3)
	s3KeysSave := createTestExpWithStorage(t, api, alice, s3KeysSaveStorage)
	s3OtherKeys := createTestExpWithStorage(t, api, alice,
		tbSourcesS3("tb-bucket", tbSourcesAccessKey, tbSourcesOtherSecretKey))
	s3Open := createTestExpWithStorage(t, api, alice, tbSourcesS3("tb-open", "", ""))
	s3OpenTrial := db.RequireMockTrialID(t, api.m.db, s3Open)

	// Both the preview and the launch of a request refuse it as check does.
	requireBothRefuseBy := func(
		t *testing.T, ctx context.Context, cfg *structpb.Struct, check func(*testing.T, error),
	) {
		t.Helper()
		_, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{Config: cfg})
		check(t, err)
		requireNoSentinel(t, "preview error", err.Error())
		_, err = api.LaunchShell(ctx, &apiv1.LaunchShellRequest{Config: cfg})
		check(t, err)
		requireNoSentinel(t, "launch error", err.Error())
	}
	// Both refuse it with code and exactly message.
	requireBothRefuse := func(
		t *testing.T, ctx context.Context, cfg *structpb.Struct, code codes.Code, message string,
	) {
		t.Helper()
		requireBothRefuseBy(t, ctx, cfg, func(t *testing.T, err error) {
			t.Helper()
			requireRefused(t, err, code, message)
		})
	}
	// Both refuse its config by the rule of message.
	requireBothInvalid := func(t *testing.T, ctx context.Context, cfg *structpb.Struct, message string) {
		t.Helper()
		requireBothRefuseBy(t, ctx, cfg, func(t *testing.T, err error) {
			t.Helper()
			requireInvalidConfig(t, err, message)
		})
	}

	t.Run("shared_fs launch", func(t *testing.T) {
		cfg := tbSourcesConfig(t, map[string]any{
			"experiment_ids": tbSourcesIDs(fs2.ID, fs1.ID, fs1.ID),
			"trial_ids":      tbSourcesIDs(fs3Trial, fs1Trial, fs3Trial),
		}, nil, nil)
		before := launchRows(t, alice.ID)
		preview, err := api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{Config: cfg})
		require.NoError(t, err)
		require.Equal(t, before, launchRows(t, alice.ID), "a preview launches nothing")
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{Config: cfg})
		require.NoError(t, err)
		taskID := resp.Shell.Id
		spec := launchedSpec(t, taskID)

		// The canonical sources: sorted, without duplicates, and without the trials of the listed
		// experiments, in the config of the preview and of the launch, the shell and the metadata.
		exps, trials := []int32{int32(fs1.ID), int32(fs2.ID)}, []int32{int32(fs3Trial)}
		wantServices := map[string]any{
			model.ServiceSSH: map[string]any{"enabled": false},
			model.ServiceTensorBoard: map[string]any{
				"experiment_ids": tbSourcesIDs(fs1.ID, fs2.ID), "trial_ids": tbSourcesIDs(fs3Trial),
			},
		}
		require.Equal(t, wantServices, preview.Config.AsMap()["services"])
		require.Equal(t, wantServices, resp.Config.AsMap()["services"])
		require.Equal(t, exps, resp.Shell.ExperimentIds)
		require.Equal(t, trials, resp.Shell.TrialIds)
		require.Equal(t, exps, spec.Metadata.ExperimentIDs)
		require.Equal(t, trials, spec.Metadata.TrialIDs)
		require.Equal(t, preview.Config.AsMap()["bind_mounts"], resp.Config.AsMap()["bind_mounts"])

		// Zero slots, though the request sets none.
		require.Equal(t, 0, spec.Config.Resources.Slots)
		alloc := allocateRequest(t, rm, taskID)
		require.Equal(t, 0, alloc.SlotsNeeded)

		// TensorBoard is the only service, with no ssh record, sshd, keys or default editors.
		require.Len(t, spec.Services, 1)
		tensorBoard := spec.Services[0]
		require.Equal(t, tasks.InteractiveService{
			Name: model.ServiceTensorBoard, Port: tensorBoard.Port, Exec: true, SourcesCheck: true,
		}, tensorBoard)
		_, ok := spec.Service(model.ServiceSSH)
		require.False(t, ok)
		require.Zero(t, spec.SSHDPort)
		require.Equal(t, []string{interactiveSupervisorScript}, spec.Config.Entrypoint)
		require.Nil(t, spec.Metadata.PrivateKey)
		require.Nil(t, spec.Metadata.PublicKey)
		require.Nil(t, spec.Keys)
		require.Empty(t, resp.Shell.PrivateKey)
		require.Empty(t, resp.Shell.PublicKey)
		require.True(t, strings.HasPrefix(spec.Config.Description, "TensorBoard ("),
			spec.Config.Description)
		require.NotNil(t, spec.Metadata.TensorBoardToken)
		require.Equal(t, []*sproto.ProxyPortConfig{{
			ServiceID: taskID + ":" + strconv.Itoa(tensorBoard.Port), Port: tensorBoard.Port,
			Exec: true, SourcesCheck: true, ServiceToken: *spec.Metadata.TensorBoardToken,
		}}, alloc.ProxyPorts)

		env := spec.Base.ExtraEnvVars
		require.Equal(t, model.ServiceTensorBoard, env["DET_SERVICES"])
		require.Equal(t, tensorBoardSourcesLogDir, env[tensorBoardLogDirEnvVar])
		require.Equal(t, tensorBoardSourcesFile, env[tensorBoardSourcesEnvVar])
		for key := range env {
			require.False(t, strings.HasPrefix(key, "AWS_"), key)
		}

		// The storage of the lowest experiment and the sources, readable by the task's user alone.
		storage := tbSourcesFile(t, spec, storageConfPath)
		require.Equal(t, 0o600, int(storage.FileMode))
		require.JSONEq(t, `{"type":"shared_fs","host_path":"/"}`,
			mustStorageTypeAndHost(t, storage.Content))
		base := "/determined_shared_fs/" + tbSourcesClusterID + "/tensorboard/"
		require.Equal(t, tensorBoardSourcesFileContent{
			Version: 1, StorageType: "shared_fs", StorageConfig: storageConfPath,
			Sources: []tensorBoardSource{
				{
					Run: fmt.Sprintf("experiment/%d", fs1.ID), ExperimentID: int32(fs1.ID),
					Path: fmt.Sprintf("%sexperiment/%d/", base, fs1.ID),
				},
				{
					Run: fmt.Sprintf("experiment/%d", fs2.ID), ExperimentID: int32(fs2.ID),
					Path: fmt.Sprintf("%sexperiment/%d/", base, fs2.ID),
				},
				{
					Run:          fmt.Sprintf("experiment/%d/trial/%d", fs3.ID, fs3Trial),
					ExperimentID: int32(fs3.ID), TrialID: ptrs.Ptr(int32(fs3Trial)),
					Path: fmt.Sprintf("%sexperiment/%d/trial/%d/", base, fs3.ID, fs3Trial),
				},
			},
		}, tbSourcesFileContent(t, spec))

		// The storage's host path, mounted read-only.
		require.Contains(t, spec.Config.BindMounts, model.BindMount{
			HostPath: "/", ContainerPath: expconf.DefaultSharedFSContainerPath, ReadOnly: true,
			Propagation: expconf.DefaultSharedFSPropagation,
		})

		// The shell and its services show the sources.
		shell, err := api.GetShell(aliceCtx, &apiv1.GetShellRequest{ShellId: taskID})
		require.NoError(t, err)
		require.Equal(t, exps, shell.Shell.ExperimentIds)
		require.Equal(t, trials, shell.Shell.TrialIds)
		require.Empty(t, shell.Shell.PrivateKey)
		listed, err := api.GetTaskServices(aliceCtx, &apiv1.GetTaskServicesRequest{TaskId: taskID})
		require.NoError(t, err)
		require.Equal(t, model.ServiceTensorBoard, listed.Primary)
		require.Len(t, listed.Services, 1)
		require.True(t, listed.Services[0].MayConnect)
		require.Equal(t, exps, listed.ExperimentIds)
		require.Equal(t, trials, listed.TrialIds)
	})

	t.Run("primary tensorboard with jupyter", func(t *testing.T) {
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{
			Config: tbSourcesConfig(t, tbSourcesExperiments(fs1.ID),
				map[string]any{model.ServiceJupyter: map[string]any{}}, nil),
		})
		require.NoError(t, err)
		spec := launchedSpec(t, resp.Shell.Id)
		require.Len(t, spec.Services, 2)
		require.Equal(t, model.ServiceTensorBoard, spec.Services[0].Name)
		require.True(t, spec.Services[0].SourcesCheck)
		require.Equal(t, model.ServiceJupyter, spec.Services[1].Name)
		require.False(t, spec.Services[1].SourcesCheck)
		require.Equal(t, "tensorboard,jupyter", spec.Base.ExtraEnvVars["DET_SERVICES"])
		require.Equal(t, []string{interactiveSupervisorScript}, spec.Config.Entrypoint)
		require.Nil(t, spec.Metadata.PublicKey)
		listed, err := api.GetTaskServices(aliceCtx, &apiv1.GetTaskServicesRequest{TaskId: resp.Shell.Id})
		require.NoError(t, err)
		require.Equal(t, model.ServiceTensorBoard, listed.Primary)
	})

	t.Run("an ssh shell's tensorboard reads sources", func(t *testing.T) {
		cfg, err := structpb.NewStruct(map[string]any{
			"resources": map[string]any{"slots": float64(2)},
			"services": map[string]any{
				model.ServiceTensorBoard: tbSourcesExperiments(fs1.ID),
			},
		})
		require.NoError(t, err)
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{Config: cfg})
		require.NoError(t, err)
		spec := launchedSpec(t, resp.Shell.Id)
		require.Equal(t, 2, spec.Config.Resources.Slots)
		require.Equal(t, model.ServiceSSH, spec.Services[0].Name)
		tensorBoard, ok := spec.Service(model.ServiceTensorBoard)
		require.True(t, ok)
		require.True(t, tensorBoard.SourcesCheck)
		require.NotNil(t, spec.Metadata.PublicKey)
		require.Equal(t, []int32{int32(fs1.ID)}, spec.Metadata.ExperimentIDs)
		require.True(t, strings.HasPrefix(spec.Config.Description, "Shell ("))
	})

	t.Run("slots", func(t *testing.T) {
		// Slots set to other than 0 are refused before the pool is resolved.
		for _, slots := range []any{float64(1), float64(-1), "2"} {
			requireBothRefuse(t, aliceCtx, tbSourcesConfig(t, tbSourcesExperiments(fs1.ID), nil,
				map[string]any{"resources": map[string]any{"slots": slots}}),
				codes.InvalidArgument, "services: ssh: can be disabled only with resources.slots 0")
		}
		// Slots set to 0 are accepted.
		_, err := api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{
			Config: tbSourcesConfig(t, tbSourcesExperiments(fs1.ID), nil,
				map[string]any{"resources": map[string]any{"slots": float64(0)}}),
		})
		require.NoError(t, err)
		// A template that disables ssh does not zero the request's slots: slots other than 0,
		// the default included, are refused, and slots 0 are accepted.
		templateName := "tb-sources-" + strconv.Itoa(fs1.ID)
		_, err = db.Bun().NewRaw("INSERT INTO templates (name, config, workspace_id) VALUES (?, ?::jsonb, 1)",
			templateName, fmt.Sprintf(`{"services": {"ssh": {"enabled": false},
				"tensorboard": {"experiment_ids": [%d]}}}`, fs1.ID)).Exec(context.Background())
		require.NoError(t, err)
		t.Cleanup(func() {
			_, _ = db.Bun().NewDelete().Table("templates").Where("name = ?", templateName).
				Exec(context.Background())
		})
		slots, err := structpb.NewStruct(map[string]any{"resources": map[string]any{"slots": float64(1)}})
		require.NoError(t, err)
		_, err = api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{
			Config: slots, TemplateName: templateName,
		})
		requireInvalidConfig(t, err, "services: ssh: can be disabled only with resources.slots 0")
		_, err = api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{TemplateName: templateName})
		requireInvalidConfig(t, err, "services: ssh: can be disabled only with resources.slots 0")
		zero, err := structpb.NewStruct(map[string]any{"resources": map[string]any{"slots": float64(0)}})
		require.NoError(t, err)
		resp, err := api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{
			Config: zero, TemplateName: templateName,
		})
		require.NoError(t, err)
		services := resp.Config.AsMap()["services"].(map[string]any)
		require.Equal(t, tbSourcesIDs(fs1.ID),
			services[model.ServiceTensorBoard].(map[string]any)["experiment_ids"])
	})

	t.Run("bind mounts", func(t *testing.T) {
		mount := func(hostPath string) map[string]any {
			return map[string]any{"bind_mounts": []any{map[string]any{
				"host_path": hostPath, "container_path": expconf.DefaultSharedFSContainerPath,
			}}}
		}
		// A mount of the storage's host path is reused, as the config gives it.
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{
			Config: tbSourcesConfig(t, tbSourcesExperiments(fs1.ID), nil, mount("/")),
		})
		require.NoError(t, err)
		var mounts []model.BindMount
		for _, m := range launchedSpec(t, resp.Shell.Id).Config.BindMounts {
			if m.ContainerPath == expconf.DefaultSharedFSContainerPath {
				mounts = append(mounts, m)
			}
		}
		require.Len(t, mounts, 1)
		require.False(t, mounts[0].ReadOnly)
		// A mount of another host path there is refused.
		requireBothRefuse(t, aliceCtx, tbSourcesConfig(t, tbSourcesExperiments(fs1.ID), nil, mount("/srv/x")),
			codes.InvalidArgument,
			"services: tensorboard: bind_mounts: /determined_shared_fs is mounted from another host path")
	})

	t.Run("storage", func(t *testing.T) {
		// Different storage, named by the lowest experiment and the lowest one that differs.
		requireBothRefuse(t, aliceCtx,
			tbSourcesConfig(t, tbSourcesExperiments(s3Keys.ID, fs1.ID, fs2.ID), nil, nil),
			codes.InvalidArgument, fmt.Sprintf(
				"services: tensorboard: experiments %d and %d use different checkpoint storage",
				fs1.ID, s3Keys.ID))
		requireBothRefuse(t, aliceCtx,
			tbSourcesConfig(t, tbSourcesExperiments(fs1.ID, fsOtherHost.ID), nil, nil),
			codes.InvalidArgument, fmt.Sprintf(
				"services: tensorboard: experiments %d and %d use different checkpoint storage",
				fs1.ID, fsOtherHost.ID))
		// A trial's experiment counts.
		requireBothRefuse(t, aliceCtx, tbSourcesConfig(t, map[string]any{
			"experiment_ids": tbSourcesIDs(fs1.ID), "trial_ids": tbSourcesIDs(s3OpenTrial),
		}, nil, nil), codes.InvalidArgument, fmt.Sprintf(
			"services: tensorboard: experiments %d and %d use different checkpoint storage",
			fs1.ID, s3Open.ID))
		// The same bucket with other keys.
		requireBothRefuse(t, aliceCtx,
			tbSourcesConfig(t, tbSourcesExperiments(s3OtherKeys.ID, s3Keys.ID), nil, nil),
			codes.InvalidArgument, fmt.Sprintf(
				"services: tensorboard: experiments %d and %d use different storage credentials",
				s3Keys.ID, s3OtherKeys.ID))
		// The same storage with other save_* options.
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{
			Config: tbSourcesConfig(t, tbSourcesExperiments(s3Keys.ID, s3KeysSave.ID), nil, nil),
		})
		require.NoError(t, err)
		require.Equal(t, []int32{int32(s3Keys.ID), int32(s3KeysSave.ID)}, resp.Shell.ExperimentIds)
	})

	t.Run("s3", func(t *testing.T) {
		cfg := tbSourcesConfig(t, tbSourcesExperiments(s3Keys.ID), nil, nil)
		preview, err := api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{Config: cfg})
		require.NoError(t, err)
		requireNoSentinel(t, "preview", preview.String())
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{Config: cfg})
		require.NoError(t, err)
		requireNoSentinel(t, "launch", resp.String())
		taskID := resp.Shell.Id
		spec := launchedSpec(t, taskID)

		// The S3 environment, keys included, which only the owner's task gets.
		env := spec.Base.ExtraEnvVars
		require.Equal(t, tbSourcesAccessKey, env["AWS_ACCESS_KEY_ID"])
		require.Equal(t, tbSourcesSecretKey, env["AWS_SECRET_ACCESS_KEY"])
		require.Equal(t, "https://minio.example.com:9000", env["DET_S3_ENDPOINT_URL"])
		require.Equal(t, "minio.example.com:9000", env["S3_ENDPOINT"])
		require.Equal(t, "1", env["S3_USE_HTTPS"])
		require.Equal(t, "tb-bucket", env["AWS_BUCKET"])
		require.Equal(t, tensorBoardSourcesFile, env[tensorBoardSourcesEnvVar])
		// The storage file holds the keys; the sources file does not.
		storage := tbSourcesFile(t, spec, storageConfPath)
		require.Equal(t, 0o600, int(storage.FileMode))
		require.Contains(t, string(storage.Content), tbSourcesSecretKey)
		sources := tbSourcesFile(t, spec, tensorBoardSourcesFile)
		requireNoSentinel(t, "sources.json", string(sources.Content))
		require.Equal(t, []tensorBoardSource{{
			Run: fmt.Sprintf("experiment/%d", s3Keys.ID), ExperimentID: int32(s3Keys.ID),
			Path: fmt.Sprintf("s3://tb-bucket/runs/%s/tensorboard/experiment/%d/",
				tbSourcesClusterID, s3Keys.ID),
		}}, tbSourcesFileContent(t, spec).Sources)
		// No mount for S3 storage.
		for _, m := range spec.Config.BindMounts {
			require.NotEqual(t, expconf.DefaultSharedFSContainerPath, m.ContainerPath)
		}

		shell, err := api.GetShell(aliceCtx, &apiv1.GetShellRequest{ShellId: taskID})
		require.NoError(t, err)
		requireNoSentinel(t, "shell", shell.String())
		shells, err := api.GetShells(aliceCtx, &apiv1.GetShellsRequest{})
		require.NoError(t, err)
		requireNoSentinel(t, "shells", shells.String())
		listed, err := api.GetTaskServices(aliceCtx, &apiv1.GetTaskServicesRequest{TaskId: taskID})
		require.NoError(t, err)
		requireNoSentinel(t, "services", listed.String())
		adminShell, err := api.GetShell(adminCtx, &apiv1.GetShellRequest{ShellId: taskID})
		require.NoError(t, err)
		requireNoSentinel(t, "shell for an admin", adminShell.String())
	})

	t.Run("inline credentials", func(t *testing.T) {
		// Another user may not read the logs of storage with inline credentials, of the whole
		// experiment or of one of its trials.
		keyed := map[string]map[string]any{
			"experiment": tbSourcesExperiments(s3Keys.ID),
			"trial":      {"trial_ids": tbSourcesIDs(s3KeysTrial)},
		}
		for _, tb := range keyed {
			requireBothRefuse(t, bobCtx, tbSourcesConfig(t, tb, nil, nil),
				codes.PermissionDenied, fmt.Sprintf(
					"services: tensorboard: experiment %d: its checkpoint storage has credentials; "+
						"only its owner or an admin may read its logs", s3Keys.ID))
		}
		// Its owner and an admin may.
		for name, ctx := range map[string]context.Context{"owner": aliceCtx, "admin": adminCtx} {
			for source, tb := range keyed {
				_, err := api.PreviewShell(ctx, &apiv1.PreviewShellRequest{
					Config: tbSourcesConfig(t, tb, nil, nil),
				})
				require.NoError(t, err, "%s: %s", name, source)
			}
		}
		resp, err := api.LaunchShell(adminCtx, &apiv1.LaunchShellRequest{
			Config: tbSourcesConfig(t, tbSourcesExperiments(s3Keys.ID), nil, nil),
		})
		require.NoError(t, err)
		require.Equal(t, admin.ID, model.UserID(resp.Shell.UserId))
		// Another user may read storage without inline credentials.
		resp, err = api.LaunchShell(bobCtx, &apiv1.LaunchShellRequest{
			Config: tbSourcesConfig(t, map[string]any{"trial_ids": tbSourcesIDs(s3OpenTrial)}, nil, nil),
		})
		require.NoError(t, err)
		spec := launchedSpec(t, resp.Shell.Id)
		require.Empty(t, spec.Metadata.ExperimentIDs)
		require.Equal(t, []int32{int32(s3OpenTrial)}, spec.Metadata.TrialIDs)
		_, ok := spec.Base.ExtraEnvVars["AWS_ACCESS_KEY_ID"]
		require.False(t, ok)
	})

	t.Run("unknown sources", func(t *testing.T) {
		for name, tb := range map[string]map[string]any{
			"experiment": tbSourcesExperiments(fs1.ID, math.MaxInt32),
			"trial": {
				"experiment_ids": tbSourcesIDs(fs1.ID), "trial_ids": tbSourcesIDs(math.MaxInt32),
			},
		} {
			cfg := tbSourcesConfig(t, tb, nil, nil)
			_, err := api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{Config: cfg})
			require.Equal(t, codes.NotFound, status.Code(err), "%s: %v", name, err)
			_, err = api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{Config: cfg})
			require.Equal(t, codes.NotFound, status.Code(err), "%s: %v", name, err)
		}
	})

	t.Run("config", func(t *testing.T) {
		requireBothInvalid(t, aliceCtx, tbSourcesConfig(t, map[string]any{
			"logdir": "runs", "experiment_ids": tbSourcesIDs(fs1.ID),
		}, nil, nil), "services: tensorboard: logdir excludes experiment_ids and trial_ids")
		requireBothInvalid(t, aliceCtx, tbSourcesConfig(t, map[string]any{"logdir": "runs"}, nil, nil),
			"services: ssh: can be disabled only for tensorboard with experiment_ids or trial_ids")
		requireBothInvalid(t, aliceCtx, tbSourcesConfig(t, tbSourcesExperiments(0), nil, nil),
			"services: tensorboard: experiment_ids: 0 is not an ID")
	})

	t.Run("image not inherited", func(t *testing.T) {
		exp := createTestExpWithImage(t, api, alice, "alice-tb-sources")
		resp, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{
			Config: tbSourcesConfig(t, tbSourcesExperiments(exp.ID), nil, nil),
		})
		require.NoError(t, err)
		env := launchedSpec(t, resp.Shell.Id).Config.Environment
		require.Equal(t, model.DefaultEnvConfig(&api.m.config.TaskContainerDefaults).Image, env.Image)
		require.Nil(t, env.RegistryAuth)
		if env.PodSpec != nil {
			require.Empty(t, env.PodSpec.Spec.ImagePullSecrets)
		}
	})

	t.Run("gpu_groups", func(t *testing.T) {
		// Slots are 0, so the groups' counts cannot add up to them, as for the TensorBoard task.
		requireBothRefuse(t, aliceCtx, tbSourcesConfig(t, tbSourcesExperiments(fs1.ID), nil,
			map[string]any{"resources": map[string]any{"gpu_groups": []any{
				map[string]any{"name": "learner", "count": float64(1), "models": []any{"NVIDIA H100"}},
			}}}), codes.InvalidArgument,
			"failed to prepare launch params: resources.gpu_groups counts must add up to resources.slots (0)")
	})

	t.Run("refusals launch nothing", func(t *testing.T) {
		before := launchRows(t, bob.ID)
		for _, cfg := range []*structpb.Struct{
			tbSourcesConfig(t, tbSourcesExperiments(s3Keys.ID), nil, nil),
			tbSourcesConfig(t, tbSourcesExperiments(math.MaxInt32), nil, nil),
			tbSourcesConfig(t, tbSourcesExperiments(fs1.ID, s3Open.ID), nil, nil),
		} {
			_, err := api.PreviewShell(bobCtx, &apiv1.PreviewShellRequest{Config: cfg})
			require.Error(t, err)
			_, err = api.LaunchShell(bobCtx, &apiv1.LaunchShellRequest{Config: cfg})
			require.Error(t, err)
		}
		require.Equal(t, before, launchRows(t, bob.ID))
	})

	requireNoSentinelInLogs(t, logs)
}

// mustStorageTypeAndHost returns the type and host_path of a stored shared_fs checkpoint storage
// as JSON.
func mustStorageTypeAndHost(t *testing.T, content []byte) string {
	t.Helper()
	var storage struct {
		Type     string `json:"type"`
		HostPath string `json:"host_path"`
	}
	require.NoError(t, json.Unmarshal(content, &storage))
	raw, err := json.Marshal(storage)
	require.NoError(t, err)
	return string(raw)
}

// An experiment or trial that the launcher cannot see is not found; one whose artifacts the
// launcher cannot get is refused.
func TestLaunchShellTensorBoardSourcesAuthZ(t *testing.T) {
	api, _, _ := setupPreviewTest(t)
	alice := db.RequireMockUser(t, api.m.db)
	aliceCtx := ntscUserCtx(t, alice)
	visible := createTestExp(t, api, alice)
	hidden := createTestExp(t, api, alice)
	hiddenTrial := db.RequireMockTrialID(t, api.m.db, hidden)
	noArtifacts := createTestExp(t, api, alice)
	noArtifactsTrial := db.RequireMockTrialID(t, api.m.db, noArtifacts)
	useTBSourcesAuthZ(t, &tbSourcesAuthZ{
		hidden: map[int]bool{hidden.ID: true}, noArtifacts: map[int]bool{noArtifacts.ID: true},
	})

	for name, tc := range map[string]struct {
		tensorBoard map[string]any
		code        codes.Code
	}{
		"hidden experiment":      {tbSourcesExperiments(visible.ID, hidden.ID), codes.NotFound},
		"hidden trial":           {map[string]any{"trial_ids": tbSourcesIDs(hiddenTrial)}, codes.NotFound},
		"experiment's artifacts": {tbSourcesExperiments(noArtifacts.ID), codes.PermissionDenied},
		"trial's artifacts": {
			map[string]any{"trial_ids": tbSourcesIDs(noArtifactsTrial)}, codes.PermissionDenied,
		},
	} {
		cfg := tbSourcesConfig(t, tc.tensorBoard, nil, nil)
		_, err := api.PreviewShell(aliceCtx, &apiv1.PreviewShellRequest{Config: cfg})
		require.Equal(t, tc.code, status.Code(err), "%s: %v", name, err)
		_, err = api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{Config: cfg})
		require.Equal(t, tc.code, status.Code(err), "%s: %v", name, err)
	}
	_, err := api.LaunchShell(aliceCtx, &apiv1.LaunchShellRequest{
		Config: tbSourcesConfig(t, tbSourcesExperiments(visible.ID), nil, nil),
	})
	require.NoError(t, err)
}

// A TensorBoard-viewing shell with no pool runs in the pool that zero slots resolve to, which the
// launcher's pool access is checked against; a refused launch starts no task session.
func TestLaunchShellTensorBoardSourcesPoolAccess(t *testing.T) {
	var wsaux, wscompute string
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&wsaux, &wscompute))
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)

	granted := db.RequireMockUser(t, api.m.db)
	other := db.RequireMockUser(t, api.m.db)
	wsaux = accessTestPool(t, "wsaux", admin, true, granted)
	wscompute = accessTestPool(t, "wscompute", admin, true, granted)
	open := accessTestPool(t, "open", admin, false)
	exp := createTestExp(t, api, other)
	otherCtx := ntscUserCtx(t, other)
	sessions := countUserSessions(adminCtx, t, other.ID)

	cfg := tbSourcesConfig(t, tbSourcesExperiments(exp.ID), nil, nil)
	_, err = api.LaunchShell(otherCtx, &apiv1.LaunchShellRequest{Config: cfg})
	requirePoolDenied(t, err, other, wsaux)
	_, err = api.PreviewShell(otherCtx, &apiv1.PreviewShellRequest{Config: cfg})
	requirePoolDenied(t, err, other, wsaux)
	_, err = api.LaunchShell(otherCtx, &apiv1.LaunchShellRequest{
		Config: tbSourcesConfig(t, tbSourcesExperiments(math.MaxInt32), nil,
			map[string]any{"resources": map[string]any{"resource_pool": open}}),
	})
	require.Equal(t, codes.NotFound, status.Code(err), "%v", err)
	require.Equal(t, sessions, countUserSessions(adminCtx, t, other.ID),
		"a refused launch must not start a task session")

	resp, err := api.LaunchShell(ntscUserCtx(t, granted), &apiv1.LaunchShellRequest{Config: cfg})
	require.NoError(t, err)
	require.Equal(t, wsaux, resp.Config.AsMap()["resources"].(map[string]any)["resource_pool"])
	resp, err = api.LaunchShell(otherCtx, &apiv1.LaunchShellRequest{
		Config: tbSourcesConfig(t, tbSourcesExperiments(exp.ID), nil,
			map[string]any{"resources": map[string]any{"resource_pool": open}}),
	})
	require.NoError(t, err)
	require.Equal(t, open, resp.Config.AsMap()["resources"].(map[string]any)["resource_pool"])
}
