//go:build integration
// +build integration

package internal

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/hashicorp/go-cleanhttp"
	"github.com/labstack/echo/v4"
	"github.com/sirupsen/logrus"
	logrustest "github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	authz2 "github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/job/jobservice"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/proxy"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/internal/task/idle"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

/*
A set of tests to ensure that the NTSC APIs call the expected AuthZ methods.
*/

var authZNSC *mocks.NSCAuthZ

func setupNTSCAuthzTest(t *testing.T) (
	*apiServer, *mocks.NSCAuthZ, model.User, context.Context,
) {
	api, curUser, ctx := setupAPITest(t, nil)
	master := api.m

	cs, _ := command.NewService(master.db, master.rm)
	command.SetDefaultService(cs)

	jobservice.SetDefaultService(master.rm)

	authZNSC = &mocks.NSCAuthZ{}
	command.AuthZProvider.RegisterOverride("mock", authZNSC)
	config.GetMasterConfig().Security.AuthZ = config.AuthZConfig{Type: "mock"}
	return api, authZNSC, curUser, ctx
}

func setupNSCAuthZ() *mocks.NSCAuthZ {
	if authZNSC == nil {
		authZNSC = &mocks.NSCAuthZ{}
		command.AuthZProvider.RegisterOverride("mock", authZNSC)
	}
	return authZNSC
}

func TestTasksCountAuthZ(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	authz.On("CanGetActiveTasksCount", mock.Anything, curUser).Return(fmt.Errorf("deny"))
	_, err := api.GetActiveTasksCount(ctx, &apiv1.GetActiveTasksCountRequest{})
	require.Equal(t, status.Error(codes.PermissionDenied, "deny"), err)
}

