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

// snapshot is a snapshot of services' states, "name", state, "name", state, ...
func snapshot(nameStates ...any) []ServiceState {
	var out []ServiceState
	for i := 0; i < len(nameStates); i += 2 {
		out = append(out, ServiceState{
			Name: nameStates[i].(string), State: nameStates[i+1].(taskv1.TaskService_State),
		})
	}
	return out
}

// The states of the services are those of the last accepted snapshot, unspecified before the
// first; each snapshot replaces the states as a whole, and one with a lower sequence, an invalid
// one, one for another allocation or one after the exit changes nothing.
func TestCommandServiceStates(t *testing.T) {
	const (
		ssh         = model.ServiceSSH
		jupyter     = model.ServiceJupyter
		unspecified = taskv1.TaskService_STATE_UNSPECIFIED
		starting    = taskv1.TaskService_STATE_STARTING
		ready       = taskv1.TaskService_STATE_READY
		exited      = taskv1.TaskService_STATE_EXITED
		unavailable = taskv1.TaskService_STATE_UNAVAILABLE
	)
	c := servicesCommand(shellWithJupyter(), model.TaskTypeShell)
	states := func() map[string]taskv1.TaskService_State {
		return serviceStates(c.taskServicesWithState())
	}
	require.Equal(t, map[string]taskv1.TaskService_State{ssh: unspecified, jupyter: unspecified},
		states())

	require.NoError(t, c.setServiceStates(c.allocationID, 1, snapshot(ssh, starting, jupyter, starting)))
	require.NoError(t, c.setServiceStates(c.allocationID, 2, snapshot(jupyter, unavailable, ssh, ready)))
	want := map[string]taskv1.TaskService_State{ssh: ready, jupyter: unavailable}
	require.Equal(t, want, states())
	services := c.taskServicesWithState()
	require.Equal(t, []string{ssh, jupyter},
		[]string{services.Services[0].Name, services.Services[1].Name})

	// A re-post of the current snapshot is accepted; a lower sequence changes nothing.
	require.NoError(t, c.setServiceStates(c.allocationID, 2, snapshot(ssh, ready, jupyter, unavailable)))
	require.NoError(t, c.setServiceStates(c.allocationID, 1, snapshot(ssh, starting, jupyter, starting)))
	require.Equal(t, want, states())

	// Invalid snapshots: an unknown, missing or repeated service, no state, an unknown state, a
	// negative sequence.
	for name, invalid := range map[string]struct {
		sequence int32
		snapshot []ServiceState
	}{
		"unknown":     {3, snapshot(ssh, exited, jupyter, exited, "vscode", ready)},
		"missing":     {3, snapshot(ssh, exited)},
		"empty":       {3, nil},
		"twice":       {3, snapshot(ssh, exited, jupyter, exited, ssh, exited)},
		"unspecified": {3, snapshot(ssh, exited, jupyter, unspecified)},
		"unknown state": {
			3, snapshot(ssh, exited, jupyter, taskv1.TaskService_State(42)),
		},
		"negative": {-1, snapshot(ssh, exited, jupyter, exited)},
	} {
		err := c.setServiceStates(c.allocationID, invalid.sequence, invalid.snapshot)
		require.Equal(t, codes.InvalidArgument, status.Code(err), name)
	}
	err := c.setServiceStates(model.AllocationID(string(c.taskID)+".2"), 3,
		snapshot(ssh, exited, jupyter, exited))
	require.Equal(t, codes.NotFound, status.Code(err))
	require.Equal(t, want, states())

	// The exit makes every service EXITED, and later snapshots do not change that.
	c.mu.Lock()
	c.markServicesExited()
	c.exitStatus = &task.AllocationExited{}
	c.mu.Unlock()
	require.NoError(t, c.setServiceStates(c.allocationID, 4, snapshot(ssh, ready, jupyter, ready)))
	require.Equal(t, map[string]taskv1.TaskService_State{ssh: exited, jupyter: exited}, states())
}

// A master that restarted, whose command starts without states and at sequence zero, takes the
// container's next snapshot whatever its sequence.
func TestCommandServiceStatesAfterRestart(t *testing.T) {
	c := servicesCommand(shellWithJupyter(), model.TaskTypeShell)
	require.NoError(t, c.setServiceStates(c.allocationID, 7, snapshot(
		model.ServiceSSH, taskv1.TaskService_STATE_READY,
		model.ServiceJupyter, taskv1.TaskService_STATE_READY)))
	require.Equal(t, map[string]taskv1.TaskService_State{
		model.ServiceSSH:     taskv1.TaskService_STATE_READY,
		model.ServiceJupyter: taskv1.TaskService_STATE_READY,
	}, serviceStates(c.taskServicesWithState()))
}

// A task without records takes snapshots of the service of its type; one without services takes
// only the empty snapshot.
func TestCommandServiceStatesWithoutRecords(t *testing.T) {
	notebook := servicesCommand(tasks.GenericCommandSpec{}, model.TaskTypeNotebook)
	require.NoError(t, notebook.setServiceStates(notebook.allocationID, 1,
		snapshot(model.ServiceJupyter, taskv1.TaskService_STATE_READY)))
	require.Equal(t, map[string]taskv1.TaskService_State{
		model.ServiceJupyter: taskv1.TaskService_STATE_READY,
	}, serviceStates(notebook.taskServicesWithState()))
	err := notebook.setServiceStates(notebook.allocationID, 2, snapshot(
		model.ServiceJupyter, taskv1.TaskService_STATE_READY,
		model.ServiceSSH, taskv1.TaskService_STATE_READY))
	require.Equal(t, codes.InvalidArgument, status.Code(err))

	cmd := servicesCommand(tasks.GenericCommandSpec{}, model.TaskTypeCommand)
	err = cmd.setServiceStates(cmd.allocationID, 1,
		snapshot(model.ServiceSSH, taskv1.TaskService_STATE_READY))
	require.Equal(t, codes.InvalidArgument, status.Code(err))
	require.NoError(t, cmd.setServiceStates(cmd.allocationID, 1, nil))
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
