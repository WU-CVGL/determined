package internal

import (
	"archive/tar"
	"path"
	"slices"

	"golang.org/x/exp/maps"

	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

const (
	// interactiveServiceSSH runs sshd. As the primary service it gets the supervisor's arguments.
	interactiveServiceSSH = model.ServiceSSH
	// interactiveServiceJupyter runs JupyterLab.
	interactiveServiceJupyter = model.ServiceJupyter
	// interactiveServiceVSCode runs code-server.
	interactiveServiceVSCode = model.ServiceVSCode
)

// interactiveServiceUnits maps each service of the supervisor to the static resource of its unit.
// Its names are model.InteractiveServices, which a config's services are checked against.
var interactiveServiceUnits = map[string]string{
	interactiveServiceSSH:     etc.SSHServiceResource,
	interactiveServiceJupyter: etc.JupyterServiceResource,
	interactiveServiceVSCode:  etc.VSCodeServiceResource,
}

// interactiveSupervisorEntrypoint is the entrypoint of a shell task: the supervisor, which passes
// primaryArgs on to its primary service's unit.
func interactiveSupervisorEntrypoint(primaryArgs ...string) []string {
	return append([]string{interactiveSupervisorScript}, primaryArgs...)
}

// addExtraEnvVars adds vars to the task's extra environment variables, keeping the others.
func addExtraEnvVars(spec *tasks.GenericCommandSpec, vars map[string]string) {
	if spec.Base.ExtraEnvVars == nil {
		spec.Base.ExtraEnvVars = map[string]string{}
	}
	maps.Copy(spec.Base.ExtraEnvVars, vars)
}

// interactiveSupervisorFiles returns the run-dir files of the supervisor: the init, the supervisor,
// the reporter of the services' states and the unit of every service.
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
		aug.OwnedArchiveItem(
			serviceStateReporterScript,
			etc.MustStaticFile(etc.ServiceStateReporterResource),
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
