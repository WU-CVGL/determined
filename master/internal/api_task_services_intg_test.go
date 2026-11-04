//go:build integration
// +build integration

package internal

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/mock"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	apiPkg "github.com/determined-ai/determined/master/internal/api"
	authz2 "github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/mocks"
	"github.com/determined-ai/determined/master/internal/rm/rmevents"
	"github.com/determined-ai/determined/master/internal/sproto"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// servicesTest is an API server with mocked NSC authz under which every user may see every task,
// except the user denied, who may see none, and a resource manager that ends an allocation when
// it is killed.
type servicesTest struct {
	api    *apiServer
	admin  model.User
	denied model.User
}

func setupServicesTest(t *testing.T) (*servicesTest, context.Context) {
	api, authz, admin, adminCtx := setupNTSCAuthzTest(t)
	denied := db.RequireMockUser(t, api.m.db)
	isDenied := mock.MatchedBy(func(u model.User) bool { return u.ID == denied.ID })
	authz.On("CanGetNSC", mock.Anything, isDenied, mock.Anything).
		Return(authz2.PermissionDeniedError{})
	authz.On("CanGetNSC", mock.Anything, mock.Anything, mock.Anything).Return(nil)
	rm := api.m.rm.(*mocks.ResourceManager)
	rm.On("Release", mock.Anything).Return().Run(func(args mock.Arguments) {
		release := args.Get(0).(sproto.ResourcesReleased)
		if release.ResourcesID == nil {
			rmevents.Publish(release.AllocationID, sproto.ResourcesReleasedEvent{})
		}
	})
	return &servicesTest{api: api, admin: admin, denied: denied}, adminCtx
}

// launchShellWithServices launches a shell of a new user with the jupyter service, as LaunchShell
// does, and returns its owner and ID.
func (s *servicesTest) launchShellWithServices(t *testing.T) (model.User, string) {
	t.Helper()
	req := mockGenericReq(t, s.api.m.db)
	req.Spec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	req.Spec.Base.AgentUserGroup = &model.AgentUserGroup{User: "alice"}
	setShellSSHD(req.Spec, minSshdPort)
	_, err := addShellJupyter(req.Spec)
	require.NoError(t, err)
	owner, err := user.ByID(context.TODO(), req.Spec.Base.Owner.ID)
	require.NoError(t, err)
	cmd, err := command.DefaultCmdService.LaunchShellCommand(req, req.Spec.Base.Owner)
	require.NoError(t, err)
	return owner.ToUser(), cmd.ToV1Shell().Id
}

func (s *servicesTest) services(
	ctx context.Context, t *testing.T, taskID string,
) *apiv1.GetTaskServicesResponse {
	t.Helper()
	resp, err := s.api.GetTaskServices(ctx, &apiv1.GetTaskServicesRequest{TaskId: taskID})
	require.NoError(t, err)
	return resp
}

func serviceStatesOf(resp *apiv1.GetTaskServicesResponse) map[string]taskv1.TaskService_State {
	states := map[string]taskv1.TaskService_State{}
	for _, s := range resp.Services {
		states[s.Name] = s.State
	}
	return states
}

