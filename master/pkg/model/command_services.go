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

// ServiceConfig holds the options of one service in CommandConfig.Services.
type ServiceConfig struct {
	// Enabled is whether the shell runs the service; unset means yes. A disabled service stays in
	// the config, so that the config shows the choice.
	Enabled *bool `json:"enabled,omitempty"`
}

// Runs returns whether the service runs: it is enabled unless the config says otherwise.
func (c ServiceConfig) Runs() bool {
	return c.Enabled == nil || *c.Enabled
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

// validateServices returns an error for each name in services that is not a known service, and
// one for ssh disabled: every shell runs its SSH server.
func validateServices(services map[string]ServiceConfig) []error {
	var errs []error
	for _, name := range configuredServiceNames(services) {
		if !slices.Contains(InteractiveServices, name) {
			errs = append(errs, fmt.Errorf("services: unknown service %q; known services: %s",
				name, strings.Join(InteractiveServices, ", ")))
		} else if name == ServiceSSH && !services[name].Runs() {
			errs = append(errs, fmt.Errorf("services: ssh cannot be disabled"))
		}
	}
	return errs
}
