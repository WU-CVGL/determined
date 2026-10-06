package internal

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// Directory checkpoint storage is collected only where the master can confirm that a GC task sees
// it at the same place as the experiment's trials did: the same host path of a bind mount, or the
// same hostPath, persistentVolumeClaim or nfs volume and subPath of a pod spec volumeMount, with the
// same path below the mount point, at the storage directory and at every mount point below it.
// Where the trials had no mount, or an emptyDir volume, their files went with their containers.
//
//nolint:exhaustruct
func TestCheckpointGCSeesStorage(t *testing.T) {
	dir := func(p string) expconf.CheckpointStorageConfig {
		return expconf.CheckpointStorageConfig{
			RawDirectoryConfig: &expconf.DirectoryConfig{RawContainerPath: &p},
		}
	}
	sharedFSAt := func(host string) expconf.CheckpointStorageConfig {
		return expconf.CheckpointStorageConfig{
			RawSharedFSConfig: &expconf.SharedFSConfig{RawHostPath: &host},
		}
	}
	sharedFS := sharedFSAt("/srv")
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
	// An experiment whose own checkpoint storage is shared_fs at this host path, which its trials
	// had mounted at /determined_shared_fs, whatever storage the checkpoints were saved to.
	onSharedFS := func(host string, mounts ...bind) expconf.LegacyConfig {
		c := exp(nil, mounts...)
		c.CheckpointStorage = sharedFSAt(host)
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
			i := slices.IndexFunc(p.Spec.Containers, func(c k8sV1.Container) bool {
				return c.Name == vm.container
			})
			if i < 0 {
				i = len(p.Spec.Containers)
				p.Spec.Containers = append(p.Spec.Containers, k8sV1.Container{Name: vm.container})
			}
			p.Spec.Containers[i].VolumeMounts = append(p.Spec.Containers[i].VolumeMounts,
				k8sV1.VolumeMount{
					Name: vm.volume, MountPath: vm.mountPath,
					SubPath: vm.subPath, SubPathExpr: vm.subPathExpr,
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
	nfs := func(server, p string) k8sV1.VolumeSource {
		return k8sV1.VolumeSource{NFS: &k8sV1.NFSVolumeSource{Server: server, Path: p}}
	}
	emptyDir := k8sV1.VolumeSource{EmptyDir: &k8sV1.EmptyDirVolumeSource{}}
	csi := k8sV1.VolumeSource{CSI: &k8sV1.CSIVolumeSource{Driver: "csi.example.com"}}
	aliceNFS := pod(volumeMount{source: nfs("nfs1", "/export/alice"), mountPath: "/mnt/ckpts"})
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
		{
			"shared_fs has its own mount, the experiment on shared_fs", sharedFS,
			onSharedFS("/srv"), gc(), true,
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

		// The mount of the experiment's shared_fs storage, which the trials had too.
		{
			"under the experiment's shared_fs mount, GC none", dir("/determined_shared_fs/mine"),
			onSharedFS("/srv/shared"), gc(), false,
		},
		{
			"at the experiment's shared_fs mount, GC none", dir("/determined_shared_fs"),
			onSharedFS("/srv/shared"), gc(), false,
		},
		{
			"under the experiment's shared_fs mount, the same from the task container defaults",
			dir("/determined_shared_fs/mine"), onSharedFS("/srv/shared"),
			gc(bind{"/srv/shared", "/determined_shared_fs"}), true,
		},
		{
			"under the experiment's shared_fs mount, the same host path at its path",
			dir("/determined_shared_fs/mine"), onSharedFS("/srv/shared"),
			gc(bind{"/srv/shared/mine", "/determined_shared_fs/mine"}), true,
		},
		{
			"under the experiment's shared_fs mount, another host path",
			dir("/determined_shared_fs/mine"), onSharedFS("/srv/shared"),
			gc(bind{"/srv/other", "/determined_shared_fs"}), false,
		},
		{
			"under the experiment's shared_fs mount, a trial mount deeper", dir("/determined_shared_fs/mine"),
			onSharedFS("/srv/shared", bind{"/srv/alice", "/determined_shared_fs/mine"}),
			gc(bind{"/srv/alice", "/determined_shared_fs/mine"}), true,
		},
		{
			"elsewhere, the experiment on shared_fs", dir("/mnt/ckpts"), onSharedFS("/srv/shared"),
			gc(), true,
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
			"a trial nfs volume, GC none", dir("/mnt/ckpts/run"), exp(aliceNFS), gc(), false,
		},
		{
			"a trial nfs volume, a cpu_pod_spec with a nodeSelector only", dir("/mnt/ckpts/run"),
			exp(aliceNFS), cpuPod(nodeSelectorOnly), false,
		},
		{
			"a trial nfs volume, the same in checkpoint_gc_pod_spec", dir("/mnt/ckpts/run"),
			exp(aliceNFS), gcPod(aliceNFS), true,
		},
		{
			"a trial nfs volume, the same in cpu_pod_spec", dir("/mnt/ckpts/run"),
			exp(aliceNFS), cpuPod(aliceNFS), true,
		},
		{
			"a trial nfs volume, the same export through a subPath elsewhere", dir("/mnt/ckpts/run"),
			exp(aliceNFS),
			gcPod(pod(volumeMount{source: nfs("nfs1", "/export"), mountPath: "/mnt/ckpts", subPath: "alice"})),
			true,
		},
		{
			"a trial nfs volume, another server", dir("/mnt/ckpts/run"), exp(aliceNFS),
			gcPod(pod(volumeMount{source: nfs("nfs2", "/export/alice"), mountPath: "/mnt/ckpts"})),
			false,
		},
		{
			"a trial nfs volume, another export", dir("/mnt/ckpts/run"), exp(aliceNFS),
			gcPod(pod(volumeMount{source: nfs("nfs1", "/export/bob"), mountPath: "/mnt/ckpts"})),
			false,
		},
		{
			"a trial nfs volume, another subPath", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: nfs("nfs1", "/export"), mountPath: "/mnt/ckpts", subPath: "alice"})),
			gcPod(pod(volumeMount{source: nfs("nfs1", "/export"), mountPath: "/mnt/ckpts", subPath: "bob"})),
			false,
		},
		{
			"a trial nfs volume, a GC hostPath volume of the export path", dir("/mnt/ckpts/run"),
			exp(aliceNFS),
			gcPod(pod(volumeMount{source: hostPath("/export/alice"), mountPath: "/mnt/ckpts"})),
			false,
		},
		{
			"a trial emptyDir volume, the same for GC", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})),
			gcPod(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})), true,
		},
		{
			"a trial emptyDir volume, GC none", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})), gc(), true,
		},
		{
			"a trial emptyDir volume, a GC mount", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})),
			gc(bind{"/srv/default", "/mnt/ckpts"}), true,
		},
		{
			"a trial emptyDir volume under a trial mount", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"}), bind{"/srv/a", "/mnt"}),
			gc(), true,
		},
		{
			"a trial PVC under a trial emptyDir volume, GC none", dir("/mnt/ckpts"),
			exp(pod(
				volumeMount{volume: "scratch", source: emptyDir, mountPath: "/mnt/ckpts"},
				volumeMount{source: pvc("tb"), mountPath: "/mnt/ckpts/tb"},
			)),
			gc(), false,
		},
		{
			"a trial mount, a GC emptyDir volume", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/a", "/mnt/ckpts"}),
			gcPod(pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"})), false,
		},
		{
			"a trial csi volume, the same for GC", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: csi, mountPath: "/mnt/ckpts"})),
			gcPod(pod(volumeMount{source: csi, mountPath: "/mnt/ckpts"})), false,
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

	// A refusal says what the trials had and what gives checkpoint GC tasks the same: a task
	// container default bind mount for a bind mount, a GC pod spec volume for a volume, and nothing
	// for a volume the master cannot match. It names no host path, claim or server of the GC task.
	for _, tc := range []struct {
		name    string
		storage expconf.CheckpointStorageConfig
		exp     expconf.LegacyConfig
		tcd     model.TaskContainerDefaultsConfig
		says    []string
		not     []string
	}{
		{
			"a bind mount", dir("/mnt/ckpts/run"),
			exp(nil, bind{"/srv/alice", "/mnt/ckpts"}), gc(bind{"/srv/default", "/mnt/ckpts"}),
			[]string{
				"had /mnt/ckpts/run on a bind mount, at host path /srv/alice/run",
				"would have a different host path there",
				"kept until task_container_defaults.bind_mounts mounts host path /srv/alice/run at " +
					"/mnt/ckpts/run",
			},
			[]string{"pod_spec (else", "/srv/default"},
		},
		{
			"the experiment's shared_fs mount", dir("/determined_shared_fs/mine"),
			onSharedFS("/srv/shared"), gc(),
			[]string{
				"had /determined_shared_fs/mine on a bind mount, at host path /srv/shared/mine",
				"would have no mount there",
				"kept until task_container_defaults.bind_mounts mounts host path /srv/shared/mine at " +
					"/determined_shared_fs/mine",
			},
			[]string{"pod_spec (else"},
		},
		{
			"a persistentVolumeClaim volume", dir("/mnt/ckpts/run"), exp(alicePVC), cpuPod(nodeSelectorOnly),
			[]string{
				"had /mnt/ckpts/run on the persistentVolumeClaim alice-ckpts, at /run in the volume",
				"would have no mount there",
				"kept until checkpoint_gc_pod_spec (else cpu_pod_spec, merged over gpu_pod_spec) mounts " +
					"a persistentVolumeClaim volume in determined-container so that /run in the claim " +
					"alice-ckpts is at /mnt/ckpts/run",
			},
			[]string{"bind_mounts mounts"},
		},
		{
			"a hostPath volume", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: hostPath("/srv/a"), mountPath: "/mnt/ckpts"})),
			gc(bind{"/srv/a", "/mnt/ckpts"}),
			[]string{
				"had /mnt/ckpts/run on a hostPath volume, at host path /srv/a/run",
				"would have a bind mount there",
				"mounts a hostPath volume in determined-container so that host path /srv/a/run is at " +
					"/mnt/ckpts/run",
			},
			[]string{"bind_mounts mounts"},
		},
		{
			"an nfs volume", dir("/mnt/ckpts/run"), exp(aliceNFS),
			gcPod(pod(volumeMount{source: nfs("nfs2", "/export/alice"), mountPath: "/mnt/ckpts"})),
			[]string{
				"had /mnt/ckpts/run on an nfs volume of server nfs1, at /export/alice/run on the server",
				"would have a different nfs volume or path there",
				"mounts an nfs volume in determined-container so that /export/alice/run on the server " +
					"nfs1 is at /mnt/ckpts/run",
			},
			[]string{"nfs2", "bind_mounts mounts"},
		},
		{
			"a volume the master cannot match", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: csi, mountPath: "/mnt/ckpts"})),
			gcPod(pod(volumeMount{source: csi, mountPath: "/mnt/ckpts"})),
			[]string{"a volume that the master cannot match", "whatever checkpoint GC tasks mount"},
			[]string{"kept until"},
		},
	} {
		t.Run("message/"+tc.name, func(t *testing.T) {
			err := checkpointGCSeesStorage(tc.storage, tc.exp, tc.tcd)
			require.ErrorContains(t, err, "checkpoints are kept")
			for _, s := range tc.says {
				require.ErrorContains(t, err, s)
			}
			for _, s := range tc.not {
				require.NotContains(t, err.Error(), s)
			}
		})
	}
}
