package internal

import (
	"archive/tar"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"
	"time"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/pkg/errors"
	"golang.org/x/crypto/argon2"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/api/apiutils"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/proxy"
	"github.com/determined-ai/determined/master/internal/rbac/audit"
	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/check"
	pkgCommand "github.com/determined-ai/determined/master/pkg/command"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/protoutils"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/ssh"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
)

const (
	shellSSHDConfigFile   = "/run/determined/ssh/sshd_config"
	shellEntrypointScript = "/run/determined/ssh/shell-entrypoint.sh"
	// Agent ports 2600 - 3500 are split between TensorBoards, Notebooks, and Shells.
	minSshdPort = 3200
	maxSshdPort = minSshdPort + 299
	// minShellIdleTimeout is the shortest idle_timeout a shell takes: JupyterLab's idle check
	// reports every quarter of the timeout and gives each request as long in all, 2.5 s at this
	// minimum, so that a JupyterLab that answers within 2 s still reaches the task's record more
	// often than the timeout. It is also longer than the idle watcher's tick.
	minShellIdleTimeout = 10 * time.Second
)

func (a *apiServer) GetShells(
	ctx context.Context, req *apiv1.GetShellsRequest,
) (resp *apiv1.GetShellsResponse, err error) {
	defer func() {
		if status.Code(err) == codes.Unknown {
			err = apiutils.MapAndFilterErrors(err, nil, nil)
		}
	}()

	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, err
	}

	workspaceNotFoundErr := api.NotFoundErrs("workspace", strconv.Itoa(int(req.WorkspaceId)), true)

	if req.WorkspaceId != 0 {
		// check if the workspace exists.
		_, err = a.GetWorkspaceByID(ctx, req.WorkspaceId, *curUser, false)
		if errors.Is(err, db.ErrNotFound) {
			return nil, workspaceNotFoundErr
		} else if err != nil {
			return nil, err
		}
	}

	resp, err = command.DefaultCmdService.GetShells(req)
	if err != nil {
		return nil, err
	}

	limitedScopes, err := command.AuthZProvider.Get().AccessibleScopes(
		ctx, *curUser, model.AccessScopeID(req.WorkspaceId),
	)
	if err != nil {
		return nil, apiutils.MapAndFilterErrors(err, nil, nil)
	}

	if req.WorkspaceId != 0 && len(limitedScopes) == 0 {
		return nil, workspaceNotFoundErr
	}

	api.Where(&resp.Shells, func(i int) bool {
		return limitedScopes[model.AccessScopeID(resp.Shells[i].WorkspaceId)]
	})

	if err != nil {
		return nil, err
	}

	api.Sort(resp.Shells, req.OrderBy, req.SortBy, apiv1.GetShellsRequest_SORT_BY_ID)
	if err = api.Paginate(&resp.Pagination, &resp.Shells, req.Offset, req.Limit); err != nil {
		return nil, err
	}
	starts := make([]allocationStartTarget, 0, len(resp.Shells))
	for _, item := range resp.Shells {
		starts = append(starts, allocationStartTarget{item.Id, &item.AllocationStartTime})
	}
	if err = a.enrichAllocationStartTimes(ctx, starts, allocationStartTask); err != nil {
		return nil, err
	}
	return resp, nil
}

func (a *apiServer) GetShell(
	ctx context.Context, req *apiv1.GetShellRequest,
) (*apiv1.GetShellResponse, error) {
	resp, curUser, err := a.getShell(ctx, req.ShellId)
	if err != nil {
		return nil, err
	}

	// The key logs in to the shell's sshd as the shell's user.
	if canReadTaskCredential(*curUser, resp.Shell.UserId) {
		resp.Shell.PrivateKey, err = command.DefaultCmdService.GetShellPrivateKey(req.ShellId)
		if err != nil {
			return nil, err
		}
		logCredentialRead(*curUser, "shell private key", req.ShellId, resp.Shell.UserId)
	}
	redactTaskConfig(*curUser, resp.Shell.UserId, req.ShellId, resp.Config)
	return resp, nil
}

// getShell returns a shell, without its private key but with its full config, if the current user
// may see it.
func (a *apiServer) getShell(
	ctx context.Context, shellID string,
) (*apiv1.GetShellResponse, *model.User, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, nil, err
	}
	resp, err := command.DefaultCmdService.GetShell(&apiv1.GetShellRequest{ShellId: shellID})
	if err != nil {
		return nil, nil, err
	}

	ctx = audit.SupplyEntityID(ctx, shellID)
	if err := command.AuthZProvider.Get().CanGetNSC(
		ctx, *curUser, model.AccessScopeID(resp.Shell.WorkspaceId)); err != nil {
		return nil, nil, authz.SubIfUnauthorized(err, api.NotFoundErrs("shell", shellID, true))
	}
	return resp, curUser, nil
}

