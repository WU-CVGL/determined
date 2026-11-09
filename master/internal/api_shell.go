package internal

import (
	"archive/tar"
	"context"
	cryptorand "crypto/rand"
	"encoding/base64"
	"fmt"
	"math/rand"
	"slices"
	"strconv"
	"time"

	petname "github.com/dustinkirkland/golang-petname"
	"github.com/pkg/errors"
	"golang.org/x/crypto/argon2"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/api/apiutils"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/config"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
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
	"github.com/determined-ai/determined/proto/pkg/shellv1"
)

const (
	shellSSHDConfigFile   = "/run/determined/ssh/sshd_config"
	shellEntrypointScript = "/run/determined/ssh/shell-entrypoint.sh"
	// Agent ports 2600 - 3500 are split between TensorBoards, services, and sshd.
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

	// Hidden descriptions are cleared before the sort, so a hidden row sorts as an empty one.
	if _, err = setShellCapabilities(ctx, *curUser, resp.Shells...); err != nil {
		return nil, err
	}
	api.Sort(resp.Shells, req.OrderBy, req.SortBy, apiv1.GetShellsRequest_SORT_BY_ID)
	if err = api.Paginate(&resp.Pagination, &resp.Shells, req.Offset, req.Limit); err != nil {
		return nil, err
	}
	// A list checks no permission per shell: only the owner and admins see a shell's sources.
	for _, shell := range resp.Shells {
		if !canReadTaskCredential(*curUser, shell.UserId) {
			redactShellSources(shell, nil)
		}
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
	if err = checkShellSources(ctx, *curUser, req.ShellId, resp.Shell, resp.Config); err != nil {
		return nil, err
	}
	caps, err := setShellCapabilities(ctx, *curUser, resp.Shell)
	if err != nil {
		return nil, err
	}
	if !caps[resp.Shell.Id].ViewConfig {
		resp.Config = nil
	}
	return resp, nil
}

// setShellCapabilities sets the user's job privacy capabilities on the shells, clears the
// description of a shell whose title the user may not read and the exit status of one whose logs
// the user may not read, and returns the capabilities by shell ID.
func setShellCapabilities(
	ctx context.Context, curUser model.User, shells ...*shellv1.Shell,
) (map[string]jobprivacy.Caps, error) {
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return nil, err
	}
	byID := make(map[string]jobprivacy.Caps, len(shells))
	return byID, applyJobPrivacy(ctx, v, shells,
		func(s *shellv1.Shell) string { return s.JobId },
		func(s *shellv1.Shell, caps jobprivacy.Caps) {
			if !caps.ViewTitle {
				s.Description = ""
			}
			// The exit status is a free-text failure reason, which follows the logs.
			if !caps.ViewLogs {
				s.ExitStatus = ""
			}
			s.Capabilities = caps.Proto()
			byID[s.Id] = caps
		})
}

// mayReadTensorBoardSources reports whether user may see which experiments and trials a task's
// tensorboard service reads: the task's owner and admins may, and anyone else with the permission
// on every one of them. A refusal is false; any other error is returned.
func mayReadTensorBoardSources(
	ctx context.Context, user model.User, ownerID int32, experimentIDs, trialIDs []int32,
) (bool, error) {
	if canReadTaskCredential(user, ownerID) || len(experimentIDs)+len(trialIDs) == 0 {
		return true, nil
	}
	err := command.AuthZProvider.Get().CanGetTensorBoardSources(ctx, user, experimentIDs, trialIDs)
	switch {
	case err == nil:
		return true, nil
	case authz.IsPermissionDenied(err):
		return false, nil
	default:
		return false, err
	}
}

// checkShellSources removes a shell's tensorboard sources from the shell and its config, if it
// has one, unless user may see them (mayReadTensorBoardSources).
func checkShellSources(
	ctx context.Context, user model.User, shellID string, shell *shellv1.Shell,
	config *structpb.Struct,
) error {
	ok, err := mayReadTensorBoardSources(audit.SupplyEntityID(ctx, shellID), user, shell.UserId,
		shell.ExperimentIds, shell.TrialIds)
	if err != nil {
		return err
	}
	if !ok {
		redactShellSources(shell, config)
	}
	return nil
}

