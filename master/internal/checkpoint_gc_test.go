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

// Directory checkpoint storage is collected only where the master can confirm that a GC task sees
// it at the same place as the experiment's trials did: the same host path of a bind mount, or the
// same hostPath or persistentVolumeClaim volume and subPath of a pod spec volumeMount, with the
// same path below the mount point, at the storage directory and at every mount point below it.
// Where the trials had no mount, their files went with their containers.
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
	type bind struct{ host, container string }
	// The experiment as its trials got it: its bind mounts, which the master merged over those of
	// the task container defaults when it created the experiment, and its pod spec.
	exp := func(pod *k8sV1.Pod, mounts ...bind) expconf.LegacyConfig {
		var c expconf.LegacyConfig
		for _, m := range mounts {
			c.BindMounts = append(c.BindMounts,
				expconf.BindMount{RawHostPath: m.host, RawContainerPath: m.container})
		}
		c.Environment.RawPodSpec = (*expconf.PodSpec)(pod)
		return c
	}
	// The task container defaults of the GC task.
	gc := func(mounts ...bind) model.TaskContainerDefaultsConfig {
		var tcd model.TaskContainerDefaultsConfig
		for _, m := range mounts {
			tcd.BindMounts = append(tcd.BindMounts,
				model.BindMount{HostPath: m.host, ContainerPath: m.container})
		}
		return tcd
	}
	gcPod := func(pod *k8sV1.Pod) model.TaskContainerDefaultsConfig {
		return model.TaskContainerDefaultsConfig{CheckpointGCPodSpec: pod}
	}
	cpuPod := func(pod *k8sV1.Pod) model.TaskContainerDefaultsConfig {
		return model.TaskContainerDefaultsConfig{CPUPodSpec: pod}
	}
	gpuPod := func(pod *k8sV1.Pod) model.TaskContainerDefaultsConfig {
		return model.TaskContainerDefaultsConfig{GPUPodSpec: pod}
	}

	// A pod spec whose container mounts a volume.
	type volumeMount struct {
		container, volume string
		source            k8sV1.VolumeSource
		mountPath         string
		subPath           string
		subPathExpr       string
	}
	pod := func(vms ...volumeMount) *k8sV1.Pod {
		var p k8sV1.Pod
		for _, vm := range vms {
			if vm.container == "" {
				vm.container = model.DeterminedK8ContainerName
			}
			if vm.volume == "" {
				vm.volume = "ckpts"
			}
			if vm.source != (k8sV1.VolumeSource{}) {
				p.Spec.Volumes = append(p.Spec.Volumes,
					k8sV1.Volume{Name: vm.volume, VolumeSource: vm.source})
			}
			p.Spec.Containers = append(p.Spec.Containers, k8sV1.Container{
				Name: vm.container,
				VolumeMounts: []k8sV1.VolumeMount{{
					Name: vm.volume, MountPath: vm.mountPath,
					SubPath: vm.subPath, SubPathExpr: vm.subPathExpr,
				}},
			})
		}
		return &p
	}
	pvc := func(claim string) k8sV1.VolumeSource {
		return k8sV1.VolumeSource{
			PersistentVolumeClaim: &k8sV1.PersistentVolumeClaimVolumeSource{ClaimName: claim},
		}
	}
	hostPath := func(p string) k8sV1.VolumeSource {
		return k8sV1.VolumeSource{HostPath: &k8sV1.HostPathVolumeSource{Path: p}}
	}
	emptyDir := k8sV1.VolumeSource{EmptyDir: &k8sV1.EmptyDirVolumeSource{}}
	alicePVC := pod(volumeMount{source: pvc("alice-ckpts"), mountPath: "/mnt/ckpts"})
	nodeSelectorOnly := &k8sV1.Pod{Spec: k8sV1.PodSpec{NodeSelector: map[string]string{"gc": "yes"}}}

	for _, tc := range []struct {
		name    string
		storage expconf.CheckpointStorageConfig
		exp     expconf.LegacyConfig
		tcd     model.TaskContainerDefaultsConfig
		sees    bool
	}{
		{
			"shared_fs has its own mount", sharedFS,
			exp(alicePVC, bind{"/srv/alice", "/mnt/ckpts"}), gc(), true,
		},

		// The trials had no mount there: their files went with their containers.
		{"not mounted", dir("/mnt/ckpts/run"), exp(nil), gc(), true},
		{"a trial mount elsewhere", dir("/mnt/ckpts/run"), exp(nil, bind{"/h", "/hooks"}), gc(), true},
		{
			"a trial mount that only shares a prefix", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/h", "/mnt/ck"}), gc(), true,
		},
		{
			"not mounted for the trials, mounted for GC", dir("/mnt/ckpts/run"), exp(nil),
			gc(bind{"/srv/default", "/mnt/ckpts"}), true,
		},
		{"a trial pod spec with a nodeSelector only", dir("/mnt/ckpts"), exp(nodeSelectorOnly), gc(), true},
		{
			"a trial pod spec that mounts it in a sidecar only", dir("/mnt/ckpts"),
			exp(pod(volumeMount{container: "sidecar", source: pvc("c"), mountPath: "/mnt/ckpts"})),
			gc(), true,
		},

		// Bind mounts.
		{"trial mount above, GC none", dir("/mnt/ckpts/run"), exp(nil, bind{"/h", "/mnt/ckpts"}), gc(), false},
		{
			"trial mount at its path, GC none", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/h", "/mnt/ckpts/run/"}), gc(), false,
		},
		{"trial mount at the root, GC none", dir("/mnt/ckpts"), exp(nil, bind{"/h", "/"}), gc(), false},
		{"relative trial mount, GC none", dir("ckpts/run"), exp(nil, bind{"/h", "ckpts"}), gc(), false},
		{
			"the same mount, from the task container defaults", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/default", "/mnt/ckpts"}), gc(bind{"/srv/default", "/mnt/ckpts"}), true,
		},
		{
			"the same mount at its path", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/a", "/mnt/ckpts/run"}), gc(bind{"/srv/a/", "/mnt/ckpts/run/"}), true,
		},
		{
			"the same relative mount", dir("ckpts/run"),
			exp(nil, bind{"/h", "ckpts"}), gc(bind{"/h", "ckpts"}), true,
		},
		{
			"the same host path through another mount point", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/a", "/mnt"}), gc(bind{"/opt", "/opt"}, bind{"/srv/a/ckpts", "/mnt/ckpts"}),
			true,
		},
		{
			"the same mount point, another host path", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/alice", "/mnt/ckpts"}), gc(bind{"/srv/default", "/mnt/ckpts"}), false,
		},
		{
			"the same host path, another mount point", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/a", "/mnt"}), gc(bind{"/srv/a", "/mnt/ckpts"}), false,
		},
		{
			"a GC mount at the root", dir("/mnt/ckpts"),
			exp(nil, bind{"/srv/a", "/mnt"}), gc(bind{"/", "/"}), false,
		},
		{
			"a GC mount that only shares a prefix", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/h", "/mnt/ckpts"}), gc(bind{"/h", "/mnt/ck"}), false,
		},
		{
			"a GC mount under the directory only", dir("/mnt/ckpts"),
			exp(nil, bind{"/h", "/mnt"}), gc(bind{"/h/ckpts/run", "/mnt/ckpts/run"}), false,
		},
		{
			"the longest mount decides, the same", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/a", "/mnt"}, bind{"/srv/b", "/mnt/ckpts"}),
			gc(bind{"/srv/b", "/mnt/ckpts"}), true,
		},
		{
			"the longest mount decides, another", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/a", "/mnt"}, bind{"/srv/b", "/mnt/ckpts"}),
			gc(bind{"/srv/a", "/mnt"}), false,
		},
		{
			"a trial mount below the directory, GC none", dir("/mnt/ckpts"),
			exp(nil, bind{"/h", "/mnt/ckpts/tb"}), gc(), false,
		},
		{
			"a trial mount below the directory, the same for GC", dir("/mnt/ckpts"),
			exp(nil, bind{"/srv/a", "/mnt/ckpts"}, bind{"/srv/tb", "/mnt/ckpts/tb"}),
			gc(bind{"/srv/a", "/mnt/ckpts"}, bind{"/srv/tb", "/mnt/ckpts/tb"}), true,
		},
		{
			"a trial mount below the directory that GC lacks", dir("/mnt/ckpts"),
			exp(nil, bind{"/srv/a", "/mnt/ckpts"}, bind{"/srv/tb", "/mnt/ckpts/tb"}),
			gc(bind{"/srv/a", "/mnt/ckpts"}), false,
		},
		{
			"a GC mount below the directory that the trials lacked", dir("/mnt/ckpts"),
			exp(nil, bind{"/srv/a", "/mnt/ckpts"}),
			gc(bind{"/srv/a", "/mnt/ckpts"}, bind{"/srv/x", "/mnt/ckpts/x"}), false,
		},
		{
			"a trial mount, an empty checkpoint_gc_pod_spec", dir("/mnt/ckpts"),
			exp(nil, bind{"/h", "/mnt/ckpts"}), gcPod(&k8sV1.Pod{}), false,
		},
		{
			"a trial mount, an empty cpu_pod_spec", dir("/mnt/ckpts"),
			exp(nil, bind{"/h", "/mnt/ckpts"}), cpuPod(&k8sV1.Pod{}), false,
		},
		{
			"a trial mount, an empty gpu_pod_spec", dir("/mnt/ckpts"),
			exp(nil, bind{"/h", "/mnt/ckpts"}), gpuPod(&k8sV1.Pod{}), false,
		},
		{
			"a trial mount, a GC hostPath volume of the same path", dir("/mnt/ckpts"),
			exp(nil, bind{"/srv/a", "/mnt/ckpts"}),
			gcPod(pod(volumeMount{source: hostPath("/srv/a"), mountPath: "/mnt/ckpts"})), false,
		},

		// Pod spec volumes.
		{"a trial PVC, GC none", dir("/mnt/ckpts/run"), exp(alicePVC), gc(), false},
		{
			"a trial PVC, a cpu_pod_spec with a nodeSelector only", dir("/mnt/ckpts/run"),
			exp(alicePVC), cpuPod(nodeSelectorOnly), false,
		},
		{
			"a trial PVC, a gpu_pod_spec with a nodeSelector only", dir("/mnt/ckpts/run"),
			exp(alicePVC), gpuPod(nodeSelectorOnly), false,
		},
		{
			"a trial PVC, the same in checkpoint_gc_pod_spec", dir("/mnt/ckpts/run"),
			exp(alicePVC), gcPod(alicePVC), true,
		},
		{"a trial PVC, the same in cpu_pod_spec", dir("/mnt/ckpts/run"), exp(alicePVC), cpuPod(alicePVC), true},
		{"a trial PVC, the same in gpu_pod_spec", dir("/mnt/ckpts/run"), exp(alicePVC), gpuPod(alicePVC), true},
		{
			"a trial PVC, the same claim and path as another volume at another mount point",
			dir("/mnt/ckpts/run"), exp(pod(volumeMount{source: pvc("alice-ckpts"), mountPath: "/mnt"})),
			gcPod(pod(volumeMount{
				volume: "gc", source: pvc("alice-ckpts"), mountPath: "/mnt/ckpts", subPath: "ckpts",
			})),
			true,
		},
		{
			"a trial PVC, another claim", dir("/mnt/ckpts/run"), exp(alicePVC),
			gcPod(pod(volumeMount{source: pvc("bob-ckpts"), mountPath: "/mnt/ckpts"})), false,
		},
		{
			"a trial PVC, the same claim at another subPath", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: pvc("shared"), mountPath: "/mnt/ckpts", subPath: "alice"})),
			gcPod(pod(volumeMount{source: pvc("shared"), mountPath: "/mnt/ckpts", subPath: "bob"})),
			false,
		},
		{
			"a trial PVC with a subPathExpr", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: pvc("shared"), mountPath: "/mnt/ckpts", subPathExpr: "$(U)"})),
			gcPod(pod(volumeMount{source: pvc("shared"), mountPath: "/mnt/ckpts", subPathExpr: "$(U)"})),
			false,
		},
		{
			"a trial PVC, the same in a GC sidecar only", dir("/mnt/ckpts/run"), exp(alicePVC),
			gcPod(pod(volumeMount{
				container: "sidecar", source: pvc("alice-ckpts"), mountPath: "/mnt/ckpts",
			})),
			false,
		},
		{
			"a trial PVC, a GC bind mount", dir("/mnt/ckpts/run"), exp(alicePVC),
			gc(bind{"/srv/alice-ckpts", "/mnt/ckpts"}), false,
		},
		{
			"a trial hostPath volume, the same for GC", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: hostPath("/srv/a"), mountPath: "/mnt/ckpts"})),
			gcPod(pod(volumeMount{source: hostPath("/srv/a/"), mountPath: "/mnt/ckpts"})), true,
		},
		{
			"a trial hostPath volume, another path", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: hostPath("/srv/a"), mountPath: "/mnt/ckpts"})),
			gcPod(pod(volumeMount{source: hostPath("/srv/b"), mountPath: "/mnt/ckpts"})), false,
		},
		{
			"a trial emptyDir volume, the same for GC", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})),
			gcPod(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})), false,
		},
		{
			"a trial volumeMount without its volume", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{mountPath: "/mnt/ckpts"})), gcPod(pod(volumeMount{mountPath: "/mnt/ckpts"})),
			false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkpointGCSeesStorage(tc.storage, tc.exp, tc.tcd)
			if tc.sees {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "checkpoints are kept")
			}
		})
	}
}
