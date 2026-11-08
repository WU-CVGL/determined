// Package etc provides configuration files for setting up common
// system programs like ssh, sshd, bash, JupyterLab, and tensorboard.
package etc

import (
	"os"
	"path/filepath"
	"strings"

	"github.com/pkg/errors"

	"github.com/determined-ai/determined/master/pkg/check"
)

const (
	// CommandEntrypointResource is the pre-flight script for commands.
	CommandEntrypointResource = "command-entrypoint.sh"
	// SSHConfigResource is the template SSH config file.
	SSHConfigResource = "ssh_config"
	// SSHDConfigResource is the template SSHD config file.
	SSHDConfigResource = "sshd_config"
	// ShellEntrypointResource is the script to set up sshd.
	ShellEntrypointResource = "shell-entrypoint.sh"
	// GCCheckpointsEntrypointResource is the script to run checkpoint GC.
	GCCheckpointsEntrypointResource = "gc-checkpoints-entrypoint.sh"
	// NotebookIdleCheckResource is the jupyter service's idle check, which reports a busy
	// JupyterLab.
	NotebookIdleCheckResource = "check_idle.py"
	// InteractiveInitScriptResource is the container init that the interactive task supervisor
	// sources.
	InteractiveInitScriptResource = "interactive-init.sh"
	// InteractiveSupervisorResource is the process of shell tasks: it runs the init, then the
	// services in DET_SERVICES.
	InteractiveSupervisorResource = "interactive-supervisor.sh"
	// SSHServiceResource is the supervisor's unit for sshd.
	SSHServiceResource = "services/ssh.sh"
	// JupyterServiceResource is the supervisor's unit for JupyterLab.
	JupyterServiceResource = "services/jupyter.sh"
	// VSCodeServiceResource is the supervisor's unit for code-server.
	VSCodeServiceResource = "services/vscode.sh"
	// TensorBoardServiceResource is the supervisor's unit for TensorBoard.
	TensorBoardServiceResource = "services/tensorboard.sh"
	// TensorBoardServiceProgramResource is the program that the tensorboard service's unit runs
	// with TensorBoard's python: TensorBoard behind the master proxy's token.
	TensorBoardServiceProgramResource = "services/tensorboard_service.py"
	// ServiceStateReporterResource is the interactive task supervisor's reporter, which posts the
	// snapshot of the states of the task's services to the master.
	ServiceStateReporterResource = "report_service_states.py"
	// TaskCheckReadyLogsResource is the script to parse logs to check if a task is ready.
	TaskCheckReadyLogsResource = "check_ready_logs.py"
	// TaskShipLogsShellResource is the shell script to call the python script to ship logs.
	TaskShipLogsShellResource = "ship-logs.sh"
	// TaskShipLogsPythonResource is the python script to ship logs.
	TaskShipLogsPythonResource = "ship_logs.py"
	// TensorboardEntryScriptResource is the script to set up TensorBoard.
	TensorboardEntryScriptResource = "tensorboard-entrypoint.sh"
	// TrialEntrypointScriptResource is the script to set up a trial.
	TrialEntrypointScriptResource = "entrypoint.sh"
	// AgentSetupScriptTemplateResource is the template for the script to run a dynamic agent.
	AgentSetupScriptTemplateResource = "agent_setup_script.sh.template"
	// K8InitContainerEntryScriptResource is the script to run the init container on k8s.
	K8InitContainerEntryScriptResource = "k8_init_container_entrypoint.sh"
	// TaskSetupScriptResource is the script to setup various things for all tasks.
	TaskSetupScriptResource = "task-setup.sh"
	// SingularityEntrypointWrapperScriptResource is the entrypoint for singularity containers.
	SingularityEntrypointWrapperScriptResource = "singularity-entrypoint-wrapper.sh"
)

var staticRoot string

// SetRootPath sets the path relative to which the paths for resources are resolved.
func SetRootPath(root string) error {
	root, err := filepath.Abs(root)
	if err != nil {
		return err
	}
	staticRoot = root
	return nil
}

// MustStaticFile returns the content of the file with the provided name as a byte array.
func MustStaticFile(name string) []byte {
	if staticRoot == "" {
		panic("static file root has not been set")
	}
	path := filepath.Join(staticRoot, name)

	// Check that the final path is inside the root directory.
	rel, err := filepath.Rel(staticRoot, path)
	check.Panic(errors.Wrapf(err, "unable to find static file: %s", name))
	insideDir := rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
	check.Panic(
		check.TrueSilent(
			insideDir,
			"attempted to read path outside the static root: %s",
			path,
		),
	)

	bytes, err := os.ReadFile(path) // #nosec G304
	check.Panic(errors.Wrapf(err, "unable to find static file: %s", name))
	return bytes
}