// redactShellSources removes the experiment and trial IDs of a shell's tensorboard service from
// the shell and, when it is not nil, from its config.
func redactShellSources(shell *shellv1.Shell, config *structpb.Struct) {
	shell.ExperimentIds, shell.TrialIds = nil, nil
	services := config.GetFields()["services"].GetStructValue()
	tensorBoard := services.GetFields()[model.ServiceTensorBoard].GetStructValue()
	if tensorBoard != nil {
		delete(tensorBoard.Fields, "experiment_ids")
		delete(tensorBoard.Fields, "trial_ids")
	}
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
	shell := cmd.ToV1Shell()
	if err = checkShellSources(ctx, *curUser, req.ShellId, shell, nil); err != nil {
		return nil, err
	}
	if _, err = setShellCapabilities(ctx, *curUser, shell); err != nil {
		return nil, err
	}
	return &apiv1.KillShellResponse{Shell: shell}, nil
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

	shell := cmd.ToV1Shell()
	if err = checkShellSources(ctx, *curUser, req.ShellId, shell, nil); err != nil {
		return nil, err
	}
	if _, err = setShellCapabilities(ctx, *curUser, shell); err != nil {
		return nil, err
	}
	return &apiv1.SetShellPriorityResponse{Shell: shell}, nil
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

// prepareShellLaunch prepares the launch of a shell, without side effects: the preparation of every
// NTSC launch with the shell's services, the launch authorization, the default services, the
// services and the config, checked as the launch runs them, and last the tensorboard service's
// sources, which it writes into the spec as their canonical IDs and mount (see
// resolveTensorBoardSources). A request that disables ssh runs with resources.slots 0, which it
// may not set to another value. LaunchShell and PreviewShell share it, so that a preview refuses
// what a launch refuses and returns the config that a launch runs, but for what the launch chooses
// afterwards: the description, the entrypoint and the services' ports.
func (a *apiServer) prepareShellLaunch(
	ctx context.Context, req *apiv1.LaunchShellRequest, user *model.User,
) (*command.CreateGeneric, *tensorBoardSources, []pkgCommand.LaunchWarning, error) {
	disablesSSH, err := requestDisablesSSH(req.Config)
	if err != nil {
		return nil, nil, nil, err
	}
	launchReq, launchWarnings, err := a.prepareCommandLaunch(ctx, &protoCommandParams{
		TemplateName:  req.TemplateName,
		WorkspaceID:   req.WorkspaceId,
		Config:        req.Config,
		Files:         req.Files,
		MustZeroSlot:  disablesSSH,
		AllowServices: true,
	}, user)
	if err != nil {
		return nil, nil, nil, api.WrapWithFallbackCode(err, codes.InvalidArgument,
			"failed to prepare launch params")
	}

	if err = a.isNTSCPermittedToLaunch(ctx, launchReq.Spec, user); err != nil {
		return nil, nil, nil, err
	}
	a.addDefaultShellServices(&launchReq.Spec.Config)
	if err = validateShellConfig(launchReq.Spec.Config); err != nil {
		return nil, nil, nil, err
	}
	if err = a.validateShellServices(launchReq.Spec); err != nil {
		return nil, nil, nil, err
	}
	sources, err := a.resolveTensorBoardSources(ctx, user, launchReq.Spec)
	if err != nil {
		return nil, nil, nil, err
	}
	return launchReq, sources, launchWarnings, nil
}

// requestDisablesSSH returns whether a shell's request config sets services.ssh.enabled to false.
// It refuses one that does and sets resources.slots to other than 0.
func requestDisablesSSH(cfg *structpb.Struct) (bool, error) {
	enabled := cfg.GetFields()["services"].GetStructValue().GetFields()[model.ServiceSSH].
		GetStructValue().GetFields()["enabled"]
	if _, ok := enabled.GetKind().(*structpb.Value_BoolValue); !ok || enabled.GetBoolValue() {
		return false, nil
	}
	slots := cfg.GetFields()["resources"].GetStructValue().GetFields()["slots"]
	switch slots.GetKind().(type) {
	case nil, *structpb.Value_NullValue:
	case *structpb.Value_NumberValue:
		if slots.GetNumberValue() != 0 {
			return false, status.Error(codes.InvalidArgument,
				"services: ssh: can be disabled only with resources.slots 0")
		}
	default:
		return false, status.Error(codes.InvalidArgument,
			"services: ssh: can be disabled only with resources.slots 0")
	}
	return true, nil
}

// addDefaultShellServices adds the services that a shell runs by default, jupyter and, when the
// master serves code-server, vscode, to a config that does not mention them. A template's or the
// request's entry for one of them, enabled or not, is kept. A shell whose primary service is
// tensorboard gets none.
func (a *apiServer) addDefaultShellServices(config *model.CommandConfig) {
	if config.TensorBoardPrimary() {
		return
	}
	if config.Services == nil {
		config.Services = map[string]model.ServiceConfig{}
	}
	for _, name := range model.DefaultShellServices(a.m.config.Interactive.CodeServer.Enabled()) {
		if _, ok := config.Services[name]; !ok {
			config.Services[name] = model.ServiceConfig{}
		}
	}
}

// validateShellConfig checks a shell's config as its launch runs it: with the supervisor as its
// entrypoint, which the launch sets, and an idle_timeout of at least minShellIdleTimeout.
func validateShellConfig(config model.CommandConfig) error {
	config.Entrypoint = interactiveSupervisorEntrypoint()
	if err := check.Validate(config); err != nil {
		return status.Error(codes.InvalidArgument, err.Error())
	}
	if t := config.IdleTimeout; t != nil && time.Duration(*t) < minShellIdleTimeout {
		return status.Errorf(codes.InvalidArgument,
			"idle_timeout must be at least %s", minShellIdleTimeout)
	}
	return nil
}

// validateShellServices checks that the master can run a shell's enabled services, the ones that
// run, ssh included unless the config disables it: that it knows each, that it serves code-server
// for vscode, and that each gets a port of its range outside the spec's other ports, as the launch
// assigns them, on a copy of the spec's services.
func (a *apiServer) validateShellServices(spec *tasks.GenericCommandSpec) error {
	scratch := *spec
	scratch.Services = slices.Clone(spec.Services)
	names := model.EnabledServiceNames(spec.Config.Services)
	if model.ShellRunsSSH(spec.Config.Services) {
		names = append([]string{interactiveServiceSSH}, names...)
	}
	for _, name := range names {
		switch name {
		case interactiveServiceSSH, interactiveServiceJupyter, interactiveServiceTensorBoard:
		case interactiveServiceVSCode:
			if !a.m.config.Interactive.CodeServer.Enabled() {
				return status.Error(codes.InvalidArgument,
					"services: vscode: the master serves no code-server")
			}
		default:
			return status.Errorf(codes.InvalidArgument, "services: unknown service %q", name)
		}
		if _, ok := scratch.Service(name); ok {
			continue
		}
		lo, hi := servicePortRange(name)
		port, err := freeServicePort(&scratch, name, lo, hi)
		if err != nil {
			return err
		}
		scratch.AddService(tasks.InteractiveService{Name: name, Port: port})
	}
	return nil
}

// servicePortRange returns the range of the container ports of a shell's service: sshd's own range,
// and the services' range for the others.
func servicePortRange(name string) (lo, hi int) {
	if name == interactiveServiceSSH {
		return minSshdPort, maxSshdPort
	}
	return minServicePort, maxServicePort
}

// PreviewShell returns the config that a shell launched with the request runs, but for what the
// launch chooses (see prepareShellLaunch), and the launch's warnings. It launches nothing: it
// starts no session and creates no task, keys, secrets or ports.
func (a *apiServer) PreviewShell(
	ctx context.Context, req *apiv1.PreviewShellRequest,
) (*apiv1.PreviewShellResponse, error) {
	user, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get the user: %s", err)
	}
	launchReq, _, launchWarnings, err := a.prepareShellLaunch(ctx, &apiv1.LaunchShellRequest{
		Config:       req.Config,
		TemplateName: req.TemplateName,
		Files:        req.Files,
		WorkspaceId:  req.WorkspaceId,
	}, user)
	if err != nil {
		return nil, err
	}
	return &apiv1.PreviewShellResponse{
		Config:   protoutils.ToStruct(launchReq.Spec.Config),
		Warnings: pkgCommand.LaunchWarningToProto(launchWarnings),
	}, nil
}

