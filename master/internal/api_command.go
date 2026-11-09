package internal

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand"
	"strconv"

	petname "github.com/dustinkirkland/golang-petname"
	pstruct "github.com/golang/protobuf/ptypes/struct"
	"github.com/pkg/errors"

	"golang.org/x/exp/maps"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/api/apiutils"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/configpolicy"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/jobprivacy"
	"github.com/determined-ai/determined/master/internal/poolaccess"
	"github.com/determined-ai/determined/master/internal/rbac/audit"
	"github.com/determined-ai/determined/master/internal/rm"
	"github.com/determined-ai/determined/master/internal/templates"
	"github.com/determined-ai/determined/master/internal/user"
	"github.com/determined-ai/determined/master/pkg/archive"
	"github.com/determined-ai/determined/master/pkg/check"
	pkgCommand "github.com/determined-ai/determined/master/pkg/command"
	"github.com/determined-ai/determined/master/pkg/etc"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/protoutils"
	"github.com/determined-ai/determined/master/pkg/schemas/expconf"
	"github.com/determined-ai/determined/master/pkg/tasks"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/commandv1"
	"github.com/determined-ai/determined/proto/pkg/jobv1"
	"github.com/determined-ai/determined/proto/pkg/rbacv1"
	"github.com/determined-ai/determined/proto/pkg/utilv1"
	"github.com/determined-ai/determined/proto/pkg/workspacev1"
)

const (
	commandEntrypoint = "/run/determined/command-entrypoint.sh"
)

func getRandomPort(min, max int) int {
	//nolint:gosec // Weak RNG doesn't matter here.
	return rand.Intn(max-min) + min
}

type protoCommandParams struct {
	TemplateName string
	WorkspaceID  int32
	Config       *pstruct.Struct
	Files        []*utilv1.File
	MustZeroSlot bool
	// AllowServices accepts the config's services, which only shells run.
	AllowServices bool
}

// notebookIdleTypeKey is the config key that services.jupyter.idle_type replaces. A request config
// or a template that sets it is refused with a message that names the new key.
const notebookIdleTypeKey = "notebook_idle_type"

// refuseNotebookIdleType refuses a config, as its top-level keys, that sets notebook_idle_type.
func refuseNotebookIdleType[V any](config map[string]V, source string) error {
	if _, ok := config[notebookIdleTypeKey]; !ok {
		return nil
	}
	return status.Errorf(codes.InvalidArgument, "%s%s: use services.jupyter.idle_type",
		source, notebookIdleTypeKey)
}

