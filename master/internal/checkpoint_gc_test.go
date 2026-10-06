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
							// It runs as the experiment's owner, without the user session of the
							// spec it was given: the end-of-experiment GC gets a copy of the
							// experiment's spec, whose session is revoked when the experiment stops.
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

// The GC that an experiment starts when it stops runs as its owner, as before, and without the
// experiment's user session, which stop() revokes right after starting the GC.
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

//nolint:exhaustruct
func TestCheckpointGCSeesStorage(t *testing.T) {
	dir := func(p string) expconf.CheckpointStorageConfig {
		return expconf.CheckpointStorageConfig{
			RawDirectoryConfig: &expconf.DirectoryConfig{RawContainerPath: &p},
		}
	}
	mounts := func(paths ...string) model.TaskContainerDefaultsConfig {
		var tcd model.TaskContainerDefaultsConfig
		for _, p := range paths {
			tcd.BindMounts = append(tcd.BindMounts, model.BindMount{HostPath: "/h", ContainerPath: p})
		}
		return tcd
	}
	gcPodSpec := model.TaskContainerDefaultsConfig{CheckpointGCPodSpec: &k8sV1.Pod{}}
	sharedFS := expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr("/srv")},
	}

	for _, tc := range []struct {
		name    string
		storage expconf.CheckpointStorageConfig
		tcd     model.TaskContainerDefaultsConfig
		sees    bool
	}{
		{"shared_fs is always mounted", sharedFS, mounts(), true},
		{"unmounted directory", dir("/mnt/ckpts/run"), mounts(), false},
		{"directory mounted at its path", dir("/mnt/ckpts/run"), mounts("/mnt/ckpts/run/"), true},
		{"directory under a mount", dir("/mnt/ckpts/run"), mounts("/opt", "/mnt/ckpts"), true},
		{"a mount that only shares a prefix", dir("/mnt/ckpts/run"), mounts("/mnt/ck"), false},
		{"a mount under the directory", dir("/mnt/ckpts"), mounts("/mnt/ckpts/run"), false},
		{"relative paths are in the work dir", dir("ckpts/run"), mounts("ckpts"), true},
		{"the root", dir("/mnt/ckpts"), mounts("/"), true},
		{"a pod spec may mount it", dir("/mnt/ckpts"), gcPodSpec, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkpointGCSeesStorage(tc.storage, tc.tcd)
			if tc.sees {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "checkpoint_gc_pod_spec")
			}
		})
	}
}