func (a *apiServer) KillShell(
	ctx context.Context, req *apiv1.KillShellRequest,
) (resp *apiv1.KillShellResponse, err error) {
	defer func() {
		if status.Code(err) == codes.Unknown {
			err = apiutils.MapAndFilterErrors(err, nil, nil)
		}
	}()

	getResponse, curUser, err := a.getShell(ctx, req.ShellId)
	if err != nil {
		return nil, err
	}

	ctx = audit.SupplyEntityID(ctx, req.ShellId)
	err = command.AuthZProvider.Get().CanTerminateNSC(
		ctx, *curUser, model.AccessScopeID(getResponse.Shell.WorkspaceId))
	if err != nil {
		return nil, err
	}
	if err = authorizeNSCControl(ctx, *curUser,
		model.AccessScopeID(getResponse.Shell.WorkspaceId), getResponse.Shell.UserId); err != nil {
		return nil, err
	}

	cmd, err := command.DefaultCmdService.KillNTSC(req.ShellId, model.TaskTypeShell)
	if err != nil {
		return nil, err
	}
	return &apiv1.KillShellResponse{Shell: cmd.ToV1Shell()}, nil
}

func (a *apiServer) SetShellPriority(
	ctx context.Context, req *apiv1.SetShellPriorityRequest,
) (resp *apiv1.SetShellPriorityResponse, err error) {
	defer func() {
		if status.Code(err) == codes.Unknown {
			err = apiutils.MapAndFilterErrors(err, nil, nil)
		}
	}()

	getResponse, curUser, err := a.getShell(ctx, req.ShellId)
	if err != nil {
		return nil, err
	}

	ctx = audit.SupplyEntityID(ctx, req.ShellId)
	err = command.AuthZProvider.Get().CanSetNSCsPriority(
		ctx, *curUser, model.AccessScopeID(getResponse.Shell.WorkspaceId), int(req.Priority))
	if err != nil {
		return nil, err
	}
	if err = authorizeNSCControl(ctx, *curUser,
		model.AccessScopeID(getResponse.Shell.WorkspaceId), getResponse.Shell.UserId); err != nil {
		return nil, err
	}

	cmd, err := command.DefaultCmdService.SetNTSCPriority(req.ShellId, int(req.Priority), model.TaskTypeShell)
	if err != nil {
		return nil, err
	}

	return &apiv1.SetShellPriorityResponse{Shell: cmd.ToV1Shell()}, nil
}

// sshService is the ssh service of a shell, at sshd's port: the shell's primary service and its
// default TCP proxy port, which the proxy passes without a master login, since sshd checks the
// shell's key.
func sshService(port int) tasks.InteractiveService {
	return tasks.InteractiveService{
		Name:             interactiveServiceSSH,
		Port:             port,
		DefaultServiceID: true,
		ProxyTCP:         true,
		Unauthenticated:  true,
	}
}

// freeServicePort returns a random port of [min, max] for the named service of a spec, other than
// the ports of its proxy ports, the config's and the launch's, and of the services it already has,
// so that each container port has one proxy.
func freeServicePort(spec *tasks.GenericCommandSpec, name string, min, max int) (int, error) {
	used := map[int]bool{}
	for _, pp := range spec.ProxyPorts() {
		used[pp.ProxyPort()] = true
	}
	for _, service := range spec.Services {
		used[service.Port] = true
	}
	var free []int
	for port := min; port <= max; port++ {
		if !used[port] {
			free = append(free, port)
		}
	}
	if len(free) == 0 {
		return 0, status.Errorf(codes.InvalidArgument,
			"services: %s: no free port in %d-%d", name, min, max)
	}
	//nolint:gosec // Weak RNG doesn't matter here.
	return free[rand.Intn(len(free))], nil
}

// setShellSSHD makes a shell spec run its sshd on the given container port: the entrypoint starts
// sshd there, the port is the shell's ssh service and default TCP proxy port, and SSHDPort records
// it.
func setShellSSHD(spec *tasks.GenericCommandSpec, port int) {
	spec.AddService(sshService(port))
	spec.SSHDPort = port
	// The supervisor hands its arguments to its primary service, sshd.
	spec.Config.Entrypoint = interactiveSupervisorEntrypoint(
		"-f", shellSSHDConfigFile, "-p", strconv.Itoa(port), "-D", "-e",
	)
}