// prepareCommandLaunch prepares the launch of an NTSC task from a request, without side effects:
// the merged config of the defaults, the template and the request, the resolved pool, the base task
// spec and the context directory, checked against pool access, the GPU topology and the config
// policies. It starts no task session; the launch does, just before it starts the task (see
// startTaskSession).
func (a *apiServer) prepareCommandLaunch(ctx context.Context, req *protoCommandParams,
	aUser *model.User) (
	*command.CreateGeneric, []pkgCommand.LaunchWarning, error,
) {
	var err error
	cmdSpec := tasks.GenericCommandSpec{}

	cmdSpec.Metadata.WorkspaceID = model.DefaultWorkspaceID
	if req.WorkspaceID != 0 {
		cmdSpec.Metadata.WorkspaceID = model.AccessScopeID(req.WorkspaceID)
	}

	// Validate the userModel and get the agent userModel group.
	userModel, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil,
			nil,
			status.Errorf(codes.Unauthenticated, "failed to get the user: %s", err)
	}

	// TODO(ilia): When commands are workspaced, also use workspace AgentUserGroup here.
	agentUserGroup, err := user.GetAgentUserGroup(ctx, userModel.ID, int(cmdSpec.Metadata.WorkspaceID))
	if err != nil {
		return nil, nil, err
	}

	if err := refuseNotebookIdleType(req.Config.GetFields(), ""); err != nil {
		return nil, nil, err
	}
	var configBytes []byte
	if req.Config != nil {
		configBytes, err = protojson.Marshal(req.Config)
		if err != nil {
			return nil, nil, status.Errorf(
				codes.InvalidArgument, "failed to parse config %s: %s", configBytes, err)
		}
	}

	// Validate the resource configuration.
	resources := model.ParseJustResources(configBytes)
	if req.MustZeroSlot {
		resources.Slots = 0
	}

	poolName, launchWarnings, err := a.m.ResolveResources(
		resources.ResourcePool,
		resources.Slots,
		int(cmdSpec.Metadata.WorkspaceID),
		true,
	)
	if err != nil {
		return nil, launchWarnings, err
	}

	// Get the base TaskSpec.
	taskSpec, err := a.m.fillTaskSpec(poolName, agentUserGroup, userModel)
	if err != nil {
		return nil, nil, err
	}

	// Get the full configuration.
	config := model.DefaultConfig(&taskSpec.TaskContainerDefaults)
	if req.TemplateName != "" {
		// The template is read as its keys first, since its decode ignores unknown keys.
		var template map[string]any
		err := templates.UnmarshalTemplateConfig(ctx, req.TemplateName, aUser, &template, false)
		if err != nil {
			return nil, launchWarnings, err
		}
		if err := refuseNotebookIdleType(template,
			fmt.Sprintf("template %s: ", req.TemplateName)); err != nil {
			return nil, launchWarnings, err
		}
		templateBytes, err := json.Marshal(template)
		if err != nil {
			return nil, launchWarnings, err
		}
		if err := json.Unmarshal(templateBytes, &config); err != nil {
			return nil, launchWarnings, fmt.Errorf("template %s: %w", req.TemplateName, err)
		}
	}
	w, err := a.GetWorkspaceByID(ctx, int32(cmdSpec.Metadata.WorkspaceID), *userModel, false)
	if err != nil {
		return nil, launchWarnings, err
	}
	taskSpec.Workspace = w.Name

	workDirInDefaults := config.WorkDir
	if len(configBytes) != 0 {
		// Decoding replaces a service's entry as a whole, so the request's entries are merged
		// over the template's afterwards: a request's entry keeps the template's options that it
		// does not set.
		templateServices := maps.Clone(config.Services)
		dec := json.NewDecoder(bytes.NewBuffer(configBytes))
		dec.DisallowUnknownFields()

		if err := dec.Decode(&config); err != nil {
			return nil, launchWarnings, status.Errorf(codes.InvalidArgument,
				errors.Wrapf(err,
					"unable to decode the merged config: %s", string(configBytes)).Error())
		}
		// A request's "services: null" clears the template's, as a null clears any setting.
		if config.Services != nil {
			config.Services = model.MergeServices(templateServices, config.Services)
		}
	}
	if len(config.Services) > 0 && !req.AllowServices {
		return nil, launchWarnings, status.Error(codes.InvalidArgument,
			"services: only shells run services")
	}
	// Copy discovered (default) resource pool name and slot count.
	fillTaskConfig(resources.Slots, taskSpec, &config.Environment)
	config.Resources.ResourcePool = poolName.String()
	config.Resources.Slots = resources.Slots

	// The pool is final here: a template or the request cannot change it after the line above.
	if err := poolaccess.CanUseResourcePool(ctx, *userModel, poolName.String()); err != nil {
		return nil, launchWarnings, err
	}
	if err := validateGPUTopology(a.m.rm, poolName, resources.Slots, slotsField,
		config.Resources.GPUTopology(), config.Resources.GPUGroupRequests()); err != nil {
		return nil, launchWarnings, err
	}

	// Apply the scheduler's default priority.
	if config.Resources.Priority == nil {
		prio := rm.DefaultPriorityForPool(a.m.rm, poolName.String())
		config.Resources.Priority = &prio
	}
	// TODO (CM-493) NTSC invariant config overrides

	var contextDirectory []byte
	config.WorkDir, contextDirectory, err = fillContextDir(config.WorkDir, workDirInDefaults, req.Files)
	if err != nil {
		return nil, nil, err
	}

	// Check submitted config against task config policies.
	err = configpolicy.CheckNTSCConstraints(ctx, int(cmdSpec.Metadata.WorkspaceID), config, a.m.rm)
	if err != nil {
		return nil, nil, status.Errorf(codes.InvalidArgument, "failed constraint check: %v", err)
	}

	cmdSpec.Base = taskSpec
	cmdSpec.Config = config

	return &command.CreateGeneric{
		Spec:             &cmdSpec,
		ContextDirectory: contextDirectory,
	}, launchWarnings, nil
}