// LaunchShell launches a shell: the preparation that PreviewShell shares, then, in order, the
// description, sshd's port and the supervisor, the idle watch, the files and environment of each
// service, the SSH keys, the task's session and the launch itself. A shell whose primary service is
// tensorboard runs no sshd and has no SSH keys: its supervisor starts tensorboard first.
func (a *apiServer) LaunchShell(
	ctx context.Context, req *apiv1.LaunchShellRequest,
) (*apiv1.LaunchShellResponse, error) {
	user, session, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get the user: %s", err)
	}

	launchReq, sources, launchWarnings, err := a.prepareShellLaunch(ctx, req, user)
	if err != nil {
		return nil, err
	}
	tensorBoardPrimary := launchReq.Spec.Config.TensorBoardPrimary()

	// Postprocess the launchReq.Spec.
	if launchReq.Spec.Config.Description == "" {
		kind := "Shell"
		if tensorBoardPrimary {
			kind = "TensorBoard"
		}
		launchReq.Spec.Config.Description = fmt.Sprintf(
			"%s (%s)", kind,
			petname.Generate(expconf.TaskNameGeneratorWords, expconf.TaskNameGeneratorSep),
		)
	}

	if tensorBoardPrimary {
		// The primary service, tensorboard, takes no arguments of the supervisor.
		launchReq.Spec.Config.Entrypoint = interactiveSupervisorEntrypoint()
	} else {
		// Selecting a random port mitigates the risk of multiple processes binding
		// the same port on an agent in host mode.
		lo, hi := servicePortRange(interactiveServiceSSH)
		sshdPort, err := freeServicePort(launchReq.Spec, interactiveServiceSSH, lo, hi)
		if err != nil {
			return nil, err
		}
		setShellSSHD(launchReq.Spec, sshdPort)
	}
	// A shell's idle time is watched only when its config sets idle_timeout.
	launchReq.Spec.WatchTaskActivity = true

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
	// The enabled services after the primary one, ssh or tensorboard, in sorted order, which
	// Command.Start passes on to the supervisor from their records. prepareShellLaunch checked
	// them; a disabled service stays in the config and gets no record.
	names := model.EnabledServiceNames(launchReq.Spec.Config.Services)
	if tensorBoardPrimary {
		names = append([]string{interactiveServiceTensorBoard}, slices.DeleteFunc(names,
			func(name string) bool { return name == interactiveServiceTensorBoard })...)
	}
	for _, name := range names {
		files, err := a.addShellService(launchReq.Spec, name, sources)
		if err != nil {
			return nil, err
		}
		launchReq.Spec.AdditionalFiles = append(launchReq.Spec.AdditionalFiles, files...)
	}
	launchReq.Spec.AdditionalFiles = append(launchReq.Spec.AdditionalFiles,
		interactiveSupervisorFiles(launchReq.Spec.Base.AgentUserGroup)...)

	addExtraEnvVars(launchReq.Spec, map[string]string{"DET_TASK_TYPE": string(model.TaskTypeShell)})

	oidcPachydermEnvVars, err := a.getOIDCPachydermEnvVars(session)
	if err != nil {
		return nil, err
	}
	addExtraEnvVars(launchReq.Spec, oidcPachydermEnvVars)

	if !tensorBoardPrimary {
		keys, err := ssh.GenerateKey(launchReq.Spec.Base.SSHConfig)
		if err != nil {
			return nil, status.Error(codes.Internal, err.Error())
		}
		launchReq.Spec.Metadata.PrivateKey = ptrs.Ptr(string(keys.PrivateKey))
		launchReq.Spec.Metadata.PublicKey = ptrs.Ptr(string(keys.PublicKey))
		launchReq.Spec.Keys = &keys
	}
	if err = startTaskSession(ctx, user, launchReq.Spec); err != nil {
		return nil, err
	}

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

