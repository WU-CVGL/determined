package internal

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm/agentrm"
	"github.com/determined-ai/determined/master/internal/rm/dispatcherrm"
	"github.com/determined-ai/determined/master/internal/rm/kubernetesrm"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// The Kubernetes resource manager applies pod specs, and the agent and dispatcher ones do not. For
// a resource manager that does not say, such as a mock, the master cannot tell.
//
//nolint:exhaustruct
func TestPodSpecsOf(t *testing.T) {
	require.Equal(t, podSpecsApplied, podSpecsOf(&kubernetesrm.ResourceManager{}, "default"))
	require.Equal(t, podSpecsIgnored, podSpecsOf(&agentrm.ResourceManager{}, "default"))
	require.Equal(t, podSpecsIgnored,
		podSpecsOf(&dispatcherrm.DispatcherResourceManager{}, "default"))
	require.Equal(t, podSpecsUnknown, podSpecsOf(&mocks.ResourceManager{}, "default"))
}

// Directory checkpoint storage is collected only where the master finds that a GC task sees it at
// the same place as the experiment's trials did: the same host path of a bind mount, or the
// same hostPath, persistentVolumeClaim or nfs volume and subPath of a pod spec volumeMount, with the
// same path below the mount point, at the storage directory and at every mount point below it. A
// host path, of a hostPath volume or a bind mount, must also be on the node that the trials' pod
// spec pins them to, if it pins them to one; a host path of trials that are not pinned to a node is
// assumed to be the same on every node. Where the trials had no mount, or an emptyDir volume, the
// check does not apply and the existing handling is kept.
//
// The first two tables are on Kubernetes, which applies the pod specs of the trials and of GC. The
// last one has the agent resource manager, which applies none, and a resource manager that the
// master cannot find, for the trials or for GC.
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

	// Pod specs pinned to a node: by a kubernetes.io/hostname nodeSelector, by nodeName, or by a
	// required node affinity on the hostname.
	on := func(node string, p *k8sV1.Pod) *k8sV1.Pod {
		p = p.DeepCopy()
		p.Spec.NodeSelector = map[string]string{"kubernetes.io/hostname": node}
		return p
	}
	onNodeName := func(node string, p *k8sV1.Pod) *k8sV1.Pod {
		p = p.DeepCopy()
		p.Spec.NodeName = node
		return p
	}
	onAffinity := func(p *k8sV1.Pod, nodes ...string) *k8sV1.Pod {
		p = p.DeepCopy()
		p.Spec.Affinity = &k8sV1.Affinity{NodeAffinity: &k8sV1.NodeAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: &k8sV1.NodeSelector{
				NodeSelectorTerms: []k8sV1.NodeSelectorTerm{{
					MatchExpressions: []k8sV1.NodeSelectorRequirement{{
						Key: "kubernetes.io/hostname", Operator: k8sV1.NodeSelectorOpIn, Values: nodes,
					}},
				}},
			},
		}}
		return p
	}
	// The task container defaults of a GC task with these bind mounts and checkpoint_gc_pod_spec.
	gcWith := func(pod *k8sV1.Pod, mounts ...bind) model.TaskContainerDefaultsConfig {
		tcd := gc(mounts...)
		tcd.CheckpointGCPodSpec = pod
		return tcd
	}
	dataCkpts := pod(volumeMount{source: hostPath("/data/ckpts"), mountPath: "/mnt/ckpts"})
	// A gpu_pod_spec with that volume on node-a, and a cpu_pod_spec on this node, whose hostname is
	// merged over the gpu_pod_spec's. The cpu_pod_spec lists determined-container, else the strategic
	// merge would drop the gpu_pod_spec's containers and their volumeMounts.
	cpuOverGPUData := func(node string) model.TaskContainerDefaultsConfig {
		cpu := &k8sV1.Pod{Spec: k8sV1.PodSpec{
			Containers: []k8sV1.Container{{Name: model.DeterminedK8ContainerName}},
		}}
		return model.TaskContainerDefaultsConfig{
			GPUPodSpec: on("node-a", dataCkpts), CPUPodSpec: on(node, cpu),
		}
	}

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

		// The trials had no mount there: the check does not apply.
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

		// A host path is on the node where the pod runs: a GC task must be pinned to the node that
		// the trials were pinned to.
		{
			"a trial hostPath volume on a node, the same path on another node", dir("/mnt/ckpts"),
			exp(on("node-a", dataCkpts)), cpuPod(on("node-b", dataCkpts)), false,
		},
		{
			"a trial hostPath volume on a node, the same path on the same node", dir("/mnt/ckpts"),
			exp(on("node-a", dataCkpts)), cpuPod(on("node-a", dataCkpts)), true,
		},
		{
			"a trial hostPath volume on a node, the same path, GC not pinned", dir("/mnt/ckpts"),
			exp(on("node-a", dataCkpts)), gcPod(dataCkpts), false,
		},
		{
			"a trial hostPath volume on a node, another path on the same node", dir("/mnt/ckpts"),
			exp(on("node-a", dataCkpts)),
			gcPod(on("node-a", pod(volumeMount{source: hostPath("/data/other"), mountPath: "/mnt/ckpts"}))),
			false,
		},
		{
			"a trial hostPath volume by nodeName, the same path by a hostname nodeSelector",
			dir("/mnt/ckpts"), exp(onNodeName("node-a", dataCkpts)), gcPod(on("node-a", dataCkpts)), true,
		},
		{
			"a trial hostPath volume by hostname affinity, the same path on another node",
			dir("/mnt/ckpts"), exp(onAffinity(dataCkpts, "node-a")), gcPod(onNodeName("node-b", dataCkpts)),
			false,
		},
		{
			"a trial hostPath volume by hostname affinity, the same path by the same affinity",
			dir("/mnt/ckpts"), exp(onAffinity(dataCkpts, "node-a")), gcPod(onAffinity(dataCkpts, "node-a")),
			true,
		},
		{
			"a trial hostPath volume on a node, the same path on the same node in gpu_pod_spec",
			dir("/mnt/ckpts"), exp(on("node-a", dataCkpts)), gpuPod(on("node-a", dataCkpts)), true,
		},
		{
			"a trial hostPath volume on a node, the same path in gpu_pod_spec, cpu_pod_spec on the same node",
			dir("/mnt/ckpts"), exp(on("node-a", dataCkpts)), cpuOverGPUData("node-a"), true,
		},
		{
			"a trial hostPath volume on a node, the same path in gpu_pod_spec, cpu_pod_spec on another node",
			dir("/mnt/ckpts"), exp(on("node-a", dataCkpts)), cpuOverGPUData("node-b"), false,
		},
		{
			"a trial hostPath volume not pinned, the same path on a node", dir("/mnt/ckpts"),
			exp(dataCkpts), gcPod(on("node-b", dataCkpts)), true,
		},
		{
			// Not read as a pin: the master takes the host path to be the same on every node.
			"a trial hostPath volume on one of two nodes, the same path, GC not pinned", dir("/mnt/ckpts"),
			exp(onAffinity(dataCkpts, "node-a", "node-b")), gcPod(dataCkpts), true,
		},
		{
			"a trial bind mount on a node, the same from the task container defaults, GC not pinned",
			dir("/mnt/ckpts"), exp(on("node-a", &k8sV1.Pod{}), bind{"/data/ckpts", "/mnt/ckpts"}),
			gc(bind{"/data/ckpts", "/mnt/ckpts"}), false,
		},
		{
			"a trial bind mount on a node, the same on the same node", dir("/mnt/ckpts"),
			exp(on("node-a", &k8sV1.Pod{}), bind{"/data/ckpts", "/mnt/ckpts"}),
			gcWith(on("node-a", &k8sV1.Pod{}), bind{"/data/ckpts", "/mnt/ckpts"}), true,
		},
		{
			"under the experiment's shared_fs mount on a node, the same host path, GC not pinned",
			dir("/determined_shared_fs/mine"),
			func() expconf.LegacyConfig {
				c := onSharedFS("/srv/shared")
				c.Environment.RawPodSpec = (*expconf.PodSpec)(on("node-a", &k8sV1.Pod{}))
				return c
			}(),
			gc(bind{"/srv/shared", "/determined_shared_fs"}), false,
		},
		{
			"a trial PVC on a node, the same claim, GC not pinned", dir("/mnt/ckpts/run"),
			exp(on("node-a", alicePVC)), gcPod(alicePVC), true,
		},
		{
			"a trial nfs volume on a node, the same export, GC on another node", dir("/mnt/ckpts/run"),
			exp(on("node-a", aliceNFS)), gcPod(on("node-b", aliceNFS)), true,
		},
		{
			"a trial emptyDir volume on a node, GC none", dir("/mnt/ckpts/run"),
			exp(on("node-a", pod(volumeMount{source: emptyDir, mountPath: "/mnt/ckpts"}))), gc(), true,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := checkpointGCSeesStorage(tc.storage, tc.exp, podSpecsApplied, tc.tcd, podSpecsApplied)
			if tc.sees {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "checkpoints are kept")
			}
		})
	}

	// A refusal names the trials' place and the setting that gives checkpoint GC tasks the same: a
	// task container default bind mount for a bind mount, a GC pod spec volume for a volume, a GC pod
	// spec pinned to the trials' node for a host path there, and nothing for a volume the master
	// cannot match. It names no host path, claim, server or node of the GC task.
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
				"its checkpoint storage is the directory /mnt/ckpts/run. The experiment's trials had " +
					"/mnt/ckpts/run on a bind mount of host path /srv/alice/run, and a checkpoint GC task, " +
					"which takes no bind_mounts or pod_spec from the experiment, would not have the same " +
					"there. Its checkpoints are kept until task_container_defaults.bind_mounts mounts host " +
					"path /srv/alice/run at /mnt/ckpts/run",
			},
			[]string{"pod_spec (else", "/srv/default", "node"},
		},
		{
			"the experiment's shared_fs mount", dir("/determined_shared_fs/mine"),
			onSharedFS("/srv/shared"), gc(),
			[]string{
				"had /determined_shared_fs/mine on a bind mount of host path /srv/shared/mine,",
				"kept until task_container_defaults.bind_mounts mounts host path /srv/shared/mine at " +
					"/determined_shared_fs/mine",
			},
			[]string{"pod_spec (else"},
		},
		{
			"a persistentVolumeClaim volume", dir("/mnt/ckpts/run"), exp(alicePVC), cpuPod(nodeSelectorOnly),
			[]string{
				"had /mnt/ckpts/run on path /run of the persistentVolumeClaim alice-ckpts,",
				"kept until checkpoint_gc_pod_spec (else cpu_pod_spec, merged over gpu_pod_spec) mounts " +
					"path /run of the persistentVolumeClaim alice-ckpts at /mnt/ckpts/run in " +
					"determined-container",
			},
			[]string{"bind_mounts mounts"},
		},
		{
			"a hostPath volume", dir("/mnt/ckpts/run"),
			exp(pod(volumeMount{source: hostPath("/srv/a"), mountPath: "/mnt/ckpts"})),
			gc(bind{"/srv/a", "/mnt/ckpts"}),
			[]string{
				"had /mnt/ckpts/run on a hostPath volume of host path /srv/a/run,",
				"kept until checkpoint_gc_pod_spec (else cpu_pod_spec, merged over gpu_pod_spec) mounts " +
					"a hostPath volume of host path /srv/a/run at /mnt/ckpts/run in determined-container",
			},
			[]string{"bind_mounts mounts", "node"},
		},
		{
			"an nfs volume", dir("/mnt/ckpts/run"), exp(aliceNFS),
			gcPod(pod(volumeMount{source: nfs("nfs2", "/export/alice"), mountPath: "/mnt/ckpts"})),
			[]string{
				"had /mnt/ckpts/run on path /export/alice/run of the nfs server nfs1,",
				"mounts path /export/alice/run of the nfs server nfs1 at /mnt/ckpts/run in " +
					"determined-container",
			},
			[]string{"nfs2", "bind_mounts mounts"},
		},
		{
			"a hostPath volume on another node", dir("/mnt/ckpts"),
			exp(on("node-a", dataCkpts)), cpuPod(on("node-b", dataCkpts)),
			[]string{
				"its checkpoint storage is the directory /mnt/ckpts. The experiment's trials had " +
					"/mnt/ckpts on a hostPath volume of host path /data/ckpts on node node-a, where their " +
					"pod spec pins them, and a checkpoint GC task, which takes no bind_mounts or pod_spec " +
					"from the experiment, would not have the same there. Its checkpoints are kept until " +
					"checkpoint_gc_pod_spec pins the pod to node node-a, by nodeName or a " +
					"kubernetes.io/hostname nodeSelector, and mounts a hostPath volume of host path " +
					"/data/ckpts at /mnt/ckpts in determined-container",
			},
			[]string{"node-b", "cpu_pod_spec"},
		},
		{
			"a bind mount on a node, GC not pinned", dir("/mnt/ckpts"),
			exp(on("node-a", &k8sV1.Pod{}), bind{"/data/ckpts", "/mnt/ckpts"}),
			gc(bind{"/data/ckpts", "/mnt/ckpts"}),
			[]string{
				"had /mnt/ckpts on a bind mount of host path /data/ckpts on node node-a, where their pod " +
					"spec pins them,",
				"kept until checkpoint_gc_pod_spec pins the pod to node node-a, by nodeName or a " +
					"kubernetes.io/hostname nodeSelector, and task_container_defaults.bind_mounts mounts " +
					"host path /data/ckpts at /mnt/ckpts",
			},
			[]string{"hostPath volume"},
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
			err := checkpointGCSeesStorage(tc.storage, tc.exp, podSpecsApplied, tc.tcd, podSpecsApplied)
			require.ErrorContains(t, err, "checkpoints are kept")
			for _, s := range tc.says {
				require.ErrorContains(t, err, s)
			}
			for _, s := range tc.not {
				require.NotContains(t, err.Error(), s)
			}
		})
	}
	// A pod spec counts only where the resource manager applies it. On the agent resource manager,
	// a container gets its bind mounts alone: no volume and no node of a pod spec, which the trials
	// can have from the task container defaults' gpu_pod_spec, merged into the experiment's, and a
	// GC task from the same, merged into its own. Where the master cannot tell, a place counts only
	// if it is the same with the pod spec and without it.
	ckptsPVC := pod(volumeMount{source: pvc("ckpts"), mountPath: "/mnt/ckpts"})
	alice := bind{"/srv/alice", "/mnt"}
	pinnedBind := exp(on("node-a", &k8sV1.Pod{}), bind{"/data/ckpts", "/mnt/ckpts"})
	k8s, agent, unknown := podSpecsApplied, podSpecsIgnored, podSpecsUnknown
	for _, tc := range []struct {
		name      string
		exp       expconf.LegacyConfig
		trialPods podSpecs
		tcd       model.TaskContainerDefaultsConfig
		gcPods    podSpecs
		sees      bool
		says      string
	}{
		{
			// The trials wrote to /srv/alice/ckpts, and a GC task has an empty /mnt/ckpts.
			"agent RM: a trial bind mount under a gpu_pod_spec PVC, GC no bind mount",
			exp(ckptsPVC, alice), agent, gpuPod(ckptsPVC), agent, false,
			"had /mnt/ckpts on a bind mount of host path /srv/alice/ckpts,",
		},
		{
			"agent RM: a trial bind mount under a gpu_pod_spec PVC, GC the same bind mount",
			exp(ckptsPVC, alice), agent, func() model.TaskContainerDefaultsConfig {
				tcd := gc(alice)
				tcd.GPUPodSpec = ckptsPVC
				return tcd
			}(), agent, true, "",
		},
		{
			"Kubernetes: a trial bind mount under a gpu_pod_spec PVC, the same PVC for GC",
			exp(ckptsPVC, alice), k8s, gpuPod(ckptsPVC), k8s, true, "",
		},
		{
			"agent RM: a trial PVC alone, the same for GC", exp(ckptsPVC), agent, gpuPod(ckptsPVC), agent,
			true, "",
		},
		{
			"agent RM: a trial pod spec on a node, the same bind mount for GC not pinned",
			pinnedBind, agent, gc(bind{"/data/ckpts", "/mnt/ckpts"}), agent, true, "",
		},
		{
			"trials on Kubernetes, GC on the agent RM: a trial PVC, the same in gpu_pod_spec",
			exp(ckptsPVC), k8s, gpuPod(ckptsPVC), agent, false,
			"had /mnt/ckpts on path / of the persistentVolumeClaim ckpts,",
		},
		{
			"trials on Kubernetes, GC on the agent RM: a trial bind mount on a node, the same on it",
			pinnedBind, k8s, gcWith(on("node-a", &k8sV1.Pod{}), bind{"/data/ckpts", "/mnt/ckpts"}),
			agent, false, "on node node-a, where their pod spec pins them",
		},
		{
			"trials on the agent RM, GC on Kubernetes: a trial bind mount under a PVC, GC the PVC",
			exp(ckptsPVC, alice), agent, gcWith(ckptsPVC, alice), k8s, false,
			"had /mnt/ckpts on a bind mount of host path /srv/alice/ckpts,",
		},
		{
			"trials on the agent RM, GC on Kubernetes: a trial bind mount, the same for GC",
			exp(ckptsPVC, alice), agent, gc(alice), k8s, true, "",
		},
		{
			"trials unknown: a trial bind mount under a PVC, the same PVC for GC on Kubernetes",
			exp(ckptsPVC, alice), unknown, gpuPod(ckptsPVC), k8s, false,
			"What the experiment's trials had at /mnt/ckpts depends on whether the resource manager " +
				"of the experiment's resource pool applied their pod spec",
		},
		{
			"trials unknown: a trial PVC alone, the same for GC on Kubernetes",
			exp(ckptsPVC), unknown, gpuPod(ckptsPVC), k8s, false, "depends on whether",
		},
		{
			"trials unknown: a trial bind mount and no pod spec, the same for GC on the agent RM",
			exp(nil, alice), unknown, gc(alice), agent, true, "",
		},
		{
			"trials unknown: a trial pod spec on a node, the same bind mount for GC on the agent RM",
			pinnedBind, unknown, gc(bind{"/data/ckpts", "/mnt/ckpts"}), agent, false,
			"on node node-a, where their pod spec pins them",
		},
		{
			"GC unknown: a trial bind mount, the same for GC under a PVC",
			exp(nil, alice), agent, gcWith(ckptsPVC, alice), unknown, false,
			"had /mnt/ckpts on a bind mount of host path /srv/alice/ckpts,",
		},
		{
			"GC unknown: a trial bind mount, the same for GC", exp(nil, alice), agent, gc(alice), unknown,
			true, "",
		},
		{
			"GC unknown: a trial PVC on Kubernetes, the same for GC", exp(ckptsPVC), k8s, gpuPod(ckptsPVC),
			unknown, false, "had /mnt/ckpts on path / of the persistentVolumeClaim ckpts,",
		},
	} {
		t.Run("resource managers/"+tc.name, func(t *testing.T) {
			err := checkpointGCSeesStorage(dir("/mnt/ckpts"), tc.exp, tc.trialPods, tc.tcd, tc.gcPods)
			if tc.sees {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, "checkpoints are kept")
			require.ErrorContains(t, err, tc.says)
		})
	}
}