// A shell with jupyter lists ssh, its primary service, at the task ID over TCP and jupyter at
// "<task>:<port>" over HTTP with its URL, to the owner, admins and other users who may see the
// task; only the owner and admins may connect. A user who may not see the task gets NotFound, as
// GetShell gives.
func TestGetTaskServicesShellWithJupyter(t *testing.T) {
	s, adminCtx := setupServicesTest(t)
	owner, taskID := s.launchShellWithServices(t)
	spec := launchedSpec(t, taskID)
	jupyterID := taskID + ":" + strconv.Itoa(spec.Services[1].Port)
	other := db.RequireMockUser(t, s.api.m.db)

	for name, ctx := range map[string]context.Context{
		"owner":      ntscUserCtx(t, owner),
		"admin":      adminCtx,
		nonOwnerCase: ntscUserCtx(t, other),
	} {
		mayConnect := name != nonOwnerCase
		resp := s.services(ctx, t, taskID)
		require.Equal(t, &apiv1.GetTaskServicesResponse{
			TaskId:      taskID,
			UserId:      int32(owner.ID),
			WorkspaceId: int32(model.DefaultWorkspaceID),
			Primary:     model.ServiceSSH,
			Services: []*taskv1.TaskService{
				{
					Name: model.ServiceSSH, ServiceId: taskID,
					Protocol: taskv1.TaskService_PROTOCOL_TCP, MayConnect: mayConnect,
				},
				{
					Name: model.ServiceJupyter, ServiceId: jupyterID,
					Protocol: taskv1.TaskService_PROTOCOL_HTTP, Url: "/proxy/" + jupyterID + "/",
					MayConnect: mayConnect,
				},
			},
		}, resp, name)
	}

	_, err := s.api.GetTaskServices(ntscUserCtx(t, s.denied),
		&apiv1.GetTaskServicesRequest{TaskId: taskID})
	require.Equal(t, apiPkg.NotFoundErrs("task", taskID, true), err)
	_, err = s.api.GetShell(ntscUserCtx(t, s.denied), &apiv1.GetShellRequest{ShellId: taskID})
	require.Equal(t, codes.NotFound, status.Code(err))
	missing := string(model.NewTaskID())
	_, err = s.api.GetTaskServices(adminCtx, &apiv1.GetTaskServicesRequest{TaskId: missing})
	require.Equal(t, apiPkg.NotFoundErrs("task", missing, true), err)
}

// Tasks saved without service records list the service of their type: a shell its sshd at the
// task ID, a notebook JupyterLab at the task ID; a command has no services.
func TestGetTaskServicesWithoutRecords(t *testing.T) {
	s, adminCtx := setupServicesTest(t)

	shellReq := mockGenericReq(t, s.api.m.db)
	shellReq.Spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{
		RawProxyPort: minSshdPort, RawProxyTCP: ptrs.Ptr(true),
		RawUnauthenticated: ptrs.Ptr(true), RawDefaultServiceID: ptrs.Ptr(true),
	}}
	shell, err := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeShell, model.JobTypeShell, shellReq)
	require.NoError(t, err)
	shellID := shell.ToV1Shell().Id
	require.Equal(t, []*taskv1.TaskService{{
		Name: model.ServiceSSH, ServiceId: shellID, Protocol: taskv1.TaskService_PROTOCOL_TCP,
		MayConnect: true,
	}}, s.services(adminCtx, t, shellID).Services)

	notebookReq := mockGenericReq(t, s.api.m.db)
	notebookReq.Spec.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{
		RawProxyPort: minNotebookPort, RawDefaultServiceID: ptrs.Ptr(true),
	}}
	notebook, err := command.DefaultCmdService.LaunchNotebookCommand(
		notebookReq, notebookReq.Spec.Base.Owner)
	require.NoError(t, err)
	notebookID := notebook.ToV1Notebook().Id
	resp := s.services(adminCtx, t, notebookID)
	require.Equal(t, model.ServiceJupyter, resp.Primary)
	require.Equal(t, []*taskv1.TaskService{{
		Name: model.ServiceJupyter, ServiceId: notebookID,
		Protocol: taskv1.TaskService_PROTOCOL_HTTP, Url: "/proxy/" + notebookID + "/",
		MayConnect: true,
	}}, resp.Services)

	_, _, commandID := launchOwnedCommand(t, s.api)
	resp = s.services(adminCtx, t, commandID)
	require.Empty(t, resp.Primary)
	require.Empty(t, resp.Services)
	require.NotNil(t, resp.Services)
}

