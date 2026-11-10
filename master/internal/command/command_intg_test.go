//go:build integration
// +build integration

package command

import (
	"context"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ssh"
	"github.com/determined-ai/determined/master/pkg/syncx/queue"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/commandv1"
	"github.com/determined-ai/determined/proto/pkg/shellv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

func TestCommandManagerLifecycle(t *testing.T) {
	db := setupTest(t)

	// Launch a Command.
	cmd1 := launchCommand(t, db)

	// Get Command.
	resp1, err := DefaultCmdService.GetCommand(&apiv1.GetCommandRequest{CommandId: cmd1.Id})
	require.NotNil(t, resp1)
	require.NoError(t, err)

	// Launch another command.
	cmd2 := launchCommand(t, db)

	// Get Commands.
	resp2, err := DefaultCmdService.GetCommands(&apiv1.GetCommandsRequest{})
	require.NotNil(t, resp2)
	require.NoError(t, err)
	require.Len(t, resp2.Commands, 2)

	// Kill 1 command.
	resp3, err := DefaultCmdService.KillNTSC(cmd2.Id, model.TaskTypeCommand)
	require.NotNil(t, resp3)
	require.NoError(t, err)

	cmd3 := resp3.ToV1Command()
	require.Equal(t, taskv1.State_STATE_TERMINATED, cmd3.State)

	// Set command priority.
	resp4, err := DefaultCmdService.SetNTSCPriority(cmd1.Id, 0, model.TaskTypeCommand)
	require.NotNil(t, resp4)
	require.NoError(t, err)
}

func TestShellManagerLifecycle(t *testing.T) {
	db := setupTest(t)

	// Launch a Shell.
	cmd1 := launchShell(t, db)

	// Get Shell.
	resp1, err := DefaultCmdService.GetShell(&apiv1.GetShellRequest{ShellId: cmd1.Id})
	require.NotNil(t, resp1)
	require.NoError(t, err)

	// Summaries never carry the private key; it is read separately after an ownership check.
	require.Empty(t, cmd1.PrivateKey)
	require.Empty(t, resp1.Shell.PrivateKey)
	key, err := DefaultCmdService.GetShellPrivateKey(cmd1.Id)
	require.NoError(t, err)
	require.Equal(t, "pass", key)
	_, err = DefaultCmdService.GetShellPrivateKey(launchCommand(t, db).Id)
	require.Equal(t, codes.NotFound, status.Code(err))
	_, err = DefaultCmdService.GetShellPrivateKey(uuid.NewString())
	require.Equal(t, codes.NotFound, status.Code(err))

	// The SSH key of any task with an sshd: every shell, and another task launched with keys and
	// an sshd port, but not a task with keys alone.
	target, err := DefaultCmdService.ShellTerminalTarget(model.TaskID(cmd1.Id))
	require.NoError(t, err)
	require.Equal(t, "pass", string(target.PrivateKey))
	_, err = DefaultCmdService.ShellTerminalTarget(model.TaskID(launchCommand(t, db).Id))
	require.ErrorIs(t, err, ErrShellTerminalNotFound)
	_, err = DefaultCmdService.ShellTerminalTarget(model.TaskID(uuid.NewString()))
	require.ErrorIs(t, err, ErrShellTerminalNotFound)
	req := CreateMockGenericReq(t, db)
	req.Spec.Keys = &ssh.PrivateAndPublicKeys{}
	req.Spec.SSHDPort = 2222
	withSSHD, err := DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand, model.JobTypeCommand, req)
	require.NoError(t, err)
	target, err = DefaultCmdService.ShellTerminalTarget(model.TaskID(withSSHD.ToV1Command().Id))
	require.NoError(t, err)
	require.Equal(t, "pass", string(target.PrivateKey))

	// Launch another Shell.
	cmd2 := launchShell(t, db)

	// Get Shells.
	resp2, err := DefaultCmdService.GetShells(&apiv1.GetShellsRequest{})
	require.NotNil(t, resp2)
	require.NoError(t, err)
	require.Len(t, resp2.Shells, 2)
	for _, s := range resp2.Shells {
		require.Empty(t, s.PrivateKey)
	}

	// Kill 1 Shell.
	resp3, err := DefaultCmdService.KillNTSC(cmd2.Id, model.TaskTypeShell)
	require.NotNil(t, resp3)
	require.NoError(t, err)

	shell3 := resp3.ToV1Shell()
	require.Equal(t, taskv1.State_STATE_TERMINATED, shell3.State)
	require.Empty(t, shell3.PrivateKey)

	// Set Shell priority.
	resp4, err := DefaultCmdService.SetNTSCPriority(cmd1.Id, 0, model.TaskTypeShell)
	require.NotNil(t, resp4)
	require.NoError(t, err)
	require.Empty(t, resp4.ToV1Shell().PrivateKey)
}

