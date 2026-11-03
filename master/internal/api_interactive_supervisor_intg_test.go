//go:build integration
// +build integration

package internal

import (
	"context"
	"encoding/json"
	"strconv"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

// launchedSpec returns the spec that the master keeps for the task.
func launchedSpec(t *testing.T, taskID string) tasks.GenericCommandSpec {
	t.Helper()
	var raw []byte
	require.NoError(t, db.Bun().NewRaw(
		"SELECT generic_command_spec FROM command_state WHERE task_id = ?", taskID,
	).Scan(context.Background(), &raw))
	var spec tasks.GenericCommandSpec
	require.NoError(t, json.Unmarshal(raw, &spec))
	return spec
}

func requireArchivePath(t *testing.T, spec tasks.GenericCommandSpec, path string) {
	t.Helper()
	for _, item := range spec.AdditionalFiles {
		if item.Path == path {
			return
		}
	}
	require.Failf(t, "missing file", "%s is not in the task's files", path)
}

// Shells and notebooks run the supervisor with their one service; the task's other extra
// environment variables stay, and the entrypoint wrappers still ship.
func TestInteractiveLaunchersUseSupervisor(t *testing.T) {
	api, _, ctx := setupAPITest(t, nil)
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)

	shell, err := api.LaunchShell(ctx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	spec := launchedSpec(t, shell.Shell.Id)
	port := strconv.Itoa(spec.SSHDPort)
	require.Equal(t, []string{
		interactiveSupervisorScript, "-f", shellSSHDConfigFile, "-p", port, "-D", "-e",
	}, spec.Config.Entrypoint)
	require.Equal(t, "ssh", spec.Base.ExtraEnvVars["DET_SERVICES"])
	require.Equal(t, string(model.TaskTypeShell), spec.Base.ExtraEnvVars["DET_TASK_TYPE"])
	requireInteractiveSupervisorFiles(t, spec.AdditionalFiles, 0, 0)
	requireArchivePath(t, spec, shellEntrypointScript)
	requireArchivePath(t, spec, taskReadyCheckLogs)

	notebook, err := api.LaunchNotebook(ctx, &apiv1.LaunchNotebookRequest{})
	require.NoError(t, err)
	spec = launchedSpec(t, notebook.Notebook.Id)
	require.Equal(t, []string{interactiveSupervisorScript}, spec.Config.Entrypoint)
	env := spec.Base.ExtraEnvVars
	require.Equal(t, "jupyter", env["DET_SERVICES"])
	require.Equal(t, string(model.TaskTypeNotebook), env["DET_TASK_TYPE"])
	require.Equal(t, strconv.Itoa(spec.Base.ExtraProxyPorts[0].ProxyPort()), env["NOTEBOOK_PORT"])
	require.NotEmpty(t, env["NOTEBOOK_CONFIG"])
	require.NotEmpty(t, env["DET_NOTEBOOK_TOKEN"], "the command service adds the token")
	requireInteractiveSupervisorFiles(t, spec.AdditionalFiles, 0, 0)
	requireArchivePath(t, spec, jupyterEntrypoint)
	requireArchivePath(t, spec, jupyterIdleCheck)
}