// The task's container reports the snapshot of its services' states with its allocation token,
// and the listing shows it. As for AllocationReady, the task's owner, the owner's other tasks'
// containers and admins may report it too; other users and their tasks' containers are refused,
// as are snapshots that name an unknown service, miss one, carry the unspecified state. A
// snapshot with a lower sequence than the last accepted one changes nothing. When the task ends,
// every service is EXITED, also once the command service no longer has the task.
func TestReportTaskServices(t *testing.T) {
	const (
		ssh     = model.ServiceSSH
		jupyter = model.ServiceJupyter
	)
	s, adminCtx := setupServicesTest(t)
	owner, taskID := s.launchShellWithServices(t)
	allocationID := model.AllocationID(taskID + ".1")
	other, _, otherTaskID := launchOwnedCommand(t, s.api)
	otherAllocationID := model.AllocationID(otherTaskID + ".1")
	ownSession := allocationSessionCtx(t, allocationID, owner)
	// Another task of the same owner.
	sameOwnerReq := mockGenericReq(t, s.api.m.db)
	sameOwnerReq.Spec.Base.Owner = &model.User{ID: owner.ID}
	sameOwnerCmd, err := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand, model.JobTypeCommand, sameOwnerReq)
	require.NoError(t, err)
	sameOwnerSession := allocationSessionCtx(
		t, model.AllocationID(sameOwnerCmd.ToV1Command().Id+".1"), owner)

	report := func(
		ctx context.Context, sequence int32, states map[string]taskv1.TaskService_State,
	) error {
		req := &apiv1.ReportTaskServicesRequest{AllocationId: string(allocationID), Sequence: sequence}
		for _, name := range []string{ssh, jupyter, "vscode"} {
			if state, ok := states[name]; ok {
				req.Services = append(req.Services, &apiv1.TaskServiceReport{Name: name, State: state})
			}
		}
		_, err := s.api.ReportTaskServices(ctx, req)
		return err
	}
	all := func(sshState, jupyterState taskv1.TaskService_State) map[string]taskv1.TaskService_State {
		return map[string]taskv1.TaskService_State{ssh: sshState, jupyter: jupyterState}
	}
	unspecified := all(taskv1.TaskService_STATE_UNSPECIFIED, taskv1.TaskService_STATE_UNSPECIFIED)
	starting := all(taskv1.TaskService_STATE_STARTING, taskv1.TaskService_STATE_STARTING)
	ready := all(taskv1.TaskService_STATE_READY, taskv1.TaskService_STATE_UNAVAILABLE)
	exited := all(taskv1.TaskService_STATE_EXITED, taskv1.TaskService_STATE_EXITED)
	require.Equal(t, unspecified, serviceStatesOf(s.services(adminCtx, t, taskID)))

	// Refused for another user and that user's task.
	for name, ctx := range map[string]context.Context{
		"user":               ntscUserCtx(t, other),
		"other owner's task": allocationSessionCtx(t, otherAllocationID, other),
	} {
		require.Equal(t, codes.PermissionDenied, status.Code(report(ctx, 1, ready)), name)
	}
	require.Error(t, report(context.Background(), 1, ready))
	require.Equal(t, unspecified, serviceStatesOf(s.services(adminCtx, t, taskID)))

	// A snapshot must hold every service of the task once, each with a state.
	for name, invalid := range map[string]map[string]taskv1.TaskService_State{
		"unknown service": {
			ssh: taskv1.TaskService_STATE_READY, jupyter: taskv1.TaskService_STATE_READY,
			"vscode": taskv1.TaskService_STATE_READY,
		},
		"missing service": {ssh: taskv1.TaskService_STATE_READY},
		"unspecified":     all(taskv1.TaskService_STATE_READY, taskv1.TaskService_STATE_UNSPECIFIED),
	} {
		require.Equal(t, codes.InvalidArgument, status.Code(report(ownSession, 1, invalid)), name)
	}
	require.Equal(t, unspecified, serviceStatesOf(s.services(adminCtx, t, taskID)))

	// The container's snapshots; a re-post of the current one is accepted, an older one is not.
	require.NoError(t, report(ownSession, 1, starting))
	require.Equal(t, starting, serviceStatesOf(s.services(adminCtx, t, taskID)))
	// The owner, the owner's other task and an admin pass the same gate.
	sshReady := all(taskv1.TaskService_STATE_READY, taskv1.TaskService_STATE_STARTING)
	reporters := map[string]context.Context{
		"owner":              ntscUserCtx(t, owner),
		"owner's other task": sameOwnerSession,
		"admin":              adminCtx,
	}
	for i, name := range []string{"owner", "owner's other task", "admin"} {
		states := sshReady
		if i%2 == 1 {
			states = starting
		}
		require.NoError(t, report(reporters[name], int32(2+i), states), name)
		require.Equal(t, states, serviceStatesOf(s.services(adminCtx, t, taskID)), name)
	}
	require.NoError(t, report(ownSession, 6, ready))
	require.NoError(t, report(ownSession, 6, ready))
	require.NoError(t, report(ownSession, 5, starting))
	require.Equal(t, ready, serviceStatesOf(s.services(ntscUserCtx(t, owner), t, taskID)))
	require.Equal(t, ready, serviceStatesOf(s.services(ntscUserCtx(t, other), t, taskID)))
	// Another task's services are its own.
	require.Empty(t, s.services(adminCtx, t, otherTaskID).Services)

	// The end of the task.
	_, err = command.DefaultCmdService.KillNTSC(taskID, model.TaskTypeShell)
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return serviceStatesOf(s.services(adminCtx, t, taskID))[ssh] ==
			taskv1.TaskService_STATE_EXITED
	}, 10*time.Second, 50*time.Millisecond)
	require.Equal(t, exited, serviceStatesOf(s.services(adminCtx, t, taskID)))
	// A late snapshot changes nothing: the allocation session goes with the allocation, so the
	// post is refused as unauthenticated, or ignored if the session is still there.
	err = report(ownSession, 7, all(taskv1.TaskService_STATE_READY, taskv1.TaskService_STATE_READY))
	require.Contains(t, []codes.Code{codes.OK, codes.Unauthenticated}, status.Code(err))
	require.Equal(t, exited, serviceStatesOf(s.services(adminCtx, t, taskID)))

	// A master without the command, as after a restart once the task has ended, lists them from
	// the task's persisted state.
	running := command.DefaultCmdService
	cs, err := command.NewService(s.api.m.db, s.api.m.rm)
	require.NoError(t, err)
	command.SetDefaultService(cs)
	t.Cleanup(func() { command.SetDefaultService(running) })
	resp := s.services(adminCtx, t, taskID)
	require.Equal(t, exited, serviceStatesOf(resp))
	require.Equal(t, int32(owner.ID), resp.UserId)
	require.Equal(t, model.ServiceSSH, resp.Primary)
}

