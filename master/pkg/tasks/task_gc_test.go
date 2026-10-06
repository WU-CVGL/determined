
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
			},
		},
		// Without checkpoint_gc_pod_spec, the CPU pod spec.
		"CPUPodSpecTestCase": {
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

// The GC task's pod spec is checkpoint_gc_pod_spec, else cpu_pod_spec, with gpu_pod_spec merged
// under it by Kubernetes strategic merge, as before; with only gpu_pod_spec set it is that one, and
// with none set there is none. GCPodSpec, which checkpointGCSeesStorage reads, gives the same.
//
//nolint:exhaustruct
func Test_GCCkptSpec_ToTaskSpecPodSpec(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv/"))
	pod := func(name string) *k8sV1.Pod {
		return &k8sV1.Pod{Spec: k8sV1.PodSpec{Volumes: []k8sV1.Volume{{Name: name}}}}
	}
	storage := schemas.WithDefaults(expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr("/srv/gc-ckpts")},
	})

	for _, tc := range []struct {
		name    string
		tcd     model.TaskContainerDefaultsConfig
		volumes []string // nil for no pod spec
	}{
		{"none", model.TaskContainerDefaultsConfig{}, nil},
		{"gpu only", model.TaskContainerDefaultsConfig{GPUPodSpec: pod("gpu")}, []string{"gpu"}},
		{"cpu only", model.TaskContainerDefaultsConfig{CPUPodSpec: pod("cpu")}, []string{"cpu"}},
		{
			"cpu and gpu",
			model.TaskContainerDefaultsConfig{CPUPodSpec: pod("cpu"), GPUPodSpec: pod("gpu")},
			[]string{"cpu", "gpu"},
		},
		{
			"checkpoint gc only",
			model.TaskContainerDefaultsConfig{CheckpointGCPodSpec: pod("gc")},
			[]string{"gc"},
		},
		{
			"all three",
			model.TaskContainerDefaultsConfig{
				CheckpointGCPodSpec: pod("gc"), CPUPodSpec: pod("cpu"), GPUPodSpec: pod("gpu"),
			},
			[]string{"gc", "gpu"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := GCCkptSpec{
				Base: TaskSpec{
					TaskContainerDefaults: tc.tcd,
					AgentUserGroup:        &model.AgentUserGroup{UID: 4242, GID: 4343},
				},
				ExperimentID:      1,
				CheckpointStorage: storage,
			}.ToTaskSpec()
			require.Equal(t, GCPodSpec(tc.tcd), res.Environment.PodSpec())

			if tc.volumes == nil {
				require.Nil(t, res.Environment.PodSpec())
				return
			}
			require.NotNil(t, res.Environment.PodSpec())
			var volumes []string
			for _, v := range res.Environment.PodSpec().Spec.Volumes {
				volumes = append(volumes, v.Name)
			}
			require.Equal(t, tc.volumes, volumes)
		})
	}
}

// The GC task's environment variables, image, bind mounts and pod spec come from the task container
// defaults and the checkpoint storage, which is the only part of the experiment's config that the
// spec has.
//
//nolint:exhaustruct
func Test_GCCkptSpec_ToTaskSpecTakesNoExperimentEnvironment(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../../static/srv/"))

	storage := schemas.WithDefaults(expconf.CheckpointStorageConfig{
		RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: ptrs.Ptr("/srv/gc-ckpts")},
	})
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
		ExperimentID:      1,
		CheckpointStorage: storage,
		ToDelete:          "7f3c7a6e-1f7e-4f43-9c9e-0e0f5c3e0001",
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
	require.Equal(t, GCMounts(tcd, storage), res.Mounts)

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
