package command

import (
	"context"
	"database/sql"

	"github.com/pkg/errors"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/schemas"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// A task's services and their runtime state.
//
// The services of a task are part of its persisted spec (GenericCommandSpec.Services). Their state
// is not: the task's container reports each service's state (ReportTaskServiceState), and the
// command keeps the last report of each in memory, next to the rest of its runtime state. A
// master that restarts starts without states, until the containers report again, which they do
// periodically. When the task's allocation exits, every service is EXITED; the states go away
// with the command when the command service forgets it.

// TaskService is a service of a task with its state.
type TaskService struct {
	tasks.InteractiveService
	State taskv1.TaskService_State
}

// TaskServices are the services of a task, the primary one first.
type TaskServices struct {
	TaskID   model.TaskID
	OwnerID  model.UserID
	Services []TaskService
}

// TaskServiceCredential is the credential of a service: the SSH key pair and login user of the
// ssh service, the token of the jupyter service.
type TaskServiceCredential struct {
	Service    tasks.InteractiveService
	OwnerID    model.UserID
	PrivateKey string
	PublicKey  string
	User       string
	Token      string
}

// taskServices returns the services of a task: its records, or, for a task saved without them,
// the one service that its type runs, as proxyPortConfigs proxies it: a shell's sshd at the task
// ID, a notebook's JupyterLab at the task ID over HTTPS. Commands and TensorBoards have none.
func taskServices(spec tasks.GenericCommandSpec, taskType model.TaskType) []tasks.InteractiveService {
	if len(spec.Services) > 0 {
		return spec.Services
	}
	switch taskType {
	case model.TaskTypeShell:
		port := spec.SSHDPort
		if port == 0 {
			port = shellDefaultTCPPort(spec.Base.ExtraProxyPorts)
		}
		return []tasks.InteractiveService{{
			Name:             model.ServiceSSH,
			Port:             port,
			DefaultServiceID: true,
			ProxyTCP:         true,
			Unauthenticated:  true,
		}}
	case model.TaskTypeNotebook:
		port := 0
		for _, pp := range schemas.WithDefaults(spec.Base.ExtraProxyPorts) {
			if pp.DefaultServiceID() {
				port = pp.ProxyPort()
				break
			}
		}
		return []tasks.InteractiveService{{
			Name:             model.ServiceJupyter,
			Port:             port,
			DefaultServiceID: true,
			TLS:              true,
		}}
	default:
		return nil
	}
}

// findService returns the service name of a task, if the task has it.
func findService(
	spec tasks.GenericCommandSpec, taskType model.TaskType, name string,
) (tasks.InteractiveService, bool) {
	if len(spec.Services) > 0 {
		return spec.Service(name)
	}
	for _, s := range taskServices(spec, taskType) {
		if s.Name == name {
			return s, true
		}
	}
	return tasks.InteractiveService{}, false
}

// setServiceState records the state that the container of the allocation allocationID reported
// for one of the task's services. Once the allocation has exited, every service stays EXITED.
func (c *Command) setServiceState(
	allocationID model.AllocationID, name string, state taskv1.TaskService_State,
) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	if c.allocationID != allocationID {
		return api.NotFoundErrs("allocation", string(allocationID), true)
	}
	if _, ok := findService(c.GenericCommandSpec, c.taskType, name); !ok {
		return status.Errorf(codes.InvalidArgument, "task %s has no service %q", c.taskID, name)
	}
	if c.exitStatus != nil {
		return nil
	}
	if c.serviceStates == nil {
		c.serviceStates = map[string]taskv1.TaskService_State{}
	}
	c.serviceStates[name] = state
	return nil
}

// markServicesExited makes every service of the task EXITED. Callers hold c.mu.
func (c *Command) markServicesExited() {
	services := taskServices(c.GenericCommandSpec, c.taskType)
	if len(services) == 0 {
		return
	}
	c.serviceStates = make(map[string]taskv1.TaskService_State, len(services))
	for _, s := range services {
		c.serviceStates[s.Name] = taskv1.TaskService_STATE_EXITED
	}
}

