package internal

import (
	"archive/tar"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"
	"golang.org/x/exp/maps"

	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

func TestInteractiveSupervisorEntrypoint(t *testing.T) {
	require.Equal(t, []string{"/run/determined/interactive-supervisor.sh"},
		interactiveSupervisorEntrypoint())
	require.Equal(t, []string{"/run/determined/interactive-supervisor.sh", "-p", "3200", "-D"},
		interactiveSupervisorEntrypoint("-p", "3200", "-D"))
}

// Added variables join the task's other extra environment variables, replacing those of the same
// name.
func TestAddExtraEnvVarsKeepsOthers(t *testing.T) {
	var spec tasks.GenericCommandSpec
	addExtraEnvVars(&spec, map[string]string{"DET_TASK_TYPE": "SHELL"})
	require.Equal(t, map[string]string{"DET_TASK_TYPE": "SHELL"}, spec.Base.ExtraEnvVars)

	spec.Base.ExtraEnvVars = map[string]string{"DET_TASK_TYPE": "SHELL", "NOTEBOOK_PORT": "old"}
	addExtraEnvVars(&spec, map[string]string{"NOTEBOOK_PORT": "2900", "DET_CODE_SERVER_PORT": "2901"})
	require.Equal(t, map[string]string{
		"DET_TASK_TYPE": "SHELL", "NOTEBOOK_PORT": "2900", "DET_CODE_SERVER_PORT": "2901",
	}, spec.Base.ExtraEnvVars)
}

// requireInteractiveSupervisorFiles checks that files has the init, the supervisor and every unit
// at their run-dir paths, with the static content, owned by uid and gid.
func requireInteractiveSupervisorFiles(t *testing.T, files archive.Archive, uid, gid int) {
	t.Helper()
	want := map[string]string{
		"/run/determined/interactive-init.sh":       etc.InteractiveInitScriptResource,
		"/run/determined/interactive-supervisor.sh": etc.InteractiveSupervisorResource,
		"/run/determined/report_service_states.py":  etc.ServiceStateReporterResource,
		"/run/determined/services/ssh.sh":           etc.SSHServiceResource,
		"/run/determined/services/jupyter.sh":       etc.JupyterServiceResource,
		"/run/determined/services/vscode.sh":        etc.VSCodeServiceResource,
	}
	found := map[string]bool{}
	for _, item := range files {
		if item.Path == "/run/determined/services" {
			require.Equal(t, byte(tar.TypeDir), item.Type)
			require.Equal(t, 0o700, int(item.FileMode))
			require.Equal(t, uid, item.UserID)
			require.Equal(t, gid, item.GroupID)
			found[item.Path] = true
			continue
		}
		resource, ok := want[item.Path]
		if !ok {
			continue
		}
		require.False(t, found[item.Path], "%s appears once", item.Path)
		found[item.Path] = true
		require.Equal(t, byte(tar.TypeReg), item.Type, item.Path)
		require.Equal(t, 0o700, int(item.FileMode), item.Path)
		require.Equal(t, string(etc.MustStaticFile(resource)), string(item.Content), item.Path)
		require.Equal(t, uid, item.UserID, item.Path)
		require.Equal(t, gid, item.GroupID, item.Path)
	}
	require.Len(t, found, len(want)+1, "found %v", found)
}

func TestInteractiveSupervisorFiles(t *testing.T) {
	require.NoError(t, etc.SetRootPath("../static/srv"))
	requireInteractiveSupervisorFiles(t,
		interactiveSupervisorFiles(&model.AgentUserGroup{UID: 1001, GID: 1002}), 1001, 1002)
	requireInteractiveSupervisorFiles(t, interactiveSupervisorFiles(nil), 0, 0)
	// The config's services are the services that the supervisor has a unit for.
	units := maps.Keys(interactiveServiceUnits)
	slices.Sort(units)
	require.Equal(t, model.InteractiveServices, units)
}