// The owner and admins get each service's credential, an admin's read of another user's is
// logged; other users are refused, and an unknown service is not found.
func TestGetTaskServiceCredential(t *testing.T) {
	s, adminCtx := setupServicesTest(t)
	audits := credentialReadLogs(t)
	owner, taskID := s.launchShellWithServices(t)
	spec := launchedSpec(t, taskID)
	token := spec.Base.ExtraEnvVars[model.NotebookSessionEnvVar]
	require.NotEmpty(t, token)
	jupyterID := taskID + ":" + strconv.Itoa(spec.Services[1].Port)
	other := db.RequireMockUser(t, s.api.m.db)

	credential := func(
		ctx context.Context, service string,
	) (*apiv1.GetTaskServiceCredentialResponse, error) {
		return s.api.GetTaskServiceCredential(ctx, &apiv1.GetTaskServiceCredentialRequest{
			TaskId: taskID, Service: service,
		})
	}
	for name, ctx := range map[string]context.Context{
		"owner": ntscUserCtx(t, owner), "admin": adminCtx,
	} {
		ssh, err := credential(ctx, model.ServiceSSH)
		require.NoError(t, err, name)
		require.Equal(t, &apiv1.GetTaskServiceCredentialResponse{
			PrivateKey: spec.Metadata.PrivateKey,
			PublicKey:  spec.Metadata.PublicKey,
			User:       ptrs.Ptr("alice"),
		}, ssh, name)

		jupyter, err := credential(ctx, model.ServiceJupyter)
		require.NoError(t, err, name)
		require.Equal(t, &apiv1.GetTaskServiceCredentialResponse{
			Token: ptrs.Ptr(token),
			Url:   ptrs.Ptr("/proxy/" + jupyterID + "/?token=" + token),
		}, jupyter, name)
	}
	entries := audits()
	require.Len(t, entries, 2)
	requireCredentialReadLog(t, entries[:1], s.admin, "ssh credential", taskID, int32(owner.ID))
	requireCredentialReadLog(t, entries[1:], s.admin, "jupyter credential", taskID,
		int32(owner.ID))

	for _, service := range []string{model.ServiceSSH, model.ServiceJupyter} {
		_, err := credential(ntscUserCtx(t, other), service)
		require.Equal(t, codes.PermissionDenied, status.Code(err), service)
		_, err = credential(ntscUserCtx(t, s.denied), service)
		require.Equal(t, apiPkg.NotFoundErrs("task", taskID, true), err, service)
	}
	_, err := credential(ntscUserCtx(t, owner), "vscode")
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Len(t, audits(), 2)

	// A command has no services.
	commandOwner, _, commandID := launchOwnedCommand(t, s.api)
	_, err = s.api.GetTaskServiceCredential(ntscUserCtx(t, commandOwner),
		&apiv1.GetTaskServiceCredentialRequest{TaskId: commandID, Service: model.ServiceSSH})
	require.Equal(t, codes.NotFound, status.Code(err))
}

