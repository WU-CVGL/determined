//go:build integration
// +build integration

package command

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"testing"

	"github.com/google/uuid"
	"github.com/stretchr/testify/require"

	"github.com/determined-ai/determined/master/internal/authz"
	"github.com/determined-ai/determined/master/internal/db"
	"github.com/determined-ai/determined/master/pkg/model"
	"github.com/determined-ai/determined/proto/pkg/rbacv1"
)

// assignRoleForTest gives u the role named roleName in the workspace, or in every workspace when
// workspaceID is nil, through a group of u's own.
func assignRoleForTest(t *testing.T, u model.User, roleName string, workspaceID *int) {
	t.Helper()
	ctx := context.Background()
	var roleID int
	require.NoError(t, db.Bun().NewRaw("SELECT id FROM roles WHERE role_name = ?", roleName).
		Scan(ctx, &roleID))
	// A scope is unique per workspace.
	scope := &model.RoleAssignmentScope{}
	query := db.Bun().NewSelect().Model(scope)
	if workspaceID != nil {
		scope.WorkspaceID = sql.NullInt32{Int32: int32(*workspaceID), Valid: true}
		query = query.Where("scope_workspace_id = ?", *workspaceID)
	} else {
		query = query.Where("scope_workspace_id IS NULL")
	}
	if err := query.Scan(ctx); errors.Is(err, sql.ErrNoRows) {
		_, err = db.Bun().NewInsert().Model(scope).Returning("id").Exec(ctx)
		require.NoError(t, err)
	} else {
		require.NoError(t, err)
	}
	group := &model.Group{Name: "tb-sources-" + uuid.NewString()}
	_, err := db.Bun().NewInsert().Model(group).Returning("id").Exec(ctx)
	require.NoError(t, err)
	_, err = db.Bun().NewInsert().Model(&map[string]any{
		"group_id": group.ID, "role_id": roleID, "scope_id": scope.ID,
	}).Table("role_assignments").Exec(ctx)
	require.NoError(t, err)
	_, err = db.Bun().NewInsert().Model(&map[string]any{
		"user_id": u.ID, "group_id": group.ID,
	}).Table("user_group_membership").Exec(ctx)
	require.NoError(t, err)
}

