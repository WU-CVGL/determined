package internal

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"golang.org/x/sync/errgroup"

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

// checkpointGCSeesStorage returns an error for directory checkpoint storage that the experiment
// mounts itself but that a GC task with these task container defaults would not see.
//
// That storage is a path in the container. If the experiment mounts it with its own bind mounts or
// pod spec, the checkpoints are on that mount. A GC task takes neither from the experiment, so only
// the task container defaults can mount the path for it. Without them the task would find no files
// and record the checkpoints as deleted while their files remain. If the experiment does not mount
// it, the checkpoints were in the experiment's own container and went with it, so the task runs and
// records them as deleted, as before; this also covers a path that the container runtime binds by
// default (Singularity or enroot on Slurm/PBS), which the GC task gets as well.
//
// What a pod spec mounts cannot be told reliably from the spec (CSI drivers, sidecars, admission
// webhooks), so any pod spec is taken to mount the path. An experiment's pod spec makes the check
// refuse, since its files may be on that mount. A pod spec of the GC task makes it pass, and the
// administrator who set it is then the one to make it mount the path. The GC task has a pod spec
// whenever any of these three is set: tasks.GCCkptSpec gives it checkpoint_gc_pod_spec, else
// cpu_pod_spec, merged over gpu_pod_spec.
func checkpointGCSeesStorage(
	storage expconf.CheckpointStorageConfig,
	exp expconf.LegacyConfig,
	tcd model.TaskContainerDefaultsConfig,
) error {
	dir := storage.RawDirectoryConfig
	if dir == nil || dir.RawContainerPath == nil {
		return nil
	}
	inContainer := func(p string) string {
		if !filepath.IsAbs(p) {
			p = filepath.Join(tasks.DefaultWorkDir, p)
		}
		return filepath.Clean(p)
	}
	storagePath := inContainer(dir.ContainerPath())
	covers := func(containerPath string) bool {
		mountPath := inContainer(containerPath)
		return mountPath == "/" || storagePath == mountPath ||
			strings.HasPrefix(storagePath, mountPath+"/")
	}

	experimentMountsIt := exp.Environment.PodSpec() != nil
	for _, m := range exp.BindMounts {
		experimentMountsIt = experimentMountsIt || covers(m.ContainerPath())
	}
	if !experimentMountsIt {
		return nil
	}
	if tcd.CheckpointGCPodSpec != nil || tcd.CPUPodSpec != nil || tcd.GPUPodSpec != nil {
		return nil
	}
	for _, m := range tcd.BindMounts {
		if covers(m.ContainerPath) {
			return nil
		}
	}
	return fmt.Errorf("its checkpoint storage is the directory %s, which the experiment mounts "+
		"with its own bind_mounts or pod_spec. Checkpoint GC tasks take neither from the "+
		"experiment, so it stays unmounted for them, and its checkpoints are kept until "+
		"task_container_defaults.bind_mounts or checkpoint_gc_pod_spec mounts it", storagePath)
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
