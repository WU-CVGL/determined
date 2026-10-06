package internal

import (
	"context"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/docker/docker/api/types/mount"
	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"
	k8sV1 "k8s.io/api/core/v1"

	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/rm/tasklist"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/storage"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/internal/workspace"
	"github.com/determined-ai/determined/master/pkg/logger"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/protoutils/protoconverter"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

const fullDeleteGlob = "**/*"

func runCheckpointGCForCheckpoints(
	rm rm.ResourceManager,
	db *db.PgDB,
	jobID model.JobID,
	jobSubmissionTime time.Time,
	taskSpec *tasks.TaskSpec,
	expID int,
	legacyConfig expconf.LegacyConfig,
	toDeleteCheckpoints []uuid.UUID,
	checkpointGlobs []string,
	deleteTensorboards bool,
	logCtx logger.Context,
) error {
	groups, err := storage.GroupCheckpoints(context.TODO(), toDeleteCheckpoints)
	if err != nil {
		return err
	}

	var wg errgroup.Group
	for _, g := range groups {
		wg.Go(func() error {
			taskID := model.TaskID(fmt.Sprintf("%d.%s", expID, uuid.New()))
			if err := runCheckpointGCTask(
				rm, db, taskID, jobID, jobSubmissionTime, *taskSpec,
				expID, legacyConfig, g.StorageID, g.Checkpoints,
				checkpointGlobs, deleteTensorboards, logCtx,
			); err != nil {
				return err
			}

			return nil
		})
	}

	if err := wg.Wait(); err != nil {
		return fmt.Errorf("one or more checkpoint GC jobs failed: %w", err)
	}

	return nil
}

// checkpointGCIdentity returns whom a checkpoint GC task of the experiment runs as: always the
// experiment's owner, with the owner's agent user and group, whoever asked for the GC. The task
// works on the owner's files with settings the owner chose, such as the checkpoint storage, so
// running it as the user who asked, who may be an administrator, would lend that user's identity to
// the owner. Whether the user who asked may do so is checked before, as that user.
//
// A deactivated owner is still the owner: the task needs no user session (see runCheckpointGCTask),
// its allocation token acts as its owner whether or not the owner is active, and its fixed
// environment (see tasks.GCCkptSpec) runs no code of the owner's choice.
func checkpointGCIdentity(
	ctx context.Context, expID int,
) (*model.User, *model.AgentUserGroup, error) {
	exp, err := db.ExperimentByID(ctx, expID)
	if err != nil {
		return nil, nil, fmt.Errorf("getting experiment %d: %w", expID, err)
	}
	if exp.OwnerID == nil {
		return nil, nil, fmt.Errorf("experiment %d has no owner", expID)
	}
	owner, err := user.ByID(ctx, *exp.OwnerID)
	if err != nil {
		return nil, nil, fmt.Errorf("getting user %d, the owner of experiment %d: %w",
			*exp.OwnerID, expID, err)
	}
	workspaceIDs, err := workspace.WorkspacesIDsByExperimentIDs(ctx, []int{expID})
	if err != nil {
		return nil, nil, err
	}
	agentUserGroup, err := user.GetAgentUserGroup(ctx, owner.ID, workspaceIDs[0])
	if err != nil {
		return nil, nil, fmt.Errorf("getting the agent user and group of user %d: %w", owner.ID, err)
	}
	return ptrs.Ptr(owner.ToUser()), agentUserGroup, nil
}

// checkpointGCSeesStorage returns an error for directory checkpoint storage that a checkpoint GC
// task with these task container defaults would not see where the experiment's trials saw it.
//
// That storage is a path in the container, and its files are wherever the container's mounts put
// that path. The trials had the experiment's bind mounts and pod spec, which the master merged over
// the task container defaults when it created the experiment, so exp has them as the trials got
// them. A GC task takes neither from the experiment: it gets the bind mounts and the pod spec of
// the current task container defaults (tasks.GCMounts and tasks.GCPodSpec). Where the two differ,
// the task finds no checkpoint directories, the harness takes a missing directory as already
// deleted, and the checkpoints would be recorded as deleted while their files remain. So the task
// runs only when the master can confirm that it sees the same place.
//
// The place of a path is decided by the longest mount that covers it, as in Docker: a bind mount,
// by its host path, or a volumeMount of the pod spec's determined-container, by its hostPath or
// persistentVolumeClaim volume and subPath, in each case with the path below the mount point. Other
// volumes and a subPathExpr cannot be matched, and a bind mount never matches a volume. The places
// must be the same at the storage directory and at every mount point below it, where checkpoints
// and TensorBoard files are too, wherever the trials had a mount. Where they had none, their files
// were in their own containers and went with them, so the task runs and records them as deleted,
// as before; this also covers a path that the container runtime binds by default (Singularity or
// enroot on Slurm/PBS). Pod specs are taken to apply, as on Kubernetes.
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

	// The trials' bind mounts, as task_trial.go gives them.
	trial := append(
		bindMountsOf(tasks.ToDockerMounts(schemas.WithDefaults(exp.BindMounts), tasks.DefaultWorkDir)),
		volumeMountsOf(exp.Environment.PodSpec())...)
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
		if seen.kind == placeContainer {
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
				"(it matches bind mounts by host path, and hostPath and persistentVolumeClaim "+
				"volumes by source and subPath), so its checkpoints are kept", msg)
		}
		return fmt.Errorf("%s %s, but a checkpoint GC task would have %s, as it takes no "+
			"bind_mounts or pod_spec from the experiment. Its checkpoints are kept until "+
			"task_container_defaults.bind_mounts or checkpoint_gc_pod_spec mounts the same place "+
			"at %s", msg, seen, gcSees.other(seen), p)
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
	placeUnknown   = "a volume whose source the master cannot match"
	placeBind      = "bind mount"
	placeHostPath  = "hostPath volume"
	placePVC       = "persistentVolumeClaim volume"
)

