//go:build integration
// +build integration

package internal

import (
	"context"
	"crypto/rand"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/google/uuid"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/metadata"
	"gopkg.in/guregu/null.v3"
	k8sV1 "k8s.io/api/core/v1"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/mocks/allocationmocks"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/checkpointv1"
)

const gcTestHostPath = "/srv/gc-test-checkpoints"

// gcTestOwnerAUG is the agent user and group of the experiment owner in these tests. The admin of
// setupAPITest has none of its own.
var gcTestOwnerAUG = model.AgentUserGroup{UID: 4242, GID: 4343, User: "gc-owner", Group: "gc-owners"}

// gcTestExperimentEnv is what an experiment's owner could set to run code of their choice in each
// container that takes the experiment's environment: BASH_ENV before the bash entrypoint,
// LD_PRELOAD and PYTHONPATH before the Python code, all from a bind mount of their own.
var gcTestExperimentEnv = []string{
	"BASH_ENV=/hooks/run-first.sh",
	"LD_PRELOAD=/hooks/libhook.so",
	"PYTHONPATH=/hooks",
}

// addGCTestOwner adds a user who is not an administrator, with gcTestOwnerAUG.
func addGCTestOwner(t *testing.T) model.User {
	owner := model.User{
		Username:     uuid.NewString(),
		PasswordHash: null.NewString("", false),
		Active:       true,
	}
	aug := gcTestOwnerAUG
	id, err := user.Add(context.TODO(), &owner, &aug)
	require.NoError(t, err)
	owner.ID = id
	return owner
}

// userContext returns a request context of u.
func userContext(t *testing.T, u model.User) context.Context {
	token, err := user.StartSession(context.TODO(), &u)
	require.NoError(t, err)
	return metadata.NewIncomingContext(context.TODO(),
		metadata.Pairs("x-user-token", "Bearer "+token))
}

// createGCTestExperiment creates a completed experiment of owner, with gcTestExperimentEnv, a bind
// mount and shared_fs checkpoint storage at gcTestHostPath, and one checkpoint, whose UUID it
// returns. The experiment is in a project of its own, so that tests that delete every experiment
// of a project leave it alone. ctx is an administrator's.
//
// nolint: exhaustruct
func createGCTestExperiment(
	ctx context.Context, t *testing.T, api *apiServer, owner model.User,
) (*model.Experiment, string) {
	return createGCTestExperimentWithStorage(ctx, t, api, owner, &expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr(gcTestHostPath)},
	})
}

// createGCTestExperimentWithStorage is createGCTestExperiment with other checkpoint storage.
//
// nolint: exhaustruct
func createGCTestExperimentWithStorage(
	ctx context.Context, t *testing.T, api *apiServer, owner model.User,
	storage *expconf.CheckpointStorageConfig,
) (*model.Experiment, string) {
	env := gcTestExperimentEnv
	conf := expconf.ExperimentConfig{
		RawEnvironment: &expconf.EnvironmentConfigV0{
			RawEnvironmentVariables: &expconf.EnvironmentVariablesMapV0{
				RawCPU: env, RawCUDA: env, RawROCM: env,
			},
		},
		RawBindMounts: expconf.BindMountsConfigV0{{
			RawHostPath:      "/home/gc-owner/hooks",
			RawContainerPath: "/hooks",
		}},
		RawCheckpointStorage: storage,
	}
	_, projectID := createProjectAndWorkspace(ctx, t, api)
	exp := createTestExpWithActiveConfig(t, api, owner, projectID,
		schemas.WithDefaults(schemas.Merge(conf, minExpConfig)))
	require.Equal(t, storage.RawSharedFSConfig != nil,
		exp.Config.CheckpointStorage.RawSharedFSConfig != nil)
	require.Equal(t, gcTestExperimentEnv,
		exp.Config.Environment.EnvironmentVariables().For(device.CPU))
	require.Len(t, exp.Config.BindMounts, 1)

	requestID := model.NewRequestID(rand.Reader)
	tk := &model.Task{
		TaskType:   model.TaskTypeTrial,
		LogVersion: model.TaskLogVersion1,
		StartTime:  time.Now(),
		TaskID:     trialTaskID(exp.ID, requestID),
	}
	require.NoError(t, db.AddTask(ctx, tk))
	tr := &model.Trial{
		StartTime:    time.Now(),
		RequestID:    &requestID,
		State:        model.CompletedState,
		ExperimentID: exp.ID,
	}
	require.NoError(t, db.AddTrial(ctx, tr, tk.TaskID))
	aID := model.AllocationID(string(tk.TaskID) + "-1")
	require.NoError(t, db.AddAllocation(ctx, &model.Allocation{
		AllocationID: aID,
		TaskID:       tk.TaskID,
		Slots:        1,
		ResourcePool: "default",
		StartTime:    ptrs.Ptr(time.Now().UTC().Truncate(time.Millisecond)),
	}))
	ckpt := &model.CheckpointV2{
		UUID:         uuid.New(),
		TaskID:       tk.TaskID,
		AllocationID: &aID,
		ReportTime:   time.Now(),
		State:        model.CompletedState,
		Resources:    map[string]int64{"model.pt": 128},
		Metadata:     map[string]interface{}{"steps_completed": 5},
	}
	require.NoError(t, db.AddCheckpointMetadata(ctx, ckpt, tr.ID))

	_, err := db.Bun().NewUpdate().Table("experiments").
		Set("state = ?", model.CompletedState).Where("id = ?", exp.ID).Exec(ctx)
	require.NoError(t, err)
	return exp, ckpt.UUID.String()
}

