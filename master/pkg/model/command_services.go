package model

import (
	"fmt"
	"slices"
	"strings"

	"golang.org/x/exp/maps"
)

// The services that the container supervisor of an interactive task can run. The master ships a
// unit for each of them.
const (
	// ServiceSSH runs sshd. It is the primary service of every shell.
	ServiceSSH = "ssh"
	// ServiceJupyter runs JupyterLab.
	ServiceJupyter = "jupyter"
	// ServiceVSCode runs VS Code in the browser, coder/code-server.
	ServiceVSCode = "vscode"
	// ServiceTensorBoard runs TensorBoard on a log directory in the container.
	ServiceTensorBoard = "tensorboard"
)

// InteractiveServices lists every service of ServiceSSH, ServiceJupyter, ... in sorted order.
var InteractiveServices = []string{ServiceJupyter, ServiceSSH, ServiceTensorBoard, ServiceVSCode}

// The idle types of the jupyter service: what JupyterLab's idle check counts as activity.
const (
	// JupyterIdleTypeKernelsOrTerminals counts any open kernel or terminal.
	JupyterIdleTypeKernelsOrTerminals = "kernels_or_terminals"
	// JupyterIdleTypeKernelConnections counts any open connection to a kernel.
	JupyterIdleTypeKernelConnections = "kernel_connections"
	// JupyterIdleTypeActivity counts a kernel that is executing or a terminal that is reading or
	// writing data. It is the default.
	JupyterIdleTypeActivity = "activity"
)

// JupyterIdleTypes lists the idle types of the jupyter service.
var JupyterIdleTypes = []string{
	JupyterIdleTypeKernelsOrTerminals, JupyterIdleTypeKernelConnections, JupyterIdleTypeActivity,
}

// ServiceConfig holds the options of one service in CommandConfig.Services.
type ServiceConfig struct {
	// Enabled is whether the shell runs the service; unset means yes. A disabled service stays in
	// the config, so that the config shows the choice.
	Enabled *bool `json:"enabled,omitempty"`
	// IdleType is what the jupyter service's idle check counts as activity, one of
	// JupyterIdleTypes; unset means JupyterIdleTypeActivity. Only the jupyter service takes it.
	IdleType string `json:"idle_type,omitempty"`
	// LogDir is the directory that the tensorboard service reads, a path in the container; a
	// relative one is resolved against the task's work directory when the service starts. An
	// enabled tensorboard service requires it, and only the tensorboard service takes it.
	LogDir string `json:"logdir,omitempty"`
	// Args are more arguments of TensorBoard, after the ones that the service sets. Only the
	// tensorboard service takes them.
	Args []string `json:"args,omitempty"`
}

// MergeServiceConfig returns a request's entry for a service over a template's: the request's
// enabled wins, unset meaning enabled, so that an explicit enabled: false is kept and an entry
// without it runs the service; each other option that the request leaves unset is the template's.
// An explicit empty args list clears the template's.
func MergeServiceConfig(template, request ServiceConfig) ServiceConfig {
	merged := request
	if merged.IdleType == "" {
		merged.IdleType = template.IdleType
	}
	if merged.LogDir == "" {
		merged.LogDir = template.LogDir
	}
	if merged.Args == nil {
		merged.Args = slices.Clone(template.Args)
	}
	return merged
}

// MergeServices returns the services of a config whose request's entries replace the
// template's: each entry of the request merged over the template's entry for the same service
// with MergeServiceConfig, and the template's other entries as they are. Either may be nil.
func MergeServices(template, request map[string]ServiceConfig) map[string]ServiceConfig {
	if template == nil && request == nil {
		return nil
	}
	merged := maps.Clone(template)
	if merged == nil {
		merged = map[string]ServiceConfig{}
	}
	for name, service := range request {
		merged[name] = MergeServiceConfig(template[name], service)
	}
	return merged
}

// Runs returns whether the service runs: it is enabled unless the config says otherwise.
func (c ServiceConfig) Runs() bool {
	return c.Enabled == nil || *c.Enabled
}

// JupyterIdleType returns the idle type of the jupyter service: the config's, or the default.
func (c ServiceConfig) JupyterIdleType() string {
	if c.IdleType == "" {
		return JupyterIdleTypeActivity
	}
	return c.IdleType
}

// DefaultShellServices returns the services that a shell runs unless its config disables them:
// jupyter, and vscode when the master serves code-server.
func DefaultShellServices(codeServer bool) []string {
	services := []string{ServiceJupyter}
	if codeServer {
		services = append(services, ServiceVSCode)
	}
	return services
}

