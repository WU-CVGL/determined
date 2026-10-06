package internal

import (
	"fmt"
	"path/filepath"
	"sort"
	"strings"

	"github.com/docker/docker/api/types/mount"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// checkpointGCSeesStorage returns an error for directory checkpoint storage that a checkpoint GC
// task with these task container defaults would not see where the experiment's trials saw it.
//
// That storage is a path in the container, and its files are wherever the container's mounts put
// that path. The trials had the mounts that task_trial.go gives them: the experiment's bind mounts,
// the mount of the experiment's own checkpoint storage if that is shared_fs, and the experiment's
// pod spec. The master merged the bind mounts and the pod spec over the task container defaults
// when it created the experiment, so exp has them as the trials got them. The experiment's storage
// counts even when the checkpoints' storage is another one, which a trial can report to the master
// (core.init's checkpoint_storage). A GC task takes none of these from the experiment: it gets the
// bind mounts and the pod spec of the current task container defaults and the mount of the
// checkpoints' own shared_fs storage (tasks.GCMounts and tasks.GCPodSpec). Where the two differ,
// the task finds no checkpoint directories, the harness takes a missing directory as already
// deleted, and the checkpoints would be recorded as deleted while their files remain. So the task
// runs only when the master finds the same place for it (a host path of trials that are not pinned
// to a node is assumed to be the same on every node).
//
// The place of a path is decided by the longest mount that covers it, as in Docker: a bind mount,
// by its host path, or a volumeMount of the pod spec's determined-container, by its hostPath,
// persistentVolumeClaim or nfs volume and subPath, in each case with the path below the mount
// point. Other volumes and a subPathExpr cannot be matched, and a bind mount never matches a volume.
// The places must be the same at the storage directory and at every mount point below it, where
// checkpoints and TensorBoard files are too, wherever the trials had a mount.
//
// A host path, of a hostPath volume or of a bind mount, which Kubernetes makes a hostPath volume,
// is on the node where the pod runs. If the trials' pod spec pins them to a node (pinnedNode), the
// GC task's pod spec must pin it to the same node. If it pins them to none that the master can
// read, the host path is taken to be the same storage on every node, as it always was on every
// agent of the agent resource manager, where a GC task runs on any agent of its pool; nothing
// confirms that.
//
// Where the trials had no mount, or an emptyDir volume, the check does not apply and the existing
// handling is kept: the task runs and records the checkpoints as deleted, as before. This also
// covers a path that the container runtime binds by default (Singularity or enroot on Slurm/PBS).
// Pod specs are taken to apply, as on Kubernetes.
func checkpointGCSeesStorage(
	storage expconf.CheckpointStorageConfig,
	exp expconf.LegacyConfig,
	tcd model.TaskContainerDefaultsConfig,
) error {
	dir := storage.RawDirectoryConfig
	if dir == nil || dir.RawContainerPath == nil {
		return nil
	}
	storagePath := pathInContainer(dir.ContainerPath())

	// The trials' mounts, as task_trial.go gives them: the experiment's bind mounts, then the mount
	// of the experiment's shared_fs storage, whatever storage the checkpoints were saved to.
	trialBinds := tasks.ToDockerMounts(schemas.WithDefaults(exp.BindMounts), tasks.DefaultWorkDir)
	if fs := exp.CheckpointStorage.RawSharedFSConfig; fs != nil && fs.RawHostPath != nil {
		trialBinds = append(trialBinds, mount.Mount{
			Type: mount.TypeBind, Source: fs.HostPath(), Target: expconf.DefaultSharedFSContainerPath,
		})
	}
	trialPod, gcPod := exp.Environment.PodSpec(), tasks.GCPodSpec(tcd)
	trial := append(bindMountsOf(trialBinds), volumeMountsOf(trialPod)...)
	gc := append(bindMountsOf(tasks.GCMounts(tcd, storage)), volumeMountsOf(gcPod)...)
	trialNode, gcNode := pinnedNode(trialPod), pinnedNode(gcPod)

	paths := []string{storagePath}
	for _, m := range append(append([]containerMount{}, trial...), gc...) {
		if m.target != storagePath && pathCovers(storagePath, m.target) {
			paths = append(paths, m.target)
		}
	}
	sort.Strings(paths)
	for _, p := range paths {
		seen := placeOf(trial, p)
		if seen.kind == placeContainer || seen.kind == placeEmptyDir {
			continue
		}
		node := ""
		if seen.kind == placeBind || seen.kind == placeHostPath {
			node = trialNode
		}
		if seen.same(placeOf(gc, p)) && (node == "" || gcNode == node) {
			continue
		}
		return gcStorageRefusal(storagePath, p, seen, node)
	}
	return nil
}

// The refusals of checkpointGCSeesStorage. Each names the storage directory, the path where a GC
// task would not see what the trials saw, the trials' place there, which comes from the
// experiment's config, and the setting that gives GC tasks the same place. None says what a GC
// task has there: its host paths, claims, servers and node come from the task container defaults.
const (
	gcRefusal = "its checkpoint storage is the directory %[1]s. The experiment's trials had %[2]s on "
	gcLacks   = ", and a checkpoint GC task, which takes no bind_mounts or pod_spec from the " +
		"experiment, would not have the same there. Its checkpoints are kept until "
	gcPin = "checkpoint_gc_pod_spec pins the pod to node %[4]s, by nodeName or a " +
		"kubernetes.io/hostname nodeSelector, and "

	gcRefusalUnknown = gcRefusal + "a volume that the master cannot match for a checkpoint GC task " +
		"(it matches bind mounts, and hostPath, persistentVolumeClaim and nfs volumes without a " +
		"subPathExpr), so its checkpoints are kept, whatever checkpoint GC tasks mount"
	gcRefusalBind = gcRefusal + "a bind mount of host path %[3]s" + gcLacks +
		"task_container_defaults.bind_mounts mounts host path %[3]s at %[2]s"
	gcRefusalBindOnNode = gcRefusal + "a bind mount of host path %[3]s on node %[4]s, where " +
		"their pod spec pins them" + gcLacks + gcPin +
		"task_container_defaults.bind_mounts mounts host path %[3]s at %[2]s"
	gcRefusalVolume = gcRefusal + "%[3]s" + gcLacks + "checkpoint_gc_pod_spec (else cpu_pod_spec, " +
		"merged over gpu_pod_spec) mounts %[3]s at %[2]s in determined-container"
	gcRefusalHostPathOnNode = gcRefusal + "a hostPath volume of host path %[3]s on node %[4]s, " +
		"where their pod spec pins them" + gcLacks + gcPin +
		"mounts a hostPath volume of host path %[3]s at %[2]s in determined-container"
)

// gcStorageRefusal returns the refusal for storage at storagePath that a GC task would not see at
// path p, where the trials saw seen, on node if their pod spec pins them to one.
func gcStorageRefusal(storagePath, p string, seen storagePlace, node string) error {
	switch seen.kind {
	case placeUnknown:
		return fmt.Errorf(gcRefusalUnknown, storagePath, p)
	case placeBind:
		if node != "" {
			return fmt.Errorf(gcRefusalBindOnNode, storagePath, p, seen.path, node)
		}
		return fmt.Errorf(gcRefusalBind, storagePath, p, seen.path)
	case placeHostPath:
		if node != "" {
			return fmt.Errorf(gcRefusalHostPathOnNode, storagePath, p, seen.path, node)
		}
		return fmt.Errorf(gcRefusalVolume, storagePath, p, "a hostPath volume of host path "+seen.path)
	case placePVC:
		return fmt.Errorf(gcRefusalVolume, storagePath, p,
			"path "+seen.path+" of the persistentVolumeClaim "+seen.source)
	default: // placeNFS
		return fmt.Errorf(gcRefusalVolume, storagePath, p,
			"path "+seen.path+" of the nfs server "+seen.source)
	}
}

// pinnedNode returns the node that a pod spec pins its pod to, "" if none that the master reads:
// its nodeName, else its kubernetes.io/hostname nodeSelector, else a required node affinity of one
// term with a kubernetes.io/hostname In expression of one value. A nodeName and a hostname are
// taken to name the same node when they are equal. Other constraints, such as other labels, several
// hostnames or several terms, preferred affinity, taints or the resource pool, are not read.
func pinnedNode(pod *expconf.PodSpec) string {
	if pod == nil {
		return ""
	}
	if pod.Spec.NodeName != "" {
		return pod.Spec.NodeName
	}
	if node := pod.Spec.NodeSelector[k8sV1.LabelHostname]; node != "" {
		return node
	}
	affinity := pod.Spec.Affinity
	if affinity == nil || affinity.NodeAffinity == nil ||
		affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		return ""
	}
	terms := affinity.NodeAffinity.RequiredDuringSchedulingIgnoredDuringExecution.NodeSelectorTerms
	if len(terms) != 1 {
		return ""
	}
	for _, e := range terms[0].MatchExpressions {
		if e.Key == k8sV1.LabelHostname && e.Operator == k8sV1.NodeSelectorOpIn && len(e.Values) == 1 {
			return e.Values[0]
		}
	}
	return ""
}