func TestCanGetNTSC(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	var err error

	// check permission errors are returned with not found status and follow the same pattern.
	authz.On("CanGetNSC", mock.Anything, curUser, mock.Anything, mock.Anything).Return(
		authz2.PermissionDeniedError{}).Times(2)

	invalidID := "non-existing"

	// Commands.
	genericCmd, _ := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand,
		model.JobTypeCommand,
		mockGenericReq(t, api.m.db))

	cmd := genericCmd.ToV1Command()

	_, err = api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: invalidID})
	require.Equal(t, apiPkg.NotFoundErrs("command", invalidID, true), err)

	_, err = api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Id})
	require.Equal(t, apiPkg.NotFoundErrs("command", cmd.Id, true), err)

	// Shells.
	genericShell, _ := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell,
		model.JobTypeShell,
		mockGenericReq(t, api.m.db))
	shell := genericShell.ToV1Shell()

	_, err = api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: invalidID})
	require.Equal(t, apiPkg.NotFoundErrs("shell", invalidID, true), err)

	_, err = api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Id})
	require.Equal(t, apiPkg.NotFoundErrs("shell", shell.Id, true), err)

	// Tensorboards.
	// check permission errors are returned with not found status and follow the same pattern.
	authz.On("CanGetTensorboard", mock.Anything, curUser, mock.Anything, mock.Anything,
		mock.Anything).Return(authz2.PermissionDeniedError{}).Once()

	genericTb, _ := command.DefaultCmdService.LaunchGenericCommand(model.TaskTypeTensorboard,
		model.JobTypeTensorboard, mockGenericReq(t, api.m.db))
	tb := genericTb.ToV1Tensorboard()

	_, err = api.GetTensorboard(ctx, &apiv1.GetTensorboardRequest{TensorboardId: invalidID})
	require.Equal(t, apiPkg.NotFoundErrs("tensorboard", invalidID, true), err)

	_, err = api.GetTensorboard(ctx, &apiv1.GetTensorboardRequest{TensorboardId: tb.Id})
	require.Equal(t, apiPkg.NotFoundErrs("tensorboard", tb.Id, true), err)

	// check other errors are not returned with permission denied status.
	authz.On("CanGetNSC", mock.Anything, curUser, mock.Anything, mock.Anything).Return(
		fmt.Errorf("other error"),
	).Times(2)
	_, err = api.GetCommand(ctx, &apiv1.GetCommandRequest{CommandId: cmd.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
	require.NotEqual(t, codes.NotFound, status.Code(err))

	_, err = api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shell.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
	require.NotEqual(t, codes.NotFound, status.Code(err))

	authz.On("CanGetTensorboard", mock.Anything, curUser, mock.Anything, mock.Anything,
		mock.Anything).Return(fmt.Errorf("other error")).Once()

	_, err = api.GetTensorboard(ctx, &apiv1.GetTensorboardRequest{TensorboardId: tb.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
	require.NotEqual(t, codes.NotFound, status.Code(err))
}

func TestAuthZCanTerminateNSC(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	var err error
	authz.On("CanGetNSC", mock.Anything, curUser, mock.Anything, mock.Anything).Return(
		nil,
	)
	authz.On("CanGetTensorboard", mock.Anything, curUser, mock.Anything, mock.Anything,
		mock.Anything).Return(nil)

	// check permission errors are returned with permission denied status.
	authz.On("CanTerminateNSC", mock.Anything, curUser, mock.Anything).Return(
		authz2.PermissionDeniedError{},
	).Times(2)

	// Commands.
	genericCmd, _ := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand,
		model.JobTypeCommand,
		mockGenericReq(t, api.m.db))
	cmd := genericCmd.ToV1Command()
	_, err = api.KillCommand(ctx, &apiv1.KillCommandRequest{CommandId: cmd.Id})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// Shells.
	genericShell, _ := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell,
		model.JobTypeShell,
		mockGenericReq(t, api.m.db))
	shell := genericShell.ToV1Shell()
	_, err = api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: shell.Id})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// Tensorboards.
	authz.On("CanTerminateTensorboard", mock.Anything, curUser, mock.Anything).Return(
		authz2.PermissionDeniedError{},
	).Once()
	genericTb, _ := command.DefaultCmdService.LaunchGenericCommand(model.TaskTypeTensorboard,
		model.JobTypeTensorboard, mockGenericReq(t, api.m.db))
	tb := genericTb.ToV1Tensorboard()
	_, err = api.KillTensorboard(ctx, &apiv1.KillTensorboardRequest{TensorboardId: tb.Id})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// check other errors are not returned with permission denied status.
	authz.On("CanTerminateNSC", mock.Anything, curUser, mock.Anything).Return(
		fmt.Errorf("other error"),
	).Times(2)

	_, err = api.KillCommand(ctx, &apiv1.KillCommandRequest{CommandId: cmd.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))

	_, err = api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: shell.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))

	authz.On("CanTerminateTensorboard", mock.Anything, curUser, mock.Anything).Return(
		fmt.Errorf("other error"),
	)
	_, err = api.KillTensorboard(ctx, &apiv1.KillTensorboardRequest{TensorboardId: tb.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
}

// signalFailingAllocations is an allocation service that cannot signal allocations.
type signalFailingAllocations struct{ task.AllocationService }

func (signalFailingAllocations) Signal(model.AllocationID, task.AllocationSignal, string) error {
	return fmt.Errorf("signal failed")
}

// A shell kill that fails returns the error.
func TestKillShellError(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	authz.On("CanGetNSC", mock.Anything, curUser, mock.Anything).Return(nil)
	authz.On("CanTerminateNSC", mock.Anything, curUser, mock.Anything).Return(nil)
	authz.On("CanControlGenericTask", mock.Anything, curUser, mock.Anything,
		mock.Anything).Return(nil)
	shell, err := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell, model.JobTypeShell, mockGenericReq(t, api.m.db))
	require.NoError(t, err)

	allocations := task.DefaultService
	task.DefaultService = signalFailingAllocations{allocations}
	t.Cleanup(func() { task.DefaultService = allocations })
	resp, err := api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: shell.ToV1Shell().Id})
	require.Equal(t, codes.Internal, status.Code(err))
	require.Nil(t, resp)
}