// taskServicesWithState returns the task's services with their last reported states.
func (c *Command) taskServicesWithState() TaskServices {
	c.mu.Lock()
	defer c.mu.Unlock()

	out := TaskServices{TaskID: c.taskID}
	if c.Base.Owner != nil {
		out.OwnerID = c.Base.Owner.ID
	}
	for _, s := range taskServices(c.GenericCommandSpec, c.taskType) {
		out.Services = append(out.Services, TaskService{InteractiveService: s, State: c.serviceStates[s.Name]})
	}
	return out
}

// serviceCredential returns the credential of one of the task's services.
func (c *Command) serviceCredential(name string) (TaskServiceCredential, error) {
	c.mu.Lock()
	defer c.mu.Unlock()

	notFound := api.NotFoundErrs("service", name, true)
	service, ok := findService(c.GenericCommandSpec, c.taskType, name)
	if !ok {
		return TaskServiceCredential{}, notFound
	}
	cred := TaskServiceCredential{Service: service}
	if c.Base.Owner != nil {
		cred.OwnerID = c.Base.Owner.ID
	}
	switch name {
	case model.ServiceSSH:
		if c.Metadata.PrivateKey == nil || c.Metadata.PublicKey == nil {
			return TaskServiceCredential{}, notFound
		}
		cred.PrivateKey = *c.Metadata.PrivateKey
		cred.PublicKey = *c.Metadata.PublicKey
		// The same rule as `det shell open`.
		cred.User = "root"
		if c.Base.AgentUserGroup != nil && c.Base.AgentUserGroup.User != "" {
			cred.User = c.Base.AgentUserGroup.User
		}
	case model.ServiceJupyter:
		cred.Token = c.Base.ExtraEnvVars[model.NotebookSessionEnvVar]
		if cred.Token == "" {
			return TaskServiceCredential{}, notFound
		}
	default:
		return TaskServiceCredential{}, notFound
	}
	return cred, nil
}

// GetTaskServices returns the services of the NTSC task taskID with their states: from the
// command service while it has the task, else from the task's persisted state, as an ended task
// whose services are all EXITED. Returns db.ErrNotFound if no NTSC task has that ID.
func (cs *CommandService) GetTaskServices(
	ctx context.Context, taskID model.TaskID,
) (TaskServices, error) {
	cs.mu.Lock()
	c, ok := cs.commands[taskID]
	cs.mu.Unlock()
	if ok {
		return c.taskServicesWithState(), nil
	}

	var snapshot CommandSnapshot
	err := db.Bun().NewSelect().Model(&snapshot).Where("task_id = ?", taskID).Scan(ctx)
	if errors.Is(err, sql.ErrNoRows) {
		return TaskServices{}, db.ErrNotFound
	} else if err != nil {
		return TaskServices{}, err
	}
	spec := snapshot.GenericCommandSpec
	out := TaskServices{TaskID: taskID}
	switch {
	case spec.Base.Owner != nil:
		out.OwnerID = spec.Base.Owner.ID
	case snapshot.GenericTaskSpec != nil && snapshot.GenericTaskSpec.Base.Owner != nil:
		out.OwnerID = snapshot.GenericTaskSpec.Base.Owner.ID
	}
	for _, s := range taskServices(spec, spec.TaskType) {
		out.Services = append(out.Services,
			TaskService{InteractiveService: s, State: taskv1.TaskService_STATE_EXITED})
	}
	return out, nil
}

// GetTaskServiceCredential returns the credential of a service of the task taskID, which the
// command service must have, with the task's owner. Callers must hand it only to the owner or an
// admin.
func (cs *CommandService) GetTaskServiceCredential(
	taskID model.TaskID, name string,
) (TaskServiceCredential, error) {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	c, ok := cs.commands[taskID]
	if !ok {
		return TaskServiceCredential{}, api.NotFoundErrs("task", string(taskID), true)
	}
	return c.serviceCredential(name)
}

// SetTaskServiceState records the state of a service of the task of allocation allocationID, as
// the task's container reports it.
func (cs *CommandService) SetTaskServiceState(
	allocationID model.AllocationID, name string, state taskv1.TaskService_State,
) error {
	cs.mu.Lock()
	defer cs.mu.Unlock()

	c, ok := cs.commands[allocationID.ToTaskID()]
	if !ok {
		return api.NotFoundErrs("allocation", string(allocationID), true)
	}
	return c.setServiceState(allocationID, name, state)
}