// pathInContainer returns the absolute, clean path in a task container of a container path, which
// is relative to the working directory unless it is absolute.
func pathInContainer(p string) string {
	if !filepath.IsAbs(p) {
		p = filepath.Join(tasks.DefaultWorkDir, p)
	}
	return filepath.Clean(p)
}

// pathCovers reports whether a mount at mountPath covers p. Both are absolute and clean.
func pathCovers(mountPath, p string) bool {
	return mountPath == "/" || p == mountPath || strings.HasPrefix(p, mountPath+"/")
}

// The kinds of storagePlace.
const (
	placeContainer = "the container's own filesystem"
	placeEmptyDir  = "an emptyDir volume"
	placeUnknown   = "a volume whose source the master cannot match"
	placeBind      = "bind mount"
	placeHostPath  = "hostPath volume"
	placePVC       = "persistentVolumeClaim volume"
	placeNFS       = "nfs volume"
)

// storagePlace is where the files at a path in a task container are.
type storagePlace struct {
	kind string
	// source is the claim name of a persistentVolumeClaim volume or the server of an nfs volume.
	source string
	// path is the host path of a bind mount or a hostPath volume, the path in the volume of a
	// persistentVolumeClaim volume, or the path on the server of an nfs volume.
	path string
}

func (p storagePlace) same(o storagePlace) bool {
	return p.kind != placeUnknown && p == o
}

