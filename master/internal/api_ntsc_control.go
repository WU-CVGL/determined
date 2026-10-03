package internal

import (
	"context"

	"github.com/determined-ai/determined/master/internal/api/apiutils"
	"github.com/determined-ai/determined/master/internal/command"
	"github.com/determined-ai/determined/master/pkg/model"
)

// authorizeNSCControl applies the same task-owner rule used by generic tasks
// after the endpoint's operation-specific workspace permission check. Basic and
// permissive authorization require the owner or an admin; RBAC retains its
// workspace UPDATE_NSC permission. Missing owners fail closed for non-admins.
func authorizeNSCControl(
	ctx context.Context, user model.User, workspaceID model.AccessScopeID, ownerUserID int32,
) error {
	var ownerID *model.UserID
	if ownerUserID > 0 {
		id := model.UserID(ownerUserID)
		ownerID = &id
	}
	return apiutils.MapAndFilterErrors(command.AuthZProvider.Get().CanControlGenericTask(
		ctx, user, workspaceID, ownerID,
	), nil, nil)
}

// canReadTaskCredential reports whether a user may receive a credential that acts as a task's
// owner inside the task, such as a shell's SSH private key or a notebook's Jupyter token. Only the
// owner and admins may, in every authz mode; workspace permissions such as RBAC's UPDATE_NSC are
// not enough.
func canReadTaskCredential(user model.User, ownerID int32) bool {
	return user.Admin || ownerID > 0 && user.ID == model.UserID(ownerID)
}
