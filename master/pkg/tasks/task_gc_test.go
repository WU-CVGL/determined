//go:build integration
// +build integration

package tasks

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/docker/docker/api/types/mount"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/device"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

//nolint:exhaustruct
func Test_GCCkptSpec_ToTaskSpec(t *testing.T) {
	tests := map[string]struct {
		expectedType         model.TaskType
		expectedEntrypoint   string
		expectedDescription  string
		expectedExtraEnvVars map[string]string
		expectedPodSpec      k8sV1.Pod
		gctaskSpec           GCCkptSpec
	}{
		"CheckpointGCPodSpecOverwriteTestCase": {
			expectedDescription:  "gc",
			expectedEntrypoint:   filepath.Join("/run/determined/checkpoint_gc", etc.GCCheckpointsEntrypointResource),
			expectedType:         model.TaskTypeCheckpointGC,
			expectedExtraEnvVars: map[string]string{"DET_TASK_TYPE": string(model.TaskTypeCheckpointGC)},
			expectedPodSpec: k8sV1.Pod{
				Spec: k8sV1.PodSpec{
					Volumes: []k8sV1.Volume{
						{
							Name: "CheckpointGC Pod Spec",
						},
					},
				},
			},
			gctaskSpec: GCCkptSpec{
				Base: TaskSpec{
					TaskContainerDefaults: model.TaskContainerDefaultsConfig{
						CheckpointGCPodSpec: &k8sV1.Pod{
							Spec: k8sV1.PodSpec{
								Volumes: []k8sV1.Volume{
									{
										Name: "CheckpointGC Pod Spec",
									},
								},
							},
						},
					},
				},
				LegacyConfig: expconf.LegacyConfig{
					Environment: expconf.EnvironmentConfig{
						RawEnvironmentVariables: &expconf.EnvironmentVariablesMap{
							RawCPU:  []string{"HOME=/where/the/heart/is"},
							RawCUDA: []string{"HOME=/where/the/heart/is"},
							RawROCM: []string{"HOME=/where/the/heart/is"},
						},
						RawPodSpec: &expconf.PodSpec{
							Spec: k8sV1.PodSpec{
								Volumes: []k8sV1.Volume{
									{
										Name: "Legacy Pod Spec",
									},
								},
							},
						},
					},
				},
			},
		},
		// The experiment's pod spec is never used.
		"CPUPodSpecNotLegacyPodSpecTestCase": {
			expectedDescription:  "gc",
			expectedEntrypoint:   filepath.Join("/run/determined/checkpoint_gc", etc.GCCheckpointsEntrypointResource),
			expectedType:         model.TaskTypeCheckpointGC,
			expectedExtraEnvVars: map[string]string{"DET_TASK_TYPE": string(model.TaskTypeCheckpointGC)},
			expectedPodSpec: k8sV1.Pod{
				Spec: k8sV1.PodSpec{
					Volumes: []k8sV1.Volume{
						{
							Name: "CPU Pod Spec",
						},
					},
				},
			},
			gctaskSpec: GCCkptSpec{
				Base: TaskSpec{
					TaskContainerDefaults: model.TaskContainerDefaultsConfig{
						CPUPodSpec: &k8sV1.Pod{
							Spec: k8sV1.PodSpec{
								Volumes: []k8sV1.Volume{
									{
										Name: "CPU Pod Spec",
									},
								},
							},
						},
					},
				},
				LegacyConfig: expconf.LegacyConfig{
					Environment: expconf.EnvironmentConfig{
						RawEnvironmentVariables: &expconf.EnvironmentVariablesMap{
							RawCPU:  []string{"HOME=/where/the/heart/is"},
							RawCUDA: []string{"HOME=/where/the/heart/is"},
							RawROCM: []string{"HOME=/where/the/heart/is"},
						},
						RawPodSpec: &expconf.PodSpec{
							Spec: k8sV1.PodSpec{
								Volumes: []k8sV1.Volume{
									{
										Name: "Legacy Pod Spec",
									},
								},
							},
						},
					},
				},
			},
		},
	}

	for testCase, testVars := range tests {
		t.Run(testCase, func(t *testing.T) {
			err := etc.SetRootPath("../../static/srv/")
			require.NoError(t, err)
			res := testVars.gctaskSpec.ToTaskSpec()
			require.Equal(t, testVars.expectedDescription, strings.Split(res.Description, "-")[0])
			require.Equal(t, testVars.expectedPodSpec.Spec, res.Environment.RawPodSpec.Spec)
			require.Equal(t, testVars.expectedEntrypoint, res.Entrypoint[0])
			require.Equal(t, testVars.expectedExtraEnvVars, res.ExtraEnvVars)
			require.Equal(t, testVars.expectedType, res.TaskType)
		})
	}
}