// The lists of shells and commands report the slot count each one requests, also
// when it requests none.
func TestNTSCListsReportSlots(t *testing.T) {
	db := setupTest(t)

	for _, slots := range []int{0, 2} {
		request := func() *CreateGeneric {
			req := CreateMockGenericReq(t, db)
			req.Spec.Config.Resources.Slots = slots
			return req
		}
		launch := func(taskType model.TaskType, jobType model.JobType) string {
			cmd, err := DefaultCmdService.LaunchGenericCommand(taskType, jobType, request())
			require.NoError(t, err)
			return cmd.stringID()
		}
		listed := map[string]int32{}

		commandID := launch(model.TaskTypeCommand, model.JobTypeCommand)
		commands, err := DefaultCmdService.GetCommands(&apiv1.GetCommandsRequest{})
		require.NoError(t, err)
		for _, c := range commands.Commands {
			listed[c.Id] = c.Slots
		}
		require.Contains(t, listed, commandID, "command with %d slots", slots)
		require.Equal(t, int32(slots), listed[commandID], "command with %d slots", slots)

		shellID := launch(model.TaskTypeShell, model.JobTypeShell)
		shells, err := DefaultCmdService.GetShells(&apiv1.GetShellsRequest{})
		require.NoError(t, err)
		for _, s := range shells.Shells {
			listed[s.Id] = s.Slots
		}
		require.Contains(t, listed, shellID, "shell with %d slots", slots)
		require.Equal(t, int32(slots), listed[shellID], "shell with %d slots", slots)
	}
}

func TestNTSCEndTime(t *testing.T) {
	pgDB := setupTest(t)
	v1cmd := launchCommand(t, pgDB)
	require.Nil(t, v1cmd.EndTime)

	cmd := DefaultCmdService.commands[model.TaskID(v1cmd.Id)]
	cmd.OnExit(&task.AllocationExited{FinalState: task.AllocationState{
		State: model.AllocationStateTerminated,
	}})

	endTime := cmd.ToV1Command().EndTime
	require.NotNil(t, endTime)
	completed, err := db.TaskByID(context.Background(), model.TaskID(v1cmd.Id))
	require.NoError(t, err)
	require.NotNil(t, completed.EndTime)
	require.WithinDuration(t, *completed.EndTime, endTime.AsTime(), time.Microsecond)
}

func setupTest(t *testing.T) *db.PgDB {
	// First init the new Command Service
	var mockRM mocks.ResourceManager
	sub := sproto.NewAllocationSubscription(queue.New[sproto.ResourcesEvent](), func() {})
	mockRM.On("Allocate", mock.Anything, mock.Anything).Return(sub, nil)
	mockRM.On("Release", mock.Anything, mock.Anything).Return(nil)
	mockRM.On("SetGroupPriority", mock.Anything, mock.Anything).Return(nil)
	mockRM.On("SmallerValueIsHigherPriority").Return(true, nil)

	cs, _ := NewService(db.SingleDB(), &mockRM)
	SetDefaultService(cs)

	jobservice.SetDefaultService(&mockRM)

	require.NotNil(t, DefaultCmdService)
	return db.SingleDB()
}

func CreateMockGenericReq(t *testing.T, pgDB *db.PgDB) *CreateGeneric {
	user := db.RequireMockUser(t, pgDB)
	cmdSpec := tasks.GenericCommandSpec{}
	key := "pass"
	cmdSpec.Base = tasks.TaskSpec{
		Owner:        &model.User{ID: user.ID},
		TaskID:       string(model.NewTaskID()),
		ExtraEnvVars: map[string]string{},
	}
	cmdSpec.CommandID = uuid.New().String()
	cmdSpec.Metadata.PrivateKey = &key
	cmdSpec.Metadata.PublicKey = &key
	cmdSpec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	cmdSpec.Base.Workspace = model.DefaultWorkspaceName
	return &CreateGeneric{Spec: &cmdSpec}
}

func launchCommand(t *testing.T, db *db.PgDB) *commandv1.Command {
	cmd, err := DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand,
		model.JobTypeCommand,
		CreateMockGenericReq(t, db))
	v1cmd := cmd.ToV1Command()
	require.NoError(t, err)
	require.NotNil(t, cmd)
	require.NotNil(t, DefaultCmdService.commands[model.TaskID(v1cmd.Id)])
	return v1cmd
}

func launchShell(t *testing.T, db *db.PgDB) *shellv1.Shell {
	cmd, err := DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell,
		model.JobTypeShell,
		CreateMockGenericReq(t, db))
	v1shell := cmd.ToV1Shell()
	require.NoError(t, err)
	require.NotNil(t, v1shell)
	require.NotNil(t, DefaultCmdService.commands[model.TaskID(v1shell.Id)])
	return v1shell
}