// addShellService adds an enabled service other than ssh, which the launch adds itself, to a shell
// spec, and returns its files. The tensorboard service reads sources when they are set.
func (a *apiServer) addShellService(
	spec *tasks.GenericCommandSpec, name string, sources *tensorBoardSources,
) (archive.Archive, error) {
	switch name {
	case interactiveServiceJupyter:
		return addShellJupyter(spec)
	case interactiveServiceVSCode:
		return addShellVSCode(spec, a.m.config.Interactive.CodeServer)
	case interactiveServiceTensorBoard:
		return addShellTensorBoard(spec, sources)
	default:
		return nil, nil
	}
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

// addShellVSCode adds the vscode service to a shell spec: a port from the services' range,
// outside the spec's other ports, and a new password, which the spec keeps in its metadata, as it
// keeps the private key. The container gets the password's hash, the release that conf serves now,
// which the shell keeps, and the port, in its environment, and the run directory, which
// addShellVSCode returns. Without code-server on the master the service is refused.
func addShellVSCode(spec *tasks.GenericCommandSpec, conf config.CodeServerConfig) (archive.Archive, error) {
	if !conf.Enabled() {
		return nil, status.Error(codes.InvalidArgument,
			"services: vscode: the master serves no code-server")
	}
	lo, hi := servicePortRange(interactiveServiceVSCode)
	port, err := freeServicePort(spec, interactiveServiceVSCode, lo, hi)
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
