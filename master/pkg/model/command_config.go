package model

import (
	"github.com/determined-ai/determined/master/pkg/check"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
)

// DefaultConfig is the default configuration used by commands and shells if a request does not
// specify any configuration options.
func DefaultConfig(taskContainerDefaults *TaskContainerDefaultsConfig) CommandConfig {
	out := CommandConfig{
		Resources:   DefaultResourcesConfig(taskContainerDefaults),
		Environment: DefaultEnvConfig(taskContainerDefaults),
	}

	if taskContainerDefaults != nil {
		out.WorkDir = taskContainerDefaults.WorkDir
		out.BindMounts = taskContainerDefaults.BindMounts
		out.Pbs = taskContainerDefaults.Pbs
		out.Slurm = taskContainerDefaults.Slurm
	}

	return out
}

// CommandConfig holds the necessary configurations to launch a command task in
// the cluster.
type CommandConfig struct {
	Description string              `json:"description"`
	BindMounts  BindMountsConfig    `json:"bind_mounts"`
	Environment Environment         `json:"environment"`
	Resources   ResourcesConfig     `json:"resources"`
	Entrypoint  []string            `json:"entrypoint"`
	IdleTimeout *Duration           `json:"idle_timeout"`
	WorkDir     *string             `json:"work_dir"`
	Debug       bool                `json:"debug"`
	Pbs         expconf.PbsConfig   `json:"pbs,omitempty"`
	Slurm       expconf.SlurmConfig `json:"slurm,omitempty"`
	// Services are the services that a shell runs besides its sshd, by name, and ssh, which only
	// a shell that views TensorBoard logs disables. Only shells take them.
	Services map[string]ServiceConfig `json:"services,omitempty"`
}

// Validate implements the check.Validatable interface.
func (c *CommandConfig) Validate() []error {
	return append([]error{
		check.GreaterThanOrEqualTo(c.Resources.Slots, 0, "resources.slots must be >= 0"),
		check.GreaterThan(len(c.Entrypoint), 0, "entrypoint must be non-empty"),
		check.True(c.Resources.IsSingleNode == nil, "resources.is_single_node cannot be set for NTSCs"),
		check.True(c.IdleTimeout == nil || *c.IdleTimeout > 0, "idle_timeout must be positive"),
		check.True(ShellRunsSSH(c.Services) || c.Resources.Slots == 0,
			"services: ssh: can be disabled only with resources.slots 0"),
		check.True(ShellRunsSSH(c.Services) || len(c.Resources.GPUGroupRequests()) == 0,
			"services: ssh: can be disabled only without resources.gpu_groups"),
	}, validateServices(c.Services)...)
}

// TensorBoardPrimary returns whether the config is of a shell whose tensorboard service is its
// primary service: one that does not run ssh. Once validated, its tensorboard service runs with
// experiment_ids or trial_ids.
func (c CommandConfig) TensorBoardPrimary() bool {
	return !ShellRunsSSH(c.Services)
}
