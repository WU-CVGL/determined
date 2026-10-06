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
// runs only when the master can confirm that it sees the same place.
//
// The place of a path is decided by the longest mount that covers it, as in Docker: a bind mount,
// by its host path, or a volumeMount of the pod spec's determined-container, by its hostPath,
// persistentVolumeClaim or nfs volume and subPath, in each case with the path below the mount
// point. Other volumes and a subPathExpr cannot be matched, and a bind mount never matches a volume.
// The places must be the same at the storage directory and at every mount point below it, where
// checkpoints and TensorBoard files are too, wherever the trials had a mount. Where they had none,
// or an emptyDir volume, their files were in their own containers or pods and went with them, so
// the task runs and records them as deleted, as before; this also covers a path that the container
// runtime binds by default (Singularity or enroot on Slurm/PBS). Pod specs are taken to apply, as on
// Kubernetes.
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
	trial := append(bindMountsOf(trialBinds), volumeMountsOf(exp.Environment.PodSpec())...)
	gc := append(
		bindMountsOf(tasks.GCMounts(tcd, storage)),
		volumeMountsOf(tasks.GCPodSpec(tcd))...)

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
		gcSees := placeOf(gc, p)
		if seen.same(gcSees) {
			continue
		}
		msg := fmt.Sprintf("its checkpoint storage is the directory %s. The experiment's trials "+
			"had %s on", storagePath, p)
		if seen.kind == placeUnknown {
			return fmt.Errorf("%s a volume that the master cannot match for a checkpoint GC task "+
				"(it matches bind mounts by host path, hostPath and persistentVolumeClaim volumes "+
				"by source and subPath, and nfs volumes by server, path and subPath), so its "+
				"checkpoints are kept, whatever checkpoint GC tasks mount", msg)
		}
		return fmt.Errorf("%s %s, but a checkpoint GC task would have %s, as it takes no "+
			"bind_mounts or pod_spec from the experiment. Its checkpoints are kept until %s",
			msg, seen, gcSees.other(seen), seen.remedy(p))
	}
	return nil
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

func (p storagePlace) String() string {
	switch p.kind {
	case placeBind:
		return "a bind mount, at host path " + p.path
	case placeHostPath:
		return "a hostPath volume, at host path " + p.path
	case placePVC:
		return "the persistentVolumeClaim " + p.source + ", at " + p.path + " in the volume"
	case placeNFS:
		return "an nfs volume of server " + p.source + ", at " + p.path + " on the server"
	default:
		return p.kind
	}
}

// other describes the place that a checkpoint GC task would have where the trials had seen,
// without naming its host path, claim or server, which come from the task container defaults.
func (p storagePlace) other(seen storagePlace) string {
	switch {
	case p.kind == placeContainer:
		return "no mount there"
	case p.kind == placeBind && seen.kind == placeBind:
		return "a different host path there"
	case p.kind == seen.kind && p.kind != placeUnknown:
		return "a different " + p.kind + " or path there"
	case p.kind == placeUnknown || p.kind == placeEmptyDir:
		return p.kind + " there"
	default:
		return p.aKind() + " there"
	}
}

// aKind returns the kind with its indefinite article.
func (p storagePlace) aKind() string {
	if p.kind == placeNFS {
		return "an " + p.kind
	}
	return "a " + p.kind
}

// remedy says what gives checkpoint GC tasks this place, which the trials had, at path p: a task
// container default bind mount for a bind mount, a volumeMount of the GC pod spec for a volume. A
// bind mount never matches a volume, nor the reverse.
func (p storagePlace) remedy(at string) string {
	if p.kind == placeBind {
		return fmt.Sprintf("task_container_defaults.bind_mounts mounts host path %s at %s", p.path, at)
	}
	return fmt.Sprintf("checkpoint_gc_pod_spec (else cpu_pod_spec, merged over gpu_pod_spec) "+
		"mounts %s in %s so that %s is at %s", p.aKind(), model.DeterminedK8ContainerName, p.where(), at)
}

// where names the place in its volume, for remedy.
func (p storagePlace) where() string {
	switch p.kind {
	case placePVC:
		return p.path + " in the claim " + p.source
	case placeNFS:
		return p.path + " on the server " + p.source
	default:
		return "host path " + p.path
	}
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
