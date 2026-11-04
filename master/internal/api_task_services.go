package internal

import (
	"context"
	"errors"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/determined-ai/determined/master/internal/api"
	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/internal/grpcutil"
	"github.com/determined-ai/determined/master/internal/rbac/audit"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/master/pkg/ptrs"
	"github.com/determined-ai/determined/proto/pkg/apiv1"
	"github.com/determined-ai/determined/proto/pkg/taskv1"
)

// canViewTaskServices returns the current user and the task's metadata if the user may see the
// NTSC task taskID, by the rule of GetShell and the proxy: the workspace's NSC view
// permission, or for a TensorBoard the TensorBoard's. Anyone else gets NotFound, as for a task
// that does not exist.
func canViewTaskServices(
	ctx context.Context, taskID string,
) (*model.User, command.TaskMetadata, error) {
	curUser, _, err := grpcutil.GetUser(ctx)
	if err != nil {
		return nil, command.TaskMetadata{}, err
	}
	notFound := api.NotFoundErrs("task", taskID, true)
	spec, err := command.IdentifyTask(ctx, model.TaskID(taskID))
	if errors.Is(err, db.ErrNotFound) {
		return nil, spec, notFound
	} else if err != nil {
		return nil, spec, err
	}

	ctx = audit.SupplyEntityID(ctx, taskID)
	if spec.TaskType == model.TaskTypeTensorboard {
		err = command.AuthZProvider.Get().CanGetTensorboard(
			ctx, *curUser, spec.WorkspaceID, spec.ExperimentIDs, spec.TrialIDs)
	} else {
		err = command.AuthZProvider.Get().CanGetNSC(ctx, *curUser, spec.WorkspaceID)
	}
	if err != nil {
		return nil, spec, authz.SubIfUnauthorized(err, notFound)
	}
	return curUser, spec, nil
}

// serviceURL returns the path under which the master proxies an HTTP service; a TCP service has
// none.
func serviceURL(serviceID string, tcp bool) string {
	if tcp {
		return ""
	}
	return "/proxy/" + serviceID + "/"
}

func (a *apiServer) GetTaskServices(
	ctx context.Context, req *apiv1.GetTaskServicesRequest,
) (*apiv1.GetTaskServicesResponse, error) {
	curUser, spec, err := canViewTaskServices(ctx, req.TaskId)
	if err != nil {
		return nil, err
	}
	services, err := command.DefaultCmdService.GetTaskServices(ctx, model.TaskID(req.TaskId))
	if errors.Is(err, db.ErrNotFound) {
		return nil, api.NotFoundErrs("task", req.TaskId, true)
	} else if err != nil {
		return nil, err
	}

	mayConnect := canReadTaskCredential(*curUser, int32(services.OwnerID))
	resp := &apiv1.GetTaskServicesResponse{
		TaskId:      req.TaskId,
		UserId:      int32(services.OwnerID),
		WorkspaceId: int32(spec.WorkspaceID),
		Services:    []*taskv1.TaskService{},
	}
	for i, s := range services.Services {
		if i == 0 {
			resp.Primary = s.Name
		}
		serviceID := s.ServiceID(services.TaskID)
		protocol := taskv1.TaskService_PROTOCOL_HTTP
		if s.ProxyTCP {
			protocol = taskv1.TaskService_PROTOCOL_TCP
		}
		resp.Services = append(resp.Services, &taskv1.TaskService{
			Name:       s.Name,
			State:      s.State,
			ServiceId:  serviceID,
			Protocol:   protocol,
			Url:        serviceURL(serviceID, s.ProxyTCP),
			MayConnect: mayConnect,
		})
	}
	return resp, nil
}

func (a *apiServer) GetTaskServiceCredential(
	ctx context.Context, req *apiv1.GetTaskServiceCredentialRequest,
) (*apiv1.GetTaskServiceCredentialResponse, error) {
	curUser, _, err := canViewTaskServices(ctx, req.TaskId)
	if err != nil {
		return nil, err
	}
	cred, err := command.DefaultCmdService.GetTaskServiceCredential(
		model.TaskID(req.TaskId), req.Service)
	if err != nil {
		return nil, err
	}
	// A credential acts as the task's owner inside the task, as a shell's key and its jupyter
	// token do.
	ownerID := int32(cred.OwnerID)
	if !canReadTaskCredential(*curUser, ownerID) {
		return nil, status.Error(codes.PermissionDenied,
			"only the task's owner or an admin may read its credentials")
	}

	resp := &apiv1.GetTaskServiceCredentialResponse{}
	switch req.Service {
	case model.ServiceSSH:
		resp.PrivateKey = ptrs.Ptr(cred.PrivateKey)
		resp.PublicKey = ptrs.Ptr(cred.PublicKey)
		resp.User = ptrs.Ptr(cred.User)
	case model.ServiceJupyter:
		resp.Token = ptrs.Ptr(cred.Token)
		resp.Url = ptrs.Ptr(jupyterServiceAddress(
			serviceURL(cred.Service.ServiceID(model.TaskID(req.TaskId)), false), cred.Token))
	case model.ServiceVSCode:
		// code-server's login form is at <url>login.
		resp.Password = ptrs.Ptr(cred.Password)
		resp.Url = ptrs.Ptr(serviceURL(cred.Service.ServiceID(model.TaskID(req.TaskId)), false))
	}
	logCredentialRead(*curUser, req.Service+" credential", req.TaskId, ownerID)
	return resp, nil
}

func (a *apiServer) ReportTaskServices(
	ctx context.Context, req *apiv1.ReportTaskServicesRequest,
) (*apiv1.ReportTaskServicesResponse, error) {
	if err := a.canControlAllocation(ctx, req.AllocationId); err != nil {
		return nil, err
	}
	snapshot := make([]command.ServiceState, 0, len(req.Services))
	for _, s := range req.Services {
		snapshot = append(snapshot, command.ServiceState{Name: s.GetName(), State: s.GetState()})
	}
	if err := command.DefaultCmdService.SetTaskServiceStates(
		model.AllocationID(req.AllocationId), req.Sequence, snapshot,
	); err != nil {
		return nil, err
	}
	return &apiv1.ReportTaskServicesResponse{}, nil
}
