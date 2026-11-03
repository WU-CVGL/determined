package command

import (
	"testing"

	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/task"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

func servicesCommand(spec tasks.GenericCommandSpec, taskType model.TaskType) *Command {
	taskID := model.NewTaskID()
	return &Command{
		GenericCommandSpec: spec,
		taskID:             taskID,
		taskType:           taskType,
		allocationID:       model.AllocationID(string(taskID) + ".1"),
	}
}

func shellWithJupyter() tasks.GenericCommandSpec {
	var spec tasks.GenericCommandSpec
	spec.AddService(tasks.InteractiveService{
		Name: model.ServiceSSH, Port: 3200, DefaultServiceID: true, ProxyTCP: true,
		Unauthenticated: true,
	})
	spec.AddService(tasks.InteractiveService{Name: model.ServiceJupyter, Port: 2900, TLS: true, Exec: true})
	return spec
}

func serviceStates(services TaskServices) map[string]taskv1.TaskService_State {
	states := map[string]taskv1.TaskService_State{}
	for _, s := range services.Services {
		states[s.Name] = s.State
	}
	return states
}

// A task's services are its records, else the one service of its type, as its proxies have it.
func TestTaskServices(t *testing.T) {
	spec := shellWithJupyter()
	require.Equal(t, spec.Services, taskServices(spec, model.TaskTypeShell))

	// A shell saved without records: sshd at SSHDPort, else at its default TCP proxy port.
	var shell tasks.GenericCommandSpec
	shell.SSHDPort = 3210
	ssh := tasks.InteractiveService{
		Name: model.ServiceSSH, Port: 3210, DefaultServiceID: true, ProxyTCP: true,
		Unauthenticated: true,
	}
	require.Equal(t, []tasks.InteractiveService{ssh}, taskServices(shell, model.TaskTypeShell))
	shell.SSHDPort = 0
	shell.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{
		RawProxyPort: 3220, RawProxyTCP: ptrs.Ptr(true), RawDefaultServiceID: ptrs.Ptr(true),
	}}
	ssh.Port = 3220
	require.Equal(t, []tasks.InteractiveService{ssh}, taskServices(shell, model.TaskTypeShell))

	// A notebook: JupyterLab at the task ID, over HTTPS.
	var notebook tasks.GenericCommandSpec
	notebook.Base.ExtraProxyPorts = expconf.ProxyPortsConfig{{
		RawProxyPort: 2910, RawDefaultServiceID: ptrs.Ptr(true),
	}}
	require.Equal(t, []tasks.InteractiveService{{
		Name: model.ServiceJupyter, Port: 2910, DefaultServiceID: true, TLS: true,
	}}, taskServices(notebook, model.TaskTypeNotebook))

	// Commands and TensorBoards have none.
	require.Empty(t, taskServices(tasks.GenericCommandSpec{}, model.TaskTypeCommand))
	require.Empty(t, taskServices(tasks.GenericCommandSpec{}, model.TaskTypeTensorboard))
}