func (a *apiServer) LaunchShell(
	ctx context.Context, req *apiv1.LaunchShellRequest,
) (*apiv1.LaunchShellResponse, error) {
	user, session, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get the user: %s", err)
	}

	launchReq, launchWarnings, err := a.getCommandLaunchParams(ctx, &protoCommandParams{
		TemplateName:  req.TemplateName,
		WorkspaceID:   req.WorkspaceId,
		Config:        req.Config,
		Files:         req.Files,
		AllowServices: true,
		// A shell's JupyterLab is active while a kernel executes, unless its config says otherwise.
		NotebookIdleType: model.NotebookIdleTypeActivity,
	}, user)
	if err != nil {
		return nil, api.WrapWithFallbackCode(err, codes.InvalidArgument,
			"failed to prepare launch params")
	}

	if err = a.isNTSCPermittedToLaunch(ctx, launchReq.Spec, user); err != nil {
		return nil, err
	}

	// Postprocess the launchReq.Spec.
	if launchReq.Spec.Config.Description == "" {
		launchReq.Spec.Config.Description = fmt.Sprintf(
			"Shell (%s)",
			petname.Generate(expconf.TaskNameGeneratorWords, expconf.TaskNameGeneratorSep),
		)
	}

	// Selecting a random port mitigates the risk of multiple processes binding
	// the same port on an agent in host mode.
	sshdPort, err := freeServicePort(launchReq.Spec, interactiveServiceSSH, minSshdPort, maxSshdPort)
	if err != nil {
		return nil, err
	}
	setShellSSHD(launchReq.Spec, sshdPort)
	// A shell's idle time is watched only when its config sets idle_timeout.
	launchReq.Spec.WatchTaskActivity = true

	if err = check.Validate(launchReq.Spec.Config); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	if t := launchReq.Spec.Config.IdleTimeout; t != nil && time.Duration(*t) < minShellIdleTimeout {
		return nil, status.Errorf(codes.InvalidArgument,
			"idle_timeout must be at least %s", minShellIdleTimeout)
	}

	launchReq.Spec.AdditionalFiles = archive.Archive{
		// A wrapper that runs the supervisor with this task's service, for whatever names it.
		launchReq.Spec.Base.AgentUserGroup.OwnedArchiveItem(
			shellEntrypointScript,
			etc.MustStaticFile(etc.ShellEntrypointResource),
			0o700,
			tar.TypeReg,
		),
		launchReq.Spec.Base.AgentUserGroup.OwnedArchiveItem(
			taskReadyCheckLogs,
			etc.MustStaticFile(etc.TaskCheckReadyLogsResource),
			0o700,
			tar.TypeReg,
		),
	}
	// The declared services after ssh, the primary one, in sorted order, which Command.Start
	// passes on to the supervisor from their records.
	for _, name := range model.DeclaredServices(launchReq.Spec.Config.Services) {
		switch name {
		case interactiveServiceSSH:
			// Every shell runs it.
		case interactiveServiceJupyter:
			files, err := addShellJupyter(launchReq.Spec)
			if err != nil {
				return nil, err
			}
			launchReq.Spec.AdditionalFiles = append(launchReq.Spec.AdditionalFiles, files...)
		case interactiveServiceVSCode:
			files, err := addShellVSCode(launchReq.Spec, a.m.config.Interactive.CodeServer)
			if err != nil {
				return nil, err
			}
			launchReq.Spec.AdditionalFiles = append(launchReq.Spec.AdditionalFiles, files...)
		default:
			return nil, status.Errorf(codes.InvalidArgument, "services: unknown service %q", name)
		}
	}
	launchReq.Spec.AdditionalFiles = append(launchReq.Spec.AdditionalFiles,
		interactiveSupervisorFiles(launchReq.Spec.Base.AgentUserGroup)...)

	addExtraEnvVars(launchReq.Spec, map[string]string{"DET_TASK_TYPE": string(model.TaskTypeShell)})

	oidcPachydermEnvVars, err := a.getOIDCPachydermEnvVars(session)
	if err != nil {
		return nil, err
	}
	addExtraEnvVars(launchReq.Spec, oidcPachydermEnvVars)

	keys, err := ssh.GenerateKey(launchReq.Spec.Base.SSHConfig)
	if err != nil {
		return nil, status.Error(codes.Internal, err.Error())
	}
	launchReq.Spec.Metadata.PrivateKey = ptrs.Ptr(string(keys.PrivateKey))
	launchReq.Spec.Metadata.PublicKey = ptrs.Ptr(string(keys.PublicKey))
	launchReq.Spec.Keys = &keys

	// Launch a Shell.
	cmd, err := command.DefaultCmdService.LaunchShellCommand(launchReq, user)
	if err != nil {
		return nil, err
	}

	// The caller owns the new shell and needs its key to connect (`det shell start`).
	shell := cmd.ToV1Shell()
	shell.PrivateKey = cmd.ShellPrivateKey()

	return &apiv1.LaunchShellResponse{
		Shell:    shell,
		Config:   protoutils.ToStruct(launchReq.Spec.Config),
		Warnings: pkgCommand.LaunchWarningToProto(launchWarnings),
	}, nil
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

