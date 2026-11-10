package command

import (
	"context"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/pkg/model"
)

// NSCAuthZ describes authz methods for Shells and Commands.
type NSCAuthZ interface {
	// NSC functions
	// GET /api/v1/NSCs/:nsc_id
	// GET /tasks
	CanGetNSC(
		ctx context.Context, curUser model.User, workspaceID model.AccessScopeID,
	) error

	// GET /api/v1/tasks/count
	CanGetActiveTasksCount(ctx context.Context, curUser model.User) error

	// POST /api/v1/NSCs/:nsc_id/kill
	CanTerminateNSC(
		ctx context.Context, curUser model.User, workspaceID model.AccessScopeID,
	) error

	// POST /api/v1/NSCs
	CanCreateNSC(
		ctx context.Context, curUser model.User, workspaceID model.AccessScopeID,
	) error

	// PATCH /NSCs/:nsc_id
	CanSetNSCsPriority(
		ctx context.Context, curUser model.User, workspaceID model.AccessScopeID, priority int,
	) error

	// GET /api/v1/NSCs
	AccessibleScopes(
		ctx context.Context, curUser model.User, requestedScope model.AccessScopeID,
	) (model.AccessScopeSet, error)

	// CanGetTensorBoardSources returns nil if the user may read the TensorBoard logs of every
	// experiment in experimentIDs and every trial in trialIDs. It refuses empty lists and IDs that
	// name nothing.
	CanGetTensorBoardSources(
		ctx context.Context, curUser model.User, experimentIDs, trialIDs []int32,
	) error

	CanCreateGenericTask(
		ctx context.Context, curUser model.User, workspaceID model.AccessScopeID,
	) error

	// POST /api/v1/tasks/:task_id/{kill,pause,unpause}
	CanControlGenericTask(
		ctx context.Context, curUser model.User, workspaceID model.AccessScopeID,
		ownerID *model.UserID,
	) error
}

// AuthZProvider is the authz registry for Shells and Commands.
var AuthZProvider authz.AuthZProviderType[NSCAuthZ]