// The state of each service is the last one reported for it, unspecified before the first report;
// reports for other services, for another allocation or after the exit change nothing.
func TestCommandServiceStates(t *testing.T) {
	c := servicesCommand(shellWithJupyter(), model.TaskTypeShell)
	require.Equal(t, map[string]taskv1.TaskService_State{
		model.ServiceSSH:     taskv1.TaskService_STATE_UNSPECIFIED,
		model.ServiceJupyter: taskv1.TaskService_STATE_UNSPECIFIED,
	}, serviceStates(c.taskServicesWithState()))

	require.NoError(t, c.setServiceState(c.allocationID, model.ServiceSSH,
		taskv1.TaskService_STATE_STARTING))
	require.NoError(t, c.setServiceState(c.allocationID, model.ServiceSSH,
		taskv1.TaskService_STATE_READY))
	// Reports are idempotent.
	require.NoError(t, c.setServiceState(c.allocationID, model.ServiceSSH,
		taskv1.TaskService_STATE_READY))
	require.NoError(t, c.setServiceState(c.allocationID, model.ServiceJupyter,
		taskv1.TaskService_STATE_UNAVAILABLE))
	services := c.taskServicesWithState()
	require.Equal(t, []string{model.ServiceSSH, model.ServiceJupyter},
		[]string{services.Services[0].Name, services.Services[1].Name})
	require.Equal(t, map[string]taskv1.TaskService_State{
		model.ServiceSSH:     taskv1.TaskService_STATE_READY,
		model.ServiceJupyter: taskv1.TaskService_STATE_UNAVAILABLE,
	}, serviceStates(services))

	err := c.setServiceState(c.allocationID, "vscode", taskv1.TaskService_STATE_READY)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	err = c.setServiceState(model.AllocationID(string(c.taskID)+".2"), model.ServiceSSH,
		taskv1.TaskService_STATE_EXITED)
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(t, taskv1.TaskService_STATE_READY,
		serviceStates(c.taskServicesWithState())[model.ServiceSSH])

	// The exit makes every service EXITED, and later reports do not change that.
	c.mu.Lock()
	c.markServicesExited()
	c.exitStatus = &task.AllocationExited{}
	c.mu.Unlock()
	require.NoError(t, c.setServiceState(c.allocationID, model.ServiceJupyter,
		taskv1.TaskService_STATE_READY))
	require.Equal(t, map[string]taskv1.TaskService_State{
		model.ServiceSSH:     taskv1.TaskService_STATE_EXITED,
		model.ServiceJupyter: taskv1.TaskService_STATE_EXITED,
	}, serviceStates(c.taskServicesWithState()))
}

// A task without records takes reports for the service of its type; one without services takes
// none.
func TestCommandServiceStatesWithoutRecords(t *testing.T) {
	notebook := servicesCommand(tasks.GenericCommandSpec{}, model.TaskTypeNotebook)
	require.NoError(t, notebook.setServiceState(notebook.allocationID, model.ServiceJupyter,
		taskv1.TaskService_STATE_READY))
	require.Equal(t, map[string]taskv1.TaskService_State{
		model.ServiceJupyter: taskv1.TaskService_STATE_READY,
	}, serviceStates(notebook.taskServicesWithState()))
	err := notebook.setServiceState(notebook.allocationID, model.ServiceSSH,
		taskv1.TaskService_STATE_READY)
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	cmd := servicesCommand(tasks.GenericCommandSpec{}, model.TaskTypeCommand)
	err = cmd.setServiceState(cmd.allocationID, model.ServiceSSH, taskv1.TaskService_STATE_READY)
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.Empty(t, cmd.taskServicesWithState().Services)
	cmd.markServicesExited()
	require.Empty(t, cmd.taskServicesWithState().Services)
}

// The credential of each service: the key pair and login user of ssh, the token of jupyter.
func TestCommandServiceCredential(t *testing.T) {
	spec := shellWithJupyter()
	spec.Base.Owner = &model.User{ID: 7}
	spec.Metadata.PrivateKey = ptrs.Ptr("private")
	spec.Metadata.PublicKey = ptrs.Ptr("public")
	spec.Base.ExtraEnvVars = map[string]string{model.NotebookSessionEnvVar: "token"}
	c := servicesCommand(spec, model.TaskTypeShell)

	cred, err := c.serviceCredential(model.ServiceSSH)
	require.NoError(t, err)
	require.Equal(t, TaskServiceCredential{
		Service: spec.Services[0], OwnerID: 7, PrivateKey: "private", PublicKey: "public",
		User: "root",
	}, cred)
	c.Base.AgentUserGroup = &model.AgentUserGroup{User: "alice"}
	cred, err = c.serviceCredential(model.ServiceSSH)
	require.NoError(t, err)
	require.Equal(t, "alice", cred.User)

	cred, err = c.serviceCredential(model.ServiceJupyter)
	require.NoError(t, err)
	require.Equal(t, TaskServiceCredential{Service: spec.Services[1], OwnerID: 7, Token: "token"}, cred)

	_, err = c.serviceCredential("vscode")
	require.Equal(t, codes.NotFound, status.Code(err))
	// A shell without jupyter has no jupyter credential.
	shell := servicesCommand(tasks.GenericCommandSpec{Services: spec.Services[:1]}, model.TaskTypeShell)
	_, err = shell.serviceCredential(model.ServiceJupyter)
	require.Equal(t, codes.NotFound, status.Code(err))
}
