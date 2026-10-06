//go:build integration

package internal

import (
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/experiment"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/mocks/allocationmocks"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

func TestRunCheckpointGCTask(t *testing.T) {
	pgDB, _ := db.MustResolveTestPostgres(t)
	db.MustMigrateTestPostgres(t, pgDB, "file://../static/migrations")
	user := db.RequireMockUser(t, pgDB)

	type args struct {
		rm                  *mocks.ResourceManager
		as                  func(t *testing.T) *allocationmocks.AllocationService
		toDeleteCheckpoints []uuid.UUID
		checkpointGlobs     []string
		deleteTensorboards  bool
		unknownExperiment   bool
	}
	tests := []struct {
		name    string
		args    args
		wantErr bool
		errText string
	}{
		{
			name: "delete nothing does nothing",
			args: args{
				rm: func() *mocks.ResourceManager {
					return &mocks.ResourceManager{}
				}(),
				as: func(t *testing.T) *allocationmocks.AllocationService {
					return &allocationmocks.AllocationService{}
				},
			},
			wantErr: false,
		},
		{
			name: "simple success",
			args: args{
				rm: func() *mocks.ResourceManager {
					var r mocks.ResourceManager

					r.On("ResolveResourcePool", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
						Return(rm.ResourcePoolName("default"), nil)

					r.On("TaskContainerDefaults", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
						Return(model.TaskContainerDefaultsConfig{}, nil)

					return &r
				}(),
				as: func(t *testing.T) *allocationmocks.AllocationService {
					var as allocationmocks.AllocationService

					as.On(
						"StartAllocation",
						mock.Anything,
						mock.MatchedBy(func(ar sproto.AllocateRequest) bool {
							return !ar.IsUserVisible &&
								ar.ResourcePool == "default" &&
								ar.SlotsNeeded == 0
						}),
						mock.Anything,
						mock.Anything,
						mock.MatchedBy(func(spec tasks.GCCkptSpec) bool {
							ok := true
							// It runs as the experiment's owner, with no user session. None is
							// minted for it, and the user token of the spec it was given is
							// dropped: for the end-of-experiment GC, that spec is a copy of the
							// experiment's, whose token stop() revokes.
							if spec.Base.Owner == nil || spec.Base.Owner.ID != user.ID {
								t.Errorf("GC runs as %v, not as the experiment's owner %d",
									spec.Base.Owner, user.ID)
								ok = false
							}
							if spec.Base.UserSessionToken != "" {
								t.Error("GC task has a user session token")
								ok = false
							}
							if spec.ToDelete == "" {
								t.Error("to delete was not set")
								ok = false
							}
							if !spec.DeleteTensorboards {
								t.Error("delete tensorboards was not set")
								ok = false
							}
							if spec.CheckpointGlobs == nil {
								t.Error("checkpoint globs missing")
								ok = false
							}
							return ok
						}),
						mock.Anything,
					).Return(nil).Run(func(args mock.Arguments) {
						cb := args.Get(5).(func(*task.AllocationExited))
						cb(&task.AllocationExited{FinalState: task.AllocationState{
							State: model.AllocationStateTerminated,
						}})
					})

					return &as
				},
				toDeleteCheckpoints: []uuid.UUID{uuid.New()},
				checkpointGlobs:     []string{"optimizer_state.pkl"},
				deleteTensorboards:  true,
			},
			wantErr: false,
		},
		{
			name: "unknown experiment fails before starting anything",
			args: args{
				rm: func() *mocks.ResourceManager {
					var r mocks.ResourceManager
					r.On("ResolveResourcePool", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
						Return(rm.ResourcePoolName("default"), nil)
					r.On("TaskContainerDefaults", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
						Return(model.TaskContainerDefaultsConfig{}, nil)
					return &r
				}(),
				as: func(t *testing.T) *allocationmocks.AllocationService {
					return &allocationmocks.AllocationService{}
				},
				toDeleteCheckpoints: []uuid.UUID{uuid.New()},
				unknownExperiment:   true,
			},
			wantErr: true,
			errText: "finding whom checkpoint GC runs as: getting experiment -1",
		},
		{
			name: "simple failure",
			args: args{
				rm: func() *mocks.ResourceManager {
					var r mocks.ResourceManager

					r.On("ResolveResourcePool", mock.Anything, mock.Anything, mock.Anything, mock.Anything).
						Return(rm.ResourcePoolName(""), fmt.Errorf("rm is down or something"))

					return &r
				}(),
				as: func(t *testing.T) *allocationmocks.AllocationService {
					return &allocationmocks.AllocationService{}
				},
				toDeleteCheckpoints: []uuid.UUID{uuid.New()},
			},
			wantErr: true,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tmp := task.DefaultService
			task.DefaultService = tt.args.as(t)
			defer func() { task.DefaultService = tmp }()

			jobID := db.RequireMockJob(t, pgDB, &user.ID)
			exp := &model.Experiment{
				JobID:     model.NewJobID(),
				State:     model.CompletedState,
				OwnerID:   &user.ID,
				ProjectID: 1,
				StartTime: time.Now(),
				Config:    schemas.WithDefaults(minExpConfig).AsLegacy(),
			}
			require.NoError(t, pgDB.AddExperiment(exp, []byte{}, schemas.WithDefaults(minExpConfig)))
			expID := exp.ID
			if tt.args.unknownExperiment {
				expID = -1
			}

			err := runCheckpointGCTask(
				tt.args.rm,
				pgDB,
				model.NewTaskID(),
				jobID,
				time.Now(),
				tasks.TaskSpec{UserSessionToken: "the-stopping-experiment's-token"}, //nolint:exhaustruct
				expID,
				expconf.LegacyConfig{}, //nolint:exhaustruct
				nil,
				tt.args.toDeleteCheckpoints,
				tt.args.checkpointGlobs,
				tt.args.deleteTensorboards,
				nil,
			)
			if (err != nil) != tt.wantErr {
				t.Errorf("runCheckpointGCTask() error = %v, wantErr %v", err, tt.wantErr)
			}
			if tt.errText != "" {
				require.ErrorContains(t, err, tt.errText)
			}

			require.True(t, tt.args.rm.AssertExpectations(t))
		})
	}
}

// The GC that an experiment starts when it stops runs as its owner, as before, now with no user
// session: none is minted for it, and the experiment's own token, which its spec carries and which
// stop() revokes right after starting the GC, is dropped rather than passed on.
func TestEndOfExperimentCheckpointGCRunsAsOwner(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	owner := addGCTestOwner(t)
	specs := captureCheckpointGC(t)
	exp, ckpt := createGCTestExperiment(ctx, t, api, owner)

	// The experiment's spec as core_experiment.go, restore.go and newExperiment build it.
	expSpec := *api.m.taskSpec
	expSpec.Owner = &owner
	aug := gcTestOwnerAUG
	expSpec.AgentUserGroup = &aug
	token, err := user.StartSession(ctx, &owner)
	require.NoError(t, err)
	expSpec.UserSessionToken = token

	// What internalExperiment.stop() does.
	taskSpec, err := expSpec.Clone()
	require.NoError(t, err)
	toGC, err := experiment.ExperimentCheckpointsToGCRaw(ctx, exp.ID, 0, 0, 0)
	require.NoError(t, err)
	require.Equal(t, []uuid.UUID{uuid.MustParse(ckpt)}, toGC)
	require.NoError(t, runCheckpointGCForCheckpoints(
		api.m.rm, api.m.db, exp.JobID, exp.StartTime, taskSpec, exp.ID, exp.Config, toGC,
		[]string{fullDeleteGlob}, false, nil,
	))

	spec := nextGCSpec(t, specs)
	requireGCRunsAsOwner(t, spec, owner, exp.ID)
	require.Equal(t, ckpt, spec.ToDelete)
	require.Equal(t, []string{fullDeleteGlob}, spec.CheckpointGlobs)
	require.False(t, spec.DeleteTensorboards)
}

// Directory checkpoint storage is refused only when the experiment mounts it itself, with a bind
// mount at or above its path or a pod spec, and the GC task mounts it with neither a task container
// default bind mount nor a pod spec. Storage that the experiment does not mount was ephemeral.
//
//nolint:exhaustruct
func TestCheckpointGCSeesStorage(t *testing.T) {
	dir := func(p string) expconf.CheckpointStorageConfig {
		return expconf.CheckpointStorageConfig{
			RawDirectoryConfig: &expconf.DirectoryConfig{RawContainerPath: &p},
		}
	}
	sharedFS := expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr("/srv")},
	}
	// The experiment's bind mounts, at these container paths.
	exp := func(paths ...string) expconf.LegacyConfig {
		var c expconf.LegacyConfig
		for _, p := range paths {
			c.BindMounts = append(c.BindMounts,
				expconf.BindMount{RawHostPath: "/h", RawContainerPath: p})
		}
		return c
	}
	expPodSpec := expconf.LegacyConfig{
		Environment: expconf.EnvironmentConfig{RawPodSpec: &expconf.PodSpec{}},
	}
	// The GC task's bind mounts from the task container defaults, at these container paths.
	gc := func(paths ...string) model.TaskContainerDefaultsConfig {
		var tcd model.TaskContainerDefaultsConfig
		for _, p := range paths {
			tcd.BindMounts = append(tcd.BindMounts, model.BindMount{HostPath: "/h", ContainerPath: p})
		}
		return tcd
	}
	gcPodSpec := model.TaskContainerDefaultsConfig{CheckpointGCPodSpec: &k8sV1.Pod{}}
	cpuPodSpec := model.TaskContainerDefaultsConfig{CPUPodSpec: &k8sV1.Pod{}}
	gpuPodSpec := model.TaskContainerDefaultsConfig{GPUPodSpec: &k8sV1.Pod{}}

	for _, tc := range []struct {
		name    string
		storage expconf.CheckpointStorageConfig
		exp     expconf.LegacyConfig
		tcd     model.TaskContainerDefaultsConfig
		sees    bool
	}{
		{"shared_fs is always mounted", sharedFS, exp("/mnt/ckpts"), gc(), true},

		// The experiment does not mount it: its checkpoints went with its containers.
		{"directory the experiment does not mount", dir("/mnt/ckpts/run"), exp(), gc(), true},
		{"an experiment mount elsewhere", dir("/mnt/ckpts/run"), exp("/hooks"), gc(), true},
		{"an experiment mount that only shares a prefix", dir("/mnt/ckpts/run"), exp("/mnt/ck"), gc(), true},
		{"an experiment mount under the directory", dir("/mnt/ckpts"), exp("/mnt/ckpts/run"), gc(), true},

		// The experiment mounts it.
		{"experiment mount above, GC none", dir("/mnt/ckpts/run"), exp("/mnt/ckpts"), gc(), false},
		{"experiment mount at its path, GC none", dir("/mnt/ckpts/run"), exp("/mnt/ckpts/run/"), gc(), false},
		{"experiment mount at the root, GC none", dir("/mnt/ckpts"), exp("/"), gc(), false},
		{"relative experiment mount, GC none", dir("ckpts/run"), exp("ckpts"), gc(), false},
		{"experiment pod spec, GC none", dir("/mnt/ckpts/run"), expPodSpec, gc(), false},

		// The GC task mounts it as well.
		{"GC mount at its path", dir("/mnt/ckpts/run"), exp("/mnt/ckpts"), gc("/mnt/ckpts/run/"), true},
		{"GC mount above", dir("/mnt/ckpts/run"), exp("/mnt/ckpts"), gc("/opt", "/mnt/ckpts"), true},
		{"GC mount at the root", dir("/mnt/ckpts"), exp("/mnt"), gc("/"), true},
		{"relative GC mount", dir("ckpts/run"), exp("ckpts"), gc("ckpts"), true},
		{"GC mount that only shares a prefix", dir("/mnt/ckpts/run"), exp("/mnt/ckpts"), gc("/mnt/ck"), false},
		{"GC mount under the directory", dir("/mnt/ckpts"), exp("/mnt"), gc("/mnt/ckpts/run"), false},
		{"checkpoint_gc_pod_spec", dir("/mnt/ckpts"), expPodSpec, gcPodSpec, true},
		{"cpu_pod_spec", dir("/mnt/ckpts"), exp("/mnt/ckpts"), cpuPodSpec, true},
		{"gpu_pod_spec", dir("/mnt/ckpts"), exp("/mnt/ckpts"), gpuPodSpec, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkpointGCSeesStorage(tc.storage, tc.exp, tc.tcd)
			if tc.sees {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "checkpoint_gc_pod_spec")
			}
		})
	}
}
