package internal

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// continueGuardTestConfig is an experiment's config before defaults: it has hyperparameters, data,
// an image, environment variables, a bind mount, labels and a checkpoint storage location.
const continueGuardTestConfig = `
name: owner experiment
description: the owner's
labels: [a]
entrypoint: python3 train.py
hyperparameters:
  model_name: mnist
  lr: {type: const, val: 0.1}
data:
  url: https://example.com/owner.tar
environment:
  image: owner/image:1
  environment_variables: [A=1, B=2]
bind_mounts:
  - host_path: /data
    container_path: /data
checkpoint_storage:
  type: shared_fs
  host_path: /ckpt
  save_trial_latest: 1
searcher:
  name: single
  metric: loss
  max_length: {batches: 10}
resources:
  resource_pool: default
`

// continueGuardActive returns continueGuardTestConfig with defaults, as ActiveExperimentConfig
// returns the stored config.
func continueGuardActive(t *testing.T) expconf.ExperimentConfig {
	t.Helper()
	c, err := expconf.ParseAnyExperimentConfigYAML([]byte(continueGuardTestConfig))
	require.NoError(t, err)
	return schemas.WithDefaults(c)
}

// continueGuardMerge merges override into active as parseAndMergeContinueConfig does.
func continueGuardMerge(
	t *testing.T, active expconf.ExperimentConfig, override string,
) expconf.ExperimentConfig {
	t.Helper()
	provided, err := expconf.ParseAnyExperimentConfigYAML([]byte(override))
	require.NoError(t, err)
	return schemas.Merge(provided, active)
}

func TestContinueOwnerOnlyChanges(t *testing.T) {
	active := continueGuardActive(t)

	unchanged := map[string]string{
		"no override": `{}`,
		"the same values": `
entrypoint: python3 train.py
hyperparameters: {model_name: mnist, lr: {type: const, val: 0.1}}
data: {url: https://example.com/owner.tar}
environment: {image: owner/image:1, environment_variables: [B=2]}
bind_mounts: [{host_path: /data, container_path: /data}]
resources: {resource_pool: default}
`,
		"name":         `name: renamed`,
		"description":  `description: Fork of the owner's`,
		"labels":       `labels: [b]`,
		"max_restarts": `max_restarts: 9`,
		"searcher.max_length": `
searcher: {name: single, metric: loss, max_length: {batches: 1000}}`,
		"checkpoint_storage.save_*": `
checkpoint_storage:
  type: shared_fs
  host_path: /ckpt
  save_experiment_best: 2
  save_trial_best: 3
  save_trial_latest: 4
`,
		// As det experiment continue --config checkpoint_storage.save_trial_best=3 sends it.
		"checkpoint_storage.save_* alone": `checkpoint_storage: {save_trial_best: 3}`,
	}
	for name, override := range unchanged {
		t.Run("allowed: "+name, func(t *testing.T) {
			changed, err := continueOwnerOnlyChanges(active, continueGuardMerge(t, active, override))
			require.NoError(t, err)
			require.Empty(t, changed)
		})
	}

	refused := []struct{ name, override, field string }{
		{"entrypoint", `entrypoint: python3 other.py`, "entrypoint"},
		{"hyperparameter value", `hyperparameters: {model_name: other}`, "hyperparameters"},
		{"new hyperparameter", `hyperparameters: {model_version: 2}`, "hyperparameters"},
		{"data", `data: {url: https://example.com/other.tar}`, "data"},
		{"image", `environment: {image: other/image:1}`, "environment"},
		{"environment variable", `environment: {environment_variables: [B=3]}`, "environment"},
		{"bind mount", `bind_mounts: [{host_path: /x, container_path: /x}]`, "bind_mounts"},
		{
			"checkpoint location", `checkpoint_storage: {type: shared_fs, host_path: /x}`,
			"checkpoint_storage",
		},
		{"warm start", `searcher: {name: single, metric: loss, source_trial_id: 1}`, "searcher"},
		{"searcher metric", `searcher: {name: single, metric: accuracy}`, "searcher"},
		{"slurm.sbatch_args", `slurm: {sbatch_args: [--export=ALL]}`, "slurm"},
		{"pbs.pbsbatch_args", `pbs: {pbsbatch_args: [-V]}`, "pbs"},
		{"resource pool", `resources: {resource_pool: other}`, "resources"},
		{"priority", `resources: {priority: 1}`, "resources"},
		{"min_validation_period", `min_validation_period: {batches: 1}`, "min_validation_period"},
		{"min_checkpoint_period", `min_checkpoint_period: {batches: 1}`, "min_checkpoint_period"},
		{"debug", `debug: true`, "debug"},
		{
			"log_policies", `log_policies: [{name: x, pattern: ".*", action: exclude_node}]`,
			"log_policies",
		},
	}
	for _, c := range refused {
		t.Run("refused: "+c.name, func(t *testing.T) {
			changed, err := continueOwnerOnlyChanges(active, continueGuardMerge(t, active, c.override))
			require.NoError(t, err)
			require.Equal(t, []string{c.field}, changed)
		})
	}

	t.Run("every changed field is named once, sorted", func(t *testing.T) {
		changed, err := continueOwnerOnlyChanges(active, continueGuardMerge(t, active, `
name: renamed
hyperparameters: {model_name: other}
entrypoint: python3 other.py
searcher: {name: single, metric: loss, max_length: {batches: 20}, source_trial_id: 1}
`))
		require.NoError(t, err)
		require.Equal(t, []string{"entrypoint", "hyperparameters", "searcher"}, changed)
	})
}
