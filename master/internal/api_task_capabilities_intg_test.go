//go:build integration
// +build integration

package internal

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/proto"

	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
)

// TestJobPrivacyGetTaskCapabilities checks that GetTask returns the user's capabilities on the
// task's job: all of them for the owner and an admin, and for another user those of the job's
// sharing, for a shared command, a hidden shell and a trial task without a job of its own, which
// takes its experiment's. They match those of the command, the shell and the experiment. A shell
// that is no longer in the command registry still gets them, and they follow a change of its
// sharing.
func TestJobPrivacyGetTaskCapabilities(t *testing.T) {
	var aux, compute string
	mockRM := mockRMWithResolver(resolveOmittedPoolTo(&aux, &compute))
	api, admin, adminCtx := setupAPITest(t, nil, mockRM)
	lockJobPrivacyPolicy(t)
	oldCommands := command.DefaultCmdService
	t.Cleanup(func() { command.SetDefaultService(oldCommands) })
	cs, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	aux = accessTestPool(t, "aux", admin, false)
	compute = accessTestPool(t, "compute", admin, false)
	owner := addGCTestOwner(t)
	other := addGCTestOwner(t)
	ownerCtx := userContext(t, owner)

	exp := createTestExp(t, api, owner)
	shareJob(ownerCtx, t, api, exp.JobID.String(), false, false, false)
	_, jobless := db.RequireMockTrial(t, db.SingleDB(), exp)
	_, err = db.Bun().NewRaw(`UPDATE tasks SET job_id = NULL WHERE task_id = ?`,
		jobless.TaskID).Exec(adminCtx)
	require.NoError(t, err)

	services := &exitAllocationService{
		AllocationService: task.DefaultService,
		onExit:            map[model.TaskID]func(*task.AllocationExited){},
	}
	oldService := task.DefaultService
	task.DefaultService = services
	t.Cleanup(func() { task.DefaultService = oldService })
	cmd, err := api.LaunchCommand(ownerCtx, &apiv1.LaunchCommandRequest{})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, cmd.Command.JobId, true, true, true)
	shell, err := api.LaunchShell(ownerCtx, &apiv1.LaunchShellRequest{})
	require.NoError(t, err)
	shareJob(ownerCtx, t, api, shell.Shell.JobId, false, false, false)

	all := &jobv1.JobCapabilities{
		ViewTitle: true, ViewLogs: true, ViewConfig: true, ManageSharing: true,
	}
	shared := &jobv1.JobCapabilities{ViewTitle: true, ViewLogs: true, ViewConfig: true}
	hidden := &jobv1.JobCapabilities{}
	requireCaps := func(t *testing.T, want, got *jobv1.JobCapabilities, msg string) {
		t.Helper()
		require.NotNil(t, got, msg)
		require.True(t, proto.Equal(want, got), "%s: want %v, got %v", msg, want, got)
	}
	viewers := map[string]struct {
		user model.User
		// The capabilities on a shared job and on a hidden one.
		shared, hidden *jobv1.JobCapabilities
	}{
		"owner": {owner, all, all},
		"admin": {admin, all, all},
		"other": {other, shared, hidden},
	}
	taskCapsOf := func(t *testing.T, ctx context.Context, taskID string) *jobv1.JobCapabilities {
		t.Helper()
		resp, err := api.GetTask(ctx, &apiv1.GetTaskRequest{TaskId: taskID})
		require.NoError(t, err, taskID)
		return resp.Task.Capabilities
	}

	for name, vw := range viewers {
		t.Run(name, func(t *testing.T) {
			ctx := userContext(t, vw.user)
			cmdCaps := taskCapsOf(t, ctx, cmd.Command.Id)
			requireCaps(t, vw.shared, cmdCaps, "command")
			getCmd, err := api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Command.Id})
			require.NoError(t, err)
			requireCaps(t, getCmd.Command.Capabilities, cmdCaps, "command as GetCommand")

			shellCaps := taskCapsOf(t, ctx, shell.Shell.Id)
			requireCaps(t, vw.hidden, shellCaps, "shell")
			getShell, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Shell.Id})
			require.NoError(t, err)
			requireCaps(t, getShell.Shell.Capabilities, shellCaps, "shell as GetShell")

			trialCaps := taskCapsOf(t, ctx, string(jobless.TaskID))
			requireCaps(t, vw.hidden, trialCaps, "trial task without a job")
			getExp, err := api.GetExperiment(ctx, &apiv1.GetExperimentRequest{
				ExperimentId: int32(exp.ID),
			})
			require.NoError(t, err)
			requireCaps(t, getExp.Experiment.Capabilities, trialCaps, "trial task as GetExperiment")
		})
	}

	// The shell ends and leaves the command registry, as it does a day after its end; its
	// records stay in the database.
	services.exit(t, shell.Shell.Id, nil)
	reclaimed, err := command.NewService(api.m.db, api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(reclaimed)
	shells, err := api.GetShells(ownerCtx, &apiv1.GetShellsRequest{})
	require.NoError(t, err)
	for _, s := range shells.Shells {
		require.NotEqual(t, shell.Shell.Id, s.Id, "the shell is still in the command registry")
	}

	// Logs need the title.
	for _, step := range []struct {
		shared bool
		other  *jobv1.JobCapabilities
	}{
		{false, hidden},
		{true, &jobv1.JobCapabilities{ViewTitle: true, ViewLogs: true}},
		{false, hidden},
	} {
		shareJob(ownerCtx, t, api, shell.Shell.JobId, step.shared, step.shared, false)
		for name, vw := range viewers {
			want := vw.hidden
			if name == "other" {
				want = step.other
			}
			requireCaps(t, want, taskCapsOf(t, userContext(t, vw.user), shell.Shell.Id),
				"reclaimed shell as "+name)
		}
	}
}