// addShellJupyter adds the jupyter service to a shell spec: JupyterLab's port, from the notebooks'
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
	port, err := freeServicePort(spec, interactiveServiceJupyter, minNotebookPort, maxNotebookPort)
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

const (
	// codeServerRunDir holds the files of a shell's code-server run: its config file and its
	// session socket.
	codeServerRunDir = "/run/determined/code-server"
	// codeServerVersionEnvVar is the code-server release that a shell's vscode service runs: the
	// one that the master served when it created the shell, which the shell keeps.
	codeServerVersionEnvVar = "DET_CODE_SERVER_VERSION"
	// codeServerPortEnvVar is the container port of a shell's code-server.
	codeServerPortEnvVar = "DET_CODE_SERVER_PORT"
	// codeServerHashEnvVar is the hash of the password of a shell's code-server, which code-server
	// reads and removes from its environment.
	codeServerHashEnvVar = "HASHED_PASSWORD"
	// codeServerPasswordBytes is the entropy of a code-server password.
	codeServerPasswordBytes = 32
)

// The argon2id parameters of a code-server password's hash: OWASP's minimum setting (19 MiB, t=2,
// p=1). A password of codeServerPasswordBytes random bytes needs no harder hash, and code-server
// checks the hash on every login.
const (
	codeServerArgon2Time    = 2
	codeServerArgon2Memory  = 19 * 1024
	codeServerArgon2Threads = 1
	codeServerArgon2KeyLen  = 32
	codeServerArgon2SaltLen = 16
)

// vscodeService is the vscode service of a shell: at "<task>:<port>", reached over plain HTTP,
// since code-server serves no TLS in the container, by the task's owner and admins only, since it
// runs code as the owner. The proxy strips its prefix: code-server's routes see the stripped path
// and its pages link relative to it.
func vscodeService(port int) tasks.InteractiveService {
	return tasks.InteractiveService{
		Name:        interactiveServiceVSCode,
		Port:        port,
		Exec:        true,
		StripPrefix: true,
	}
}

// codeServerPasswordHash returns the argon2id hash of password, with a random salt, as a PHC
// string, "$argon2id$v=19$m=...,t=...,p=...$<salt>$<hash>", which code-server takes as its
// HASHED_PASSWORD.
func codeServerPasswordHash(password string) (string, error) {
	salt := make([]byte, codeServerArgon2SaltLen)
	if _, err := cryptorand.Read(salt); err != nil {
		return "", err
	}
	key := argon2.IDKey([]byte(password), salt,
		codeServerArgon2Time, codeServerArgon2Memory, codeServerArgon2Threads, codeServerArgon2KeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version,
		codeServerArgon2Memory, codeServerArgon2Time, codeServerArgon2Threads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// addShellVSCode adds the vscode service to a shell spec: a port from the notebooks' range,
// outside the spec's other ports, and a new password, which the spec keeps in its metadata, as it
// keeps the private key. The container gets the password's hash, the release that conf serves now,
// which the shell keeps, and the port, in its environment, and the run directory, which
// addShellVSCode returns. Without code-server on the master the service is refused.
func addShellVSCode(spec *tasks.GenericCommandSpec, conf config.CodeServerConfig) (archive.Archive, error) {
	if !conf.Enabled() {
		return nil, status.Error(codes.InvalidArgument,
			"services: vscode: the master serves no code-server")
	}
	port, err := freeServicePort(spec, interactiveServiceVSCode, minNotebookPort, maxNotebookPort)
	if err != nil {
		return nil, err
	}
	secret := make([]byte, codeServerPasswordBytes)
	if _, err := cryptorand.Read(secret); err != nil {
		return nil, status.Errorf(codes.Internal, "generating the code-server password: %s", err)
	}
	password := base64.RawURLEncoding.EncodeToString(secret)
	hash, err := codeServerPasswordHash(password)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "hashing the code-server password: %s", err)
	}
	spec.AddService(vscodeService(port))
	spec.Metadata.CodeServerPassword = &password
	addExtraEnvVars(spec, map[string]string{
		codeServerVersionEnvVar: conf.Version,
		codeServerPortEnvVar:    strconv.Itoa(port),
		codeServerHashEnvVar:    hash,
	})
	return archive.Archive{
		spec.Base.AgentUserGroup.OwnedArchiveItem(codeServerRunDir, nil, 0o700, tar.TypeDir),
	}, nil
}
