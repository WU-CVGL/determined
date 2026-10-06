package tasks

import (
	"archive/tar"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/docker/docker/api/types/mount"

	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/cproto"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// GCCkptSpec is a description of a task for running checkpoint GC.
type GCCkptSpec struct {
	Base TaskSpec

	ExperimentID int
	// CheckpointStorage is the storage of the checkpoints. It is all that the task takes from the
	// experiment's config: the experiment's environment, bind mounts and pod spec have no place
	// here. The master checks before that the task sees the storage.
	CheckpointStorage expconf.CheckpointStorageConfig
	ToDelete          string
	// If len(CheckpointGlobs) == 0 then we won't delete any checkpoint files
	// and just refresh the state of the checkpoint.
	CheckpointGlobs    []string
	DeleteTensorboards bool
}

// ToTaskSpec generates a TaskSpec.
//
// The GC task runs a fixed entrypoint in a fixed environment: the task container defaults of its
// resource pool, which the administrator sets, and what the checkpoint storage needs, which is its
// configuration and, for shared_fs, the mount of its host path. It takes no environment variables,
// bind mounts, pod spec or image from the experiment. Those are for the experiment's own containers,
// and some, such as BASH_ENV, PYTHONPATH or LD_PRELOAD, would run code of the owner's choice in the
// GC container before the GC itself.
func (g GCCkptSpec) ToTaskSpec() TaskSpec {
	res := g.Base
	tcd := g.Base.TaskContainerDefaults

	env, defaultConfig := gcEnvironment(tcd)
	res.Environment = env
	res.ExtraEnvVars = map[string]string{"DET_TASK_TYPE": string(model.TaskTypeCheckpointGC)}
	res.ResourcesConfig = schemas.WithDefaults(res.ResourcesConfig)
	res.SlurmConfig = defaultConfig.SlurmConfig()
	res.PbsConfig = defaultConfig.PbsConfig()

	res.WorkDir = DefaultWorkDir

	globs := g.CheckpointGlobs
	if globs == nil { // This matters for JSON parsing as [] vs None.
		globs = []string{}
	}

	storageConfigPath := "checkpoint_gc/storage_config.json"
	checkpointsToDeletePath := "checkpoint_gc/checkpoints_to_delete.json"
	checkpointsGlobsPath := "checkpoint_gc/checkpoints_globs.json"
	res.ExtraArchives = []cproto.RunArchive{
		wrapArchive(
			archive.Archive{
				g.Base.AgentUserGroup.OwnedArchiveItem("checkpoint_gc", nil, 0o700, tar.TypeDir),
				g.Base.AgentUserGroup.OwnedArchiveItem(
					storageConfigPath,
					[]byte(jsonify(g.CheckpointStorage)),
					0o600,
					tar.TypeReg,
				),
				g.Base.AgentUserGroup.OwnedArchiveItem(
					checkpointsToDeletePath,
					[]byte(jsonify(strings.Split(g.ToDelete, ","))),
					0o600,
					tar.TypeReg,
				),
				g.Base.AgentUserGroup.OwnedArchiveItem(
					checkpointsGlobsPath,
					[]byte(jsonify(globs)),
					0o600,
					tar.TypeReg,
				),
				g.Base.AgentUserGroup.OwnedArchiveItem(
					filepath.Join("checkpoint_gc", etc.GCCheckpointsEntrypointResource),
					etc.MustStaticFile(etc.GCCheckpointsEntrypointResource),
					0o700,
					tar.TypeReg,
				),
			},
			RunDir,
		),
	}

	res.Description = fmt.Sprintf("gc-%d", g.ExperimentID)

	// We pass storage-config / delete / globs through a JSON file instead of a JSON string
	// to avoid reaching any OS limitations on sizes of CLI arguments.
	res.Entrypoint = []string{
		filepath.Join("/run/determined/checkpoint_gc", etc.GCCheckpointsEntrypointResource),
		"--experiment-id",
		strconv.Itoa(g.ExperimentID),
		"--storage-config", fmt.Sprintf("/run/determined/%s", storageConfigPath),
	}

	if len(g.ToDelete) > 0 {
		res.Entrypoint = append(res.Entrypoint, "--delete", fmt.Sprintf("/run/determined/%s", checkpointsToDeletePath))
		res.Entrypoint = append(res.Entrypoint, "--globs", fmt.Sprintf("/run/determined/%s", checkpointsGlobsPath))
	}

	if g.DeleteTensorboards {
		res.Entrypoint = append(res.Entrypoint, "--delete-tensorboards")
	}

	res.Mounts = GCMounts(tcd, g.CheckpointStorage)
	res.TaskType = model.TaskTypeCheckpointGC

	return res
}

// gcEnvironment returns the environment of a checkpoint GC task with these task container defaults,
// and the defaults as an experiment config, for the Slurm and PBS settings.
func gcEnvironment(
	tcd model.TaskContainerDefaultsConfig,
) (expconf.EnvironmentConfig, expconf.ExperimentConfig) {
	// The task uses no slots, so it takes the CPU pod spec unless one is set for checkpoint GC, and
	// never the experiment's. Merging the defaults below then merges the GPU pod spec under it by
	// Kubernetes strategic merge, as it did before: for a config without resources, such as
	// defaultConfig, MergeIntoExpConfig takes the GPU pod spec, since slots_per_trial defaults to 1.
	// So with only a GPU pod spec set, the task gets that one.
	podSpec := tcd.CPUPodSpec
	if tcd.CheckpointGCPodSpec != nil {
		podSpec = tcd.CheckpointGCPodSpec
	}

	//nolint:exhaustruct // This has caused an issue before, but is valid as a partial struct.
	env := expconf.EnvironmentConfig{
		RawPodSpec: (*expconf.PodSpec)(podSpec),
	}
	// Fill the rest of the environment, environment variables and image included, from the task
	// container defaults.
	var defaultConfig expconf.ExperimentConfig
	tcd.MergeIntoExpConfig(&defaultConfig)
	if defaultConfig.RawEnvironment != nil {
		env = schemas.Merge(env, *defaultConfig.RawEnvironment)
	}
	return schemas.WithDefaults(env), defaultConfig
}

// GCPodSpec returns the pod spec of a checkpoint GC task with these task container defaults, nil if
// it has none. It is the pod spec that ToTaskSpec gives the task.
func GCPodSpec(tcd model.TaskContainerDefaultsConfig) *expconf.PodSpec {
	env, _ := gcEnvironment(tcd)
	return env.PodSpec()
}

// GCMounts returns the bind mounts of a checkpoint GC task with these task container defaults and
// this checkpoint storage: those of the task container defaults and, for shared_fs, the mount of
// its host path. They are the mounts that ToTaskSpec gives the task.
func GCMounts(
	tcd model.TaskContainerDefaultsConfig, storage expconf.CheckpointStorageConfig,
) []mount.Mount {
	mounts := ToDockerMounts(tcd.BindMounts.ToExpconf(), DefaultWorkDir)
	if fs := storage.RawSharedFSConfig; fs != nil {
		mounts = append(mounts, mount.Mount{
			Type:   mount.TypeBind,
			Source: fs.HostPath(),
			Target: expconf.DefaultSharedFSContainerPath,
			BindOptions: &mount.BindOptions{
				Propagation: expconf.DefaultSharedFSPropagation,
			},
		})
	}
	return mounts
}