// startTaskSession starts the session of the request's user that a launched task acts with, and
// adds its token to the task's spec. A launch calls it after its last check, just before it starts
// the task, so that a refused launch or a preview starts none.
func startTaskSession(ctx context.Context, user *model.User, spec *tasks.GenericCommandSpec) error {
	token, err := getTaskSessionToken(ctx, user)
	if err != nil {
		return err
	}
	spec.Base.UserSessionToken = token
	return nil
}

// isNTSCPermittedToLaunch checks authorization to launch in a given
// workspace.
func (a *apiServer) isNTSCPermittedToLaunch(
	ctx context.Context, spec *tasks.GenericCommandSpec, user *model.User,
) error {
	workspaceID := spec.Metadata.WorkspaceID
	if workspaceID == 0 {
		return status.Errorf(codes.InvalidArgument, "workspace_id is required")
	}

	w := &workspacev1.Workspace{}
	notFoundErr := api.NotFoundErrs("workspace", strconv.Itoa(int(workspaceID)), true)
	if err := a.m.db.QueryProto(
		"get_workspace", w, workspaceID, user.ID,
	); errors.Is(err, db.ErrNotFound) {
		return notFoundErr
	} else if err != nil {
		return errors.Wrapf(err, "error fetching workspace (%d) from database", workspaceID)
	}
	if w.Archived {
		return notFoundErr
	}

	if spec.TaskType == model.TaskTypeTensorboard {
		if err := command.AuthZProvider.Get().CanGetTensorboard(
			ctx, *user, workspaceID, spec.Metadata.ExperimentIDs, spec.Metadata.TrialIDs,
		); err != nil {
			var pdErr authz.PermissionDeniedError
			if errors.As(err, &pdErr) {
				for _, perm := range pdErr.RequiredPermissions {
					if perm == rbacv1.PermissionType_PERMISSION_TYPE_VIEW_WORKSPACE {
						return apiutils.ErrNotFound
					}
				}
			}

			return apiutils.MapAndFilterErrors(err, nil, nil)
		}
	} else {
		if err := command.AuthZProvider.Get().CanCreateNSC(
			ctx, *user, workspaceID); err != nil {
			return apiutils.MapAndFilterErrors(err, nil, nil)
		}
	}

	return nil
}

func (a *apiServer) GetCommands(
	ctx context.Context, req *apiv1.GetCommandsRequest,
) (resp *apiv1.GetCommandsResponse, err error) {
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
		_, err := a.GetWorkspaceByID(ctx, req.WorkspaceId, *curUser, false)
		if errors.Is(err, db.ErrNotFound) {
			return nil, workspaceNotFoundErr
		} else if err != nil {
			return nil, err
		}
	}
	resp, err = command.DefaultCmdService.GetCommands(req)
	if err != nil {
		return nil, err
	}

	limitedScopes, err := command.AuthZProvider.Get().AccessibleScopes(
		ctx, *curUser, model.AccessScopeID(req.WorkspaceId),
	)
	if err != nil {
		return nil, err
	}
	if req.WorkspaceId != 0 && len(limitedScopes) == 0 {
		return nil, workspaceNotFoundErr
	}

	api.Where(&resp.Commands, func(i int) bool {
		return limitedScopes[model.AccessScopeID(resp.Commands[i].WorkspaceId)]
	})

	if err = setCommandCapabilities(ctx, *curUser, resp.Commands...); err != nil {
		return nil, err
	}
	api.Sort(resp.Commands, req.OrderBy, req.SortBy, apiv1.GetCommandsRequest_SORT_BY_ID)
	if err = api.Paginate(&resp.Pagination, &resp.Commands, req.Offset, req.Limit); err != nil {
		return nil, err
	}
	starts := make([]allocationStartTarget, 0, len(resp.Commands))
	for _, item := range resp.Commands {
		starts = append(starts, allocationStartTarget{item.Id, &item.AllocationStartTime})
	}
	if err = a.enrichAllocationStartTimes(ctx, starts, allocationStartTask); err != nil {
		return nil, err
	}
	return resp, nil
}

