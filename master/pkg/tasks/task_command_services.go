package tasks

import (
	"strconv"
	"strings"

	"github.com/determined-ai/determined/master/pkg/model"
)

const (
	// ServicesEnvVar names the services that the container supervisor starts, comma-separated,
	// the primary service first.
	ServicesEnvVar = "DET_SERVICES"
	// NotebookBaseURLEnvVar is the path under which JupyterLab serves: /proxy/<service ID>/.
	NotebookBaseURLEnvVar = "NOTEBOOK_BASE_URL"
	// NotebookIdleTypeEnvVar is the notebook_idle_type of JupyterLab's idle check. The jupyter
	// service starts the check when it is set.
	NotebookIdleTypeEnvVar = "NOTEBOOK_IDLE_TYPE"
	// ActivityReportEnvVar set to ActivityReportTask makes JupyterLab's idle check report to its
	// task's activity record instead of the notebook API.
	ActivityReportEnvVar = "DET_ACTIVITY_REPORT"
	// ActivityReportTask is the value of ActivityReportEnvVar for the task's activity record.
	ActivityReportTask = "task"
	// IdleTimeoutSecondsEnvVar is the task's idle timeout in seconds, at a fraction of which
	// JupyterLab's idle check reports to its task's activity record.
	IdleTimeoutSecondsEnvVar = "DET_IDLE_TIMEOUT_SECONDS"
)

// InteractiveService is one service of an interactive task, as GenericCommandSpec.Services
// records it: the port the service listens on in the container and how the master proxies that
// port. The records are part of the persisted spec, so a restored task gets the same proxies.
type InteractiveService struct {
	// Name is the service's name, one of model.InteractiveServices.
	Name string `json:"name"`
	// Port is the container port of the service.
	Port int `json:"port"`
	// DefaultServiceID makes the task ID the proxy service ID of the port; otherwise it is
	// "<task ID>:<port>".
	DefaultServiceID bool `json:"default_service_id"`
	// ProxyTCP proxies the port as TCP instead of HTTP.
	ProxyTCP bool `json:"proxy_tcp"`
	// Unauthenticated lets the proxy pass requests without a master login.
	Unauthenticated bool `json:"unauthenticated"`
	// TLS makes the master reach the port over HTTPS.
	TLS bool `json:"tls"`
	// Exec restricts the proxied port to the task's owner and admins.
	Exec bool `json:"exec"`
	// StripPrefix makes the proxy remove /proxy/<service ID> from request paths.
	StripPrefix bool `json:"strip_prefix"`
}

// ServiceID returns the proxy service ID of the service's port in the task taskID.
func (s InteractiveService) ServiceID(taskID model.TaskID) string {
	if s.DefaultServiceID {
		return string(taskID)
	}
	return string(taskID) + ":" + strconv.Itoa(s.Port)
}

// AddService records a service of the task. The first service added is the primary one. The
// service's port is exposed and proxied from its record, so it is not one of the extra proxy
// ports.
func (s *GenericCommandSpec) AddService(service InteractiveService) {
	s.Services = append(s.Services, service)
}

// Service returns the recorded service name, if the task has it.
func (s *GenericCommandSpec) Service(name string) (InteractiveService, bool) {
	for _, service := range s.Services {
		if service.Name == name {
			return service, true
		}
	}
	return InteractiveService{}, false
}

// SetServicesEnv sets the environment that the services take from their records: the services
// that the supervisor starts and JupyterLab's base URL. It only reads the persisted spec, so a
// restore sets the same values. A spec without records keeps its environment.
func (s *GenericCommandSpec) SetServicesEnv(taskID model.TaskID) {
	if len(s.Services) == 0 {
		return
	}
	if s.Base.ExtraEnvVars == nil {
		s.Base.ExtraEnvVars = map[string]string{}
	}
	names := make([]string, 0, len(s.Services))
	for _, service := range s.Services {
		names = append(names, service.Name)
	}
	s.Base.ExtraEnvVars[ServicesEnvVar] = strings.Join(names, ",")
	if jupyter, ok := s.Service(model.ServiceJupyter); ok {
		s.Base.ExtraEnvVars[NotebookBaseURLEnvVar] = "/proxy/" + jupyter.ServiceID(taskID) + "/"
	}
}