// Under RBAC, a viewer reads the TensorBoard logs of experiments and trials only with
// VIEW_EXPERIMENT_ARTIFACTS in the workspace of every one of them. Empty sources, and sources that
// name nothing, such as a deleted experiment, are refused.
func TestCanGetTensorBoardSourcesRBAC(t *testing.T) {
	pgDB := setupTest(t)
	ctx := context.Background()
	impl := &NSCAuthZRBAC{}

	owner := db.RequireMockUser(t, pgDB)
	workspace := func() (int, int) {
		wsID, _ := db.RequireMockWorkspaceID(t, pgDB, "")
		projectID, _ := db.RequireMockProjectID(t, pgDB, wsID, false)
		return wsID, projectID
	}
	ws1, project1 := workspace()
	ws2, project2 := workspace()
	exp1 := db.RequireMockExperimentProject(t, pgDB, owner, project1)
	exp2 := db.RequireMockExperimentProject(t, pgDB, owner, project2)
	trial1 := int32(db.RequireMockTrialID(t, pgDB, exp1))
	trial2 := int32(db.RequireMockTrialID(t, pgDB, exp2))
	deleted := db.RequireMockExperimentProject(t, pgDB, owner, project1)
	_, err := db.Bun().NewDelete().Table("experiments").Where("id = ?", deleted.ID).Exec(ctx)
	require.NoError(t, err)

	viewer1 := db.RequireMockUser(t, pgDB)
	assignRoleForTest(t, viewer1, "Viewer", &ws1)
	viewerBoth := db.RequireMockUser(t, pgDB)
	assignRoleForTest(t, viewerBoth, "Viewer", &ws1)
	assignRoleForTest(t, viewerBoth, "Viewer", &ws2)
	creator := db.RequireMockUser(t, pgDB)
	assignRoleForTest(t, creator, "WorkspaceCreator", &ws1)
	clusterViewer := db.RequireMockUser(t, pgDB)
	assignRoleForTest(t, clusterViewer, "Viewer", nil)
	stranger := db.RequireMockUser(t, pgDB)

	exps := func(es ...*model.Experiment) []int32 {
		var ids []int32
		for _, e := range es {
			ids = append(ids, int32(e.ID))
		}
		return ids
	}
	for _, tc := range []struct {
		name        string
		user        model.User
		exps        []int32
		trials      []int32
		allowed     bool
		artifactsOf bool
	}{
		{name: "empty", user: clusterViewer},
		{name: "deleted experiment", user: clusterViewer, exps: exps(exp1, deleted)},
		{name: "unknown experiment", user: clusterViewer, exps: []int32{math.MaxInt32}},
		{name: "unknown trial", user: clusterViewer, trials: []int32{trial1, math.MaxInt32}},
		{name: "viewer, own workspace", user: viewer1, exps: exps(exp1), trials: []int32{trial1}, allowed: true},
		{name: "viewer, other workspace's experiment", user: viewer1, exps: exps(exp1, exp2), artifactsOf: true},
		{name: "viewer, other workspace's trial", user: viewer1, trials: []int32{trial1, trial2}, artifactsOf: true},
		{name: "viewer of both", user: viewerBoth, exps: exps(exp2), trials: []int32{trial1}, allowed: true},
		{name: "role without the permission", user: creator, exps: exps(exp1), artifactsOf: true},
		{name: "no role", user: stranger, trials: []int32{trial1}, artifactsOf: true},
		{name: "cluster-wide viewer", user: clusterViewer, exps: exps(exp1, exp2), allowed: true},
	} {
		err := impl.CanGetTensorBoardSources(ctx, tc.user, tc.exps, tc.trials)
		if tc.allowed {
			require.NoError(t, err, tc.name)
			continue
		}
		require.True(t, authz.IsPermissionDenied(err), "%s: %v", tc.name, err)
		if tc.artifactsOf {
			require.Equal(t, authz.PermissionDeniedError{
				RequiredPermissions: []rbacv1.PermissionType{
					rbacv1.PermissionType_PERMISSION_TYPE_VIEW_EXPERIMENT_ARTIFACTS,
				},
			}, err, tc.name)
		}
	}
}

// Without RBAC, any user reads the TensorBoard logs of experiments and trials that exist; sources
// that name nothing, such as a deleted experiment, are refused, so a deletion denies the next
// request.
func TestCanGetTensorBoardSourcesBasicNotFound(t *testing.T) {
	pgDB := setupTest(t)
	ctx := context.Background()

	owner := db.RequireMockUser(t, pgDB)
	viewer := db.RequireMockUser(t, pgDB)
	exp := db.RequireMockExperiment(t, pgDB, owner)
	trial := int32(db.RequireMockTrialID(t, pgDB, exp))
	deleted := db.RequireMockExperiment(t, pgDB, owner)
	_, err := db.Bun().NewDelete().Table("experiments").Where("id = ?", deleted.ID).Exec(ctx)
	require.NoError(t, err)

	for name, impl := range map[string]NSCAuthZ{
		"basic": &NSCAuthZBasic{}, "permissive": &NSCAuthZPermissive{},
	} {
		for _, tc := range []struct {
			name    string
			exps    []int32
			trials  []int32
			allowed bool
		}{
			{name: "experiment", exps: []int32{int32(exp.ID)}, allowed: true},
			{name: "trial", trials: []int32{trial}, allowed: true},
			{name: "both", exps: []int32{int32(exp.ID)}, trials: []int32{trial}, allowed: true},
			{name: "deleted experiment", exps: []int32{int32(exp.ID), int32(deleted.ID)}},
			{name: "unknown experiment", exps: []int32{math.MaxInt32}},
			{name: "unknown trial", exps: []int32{int32(exp.ID)}, trials: []int32{math.MaxInt32}},
		} {
			err := impl.CanGetTensorBoardSources(ctx, viewer, tc.exps, tc.trials)
			if tc.allowed {
				require.NoError(t, err, "%s: %s", name, tc.name)
			} else {
				require.Equal(t, authz.PermissionDeniedError{}, err, "%s: %s", name, tc.name)
			}
		}
	}
}