func TestNSCControlChecksOwnerBeforeMutation(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	authz.On("CanGetNSC", mock.Anything, curUser, mock.Anything).Return(nil)
	authz.On("CanGetTensorboard", mock.Anything, curUser, mock.Anything,
		mock.Anything, mock.Anything).Return(nil)
	authz.On("CanTerminateNSC", mock.Anything, curUser, mock.Anything).Return(nil)
	authz.On("CanTerminateTensorboard", mock.Anything, curUser, mock.Anything).Return(nil)
	authz.On("CanSetNSCsPriority", mock.Anything, curUser, mock.Anything,
		mock.Anything).Return(nil)
	authz.On("CanControlGenericTask", mock.Anything, curUser, mock.Anything,
		mock.Anything).Return(authz2.PermissionDeniedError{}).Times(6)

	launch := func(taskType model.TaskType, jobType model.JobType) *command.Command {
		t.Helper()
		cmd, err := command.DefaultCmdService.LaunchGenericCommand(
			taskType, jobType, mockGenericReq(t, api.m.db))
		require.NoError(t, err)
		return cmd
	}
	cmd := launch(model.TaskTypeCommand, model.JobTypeCommand)
	shell := launch(model.TaskTypeShell, model.JobTypeShell)
	tensorboard := launch(model.TaskTypeTensorboard, model.JobTypeTensorboard)

	cmdID := cmd.ToV1Command().Id
	shellID, tbID := shell.ToV1Shell().Id, tensorboard.ToV1Tensorboard().Id
	states := []interface{}{cmd.ToV1Command().State,
		shell.ToV1Shell().State, tensorboard.ToV1Tensorboard().State}
	_, err := api.KillCommand(ctx, &apiv1.KillCommandRequest{CommandId: cmdID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.KillShell(ctx, &apiv1.KillShellRequest{ShellId: shellID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.KillTensorboard(ctx, &apiv1.KillTensorboardRequest{TensorboardId: tbID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.SetCommandPriority(ctx, &apiv1.SetCommandPriorityRequest{CommandId: cmdID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.SetShellPriority(ctx, &apiv1.SetShellPriorityRequest{ShellId: shellID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.SetTensorboardPriority(ctx,
		&apiv1.SetTensorboardPriorityRequest{TensorboardId: tbID})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	require.Equal(t, states, []interface{}{
		cmd.ToV1Command().State, shell.ToV1Shell().State, tensorboard.ToV1Tensorboard().State})
	authz.AssertExpectations(t)
}

// ntscUserCtx starts a session for u and returns a context that authenticates as u.
func ntscUserCtx(t *testing.T, u model.User) context.Context {
	token, err := user.StartSession(context.TODO(), &u)
	require.NoError(t, err)
	return metadata.NewIncomingContext(context.TODO(),
		metadata.Pairs("x-user-token", "Bearer "+token))
}

// nonOwnerCase names the test case of a user who neither owns a task nor is an admin.
const nonOwnerCase = "other"

// credentialReadLogs captures the audit lines for credential reads until the test ends.
func credentialReadLogs(t *testing.T) func() []*logrus.Entry {
	logger := logrus.StandardLogger()
	old := logrus.LevelHooks{}
	for level, hooks := range logger.Hooks {
		old[level] = append([]logrus.Hook(nil), hooks...)
	}
	hook := &logrustest.Hook{}
	logger.AddHook(hook)
	t.Cleanup(func() { logger.ReplaceHooks(old) })
	return func() []*logrus.Entry {
		var entries []*logrus.Entry
		for _, e := range hook.AllEntries() {
			if e.Message == "admin read the credential of another user's task" {
				entries = append(entries, e)
			}
		}
		return entries
	}
}

func requireCredentialReadLog(
	t *testing.T, entries []*logrus.Entry, admin model.User, credential, taskID string,
	ownerID int32,
) {
	require.Len(t, entries, 1)
	require.Equal(t, logrus.InfoLevel, entries[0].Level)
	require.Equal(t, logrus.Fields{
		"user": admin.Username, "user_id": admin.ID, "owner_id": ownerID,
		"task_id": taskID, "credential": credential,
	}, entries[0].Data)
}

func TestShellPrivateKeyOnlyForOwnerOrAdmin(t *testing.T) {
	api, authz, admin, adminCtx := setupNTSCAuthzTest(t)
	audits := credentialReadLogs(t)
	// Allow every workspace-level check, as RBAC does for a workspace member with UPDATE_NSC.
	// The key must still reach only the owner and admins.
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	authz.On("AccessibleScopes", mock.Anything, mock.Anything, mock.Anything).
		Return(model.AccessScopeSet{model.DefaultWorkspaceID: true}, nil)
	authz.On("CanTerminateNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	authz.On("CanSetNSCsPriority", mock.Anything, mock.Anything, mock.Anything,
		mock.Anything).Return(nil)
	authz.On("CanControlGenericTask", mock.Anything, mock.Anything, mock.Anything,
		mock.Anything).Return(nil)
	api.m.rm.(*mocks.ResourceManager).On("Release", mock.Anything).Return()

	req := mockGenericReq(t, api.m.db)
	req.Spec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	owner, err := user.ByID(context.TODO(), req.Spec.Base.Owner.ID)
	require.NoError(t, err)
	ownerCtx := ntscUserCtx(t, owner.ToUser())
	otherCtx := ntscUserCtx(t, db.RequireMockUser(t, api.m.db))

	launched, err := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell, model.JobTypeShell, req)
	require.NoError(t, err)
	shellID := launched.ToV1Shell().Id

	for name, ctx := range map[string]context.Context{
		"owner": ownerCtx, "admin": adminCtx, nonOwnerCase: otherCtx,
	} {
		shells, err := api.GetShells(ctx, &apiv1.GetShellsRequest{})
		require.NoError(t, err, name)
		require.Len(t, shells.Shells, 1, name)
		require.Equal(t, shellID, shells.Shells[0].Id, name)
		require.Empty(t, shells.Shells[0].PrivateKey, "GetShells returned a key to %s", name)

		shell, err := api.GetShell(ctx, &apiv1.GetShellRequest{ShellId: shellID})
		require.NoError(t, err, name)
		require.Equal(t, shellID, shell.Shell.Id, name)
		if name == nonOwnerCase {
			require.Empty(t, shell.Shell.PrivateKey, "GetShell returned a key to a non-owner")
		} else {
			require.Equal(t, *req.Spec.Metadata.PrivateKey, shell.Shell.PrivateKey, name)
		}
	}

	// Only the admin's read of another user's key is logged.
	requireCredentialReadLog(t, audits(), admin, "shell private key", shellID,
		launched.ToV1Shell().UserId)

	// A non-owner whom authz lets control the shell still gets no key back, and an admin who
	// controls it does not read the key.
	prio, err := api.SetShellPriority(otherCtx,
		&apiv1.SetShellPriorityRequest{ShellId: shellID, Priority: 10})
	require.NoError(t, err)
	require.Empty(t, prio.Shell.PrivateKey)
	killed, err := api.KillShell(adminCtx, &apiv1.KillShellRequest{ShellId: shellID})
	require.NoError(t, err)
	require.Empty(t, killed.Shell.PrivateKey)
	require.Len(t, audits(), 1)
}

// launchOwnedCommand launches a command with a proxied port for a new user. It returns the owner,
// the command's allocation ID and its proxy service ID.
func launchOwnedCommand(t *testing.T, api *apiServer) (model.User, model.AllocationID, string) {
	req := mockGenericReq(t, api.m.db)
	req.Spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{
		RawProxyPort: 8080, RawDefaultServiceID: ptrs.Ptr(true),
	}}
	owner, err := user.ByID(context.TODO(), req.Spec.Base.Owner.ID)
	require.NoError(t, err)
	cmd, err := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand, model.JobTypeCommand, req)
	require.NoError(t, err)
	taskID := cmd.ToV1Command().Id
	return owner.ToUser(), model.AllocationID(taskID + ".1"), taskID
}

// allocationSessionCtx returns a context that authenticates as a task container of allocation id,
// the way the harness's TaskSession does.
func allocationSessionCtx(t *testing.T, id model.AllocationID, u model.User) context.Context {
	token, err := db.StartAllocationSession(context.TODO(), id, &u)
	require.NoError(t, err)
	return metadata.NewIncomingContext(context.TODO(),
		metadata.Pairs("x-allocation-token", "Bearer "+token))
}

func TestAllocationMutationsRequireOwnSessionOwnerOrAdmin(t *testing.T) {
	api, authz, _, adminCtx := setupNTSCAuthzTest(t)
	// Everyone can see every task, as under basic authz.
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	rm := api.m.rm.(*mocks.ResourceManager)
	rm.On("Release", mock.Anything).Return()
	rm.On("NotifyContainerRunning", mock.Anything).Return(nil)

	owner, allocationID, _ := launchOwnedCommand(t, api)
	other, otherAllocationID, _ := launchOwnedCommand(t, api)
	id := string(allocationID)
	ownSession := allocationSessionCtx(t, allocationID, owner)

	calls := []struct {
		name string
		call func(context.Context) error
	}{
		{"AllocationReady", func(ctx context.Context) error {
			_, err := api.AllocationReady(ctx, &apiv1.AllocationReadyRequest{AllocationId: id})
			return err
		}},
		{"AllocationWaiting", func(ctx context.Context) error {
			_, err := api.AllocationWaiting(ctx, &apiv1.AllocationWaitingRequest{AllocationId: id})
			return err
		}},
		{"ReportTaskActivity", func(ctx context.Context) error {
			_, err := api.ReportTaskActivity(ctx,
				&apiv1.ReportTaskActivityRequest{AllocationId: id, Active: true})
			return err
		}},
		{"AllocationAllGather", func(ctx context.Context) error {
			_, err := api.AllocationAllGather(ctx, &apiv1.AllocationAllGatherRequest{
				AllocationId: id, RequestUuid: uuid.NewString(), NumPeers: 1,
				Data: &structpb.Struct{},
			})
			return err
		}},
		{"PostAllocationAcceleratorData", func(ctx context.Context) error {
			_, err := api.PostAllocationAcceleratorData(ctx,
				&apiv1.PostAllocationAcceleratorDataRequest{
					AllocationId:    id,
					AcceleratorData: &apiv1.AcceleratorData{ContainerId: uuid.NewString()},
				})
			return err
		}},
		{"AckAllocationPreemptionSignal", func(ctx context.Context) error {
			_, err := api.AckAllocationPreemptionSignal(ctx,
				&apiv1.AckAllocationPreemptionSignalRequest{AllocationId: id})
			return err
		}},
		{"NotifyContainerRunning", func(ctx context.Context) error {
			_, err := api.NotifyContainerRunning(ctx,
				&apiv1.NotifyContainerRunningRequest{AllocationId: id, NumPeers: 1})
			return err
		}},
		{"MarkAllocationResourcesDaemon", func(ctx context.Context) error {
			_, err := api.MarkAllocationResourcesDaemon(ctx,
				&apiv1.MarkAllocationResourcesDaemonRequest{AllocationId: id, ResourcesId: "r"})
			return err
		}},
		{"AllocationRendezvousInfo", func(ctx context.Context) error {
			_, err := api.AllocationRendezvousInfo(ctx,
				&apiv1.AllocationRendezvousInfoRequest{AllocationId: id, ResourcesId: "r"})
			return err
		}},
		{"AllocationPendingPreemptionSignal", func(ctx context.Context) error {
			_, err := api.AllocationPendingPreemptionSignal(ctx,
				&apiv1.AllocationPendingPreemptionSignalRequest{AllocationId: id})
			return err
		}},
	}

	// Users who can see the task but do not own it, and other tasks' sessions, are refused.
	for name, ctx := range map[string]context.Context{
		"user":       ntscUserCtx(t, other),
		"other task": allocationSessionCtx(t, otherAllocationID, other),
	} {
		for _, c := range calls {
			require.Equal(t, codes.PermissionDenied, status.Code(c.call(ctx)), "%s: %s", name, c.name)
		}
	}
	// Nothing changed: the allocation is still pending, not ready, and has no accelerator data.
	state, err := task.DefaultService.State(allocationID)
	require.NoError(t, err)
	require.Equal(t, model.AllocationStatePending, state.State)
	a, err := db.AllocationByID(context.TODO(), allocationID)
	require.NoError(t, err)
	require.False(t, a.IsReady != nil && *a.IsReady)
	accelerators, err := db.Bun().NewSelect().Table("allocation_accelerators").
		Where("allocation_id = ?", allocationID).Count(context.TODO())
	require.NoError(t, err)
	require.Zero(t, accelerators)

	// The allocation's own task session, its owner and admins pass the authorization check. The
	// last call terminates the allocation, so it runs once at the end.
	last := calls[len(calls)-1]
	for name, ctx := range map[string]context.Context{
		"task session": ownSession,
		"owner":        ntscUserCtx(t, owner),
		"admin":        adminCtx,
	} {
		for _, c := range calls[:len(calls)-1] {
			require.NotContains(t,
				[]codes.Code{codes.PermissionDenied, codes.Unauthenticated, codes.NotFound},
				status.Code(c.call(ctx)), "%s: %s", name, c.name)
		}
	}
	require.NoError(t, last.call(ownSession))
}

// A busy service's report from the task's container touches the task's activity record; an idle
// report does not. Another owner's user session and another owner's allocation token are refused;
// a token of the same owner's other allocation follows canControlAllocation, as AllocationReady
// does, and is not covered here.
func TestReportTaskActivity(t *testing.T) {
	api, authz, _, _ := setupNTSCAuthzTest(t)
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	owner, allocationID, taskID := launchOwnedCommand(t, api)
	other, otherAllocationID, _ := launchOwnedCommand(t, api)
	idle.Register(sproto.IdleTimeoutConfig{
		ServiceID: taskID, TaskID: model.TaskID(taskID), UseTaskActivity: true,
		TimeoutDuration: time.Hour,
	}, func(context.Context, error) {})
	defer idle.Unregister(taskID)
	report := func(ctx context.Context, active bool) error {
		_, err := api.ReportTaskActivity(ctx, &apiv1.ReportTaskActivityRequest{
			AllocationId: string(allocationID), Active: active,
		})
		return err
	}
	lastActivity := func() time.Time {
		a, ok := idle.ActivityOf(model.TaskID(taskID))
		require.True(t, ok)
		return a.LastActivity
	}

	for name, ctx := range map[string]context.Context{
		"other owner's user session":     ntscUserCtx(t, other),
		"other owner's allocation token": allocationSessionCtx(t, otherAllocationID, other),
	} {
		require.Equal(t, codes.PermissionDenied, status.Code(report(ctx, true)), name)
	}
	ownSession := allocationSessionCtx(t, allocationID, owner)
	require.NoError(t, report(ownSession, false))
	require.True(t, lastActivity().IsZero(), "refused and idle reports are not activity")

	before := time.Now()
	require.NoError(t, report(ownSession, true))
	require.False(t, lastActivity().Before(before))
	a, _ := idle.ActivityOf(model.TaskID(taskID))
	require.Zero(t, a.Holds, "a report never holds the task")
}

func TestPostAllocationProxyAddressOwnerOrAdmin(t *testing.T) {
	api, authz, _, adminCtx := setupNTSCAuthzTest(t)
	// Everyone can see every task, as under basic authz.
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	if proxy.DefaultProxy == nil {
		proxy.InitProxy(processProxyAuthentication, user.IsMasterSignedToken)
	}

	owner, allocationID, serviceID := launchOwnedCommand(t, api)
	other, otherAllocationID, _ := launchOwnedCommand(t, api)
	taskSessionCtx := func(id model.AllocationID, u model.User) context.Context {
		return allocationSessionCtx(t, id, u)
	}
	post := func(ctx context.Context, address string) error {
		_, err := api.PostAllocationProxyAddress(ctx, &apiv1.PostAllocationProxyAddressRequest{
			AllocationId: string(allocationID), ProxyAddress: address,
		})
		return err
	}
	requireUnchanged := func() {
		a, err := db.AllocationByID(context.TODO(), allocationID)
		require.NoError(t, err)
		require.Nil(t, a.ProxyAddress)
		require.Nil(t, proxy.DefaultProxy.GetService(serviceID))
	}

	// Users who can see the task but do not own it are refused, as is another task's session.
	for name, ctx := range map[string]context.Context{
		"user":              ntscUserCtx(t, other),
		"other task":        taskSessionCtx(otherAllocationID, other),
		"user, bad address": ntscUserCtx(t, other),
	} {
		address := "10.0.0.66"
		if name == "user, bad address" {
			address = "attacker.example:443"
		}
		require.Equal(t, codes.PermissionDenied, status.Code(post(ctx, address)), name)
		requireUnchanged()
	}

	_, err := api.PostAllocationProxyAddress(adminCtx, &apiv1.PostAllocationProxyAddressRequest{
		AllocationId: "missing.1", ProxyAddress: "10.0.0.1",
	})
	require.Equal(t, codes.NotFound, status.Code(err))

	// A job without an owner is refused for everyone but admins.
	jobID, taskID := model.NewJobID(), model.NewTaskID()
	require.NoError(t, db.AddJob(&model.Job{JobID: jobID, JobType: model.JobTypeCommand}))
	require.NoError(t, db.AddTask(context.TODO(), &model.Task{
		TaskID: taskID, TaskType: model.TaskTypeCommand, JobID: &jobID,
		StartTime: time.Now().UTC().Truncate(time.Millisecond),
	}))
	require.NoError(t, db.AddAllocation(context.TODO(), &model.Allocation{
		TaskID: taskID, AllocationID: model.AllocationID(taskID + ".1"),
		Slots: 1, ResourcePool: "default",
	}))
	_, err = api.PostAllocationProxyAddress(ntscUserCtx(t, owner),
		&apiv1.PostAllocationProxyAddressRequest{
			AllocationId: string(taskID) + ".1", ProxyAddress: "10.0.0.1",
		})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// The owner, the task's own session and admins pass the ownership check; the address must be
	// an IP, and an allocation that has no resources yet cannot take one.
	for name, ctx := range map[string]context.Context{
		"owner":        ntscUserCtx(t, owner),
		"task session": taskSessionCtx(allocationID, owner),
		"admin":        adminCtx,
	} {
		require.Equal(t, codes.InvalidArgument,
			status.Code(post(ctx, "attacker.example:443")), name)
		require.Equal(t, codes.FailedPrecondition, status.Code(post(ctx, "10.0.0.1")), name)
		requireUnchanged()
	}
}

// TestProxyKeepsMasterCredentialsFromServices runs the master's proxy authentication and credential
// filtering together, with real tokens, against a service that records what reaches it.
func TestProxyKeepsMasterCredentialsFromServices(t *testing.T) {
	api, authz, _, _ := setupNTSCAuthzTest(t)
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	user.InitService(api.m.db, &model.ExternalSessions{})
	if proxy.DefaultProxy == nil {
		proxy.InitProxy(processProxyAuthentication, user.IsMasterSignedToken)
	}

	ctx := context.TODO()
	owner, allocationID, taskID := launchOwnedCommand(t, api)
	sessionToken, err := user.StartSession(ctx, &owner)
	require.NoError(t, err)
	revokedToken, err := user.StartSession(ctx, &owner)
	require.NoError(t, err)
	require.NoError(t, user.DeleteSessionByToken(ctx, revokedToken))
	allocationToken, err := db.StartAllocationSession(ctx, allocationID, &owner)
	require.NoError(t, err)

	reached := make(chan http.Header, 1)
	service := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached <- r.Header.Clone()
	}))
	defer service.Close()
	serviceURL, err := url.Parse(service.URL)
	require.NoError(t, err)
	protected, public := taskID, taskID+":open"
	proxy.DefaultProxy.Register(protected, serviceURL, false, false)
	proxy.DefaultProxy.Register(public, serviceURL, false, true)
	defer proxy.DefaultProxy.Unregister(protected)
	defer proxy.DefaultProxy.Unregister(public)

	e := echo.New()
	e.Any("/proxy/:service/*", proxy.DefaultProxy.NewProxyHandler("service"))
	master := httptest.NewServer(e)
	defer master.Close()
	client := cleanhttp.DefaultClient()
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	send := func(serviceID string, header http.Header) (int, http.Header) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet,
			master.URL+"/proxy/"+serviceID+"/", nil)
		require.NoError(t, err)
		req.Header = header
		resp, err := client.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		select {
		case got := <-reached:
			return resp.StatusCode, got
		default:
			return resp.StatusCode, nil
		}
	}
	requireNoMasterCredentials := func(got http.Header) {
		require.Empty(t, got.Values("Authorization"))
		require.Empty(t, got.Values("Grpc-Metadata-X-Allocation-Token"))
		require.Equal(t, []string{"app=1"}, got.Values("Cookie"))
	}

	// A protected service needs a valid master session, and never sees it.
	code, got := send(protected, http.Header{
		"Authorization":                    {"Bearer " + sessionToken},
		"Grpc-Metadata-X-Allocation-Token": {"Bearer " + allocationToken},
		"Cookie":                           {"auth=" + sessionToken + "; app=1"},
	})
	require.Equal(t, http.StatusOK, code)
	requireNoMasterCredentials(got)
	for name, header := range map[string]http.Header{
		"revoked session": {"Authorization": {"Bearer " + revokedToken}},
		"service's key":   {"Authorization": {"Bearer app-key"}},
		"no credentials":  {},
	} {
		code, got = send(protected, header)
		require.NotEqual(t, http.StatusOK, code, name)
		require.Nil(t, got, name)
	}

	// A public service gets no master credential either, valid or not, but keeps its own.
	code, got = send(public, http.Header{
		"Authorization":                    {"Bearer " + revokedToken},
		"Grpc-Metadata-X-Allocation-Token": {"Bearer " + allocationToken},
		"Cookie":                           {"auth=" + sessionToken + "; app=1"},
	})
	require.Equal(t, http.StatusOK, code)
	requireNoMasterCredentials(got)
	code, got = send(public, http.Header{
		"Authorization": {"Bearer " + allocationToken},
		"Cookie":        {"app=1"},
	})
	require.Equal(t, http.StatusOK, code)
	requireNoMasterCredentials(got)
	code, got = send(public, http.Header{"Authorization": {"Bearer app-key"}})
	require.Equal(t, http.StatusOK, code)
	require.Equal(t, []string{"Bearer app-key"}, got.Values("Authorization"))
}

func TestAuthZCanSetNSCsPriority(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	var err error
	authz.On("CanGetNSC", mock.Anything, curUser, mock.Anything, mock.Anything).Return(
		nil,
	)
	authz.On("CanGetTensorboard", mock.Anything, curUser, mock.Anything, mock.Anything,
		mock.Anything).Return(nil)

	// check permission errors are returned with permission denied status.
	authz.On("CanSetNSCsPriority", mock.Anything, curUser, mock.Anything, mock.Anything).Return(
		authz2.PermissionDeniedError{},
	).Times(3)

	// Commands.
	genericCmd, _ := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand,
		model.JobTypeCommand,
		mockGenericReq(t, api.m.db))
	cmd := genericCmd.ToV1Command()
	_, err = api.SetCommandPriority(ctx, &apiv1.SetCommandPriorityRequest{CommandId: cmd.Id})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// Shells.
	genericShell, _ := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell,
		model.JobTypeShell,
		mockGenericReq(t, api.m.db))
	shell := genericShell.ToV1Shell()
	_, err = api.SetShellPriority(ctx, &apiv1.SetShellPriorityRequest{ShellId: shell.Id})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// Tensorboards.
	genericTb, _ := command.DefaultCmdService.LaunchGenericCommand(model.TaskTypeTensorboard,
		model.JobTypeTensorboard, mockGenericReq(t, api.m.db))
	tb := genericTb.ToV1Tensorboard()
	_, err = api.SetTensorboardPriority(ctx, &apiv1.SetTensorboardPriorityRequest{TensorboardId: tb.Id})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// check other errors are not returned with permission denied status.
	authz.On("CanSetNSCsPriority", mock.Anything, curUser, mock.Anything, mock.Anything).Return(
		fmt.Errorf("other error"),
	).Times(3)
	_, err = api.SetCommandPriority(ctx, &apiv1.SetCommandPriorityRequest{CommandId: cmd.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))

	_, err = api.SetShellPriority(ctx, &apiv1.SetShellPriorityRequest{ShellId: shell.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))

	_, err = api.SetTensorboardPriority(ctx, &apiv1.SetTensorboardPriorityRequest{TensorboardId: tb.Id})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
}