func (a *apiServer) GetCommand(
	ctx context.Context, req *apiv1.GetCommandRequest,
) (*apiv1.GetCommandResponse, error) {
	resp, curUser, err := a.getCommand(ctx, req.CommandId)
	if err != nil {
		return nil, err
	}
	redactTaskConfig(*curUser, resp.Command.UserId, req.CommandId, resp.Config)
	if err = setCommandCapabilities(ctx, *curUser, resp.Command); err != nil {
		return nil, err
	}
	return resp, nil
}

// setCommandCapabilities sets the user's job privacy capabilities on the commands.
func setCommandCapabilities(
	ctx context.Context, curUser model.User, commands ...*commandv1.Command,
) error {
	v, err := jobprivacy.NewViewer(ctx, curUser)
	if err != nil {
		return err
	}
	return setJobCapabilities(ctx, v, commands,
		func(c *commandv1.Command) string { return c.JobId },
		func(c *commandv1.Command, caps *jobv1.JobCapabilities) { c.Capabilities = caps })
}

// getCommand returns a command, with its full config, if the current user may see it.
func (a *apiServer) getCommand(
	ctx context.Context, commandID string,
) (*apiv1.GetCommandResponse, *model.User, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, nil, err
	}

	resp, err := command.DefaultCmdService.GetCommand(
		&apiv1.GetCommandRequest{CommandId: commandID})
	if err != nil {
		return nil, nil, err
	}

	ctx = audit.SupplyEntityID(ctx, commandID)
	if err := command.AuthZProvider.Get().CanGetNSC(
		ctx, *curUser, model.AccessScopeID(resp.Command.WorkspaceId)); err != nil {
		return nil, nil, authz.SubIfUnauthorized(err, api.NotFoundErrs("command", commandID, true))
	}
	return resp, curUser, nil
}

func (a *apiServer) KillCommand(
	ctx context.Context, req *apiv1.KillCommandRequest,
) (resp *apiv1.KillCommandResponse, err error) {
	defer func() {
		if status.Code(err) == codes.Unknown {
			err = apiutils.MapAndFilterErrors(err, nil, nil)
		}
	}()

	targetCmd, curUser, err := a.getCommand(ctx, req.CommandId)
	if err != nil {
		return nil, err
	}

	ctx = audit.SupplyEntityID(ctx, req.CommandId)
	if err = command.AuthZProvider.Get().CanTerminateNSC(
		ctx, *curUser, model.AccessScopeID(targetCmd.Command.WorkspaceId),
	); err != nil {
		return nil, err
	}
	if err = authorizeNSCControl(ctx, *curUser,
		model.AccessScopeID(targetCmd.Command.WorkspaceId), targetCmd.Command.UserId); err != nil {
		return nil, err
	}

	cmd, err := command.DefaultCmdService.KillNTSC(req.CommandId, model.TaskTypeCommand)
	if err != nil {
		return nil, err
	}

	return &apiv1.KillCommandResponse{Command: cmd.ToV1Command()}, nil
}