// captureCheckpointGC replaces the allocation service with one that hands each started checkpoint
// GC task to the returned channel and finishes it at once.
func captureCheckpointGC(t *testing.T) chan tasks.GCCkptSpec {
	specs := make(chan tasks.GCCkptSpec, 16)
	var as allocationmocks.AllocationService
	as.On("StartAllocation", mock.Anything, mock.Anything, mock.Anything, mock.Anything,
		mock.Anything, mock.Anything).Return(nil).Run(func(args mock.Arguments) {
		specs <- args.Get(4).(tasks.GCCkptSpec)
		args.Get(5).(func(*task.AllocationExited))(&task.AllocationExited{
			FinalState: task.AllocationState{State: model.AllocationStateTerminated},
		})
	})
	old := task.DefaultService
	task.DefaultService = &as
	t.Cleanup(func() { task.DefaultService = old })
	return specs
}

func nextGCSpec(t *testing.T, specs chan tasks.GCCkptSpec) tasks.GCCkptSpec {
	t.Helper()
	select {
	case spec := <-specs:
		return spec
	case <-time.After(30 * time.Second):
		t.Fatal("no checkpoint GC task was started")
		return tasks.GCCkptSpec{} //nolint:exhaustruct
	}
}

// requireGCRunsAsOwner checks that a checkpoint GC task of the experiment runs as its owner, with
// no user session, and with none of the experiment's environment variables or bind mounts, but
// with its checkpoint storage. It reports every check that fails, then stops the test.
func requireGCRunsAsOwner(t *testing.T, spec tasks.GCCkptSpec, owner model.User, expID int) {
	t.Helper()
	require.Equal(t, expID, spec.ExperimentID)
	require.NotNil(t, spec.Base.Owner)
	require.NotNil(t, spec.Base.AgentUserGroup)
	failed := t.Failed()

	// Identity.
	assert.Equal(t, owner.ID, spec.Base.Owner.ID, "GC must run as the experiment's owner")
	assert.Equal(t, owner.Username, spec.Base.Owner.Username)
	assert.Equal(t, gcTestOwnerAUG.UID, spec.Base.AgentUserGroup.UID, "agent uid")
	assert.Equal(t, gcTestOwnerAUG.GID, spec.Base.AgentUserGroup.GID, "agent gid")
	assert.Equal(t, gcTestOwnerAUG.User, spec.Base.AgentUserGroup.User)
	assert.Equal(t, gcTestOwnerAUG.Group, spec.Base.AgentUserGroup.Group)
	assert.Empty(t, spec.Base.UserSessionToken, "GC must get no user session")

	// Environment.
	ts := spec.ToTaskSpec()
	assert.NotContains(t, ts.EnvVars(), "DET_USER_TOKEN")
	for _, d := range []device.Type{device.CPU, device.CUDA, device.ROCM} {
		for _, v := range ts.Environment.EnvironmentVariables().For(d) {
			for _, name := range []string{"BASH_ENV", "LD_PRELOAD", "PYTHONPATH"} {
				assert.False(t, strings.HasPrefix(v, name+"="),
					"GC takes %q from the experiment's environment for %s", v, d)
			}
		}
	}
	for _, m := range ts.Mounts {
		assert.NotEqual(t, "/hooks", m.Target, "GC takes the experiment's bind mount %+v", m)
	}

	// Storage.
	assert.Equal(t, gcTestHostPath, spec.LegacyConfig.CheckpointStorage.RawSharedFSConfig.HostPath())
	assert.Contains(t, ts.Mounts, mount.Mount{
		Type:        mount.TypeBind,
		Source:      gcTestHostPath,
		Target:      expconf.DefaultSharedFSContainerPath,
		BindOptions: &mount.BindOptions{Propagation: expconf.DefaultSharedFSPropagation},
	})
	if !failed && t.Failed() {
		t.FailNow()
	}
}