// The GC task's environment comes from the task container defaults and the checkpoint storage, never
// from the experiment: no environment variables, bind mounts, pod spec or image of its own.
//
//nolint:exhaustruct
func Test_GCCkptSpec_ToTaskSpecTakesNoExperimentEnvironment(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv/"))

	experimentEnv := []string{
		"BASH_ENV=/hooks/run-first.sh",
		"LD_PRELOAD=/hooks/libhook.so",
		"PYTHONPATH=/hooks",
		"AWS_ACCESS_KEY_ID=owner-key",
		"HTTPS_PROXY=http://proxy.owner.example:3128",
	}
	legacy := expconf.LegacyConfig{
		CheckpointStorage: schemas.WithDefaults(expconf.CheckpointStorageConfig{
			RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr("/srv/gc-ckpts")},
		}),
		BindMounts: schemas.WithDefaults(expconf.BindMountsConfig{{
			RawHostPath:      "/home/owner/hooks",
			RawContainerPath: "/hooks",
		}}),
		Environment: schemas.WithDefaults(expconf.EnvironmentConfig{
			RawEnvironmentVariables: &expconf.EnvironmentVariablesMap{
				RawCPU: experimentEnv, RawCUDA: experimentEnv, RawROCM: experimentEnv,
			},
			RawImage: &expconf.EnvironmentImageMap{
				RawCPU: ptrs.Ptr("owner/image:cpu"), RawCUDA: ptrs.Ptr("owner/image:cuda"),
				RawROCM: ptrs.Ptr("owner/image:rocm"),
			},
			RawPodSpec: &expconf.PodSpec{Spec: k8sV1.PodSpec{
				Volumes: []k8sV1.Volume{{Name: "Experiment Pod Spec"}},
			}},
		}),
	}
	tcd := model.TaskContainerDefaultsConfig{
		Image: &model.RuntimeItem{CPU: "admin/image:cpu", CUDA: "admin/image:cuda", ROCM: "admin/image:rocm"},
		EnvironmentVariables: &model.RuntimeItems{
			CPU:  []string{"HTTPS_PROXY=http://proxy.admin.example:3128"},
			CUDA: []string{"HTTPS_PROXY=http://proxy.admin.example:3128"},
			ROCM: []string{"HTTPS_PROXY=http://proxy.admin.example:3128"},
		},
		BindMounts: model.BindMountsConfig{{
			HostPath: "/opt/admin-ca", ContainerPath: "/opt/admin-ca", ReadOnly: true, Propagation: "rprivate",
		}},
		CPUPodSpec: &k8sV1.Pod{Spec: k8sV1.PodSpec{Volumes: []k8sV1.Volume{{Name: "CPU Pod Spec"}}}},
	}
	owner := model.User{ID: 7, Username: "owner"}
	gc := GCCkptSpec{
		Base: TaskSpec{
			TaskContainerDefaults: tcd,
			Owner:                 &owner,
			AgentUserGroup:        &model.AgentUserGroup{UID: 4242, GID: 4343, User: "ou", Group: "og"},
		},
		ExperimentID: 1,
		LegacyConfig: legacy,
		ToDelete:     "7f3c7a6e-1f7e-4f43-9c9e-0e0f5c3e0001",
	}

	res := gc.ToTaskSpec()

	for _, d := range []device.Type{device.CPU, device.CUDA, device.ROCM} {
		require.Equal(t, []string{"HTTPS_PROXY=http://proxy.admin.example:3128"},
			res.Environment.EnvironmentVariables().For(d), "environment variables for %s", d)
		require.Equal(t, "admin/image:"+string(d), res.Environment.Image().For(d))
	}
	require.Equal(t, []k8sV1.Volume{{Name: "CPU Pod Spec"}}, res.Environment.PodSpec().Spec.Volumes)
	require.Equal(t, []mount.Mount{
		{
			Type: mount.TypeBind, Source: "/opt/admin-ca", Target: "/opt/admin-ca", ReadOnly: true,
			BindOptions: &mount.BindOptions{Propagation: "rprivate"},
		},
		{
			Type: mount.TypeBind, Source: "/srv/gc-ckpts", Target: expconf.DefaultSharedFSContainerPath,
			BindOptions: &mount.BindOptions{Propagation: expconf.DefaultSharedFSPropagation},
		},
	}, res.Mounts)

	// The storage configuration still reaches the task, owned by the owner's agent user.
	var storageConfig []byte
	for _, a := range res.ExtraArchives {
		for _, item := range a.Archive {
			if item.Path == "checkpoint_gc/storage_config.json" {
				storageConfig = item.Content
				require.Equal(t, 4242, item.UserID)
				require.Equal(t, 4343, item.GroupID)
			}
		}
	}
	require.Contains(t, string(storageConfig), `"host_path":"/srv/gc-ckpts"`)

	// No user session: no DET_USER_TOKEN, not even an empty one.
	envVars := res.EnvVars()
	require.NotContains(t, envVars, "DET_USER_TOKEN")
	require.Equal(t, "owner", envVars["DET_USER"])
}