func (a *apiServer) SetCommandPriority(
	ctx context.Context, req *apiv1.SetCommandPriorityRequest,
) (resp *apiv1.SetCommandPriorityResponse, err error) {
	defer func() {
		if status.Code(err) == codes.Unknown {
			err = apiutils.MapAndFilterErrors(err, nil, nil)
		}
	}()
	targetCmd, curUser, err := a.getCommand(ctx, req.CommandId)
	if err != nil {
		return nil, err
	}

	ctx = audit.SupplyEntityID(ctx, req.CommandId)
	if err = command.AuthZProvider.Get().CanSetNSCsPriority(
		ctx, *curUser, model.AccessScopeID(targetCmd.Command.WorkspaceId), int(req.Priority),
	); err != nil {
		return nil, err
	}
	if err = authorizeNSCControl(ctx, *curUser,
		model.AccessScopeID(targetCmd.Command.WorkspaceId), targetCmd.Command.UserId); err != nil {
		return nil, err
	}

	cmd, err := command.DefaultCmdService.SetNTSCPriority(req.CommandId, int(req.Priority), model.TaskTypeCommand)
	if err != nil {
		return nil, err
	}

	return &apiv1.SetCommandPriorityResponse{Command: cmd.ToV1Command()}, nil
}

func (a *apiServer) getOIDCPachydermEnvVars(
	session *model.UserSession,
) (map[string]string, error) {
	if session == nil { // Can happen with allocation token.
		return map[string]string{}, nil
	}

	envVars := make(map[string]string)

	if val, ok := session.InheritedClaims["OIDCRawIDToken"]; ok {
		envVars["DEX_TOKEN"] = val
	}

	if a.m.config.Integrations.Pachyderm.Address != "" {
		envVars["PACHD_ADDRESS"] = a.m.config.Integrations.Pachyderm.Address
	}
	return envVars, nil
}

func (a *apiServer) LaunchCommand(
	ctx context.Context, req *apiv1.LaunchCommandRequest,
) (*apiv1.LaunchCommandResponse, error) {
	user, session, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "failed to get the user: %s", err)
	}

	launchReq, launchWarnings, err := a.prepareCommandLaunch(ctx, &protoCommandParams{
		TemplateName: req.TemplateName,
		WorkspaceID:  req.WorkspaceId,
		Config:       req.Config,
		Files:        req.Files,
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
			"Command (%s)",
			petname.Generate(expconf.TaskNameGeneratorWords, expconf.TaskNameGeneratorSep),
		)
	}

	launchReq.Spec.Config.Entrypoint = append(
		[]string{commandEntrypoint}, launchReq.Spec.Config.Entrypoint...,
	)
	launchReq.Spec.AdditionalFiles = archive.Archive{
		launchReq.Spec.Base.AgentUserGroup.OwnedArchiveItem(
			commandEntrypoint,
			etc.MustStaticFile(etc.CommandEntrypointResource),
			0o700,
			tar.TypeReg,
		),
	}

	if err = check.Validate(launchReq.Spec.Config); err != nil {
		return nil, status.Errorf(
			codes.InvalidArgument,
			"invalid command config: %s",
			err.Error(),
		)
	}

	launchReq.Spec.Base.ExtraEnvVars = map[string]string{
		"DET_TASK_TYPE": string(model.TaskTypeCommand),
	}

	oidcPachydermEnvVars, err := a.getOIDCPachydermEnvVars(session)
	if err != nil {
		return nil, err
	}
	maps.Copy(launchReq.Spec.Base.ExtraEnvVars, oidcPachydermEnvVars)
	if err = startTaskSession(ctx, user, launchReq.Spec); err != nil {
		return nil, err
	}

	// Launch a command.
	cmd, err := command.DefaultCmdService.LaunchGenericCommand(
		model.TaskTypeCommand,
		model.JobTypeCommand,
		launchReq)
	if err != nil {
		return nil, err
	}

	return &apiv1.LaunchCommandResponse{
		Command:  cmd.ToV1Command(),
		Config:   protoutils.ToStruct(launchReq.Spec.Config),
		Warnings: pkgCommand.LaunchWarningToProto(launchWarnings),
	}, nil
}