// EnabledServiceNames returns the names in services that run, sorted: a disabled service is left
// out.
func EnabledServiceNames(services map[string]ServiceConfig) []string {
	var names []string
	for name, service := range services {
		if service.Runs() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// configuredServiceNames returns every name in services, disabled ones included, sorted.
func configuredServiceNames(services map[string]ServiceConfig) []string {
	names := maps.Keys(services)
	slices.Sort(names)
	return names
}

// validateServices returns an error for each name in services that is not a known service, one
// for ssh disabled: every shell runs its SSH server, one for each idle_type that is not on the
// jupyter service or not one of JupyterIdleTypes, one for each logdir or args that is not on the
// tensorboard service, and one for an enabled tensorboard service without a logdir.
func validateServices(services map[string]ServiceConfig) []error {
	var errs []error
	for _, name := range configuredServiceNames(services) {
		service := services[name]
		if slices.Contains(InteractiveServices, name) {
			errs = append(errs, validateTensorBoardOptions(name, service)...)
		}
		switch {
		case !slices.Contains(InteractiveServices, name):
			errs = append(errs, fmt.Errorf("services: unknown service %q; known services: %s",
				name, strings.Join(InteractiveServices, ", ")))
		case name == ServiceSSH && !service.Runs():
			errs = append(errs, fmt.Errorf("services: ssh cannot be disabled"))
		case service.IdleType == "":
		case name != ServiceJupyter:
			errs = append(errs, fmt.Errorf("services: %s: only jupyter takes idle_type", name))
		case !slices.Contains(JupyterIdleTypes, service.IdleType):
			errs = append(errs, fmt.Errorf("services: jupyter: unknown idle_type %q; idle types: %s",
				service.IdleType, strings.Join(JupyterIdleTypes, ", ")))
		}
	}
	return errs
}

// tensorBoardServiceFlags are the flags that the tensorboard service's args may not set: the
// TensorBoard flags that the service sets itself, --bind_all, which would replace its --host, and
// --reuse_port, which would let another process share its port.
var tensorBoardServiceFlags = []string{
	"--logdir", "--port", "--path_prefix", "--host", "--bind_all", "--reuse_port",
}

// tensorBoardFlagFile is TensorBoard's flag that reads more flags from a file, which the args may
// not set either.
const tensorBoardFlagFile = "--flagfile"

// tensorBoardRefusedFlag returns the flag of tensorBoardServiceFlags or tensorBoardFlagFile that
// arg sets, or "". TensorBoard takes "--flag", which may take its value from the next argument, and
// "--flag=value", each also with the flag abbreviated, and "-flagfile".
func tensorBoardRefusedFlag(arg string) string {
	name, _, _ := strings.Cut(arg, "=")
	if name == "-flagfile" {
		return tensorBoardFlagFile
	}
	if !strings.HasPrefix(name, "--") || len(name) == len("--") {
		return ""
	}
	for _, flag := range append(slices.Clone(tensorBoardServiceFlags), tensorBoardFlagFile) {
		if strings.HasPrefix(flag, name) {
			return flag
		}
	}
	return ""
}

// validateTensorBoardOptions returns an error for a logdir or args on a service other than
// tensorboard, and, on the tensorboard service, for a missing logdir when it runs, for a NUL
// character, which the container's environment cannot hold, in the logdir or an argument, and for
// an argument that sets a flag of tensorBoardRefusedFlag. The args are checked whether or not the
// service runs, as the NUL characters are.
func validateTensorBoardOptions(name string, service ServiceConfig) []error {
	if name != ServiceTensorBoard {
		var errs []error
		if service.LogDir != "" {
			errs = append(errs, fmt.Errorf("services: %s: only tensorboard takes logdir", name))
		}
		if service.Args != nil {
			errs = append(errs, fmt.Errorf("services: %s: only tensorboard takes args", name))
		}
		return errs
	}
	var errs []error
	if service.Runs() && service.LogDir == "" {
		errs = append(errs, fmt.Errorf("services: tensorboard: logdir is required"))
	}
	if strings.ContainsRune(service.LogDir, 0) {
		errs = append(errs, fmt.Errorf("services: tensorboard: logdir contains a NUL character"))
	}
	for _, arg := range service.Args {
		if strings.ContainsRune(arg, 0) {
			errs = append(errs, fmt.Errorf("services: tensorboard: args contain a NUL character"))
			break
		}
	}
	for _, arg := range service.Args {
		switch flag := tensorBoardRefusedFlag(arg); flag {
		case "":
		case tensorBoardFlagFile:
			errs = append(errs, fmt.Errorf("services: tensorboard: args: %s is not allowed", flag))
		default:
			errs = append(errs, fmt.Errorf("services: tensorboard: args: %s is set by the service", flag))
		}
	}
	return errs
}