func TestAuthZCanCreateNSC(t *testing.T) {
	api, authz, curUser, ctx := setupNTSCAuthzTest(t)
	workspaceAuthZ := setupWorkspaceAuthZ()
	var err error

	mockUserArg := mock.MatchedBy(func(u model.User) bool {
		return u.ID == curUser.ID
	})

	// check permission errors are returned with permission denied status.
	authz.On("CanCreateNSC", mock.Anything, mockUserArg, mock.Anything).Return(
		authz2.PermissionDeniedError{},
	).Times(3)
	workspaceAuthZ.On("CanGetWorkspace", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Times(3)
	mockRM := MockRM()
	mockRM.On("SmallerValueIsHigherPriority", mock.Anything).Return(true, nil)
	api.m.rm = mockRM
	// A shell's preview is refused as its launch is.
	_, err = api.PreviewShell(ctx, &apiv1.PreviewShellRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.LaunchCommand(ctx, &apiv1.LaunchCommandRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = api.LaunchShell(ctx, &apiv1.LaunchShellRequest{})
	require.Equal(t, codes.PermissionDenied, status.Code(err))

	// check other errors are not returned with permission denied status.
	authz.On("CanCreateNSC", mock.Anything, mockUserArg, mock.Anything).Return(
		fmt.Errorf("other error"),
	).Times(3)
	workspaceAuthZ.On("CanGetWorkspace", mock.Anything, mock.Anything, mock.Anything).
		Return(nil).Times(3)
	_, err = api.PreviewShell(ctx, &apiv1.PreviewShellRequest{})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
	_, err = api.LaunchCommand(ctx, &apiv1.LaunchCommandRequest{})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
	_, err = api.LaunchShell(ctx, &apiv1.LaunchShellRequest{})
	require.Error(t, err)
	require.NotEqual(t, codes.PermissionDenied, status.Code(err))
}

// HACK: duplicated from command package.
func mockGenericReq(t *testing.T, pgDB *db.PgDB) *command.CreateGeneric {
	user := db.RequireMockUser(t, pgDB)
	cmdSpec := tasks.GenericCommandSpec{}
	key := "pass"
	cmdSpec.Base = tasks.TaskSpec{
		Owner:  &model.User{ID: user.ID},
		TaskID: string(model.NewTaskID()),
	}
	cmdSpec.CommandID = uuid.New().String()
	cmdSpec.Metadata.PrivateKey = &key
	cmdSpec.Metadata.PublicKey = &key
	return &command.CreateGeneric{Spec: &cmdSpec}
}