// storagePlace is where the files at a path in a task container are.
type storagePlace struct {
	kind string
	// claim is the claim name of a persistentVolumeClaim volume.
	claim string
	// path is the host path of a bind mount or a hostPath volume, or the path in the volume of a
	// persistentVolumeClaim volume.
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
		return "the persistentVolumeClaim " + p.claim + ", at " + p.path + " in the volume"
	default:
		return p.kind
	}
}

// other describes the place that a checkpoint GC task would have where the trials had seen,
// without naming its host path or claim, which come from the task container defaults.
func (p storagePlace) other(seen storagePlace) string {
	switch {
	case p.kind == placeContainer:
		return "no mount there"
	case p.kind == placeBind && seen.kind == placeBind:
		return "a different host path there"
	case p.kind == seen.kind && p.kind != placeUnknown:
		return "a different " + p.kind + " or path there"
	case p.kind == placeUnknown:
		return p.kind + " there"
	default:
		return "a " + p.kind + " there"
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
	if place.kind == placeUnknown {
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
// volumeMounts of its determined-container. Only hostPath and persistentVolumeClaim volumes without
// a subPathExpr have a known place.
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
					kind:  placePVC,
					claim: v.PersistentVolumeClaim.ClaimName,
					path:  filepath.Join("/", vm.SubPath),
				}
			}
			out = append(out, containerMount{target: pathInContainer(vm.MountPath), place: place})
		}
	}
	return out
}