// The vscode service is listed over HTTP at its URL, and its credential, the password of
// code-server's login form and the URL whose "login" path takes it, goes to the owner and admins,
// an admin's read logged; other users are refused.
func TestGetTaskServiceCredentialVSCode(t *testing.T) {
	s, adminCtx := setupServicesTest(t)
	audits := credentialReadLogs(t)
	req := mockGenericReq(t, s.api.m.db)
	req.Spec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	setShellSSHD(req.Spec, minSshdPort)
	_, err := addShellVSCode(req.Spec, config.CodeServerConfig{Version: "4.96.4", Dir: t.TempDir()})
	require.NoError(t, err)
	ownerRecord, err := user.ByID(context.TODO(), req.Spec.Base.Owner.ID)
	require.NoError(t, err)
	owner := ownerRecord.ToUser()
	cmd, err := command.DefaultCmdService.LaunchShellCommand(req, req.Spec.Base.Owner)
	require.NoError(t, err)
	taskID := cmd.ToV1Shell().Id
	spec := launchedSpec(t, taskID)
	require.NotNil(t, spec.Metadata.CodeServerPassword)
	password := *spec.Metadata.CodeServerPassword
	vscodeID := taskID + ":" + strconv.Itoa(spec.Services[1].Port)
	other := db.RequireMockUser(t, s.api.m.db)

	listed := s.services(ntscUserCtx(t, owner), t, taskID).Services
	require.Len(t, listed, 2)
	require.Equal(t, &taskv1.TaskService{
		Name: model.ServiceVSCode, ServiceId: vscodeID, Protocol: taskv1.TaskService_PROTOCOL_HTTP,
		Url: "/proxy/" + vscodeID + "/", MayConnect: true,
	}, listed[1])

	for name, ctx := range map[string]context.Context{
		"owner": ntscUserCtx(t, owner), "admin": adminCtx,
	} {
		resp, err := s.api.GetTaskServiceCredential(ctx, &apiv1.GetTaskServiceCredentialRequest{
			TaskId: taskID, Service: model.ServiceVSCode,
		})
		require.NoError(t, err, name)
		require.Equal(t, &apiv1.GetTaskServiceCredentialResponse{
			Password: ptrs.Ptr(password),
			Url:      ptrs.Ptr("/proxy/" + vscodeID + "/"),
		}, resp, name)
	}
	entries := audits()
	require.Len(t, entries, 1)
	requireCredentialReadLog(t, entries, s.admin, "vscode credential", taskID, int32(owner.ID))

	_, err = s.api.GetTaskServiceCredential(ntscUserCtx(t, other),
		&apiv1.GetTaskServiceCredentialRequest{TaskId: taskID, Service: model.ServiceVSCode})
	require.Equal(t, codes.PermissionDenied, status.Code(err))
	_, err = s.api.GetTaskServiceCredential(ntscUserCtx(t, s.denied),
		&apiv1.GetTaskServiceCredentialRequest{TaskId: taskID, Service: model.ServiceVSCode})
	require.Equal(t, apiPkg.NotFoundErrs("task", taskID, true), err)
	require.Len(t, audits(), 1)
}