// containerMount is a mount of a task container: the path in the container it is mounted at, and
// the place of its files.
type containerMount struct {
	target string
	place  storagePlace
}

// placeOf returns the place of the files at path p of a container with these mounts.
func placeOf(mounts []containerMount, p string) storagePlace {
	var target string
	var places []storagePlace
	for _, m := range mounts {
		switch {
		case !pathCovers(m.target, p) || len(m.target) < len(target):
		case m.target == target:
			places = append(places, m.place)
		default:
			target, places = m.target, []storagePlace{m.place}
		}
	}
	if len(places) == 0 {
		return storagePlace{kind: placeContainer}
	}
	place := places[0]
	for _, other := range places[1:] {
		if other != place {
			return storagePlace{kind: placeUnknown}
		}
	}
	if place.kind == placeUnknown || place.kind == placeEmptyDir {
		return place
	}
	rel, err := filepath.Rel(target, p)
	if err != nil {
		return storagePlace{kind: placeUnknown}
	}
	place.path = filepath.Join(place.path, rel)
	return place
}

// bindMountsOf returns the bind mounts of a task container.
func bindMountsOf(mounts []mount.Mount) []containerMount {
	var out []containerMount
	for _, m := range mounts {
		out = append(out, containerMount{
			target: pathInContainer(m.Target),
			place:  storagePlace{kind: placeBind, path: filepath.Clean(m.Source)},
		})
	}
	return out
}

// volumeMountsOf returns the volume mounts that a pod spec adds to the container of a task, the
// volumeMounts of its determined-container. Only hostPath, persistentVolumeClaim and nfs volumes
// without a subPathExpr have a known place, and an emptyDir volume holds files of the pod alone.
func volumeMountsOf(pod *expconf.PodSpec) []containerMount {
	if pod == nil {
		return nil
	}
	volumes := map[string]k8sV1.VolumeSource{}
	for _, v := range pod.Spec.Volumes {
		volumes[v.Name] = v.VolumeSource
	}
	var out []containerMount
	for _, c := range pod.Spec.Containers {
		if c.Name != model.DeterminedK8ContainerName {
			continue
		}
		for _, vm := range c.VolumeMounts {
			place := storagePlace{kind: placeUnknown}
			v, ok := volumes[vm.Name]
			switch {
			case !ok || vm.SubPathExpr != "":
			case v.HostPath != nil:
				place = storagePlace{
					kind: placeHostPath, path: filepath.Join(v.HostPath.Path, vm.SubPath),
				}
			case v.PersistentVolumeClaim != nil:
				place = storagePlace{
					kind:   placePVC,
					source: v.PersistentVolumeClaim.ClaimName,
					path:   filepath.Join("/", vm.SubPath),
				}
			case v.NFS != nil:
				place = storagePlace{
					kind:   placeNFS,
					source: v.NFS.Server,
					path:   filepath.Join("/", v.NFS.Path, vm.SubPath),
				}
			case v.EmptyDir != nil:
				place = storagePlace{kind: placeEmptyDir}
			}
			out = append(out, containerMount{target: pathInContainer(vm.MountPath), place: place})
		}
	}
	return out
}
