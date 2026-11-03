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

// ServiceConfig holds the options of one service in CommandConfig.Services. A service has no
// options yet; an object leaves room for them without changing the type of the config.
type ServiceConfig struct{}

// DeclaredServices returns the names in services, sorted.
func DeclaredServices(services map[string]ServiceConfig) []string {
	names := maps.Keys(services)
	slices.Sort(names)
	return names
}

// validateServices returns an error for each name in services that is not a known service.
func validateServices(services map[string]ServiceConfig) []error {
	var errs []error
	for _, name := range DeclaredServices(services) {
		if !slices.Contains(InteractiveServices, name) {
			errs = append(errs, fmt.Errorf("services: unknown service %q; known services: %s",
				name, strings.Join(InteractiveServices, ", ")))
		}
	}
	return errs
}
