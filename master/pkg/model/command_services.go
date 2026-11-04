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
)

// InteractiveServices lists every service of ServiceSSH, ServiceJupyter, ... in sorted order.
var InteractiveServices = []string{ServiceJupyter, ServiceSSH, ServiceVSCode}

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

// DeclaredServices returns the names in services that run, sorted: a disabled service is not
// declared.
func DeclaredServices(services map[string]ServiceConfig) []string {
	var names []string
	for name, service := range services {
		if service.Runs() {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// configuredServices returns every name in services, disabled ones included, sorted.
func configuredServices(services map[string]ServiceConfig) []string {
	names := maps.Keys(services)
	slices.Sort(names)
	return names
}

// validateServices returns an error for each name in services that is not a known service, one
// for ssh disabled: every shell runs its SSH server, and one for each idle_type that is not on the
// jupyter service or not one of JupyterIdleTypes.
func validateServices(services map[string]ServiceConfig) []error {
	var errs []error
	for _, name := range configuredServices(services) {
		service := services[name]
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
