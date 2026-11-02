package internal

import (
	"archive/tar"
	"path"
	"slices"
	"strings"

	"golang.org/x/exp/maps"

	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

const (
	// interactiveServicesEnvVar names the services that the supervisor starts, comma-separated,
	// the primary service first.
	interactiveServicesEnvVar = "DET_SERVICES"
	// interactiveServiceSSH runs sshd. As the primary service it gets the supervisor's arguments.
	interactiveServiceSSH = "ssh"
	// interactiveServiceJupyter runs JupyterLab.
	interactiveServiceJupyter = "jupyter"
)

// interactiveServiceUnits maps each service of the supervisor to the static resource of its unit.
var interactiveServiceUnits = map[string]string{
	interactiveServiceSSH:     etc.SSHServiceResource,
	interactiveServiceJupyter: etc.JupyterServiceResource,
}

// interactiveSupervisorEntrypoint is the entrypoint of a notebook or shell task: the supervisor,
// which passes primaryArgs on to its primary service's unit.
func interactiveSupervisorEntrypoint(primaryArgs ...string) []string {
	return append([]string{interactiveSupervisorScript}, primaryArgs...)
}

// setInteractiveServices tells the supervisor which services to start, the primary one first.
func setInteractiveServices(spec *tasks.GenericCommandSpec, services ...string) {
	addExtraEnvVars(spec, map[string]string{interactiveServicesEnvVar: strings.Join(services, ",")})
}

// addExtraEnvVars adds vars to the task's extra environment variables, keeping the others.
func addExtraEnvVars(spec *tasks.GenericCommandSpec, vars map[string]string) {
	if spec.Base.ExtraEnvVars == nil {
		spec.Base.ExtraEnvVars = map[string]string{}
	}
	maps.Copy(spec.Base.ExtraEnvVars, vars)
}

// interactiveSupervisorFiles returns the run-dir files of the supervisor: the init, the supervisor
// and the unit of every service.
func interactiveSupervisorFiles(aug *model.AgentUserGroup) archive.Archive {
	files := archive.Archive{
		aug.OwnedArchiveItem(
			interactiveInitScript,
			etc.MustStaticFile(etc.InteractiveInitScriptResource),
			0o700,
			tar.TypeReg,
		),
		aug.OwnedArchiveItem(
			interactiveSupervisorScript,
			etc.MustStaticFile(etc.InteractiveSupervisorResource),
			0o700,
			tar.TypeReg,
		),
		aug.OwnedArchiveItem(interactiveServicesDir, nil, 0o700, tar.TypeDir),
	}
	services := maps.Keys(interactiveServiceUnits)
	slices.Sort(services)
	for _, service := range services {
		files = append(files, aug.OwnedArchiveItem(
			path.Join(interactiveServicesDir, service+".sh"),
			etc.MustStaticFile(interactiveServiceUnits[service]),
			0o700,
			tar.TypeReg,
		))
	}
	return files
}