func waitForExperimentDeleted(ctx context.Context, t *testing.T, api *apiServer, expID int) {
	t.Helper()
	for i := 0; i < 30; i++ {
		_, err := api.GetExperiment(ctx, &apiv1.GetExperimentRequest{ExperimentId: int32(expID)})
		if err != nil {
			require.Equal(t, apiPkg.NotFoundErrs("experiment", strconv.Itoa(expID), true), err)
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatalf("experiment %d was not deleted", expID)
}

// A checkpoint GC task that a user starts on another user's experiment runs as the experiment's
// owner. Before, it ran with the starting user's session token, and the experiment's environment
// variables and bind mounts ran code of the owner's choice in it, so an administrator who deleted a
// user's checkpoints, experiment or TensorBoard files handed that user their token.
func TestCheckpointGCRunsAsExperimentOwner(t *testing.T) {
	api, admin, adminCtx := setupAPITest(t, nil)
	require.True(t, admin.Admin)
	owner := addGCTestOwner(t)
	require.False(t, owner.Admin)
	ownerCtx := userContext(t, owner)
	specs := captureCheckpointGC(t)

	t.Run("an admin removes files of the owner's checkpoint", func(t *testing.T) {
		exp, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
		_, err := api.CheckpointsRemoveFiles(adminCtx, &apiv1.CheckpointsRemoveFilesRequest{
			CheckpointUuids: []string{ckpt},
			CheckpointGlobs: []string{"optimizer/**"},
		})
		require.NoError(t, err)
		spec := nextGCSpec(t, specs)
		requireGCRunsAsOwner(t, spec, owner, exp.ID)
		require.Equal(t, ckpt, spec.ToDelete)
	})

	t.Run("an admin deletes the owner's checkpoint", func(t *testing.T) {
		exp, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
		_, err := api.DeleteCheckpoints(adminCtx, &apiv1.DeleteCheckpointsRequest{
			CheckpointUuids: []string{ckpt},
		})
		require.NoError(t, err)
		spec := nextGCSpec(t, specs)
		requireGCRunsAsOwner(t, spec, owner, exp.ID)
		require.Equal(t, []string{fullDeleteGlob}, spec.CheckpointGlobs)
	})

	t.Run("an admin deletes the owner's experiment", func(t *testing.T) {
		exp, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
		_, err := api.DeleteExperiments(adminCtx, &apiv1.DeleteExperimentsRequest{
			ProjectId:     int32(exp.ProjectID),
			ExperimentIds: []int32{int32(exp.ID)},
		})
		require.NoError(t, err)
		spec := nextGCSpec(t, specs)
		requireGCRunsAsOwner(t, spec, owner, exp.ID)
		require.Equal(t, ckpt, spec.ToDelete)
		require.True(t, spec.DeleteTensorboards)
		waitForExperimentDeleted(adminCtx, t, api, exp.ID)
	})

	t.Run("an admin deletes one experiment of the owner", func(t *testing.T) {
		exp, _ := createGCTestExperiment(adminCtx, t, api, owner)
		_, err := api.DeleteExperiment(adminCtx, &apiv1.DeleteExperimentRequest{
			ExperimentId: int32(exp.ID),
		})
		require.NoError(t, err)
		requireGCRunsAsOwner(t, nextGCSpec(t, specs), owner, exp.ID)
		waitForExperimentDeleted(adminCtx, t, api, exp.ID)
	})

	t.Run("an admin deletes the TensorBoard files of the owner's experiment", func(t *testing.T) {
		exp, _ := createGCTestExperiment(adminCtx, t, api, owner)
		_, err := api.DeleteTensorboardFiles(adminCtx, &apiv1.DeleteTensorboardFilesRequest{
			ExperimentId: int32(exp.ID),
		})
		require.NoError(t, err)
		spec := nextGCSpec(t, specs)
		requireGCRunsAsOwner(t, spec, owner, exp.ID)
		require.Empty(t, spec.ToDelete)
		require.True(t, spec.DeleteTensorboards)
	})

	t.Run("the owner removes files of their own checkpoint", func(t *testing.T) {
		exp, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
		_, err := api.CheckpointsRemoveFiles(ownerCtx, &apiv1.CheckpointsRemoveFilesRequest{
			CheckpointUuids: []string{ckpt},
			CheckpointGlobs: []string{"optimizer/**"},
		})
		require.NoError(t, err)
		requireGCRunsAsOwner(t, nextGCSpec(t, specs), owner, exp.ID)
	})

	t.Run("another user still cannot remove files of the owner's checkpoint", func(t *testing.T) {
		_, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
		other := addGCTestOwner(t)
		_, err := api.CheckpointsRemoveFiles(userContext(t, other),
			&apiv1.CheckpointsRemoveFilesRequest{CheckpointUuids: []string{ckpt}})
		require.ErrorContains(t, err, "PermissionDenied")
		select {
		case spec := <-specs:
			t.Fatalf("a checkpoint GC task was started for experiment %d", spec.ExperimentID)
		case <-time.After(time.Second):
		}
	})
}

// The GC task keeps what the administrator sets: the task container defaults of its pool, with
// their environment variables and bind mounts, and the checkpoint GC pod spec.
func TestCheckpointGCKeepsTaskContainerDefaults(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	owner := addGCTestOwner(t)
	specs := captureCheckpointGC(t)

	proxy := []string{"HTTPS_PROXY=http://proxy.admin.example:3128"}
	gcPodSpec := &k8sV1.Pod{Spec: k8sV1.PodSpec{Volumes: []k8sV1.Volume{{Name: "gc-pod-spec"}}}}
	//nolint:exhaustruct
	tcd := model.TaskContainerDefaultsConfig{
		EnvironmentVariables: &model.RuntimeItems{CPU: proxy, CUDA: proxy, ROCM: proxy},
		BindMounts: model.BindMountsConfig{{
			HostPath: "/opt/admin-ca", ContainerPath: "/opt/admin-ca", ReadOnly: true,
			Propagation: "rprivate",
		}},
		CheckpointGCPodSpec: gcPodSpec,
	}
	var gcRM mocks.ResourceManager
	gcRM.On("ResolveResourcePool", mock.Anything, mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName("aux"), nil)
	gcRM.On("TaskContainerDefaults", rm.ResourcePoolName("aux"), mock.Anything).Return(tcd, nil)
	api.m.rm = &gcRM

	exp, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
	_, err := api.CheckpointsRemoveFiles(userContext(t, owner), &apiv1.CheckpointsRemoveFilesRequest{
		CheckpointUuids: []string{ckpt},
		CheckpointGlobs: []string{fullDeleteGlob},
	})
	require.NoError(t, err)
	spec := nextGCSpec(t, specs)
	requireGCRunsAsOwner(t, spec, owner, exp.ID)

	ts := spec.ToTaskSpec()
	for _, d := range []device.Type{device.CPU, device.CUDA, device.ROCM} {
		require.Equal(t, proxy, ts.Environment.EnvironmentVariables().For(d))
	}
	require.Equal(t, gcPodSpec.Spec, ts.Environment.PodSpec().Spec)
	require.Contains(t, ts.Mounts, mount.Mount{
		Type: mount.TypeBind, Source: "/opt/admin-ca", Target: "/opt/admin-ca", ReadOnly: true,
		BindOptions: &mount.BindOptions{Propagation: "rprivate"},
	})
	require.Len(t, ts.Mounts, 2)
}

// The GC task of a deactivated owner still runs as the owner, and its allocation token, the only
// credential it has and the one the GC code reports with, can still record the deleted files.
func TestCheckpointGCOfDeactivatedOwner(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	owner := addGCTestOwner(t)
	specs := captureCheckpointGC(t)

	exp, ckpt := createGCTestExperiment(adminCtx, t, api, owner)
	require.NoError(t, user.SetActive(context.TODO(), []model.UserID{owner.ID}, false))

	_, err := api.DeleteCheckpoints(adminCtx, &apiv1.DeleteCheckpointsRequest{
		CheckpointUuids: []string{ckpt},
	})
	require.NoError(t, err)
	spec := nextGCSpec(t, specs)
	requireGCRunsAsOwner(t, spec, owner, exp.ID)
	require.False(t, spec.Base.Owner.Active)

	// What gc_checkpoints.py does once the files are gone, with the token the master gives the
	// task's allocation, which acts as spec.Base.Owner.
	tk := db.RequireMockTask(t, api.m.db, &owner.ID)
	allocationID := db.RequireMockAllocation(t, api.m.db, tk.TaskID).AllocationID
	token, err := db.StartAllocationSession(context.TODO(), allocationID, spec.Base.Owner)
	require.NoError(t, err)
	taskCtx := metadata.NewIncomingContext(context.TODO(),
		metadata.Pairs("x-allocation-token", fmt.Sprintf("Bearer %s", token)))
	_, err = api.PatchCheckpoints(taskCtx, &apiv1.PatchCheckpointsRequest{
		Checkpoints: []*checkpointv1.PatchCheckpoint{{
			Uuid: ckpt,
			Resources: &checkpointv1.PatchCheckpoint_OptionalResources{
				Resources: map[string]int64{},
			},
		}},
	})
	require.NoError(t, err)
	_, _, state := getCheckpointSizeResourcesState(adminCtx, t, ckpt)
	require.Equal(t, model.DeletedState, state)
}

// Directory checkpoint storage is a path that the experiment mounts in its containers. A GC task
// takes no bind mounts from the experiment, so the master refuses to start one that would not
// see the storage, rather than let it record the checkpoints as deleted while their files remain.
// A task container default that mounts the path makes it work.
func TestCheckpointGCRefusesUnmountedDirectoryStorage(t *testing.T) {
	api, _, adminCtx := setupAPITest(t, nil)
	owner := addGCTestOwner(t)
	specs := captureCheckpointGC(t)

	//nolint:exhaustruct
	exp, _ := createGCTestExperimentWithStorage(adminCtx, t, api, owner,
		&expconf.CheckpointStorageConfig{
			RawDirectoryConfig: &expconf.DirectoryConfig{RawContainerPath: ptrs.Ptr("/mnt/ckpts/run")},
		})
	require.Equal(t, "/mnt/ckpts/run",
		exp.Config.CheckpointStorage.RawDirectoryConfig.ContainerPath())

	_, err := api.DeleteTensorboardFiles(adminCtx, &apiv1.DeleteTensorboardFilesRequest{
		ExperimentId: int32(exp.ID),
	})
	require.ErrorContains(t, err, "checkpoint storage is the directory /mnt/ckpts/run")
	select {
	case spec := <-specs:
		t.Fatalf("a checkpoint GC task was started for experiment %d", spec.ExperimentID)
	default:
	}

	//nolint:exhaustruct
	tcd := model.TaskContainerDefaultsConfig{
		BindMounts: model.BindMountsConfig{{
			HostPath: "/data/ckpts", ContainerPath: "/mnt/ckpts", Propagation: "rprivate",
		}},
	}
	var gcRM mocks.ResourceManager
	gcRM.On("ResolveResourcePool", mock.Anything, mock.Anything, mock.Anything).
		Return(rm.ResourcePoolName("aux"), nil)
	gcRM.On("TaskContainerDefaults", rm.ResourcePoolName("aux"), mock.Anything).Return(tcd, nil)
	api.m.rm = &gcRM

	_, err = api.DeleteTensorboardFiles(adminCtx, &apiv1.DeleteTensorboardFilesRequest{
		ExperimentId: int32(exp.ID),
	})
	require.NoError(t, err)
	spec := nextGCSpec(t, specs)
	require.Equal(t, owner.ID, spec.Base.Owner.ID)
	require.Equal(t, []mount.Mount{{
		Type: mount.TypeBind, Source: "/data/ckpts", Target: "/mnt/ckpts",
		BindOptions: &mount.BindOptions{Propagation: "rprivate"},
	}}, spec.ToTaskSpec().Mounts)
}