func runCheckpointGCTask(
	rm rm.ResourceManager,
	pgDB *db.PgDB,
	taskID model.TaskID,
	jobID model.JobID,
	jobSubmissionTime time.Time,
	taskSpec tasks.TaskSpec,
	expID int,
	legacyConfig expconf.LegacyConfig,
	storageID *model.StorageBackendID,
	toDeleteCheckpoints []uuid.UUID,
	checkpointGlobs []string,
	deleteTensorboards bool,
	logCtx logger.Context,
) error {
	conv := &protoconverter.ProtoConverter{}
	checkpointStrIDs := conv.ToStringList(toDeleteCheckpoints)
	deleteCheckpointsStr := strings.Join(checkpointStrIDs, ",")

	if len(deleteCheckpointsStr) == 0 && !deleteTensorboards {
		// Early return as nothing to do
		return nil
	}

	rp, err := rm.ResolveResourcePool("", -1, 0)
	if err != nil {
		return fmt.Errorf("resolving resource pool: %w", err)
	}

	// t.Base is just a shallow copy of the m.taskSpec on the master, so
	// use caution when mutating it.
	tcd, err := rm.TaskContainerDefaults(
		rp,
		config.GetMasterConfig().TaskContainerDefaults)
	if err != nil {
		return fmt.Errorf("creating task container defaults: %v", err)
	}
	taskSpec.TaskContainerDefaults = tcd

	owner, agentUserGroup, err := checkpointGCIdentity(context.TODO(), expID)
	if err != nil {
		return fmt.Errorf("finding whom checkpoint GC runs as: %w", err)
	}
	taskSpec.Owner = owner
	taskSpec.AgentUserGroup = agentUserGroup
	// The task gets no user session. It reports to the master with its allocation session token
	// (DET_SESSION_TOKEN), which acts as taskSpec.Owner, and needs nothing else. No session is minted
	// for it, so drop whatever user token the caller's spec carries rather than pass it on: for the
	// end-of-experiment GC, that is the experiment's own, which stop() revokes.
	taskSpec.UserSessionToken = ""

	gcSpec := tasks.GCCkptSpec{
		Base:               taskSpec,
		ExperimentID:       expID,
		LegacyConfig:       legacyConfig,
		ToDelete:           deleteCheckpointsStr,
		CheckpointGlobs:    checkpointGlobs,
		DeleteTensorboards: deleteTensorboards,
	}

	// Update checkpoint storage with storageID.
	if storageID != nil {
		checkpointStorage, err := storage.Backend(context.TODO(), *storageID)
		if err != nil {
			return fmt.Errorf("getting storage id %d in create gc task: %w", *storageID, err)
		}
		gcSpec.LegacyConfig.CheckpointStorage = checkpointStorage
	}
	if err := checkpointGCSeesStorage(
		gcSpec.LegacyConfig.CheckpointStorage, gcSpec.LegacyConfig, tcd,
	); err != nil {
		return fmt.Errorf("checkpoint GC of experiment %d: %w", expID, err)
	}

	logCtx = logger.MergeContexts(logCtx, logger.Context{
		"task-id":   taskID,
		"task-type": model.TaskTypeCheckpointGC,
	})
	syslog := logrus.WithField("component", "checkpointgc").WithFields(logCtx.Fields())

	if err := db.AddTask(context.TODO(), &model.Task{
		TaskID:     taskID,
		TaskType:   model.TaskTypeCheckpointGC,
		StartTime:  time.Now().UTC(),
		JobID:      &jobID,
		LogVersion: model.CurrentTaskLogVersion,
	}); err != nil {
		return errors.Wrapf(err, "persisting GC task %s", taskID)
	}

	allocationID := model.AllocationID(fmt.Sprintf("%s.%d", taskID, 1))
	gcJobID := model.JobID(fmt.Sprintf("checkpoint_gc-%s", allocationID))

	resultChan := make(chan error, 1)
	onExit := func(ae *task.AllocationExited) {
		if err := db.CompleteTask(context.TODO(), taskID, time.Now().UTC()); err != nil {
			syslog.WithError(err).Error("marking GC task complete")
		}
		if err := tasklist.GroupPriorityChangeRegistry.Delete(gcJobID); err != nil {
			syslog.WithError(err).Error("deleting group priority change registry")
		}
		resultChan <- ae.Err
	}

	if err := tasklist.GroupPriorityChangeRegistry.Add(gcJobID, nil); err != nil {
		return err
	}
	err = task.DefaultService.StartAllocation(logCtx, sproto.AllocateRequest{
		TaskID:            taskID,
		JobID:             gcJobID,
		JobSubmissionTime: jobSubmissionTime,
		AllocationID:      allocationID,
		Name:              fmt.Sprintf("Checkpoint GC (Experiment %d)", expID),
		FittingRequirements: sproto.FittingRequirements{
			SingleAgent: true,
		},
		ResourcePool: rp.String(),
	}, pgDB, rm, gcSpec, onExit)
	if err != nil {
		return err
	}
	return <-resultChan
}
