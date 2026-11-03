package internal

import (
	"archive/tar"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/proxy"
	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/tasks"
)

// The jupyter service of a shell: JupyterLab's files, its port and its launch, and its token. The
// token is a notebook session of the task's owner (model.NotebookSession), which the container gets
// as DET_NOTEBOOK_TOKEN; it logs in to the task's JupyterLab and to its proxied port.

const (
	jupyterDir         = "/run/determined/jupyter/"
	jupyterConfigDir   = "/run/determined/jupyter/config"
	jupyterDataDir     = "/run/determined/jupyter/data"
	jupyterRuntimeDir  = "/run/determined/jupyter/runtime"
	jupyterIdleCheck   = "/run/determined/jupyter/check_idle.py"
	jupyterCertPath    = "/run/determined/jupyter/jupyterCert.pem"
	jupyterKeyPath     = "/run/determined/jupyter/jupyterKey.key"
	jupyterDefaultPage = "/run/determined/workdir/README.ipynb"
	// Agent ports 2600 - 3500 are split between TensorBoards, services, and sshd. The services of a
	// shell other than ssh take their ports from this range.
	minServicePort = 2900
	maxServicePort = minServicePort + 299
)

// jupyterFiles returns the run-dir files of the jupyter service: its directories, the idle check,
// the default page, and the master-signed key and cert that JupyterLab serves HTTPS with.
func jupyterFiles(aug *model.AgentUserGroup, key, cert []byte) archive.Archive {
	return archive.Archive{
		aug.OwnedArchiveItem(jupyterDir, nil, 0o700, tar.TypeDir),
		aug.OwnedArchiveItem(jupyterConfigDir, nil, 0o700, tar.TypeDir),
		aug.OwnedArchiveItem(jupyterDataDir, nil, 0o700, tar.TypeDir),
		aug.OwnedArchiveItem(jupyterRuntimeDir, nil, 0o700, tar.TypeDir),
		aug.OwnedArchiveItem(
			jupyterIdleCheck,
			etc.MustStaticFile(etc.NotebookIdleCheckResource),
			0o700,
			tar.TypeReg,
		),
		aug.OwnedArchiveItem(
			jupyterDefaultPage,
			etc.MustStaticFile(etc.NotebookTemplateResource),
			0o644,
			tar.TypeReg,
		),
		aug.OwnedArchiveItem(jupyterKeyPath, key, 0o600, tar.TypeReg),
		aug.OwnedArchiveItem(jupyterCertPath, cert, 0o600, tar.TypeReg),
	}
}

// jupyterService is the jupyter service of a shell: at "<task>:<port>", reached over HTTPS, since
// JupyterLab serves the master-signed cert, by the task's owner and admins only, since it runs
// code as the owner. JupyterLab serves under its base URL, so the proxy keeps the path.
func jupyterService(port int) tasks.InteractiveService {
	return tasks.InteractiveService{
		Name: interactiveServiceJupyter,
		Port: port,
		TLS:  true,
		Exec: true,
	}
}

// addShellJupyter adds the jupyter service to a shell spec: JupyterLab's port, from the services'
// range, which does not overlap sshd's, outside the spec's other ports, its environment and its
// files, which it returns. The command service adds its token, and Command.Start its base URL.
// When the shell's idle time is watched, JupyterLab's idle check reports to the task's activity
// record, more often than the idle timeout.
func addShellJupyter(spec *tasks.GenericCommandSpec) (archive.Archive, error) {
	key, cert, err := proxy.GenSignedCert()
	if err != nil {
		return nil, err
	}
	configBytes, err := json.Marshal(spec.Config)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "cannot marshal shell config: %s", err)
	}
	lo, hi := servicePortRange(interactiveServiceJupyter)
	port, err := freeServicePort(spec, interactiveServiceJupyter, lo, hi)
	if err != nil {
		return nil, err
	}
	spec.AddService(jupyterService(port))
	env := map[string]string{
		"NOTEBOOK_PORT":   strconv.Itoa(port),
		"NOTEBOOK_CONFIG": string(configBytes),
	}
	if spec.Config.IdleTimeout != nil {
		env[tasks.NotebookIdleTypeEnvVar] = spec.Config.NotebookIdleType
		env[tasks.ActivityReportEnvVar] = tasks.ActivityReportTask
		env[tasks.IdleTimeoutSecondsEnvVar] = strconv.FormatFloat(
			time.Duration(*spec.Config.IdleTimeout).Seconds(), 'f', -1, 64)
	}
	addExtraEnvVars(spec, env)
	return jupyterFiles(spec.Base.AgentUserGroup, key, cert), nil
}

// jupyterServiceAddress returns the address that opens JupyterLab: its proxied service address with
// its token.
func jupyterServiceAddress(serviceAddress, token string) string {
	return fmt.Sprintf("%s?token=%s", serviceAddress, token)
}

// extractJupyterTokenFromRequest returns the jupyter token of a proxied request, from its token query
// parameter or else its Authorization header of type "token", or "" without one.
func extractJupyterTokenFromRequest(r *http.Request) string {
	token := r.URL.Query().Get("token")
	authRaw := r.Header.Get("Authorization")
	if token != "" {
		return token
	} else if authRaw != "" {
		if strings.HasPrefix(authRaw, "token ") {
			return strings.TrimPrefix(authRaw, "token ")
		}
	}
	return ""
}
